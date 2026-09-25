// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues_test

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/require"
)

func TestStackBranchMutationGuard(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: 1, BranchKey: issues_model.StackBranchKey(1, "layer")}))
	for _, branch := range []string{"release", "layer"} {
		require.ErrorIs(t, issues_model.CheckStackBranchMutation(ctx, 1, branch), issues_model.ErrBranchInStack)
		require.NoError(t, issues_model.CheckStackBranchMutation(ctx, 2, branch))
	}
	require.NoError(t, issues_model.CheckStackBranchMutation(ctx, 1, "unrelated"))
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 1, Position: 1}))
	layer, err := issues_model.GetOpenStackLayerByBranch(ctx, 1, "layer")
	require.NoError(t, err)
	require.Equal(t, &issues_model.OpenStackLayer{StackID: stack.ID, Position: 1, IsTop: true}, layer)
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 2, Position: 2}))
	layer, err = issues_model.GetOpenStackLayerByBranch(ctx, 1, "layer")
	require.NoError(t, err)
	require.False(t, layer.IsTop)
	for _, state := range []string{issues_model.StackStateComplete, issues_model.StackStateUnstacked} {
		_, err = db.GetEngine(ctx).ID(stack.ID).Cols("state").Update(&issues_model.PullRequestStack{State: state})
		require.NoError(t, err)
		for _, branch := range []string{"release", "layer"} {
			require.NoError(t, issues_model.CheckStackBranchMutation(ctx, 1, branch))
		}
		layer, err = issues_model.GetOpenStackLayerByBranch(ctx, 1, "layer")
		require.NoError(t, err)
		require.Nil(t, layer)
	}
	require.NoError(t, issues_model.ReleaseStackBranchClaims(ctx, stack.ID))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID + 1, PullRequestID: 2, BranchKey: issues_model.StackBranchKey(1, "layer")}))
	history, err := issues_model.GetPullRequestStack(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, stack.ID, history.ID)
}
