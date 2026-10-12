// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	activities_model "gitea.dev/models/activities"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	perm_model "gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	issue_service "gitea.dev/services/issue"
	notify_service "gitea.dev/services/notify"
)

type movedStackPull struct {
	pull     *issues_model.PullRequest
	oldRepo  *repo_model.Repository
	oldIndex int64
}

// moveStackLayers moves layers owned by a fork into the stack repository, bottom up, and gives fork layers that have
// a layer above them a mirror branch to target. It runs in the transaction that records the stack entries.
func moveStackLayers(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, stack *issues_model.PullRequestStack) ([]*movedStackPull, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	var moved []*movedStackPull
	var parent *issues_model.PullRequest
	var parentEntry *issues_model.StackEntry
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if pr.HasMerged {
			continue
		}
		base := stack.TrunkBranch
		if parent != nil {
			if err := ensureStackMirror(ctx, repo, parentEntry, parent, doer.ID); err != nil {
				return nil, err
			}
			base = stackChildBase(repo.ID, parent)
		}
		if pr.BaseRepoID != repo.ID {
			move, err := moveStackLayer(ctx, pr, repo, base)
			if err != nil {
				return nil, err
			}
			moved = append(moved, move)
		}
		parent, parentEntry = pr, entry
	}
	return moved, nil
}

func moveStackLayer(ctx context.Context, pr *issues_model.PullRequest, repo *repo_model.Repository, base string) (*movedStackPull, error) {
	if err := pr.LoadIssue(ctx); err != nil {
		return nil, err
	}
	if pr.Issue.IsClosed {
		return nil, fmt.Errorf("%w: reopen or remove closed pull request #%d before it can move to %s", issues_model.ErrInvalidStack, pr.Index, repo.FullName())
	}
	if err := pr.Issue.LoadPoster(ctx); err != nil {
		return nil, err
	}
	allowed, err := access_model.HasAccessUnit(ctx, pr.Issue.Poster, repo, unit.TypePullRequests, perm_model.AccessModeRead)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: the author of pull request #%d cannot open pull requests in %s", issues_model.ErrInvalidStack, pr.Index, repo.FullName())
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return nil, err
	}
	move := &movedStackPull{pull: pr, oldRepo: pr.BaseRepo, oldIndex: pr.Index}
	if err := issues_model.MovePullRequest(ctx, pr, repo, base); err != nil {
		return nil, err
	}
	// the old repository's feed entries name the issue by its old index
	return move, activities_model.DeleteIssueActions(ctx, move.oldRepo.ID, pr.IssueID, move.oldIndex)
}

// finishStackMoves publishes moved layers in their new repository; opening them there runs its workflows and webhooks.
func finishStackMoves(ctx context.Context, moved []*movedStackPull) {
	for _, move := range moved {
		pr := move.pull
		if err := PushToBaseRepo(ctx, pr); err != nil {
			log.Error("PushToBaseRepo for moved pull request %d: %v", pr.ID, err)
			continue
		}
		if err := git.RemoveRef(ctx, move.oldRepo, git.RefNameFromPullIndex(move.oldIndex).String()); err != nil {
			log.Warn("remove old head ref of moved pull request %d: %v", pr.ID, err)
		}
		if err := syncCommitDivergence(ctx, pr); err != nil {
			log.Error("syncCommitDivergence for moved pull request %d: %v", pr.ID, err)
		}
		StartPullRequestCheckImmediately(ctx, pr)
		notify_service.NewPullRequest(ctx, pr, nil)
		if pr.IsWorkInProgress(ctx) {
			continue
		}
		reviewNotifiers, err := issue_service.PullRequestCodeOwnersReview(ctx, pr)
		if err != nil {
			log.Error("PullRequestCodeOwnersReview for moved pull request %d: %v", pr.ID, err)
		} else if len(reviewNotifiers) > 0 {
			issue_service.ReviewRequestNotify(ctx, pr.Issue, pr.Issue.Poster, reviewNotifiers)
		}
	}
}

// MoveForkStackLayers moves fork-owned layers of stacks adopted before layers moved into the stack repository.
func MoveForkStackLayers(ctx context.Context) error {
	var stackIDs []int64
	if err := db.GetEngine(ctx).Table("pull_request_stack").Distinct("pull_request_stack.id").
		Join("INNER", "stack_entry", "stack_entry.stack_id = pull_request_stack.id").
		Join("INNER", "pull_request", "pull_request.id = stack_entry.pull_request_id").
		Where("pull_request_stack.state = ? AND pull_request.has_merged = ? AND pull_request.base_repo_id <> pull_request_stack.repo_id", issues_model.StackStateOpen, false).
		Find(&stackIDs); err != nil {
		return err
	}
	for _, id := range stackIDs {
		if err := moveForkStack(ctx, id); err != nil {
			log.Error("move fork layers of stack %d: %v", id, err)
		}
	}
	return nil
}

func moveForkStack(ctx context.Context, stackID int64) error {
	var doer *user_model.User
	var moved []*movedStackPull
	err := db.WithTx(ctx, func(ctx context.Context) error {
		stack, err := issues_model.GetStackByID(ctx, stackID)
		if err != nil {
			return err
		}
		entries, err := issues_model.GetStackEntries(ctx, stack.ID)
		if err != nil {
			return err
		}
		pullIDs := make([]int64, 0, len(entries))
		for _, entry := range entries {
			pullIDs = append(pullIDs, entry.PullRequestID)
		}
		if err := issues_model.LockStackMembership(ctx, pullIDs...); err != nil {
			return err
		}
		if err := issues_model.AdvanceStackRevision(ctx, stack.ID, stack.Revision); err != nil {
			return err // retried on the next run once the active operation ends
		}
		repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
		if err != nil {
			return err
		}
		if _, doer, err = user_model.GetPossibleUserByID(ctx, stack.CreatedByID); err != nil {
			return err
		}
		moved, err = moveStackLayers(ctx, doer, repo, stack)
		return err
	})
	if err != nil {
		return err
	}
	finishStackMoves(ctx, moved)
	notifyStackChanged(ctx, doer, stackID, nil)
	return nil
}
