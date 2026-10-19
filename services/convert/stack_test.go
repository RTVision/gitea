// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package convert

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"

	"github.com/stretchr/testify/require"
)

func TestStackTrunkBasePublicOnlyDoer(t *testing.T) {
	unittest.PrepareTestEnv(t)
	ctx := t.Context()
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	doer.ExtDoerData = user_model.NewTokenExtDoerData(user_model.CredentialAccessToken, 1, "public-only,read:repository")
	stack := &issues_model.PullRequestStack{RepoID: 10, TrunkBranch: "master", State: issues_model.StackStateOpen}
	entries := []*issues_model.StackEntry{{PullRequestID: 3, Position: 1}}

	_, err := stackTrunkBase(ctx, stack, entries, doer, true)
	require.NoError(t, err)
	for _, repoID := range []int64{10, 11} {
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repoID})
		for _, visibility := range []api.VisibleType{api.VisibleTypeLimited, api.VisibleTypePrivate} {
			_, err := db.GetEngine(ctx).ID(repo.OwnerID).Cols("visibility").Update(&user_model.User{Visibility: visibility})
			require.NoError(t, err)
			_, err = stackTrunkBase(ctx, stack, entries, doer, true)
			require.ErrorIs(t, err, util.ErrNotExist)
			_, err = stackTrunkBase(ctx, stack, entries, doer, false)
			require.NoError(t, err, "trusted conversion retains stack metadata")
			_, err = db.GetEngine(ctx).ID(repo.OwnerID).Cols("visibility").Update(&user_model.User{Visibility: api.VisibleTypePublic})
			require.NoError(t, err)
		}
		_, err := db.GetEngine(ctx).ID(repoID).Cols("is_private").Update(&repo_model.Repository{IsPrivate: true})
		require.NoError(t, err)
		_, err = stackTrunkBase(ctx, stack, entries, doer, true)
		require.ErrorIs(t, err, util.ErrNotExist)
		doer.ExtDoerData = user_model.NewTokenExtDoerData(user_model.CredentialAccessToken, 1, "read:repository")
		_, err = stackTrunkBase(ctx, stack, entries, doer, true)
		require.NoError(t, err, "an unrestricted administrator retains access")
		doer.ExtDoerData = user_model.NewTokenExtDoerData(user_model.CredentialAccessToken, 1, "public-only,read:repository")
		_, err = db.GetEngine(ctx).ID(repoID).Cols("is_private").Update(&repo_model.Repository{IsPrivate: false})
		require.NoError(t, err)
	}
}
