// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	perm_model "gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	notify_service "gitea.dev/services/notify"
)

type CreateStackOptions struct {
	TrunkBranch    string
	Mode           string
	PullRequestIDs []int64
}

func checkStackAuthority(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) error {
	if !repo.CanContentChange() {
		return util.NewPermissionDeniedErrorf("stack management requires an editable repository")
	}
	if doer == nil {
		return util.NewPermissionDeniedErrorf("stack management requires repository write access")
	}
	allowed, err := access_model.HasAccessUnit(ctx, doer, repo, unit.TypeCode, perm_model.AccessModeWrite)
	if err != nil {
		return err
	}
	if !allowed {
		return util.NewPermissionDeniedErrorf("stack management requires repository write access")
	}
	return nil
}

// validateStackChain checks pullIDs as a chain on trunk; retargetID names a pull request that will be retargeted to its chain parent.
func validateStackChain(ctx context.Context, repo *repo_model.Repository, trunk, mode string, pullIDs []int64, retargetID int64) ([]*issues_model.StackEntry, error) {
	if trunk == "" || len(pullIDs) == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	parentSHA, err := gitRepo.GetBranchCommitID(ctx, trunk)
	if err != nil {
		return nil, err
	}
	parentBranch := trunk
	var parentID, parentIndex int64
	seenIDs := map[int64]bool{}
	seenBranches := map[string]bool{trunk: true}
	entries := make([]*issues_model.StackEntry, 0, len(pullIDs))
	for i, id := range pullIDs {
		pr, err := issues_model.GetPullRequestByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		if id == retargetID {
			pr.BaseBranch = parentBranch
		}
		if seenIDs[id] || seenBranches[pr.HeadBranch] || pr.HasMerged || pr.Issue.IsClosed || pr.HeadRepoID != repo.ID || pr.BaseRepoID != repo.ID || pr.BaseBranch != parentBranch || pr.Flow != issues_model.PullRequestFlowGithub {
			return nil, fmt.Errorf("%w: pull request #%d does not form an open same-repository chain on %s", issues_model.ErrInvalidStack, pr.Index, parentBranch)
		}
		if scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, id); err != nil {
			return nil, err
		} else if scheduled {
			return nil, issues_model.ErrStackRevision
		}
		headSHA, err := gitRepo.GetBranchCommitID(ctx, pr.HeadBranch)
		if err != nil {
			return nil, err
		}
		boundary, err := git.MergeBase(ctx, gitRepo, parentSHA, headSHA)
		if err != nil {
			return nil, err
		}
		if mode == issues_model.StackModeMerge {
			// Updating the stack merges a parent that moved ahead, so a layer only needs its own commits.
			if boundary == headSHA {
				return nil, fmt.Errorf("%w: pull request #%d has no commits beyond %s", issues_model.ErrInvalidStack, pr.Index, parentBranch)
			}
		} else if boundary != parentSHA || headSHA == parentSHA {
			parent := parentBranch
			if parentIndex != 0 {
				parent = fmt.Sprintf("#%d (%s)", parentIndex, parentBranch)
			}
			return nil, fmt.Errorf("%w: pull request #%d must contain the current head of %s; rebase %s onto it first", issues_model.ErrInvalidStack, pr.Index, parent, pr.HeadBranch)
		} else {
			merges, _, err := gitcmd.NewCommand("rev-list", "--merges").AddDynamicArguments(parentSHA + ".." + headSHA).WithRepo(gitRepo).RunStdString(ctx)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(merges) != "" {
				return nil, fmt.Errorf("%w: pull request #%d has merge commits; use a merge-mode stack or rebase %s", issues_model.ErrInvalidStack, pr.Index, pr.HeadBranch)
			}
		}
		// A head shared by another open PR has ambiguous rewrite ownership.
		count, err := db.GetEngine(ctx).Table("pull_request").Join("INNER", "issue", "issue.id = pull_request.issue_id").Where("pull_request.head_repo_id = ? AND pull_request.head_branch = ? AND issue.is_closed = ? AND pull_request.id <> ?", repo.ID, pr.HeadBranch, false, id).Count(new(issues_model.PullRequest))
		if err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, fmt.Errorf("%w: branch %s has multiple open pull requests", issues_model.ErrInvalidStack, pr.HeadBranch)
		}
		entries = append(entries, &issues_model.StackEntry{PullRequestID: id, Position: i + 1, ParentPullRequestID: parentID, OldParentSHA: boundary, HeadSHA: headSHA})
		seenIDs[id], seenBranches[pr.HeadBranch] = true, true
		parentID, parentIndex, parentBranch, parentSHA = id, pr.Index, pr.HeadBranch, headSHA
	}
	return entries, nil
}

