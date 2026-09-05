// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
)

type StackHeadExpectation struct {
	PullRequestID int64
	HeadSHA       string
	ParentSHA     string
}

// SynchronizeStack records explicitly published local replay boundaries.
func SynchronizeStack(ctx context.Context, doer *user_model.User, stackID, expectedRevision int64, expected []StackHeadExpectation) (*issues_model.PullRequestStack, error) {
	var stack *issues_model.PullRequestStack
	err := db.WithTx(ctx, func(ctx context.Context) error {
		var err error
		stack, err = issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
		if err != nil {
			return err
		}
		if err := checkStackAuthority(ctx, doer, repo); err != nil {
			return err
		}
		if err := issues_model.AdvanceStackRevision(ctx, stack.ID, expectedRevision); err != nil {
			return err
		}
		entries, err := issues_model.GetStackEntries(ctx, stack.ID)
		if err != nil {
			return err
		}
		var open []*issues_model.StackEntry
		var pullIDs []int64
		for _, entry := range entries {
			pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
			if err != nil {
				return err
			}
			if !pr.HasMerged {
				open = append(open, entry)
				pullIDs = append(pullIDs, entry.PullRequestID)
			}
		}
		if len(expected) == 0 || len(expected) != len(open) {
			return issues_model.ErrInvalidStack
		}
		live, err := validateStackChain(ctx, repo, stack.TrunkBranch, pullIDs)
		if err != nil {
			return err
		}
		for i, entry := range live {
			if expected[i].PullRequestID != entry.PullRequestID || expected[i].HeadSHA != entry.HeadSHA || expected[i].ParentSHA != entry.OldParentSHA {
				return fmt.Errorf("%w: published layer %d no longer matches its expected head and parent", issues_model.ErrStackRevision, open[i].Position)
			}
			if _, err := db.GetEngine(ctx).ID(open[i].ID).Cols("head_sha", "old_parent_sha").Update(entry); err != nil {
				return err
			}
		}
		stack.Revision = expectedRevision + 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	notifyStackChanged(ctx, doer, stack.ID)
	return stack, nil
}
