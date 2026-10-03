// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
)

func StackHeadRefName(pullID int64) string {
	return fmt.Sprintf("refs/stack-pulls/%d/head", pullID)
}

func StackComparisonBase(ctx context.Context, pr *PullRequest) (string, error) {
	if pr.HasMerged {
		return pr.MergeBase, nil
	}
	stack, err := GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil || stack.State != StackStateOpen {
		return pr.MergeBase, err
	}
	entry := new(StackEntry)
	has, err := db.GetEngine(ctx).Where("stack_id = ? AND pull_request_id = ?", stack.ID, pr.ID).Get(entry)
	if err != nil {
		return "", err
	}
	if !has || entry.OldParentSHA == "" {
		return "", ErrInvalidStack
	}
	return entry.OldParentSHA, nil
}

func (pr *PullRequest) GetMergedTarget(ctx context.Context) (*repo_model.Repository, string, error) {
	if pr.MergedRepoID == 0 {
		if err := pr.LoadBaseRepo(ctx); err != nil {
			return nil, "", err
		}
		return pr.BaseRepo, pr.BaseBranch, nil
	}
	repo, err := repo_model.GetRepositoryByID(ctx, pr.MergedRepoID)
	return repo, pr.MergedBranch, err
}
