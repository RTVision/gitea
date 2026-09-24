// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackMergeLayer(t *testing.T) {
	work := t.TempDir()
	run := stackTestGit(t, work)
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(work, name), []byte(content), 0o600))
		run("add", name)
	}
	run("init", "--initial-branch=release")
	write("file", "base\n")
	run("commit", "-m", "base")
	run("checkout", "-b", "lower")
	write("lower.txt", "lower\n")
	run("commit", "-m", "lower")
	head := run("rev-parse", "HEAD")
	run("checkout", "release")
	write("trunk.txt", "trunk\n")
	run("commit", "-m", "trunk")
	trunk := run("rev-parse", "HEAD")
	repo, err := git.OpenRepositoryLocal(t.Context(), work)
	require.NoError(t, err)
	defer repo.Close()
	env := append(os.Environ(), "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")

	merged, err := mergeStackLayer(t.Context(), repo, env, nil, head, trunk, "release", "lower", 1, false)
	require.NoError(t, err)
	assert.Equal(t, head+"\n"+trunk, run("rev-parse", merged+"^1", merged+"^2"))
	assert.Equal(t, "Merge branch 'release' into lower", run("log", "-1", "--format=%s", merged))
	assert.Equal(t, head, run("rev-parse", "lower"), "the source branch is only updated by publication")

	// A squash of a layer that contains the trunk has the layer's tree, so the layer above absorbs it unchanged.
	run("checkout", "-b", "upper", merged)
	write("upper.txt", "upper\n")
	run("commit", "-m", "upper")
	upper := run("rev-parse", "HEAD")
	run("checkout", "release")
	run("merge", "--squash", merged)
	run("commit", "-m", "squashed lower")
	squash := run("rev-parse", "HEAD")
	landed := &issues_model.StackEntry{HeadSHA: merged, LandedCommitSHA: squash}
	equivalent, err := landedStackLayerEquivalent(t.Context(), repo, landed, upper)
	require.NoError(t, err)
	assert.True(t, equivalent)
	equivalent, err = landedStackLayerEquivalent(t.Context(), repo, &issues_model.StackEntry{HeadSHA: head, LandedCommitSHA: squash}, upper)
	require.NoError(t, err)
	assert.False(t, equivalent, "a landed commit whose tree differs from the recorded head must be merged normally")
	absorbed, err := mergeStackLayer(t.Context(), repo, env, nil, upper, squash, "release", "upper", 2, true)
	require.NoError(t, err)
	assert.Equal(t, run("rev-parse", upper+"^{tree}"), run("rev-parse", absorbed+"^{tree}"))
	assert.Equal(t, "upper.txt", run("diff", "--name-only", squash, absorbed))
	equivalent, err = landedStackLayerEquivalent(t.Context(), repo, landed, absorbed)
	require.NoError(t, err)
	assert.False(t, equivalent, "an absorbed landing is not merged twice")

	run("checkout", "-b", "conflict", head)
	write("trunk.txt", "layer\n")
	run("commit", "-m", "conflicting layer")
	conflicting := run("rev-parse", "HEAD")
	_, err = mergeStackLayer(t.Context(), repo, env, nil, conflicting, trunk, "release", "conflict", 3, false)
	var conflict StackMergeConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, []string{"trunk.txt"}, conflict.Files)
	assert.Contains(t, conflict.Error(), "locally merge release into conflict, push, then retry")
	assert.Equal(t, conflicting, run("rev-parse", "conflict"))
}

