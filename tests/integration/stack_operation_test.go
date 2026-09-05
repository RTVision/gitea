// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/json"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	pull_service "gitea.dev/services/pull"
	commitstatus_service "gitea.dev/services/repository/commitstatus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForStackOperation(t *testing.T, id int64) *issues_model.StackOperation {
	t.Helper()
	var op *issues_model.StackOperation
	for range 20 {
		require.NoError(t, queue.GetManager().FlushAll(t.Context(), 15*time.Second))
		var err error
		op, err = issues_model.GetStackOperation(t.Context(), id)
		require.NoError(t, err)
		if op.State == "completed" || op.State == "blocked" {
			return op
		}
	}
	return op
}

func TestNativeStackOrderedLanding(t *testing.T) {
	for _, style := range []repo_model.MergeStyle{repo_model.MergeStyleMerge, repo_model.MergeStyleSquash, repo_model.MergeStyleRebase} {
		t.Run(string(style), func(t *testing.T) {
			onGiteaRun(t, func(t *testing.T, _ *url.URL) {
				defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
				session := loginUser(t, "user2")
				testCreateBranch(t, session, "user2", "repo1", "branch/master", "release", http.StatusSeeOther)
				testEditFileToNewBranch(t, session, "user2", "repo1", "release", "stack-lower", "README.md", "lower layer\n")
				testEditFileToNewBranch(t, session, "user2", "repo1", "stack-lower", "stack-upper", "README.md", "lower layer\nupper layer\n")
				testPullCreate(t, session, "user2", "repo1", true, "release", "stack-lower", "stack lower")
				testPullCreate(t, session, "user2", "repo1", true, "stack-lower", "stack-upper", "stack upper")
				require.NoError(t, queue.GetManager().FlushAll(t.Context(), 10*time.Second))
				repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
				actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
				lower := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: "stack-lower"})
				upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: "stack-upper"})
				stack, err := pull_service.CreateStack(t.Context(), actor, repo, pull_service.CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{lower.ID, upper.ID}})
				require.NoError(t, err)
				require.ErrorIs(t, pull_service.Merge(upper, actor, style, "", "", false), pull_service.ErrPullRequestStacked)
				if style == repo_model.MergeStyleMerge {
					_, err := db.GetEngine(t.Context()).Insert(&git_model.ProtectedBranch{RepoID: repo.ID, RuleName: "release", EnableStatusCheck: true, StatusCheckContexts: []string{"stack-ci"}})
					require.NoError(t, err)
				}
				var op *issues_model.StackOperation
				if style == repo_model.MergeStyleSquash {
					entries, err := issues_model.GetStackEntries(t.Context(), stack.ID)
					require.NoError(t, err)
					layers := make([]map[string]any, 0, len(entries))
					for i, entry := range entries {
						pr := lower
						phase := "merging"
						if i > 0 {
							pr, phase = upper, "ready"
						}
						layers = append(layers, map[string]any{"entry_id": entry.ID, "pull_id": entry.PullRequestID, "position": entry.Position, "head_branch": pr.HeadBranch, "expected_head": entry.HeadSHA, "old_parent": entry.OldParentSHA, "landing_base_sha": entry.OldParentSHA, "phase": phase})
					}
					journal, err := json.Marshal(map[string]any{"stage": "confirm", "layers": layers})
					require.NoError(t, err)
					op = &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: stack.Revision, Kind: "land", MergeStyle: string(style), ThroughPosition: 1, State: "blocked", JournalJSON: string(journal)}
					require.NoError(t, issues_model.CreateStackOperation(t.Context(), op))
					require.NoError(t, pull_service.ResumeStackOperation(t.Context(), actor, op.ID))
				} else {
					op, err = pull_service.StartStackOperation(t.Context(), actor, pull_service.StackOperationOptions{StackID: stack.ID, ExpectedRevision: stack.Revision, ThroughPosition: 1, Kind: "land", MergeStyle: style})
					require.NoError(t, err)
				}
				if style == repo_model.MergeStyleMerge {
					op = waitForStackOperation(t, op.ID)
					require.Equal(t, "waiting", op.State, op.LastError)
					oldHead, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/stack-lower")
					require.NoError(t, err)
					trunk, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/release")
					require.NoError(t, err)
					advanced, _, err := gitcmd.NewCommand("commit-tree").AddDynamicArguments(trunk+"^{tree}").AddArguments("-p").AddDynamicArguments(trunk).AddArguments("-m", "trunk advanced while waiting").WithRepo(repo).WithEnv(append(os.Environ(), "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")).RunStdString(t.Context())
					require.NoError(t, err)
					require.NoError(t, gitcmd.NewCommand("update-ref").AddDynamicArguments("refs/heads/release", strings.TrimSpace(advanced), trunk).WithRepo(repo).Run(t.Context()))
					require.NoError(t, commitstatus_service.CreateCommitStatus(t.Context(), repo, actor, oldHead, &git_model.CommitStatus{Context: "stack-ci", State: commitstatus.CommitStatusSuccess}))
					op = waitForStackOperation(t, op.ID)
					require.Equal(t, "waiting", op.State, op.LastError)
					newHead, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/stack-lower")
					require.NoError(t, err)
					require.NotEqual(t, oldHead, newHead)
					assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: lower.ID}).HasMerged)
					require.NoError(t, commitstatus_service.CreateCommitStatus(t.Context(), repo, actor, newHead, &git_model.CommitStatus{Context: "stack-ci", State: commitstatus.CommitStatusSuccess}))
				}
				op = waitForStackOperation(t, op.ID)
				require.Equal(t, "completed", op.State, op.LastError)
				assert.Equal(t, 1, op.Completed)
				lower = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: lower.ID})
				upper = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				assert.True(t, lower.HasMerged)
				assert.False(t, upper.HasMerged)
				assert.Equal(t, "release", upper.BaseBranch)
				trunk, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/release")
				require.NoError(t, err)
				count, _, err := gitcmd.NewCommand("rev-list", "--count").AddDynamicArguments(trunk + "..refs/heads/stack-upper").WithRepo(repo).RunStdString(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "1\n", count, "only the upper layer remains after landing the lower layer")
				stack, err = issues_model.GetStackByID(t.Context(), stack.ID)
				require.NoError(t, err)
				if style == repo_model.MergeStyleMerge {
					head, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/stack-upper")
					require.NoError(t, err)
					require.NoError(t, commitstatus_service.CreateCommitStatus(t.Context(), repo, actor, head, &git_model.CommitStatus{Context: "stack-ci", State: commitstatus.CommitStatusSuccess}))
				}
				op, err = pull_service.StartStackOperation(t.Context(), actor, pull_service.StackOperationOptions{StackID: stack.ID, ExpectedRevision: stack.Revision, ThroughPosition: 2, Kind: "land", MergeStyle: style})
				require.NoError(t, err)
				op = waitForStackOperation(t, op.ID)
				require.Equal(t, "completed", op.State, op.LastError)
				stack, err = issues_model.GetStackByID(t.Context(), stack.ID)
				require.NoError(t, err)
				assert.Equal(t, issues_model.StackStateComplete, stack.State)
				content, _, err := gitcmd.NewCommand("show").AddDynamicArguments("refs/heads/release:README.md").WithRepo(repo).RunStdString(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "lower layer\nupper layer\n", content)
			})
		})
	}
}
