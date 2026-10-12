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
	Pending bool  `xorm:"INDEX NOT NULL DEFAULT false"` // the new repository has not been notified of the move yet
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

// GetPendingIssueRedirects lists moves whose new repository has not been notified yet.
func GetPendingIssueRedirects(ctx context.Context) ([]*IssueRedirect, error) {
	redirects := make([]*IssueRedirect, 0)
	return redirects, db.GetEngine(ctx).Where("pending = ?", true).Find(&redirects)
}

// SettleIssueRedirect records that the new repository of a moved issue was notified.
func SettleIssueRedirect(ctx context.Context, issueID int64) error {
	_, err := db.GetEngine(ctx).Where("issue_id = ? AND pending = ?", issueID, true).Cols("pending").Update(&IssueRedirect{Pending: false})
	return err
}

// IsIssueRedirectPending reports whether the move of an issue still awaits notifying its new repository.
func IsIssueRedirectPending(ctx context.Context, issueID int64) (bool, error) {
	return db.GetEngine(ctx).Where("issue_id = ? AND pending = ?", issueID, true).Exist(new(IssueRedirect))
}
