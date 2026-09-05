// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/commitstatus"
	api "gitea.dev/modules/structs"
	issue_service "gitea.dev/services/issue"
)

// PullRequestTrackingSummary contains the optional policy and check state
// fields exposed by the pull request API.
type PullRequestTrackingSummary struct {
	ReviewDecision *api.PullRequestReviewDecision
	ChecksState    *api.PullRequestChecksState
}

// GetPullRequestTrackingSummaries evaluates summaries for a batch of pull
// requests. Reviews, protected branch rules, and commit statuses are loaded in
// batches so a list response does not issue one query per pull request.
func GetPullRequestTrackingSummaries(ctx context.Context, prs issues_model.PullRequestList, headCommitIDs map[int64]string) (map[int64]PullRequestTrackingSummary, error) {
	summaries := make(map[int64]PullRequestTrackingSummary, len(prs))
	if len(prs) == 0 {
		return summaries, nil
	}

	issueIDs := make([]int64, 0, len(prs))
	seenIssueIDs := make(map[int64]struct{}, len(prs))
	baseRepoIDs := make(map[int64]struct{}, len(prs))
	for _, pr := range prs {
		if pr.IssueID > 0 {
			if _, ok := seenIssueIDs[pr.IssueID]; !ok {
				issueIDs = append(issueIDs, pr.IssueID)
				seenIssueIDs[pr.IssueID] = struct{}{}
			}
		}
		if pr.BaseRepoID > 0 {
			baseRepoIDs[pr.BaseRepoID] = struct{}{}
		}
	}

	reviewsByIssue, err := loadTrackingReviews(ctx, issueIDs)
	if err != nil {
		return nil, err
	}

	rulesByRepo := make(map[int64]git_model.ProtectedBranchRules, len(baseRepoIDs))
	for repoID := range baseRepoIDs {
		rules, err := git_model.FindRepoProtectedBranchRules(ctx, repoID)
		if err != nil {
			return nil, fmt.Errorf("find protected branch rules for repository %d: %w", repoID, err)
		}
		rulesByRepo[repoID] = rules
	}

	statusesByRepo, err := loadTrackingStatuses(ctx, prs, headCommitIDs)
	if err != nil {
		return nil, err
	}

	for _, pr := range prs {
		var summary PullRequestTrackingSummary
		pb := rulesByRepo[pr.BaseRepoID].GetFirstMatched(pr.BaseBranch)
		if decision := pullRequestReviewDecision(ctx, pb, pr, reviewsByIssue[pr.IssueID]); decision != nil {
			summary.ReviewDecision = decision
		}

		sha := headCommitIDs[pr.ID]
		if sha == "" {
			sha = pr.HeadCommitID
		}
		if sha != "" {
			summary.ChecksState = pullRequestChecksState(statusesByRepo[pr.BaseRepoID][sha])
		}
		summaries[pr.ID] = summary
	}

	return summaries, nil
}

func loadTrackingReviews(ctx context.Context, issueIDs []int64) (map[int64]issues_model.ReviewList, error) {
	reviewsByIssue := make(map[int64]issues_model.ReviewList, len(issueIDs))
	for _, issueID := range issueIDs {
		reviewsByIssue[issueID] = make(issues_model.ReviewList, 0)
	}
	if len(issueIDs) == 0 {
		return reviewsByIssue, nil
	}

	reviews := make(issues_model.ReviewList, 0)
	if err := db.GetEngine(ctx).
		In("issue_id", issueIDs).
		In("type", issues_model.ReviewTypeApprove, issues_model.ReviewTypeReject, issues_model.ReviewTypeRequest).
		Find(&reviews); err != nil {
		return nil, fmt.Errorf("find pull request reviews: %w", err)
	}
	for _, review := range reviews {
		reviewsByIssue[review.IssueID] = append(reviewsByIssue[review.IssueID], review)
	}
	return reviewsByIssue, nil
}

func loadTrackingStatuses(ctx context.Context, prs issues_model.PullRequestList, headCommitIDs map[int64]string) (map[int64]map[string][]*git_model.CommitStatus, error) {
	commitIDsByRepo := make(map[int64][]string)
	seenByRepo := make(map[int64]map[string]struct{})
	for _, pr := range prs {
		sha := headCommitIDs[pr.ID]
		if sha == "" {
			sha = pr.HeadCommitID
		}
		if sha == "" || pr.BaseRepoID <= 0 {
			continue
		}
		if seenByRepo[pr.BaseRepoID] == nil {
			seenByRepo[pr.BaseRepoID] = make(map[string]struct{})
		}
		if _, ok := seenByRepo[pr.BaseRepoID][sha]; ok {
			continue
		}
		seenByRepo[pr.BaseRepoID][sha] = struct{}{}
		commitIDsByRepo[pr.BaseRepoID] = append(commitIDsByRepo[pr.BaseRepoID], sha)
	}

	statusesByRepo := make(map[int64]map[string][]*git_model.CommitStatus, len(commitIDsByRepo))
	for repoID, commitIDs := range commitIDsByRepo {
		statuses, err := git_model.GetLatestCommitStatusForRepoCommitIDs(ctx, repoID, commitIDs)
		if err != nil {
			return nil, fmt.Errorf("find pull request commit statuses for repository %d: %w", repoID, err)
		}
		statusesByRepo[repoID] = statuses
	}
	return statusesByRepo, nil
}

func pullRequestReviewDecision(ctx context.Context, pb *git_model.ProtectedBranch, pr *issues_model.PullRequest, reviews issues_model.ReviewList) *api.PullRequestReviewDecision {
	if pb == nil || !pullRequestReviewPolicyEnabled(pb) {
		return nil
	}

	decision := api.PullRequestReviewApproved
	if issues_model.MergeBlockedByRejectedReviewFromReviews(pb, reviews) {
		decision = api.PullRequestReviewChangesRequested
	} else if !issues_model.HasEnoughApprovalsFromReviews(pb, reviews) ||
		issues_model.MergeBlockedByOfficialReviewRequestsFromReviews(pb, reviews) ||
		(pb.BlockOnCodeownerReviews && !issue_service.HasAllRequiredCodeownerReviewsWithReviews(ctx, pb, pr, reviews)) {
		decision = api.PullRequestReviewRequired
	}
	return &decision
}

func pullRequestReviewPolicyEnabled(pb *git_model.ProtectedBranch) bool {
	return pb.RequiredApprovals > 0 || pb.BlockOnRejectedReviews || pb.BlockOnOfficialReviewRequests || pb.BlockOnCodeownerReviews
}

func pullRequestChecksState(statuses []*git_model.CommitStatus) *api.PullRequestChecksState {
	if len(statuses) == 0 {
		return nil
	}

	hasSuccess := false
	hasPending := false
	for _, status := range statuses {
		if status == nil {
			hasPending = true
			continue
		}
		switch status.State {
		case commitstatus.CommitStatusError, commitstatus.CommitStatusFailure, commitstatus.CommitStatusWarning:
			state := api.PullRequestChecksFailing
			return &state
		case commitstatus.CommitStatusPending:
			hasPending = true
		case commitstatus.CommitStatusSuccess:
			hasSuccess = true
		case commitstatus.CommitStatusSkipped:
		default:
			hasPending = true
		}
	}

	if hasPending {
		state := api.PullRequestChecksPending
		return &state
	}
	if hasSuccess {
		state := api.PullRequestChecksPassing
		return &state
	}
	return nil
}
