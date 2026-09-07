// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues_test

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackMembershipPolicyAndRevision(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	branch, err := issues_model.ResolvePullRequestPolicyBranch(ctx, pr)
	require.NoError(t, err)
	assert.Equal(t, pr.BaseBranch, branch)
	stack := &issues_model.PullRequestStack{RepoID: pr.BaseRepoID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	claim := &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)}
	require.NoError(t, db.Insert(ctx, claim))
	entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, OldParentSHA: "saved-parent"}
	require.NoError(t, db.Insert(ctx, entry))
	branch, err = issues_model.ResolvePullRequestPolicyBranch(ctx, pr)
	require.NoError(t, err)
	assert.Equal(t, "release", branch)
	assert.NotEqual(t, "release", pr.BaseBranch)
	require.NoError(t, issues_model.AdvanceStackRevision(ctx, stack.ID, 1))
	require.ErrorIs(t, issues_model.AdvanceStackRevision(ctx, stack.ID, 1), issues_model.ErrStackRevision)
	op := &issues_model.StackOperation{StackID: stack.ID, ExpectedRevision: 2, Kind: "land", State: "queued"}
	require.NoError(t, issues_model.CreateStackOperation(ctx, op))
	require.ErrorIs(t, issues_model.AdvanceStackRevision(ctx, stack.ID, 2), issues_model.ErrStackRevision)
	_, err = db.GetEngine(ctx).ID(claim.ID).Delete(new(issues_model.StackBranchClaim))
	require.NoError(t, err)
	branch, err = issues_model.ResolvePullRequestPolicyBranch(ctx, pr)
	require.NoError(t, err)
	assert.Equal(t, pr.BaseBranch, branch)
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "saved-parent", entries[0].OldParentSHA)
}

func TestStackClaimsRejectAmbiguousMembership(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	key := issues_model.StackBranchKey(1, "feature")
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: 1, PullRequestID: 1, BranchKey: key}))
	require.Error(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: 2, PullRequestID: 1, BranchKey: issues_model.StackBranchKey(1, "other")}))
	require.Error(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: 2, PullRequestID: 2, BranchKey: key}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: 2, PullRequestID: 2, BranchKey: issues_model.StackBranchKey(2, "feature")}))
}
