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
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/json"
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
