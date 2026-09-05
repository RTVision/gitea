// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"testing"

	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/commitstatus"
	api "gitea.dev/modules/structs"

	"github.com/stretchr/testify/assert"
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
