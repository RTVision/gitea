// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"os"
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

func TestForkStackMergeRecovery(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	fork := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	work := t.TempDir()
	run := stackTestGit(t, work)
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	base := run("rev-parse", "HEAD")
	run("checkout", "-b", "lower")
	run("commit", "--allow-empty", "-m", "lower")
	boundary := run("rev-parse", "HEAD")
	run("checkout", "-b", "upper")
	run("commit", "--allow-empty", "-m", "upper")
	head := run("rev-parse", "HEAD")
	for _, repo := range []*repo_model.Repository{main, fork} {
		require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
		run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
	}
	pr.BaseRepoID, pr.HeadRepoID, pr.BaseBranch, pr.HeadBranch = fork.ID, fork.ID, "lower", "upper"
	_, err := db.GetEngine(ctx).ID(pr.ID).Cols("base_repo_id", "head_repo_id", "base_branch", "head_branch").Update(pr)
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(pr.IssueID).Cols("repo_id").Update(&issues_model.Issue{RepoID: fork.ID})
	require.NoError(t, err)
	stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, HeadSHA: head, OldParentSHA: boundary}
	require.NoError(t, db.Insert(ctx, entry))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(fork.ID, "upper")}))
	require.NoError(t, pinStackEntry(ctx, stack, entry))
	entry.OldParentSHA = ""
	require.NoError(t, pinStackEntry(ctx, stack, entry), "an unknown cancellation boundary must allow explicit synchronization")
	entry.OldParentSHA = boundary
	require.NoError(t, pinStackEntry(ctx, stack, entry))
	forkRun := stackTestGit(t, gitrepo.RepoLocalPath(fork))
	require.ErrorIs(t, MergedManually(ctx, pr, actor, nil, boundary), ErrPullRequestStacked)
	assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID}).HasMerged)
	assert.EqualValues(t, 1, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID}).Revision)
	assert.Empty(t, unittest.AssertExistsAndLoadBean(t, &issues_model.StackEntry{ID: entry.ID}).LandedCommitSHA)
	forkRun("update-ref", "refs/heads/release", head)
	layer := &stackLayerJournal{EntryID: entry.ID, PullID: pr.ID, Position: 1, HeadRepoID: fork.ID, LandingRepoID: main.ID, LandingBranch: "release", HeadBranch: "upper", ExpectedHead: head, OldParent: boundary, LandingBaseSHA: base, MergeCandidateSHA: head, Phase: "merging"}
	merged, err := reconcileStackMerge(ctx, layer, pr, actor)
	require.NoError(t, err)
	require.False(t, merged, "landing in the fork must not satisfy the main receipt")
	mainRun := stackTestGit(t, gitrepo.RepoLocalPath(main))
	mainRun("update-ref", "refs/heads/release", head)
	later := forkRun("commit-tree", head+"^{tree}", "-p", head, "-m", "later source edit")
	forkRun("update-ref", "refs/heads/upper", later)
	_, err = reconcileStackMerge(ctx, layer, pr, actor)
	var mismatch ErrSHADoesNotMatch
	require.ErrorAs(t, err, &mismatch)
	forkRun("update-ref", "refs/heads/upper", head)
	merged, err = reconcileStackMerge(ctx, layer, pr, actor)
	require.NoError(t, err)
	require.True(t, merged)
	saved := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID})
	assert.Equal(t, fork.ID, saved.BaseRepoID)
	assert.Equal(t, "lower", saved.BaseBranch)
	assert.Equal(t, pr.IssueID, saved.IssueID)
	assert.Equal(t, main.ID, saved.MergedRepoID)
	assert.Equal(t, "release", saved.MergedBranch)
	assert.Equal(t, boundary, saved.MergeBase)
	assert.Equal(t, head, saved.MergedCommitID)
	assert.Equal(t, base, saved.MergedBaseCommitID)
	wrongTarget := *saved
	wrongTarget.MergedRepoID = fork.ID
	_, err = reconcileStackMerge(ctx, layer, &wrongTarget, actor)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	merged, err = reconcileStackMerge(ctx, layer, saved, actor)
	require.NoError(t, err)
	assert.True(t, merged)
	assert.Equal(t, head, mainRun("rev-parse", "release"))
	assert.Equal(t, head, forkRun("rev-parse", saved.GetGitHeadRefName()))
	assert.True(t, git.IsReferenceExist(ctx, main, issues_model.StackHeadRefName(pr.ID)))
}