// SuggestStackChain follows base branches down from the top pull request, returning the chain bottom first and its trunk.
func SuggestStackChain(candidates []*issues_model.PullRequest, top int64, defaultBranch string) ([]*issues_model.PullRequest, string) {
	byHead := make(map[string]*issues_model.PullRequest, len(candidates))
	bases := make(map[string]int, len(candidates))
	heads := make(map[string]int, len(candidates))
	var current *issues_model.PullRequest
	for _, pr := range candidates {
		byHead[pr.HeadBranch] = pr
		bases[pr.BaseBranch]++
		heads[pr.HeadBranch]++
		if pr.Index == top {
			current = pr
		}
	}
	if current == nil || heads[current.HeadBranch] > 1 {
		return nil, "" // pull requests sharing a head branch can't be stacked
	}
	var chain []*issues_model.PullRequest
	inChain := make(map[string]bool)
	trunk := ""
	for current != nil {
		chain = append(chain, current)
		inChain[current.HeadBranch] = true
		trunk = current.BaseBranch
		next := byHead[trunk]
		// A branch several pull requests build on, like develop, is a trunk rather than a layer.
		if next == nil || trunk == defaultBranch || bases[trunk] > 1 || heads[trunk] > 1 || inChain[next.BaseBranch] {
			break
		}
		current = next
	}
	slices.Reverse(chain)
	return chain, trunk
}

func insertStackEntries(ctx context.Context, stack *issues_model.PullRequestStack, entries []*issues_model.StackEntry) error {
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return err
		}
		entry.StackID = stack.ID
		if _, err = db.GetEngine(ctx).Insert(entry); err != nil {
			return err
		}
		claim := &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(stack.RepoID, pr.HeadBranch)}
		if _, err = db.GetEngine(ctx).Insert(claim); err != nil {
			return fmt.Errorf("%w: branch or pull request already belongs to a stack: %v", issues_model.ErrInvalidStack, err)
		}
		if err = pr.LoadIssue(ctx); err != nil {
			return err
		}
		if err = issues_model.RecalculateReviewsOfficial(ctx, pr.Issue); err != nil {
			return err
		}
	}
	return nil
}

func CreateStack(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, opts CreateStackOptions) (*issues_model.PullRequestStack, error) {
	if !setting.Repository.PullRequest.EnableStacks {
		return nil, util.NewPermissionDeniedErrorf("stack creation is disabled")
	}
	if err := checkStackAuthority(ctx, doer, repo); err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = issues_model.StackModeRebase
	}
	if opts.Mode != issues_model.StackModeRebase && opts.Mode != issues_model.StackModeMerge {
		return nil, fmt.Errorf("%w: unknown stack mode %q", issues_model.ErrInvalidStack, opts.Mode)
	}
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: opts.TrunkBranch, Mode: opts.Mode, State: issues_model.StackStateOpen, Revision: 1, CreatedByID: doer.ID}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		if err := issues_model.LockStackMembership(ctx, opts.PullRequestIDs...); err != nil {
			return err
		}
		entries, err := validateStackChain(ctx, repo, opts.TrunkBranch, opts.Mode, opts.PullRequestIDs, 0)
		if err != nil {
			return err
		}
		if _, err = db.GetEngine(ctx).Insert(stack); err != nil {
			return err
		}
		return insertStackEntries(ctx, stack, entries)
	})
	if err != nil {
		return nil, err
	}
	notifyStackChanged(ctx, doer, stack.ID, nil)
	return stack, nil
}

