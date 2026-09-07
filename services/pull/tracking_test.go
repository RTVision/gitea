// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	api "gitea.dev/modules/structs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullRequestReviewDecision(t *testing.T) {
	pr := &issues_model.PullRequest{}

	assert.Nil(t, pullRequestReviewDecision(t.Context(), &git_model.ProtectedBranch{}, pr, nil))

	protectBranch := &git_model.ProtectedBranch{
		RequiredApprovals:      1,
		BlockOnRejectedReviews: true,
	}
	decision := pullRequestReviewDecision(t.Context(), protectBranch, pr, issues_model.ReviewList{
		{Type: issues_model.ReviewTypeApprove, Official: true},
		{Type: issues_model.ReviewTypeReject, Official: true},
	})
	assert.Equal(t, api.PullRequestReviewChangesRequested, *decision)

	decision = pullRequestReviewDecision(t.Context(), protectBranch, pr, issues_model.ReviewList{
		{Type: issues_model.ReviewTypeApprove, Official: true},
	})
	assert.Equal(t, api.PullRequestReviewApproved, *decision)

	protectBranch.BlockOnRejectedReviews = false
	decision = pullRequestReviewDecision(t.Context(), protectBranch, pr, nil)
	assert.Equal(t, api.PullRequestReviewRequired, *decision)
}

func TestPullRequestChecksState(t *testing.T) {
	tests := []struct {
		name     string
		statuses []*git_model.CommitStatus
		want     *api.PullRequestChecksState
	}{
		{name: "no checks", statuses: nil, want: nil},
		{name: "skipped only", statuses: []*git_model.CommitStatus{{State: commitstatus.CommitStatusSkipped}}, want: nil},
		{name: "passing", statuses: []*git_model.CommitStatus{{State: commitstatus.CommitStatusSuccess}, {State: commitstatus.CommitStatusSkipped}}, want: new(api.PullRequestChecksPassing)},
		{name: "pending", statuses: []*git_model.CommitStatus{{State: commitstatus.CommitStatusPending}, {State: commitstatus.CommitStatusSuccess}}, want: new(api.PullRequestChecksPending)},
		{name: "failure wins", statuses: []*git_model.CommitStatus{{State: commitstatus.CommitStatusPending}, {State: commitstatus.CommitStatusFailure}}, want: new(api.PullRequestChecksFailing)},
		{name: "warning fails", statuses: []*git_model.CommitStatus{{State: commitstatus.CommitStatusWarning}}, want: new(api.PullRequestChecksFailing)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, pullRequestChecksState(test.statuses))
		})
	}
}

func TestTrackingStackPolicyAndSupersededReviews(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	other := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	pb := &git_model.ProtectedBranch{RepoID: pr.BaseRepoID, RuleName: "release", RequiredApprovals: 1, BlockOnRejectedReviews: true}
	require.NoError(t, db.Insert(ctx, pb))
	stack := &issues_model.PullRequestStack{RepoID: pr.BaseRepoID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)}))
	require.NotEqual(t, stack.TrunkBranch, pr.BaseBranch)
	_, err := db.GetEngine(ctx).Where("issue_id = ?", pr.IssueID).Delete(new(issues_model.Review))
	require.NoError(t, err)
	prs := issues_model.PullRequestList{pr, other}
	summaries, err := GetPullRequestTrackingSummaries(ctx, prs, nil)
	require.NoError(t, err)
	require.NotNil(t, summaries[pr.ID].ReviewDecision)
	assert.Equal(t, api.PullRequestReviewRequired, *summaries[pr.ID].ReviewDecision)
	assert.Nil(t, summaries[other.ID].ReviewDecision)
	require.NoError(t, pr.LoadIssue(ctx))
	require.NoError(t, pr.Issue.LoadRepo(ctx))
	reviewer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	for _, reviewType := range []issues_model.ReviewType{issues_model.ReviewTypeReject, issues_model.ReviewTypeApprove, issues_model.ReviewTypeReject} {
		review, _, err := issues_model.SubmitReview(ctx, reviewer, pr.Issue, reviewType, "review decision", "", false, nil)
		require.NoError(t, err)
		require.True(t, review.Official)
		summaries, err = GetPullRequestTrackingSummaries(ctx, prs, nil)
		require.NoError(t, err)
		want := api.PullRequestReviewChangesRequested
		if reviewType == issues_model.ReviewTypeApprove {
			want = api.PullRequestReviewApproved
		}
		assert.Equal(t, want, *summaries[pr.ID].ReviewDecision)
		assert.Equal(t, reviewType == issues_model.ReviewTypeReject, issues_model.MergeBlockedByRejectedReview(ctx, pb, pr))
		assert.Equal(t, reviewType == issues_model.ReviewTypeApprove, issues_model.HasEnoughApprovals(ctx, pb, pr))
	}
}
