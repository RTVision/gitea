// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

func stackAncestor(ctx context.Context, repo *repo_model.Repository, parent, head string) (bool, error) {
	if parent == "" {
		return false, nil
	}
	err := gitcmd.NewCommand("merge-base", "--is-ancestor").AddDynamicArguments(parent, head).WithRepo(repo).Run(ctx)
	if err == nil {
		return true, nil
	}
	if gitcmd.IsErrorExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

func reconcileStackCancellation(ctx context.Context, op *issues_model.StackOperation, actor *user_model.User, repo *repo_model.Repository) error {
	journal := new(stackJournal)
	if err := json.Unmarshal([]byte(op.JournalJSON), journal); err != nil {
		return err
	}
	if op.State == "running" {
		return errors.New("operation is publishing; retry cancellation after it stops")
	}
	op.State = "cancelling"
	op.LastError = "cancellation requested; preserving published refs"
	if err := issues_model.SaveStackOperation(ctx, op); err != nil {
		return err
	}
	_, merger, err := user_model.GetPossibleUserByID(ctx, op.ActorID)
	if err != nil {
		return err
	}
	for _, layer := range remainingStackLayers(journal) {
		pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
		if err != nil {
			return err
		}
		merged, err := reconcileStackMerge(ctx, layer, pr, merger)
		if err != nil {
			return err
		}
		if merged {
			layer.Phase, layer.LandedSHA = "landed", pr.MergedCommitID
			op.Completed++
			if err := recordStackLayer(ctx, op, layer, true); err != nil {
				return err
			}
			continue
		}
		head, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+layer.HeadBranch)
		if err != nil {
			return err
		}
		published, err := stackAncestor(ctx, repo, layer.NewHead, head)
		if err != nil {
			return err
		}
		if published {
			layer.OldParent = layer.NewParent
		}
		knownBoundary, err := stackAncestor(ctx, repo, layer.OldParent, head)
		if err != nil {
			return err
		}
		if !knownBoundary {
			layer.OldParent = ""
		}
		layer.ExpectedHead, layer.Phase = head, "ready"
		if err := recordStackLayer(ctx, op, layer, false); err != nil {
			return err
		}
	}
	stack, err := issues_model.GetStackByID(ctx, op.StackID)
	if err != nil {
		return err
	}
	if stack.ActiveOperationID != op.ID || stack.Revision != op.ExpectedRevision {
		return issues_model.ErrStackRevision
	}
	if remaining := remainingStackLayers(journal); len(remaining) > 0 {
		pr, err := issues_model.GetPullRequestByID(ctx, remaining[0].PullID)
		if err != nil {
			return err
		}
		if pr.BaseBranch != stack.TrunkBranch {
			if err := changeTargetBranchForStack(ctx, pr, actor, stack.TrunkBranch, 0); err != nil {
				log.Warn("Cancelled stack %d requires target repair before synchronizing: %v", stack.ID, err)
			}
		}
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	op.JournalJSON = string(data)
	op.State = "cancelled"
	op.LastError = "published refs were preserved; synchronize the stack after any local repair"
	return issues_model.FinishStackOperation(ctx, op)
}
