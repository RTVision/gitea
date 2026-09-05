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

func TestStackDeletionCleanup(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	other := &issues_model.PullRequestStack{RepoID: 2, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, other))
	for i, pullID := range []int64{2, 5} {
		require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pullID, Position: i + 1}))
		require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pullID, BranchKey: issues_model.StackBranchKey(1, string(rune('a'+i)))}))
	}
	op := &issues_model.StackOperation{StackID: stack.ID, ExpectedRevision: 1, Kind: "rebase", State: "queued"}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	require.ErrorIs(t, issues_model.DeleteStacksForPull(ctx, 2), issues_model.ErrStackRevision)
	unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID})
	op.State = "cancelled"
	require.NoError(t, issues_model.FinishStackOperation(ctx, op))
	require.NoError(t, issues_model.DeleteStacksForPull(ctx, 2))
	unittest.AssertNotExistsBean(t, &issues_model.StackEntry{StackID: stack.ID})
	unittest.AssertNotExistsBean(t, &issues_model.StackBranchClaim{StackID: stack.ID})
	unittest.AssertNotExistsBean(t, &issues_model.StackOperation{StackID: stack.ID})
	unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 5})
	unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: other.ID})
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: other.ID, PullRequestID: 1, Position: 1}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackOperation{StackID: other.ID, Kind: "rebase", State: "queued"}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: other.ID, PullRequestID: 1, BranchKey: issues_model.StackBranchKey(2, "layer")}))
	require.NoError(t, issues_model.DeleteStacksByRepoID(ctx, 2))
	unittest.AssertNotExistsBean(t, &issues_model.PullRequestStack{RepoID: 2})
	unittest.AssertNotExistsBean(t, &issues_model.StackEntry{StackID: other.ID})
	unittest.AssertNotExistsBean(t, &issues_model.StackOperation{StackID: other.ID})
	unittest.AssertNotExistsBean(t, &issues_model.StackBranchClaim{StackID: other.ID})
}
