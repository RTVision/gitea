// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/globallock"
	repo_module "gitea.dev/modules/repository"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	notify_service "gitea.dev/services/notify"
)

type CreateStackOptions struct {
	TrunkBranch    string
	Mode           string
	PullRequestIDs []int64
}

// StackLayerCheck reports what keeps a pull request from stacking on its parent.
type StackLayerCheck struct {
	Pull         *issues_model.PullRequest
	ParentBranch string
	ParentIndex  int64 // zero when the parent is the trunk
	Entry        *issues_model.StackEntry
	Invalid      error // rules the layer out in both modes
	Behind       bool  // lacks the parent's head, which rebase mode requires
	HasMerges    bool  // rebase mode keeps layer history linear
}

func (c *StackLayerCheck) err(mode string) error {
	switch {
	case c.Invalid != nil:
		return c.Invalid
	case mode == issues_model.StackModeMerge:
		return nil // updating the stack merges a parent that moved ahead, so a layer only needs its own commits
	case c.Behind:
		parent := c.ParentBranch
		if c.ParentIndex != 0 {
			parent = fmt.Sprintf("#%d (%s)", c.ParentIndex, c.ParentBranch)
		}
		return fmt.Errorf("%w: pull request #%d must contain the current head of %s; rebase %s onto it first", issues_model.ErrInvalidStack, c.Pull.Index, parent, c.Pull.HeadBranch)
	case c.HasMerges:
		return fmt.Errorf("%w: pull request #%d has merge commits; use a merge-mode stack or rebase %s", issues_model.ErrInvalidStack, c.Pull.Index, c.Pull.HeadBranch)
	}
	return nil
}

