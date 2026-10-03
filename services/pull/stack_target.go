// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/git/gitrepo"
)

type Target struct {
	Repo   *repo_model.Repository
	Branch string
}

func stackOperationTarget(ctx context.Context, op *issues_model.StackOperation) (*Target, error) {
	stack, err := issues_model.GetStackByID(ctx, op.StackID)
	if err != nil {
		return nil, err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return nil, err
	}
	return &Target{Repo: repo, Branch: stack.TrunkBranch}, nil
}

func fetchStackObject(ctx context.Context, dest git.RepositoryFacade, source *repo_model.Repository, sha, ref string) error {
	return gitcmd.NewCommand("fetch", "--no-tags", "--no-write-fetch-head").AddDashesAndList(gitrepo.RepoLocalPath(source.CodeStorageRepo()), "+"+sha+":"+ref).WithRepo(dest).Run(ctx)
}

func stackLayerSource(ctx context.Context, layer *stackLayerJournal) (*repo_model.Repository, error) {
	pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
	if err != nil {
		return nil, err
	}
	if layer.HeadRepoID != 0 && layer.HeadRepoID != pr.HeadRepoID {
		return nil, issues_model.ErrStackRevision
	}
	if layer.HeadBranch != pr.HeadBranch {
		return nil, issues_model.ErrStackRevision
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return nil, err
	}
	if pr.HeadRepo == nil {
		return nil, &repo_model.ErrRepoNotExist{ID: pr.HeadRepoID}
	}
	return pr.HeadRepo, nil
}

func pinStackEntry(ctx context.Context, stack *issues_model.PullRequestStack, entry *issues_model.StackEntry) error {
	pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
	if err != nil {
		return err
	}
	if pr.HasMerged {
		return nil
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	if err := pr.LoadBaseRepo(ctx); err != nil {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	head, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
	if err != nil {
		return err
	}
	if err := fetchStackObject(ctx, repo, pr.HeadRepo, head, issues_model.StackHeadRefName(pr.ID)); err != nil {
		return err
	}
	if entry.OldParentSHA != "" {
		if err := fetchStackObject(ctx, pr.BaseRepo, repo, entry.OldParentSHA, fmt.Sprintf("refs/stack-pulls/%d/base", pr.ID)); err != nil {
			return err
		}
	}
	if err := fetchStackObject(ctx, pr.BaseRepo, pr.HeadRepo, head, pr.GetGitHeadRefName()); err != nil {
		return err
	}
	_, err = db.GetEngine(ctx).ID(pr.ID).And("has_merged = ?", false).Cols("merge_base").Update(&issues_model.PullRequest{MergeBase: entry.OldParentSHA})
	return err
}

func prepareStackObjects(ctx context.Context, tmp *mergeContext, layers []*stackLayerJournal) error {
	for _, layer := range layers {
		source, err := stackLayerSource(ctx, layer)
		if err != nil {
			return err
		}
		if err := fetchStackObject(ctx, tmp.tmpRepo, source, layer.ExpectedHead, fmt.Sprintf("refs/stack-pulls/%d/head", layer.PullID)); err != nil {
			return err
		}
	}
	return nil
}
