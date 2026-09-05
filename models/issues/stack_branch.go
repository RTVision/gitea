// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"crypto/sha256"
	"fmt"

	"gitea.dev/models/db"
	"gitea.dev/modules/util"
)

func ReleaseStackBranchClaims(ctx context.Context, stackID int64) error {
	var claims []*StackBranchClaim
	if err := db.GetEngine(ctx).Where("stack_id = ?", stackID).Find(&claims); err != nil {
		return err
	}
	for _, claim := range claims {
		key := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "history:%d:%d", stackID, claim.PullRequestID)))
		if _, err := db.GetEngine(ctx).ID(claim.ID).Cols("branch_key").Update(&StackBranchClaim{BranchKey: key}); err != nil {
			return err
		}
	}
	return nil
}

var ErrBranchInStack = util.ErrorWrap(util.ErrPermissionDenied, "branch belongs to an open pull request stack; finish or unstack it before deleting or renaming branches")

func CheckStackBranchMutation(ctx context.Context, repoID int64, branch string) error {
	used, err := db.GetEngine(ctx).Where("repo_id = ? AND trunk_branch = ? AND state = ?", repoID, branch, StackStateOpen).Exist(new(PullRequestStack))
	if err != nil {
		return err
	}
	if !used {
		used, err = db.GetEngine(ctx).Table("stack_branch_claim").
			Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_branch_claim.stack_id").
			Where("stack_branch_claim.branch_key = ? AND pull_request_stack.state = ?", StackBranchKey(repoID, branch), StackStateOpen).
			Exist(new(StackBranchClaim))
		if err != nil {
			return err
		}
	}
	if used {
		return ErrBranchInStack
	}
	return nil
}