// CheckStackChain checks each layer against its parent; an empty mode checks what either mode needs.
// retargetID names a pull request checked as if already retargeted to its chain parent.
// On error, the checks cover the layers below the one that failed.
func CheckStackChain(ctx context.Context, repo *repo_model.Repository, trunk, mode string, pullIDs []int64, retargetID int64) ([]*StackLayerCheck, error) {
	if trunk == "" || len(pullIDs) == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	tmpPath, gitRepo, cleanup, err := repo_module.CreateTemporaryGitRepo("stack-check")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := git.InitRepositoryLocal(ctx, tmpPath, false, repo.ObjectFormatName, ""); err != nil {
		return nil, err
	}
	parentSHA, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+trunk)
	if err != nil {
		return nil, err
	}
	if err := fetchStackObject(ctx, gitRepo, repo, parentSHA, "refs/heads/trunk"); err != nil {
		return nil, err
	}
	parentRepoID := repo.ID
	var sourceRepoID int64
	parentBranch := trunk
	var parentID, parentIndex int64
	seenIDs := map[int64]bool{}
	seenBranches := map[string]bool{issues_model.StackBranchKey(repo.ID, trunk): true}
	checks := make([]*StackLayerCheck, 0, len(pullIDs))
	for i, id := range pullIDs {
		pr, err := issues_model.GetPullRequestByID(ctx, id)
		if err != nil {
			return checks[:i], err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return checks[:i], err
		}
		if id == retargetID && pr.BaseRepoID == parentRepoID {
			pr.BaseBranch = parentBranch
		}
		baseMatches := pr.BaseRepoID == parentRepoID && pr.BaseBranch == parentBranch
		if i == 0 && !baseMatches {
			member, err := issues_model.GetPullRequestStack(ctx, pr.ID)
			if err != nil {
				return checks[:i], err
			}
			if member != nil && member.RepoID == repo.ID && member.TrunkBranch == trunk {
				entries, err := issues_model.GetStackEntries(ctx, member.ID)
				if err != nil {
					return checks[:i], err
				}
				for _, entry := range entries {
					if entry.PullRequestID != pr.ID || entry.ParentPullRequestID == 0 {
						continue
					}
					parent, err := issues_model.GetPullRequestByID(ctx, entry.ParentPullRequestID)
					if err != nil {
						return checks[:i], err
					}
					if parent.HasMerged && parent.MergedCommitID != "" {
						mergedRepo, mergedBranch, err := parent.GetMergedTarget(ctx)
						if err != nil {
							return checks[:i], err
						}
						baseMatches = mergedRepo.ID == repo.ID && mergedBranch == trunk && pr.BaseRepoID == parent.HeadRepoID && pr.BaseBranch == parent.HeadBranch
					}
				}
			}
		}
		branchKey := issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)
		check := &StackLayerCheck{Pull: pr, ParentBranch: parentBranch, ParentIndex: parentIndex}
		checks = append(checks, check)
		if seenIDs[id] || seenBranches[branchKey] || pr.HasMerged || pr.Issue.IsClosed || !baseMatches || pr.Flow != issues_model.PullRequestFlowGithub {
			check.Invalid = fmt.Errorf("%w: pull request #%d does not form an open repository-qualified chain on %s", issues_model.ErrInvalidStack, pr.Index, parentBranch)
			return checks, nil // later layers have no parent to check against
		}
		if scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, id); err != nil {
			return checks[:i], err
		} else if scheduled {
			check.Invalid = fmt.Errorf("%w: pull request #%d is scheduled to auto-merge", issues_model.ErrStackRevision, pr.Index)
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return checks[:i], err
		}
		if pr.HeadRepo == nil || pr.HeadRepo.ObjectFormatName != repo.ObjectFormatName || pr.HeadRepo.IsPrivate != repo.IsPrivate || (pr.HeadRepoID != repo.ID && (!pr.HeadRepo.IsFork || pr.HeadRepo.ForkID != repo.ID)) || (sourceRepoID != 0 && sourceRepoID != pr.HeadRepoID) {
			check.Invalid = fmt.Errorf("%w: all source branches must belong to one fork of the stack repository with matching visibility", issues_model.ErrInvalidStack)
			return checks, nil
		}
		sourceRepoID = pr.HeadRepoID
		headSHA, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
		if err != nil {
			return checks[:i], err
		}
		if err := fetchStackObject(ctx, gitRepo, pr.HeadRepo, headSHA, issues_model.StackHeadRefName(pr.ID)); err != nil {
			return checks[:i], err
		}
		boundary, err := git.MergeBase(ctx, gitRepo, parentSHA, headSHA)
		if err != nil {
			return checks[:i], err
		}
		check.Behind = boundary != parentSHA
		if boundary == headSHA && check.Invalid == nil {
			check.Invalid = fmt.Errorf("%w: pull request #%d has no commits beyond %s", issues_model.ErrInvalidStack, pr.Index, parentBranch)
		}
		if mode == "" || (mode == issues_model.StackModeRebase && !check.Behind && check.Invalid == nil) {
			merges, _, err := gitcmd.NewCommand("rev-list", "--merges", "--max-count=1").AddDynamicArguments(parentSHA + ".." + headSHA).WithRepo(gitRepo).RunStdString(ctx)
			if err != nil {
				return checks[:i], err
			}
			check.HasMerges = strings.TrimSpace(merges) != ""
		}
		// A head shared by another open PR has ambiguous rewrite ownership.
		count, err := db.GetEngine(ctx).Table("pull_request").Join("INNER", "issue", "issue.id = pull_request.issue_id").Where("pull_request.head_repo_id = ? AND pull_request.head_branch = ? AND issue.is_closed = ? AND pull_request.id <> ?", pr.HeadRepoID, pr.HeadBranch, false, id).Count(new(issues_model.PullRequest))
		if err != nil {
			return checks[:i], err
		}
		if count != 0 && check.Invalid == nil {
			check.Invalid = fmt.Errorf("%w: branch %s has multiple open pull requests", issues_model.ErrInvalidStack, pr.HeadBranch)
		}
		check.Entry = &issues_model.StackEntry{PullRequestID: id, Position: i + 1, ParentPullRequestID: parentID, OldParentSHA: boundary, HeadSHA: headSHA}
		seenIDs[id], seenBranches[branchKey] = true, true
		parentRepoID = pr.HeadRepoID
		parentID, parentIndex, parentBranch, parentSHA = id, pr.Index, pr.HeadBranch, headSHA
	}
	return checks, nil
}

func validateStackChain(ctx context.Context, repo *repo_model.Repository, trunk, mode string, pullIDs []int64, retargetID int64) ([]*issues_model.StackEntry, error) {
	checks, err := CheckStackChain(ctx, repo, trunk, mode, pullIDs, retargetID)
	if err != nil {
		return nil, err
	}
	entries := make([]*issues_model.StackEntry, 0, len(checks))
	for _, check := range checks {
		if err := check.err(mode); err != nil {
			return nil, err
		}
		entries = append(entries, check.Entry)
	}
	return entries, nil
}

// SuggestStackChain follows base branches down from the top pull request and returns the chain bottom first.
// The suggested start is the layer just above the highest branch that several pull requests build on, like develop.
func SuggestStackChain(candidates []*issues_model.PullRequest, top int64, defaultBranch string) ([]*issues_model.PullRequest, int) {
	for _, pr := range candidates {
		if pr.Index == top {
			return suggestStackChain(candidates, top, defaultBranch, pr.BaseRepoID, false)
		}
	}
	return nil, 0
}

func SuggestStackChainByID(candidates []*issues_model.PullRequest, top int64, defaultBranch string, trunkRepoID int64) ([]*issues_model.PullRequest, int) {
	return suggestStackChain(candidates, top, defaultBranch, trunkRepoID, true)
}

