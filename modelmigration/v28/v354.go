// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"
)

func AddPullRequestStacks(_ context.Context, x base.EngineMigration) error {
	type PullRequestStack struct {
		ID                int64  `xorm:"pk autoincr"`
		RepoID            int64  `xorm:"INDEX NOT NULL"`
		TrunkBranch       string `xorm:"NOT NULL"`
		State             string `xorm:"VARCHAR(20) NOT NULL"`
		Revision          int64  `xorm:"NOT NULL"`
		ActiveOperationID int64  `xorm:"NOT NULL DEFAULT 0"`
		CreatedByID       int64
		CreatedUnix       timeutil.TimeStamp `xorm:"created"`
		UpdatedUnix       timeutil.TimeStamp `xorm:"updated"`
	}

	type StackEntry struct {
		ID                  int64 `xorm:"pk autoincr"`
		StackID             int64 `xorm:"UNIQUE(stack_position) INDEX NOT NULL"`
		PullRequestID       int64 `xorm:"INDEX NOT NULL"`
		Position            int   `xorm:"UNIQUE(stack_position) NOT NULL"`
		ParentPullRequestID int64
		OldParentSHA        string `xorm:"VARCHAR(64)"`
		HeadSHA             string `xorm:"VARCHAR(64)"`
		LandedCommitSHA     string `xorm:"VARCHAR(64)"`
	}

	type StackBranchClaim struct {
		ID            int64  `xorm:"pk autoincr"`
		StackID       int64  `xorm:"INDEX NOT NULL"`
		PullRequestID int64  `xorm:"UNIQUE NOT NULL"`
		BranchKey     string `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	}

	type StackOperation struct {
		ID               int64 `xorm:"pk autoincr"`
		StackID          int64 `xorm:"INDEX NOT NULL"`
		ActorID          int64
		ExpectedRevision int64
		Version          int64
		Kind             string `xorm:"VARCHAR(20)"`
		State            string `xorm:"VARCHAR(20) INDEX"`
		MergeStyle       string `xorm:"VARCHAR(30)"`
		ThroughPosition  int
		Completed        int
		JournalJSON      string             `xorm:"LONGTEXT"`
		LastError        string             `xorm:"TEXT"`
		CreatedUnix      timeutil.TimeStamp `xorm:"created"`
		UpdatedUnix      timeutil.TimeStamp `xorm:"updated"`
	}
	return x.Sync(new(PullRequestStack), new(StackEntry), new(StackBranchClaim), new(StackOperation))
}
