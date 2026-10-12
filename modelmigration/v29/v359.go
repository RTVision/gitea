// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"

	"xorm.io/xorm"
)

func AddStackMirrorBranchAndIssueRedirect(_ context.Context, x base.EngineMigration) error {
	type StackEntry struct {
		MirrorBranch string
	}
	type IssueRedirect struct {
		ID      int64 `xorm:"pk autoincr"`
		RepoID  int64 `xorm:"UNIQUE(repo_index) NOT NULL"`
		Index   int64 `xorm:"UNIQUE(repo_index) NOT NULL"`
		IssueID int64 `xorm:"INDEX NOT NULL"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreConstrains: true, IgnoreDropIndices: true}, new(StackEntry), new(IssueRedirect))
	return err
}
