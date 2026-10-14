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
