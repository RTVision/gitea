// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"

	issues_model "gitea.dev/models/issues"
	perm_model "gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/util"
)

func checkStackPullRead(ctx context.Context, doer *user_model.User, pullIDs []int64) error {
	checked := map[int64]bool{}
	for _, id := range pullIDs {
		pr, err := issues_model.GetPullRequestByID(ctx, id)
		if err != nil {
			return err
		}
		for _, repoID := range []int64{pr.BaseRepoID, pr.HeadRepoID} {
			if checked[repoID] {
				continue
			}
			repo, err := repo_model.GetRepositoryByID(ctx, repoID)
			if err != nil {
				return err
			}
			permission, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
			if err != nil {
				return err
			}
			if !permission.CanAccess(perm_model.AccessModeRead, unit.TypeCode) {
				return util.NewPermissionDeniedErrorf("stack management requires access to every source repository")
			}
			checked[repoID] = true
		}
		if err := pr.LoadBaseRepo(ctx); err != nil {
			return err
		}
		allowed, err := access_model.HasAccessUnit(ctx, doer, pr.BaseRepo, unit.TypePullRequests, perm_model.AccessModeRead)
		if err != nil {
			return err
		}
		if !allowed {
			return util.NewPermissionDeniedErrorf("stack management requires access to every pull request")
		}
	}
	return nil
}

func checkStackMemberRead(ctx context.Context, doer *user_model.User, stackID int64, extra ...int64) error {
	entries, err := issues_model.GetStackEntries(ctx, stackID)
	if err != nil {
		return err
	}
	ids := append([]int64(nil), extra...)
	for _, entry := range entries {
		ids = append(ids, entry.PullRequestID)
	}
	return checkStackPullRead(ctx, doer, ids)
}

func checkStackAuthority(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, pullIDs ...int64) error {
	if doer == nil || !repo.CanContentChange() {
		return util.NewPermissionDeniedErrorf("stack management requires an editable repository and an authenticated user")
	}
	permission, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
	if err != nil {
		return err
	}
	if permission.CanWrite(unit.TypeCode) {
		return nil
	}
	if !permission.CanRead(unit.TypeCode) || !permission.CanRead(unit.TypePullRequests) || len(pullIDs) == 0 {
		return util.NewPermissionDeniedErrorf("stack management requires repository write access or ownership of every fork layer")
	}
	for _, id := range pullIDs {
		pr, err := issues_model.GetPullRequestByID(ctx, id)
		if err != nil {
			return err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return err
		}
		if pr.Issue.PosterID != doer.ID || pr.HeadRepoID == repo.ID {
			return util.NewPermissionDeniedErrorf("stack management requires ownership of every fork layer")
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return err
		}
		if pr.HeadRepo == nil || !pr.HeadRepo.IsFork || pr.HeadRepo.ForkID != repo.ID || !pr.HeadRepo.CanContentChange() {
			return util.NewPermissionDeniedErrorf("stack management requires writable source branches in a fork of the target repository")
		}
		push, _, err := isUserAllowedToPushOrForcePushInRepoBranch(ctx, doer, pr.HeadRepo, pr.HeadBranch)
		if err != nil {
			return err
		}
		if !push {
			return util.NewPermissionDeniedErrorf("stack management requires write access to every fork source branch")
		}
	}
	return nil
}

func stackManagementPulls(ctx context.Context, stack *issues_model.PullRequestStack, extra ...int64) ([]int64, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	ids := append([]int64(nil), extra...)
	for _, entry := range entries {
		ids = append(ids, entry.PullRequestID)
	}
	return ids, nil
}

func checkStackManagement(ctx context.Context, doer *user_model.User, stack *issues_model.PullRequestStack, extra ...int64) error {
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	ids, err := stackManagementPulls(ctx, stack, extra...)
	if err != nil {
		return err
	}
	return checkStackAuthority(ctx, doer, repo, ids...)
}

func CanManageStack(ctx context.Context, doer *user_model.User, stack *issues_model.PullRequestStack, extra ...int64) (bool, error) {
	if err := checkStackManagement(ctx, doer, stack, extra...); err != nil {
		if errors.Is(err, util.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	if err := checkStackMemberRead(ctx, doer, stack.ID, extra...); err != nil {
		if errors.Is(err, util.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func CanCreateStack(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, pullIDs []int64) (bool, error) {
	if err := checkStackAuthority(ctx, doer, repo, pullIDs...); err != nil {
		if errors.Is(err, util.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	if err := checkStackPullRead(ctx, doer, pullIDs); err != nil {
		if errors.Is(err, util.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
