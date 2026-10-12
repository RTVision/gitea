// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
)

// stackMirrorBranch names the stack repository branch tracking the fork head of layer index.
func stackMirrorBranch(index int64) string {
	return fmt.Sprintf("stack/pr-%d", index)
}

// stackChildBase returns the stack repository branch that the layer above parent targets.
func stackChildBase(stackRepoID int64, parent *issues_model.PullRequest) string {
	if parent.HeadRepoID == stackRepoID {
		return parent.HeadBranch
	}
	return stackMirrorBranch(parent.Index)
}

// writeStackMirror points a mirror branch at sha without push hooks, so it triggers no workflows or webhooks.
func writeStackMirror(ctx context.Context, repo, headRepo *repo_model.Repository, branch, sha string, doerID int64) error {
	if err := git.FetchRemoteCommitUpdateRef(ctx, repo, headRepo, sha, git.BranchPrefix+branch); err != nil {
		return err
	}
	gitRepo, closer, err := git.RepositoryFromContextOrOpen(ctx, repo)
	if err != nil {
		return err
	}
	defer closer.Close()
	commit, err := gitRepo.GetCommit(ctx, sha)
	if err != nil {
		return err
	}
	updated, err := git_model.UpdateBranch(ctx, repo.ID, doerID, branch, commit)
	if err != nil || updated > 0 {
		return err
	}
	return git_model.AddBranches(ctx, []*git_model.Branch{{
		RepoID:        repo.ID,
		Name:          branch,
		CommitID:      sha,
		CommitMessage: commit.MessageTitle(),
		PusherID:      doerID,
		CommitTime:    timeutil.TimeStamp(commit.Committer.When.Unix()),
	}})
}

// ensureStackMirror gives a fork layer a mirror branch in the stack repository for the layer above it.
func ensureStackMirror(ctx context.Context, repo *repo_model.Repository, entry *issues_model.StackEntry, pr *issues_model.PullRequest, doerID int64) error {
	if pr.HeadRepoID == repo.ID || entry.MirrorBranch != "" {
		return nil
	}
	branch := stackMirrorBranch(pr.Index)
	if exist, err := git_model.IsBranchExist(ctx, repo.ID, branch); err != nil {
		return err
	} else if exist {
		// an unstacked chain keeps its mirrors, so adopting it again reclaims them
		targeted, err := db.GetEngine(ctx).Table("pull_request").Join("INNER", "issue", "issue.id = pull_request.issue_id").
			Where("pull_request.base_repo_id = ? AND pull_request.base_branch = ? AND pull_request.head_repo_id = ? AND issue.is_closed = ?", repo.ID, branch, pr.HeadRepoID, false).
			Exist(new(issues_model.PullRequest))
		if err != nil {
			return err
		}
		if !targeted {
			return fmt.Errorf("%w: branch %s already exists in %s", issues_model.ErrInvalidStack, branch, repo.FullName())
		}
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	head, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
	if err != nil {
		return err
	}
	if err := writeStackMirror(ctx, repo, pr.HeadRepo, branch, head, doerID); err != nil {
		return err
	}
	entry.MirrorBranch = branch
	_, err = db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch").Update(entry)
	return err
}

// syncStackMirror follows a fork layer's new head and retests the layer above it.
func syncStackMirror(ctx context.Context, pr *issues_model.PullRequest, head string) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil || stack.State != issues_model.StackStateOpen {
		return err
	}
	entry := new(issues_model.StackEntry)
	has, err := db.GetEngine(ctx).Where("stack_id = ? AND pull_request_id = ?", stack.ID, pr.ID).Get(entry)
	if err != nil || !has || entry.MirrorBranch == "" {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	if current, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+entry.MirrorBranch); err == nil && current == head {
		return nil
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	if err := writeStackMirror(ctx, repo, pr.HeadRepo, entry.MirrorBranch, head, pr.HeadRepo.OwnerID); err != nil {
		return err
	}
	AddTestPullRequestTask(TestPullRequestOptions{RepoID: repo.ID, Branch: entry.MirrorBranch})
	return nil
}

// pruneStackMirrors deletes mirror branches that no open pull request targets any more.
func pruneStackMirrors(ctx context.Context, stack *issues_model.PullRequestStack, doerID int64) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		log.Error("prune mirrors of stack %d: %v", stack.ID, err)
		return
	}
	var repo *repo_model.Repository
	for _, entry := range entries {
		if entry.MirrorBranch == "" {
			continue
		}
		targets, err := issues_model.GetUnmergedPullRequestsByBaseInfo(ctx, stack.RepoID, entry.MirrorBranch)
		if err != nil || len(targets) > 0 {
			continue
		}
		if repo == nil {
			if repo, err = repo_model.GetRepositoryByID(ctx, stack.RepoID); err != nil {
				log.Error("prune mirrors of stack %d: %v", stack.ID, err)
				return
			}
		}
		if err := git.RemoveRef(ctx, repo, git.BranchPrefix+entry.MirrorBranch); err != nil {
			log.Error("remove stack mirror %s: %v", entry.MirrorBranch, err)
			continue
		}
		if err := git_model.MarkBranchAsDeleted(ctx, repo.ID, entry.MirrorBranch, doerID); err != nil && !git_model.IsErrBranchNotExist(err) {
			log.Error("mark stack mirror %s deleted: %v", entry.MirrorBranch, err)
		}
		entry.MirrorBranch = ""
		if _, err := db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch").Update(entry); err != nil {
			log.Error("clear stack mirror of entry %d: %v", entry.ID, err)
		}
	}
}
