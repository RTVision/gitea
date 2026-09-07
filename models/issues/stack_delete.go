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
		ids := builder.Select("id").From("pull_request_stack").Where(builder.Eq{"repo_id": repoID})
		for _, bean := range []any{new(StackEntry), new(StackBranchClaim), new(StackOperation)} {
			if _, err := db.GetEngine(ctx).In("stack_id", ids).Delete(bean); err != nil {
				return err
			}
		}
		_, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Delete(new(PullRequestStack))
		return err
	})
}

// DeleteStacksForPull dissolves grouping when a member is permanently deleted.
func DeleteStacksForPull(ctx context.Context, pullID int64) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		var memberships []*StackEntry
		if err := db.GetEngine(ctx).Where("pull_request_id = ?", pullID).Find(&memberships); err != nil {
			return err
		}
		remaining := map[int64]bool{}
		for _, membership := range memberships {
			n, err := db.GetEngine(ctx).Where("id = ? AND active_operation_id = 0", membership.StackID).Delete(new(PullRequestStack))
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrStackRevision
			}
			entries, err := GetStackEntries(ctx, membership.StackID)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.PullRequestID != pullID {
					remaining[entry.PullRequestID] = true
				}
			}
			for _, bean := range []any{new(StackEntry), new(StackBranchClaim), new(StackOperation)} {
				if _, err := db.GetEngine(ctx).Where("stack_id = ?", membership.StackID).Delete(bean); err != nil {
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
			if err := pr.LoadIssue(ctx); err != nil {
				return err
			}
			if err := RecalculateReviewsOfficial(ctx, pr.Issue); err != nil {
				return err
			}
		}
		return nil
	})
}
