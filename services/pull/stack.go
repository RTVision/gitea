// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
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
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
)

type CreateStackOptions struct {
	TrunkBranch    string
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

func validateStackChain(ctx context.Context, repo *repo_model.Repository, trunk string, pullIDs []int64) ([]*issues_model.StackEntry, error) {
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
	var parentID int64
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
		if seenIDs[id] || seenBranches[pr.HeadBranch] || pr.HasMerged || pr.Issue.IsClosed || pr.HeadRepoID != repo.ID || pr.BaseRepoID != repo.ID || pr.BaseBranch != parentBranch || pr.Flow != issues_model.PullRequestFlowGithub {
			return nil, fmt.Errorf("%w: pull request %d does not form an open same-repository chain", issues_model.ErrInvalidStack, id)
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
		if boundary != parentSHA || headSHA == parentSHA {
			return nil, fmt.Errorf("%w: pull request %d must contain its current parent head", issues_model.ErrInvalidStack, id)
		}
		merges, _, err := gitcmd.NewCommand("rev-list", "--merges").AddDynamicArguments(parentSHA + ".." + headSHA).WithRepo(gitRepo).RunStdString(ctx)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(merges) != "" {
			return nil, fmt.Errorf("%w: pull request %d must have linear layer history", issues_model.ErrInvalidStack, id)
		}
		// A head shared by another open PR has ambiguous rewrite ownership.
		count, err := db.GetEngine(ctx).Table("pull_request").Join("INNER", "issue", "issue.id = pull_request.issue_id").Where("pull_request.head_repo_id = ? AND pull_request.head_branch = ? AND issue.is_closed = ? AND pull_request.id <> ?", repo.ID, pr.HeadBranch, false, id).Count(new(issues_model.PullRequest))
		if err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, fmt.Errorf("%w: branch %s has multiple open pull requests", issues_model.ErrInvalidStack, pr.HeadBranch)
		}
		entries = append(entries, &issues_model.StackEntry{PullRequestID: id, Position: i + 1, ParentPullRequestID: parentID, OldParentSHA: parentSHA, HeadSHA: headSHA})
		seenIDs[id], seenBranches[pr.HeadBranch] = true, true
		parentID, parentBranch, parentSHA = id, pr.HeadBranch, headSHA
	}
	return entries, nil
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
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: opts.TrunkBranch, State: issues_model.StackStateOpen, Revision: 1, CreatedByID: doer.ID}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		entries, err := validateStackChain(ctx, repo, opts.TrunkBranch, opts.PullRequestIDs)
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
		entries, err := validateStackChain(ctx, repo, stack.TrunkBranch, allIDs)
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