func AppendStack(ctx context.Context, doer *user_model.User, stackID, expectedRevision int64, pullIDs []int64) (*issues_model.PullRequestStack, error) {
	if !setting.Repository.PullRequest.EnableStacks {
		return nil, util.NewPermissionDeniedErrorf("stack creation is disabled")
	}
	if len(pullIDs) == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	var stack *issues_model.PullRequestStack
	err := db.WithTx(ctx, func(ctx context.Context) error {
		if err := issues_model.LockStackMembership(ctx, pullIDs...); err != nil {
			return err
		}
		var err error
		stack, err = issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
		if err != nil {
			return err
		}
		if err = checkStackAuthority(ctx, doer, repo); err != nil {
			return err
		}
		existing, err := issues_model.GetStackEntries(ctx, stack.ID)
		if err != nil {
			return err
		}
		allIDs := make([]int64, 0, len(existing)+len(pullIDs))
		for _, entry := range existing {
			pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
			if err != nil {
				return err
			}
			if !pr.HasMerged {
				allIDs = append(allIDs, entry.PullRequestID)
			}
		}
		openCount := len(allIDs)
		if openCount == 0 {
			return issues_model.ErrInvalidStack
		}
		allIDs = append(allIDs, pullIDs...)
		entries, err := validateStackChain(ctx, repo, stack.TrunkBranch, stack.Mode, allIDs, 0)
		if err != nil {
			return err
		}
		if err = issues_model.AdvanceStackRevision(ctx, stack.ID, expectedRevision); err != nil {
			return err
		}
		added := entries[openCount:]
		for i, entry := range added {
			entry.Position = len(existing) + i + 1
		}
		if err = insertStackEntries(ctx, stack, added); err != nil {
			return err
		}
		stack.Revision = expectedRevision + 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	notifyStackChanged(ctx, doer, stack.ID, nil)
	return stack, nil
}

// InsertStackLayer places a pull request directly above the open layer whose branch it targets, or at the bottom when it
// targets the trunk, and retargets the layer that was above that point onto it.
func InsertStackLayer(ctx context.Context, doer *user_model.User, stackID, expectedRevision, pullID int64) (*issues_model.PullRequestStack, error) {
	if !setting.Repository.PullRequest.EnableStacks {
		return nil, util.NewPermissionDeniedErrorf("stack creation is disabled")
	}
	pr, err := issues_model.GetPullRequestByID(ctx, pullID)
	if err != nil {
		return nil, err
	}
	// Lock the layer to retarget, which shares the new layer's base, before the transaction: holders of that lock write to the database.
	var lockedID int64
	if _, err = db.GetEngine(ctx).Table("stack_entry").Join("INNER", "pull_request", "pull_request.id = stack_entry.pull_request_id").
		Where("stack_entry.stack_id = ? AND pull_request.base_branch = ? AND pull_request.has_merged = ?", stackID, pr.BaseBranch, false).
		Cols("pull_request.id").Get(&lockedID); err != nil {
		return nil, err
	}
	if lockedID != 0 {
		releaser, err := globallock.Lock(ctx, getPullWorkingLockKey(lockedID))
		if err != nil {
			return nil, err
		}
		defer releaser()
	}
	var stack *issues_model.PullRequestStack
	var above *issues_model.PullRequest
	var oldBase string
	err = db.WithTx(ctx, func(ctx context.Context) error {
		if err := issues_model.LockStackMembership(ctx, pullID); err != nil {
			return err
		}
		var err error
		stack, err = issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
		if err != nil {
			return err
		}
		if err = checkStackAuthority(ctx, doer, repo); err != nil {
			return err
		}
		if stack.State != issues_model.StackStateOpen || stack.ActiveOperationID != 0 || stack.Revision != expectedRevision {
			return issues_model.ErrStackRevision
		}
		pr, err = issues_model.GetPullRequestByID(ctx, pullID)
		if err != nil {
			return err
		}
		if member, err := issues_model.GetPullRequestStack(ctx, pr.ID); err != nil {
			return err
		} else if member != nil {
			return fmt.Errorf("%w: pull request #%d already belongs to stack #%d", issues_model.ErrInvalidStack, pr.Index, member.ID)
		}
		entries, err := issues_model.GetStackEntries(ctx, stack.ID)
		if err != nil {
			return err
		}
		layers := make([]*issues_model.PullRequest, len(entries))
		at := -1 // index of the entry the new layer goes before
		for i, entry := range entries {
			if layers[i], err = issues_model.GetPullRequestByID(ctx, entry.PullRequestID); err != nil {
				return err
			}
			if layers[i].HasMerged {
				continue
			}
			if at < 0 && pr.BaseBranch == stack.TrunkBranch {
				at = i
			}
			if layers[i].HeadBranch == pr.BaseBranch {
				at = i + 1
			}
		}
		if at < 0 {
			return fmt.Errorf("%w: pull request #%d must target %s or the branch of an open layer", issues_model.ErrInvalidStack, pr.Index, stack.TrunkBranch)
		}
		chain := make([]int64, 0, len(entries)+1)
		for i, layer := range layers {
			if i == at {
				chain = append(chain, pr.ID)
			}
			if i >= at && layer.HasMerged {
				return fmt.Errorf("%w: pull request #%d would be placed below landed pull request #%d", issues_model.ErrInvalidStack, pr.Index, layer.Index)
			}
			if !layer.HasMerged {
				chain = append(chain, layer.ID)
			}
		}
		var aboveID int64
		if at < len(entries) {
			above, aboveID = layers[at], layers[at].ID
		} else {
			chain = append(chain, pr.ID)
		}
		if aboveID != lockedID { // the chain changed after the lock was taken
			return issues_model.ErrStackRevision
		}
		validated, err := validateStackChain(ctx, repo, stack.TrunkBranch, stack.Mode, chain, aboveID)
		if err != nil {
			return err
		}
		if above != nil {
			oldBase = above.BaseBranch
			if err = changeTargetBranchLocked(ctx, above, doer, pr.HeadBranch); err != nil {
				return err
			}
		}
		if err = issues_model.AdvanceStackRevision(ctx, stack.ID, expectedRevision); err != nil {
			return err
		}
		position := entries[len(entries)-1].Position + 1
		if above != nil {
			position = entries[at].Position
		}
		for _, entry := range slices.Backward(entries[at:]) { // descending so UNIQUE(stack_id, position) never collides
			if _, err = db.GetEngine(ctx).ID(entry.ID).Cols("position").Update(&issues_model.StackEntry{Position: entry.Position + 1}); err != nil {
				return err
			}
		}
		index := slices.Index(chain, pr.ID)
		validated[index].Position = position
		if err = insertStackEntries(ctx, stack, validated[index:index+1]); err != nil {
			return err
		}
		if above != nil {
			if _, err = db.GetEngine(ctx).ID(entries[at].ID).Cols("parent_pull_request_id", "old_parent_sha", "head_sha").Update(validated[index+1]); err != nil {
				return err
			}
		}
		stack.Revision = expectedRevision + 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	if above != nil {
		notify_service.PullRequestChangeTargetBranch(ctx, doer, above, oldBase)
	}
	notifyStackChanged(ctx, doer, stack.ID, nil)
	return stack, nil
}

func Unstack(ctx context.Context, doer *user_model.User, stackID, expectedRevision int64) error {
	err := db.WithTx(ctx, func(ctx context.Context) error {
		stack, err := issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
		if err != nil {
			return err
		}
		if err = checkStackAuthority(ctx, doer, repo); err != nil {
			return err
		}
		if err = issues_model.AdvanceStackRevision(ctx, stack.ID, expectedRevision); err != nil {
			return err
		}
		entries, err := issues_model.GetStackEntries(ctx, stack.ID)
		if err != nil {
			return err
		}
		openIDs := make([]int64, 0, len(entries))
		for _, entry := range entries {
			pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
			if err != nil {
				return err
			}
			if pr.HasMerged {
				continue
			}
			openIDs = append(openIDs, pr.ID)
			if queued, _, err := pull_model.GetScheduledMergeByPullID(ctx, pr.ID); err != nil {
				return err
			} else if queued {
				return issues_model.ErrStackRevision
			}
		}
		if len(openIDs) == 0 {
			return issues_model.ErrInvalidStack
		}
		if _, err = db.GetEngine(ctx).Where("stack_id = ?", stack.ID).In("pull_request_id", openIDs).Delete(new(issues_model.StackBranchClaim)); err != nil {
			return err
		}
		if err = issues_model.ReleaseStackBranchClaims(ctx, stack.ID); err != nil {
			return err
		}
		if _, err = db.GetEngine(ctx).ID(stack.ID).Cols("state").Update(&issues_model.PullRequestStack{State: issues_model.StackStateUnstacked}); err != nil {
			return err
		}
		for _, entry := range entries {
			pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
			if err != nil {
				return err
			}
			if err = pr.LoadIssue(ctx); err != nil {
				return err
			}
			if err = issues_model.RecalculateReviewsOfficial(ctx, pr.Issue); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		notifyStackChanged(ctx, doer, stackID, nil)
	}
	return err
}
