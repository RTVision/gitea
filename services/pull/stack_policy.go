// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"

	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
)

var ErrPullRequestStacked = errors.New("use the stack operation to merge, update or retarget this pull request")

func getPullProtectedBranch(ctx context.Context, pr *issues_model.PullRequest) (*git_model.ProtectedBranch, error) {
	branch, err := issues_model.ResolvePullRequestPolicyBranch(ctx, pr)
	if err != nil {
		return nil, err
	}
	return git_model.GetFirstMatchProtectedBranchRule(ctx, pr.BaseRepoID, branch)
}

func checkOrdinaryStackMutation(ctx context.Context, pr *issues_model.PullRequest) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil {
		return err
	}
	if stack != nil {
		return ErrPullRequestStacked
	}
	return nil
}

func checkStackMergeOrder(ctx context.Context, pr *issues_model.PullRequest) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil {
		return err
	}
	if pr.BaseBranch != stack.TrunkBranch {
		return ErrPullRequestStacked
	}
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.PullRequestID == pr.ID {
			return nil
		}
		parent, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return err
		}
		if !parent.HasMerged || parent.MergedCommitID == "" {
			return ErrPullRequestStacked
		}
	}
	return issues_model.ErrInvalidStack
}

func validateStackOperation(ctx context.Context, pr *issues_model.PullRequest, opID, actorID int64) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil {
		return err
	}
	if stack == nil || stack.ActiveOperationID != opID || opID == 0 {
		return ErrPullRequestStacked
	}
	op, err := issues_model.GetStackOperation(ctx, opID)
	if err != nil {
		return err
	}
	if op.StackID != stack.ID || op.ActorID != actorID || op.ExpectedRevision != stack.Revision || op.State != "running" {
		return issues_model.ErrStackRevision
	}
	return nil
}
