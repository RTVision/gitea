// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/globallock"
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

func stackMirrorLockKey(repoID int64, branch string) string {
	return fmt.Sprintf("stack_mirror_%d_%s", repoID, branch)
}

// stackMirrorWrite records a mirror ref change so a rolled back transaction can undo it.
type stackMirrorWrite struct {
	repo        *repo_model.Repository
	branch      string
	old, commit string // old is empty when the write created the branch
}

// updateStackMirrorRef moves a mirror from its expected tip only, so concurrent writers never overwrite each other.
func updateStackMirrorRef(ctx context.Context, repo *repo_model.Repository, branch, old, commit string) error {
	cmd := gitcmd.NewCommand("update-ref")
	if commit == "" {
		cmd.AddArguments("-d").AddDynamicArguments(git.BranchPrefix + branch)
	} else {
		cmd.AddDynamicArguments(git.BranchPrefix+branch, commit)
	}
	if old == "" {
		old = git.ObjectFormatFromName(repo.ObjectFormatName).EmptyObjectID().String()
	}
	return cmd.AddDynamicArguments(old).WithRepo(repo).Run(ctx)
}

// writeStackMirror points a mirror branch at commit without push hooks, so it triggers no workflows or webhooks.
func writeStackMirror(ctx context.Context, repo, headRepo *repo_model.Repository, branch, old, commit string, doerID int64) error {
	if err := git.FetchRemoteTempCommit(ctx, repo, headRepo, commit); err != nil {
		return err
	}
	if err := updateStackMirrorRef(ctx, repo, branch, old, commit); err != nil {
		return err
	}
	gitRepo, closer, err := git.RepositoryFromContextOrOpen(ctx, repo)
	if err != nil {
		return err
	}
	defer closer.Close()
	c, err := gitRepo.GetCommit(ctx, commit)
	if err != nil {
		return err
	}
	updated, err := git_model.UpdateBranch(ctx, repo.ID, doerID, branch, c)
	if err != nil || updated > 0 {
		return err
	}
	return git_model.AddBranches(ctx, []*git_model.Branch{{
		RepoID:        repo.ID,
		Name:          branch,
		CommitID:      commit,
		CommitMessage: c.MessageTitle(),
		PusherID:      doerID,
		CommitTime:    timeutil.TimeStamp(c.Committer.When.Unix()),
	}})
}

