// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"
)

var (
	ErrStackNotExist = fmt.Errorf("stack does not exist: %w", util.ErrNotExist)
	ErrStackRevision = errors.New("stack revision changed or operation in progress")
	ErrInvalidStack  = errors.New("invalid pull request stack")
)

// LockStackMembership must precede transaction reads so membership and auto-merge
// eligibility use a snapshot taken after competing reservations have committed.
func LockStackMembership(ctx context.Context, pullIDs ...int64) error {
	if !db.InTransaction(ctx) {
		return errors.New("stack membership reservation requires a transaction")
	}
	pullIDs = slices.Clone(pullIDs)
	slices.Sort(pullIDs)
	for _, id := range slices.Compact(pullIDs) {
		if _, err := db.GetEngine(ctx).ID(id).NoAutoTime().SetExpr("id", "id").Update(new(PullRequest)); err != nil {
			return err
		}
	}
	return nil
}

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

const (
	StackStateOpen      = "open"
	StackStateComplete  = "complete"
	StackStateUnstacked = "unstacked"
)

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

// StackBranchClaim separates current ownership from immutable membership history.
type StackBranchClaim struct {
	ID            int64  `xorm:"pk autoincr"`
	StackID       int64  `xorm:"INDEX NOT NULL"`
	PullRequestID int64  `xorm:"UNIQUE NOT NULL"`
	BranchKey     string `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
}

func init() {
	db.RegisterModel(new(PullRequestStack))
	db.RegisterModel(new(StackEntry))
	db.RegisterModel(new(StackBranchClaim))
}

func StackBranchKey(repoID int64, branch string) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%d:%s", repoID, branch)))
}

func GetStackByID(ctx context.Context, id int64) (*PullRequestStack, error) {
	stack := new(PullRequestStack)
	has, err := db.GetEngine(ctx).ID(id).Get(stack)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrStackNotExist
	}
	return stack, nil
}

func GetStackEntries(ctx context.Context, stackID int64) ([]*StackEntry, error) {
	entries := make([]*StackEntry, 0)
	err := db.GetEngine(ctx).Where("stack_id = ?", stackID).Asc("position").Find(&entries)
	return entries, err
}

func GetPullRequestStack(ctx context.Context, pullID int64) (*PullRequestStack, error) {
	claim := new(StackBranchClaim)
	has, err := db.GetEngine(ctx).Where("pull_request_id = ?", pullID).Get(claim)
	if err != nil || !has {
		return nil, err
	}
	return GetStackByID(ctx, claim.StackID)
}

func ResolvePullRequestPolicyBranch(ctx context.Context, pr *PullRequest) (string, error) {
	branches, err := ResolvePullRequestPolicyBranches(ctx, PullRequestList{pr})
	return branches[pr.ID], err
}

func ResolvePullRequestPolicyBranches(ctx context.Context, prs PullRequestList) (map[int64]string, error) {
	branches := make(map[int64]string, len(prs))
	if len(prs) == 0 {
		return branches, nil
	}
	pullIDs := make([]int64, 0, len(prs))
	for _, pr := range prs {
		pullIDs = append(pullIDs, pr.ID)
		branches[pr.ID] = pr.BaseBranch
	}
	var memberships []struct {
		PullRequestID int64
		StackID       int64
		RepoID        int64
		TrunkBranch   string
	}
	if err := db.GetEngine(ctx).Table("stack_branch_claim").
		Join("LEFT", "pull_request_stack", "pull_request_stack.id = stack_branch_claim.stack_id").
		In("stack_branch_claim.pull_request_id", pullIDs).
		And("pull_request_stack.state = ? OR pull_request_stack.id IS NULL", StackStateOpen).
		Select("stack_branch_claim.pull_request_id, pull_request_stack.id AS stack_id, pull_request_stack.repo_id, pull_request_stack.trunk_branch").
		Find(&memberships); err != nil {
		return nil, err
	}
	prsByID := make(map[int64]*PullRequest, len(prs))
	for _, pr := range prs {
		prsByID[pr.ID] = pr
	}
	for _, membership := range memberships {
		if membership.StackID == 0 {
			return nil, ErrStackNotExist
		}
		if membership.RepoID != prsByID[membership.PullRequestID].BaseRepoID {
			return nil, ErrInvalidStack
		}
		branches[membership.PullRequestID] = membership.TrunkBranch
	}
	return branches, nil
}

func ListStacks(ctx context.Context, repoID int64, opts db.ListOptions) ([]*PullRequestStack, int64, error) {
	stacks := make([]*PullRequestStack, 0)
	count, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Count(new(PullRequestStack))
	if err != nil {
		return nil, 0, err
	}
	sess := db.GetEngine(ctx).Where("repo_id = ?", repoID).Desc("id")
	if !opts.IsListAll() {
		db.SetSessionPagination(sess, &opts)
	}
	err = sess.Find(&stacks)
	return stacks, count, err
}

// AdvanceStackRevision reserves a structural edit inside the caller's transaction.
func AdvanceStackRevision(ctx context.Context, id, revision int64) error {
	affected, err := db.GetEngine(ctx).Where("id = ? AND revision = ? AND active_operation_id = 0 AND state = ?", id, revision, StackStateOpen).Cols("revision").Update(&PullRequestStack{Revision: revision + 1})
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrStackRevision
	}
	return nil
}
