// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackLifecycle(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	outsider := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	run("checkout", "-b", "branch2")
	run("commit", "--allow-empty", "-m", "first")
	run("checkout", "-b", "pr-to-update")
	run("commit", "--allow-empty", "-m", "second")
	require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
	run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
	_, err := db.GetEngine(ctx).ID(2).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
	require.NoError(t, err)
	opts := CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2}}
	_, err = CreateStack(ctx, outsider, repo, opts)
	require.Error(t, err)
	_, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{5, 2}})
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	_, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2, 2}})
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	stack, err := CreateStack(ctx, owner, repo, opts)
	require.NoError(t, err)
	_, err = CreateStack(ctx, owner, repo, opts)
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	_, err = AppendStack(ctx, owner, stack.ID, 0, []int64{5})
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	stack, err = AppendStack(ctx, owner, stack.ID, 1, []int64{5})
	require.NoError(t, err)
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, entries[0].HeadSHA, entries[1].OldParentSHA)
	assert.Equal(t, int64(2), entries[1].ParentPullRequestID)
	upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 5})
	branch, err := issues_model.ResolvePullRequestPolicyBranch(ctx, upper)
	require.NoError(t, err)
	assert.Equal(t, "release", branch)
	assert.Equal(t, "branch2", upper.BaseBranch)
	run("checkout", "branch2")
	run("commit", "--amend", "--allow-empty", "-m", "first revised")
	newParent := run("rev-parse", "HEAD")
	run("rebase", "--onto", newParent, entries[1].OldParentSHA, "pr-to-update")
	expected := []StackHeadExpectation{
		{PullRequestID: 2, HeadSHA: newParent, ParentSHA: entries[0].OldParentSHA},
		{PullRequestID: 5, HeadSHA: run("rev-parse", "HEAD"), ParentSHA: newParent},
	}
	run("push", "--force-with-lease=refs/heads/branch2:"+entries[0].HeadSHA, "--force-with-lease=refs/heads/pr-to-update:"+entries[1].HeadSHA, gitrepo.RepoLocalPath(repo), "branch2", "pr-to-update")
	_, err = SynchronizeStack(ctx, outsider, stack.ID, 2, expected)
	require.Error(t, err)
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected[:1])
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	wrong := append([]StackHeadExpectation(nil), expected...)
	wrong[1].ParentSHA = entries[1].OldParentSHA
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, wrong)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	stack, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected)
	require.NoError(t, err)
	assert.EqualValues(t, 3, stack.Revision)
	synced, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Equal(t, newParent, synced[1].OldParentSHA)
	assert.Equal(t, expected[1].HeadSHA, synced[1].HeadSHA)
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	require.Error(t, Unstack(ctx, outsider, stack.ID, 3))
	require.NoError(t, Unstack(ctx, owner, stack.ID, 3))
	membership, err := issues_model.GetPullRequestStack(ctx, 5)
	require.NoError(t, err)
	assert.Nil(t, membership)
	historical, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Len(t, historical, 2)

	stack, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2, 5}})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(2).Cols("has_merged").Update(&issues_model.PullRequest{HasMerged: true})
	require.NoError(t, err)
	require.NoError(t, Unstack(ctx, owner, stack.ID, 1))
	membership, err = issues_model.GetPullRequestStack(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, membership)
	assert.Equal(t, stack.ID, membership.ID)
	membership, err = issues_model.GetPullRequestStack(ctx, 5)
	require.NoError(t, err)
	assert.Nil(t, membership)
}