// ensureStackMirror gives a fork layer a mirror branch in the stack repository for the layer above it.
func ensureStackMirror(ctx context.Context, repo *repo_model.Repository, entry *issues_model.StackEntry, pr *issues_model.PullRequest, doerID int64, moves *stackMoves) error {
	if pr.HeadRepoID == repo.ID || entry.MirrorBranch != "" {
		return nil
	}
	branch := stackMirrorBranch(pr.Index)
	old, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+branch)
	if err != nil && !git.IsErrNotExist(err) {
		return err
	}
	if old != "" {
		// only an unstacked stack's own mirror of this layer may be taken back; any other branch belongs to someone else
		owned, err := db.GetEngine(ctx).Table("stack_entry").
			Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
			Where("pull_request_stack.repo_id = ? AND pull_request_stack.state = ? AND stack_entry.pull_request_id = ? AND stack_entry.mirror_branch = ?", repo.ID, issues_model.StackStateUnstacked, pr.ID, branch).
			Exist(new(issues_model.StackEntry))
		if err != nil {
			return err
		}
		if !owned {
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
	if err := writeStackMirror(ctx, repo, pr.HeadRepo, branch, old, head, doerID); err != nil {
		return err
	}
	moves.mirrors = append(moves.mirrors, &stackMirrorWrite{repo: repo, branch: branch, old: old, commit: head})
	entry.MirrorBranch = branch
	_, err = db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch").Update(entry)
	return err
}

// revertStackMirrors undoes mirror refs written by a transaction that rolled back.
func revertStackMirrors(ctx context.Context, writes []*stackMirrorWrite) {
	for _, w := range slices.Backward(writes) {
		if err := updateStackMirrorRef(ctx, w.repo, w.branch, w.commit, w.old); err != nil {
			log.Error("revert stack mirror %s in %s: %v", w.branch, w.repo.FullName(), err)
		}
	}
}

// syncStackMirror points a fork layer's mirror at the layer's current head and retests the layer above it.
func syncStackMirror(ctx context.Context, pr *issues_model.PullRequest) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil || stack.State != issues_model.StackStateOpen {
		return err
	}
	entry := new(issues_model.StackEntry)
	has, err := db.GetEngine(ctx).Where("stack_id = ? AND pull_request_id = ?", stack.ID, pr.ID).Get(entry)
	if err != nil || !has || entry.MirrorBranch == "" {
		return err
	}
	releaser, err := globallock.Lock(ctx, stackMirrorLockKey(stack.RepoID, entry.MirrorBranch))
	if err != nil {
		return err
	}
	defer releaser()
	// pruning may have released the mirror while this waited for the lock
	if has, err = db.GetEngine(ctx).ID(entry.ID).Get(entry); err != nil || !has || entry.MirrorBranch == "" {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	head, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
	if err != nil {
		return err
	}
	current, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+entry.MirrorBranch)
	if err != nil && !git.IsErrNotExist(err) {
		return err
	}
	if current == head {
		return nil
	}
	if err := writeStackMirror(ctx, repo, pr.HeadRepo, entry.MirrorBranch, current, head, pr.HeadRepo.OwnerID); err != nil {
		return err
	}
	AddTestPullRequestTask(TestPullRequestOptions{RepoID: repo.ID, Branch: entry.MirrorBranch})
	return nil
}

// syncStackMirrors catches up mirrors with source pushes that raced the transaction creating them.
func syncStackMirrors(ctx context.Context, stackID int64) {
	entries, err := issues_model.GetStackEntries(ctx, stackID)
	if err != nil {
		log.Error("sync mirrors of stack %d: %v", stackID, err)
		return
	}
	for _, entry := range entries {
		if entry.MirrorBranch == "" {
			continue
		}
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err == nil {
			err = syncStackMirror(ctx, pr)
		}
		if err != nil {
			log.Error("sync stack mirror %s: %v", entry.MirrorBranch, err)
		}
	}
}

// pruneStackMirrors deletes mirror branches that no open pull request targets any more.
func pruneStackMirrors(ctx context.Context, stack *issues_model.PullRequestStack, doerID int64) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		log.Error("prune mirrors of stack %d: %v", stack.ID, err)
		return
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		log.Error("prune mirrors of stack %d: %v", stack.ID, err)
		return
	}
	for _, entry := range entries {
		if entry.MirrorBranch == "" {
			continue
		}
		if err := globallock.LockAndDo(ctx, stackMirrorLockKey(repo.ID, entry.MirrorBranch), func(ctx context.Context) error {
			targets, err := issues_model.GetUnmergedPullRequestsByBaseInfo(ctx, repo.ID, entry.MirrorBranch)
			if err != nil || len(targets) > 0 {
				return err
			}
			current, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+entry.MirrorBranch)
			if err != nil && !git.IsErrNotExist(err) {
				return err
			}
			if current != "" {
				if err := updateStackMirrorRef(ctx, repo, entry.MirrorBranch, current, ""); err != nil {
					return err
				}
			}
			if err := git_model.MarkBranchAsDeleted(ctx, repo.ID, entry.MirrorBranch, doerID); err != nil && !git_model.IsErrBranchNotExist(err) {
				return err
			}
			entry.MirrorBranch = ""
			_, err = db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch").Update(entry)
			return err
		}); err != nil {
			log.Error("prune stack mirror %s: %v", entry.MirrorBranch, err)
		}
	}
}
