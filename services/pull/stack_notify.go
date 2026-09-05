// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	notify_service "gitea.dev/services/notify"
)

func notifyStackChanged(ctx context.Context, doer *user_model.User, stackID int64) {
	entries, err := issues_model.GetStackEntries(ctx, stackID)
	if err != nil {
		log.Error("GetStackEntries[%d]: %v", stackID, err)
		return
	}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			log.Error("GetPullRequestByID[%d]: %v", entry.PullRequestID, err)
			continue
		}
		if pr.HasMerged || pr.LoadIssue(ctx) != nil || pr.Issue.IsClosed || pr.LoadHeadRepo(ctx) != nil {
			continue
		}
		headSHA, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
		if err != nil {
			log.Error("resolve stack pull request head[%d]: %v", pr.ID, err)
			continue
		}
		notify_service.PullRequestSynchronized(ctx, doer, pr, headSHA, headSHA)
	}
}
