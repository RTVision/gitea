// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"

	"gitea.dev/models/db"
)

// IssueRedirect keeps the old repository and index of an issue moved to another repository.
type IssueRedirect struct {
	ID      int64 `xorm:"pk autoincr"`
	RepoID  int64 `xorm:"UNIQUE(repo_index) NOT NULL"`
	Index   int64 `xorm:"UNIQUE(repo_index) NOT NULL"`
	IssueID int64 `xorm:"INDEX NOT NULL"`
}

func init() {
	db.RegisterModel(new(IssueRedirect))
}

// GetRedirectedIssue returns the issue that was moved away from the repository index.
func GetRedirectedIssue(ctx context.Context, repoID, index int64) (*Issue, error) {
	redirect := new(IssueRedirect)
	has, err := db.GetEngine(ctx).Where("repo_id = ? AND `index` = ?", repoID, index).Get(redirect)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrIssueNotExist{RepoID: repoID, Index: index}
	}
	return GetIssueByID(ctx, redirect.IssueID)
}
