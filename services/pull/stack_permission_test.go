// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkContributorStackAuthority(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 10})
	fork := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 11})
	maintainer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 12})
	contributor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 13})
	require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{RepoID: fork.ID, Type: unit.TypePullRequests}))
	_, err := db.GetEngine(ctx).ID(8).Cols("poster_id").Update(&issues_model.Issue{PosterID: contributor.ID})
	require.NoError(t, err)
	upper := &issues_model.PullRequest{HeadRepoID: fork.ID, BaseRepoID: fork.ID, HeadBranch: "own-upper", BaseBranch: "branch2"}
	require.NoError(t, issues_model.NewPullRequest(ctx, fork, &issues_model.Issue{RepoID: fork.ID, PosterID: contributor.ID, Poster: contributor, Title: "Own upper layer"}, nil, nil, upper))
	stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1, CreatedByID: maintainer.ID}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx,
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: 3, Position: 1},
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: upper.ID, Position: 2, ParentPullRequestID: 3},
	))
	allowed, err := CanCreateStack(ctx, contributor, main, []int64{3, upper.ID})
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, err = CanManageStack(ctx, contributor, stack)
	require.NoError(t, err)
	assert.True(t, allowed, "stack creator does not replace layer ownership")
	for _, kind := range []string{"rebase", "update", "land"} {
		allowed, err = CanOperateStack(ctx, contributor, stack, kind)
		require.NoError(t, err)
		assert.Equal(t, kind != "land", allowed, kind)
	}
	allowed, err = CanManageStack(ctx, maintainer, stack)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, err = CanOperateStack(ctx, maintainer, stack, "land")
	require.NoError(t, err)
	assert.True(t, allowed)

	_, err = db.GetEngine(ctx).ID(upper.IssueID).Cols("poster_id").Update(&issues_model.Issue{PosterID: maintainer.ID})
	require.NoError(t, err)
	allowed, err = CanManageStack(ctx, contributor, stack)
	require.NoError(t, err)
	assert.False(t, allowed, "writable branches do not grant management of another author's layer")
	_, err = db.GetEngine(ctx).ID(upper.IssueID).Cols("poster_id").Update(&issues_model.Issue{PosterID: contributor.ID})
	require.NoError(t, err)
	require.NoError(t, db.Insert(ctx, &git_model.ProtectedBranch{RepoID: fork.ID, RuleName: upper.HeadBranch, CanPush: false}))
	allowed, err = CanManageStack(ctx, contributor, stack)
	require.NoError(t, err)
	assert.False(t, allowed, "layer ownership does not bypass source branch protection")

	sameRepo := &issues_model.PullRequest{HeadRepoID: main.ID, BaseRepoID: main.ID, HeadBranch: "own-main-layer", BaseBranch: "master"}
	require.NoError(t, issues_model.NewPullRequest(ctx, main, &issues_model.Issue{RepoID: main.ID, PosterID: contributor.ID, Poster: contributor, Title: "Main layer"}, nil, nil, sameRepo))
	allowed, err = CanCreateStack(ctx, contributor, main, []int64{sameRepo.ID})
	require.NoError(t, err)
	assert.False(t, allowed)
}
