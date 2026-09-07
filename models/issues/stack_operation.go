// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

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

func init() { db.RegisterModel(new(StackOperation)) }

func CreateStackOperation(ctx context.Context, op *StackOperation) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		op.Version = 1
		if _, err := db.GetEngine(ctx).Insert(op); err != nil {
			return err
		}
		n, err := db.GetEngine(ctx).Where("id = ? AND revision = ? AND active_operation_id = 0 AND state = ?", op.StackID, op.ExpectedRevision, StackStateOpen).Cols("active_operation_id").Update(&PullRequestStack{ActiveOperationID: op.ID})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrStackRevision
		}
		return nil
	})
}

func GetStackOperation(ctx context.Context, id int64) (*StackOperation, error) {
	op := new(StackOperation)
	has, err := db.GetEngine(ctx).ID(id).Get(op)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, db.ErrNotExist{Resource: "stack_operation", ID: id}
	}
	return op, nil
}

func SaveStackOperation(ctx context.Context, op *StackOperation) error {
	version := op.Version
	op.Version++
	n, err := db.GetEngine(ctx).Where("id = ? AND version = ?", op.ID, version).Cols("version", "state", "completed", "journal_json", "last_error").Update(op)
	if err != nil || n != 1 {
		op.Version = version
		if err != nil {
			return err
		}
		return ErrStackRevision
	}
	return nil
}

func FinishStackOperation(ctx context.Context, op *StackOperation) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		if err := SaveStackOperation(ctx, op); err != nil {
			return err
		}
		state := StackStateOpen
		if op.State == "completed" || op.State == "cancelled" {
			complete, err := isStackFullyMerged(ctx, op.StackID)
			if err != nil {
				return err
			}
			if complete {
				state = StackStateComplete
				if err := ReleaseStackBranchClaims(ctx, op.StackID); err != nil {
					return err
				}
			}
		}
		n, err := db.GetEngine(ctx).Where("id = ? AND active_operation_id = ? AND revision = ?", op.StackID, op.ID, op.ExpectedRevision).Cols("active_operation_id", "revision", "state").Update(&PullRequestStack{Revision: op.ExpectedRevision + 1, State: state})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrStackRevision
		}
		return nil
	})
}

func GetActiveStackOperations(ctx context.Context) ([]*StackOperation, error) {
	ops := make([]*StackOperation, 0)
	err := db.GetEngine(ctx).Where("state IN (?, ?, ?, ?)", "queued", "running", "waiting", "cancelling").Find(&ops)
	return ops, err
}

func GetStackOperations(ctx context.Context, stackID int64) ([]*StackOperation, error) {
	ops := make([]*StackOperation, 0)
	err := db.GetEngine(ctx).Where("stack_id = ?", stackID).Desc("id").Limit(100).Find(&ops)
	return ops, err
}

func isStackFullyMerged(ctx context.Context, stackID int64) (bool, error) {
	remaining, err := db.GetEngine(ctx).Table(new(StackEntry)).Join("INNER", "pull_request", "pull_request.id = stack_entry.pull_request_id").Where("stack_entry.stack_id = ? AND (pull_request.has_merged = ? OR pull_request.merged_commit_id = ? OR pull_request.merged_commit_id IS NULL)", stackID, false, "").Count(new(StackEntry))
	return remaining == 0, err
}

// RecordStackManualMerge retains landing history and completes a fully merged stack.
func RecordStackManualMerge(ctx context.Context, stackID, revision int64, pr *PullRequest) error {
	if !pr.HasMerged || pr.MergedCommitID == "" {
		return ErrInvalidStack
	}
	return db.WithTx(ctx, func(ctx context.Context) error {
		n, err := db.GetEngine(ctx).Where("stack_id = ? AND pull_request_id = ?", stackID, pr.ID).Cols("landed_commit_sha").Update(&StackEntry{LandedCommitSHA: pr.MergedCommitID})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrInvalidStack
		}
		complete, err := isStackFullyMerged(ctx, stackID)
		if err != nil || !complete {
			return err
		}
		n, err = db.GetEngine(ctx).Where("id = ? AND revision = ? AND active_operation_id = 0 AND state = ?", stackID, revision, StackStateOpen).Cols("state").Update(&PullRequestStack{State: StackStateComplete})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrStackRevision
		}
		return ReleaseStackBranchClaims(ctx, stackID)
	})
}
