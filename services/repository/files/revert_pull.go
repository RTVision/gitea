// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package files

import (
	"errors"
	"fmt"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/reqctx"
	"gitea.dev/modules/structs"
	"gitea.dev/services/pull"
)

var ErrPullRevertUnavailable = errors.New("pull request cannot be reverted safely")

// RevertPullRequest creates a branch with the inverse of the recorded merge delta.
func RevertPullRequest(ctx reqctx.RequestContext, repo *repo_model.Repository, doer *user_model.User, pr *issues_model.PullRequest, opts *ApplyDiffPatchOptions) (*structs.FileResponse, error) {
	if !pr.HasMerged || pr.MergedCommitID == "" || pr.BaseRepoID != repo.ID || opts.OldBranch != pr.BaseBranch || opts.NewBranch == "" || opts.NewBranch == opts.OldBranch {
		return nil, ErrPullRevertUnavailable
	}
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return nil, err
	}
	merged, err := gitRepo.GetCommit(ctx, pr.MergedCommitID)
	if err != nil {
		return nil, fmt.Errorf("%w: merge commit is unavailable", ErrPullRevertUnavailable)
	}
	originalBase := pr.MergedBaseCommitID
	if originalBase == "" {
		// One-parent historical commits cannot distinguish squash from a multi-commit rebase.
		if merged.ParentCount() != 2 || pr.Status == issues_model.PullRequestStatusManuallyMerged {
			return nil, fmt.Errorf("%w: original base revision was not recorded", ErrPullRevertUnavailable)
		}
		parent, err := merged.ParentID(0)
		if err != nil {
			return nil, err
		}
		originalBase = parent.String()
	}
	ancestor, err := git.MergeBase(ctx, gitRepo, originalBase, pr.MergedCommitID)
	if err != nil || ancestor != originalBase {
		return nil, fmt.Errorf("%w: invalid recorded merge range", ErrPullRevertUnavailable)
	}
	t, err := gitPatchPrepare(ctx, repo, gitRepo, doer, opts)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	ancestor, err = git.MergeBase(ctx, t.gitRepo, pr.MergedCommitID, opts.LastCommitID)
	if err != nil || ancestor != pr.MergedCommitID {
		return nil, fmt.Errorf("%w: merged revision is no longer in the target branch", ErrPullRevertUnavailable)
	}
	if err := t.RefreshIndex(ctx); err != nil {
		return nil, err
	}
	conflict, _, err := pull.AttemptThreeWayMerge(ctx, t.basePath, t.gitRepo, pr.MergedCommitID, opts.LastCommitID, originalBase, fmt.Sprintf("Revert pull request %d", pr.Index))
	if err != nil {
		return nil, err
	}
	if conflict {
		return nil, fmt.Errorf("%w: revert conflicts with the target branch", ErrPullRevertUnavailable)
	}
	tree, err := t.WriteTree(ctx)
	if err != nil {
		return nil, err
	}
	head, err := t.GetCommit(ctx, opts.LastCommitID)
	if err != nil {
		return nil, err
	}
	if tree == head.TreeID.String() {
		return nil, fmt.Errorf("%w: changes are already reverted", ErrPullRevertUnavailable)
	}
	return gitPatchCommitPush(ctx, t, repo, gitRepo, doer, opts)
}
