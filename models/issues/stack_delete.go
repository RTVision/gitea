// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"

	"gitea.dev/models/db"

	"xorm.io/builder"
)

func DeleteStacksByRepoID(ctx context.Context, repoID int64) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		var ids []int64
		if err := db.GetEngine(ctx).Table("pull_request_stack").
			Join("LEFT", "stack_entry", "stack_entry.stack_id = pull_request_stack.id").
			Join("LEFT", "pull_request", "pull_request.id = stack_entry.pull_request_id").
			Where(builder.Or(builder.Eq{"pull_request_stack.repo_id": repoID}, builder.Eq{"pull_request.base_repo_id": repoID}, builder.Eq{"pull_request.head_repo_id": repoID})).
			Distinct("pull_request_stack.id").Find(&ids); err != nil {
			return err
		}
		return deleteStacks(ctx, ids, 0, repoID)
	})
}

// DeleteStacksForPull dissolves grouping when a member is permanently deleted.
func DeleteStacksForPull(ctx context.Context, pullID int64) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		var ids []int64
		if err := db.GetEngine(ctx).Table("stack_entry").Where("pull_request_id = ?", pullID).Cols("stack_id").Find(&ids); err != nil {
			return err
		}
		return deleteStacks(ctx, ids, pullID, 0)
	})
}

func deleteStacks(ctx context.Context, ids []int64, deletedPullID, deletedRepoID int64) error {
	remaining := map[int64]bool{}
	for _, id := range ids {
		n, err := db.GetEngine(ctx).Where("id = ? AND active_operation_id = 0", id).Delete(new(PullRequestStack))
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrStackRevision
		}
		entries, err := GetStackEntries(ctx, id)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.PullRequestID != deletedPullID {
				remaining[entry.PullRequestID] = true
			}
		}
		for _, bean := range []any{new(StackEntry), new(StackBranchClaim), new(StackOperation)} {
			if _, err := db.GetEngine(ctx).Where("stack_id = ?", id).Delete(bean); err != nil {
				return err
			}
		}
	}
	for id := range remaining {
		pr, err := GetPullRequestByID(ctx, id)
		if IsErrPullRequestNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if pr.BaseRepoID == deletedRepoID {
			continue
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return err
		}
		if err := RecalculateReviewsOfficial(ctx, pr.Issue); err != nil {
			return err
		}
	}
	return nil
}
