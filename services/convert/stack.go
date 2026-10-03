// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package convert

import (
	"context"
	"slices"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	context_service "gitea.dev/services/context"
)

// ToAPIPullRequestStackRef returns the PR's current stack membership.
func ToAPIPullRequestStackRef(ctx context.Context, pr *issues_model.PullRequest, doers ...*user_model.User) (*api.PullRequestStackRef, error) {
	var doer *user_model.User
	if len(doers) != 0 {
		doer = doers[0]
	}
	return toAPIPullRequestStackRef(ctx, pr, doer, true)
}

func toAPIPullRequestStackRef(ctx context.Context, pr *issues_model.PullRequest, doer *user_model.User, requireReadPermission bool) (*api.PullRequestStackRef, error) {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil {
		return nil, err
	}
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	position := 0
	for _, entry := range entries {
		if entry.PullRequestID == pr.ID {
			position = entry.Position
			break
		}
	}
	if position == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	base, err := stackTrunkBase(ctx, stack, entries, doer, requireReadPermission)
	if err != nil {
		return nil, err
	}
	return &api.PullRequestStackRef{Number: stack.ID, Size: len(entries), Position: position, Mode: api.StackMode(stack.Mode), Base: base, Repository: base.Repository}, nil
}

func stackTrunkBase(ctx context.Context, stack *issues_model.PullRequestStack, entries []*issues_model.StackEntry, doer *user_model.User, requireReadPermission bool) (*api.PullRequestStackBase, error) {
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return nil, err
	}
	if apiCtx, ok := ctx.(*context_service.APIContext); ok && requireReadPermission && apiCtx.PublicOnly {
		if !apiCtx.TokenCanAccessRepo(repo) {
			return nil, util.ErrNotExist
		}
		checked := map[int64]bool{repo.ID: true}
		for _, entry := range entries {
			pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
			if err != nil {
				return nil, err
			}
			for _, repoID := range []int64{pr.BaseRepoID, pr.HeadRepoID} {
				if checked[repoID] {
					continue
				}
				memberRepo, err := repo_model.GetRepositoryByID(ctx, repoID)
				if err != nil {
					return nil, err
				}
				if !apiCtx.TokenCanAccessRepo(memberRepo) {
					return nil, util.ErrNotExist
				}
				checked[repoID] = true
			}
		}
	}
	permission, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
	if err != nil {
		return nil, err
	}
	if requireReadPermission && (!permission.CanRead(unit.TypeCode) || !permission.CanRead(unit.TypePullRequests)) {
		return nil, util.ErrNotExist
	}
	gitRepo, err := git.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()
	trunkSHA, err := gitRepo.GetBranchCommitID(ctx, stack.TrunkBranch)
	if err != nil {
		if stack.State == issues_model.StackStateOpen {
			return nil, err
		}
		for _, entry := range slices.Backward(entries) {
			if entry.LandedCommitSHA != "" {
				trunkSHA = entry.LandedCommitSHA
				break
			}
		}
	}
	return &api.PullRequestStackBase{Ref: stack.TrunkBranch, Sha: trunkSHA, Repository: ToRepo(ctx, repo, permission)}, nil
}

// ToAPIPullRequestStack converts a stack and its per-layer pull requests.
func ToAPIPullRequestStack(ctx context.Context, stack *issues_model.PullRequestStack, doer *user_model.User) (*api.PullRequestStack, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	converted := &api.PullRequestStack{
		Number:          stack.ID,
		Trunk:           stack.TrunkBranch,
		Mode:            api.StackMode(stack.Mode),
		State:           stack.State,
		Revision:        stack.Revision,
		ActiveOperation: stack.ActiveOperationID,
		Entries:         make([]*api.PullRequestStackEntry, 0, len(entries)),
	}
	base, err := stackTrunkBase(ctx, stack, entries, doer, true)
	if err != nil {
		return nil, err
	}
	converted.Repository = base.Repository
	positions := make(map[int64]int, len(entries))
	for _, entry := range entries {
		positions[entry.PullRequestID] = entry.Position
	}
	stackRef := func(ctx context.Context, pr *issues_model.PullRequest) (*api.PullRequestStackRef, error) {
		membership, err := issues_model.GetPullRequestStack(ctx, pr.ID)
		if err != nil || membership == nil {
			return nil, err
		}
		if membership.ID != stack.ID {
			return ToAPIPullRequestStackRef(ctx, pr, doer)
		}
		return &api.PullRequestStackRef{Number: stack.ID, Size: len(entries), Position: positions[pr.ID], Mode: api.StackMode(stack.Mode), Base: base, Repository: base.Repository}, nil
	}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		if err := pr.LoadBaseRepo(ctx); err != nil {
			return nil, err
		}
		permission, err := access_model.GetDoerRepoPermission(ctx, pr.BaseRepo, doer)
		if err != nil {
			return nil, err
		}
		if !permission.CanRead(unit.TypeCode) || !permission.CanRead(unit.TypePullRequests) {
			return nil, util.ErrNotExist
		}
		var parentIndex int64
		var parentRef *api.PullRequestReference
		if entry.ParentPullRequestID != 0 {
			parent, err := issues_model.GetPullRequestByID(ctx, entry.ParentPullRequestID)
			if err != nil {
				return nil, err
			}
			parentIndex = parent.Index
			parentRef = &api.PullRequestReference{RepositoryID: parent.BaseRepoID, PullRequest: parent.Index}
		}
		converted.Entries = append(converted.Entries, &api.PullRequestStackEntry{
			Position:             entry.Position,
			PullRequest:          toAPIPullRequest(ctx, pr, doer, stackRef),
			ParentPullRequest:    parentIndex,
			ParentPullRequestRef: parentRef,
			HeadSHA:              entry.HeadSHA,
			ParentSHA:            entry.OldParentSHA,
			LandedSHA:            entry.LandedCommitSHA,
		})
	}
	return converted, nil
}

// ToAPIPullRequestStackOperation converts a durable operation and its journal.
func ToAPIPullRequestStackOperation(op *issues_model.StackOperation) *api.PullRequestStackOperation {
	var journal []byte
	if op.JournalJSON != "" {
		journal = []byte(op.JournalJSON)
	}
	return &api.PullRequestStackOperation{
		Number:           op.ID,
		StackNumber:      op.StackID,
		ExpectedRevision: op.ExpectedRevision,
		Kind:             op.Kind,
		State:            op.State,
		ThroughPosition:  op.ThroughPosition,
		Completed:        op.Completed,
		MergeStyle:       op.MergeStyle,
		Error:            op.LastError,
		Journal:          journal,
		Created:          op.CreatedUnix.AsTimePtr(),
		Updated:          op.UpdatedUnix.AsTimePtr(),
	}
}
