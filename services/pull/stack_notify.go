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

func notifyStackChanged(ctx context.Context, doer *user_model.User, stackID int64, changes map[int64][2]string) {
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
		if pr.HasMerged {
			continue
		}
		if err := pr.LoadIssue(ctx); err != nil {
			log.Error("LoadIssue[%d]: %v", pr.ID, err)
			continue
		}
		if pr.Issue.IsClosed {
			continue
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			log.Error("LoadHeadRepo[%d]: %v", pr.ID, err)
			continue
		}
		if heads, ok := changes[pr.ID]; ok {
			notify_service.PullRequestSynchronized(ctx, doer, pr, heads[0], heads[1])
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
