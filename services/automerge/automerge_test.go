// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package automerge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/services/automergequeue"
	pull_service "gitea.dev/services/pull"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
	"xorm.io/xorm/contexts"
	"xorm.io/xorm/names"
)

func TestMain(m *testing.M) { unittest.MainTest(m) }

func TestScheduleAutoMergeRejectsStackedPreferences(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(1, pr.HeadBranch)}))
	for _, message := range []string{"", "custom merge message"} {
		for _, deleteBranch := range []bool{false, true} {
			scheduled, err := ScheduleAutoMerge(ctx, actor, pr, repo_model.MergeStyleSquash, message, deleteBranch)
			require.ErrorIs(t, err, ErrStackAutoMergeUnsupported)
			assert.False(t, scheduled)
		}
	}
	operations, err := issues_model.GetStackOperations(ctx, stack.ID)
	require.NoError(t, err)
	assert.Empty(t, operations)
	scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.False(t, scheduled)
}

type stackSchedulingGate struct {
	ctx     context.Context
	arrived chan struct{}
	release chan struct{}
	hit     atomic.Bool
}

type stackSchedulingHook struct {
	gate atomic.Pointer[stackSchedulingGate]
}

func (h *stackSchedulingHook) BeforeProcess(c *contexts.ContextHook) (context.Context, error) {
	gate := h.gate.Load()
	if gate != nil && (strings.HasPrefix(c.SQL, "UPDATE") || strings.HasPrefix(c.SQL, "INSERT")) && gate.hit.CompareAndSwap(false, true) {
		close(gate.arrived)
		select {
		case <-gate.release:
		case <-gate.ctx.Done():
		}
	}
	return c.Ctx, nil
}

func (*stackSchedulingHook) AfterProcess(*contexts.ContextHook) error { return nil }

func TestScheduleAutoMergeSerializesStackMembership(t *testing.T) {
	previousEngine := unittest.GetXORMEngine()
	// Deferred transactions expose the read/write race hidden by SQLite's BEGIN IMMEDIATE.
	engine, err := xorm.NewEngine(previousEngine.DriverName(), strings.Replace(previousEngine.DataSourceName(), "_txlock=immediate", "_txlock=deferred", 1))
	require.NoError(t, err)
	engine.SetMapper(names.GonicMapper{})
	db.SetDefaultEngine(t.Context(), engine)
	fixtures := unittest.FixturesOptions{Dir: filepath.Join(setting.GetGiteaTestSourceRoot(), "models", "fixtures")}
	defer func() {
		db.SetDefaultEngine(context.Background(), previousEngine)
		require.NoError(t, unittest.InitFixtures(fixtures))
		require.NoError(t, engine.Close())
	}()
	require.NoError(t, unittest.InitFixtures(fixtures))
	hook := &stackSchedulingHook{}
	unittest.GetXORMEngine().AddHook(hook)
	defer hook.gate.Store(nil)
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	defer test.MockVariableValue(&automergequeue.AddToQueue, func(automergequeue.AutoMergeItem) {})()
	for _, appendLayer := range []bool{false, true} {
		for _, stackWins := range []bool{false, true} {
			name := "create"
			if appendLayer {
				name = "append"
			}
			if stackWins {
				name += "/stack_wins"
			} else {
				name += "/schedule_wins"
			}
			t.Run(name, func(t *testing.T) {
				require.NoError(t, unittest.PrepareTestDatabase())
				defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
				actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
				work := t.TempDir()
				run := func(args ...string) {
					t.Helper()
					cmd := exec.CommandContext(ctx, "git", args...)
					cmd.Dir = work
					cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")
					out, err := cmd.CombinedOutput()
					require.NoError(t, err, "%s", out)
				}
				run("init", "--initial-branch=release")
				run("commit", "--allow-empty", "-m", "trunk")
				run("checkout", "-b", "branch2")
				run("commit", "--allow-empty", "-m", "lower")
				run("checkout", "-b", "pr-to-update")
				run("commit", "--allow-empty", "-m", "upper")
				run("update-ref", "refs/pull/3/head", "branch2")
				run("update-ref", "refs/pull/5/head", "pr-to-update")
				require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
				run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
				_, err := db.GetEngine(ctx).ID(2).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
				require.NoError(t, err)
				pullID := int64(2)
				var stack *issues_model.PullRequestStack
				if appendLayer {
					stack, err = pull_service.CreateStack(ctx, actor, repo, pull_service.CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2}})
					require.NoError(t, err)
					pullID = 5
					_, err = db.GetEngine(ctx).ID(pullID).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "branch2"})
					require.NoError(t, err)
				}
				pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pullID})
				createMembership := func(ctx context.Context) error {
					if appendLayer {
						_, err := pull_service.AppendStack(ctx, actor, stack.ID, stack.Revision, []int64{pullID})
						return err
					}
					_, err := pull_service.CreateStack(ctx, actor, repo, pull_service.CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{pullID}})
					return err
				}
				schedule := func(ctx context.Context) error {
					_, err := ScheduleAutoMerge(ctx, actor, pr, repo_model.MergeStyleSquash, "custom message", true)
					return err
				}
				winner, loser := schedule, createMembership
				if stackWins {
					winner, loser = createMembership, schedule
				}
				gate := &stackSchedulingGate{ctx: ctx, arrived: make(chan struct{}), release: make(chan struct{})}
				hook.gate.Store(gate)
				result := make(chan error, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					result <- loser(ctx)
				}()
				defer func() {
					cancel()
					<-done
				}()
				select {
				case <-gate.arrived:
				case err := <-result:
					t.Fatalf("competing request returned before its first database write: %v", err)
				case <-ctx.Done():
					t.Fatal("competing request did not reach its first database write")
				}
				winnerErr := winner(ctx)
				close(gate.release)
				loserErr := <-result
				require.NoError(t, winnerErr)
				if stackWins {
					require.ErrorIs(t, loserErr, ErrStackAutoMergeUnsupported)
				} else {
					require.ErrorIs(t, loserErr, issues_model.ErrStackRevision)
				}
				membership, err := issues_model.GetPullRequestStack(ctx, pullID)
				require.NoError(t, err)
				scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, pullID)
				require.NoError(t, err)
				assert.Equal(t, stackWins, membership != nil)
				assert.Equal(t, !stackWins, scheduled)
				stored := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pullID})
				assert.Equal(t, pr.MergedUnix, stored.MergedUnix, "reservation must not update merge timestamps")
			})
		}
	}
}