func TestStackMergeModeUpdate(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	work := t.TempDir()
	run := stackTestGit(t, work)
	commit := func(name string) {
		require.NoError(t, os.WriteFile(filepath.Join(work, name), []byte(name+"\n"), 0o600))
		run("add", name)
		run("commit", "-m", name)
	}
	run("init", "--initial-branch=release")
	commit("base")
	run("checkout", "-b", "branch2")
	commit("lower")
	run("checkout", "-b", "topic")
	commit("topic")
	run("checkout", "branch2")
	run("merge", "--no-ff", "-m", "topic merge", "topic")
	run("checkout", "release")
	commit("trunk-1")
	run("checkout", "branch2")
	run("merge", "--no-ff", "-m", "parent merge", "release")
	run("checkout", "-b", "pr-to-update")
	commit("upper")
	run("checkout", "branch2")
	commit("lower-2")
	run("checkout", "pr-to-update")
	run("merge", "--no-ff", "-m", "pull merge", "branch2")
	bare := gitrepo.RepoLocalPath(repo)
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	run("clone", "--bare", work, bare)
	bareRun := stackTestGit(t, bare)
	_, err := db.GetEngine(ctx).ID(2).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
	require.NoError(t, err)

	opts := CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2, 5}}
	_, err = CreateStack(ctx, owner, repo, opts)
	require.ErrorContains(t, err, "linear layer history")
	opts.Mode = "bogus"
	_, err = CreateStack(ctx, owner, repo, opts)
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	opts.Mode = issues_model.StackModeMerge
	stack, err := CreateStack(ctx, owner, repo, opts)
	require.NoError(t, err)
	assert.Equal(t, issues_model.StackModeMerge, stack.Mode)

	_, err = StartStackOperation(ctx, owner, StackOperationOptions{StackID: stack.ID, ExpectedRevision: 1, Kind: "rebase"})
	require.ErrorContains(t, err, "update by merging")
	_, err = StartStackOperation(ctx, owner, StackOperationOptions{StackID: stack.ID, ExpectedRevision: 1, Kind: "land", MergeStyle: repo_model.MergeStyleRebase})
	require.ErrorContains(t, err, "cannot land")

	upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 5})
	require.NoError(t, CheckStackUpdateByMerge(ctx, upper))
	upper.BaseBranch = "release"
	require.ErrorIs(t, CheckStackUpdateByMerge(ctx, upper), ErrPullRequestStacked)

	run("checkout", "release")
	commit("trunk-2")
	run("push", bare, "release")
	trunk := bareRun("rev-parse", "release")
	oldLower, oldUpper := bareRun("rev-parse", "branch2"), bareRun("rev-parse", "pr-to-update")
	op, err := StartStackOperation(ctx, owner, StackOperationOptions{StackID: stack.ID, ExpectedRevision: 1, Kind: "update"})
	require.NoError(t, err)
	require.NoError(t, executeStackOperation(ctx, op))
	lower := bareRun("rev-parse", "branch2")
	assert.Equal(t, oldLower+"\n"+trunk, bareRun("rev-parse", lower+"^1", lower+"^2"))
	assert.Equal(t, oldUpper+"\n"+lower, bareRun("rev-parse", "pr-to-update^1", "pr-to-update^2"))
	assert.Equal(t, "Merge branch 'branch2' into pr-to-update", bareRun("log", "-1", "--format=%s", "pr-to-update"))
	op = unittest.AssertExistsAndLoadBean(t, &issues_model.StackOperation{ID: op.ID})
	assert.Equal(t, "completed", op.State, op.LastError)
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Equal(t, trunk, entries[0].OldParentSHA)
	assert.Equal(t, lower, entries[1].OldParentSHA)

	run("checkout", "-b", "side", "release~1")
	commit("side")
	run("push", bare, "side")
	side := run("rev-parse", "HEAD")
	journal := &stackJournal{Stage: "restack", Layers: []*stackLayerJournal{
		{HeadBranch: "branch2", ExpectedHead: oldLower, Phase: "ready"},
		{HeadBranch: "pr-to-update", ExpectedHead: side, Phase: "ready"},
	}}
	stack = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID})
	require.NoError(t, adoptFastForwardedStackHeads(ctx, stack, journal))
	assert.Equal(t, lower, journal.Layers[0].ExpectedHead, "a fast-forward push is adopted on retry")
	assert.Equal(t, side, journal.Layers[1].ExpectedHead, "a diverged head is never adopted")
	journal.Stage, journal.Layers[0].ExpectedHead = "publish", oldLower
	require.NoError(t, adoptFastForwardedStackHeads(ctx, stack, journal))
	assert.Equal(t, oldLower, journal.Layers[0].ExpectedHead, "published candidates were built from the recorded head")

	op = &issues_model.StackOperation{StackID: stack.ID, ActorID: owner.ID, ExpectedRevision: stack.Revision, Kind: "update", State: "running"}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	diverged := &stackLayerJournal{EntryID: entries[0].ID, PullID: 2, HeadBranch: "branch2", ExpectedHead: lower, NewHead: side}
	require.Error(t, publishStackLayer(ctx, op, diverged, owner, issues_model.StackModeMerge))
	assert.Equal(t, lower, bareRun("rev-parse", "branch2"), "merge mode never forces a non-fast-forward")
	op.State = "cancelled"
	require.NoError(t, issues_model.FinishStackOperation(ctx, op))

	// A trunk change that conflicts only with the upper layer is resolved locally, pushed normally and retried.
	run("checkout", "release")
	require.NoError(t, os.WriteFile(filepath.Join(work, "upper"), []byte("trunk\n"), 0o600))
	run("add", "upper")
	run("commit", "-m", "conflicting trunk")
	run("push", bare, "release")
	stack = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID})
	op, err = StartStackOperation(ctx, owner, StackOperationOptions{StackID: stack.ID, ExpectedRevision: stack.Revision, Kind: "update"})
	require.NoError(t, err)
	runStackOperation(ctx, op.ID)
	op = unittest.AssertExistsAndLoadBean(t, &issues_model.StackOperation{ID: op.ID})
	require.Equal(t, "blocked", op.State)
	assert.Contains(t, op.LastError, "locally merge release into branch2, then branch2 into pr-to-update, push, then retry")
	run("fetch", bare, "branch2:branch2", "pr-to-update:pr-to-update")
	run("checkout", "branch2")
	run("merge", "--no-edit", "release")
	run("checkout", "pr-to-update")
	require.Error(t, exec.Command("git", "-C", work, "merge", "--no-edit", "branch2").Run())
	require.NoError(t, os.WriteFile(filepath.Join(work, "upper"), []byte("resolved\n"), 0o600))
	run("add", "upper")
	run("commit", "--no-edit")
	run("push", bare, "branch2", "pr-to-update")
	require.NoError(t, ResumeStackOperation(ctx, owner, op.ID))
	runStackOperation(ctx, op.ID)
	op = unittest.AssertExistsAndLoadBean(t, &issues_model.StackOperation{ID: op.ID})
	assert.Equal(t, "completed", op.State, op.LastError)
	assert.Equal(t, run("rev-parse", "pr-to-update"), bareRun("rev-parse", "pr-to-update"), "retry adopts the locally resolved heads")
}
