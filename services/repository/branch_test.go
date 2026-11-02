// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repository

import (
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanDeleteBranchDefaultTargets(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())

	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	rule := &git_model.ProtectedBranch{
		RepoID: repo.ID, RuleName: "*",
		ProtectedBranchConfig: git_model.ProtectedBranchConfig{CanPush: true, CanDelete: true},
	}
	require.NoError(t, db.Insert(ctx, rule))
	assert.NoError(t, CanDeleteBranch(ctx, repo, "branch2", owner))
	assert.ErrorIs(t, CanDeleteBranch(ctx, repo, repo.DefaultBranch, owner), ErrBranchIsDefault)

	pullRequestUnit, err := repo.GetUnit(ctx, unit.TypePullRequests)
	assert.NoError(t, err)
	pullRequestConfig := pullRequestUnit.PullRequestsConfig()
	pullRequestConfig.DefaultTargetBranch = "branch2"
	pullRequestUnit.Config = pullRequestConfig
	assert.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, pullRequestUnit))
	repo.Units = nil

	assert.ErrorIs(t, CanDeleteBranch(ctx, repo, "branch2", owner), ErrBranchIsDefault)

	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: 1, BranchKey: issues_model.StackBranchKey(repo.ID, "layer")}))
	writePermission := access_model.Permission{}
	writePermission.SetUnitsWithDefaultAccessMode([]*repo_model.RepoUnit{{Type: unit.TypeCode}}, perm.AccessModeWrite)
	for _, branch := range []string{"release", "layer"} {
		assert.ErrorIs(t, CanDeleteBranch(ctx, repo, branch, owner), issues_model.ErrBranchInStack)
		assert.ErrorIs(t, CanDeleteBranchWithPermission(ctx, repo, branch, owner, writePermission), issues_model.ErrBranchInStack)
	}
}

func TestSyncBranchesToDBUsesGitState(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	gitRepo, err := git.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	_, err = db.GetEngine(ctx).Where("repo_id=? AND name=?", repo.ID, "master").Update(&git_model.Branch{CommitID: "985f0301dba5e7b34be866819cd15ad3d8f508ee"})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).Where("repo_id=? AND name=?", repo.ID, "foo").Cols("is_deleted").Update(&git_model.Branch{IsDeleted: false})
	require.NoError(t, err)
	require.NoError(t, git_model.MarkBranchAsDeleted(ctx, repo.ID, "develop", 1))

	require.NoError(t, SyncBranchesToDB(ctx, repo.ID, 2, gitRepo, []string{"master", "foo", "develop", "pr-to-update"}))

	loadBranch := func(name string) *git_model.Branch {
		return unittest.AssertExistsAndLoadBean(t, &git_model.Branch{RepoID: repo.ID, Name: name})
	}
	assert.Equal(t, "65f1bf27bc3bf70f64657658635e66094edbcb4d", loadBranch("master").CommitID)
	assert.True(t, loadBranch("foo").IsDeleted)
	assert.False(t, loadBranch("develop").IsDeleted)
	assert.Equal(t, "62fb502a7172d4453f0322a2cc85bddffa57f07a", loadBranch("pr-to-update").CommitID)
}