func suggestStackChain(candidates []*issues_model.PullRequest, top int64, defaultBranch string, trunkRepoID int64, byID bool) (chain []*issues_model.PullRequest, start int) {
	byHead := make(map[string]*issues_model.PullRequest, len(candidates))
	bases, heads := map[string]int{}, map[string]int{}
	var current *issues_model.PullRequest
	for _, pr := range candidates {
		head := issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)
		base := issues_model.StackBranchKey(pr.BaseRepoID, pr.BaseBranch)
		byHead[head] = pr
		bases[base]++
		heads[head]++
		if (byID && pr.ID == top) || (!byID && pr.Index == top) {
			current = pr
		}
	}
	if current == nil || heads[issues_model.StackBranchKey(current.HeadRepoID, current.HeadBranch)] > 1 {
		return nil, 0
	}
	inChain := map[string]bool{}
	shared := -1
	for current != nil {
		chain = append(chain, current)
		inChain[issues_model.StackBranchKey(current.HeadRepoID, current.HeadBranch)] = true
		base := issues_model.StackBranchKey(current.BaseRepoID, current.BaseBranch)
		if shared < 0 && bases[base] > 1 {
			shared = len(chain) - 1
		}
		next := byHead[base]
		if next == nil || heads[base] > 1 || inChain[issues_model.StackBranchKey(next.BaseRepoID, next.BaseBranch)] || (current.BaseBranch == defaultBranch && current.BaseRepoID == trunkRepoID) {
			break
		}
		current = next
	}
	slices.Reverse(chain)
	if shared >= 0 {
		start = len(chain) - 1 - shared
	}
	return chain, start
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
		claim := &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)}
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
	if err := checkStackAuthority(ctx, doer, repo, opts.PullRequestIDs...); err != nil {
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
		if err := checkStackAuthority(ctx, doer, repo, opts.PullRequestIDs...); err != nil {
			return err
		}
		if err := checkStackPullRead(ctx, doer, opts.PullRequestIDs); err != nil {
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
		if err = checkStackManagement(ctx, doer, stack, pullIDs...); err != nil {
			return err
		}
		if err := checkStackMemberRead(ctx, doer, stack.ID, pullIDs...); err != nil {
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

// stackInsertPoint loads the stack's layers and returns the index of the entry a pull request goes before.
func stackInsertPoint(ctx context.Context, stack *issues_model.PullRequestStack, entries []*issues_model.StackEntry, pr *issues_model.PullRequest) ([]*issues_model.PullRequest, int, error) {
	layers := make([]*issues_model.PullRequest, len(entries))
	at := -1
	for i, entry := range entries {
		var err error
		if layers[i], err = issues_model.GetPullRequestByID(ctx, entry.PullRequestID); err != nil {
			return nil, 0, err
		}
		if layers[i].HasMerged {
			continue
		}
		if at < 0 && pr.BaseRepoID == stack.RepoID && pr.BaseBranch == stack.TrunkBranch {
			at = i
		}
		if layers[i].HeadRepoID == pr.BaseRepoID && layers[i].HeadBranch == pr.BaseBranch {
			at = i + 1
		}
	}
	if at < 0 {
		return nil, 0, fmt.Errorf("%w: pull request #%d must target %s or the branch of an open layer", issues_model.ErrInvalidStack, pr.Index, stack.TrunkBranch)
	}
	for _, layer := range layers[at:] {
		if layer.HasMerged {
			return nil, 0, fmt.Errorf("%w: pull request #%d would be placed below landed pull request #%d", issues_model.ErrInvalidStack, pr.Index, layer.Index)
		}
	}
	return layers, at, nil
}

// InsertStackLayer places a pull request directly above the open layer whose branch it targets, or at the bottom when it
// targets the trunk, and retargets the layer that was above that point onto it.
func InsertStackLayer(ctx context.Context, doer *user_model.User, stackID, expectedRevision, pullID int64) (*issues_model.PullRequestStack, error) {
	if !setting.Repository.PullRequest.EnableStacks {
		return nil, util.NewPermissionDeniedErrorf("stack creation is disabled")
	}
	stack, err := issues_model.GetStackByID(ctx, stackID)
	if err != nil {
		return nil, err
	}
	if stack.State != issues_model.StackStateOpen || stack.ActiveOperationID != 0 || stack.Revision != expectedRevision {
		return nil, issues_model.ErrStackRevision
	}
	if err := checkStackMemberRead(ctx, doer, stack.ID, pullID); err != nil {
		return nil, err
	}
	pr, err := issues_model.GetPullRequestByID(ctx, pullID)
	if err != nil {
		return nil, err
	}
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	layers, at, err := stackInsertPoint(ctx, stack, entries, pr)
	if err != nil {
		return nil, err
	}
	// Lock the layer to retarget before the transaction: holders of that lock write to the database.
	var lockedID int64
	if at < len(layers) {
		lockedID = layers[at].ID
		releaser, err := globallock.Lock(ctx, getPullWorkingLockKey(lockedID))
		if err != nil {
			return nil, err
		}
		defer releaser()
	}
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
		if err = checkStackManagement(ctx, doer, stack, pullID); err != nil {
			return err
		}
		if err := checkStackMemberRead(ctx, doer, stack.ID, pullID); err != nil {
			return err
		}
		if err = issues_model.AdvanceStackRevision(ctx, stack.ID, expectedRevision); err != nil {
			return err
		}
		if pr, err = issues_model.GetPullRequestByID(ctx, pullID); err != nil {
			return err
		}
		if member, err := issues_model.GetPullRequestStack(ctx, pr.ID); err != nil {
			return err
		} else if member != nil {
			return fmt.Errorf("%w: pull request #%d already belongs to stack #%d", issues_model.ErrInvalidStack, pr.Index, member.ID)
		}
		if entries, err = issues_model.GetStackEntries(ctx, stack.ID); err != nil {
			return err
		}
		if layers, at, err = stackInsertPoint(ctx, stack, entries, pr); err != nil {
			return err
		}
		chain := make([]int64, 0, len(entries)+1)
		for i, layer := range layers {
			if i == at {
				chain = append(chain, pr.ID)
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
			if above.BaseRepoID != pr.HeadRepoID {
				return fmt.Errorf("%w: inserting this layer would change the next PR's repository", issues_model.ErrInvalidStack)
			}
			oldBase = above.BaseBranch
			if err = changeTargetBranchLocked(ctx, above, doer, pr.HeadBranch); err != nil {
				return err
			}
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

type StackInsertCandidate struct {
	Pull  *issues_model.PullRequest
	After *issues_model.PullRequest // nil at the bottom
}

// StackInsertCandidates lists up to limit unclaimed pull requests based on an open layer's branch, or on the trunk when
// they share commits with the bottom open layer so an unrelated pull request isn't offered below it.
func StackInsertCandidates(ctx context.Context, stack *issues_model.PullRequestStack, limit int) ([]*StackInsertCandidate, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	anchors := map[string]*issues_model.PullRequest{}
	var bottom *issues_model.PullRequest
	for _, entry := range entries {
		layer, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if !layer.HasMerged {
			anchors[issues_model.StackBranchKey(layer.HeadRepoID, layer.HeadBranch)] = layer
			bottom = cmp.Or(bottom, layer)
		}
	}
	if bottom == nil {
		return nil, nil
	}
	branches := []string{stack.TrunkBranch}
	for _, layer := range anchors {
		branches = append(branches, layer.HeadBranch)
	}
	pulls, err := issues_model.FindStackInsertCandidatePulls(ctx, stack.RepoID, branches, limit)
	if err != nil || len(pulls) == 0 {
		return nil, err
	}
	trunkRepo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return nil, err
	}
	if err := bottom.LoadHeadRepo(ctx); err != nil {
		return nil, err
	}
	candidates := make([]*StackInsertCandidate, 0, len(pulls))
	var trunkSHA string
	for _, pr := range pulls {
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return nil, err
		}
		if pr.HeadRepoID != bottom.HeadRepoID {
			continue
		}
		if pr.BaseRepoID == stack.RepoID && pr.BaseBranch == stack.TrunkBranch {
			if trunkSHA == "" {
				trunkSHA, err = git.GetFullCommitID(ctx, trunkRepo, git.BranchPrefix+stack.TrunkBranch)
				if err != nil {
					return nil, err
				}
				if err := fetchStackObject(ctx, bottom.HeadRepo, trunkRepo, trunkSHA, "refs/stack-trunks/"+strconv.FormatInt(stack.ID, 10)); err != nil {
					return nil, err
				}
			}
			shared, err := git.MergeBase(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch, git.BranchPrefix+bottom.HeadBranch)
			if err != nil {
				continue // unrelated histories or a missing branch
			}
			onTrunk, err := stackAncestor(ctx, pr.HeadRepo, shared, trunkSHA)
			if err != nil {
				return nil, err
			}
			if onTrunk {
				continue
			}
		}
		anchor := anchors[issues_model.StackBranchKey(pr.BaseRepoID, pr.BaseBranch)]
		if anchor == nil && (pr.BaseRepoID != stack.RepoID || pr.BaseBranch != stack.TrunkBranch) {
			continue
		}
		candidates = append(candidates, &StackInsertCandidate{Pull: pr, After: anchor})
	}
	return candidates, nil
}

func Unstack(ctx context.Context, doer *user_model.User, stackID, expectedRevision int64) error {
	err := db.WithTx(ctx, func(ctx context.Context) error {
		stack, err := issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		if err = checkStackManagement(ctx, doer, stack); err != nil {
			return err
		}
		if err := checkStackMemberRead(ctx, doer, stack.ID); err != nil {
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
