// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackOperationLeaseAndRecovery(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	work := t.TempDir()
	run := stackTestGit(t, work)
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	base := run("rev-parse", "HEAD")
	run("checkout", "-b", "branch2")
	run("commit", "--allow-empty", "-m", "layer")
	head := run("rev-parse", "HEAD")
	run("checkout", "-b", "candidate")
	run("commit", "--allow-empty", "-m", "rebuilt layer")
	newHead := run("rev-parse", "HEAD")
	run("checkout", "-b", "external")
	run("commit", "--allow-empty", "-m", "external review correction")
	externalHead := run("rev-parse", "HEAD")
	require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
	run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
	_, err := db.GetEngine(ctx).ID(pr.ID).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
	require.NoError(t, err)
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	_, err = db.GetEngine(ctx).Insert(stack)
	require.NoError(t, err)
	entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, HeadSHA: head, OldParentSHA: base}
	_, err = db.GetEngine(ctx).Insert(entry)
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).Insert(&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(repo.ID, pr.HeadBranch)})
	require.NoError(t, err)
	op := &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: 1, Kind: "land", State: "running", ThroughPosition: 1}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	other := &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: 1, Kind: "land", State: "queued"}
	require.ErrorIs(t, issues_model.CreateStackOperation(ctx, other), issues_model.ErrStackRevision)
	stale := *op
	require.NoError(t, issues_model.SaveStackOperation(ctx, op))
	require.ErrorIs(t, issues_model.SaveStackOperation(ctx, &stale), issues_model.ErrStackRevision)
	layer := &stackLayerJournal{EntryID: entry.ID, PullID: pr.ID, Position: 1, HeadBranch: pr.HeadBranch, ExpectedHead: base, OldParent: base, NewParent: base, NewHead: newHead, Phase: "publishing"}
	require.Error(t, publishStackLayer(ctx, op, layer, actor))
	layer.ExpectedHead = head
	require.NoError(t, publishStackLayer(ctx, op, layer, actor))
	require.NoError(t, publishStackLayer(ctx, op, layer, actor))
	bareRun := stackTestGit(t, gitrepo.RepoLocalPath(repo))
	assert.Equal(t, newHead, bareRun("rev-parse", "branch2"))
	require.ErrorIs(t, checkOrdinaryStackMutation(ctx, pr), ErrPullRequestStacked)
	pr.BaseBranch = "some-feature"
	require.ErrorIs(t, checkStackMergeOrder(ctx, pr), ErrPullRequestStacked)

	bareRun("update-ref", "refs/heads/branch2", externalHead)
	require.Error(t, publishStackLayer(ctx, op, layer, actor))
	partial := &stackJournal{Stage: "publish", Layers: []*stackLayerJournal{layer}}
	op.State = "blocked"
	require.NoError(t, saveStackJournal(ctx, op, partial))
	release, err := globallock.Lock(ctx, fmt.Sprintf("stack-operation:%d", op.ID))
	require.NoError(t, err)
	require.ErrorIs(t, CancelStackOperation(ctx, actor, op.ID), issues_model.ErrStackRevision)
	release()
	require.NoError(t, CancelStackOperation(ctx, actor, op.ID))
	assert.Equal(t, externalHead, bareRun("rev-parse", "branch2"))
	entry = unittest.AssertExistsAndLoadBean(t, &issues_model.StackEntry{ID: entry.ID})
	assert.Equal(t, externalHead, entry.HeadSHA)
	assert.Equal(t, base, entry.OldParentSHA)
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	assert.Zero(t, stack.ActiveOperationID)
	interrupted := &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: stack.Revision, Kind: "rebase", State: "cancelling", JournalJSON: op.JournalJSON}
	require.NoError(t, issues_model.CreateStackOperation(ctx, interrupted))
	require.ErrorIs(t, ResumeStackOperation(ctx, actor, interrupted.ID), issues_model.ErrStackRevision)
	active, err := issues_model.GetActiveStackOperations(ctx)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(active, func(candidate *issues_model.StackOperation) bool { return candidate.ID == interrupted.ID }))
	runStackOperation(ctx, interrupted.ID)
	recovered, err := issues_model.GetStackOperation(ctx, interrupted.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", recovered.State)
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	op = &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: stack.Revision, Kind: "land", State: "running", ThroughPosition: 1}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	layer.ExpectedHead = externalHead
	layer.Phase = "merging"
	journal := &stackJournal{Stage: "confirm", Layers: []*stackLayerJournal{layer}}
	data, err := json.Marshal(journal)
	require.NoError(t, err)
	op.JournalJSON = string(data)
	require.NoError(t, issues_model.SaveStackOperation(ctx, op))
	require.ErrorContains(t, executeStackOperation(ctx, op), "waiting for the recorded merge result")
	assert.Equal(t, base, bareRun("rev-parse", "release"))
	layer.LandingBaseSHA, layer.MergeCandidateSHA = base, externalHead
	require.NoError(t, saveStackJournal(ctx, op, journal))
	require.NoError(t, CheckStackMergePublication(ctx, pr.ID, actor.ID, "release", base, externalHead))
	require.ErrorIs(t, CheckStackMergePublication(ctx, pr.ID, actor.ID, "branch2", base, externalHead), ErrPullRequestStacked)
	require.ErrorIs(t, CheckStackMergePublication(ctx, pr.ID, actor.ID, "release", newHead, externalHead), issues_model.ErrStackRevision)
	require.Error(t, CheckStackMergePublication(ctx, pr.ID, 4, "release", base, externalHead))
	candidateRef := fmt.Sprintf("refs/stack-operations/%d/merge-%d", op.ID, entry.ID)
	bareRun("update-ref", candidateRef, externalHead)
	bareRun("update-ref", "refs/heads/release", externalHead)
	require.NoError(t, executeStackOperation(ctx, op))
	finished, err := issues_model.GetStackOperation(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", finished.State)
	assert.Equal(t, 1, finished.Completed)
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	assert.Zero(t, stack.ActiveOperationID)
	assert.Equal(t, issues_model.StackStateComplete, stack.State)
	assert.Equal(t, externalHead, bareRun("rev-parse", "release"))
	merged := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID})
	assert.True(t, merged.HasMerged)
	assert.Equal(t, externalHead, merged.MergedCommitID)
	assert.False(t, git.IsReferenceExist(ctx, repo, candidateRef))
	require.ErrorIs(t, CancelStackOperation(ctx, actor, op.ID), issues_model.ErrStackRevision)
}

