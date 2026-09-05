// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package convert

import (
	"context"
	"slices"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	api "gitea.dev/modules/structs"
)

func ToAPIPullRequestStackRef(ctx context.Context, pr *issues_model.PullRequest) (*api.PullRequestStackRef, error) {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil {
		return nil, err
	}
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	position := 0
	for _, entry := range entries {
		if entry.PullRequestID == pr.ID {
			position = entry.Position
			break
		}
	}
	if position == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, err
	}
	gitRepo, err := git.OpenRepository(ctx, pr.BaseRepo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	trunkSHA, err := gitRepo.GetBranchCommitID(ctx, stack.TrunkBranch)
	if err != nil {
		if stack.State == issues_model.StackStateOpen {
			return nil, err
		}
		for _, entry := range slices.Backward(entries) {
			if entry.LandedCommitSHA != "" {
				trunkSHA = entry.LandedCommitSHA
				break
			}
		}
	}
	return &api.PullRequestStackRef{
		Number:   stack.ID,
		Size:     len(entries),
		Position: position,
		Base: &api.PullRequestStackBase{
			Ref: stack.TrunkBranch,
			Sha: trunkSHA,
		},
	}, nil
}

func ToAPIPullRequestStack(ctx context.Context, stack *issues_model.PullRequestStack, doer *user_model.User) (*api.PullRequestStack, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	converted := &api.PullRequestStack{
		Number:          stack.ID,
		Trunk:           stack.TrunkBranch,
		State:           stack.State,
		Revision:        stack.Revision,
		ActiveOperation: stack.ActiveOperationID,
		Entries:         make([]*api.PullRequestStackEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		var parentIndex int64
		if entry.ParentPullRequestID != 0 {
			parent, err := issues_model.GetPullRequestByID(ctx, entry.ParentPullRequestID)
			if err != nil {
				return nil, err
			}
			parentIndex = parent.Index
		}
		converted.Entries = append(converted.Entries, &api.PullRequestStackEntry{
			Position:          entry.Position,
			PullRequest:       ToAPIPullRequest(ctx, pr, doer),
			ParentPullRequest: parentIndex,
			HeadSHA:           entry.HeadSHA,
			ParentSHA:         entry.OldParentSHA,
			LandedSHA:         entry.LandedCommitSHA,
		})
	}
	return converted, nil
}

func ToAPIPullRequestStackOperation(op *issues_model.StackOperation) *api.PullRequestStackOperation {
	return &api.PullRequestStackOperation{
		Number:           op.ID,
		StackNumber:      op.StackID,
		ExpectedRevision: op.ExpectedRevision,
		Kind:             op.Kind,
		State:            op.State,
		ThroughPosition:  op.ThroughPosition,
		Completed:        op.Completed,
		MergeStyle:       op.MergeStyle,
		Error:            op.LastError,
		Journal:          []byte(op.JournalJSON),
		Created:          op.CreatedUnix.AsTimePtr(),
		Updated:          op.UpdatedUnix.AsTimePtr(),
	}
}
