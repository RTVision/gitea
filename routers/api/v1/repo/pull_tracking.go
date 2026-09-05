// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"gitea.dev/models/issues"
	api "gitea.dev/modules/structs"
	"gitea.dev/services/context"
	"gitea.dev/services/pull"
)

type pullRequestTrackingResponse struct {
	*api.PullRequest
	ReviewDecision *api.PullRequestReviewDecision `json:"review_decision"`
	ChecksState    *api.PullRequestChecksState    `json:"checks_state"`
}

func newPullRequestTrackingResponse(apiPr *api.PullRequest) *pullRequestTrackingResponse {
	return &pullRequestTrackingResponse{
		PullRequest:    apiPr,
		ReviewDecision: apiPr.ReviewDecision,
		ChecksState:    apiPr.ChecksState,
	}
}

func pullRequestResponse(ctx *context.APIContext, apiPr *api.PullRequest) any {
	if !ctx.FormBool("include_tracking") {
		return apiPr
	}
	return newPullRequestTrackingResponse(apiPr)
}

func pullRequestListResponse(ctx *context.APIContext, apiPrs []*api.PullRequest) any {
	if !ctx.FormBool("include_tracking") {
		return apiPrs
	}
	response := make([]*pullRequestTrackingResponse, 0, len(apiPrs))
	for _, apiPr := range apiPrs {
		response = append(response, newPullRequestTrackingResponse(apiPr))
	}
	return response
}

func enrichPullRequestTrackingSummaries(ctx *context.APIContext, prs issues.PullRequestList, apiPrs []*api.PullRequest) error {
	if !ctx.FormBool("include_tracking") || len(prs) == 0 {
		return nil
	}

	headCommitIDs := make(map[int64]string, len(apiPrs))
	for _, apiPr := range apiPrs {
		if apiPr != nil && apiPr.Head != nil {
			headCommitIDs[apiPr.ID] = apiPr.Head.Sha
		}
	}

	summaries, err := pull.GetPullRequestTrackingSummaries(ctx, prs, headCommitIDs)
	if err != nil {
		return err
	}
	for _, apiPr := range apiPrs {
		if apiPr == nil {
			continue
		}
		summary := summaries[apiPr.ID]
		apiPr.ReviewDecision = summary.ReviewDecision
		apiPr.ChecksState = summary.ChecksState
	}
	return nil
}