func TestStackMergeabilityCheckWakesOperation(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1, ActiveOperationID: 42}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: 2, BranchKey: issues_model.StackBranchKey(1, "branch2")}))
	q, err := queue.NewWorkerPoolQueueWithContext(ctx, "stack-wake-test", setting.QueueSettings{Type: "channel", Length: 10}, func(ids ...int64) []int64 { return nil }, true)
	require.NoError(t, err)
	defer q.Cancel()
	defer test.MockVariableValue(&stackOperationQueue, q)()
	patchQueue, err := queue.NewWorkerPoolQueueWithContext(ctx, "stack-patch-test", setting.QueueSettings{Type: "channel", Length: 10}, func(ids ...string) []string { return nil }, true)
	require.NoError(t, err)
	defer patchQueue.Cancel()
	defer test.MockVariableValue(&prPatchCheckerQueue, patchQueue)()
	checkPullRequestMergeable(2)
	queued, err := q.Has(42)
	require.NoError(t, err)
	assert.True(t, queued, "mergeability completion must wake the stack after its process context is cancelled")
}

func TestStackManualMergeSerializesReservation(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	work := t.TempDir()
	run := stackTestGit(t, work)
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	run("checkout", "-b", "branch2")
	run("commit", "--allow-empty", "-m", "landed layer")
	head := run("rev-parse", "HEAD")
	run("branch", "-f", "release", head)
	run("checkout", "-b", "pr-to-update")
	run("commit", "--allow-empty", "-m", "upper layer")
	upperHead := run("rev-parse", "HEAD")
	require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
	run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
	pr.BaseBranch = "release"
	_, err := db.GetEngine(ctx).ID(pr.ID).Cols("base_branch").Update(pr)
	require.NoError(t, err)
	repoUnit := unittest.AssertExistsAndLoadBean(t, &repo_model.RepoUnit{RepoID: 1, Type: unit.TypePullRequests})
	repoUnit.PullRequestsConfig().AllowManualMerge = true
	require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repoUnit))
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 5, Position: 2}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: 5, BranchKey: issues_model.StackBranchKey(1, "pr-to-update")}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(1, pr.HeadBranch)}))
	op := &issues_model.StackOperation{StackID: stack.ID, ExpectedRevision: 1, State: "queued"}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	require.ErrorIs(t, MergedManually(ctx, pr, actor, nil, head), ErrPullRequestStacked)
	op.State = "cancelled"
	require.NoError(t, issues_model.FinishStackOperation(ctx, op))
	gitRepo, err := git.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	require.Error(t, MergedManually(ctx, pr, actor, gitRepo, "invalid"))
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, stack.Revision, "failed manual merge rolls back reservation fence")
	require.NoError(t, MergedManually(ctx, pr, actor, gitRepo, head))
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 3, stack.Revision)
	assert.Equal(t, issues_model.StackStateOpen, stack.State)
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Equal(t, head, entries[0].LandedCommitSHA)
	require.ErrorIs(t, issues_model.CheckStackBranchMutation(ctx, 1, pr.HeadBranch), issues_model.ErrBranchInStack)

	require.ErrorIs(t, issues_model.CreateStackOperation(ctx, &issues_model.StackOperation{StackID: stack.ID, ExpectedRevision: 2, State: "queued"}), issues_model.ErrStackRevision)
	upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 5})
	upper.BaseBranch = "release"
	_, err = db.GetEngine(ctx).ID(upper.ID).Cols("base_branch").Update(upper)
	require.NoError(t, err)
	stackTestGit(t, gitrepo.RepoLocalPath(repo))("update-ref", "refs/heads/release", upperHead)
	require.NoError(t, MergedManually(ctx, upper, actor, gitRepo, upperHead))
	stack, err = issues_model.GetStackByID(ctx, stack.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 4, stack.Revision)
	assert.Equal(t, issues_model.StackStateComplete, stack.State)
	require.NoError(t, issues_model.CheckStackBranchMutation(ctx, 1, pr.HeadBranch))
	membership, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	require.NoError(t, err)
	require.NotNil(t, membership, "completed stacks retain historical PR membership")
	entries, err = issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Equal(t, upperHead, entries[1].LandedCommitSHA)
}
