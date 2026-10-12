// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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
// A non-nil moves records the ref change as soon as it lands, so a rollback undoes it even if a later step fails.
func writeStackMirror(ctx context.Context, repo, headRepo *repo_model.Repository, branch, old, commit string, moves *stackMoves) error {
	if err := git.FetchRemoteTempCommit(ctx, repo, headRepo, commit); err != nil {
		return err
	}
	if err := updateStackMirrorRef(ctx, repo, branch, old, commit); err != nil {
		return err
	}
	if moves != nil {
		moves.mirrors = append(moves.mirrors, &stackMirrorWrite{repo: repo, branch: branch, old: old, commit: commit})
	}
	return nil
}

// recordStackMirrorBranch keeps the branch table in step with a mirror ref, as a push would.
func recordStackMirrorBranch(ctx context.Context, repo *repo_model.Repository, branch, commit string, doerID int64) error {
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
		// only an unstacked stack's own, untouched mirror of this layer may be taken back; any other branch belongs to someone else
		owned, err := db.GetEngine(ctx).Table("stack_entry").
			Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
			Where("pull_request_stack.repo_id = ? AND pull_request_stack.state = ? AND stack_entry.pull_request_id = ? AND stack_entry.mirror_branch = ?", repo.ID, issues_model.StackStateUnstacked, pr.ID, branch).
			And("stack_entry.mirror_sha = ? OR stack_entry.mirror_next_sha = ?", old, old).
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
	if err := writeStackMirror(ctx, repo, pr.HeadRepo, branch, old, head, moves); err != nil {
		return err
	}
	if err := recordStackMirrorBranch(ctx, repo, branch, head, doerID); err != nil {
		return err
	}
	entry.MirrorBranch, entry.MirrorSHA, entry.MirrorNextSHA = branch, head, ""
	_, err = db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch", "mirror_sha", "mirror_next_sha").Update(entry)
	return err
}

// revertStackMirrors undoes mirror refs written by a transaction that rolled back.
func revertStackMirrors(ctx context.Context, writes []*stackMirrorWrite) {
	if len(writes) == 0 {
		return
	}
	// the rollback may come from a cancelled request, which must not stop the cleanup
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
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
	releaser, err := globallock.Lock(ctx, issues_model.StackMirrorLockKey(stack.RepoID, entry.MirrorBranch))
	if err != nil {
		return err
	}
	defer releaser()
	// pruning or unstacking may have released the mirror while this waited for the lock
	if has, err = db.GetEngine(ctx).ID(entry.ID).NoAutoCondition().Get(entry); err != nil || !has || entry.MirrorBranch == "" {
		return err
	}
	if stack, err = issues_model.GetStackByID(ctx, stack.ID); err != nil || stack.State != issues_model.StackStateOpen {
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
	// the stack only moves a tip it wrote, so a branch released and then changed by someone else is never overwritten
	if current != entry.MirrorSHA && (entry.MirrorNextSHA == "" || current != entry.MirrorNextSHA) {
		return fmt.Errorf("mirror %s moved outside the stack from %s to %s", entry.MirrorBranch, entry.MirrorSHA, current)
	}
	if current != head {
		// the accepted tip becomes the recorded one, so a failed write leaves it owned
		entry.MirrorSHA, entry.MirrorNextSHA = current, head
		if _, err := db.GetEngine(ctx).ID(entry.ID).Cols("mirror_sha", "mirror_next_sha").Update(entry); err != nil {
			return err
		}
		if err := writeStackMirror(ctx, repo, pr.HeadRepo, entry.MirrorBranch, current, head, nil); err != nil {
			return err
		}
	} else if entry.MirrorSHA == head && entry.MirrorNextSHA == "" {
		return nil
	}
	if err := recordStackMirrorBranch(ctx, repo, entry.MirrorBranch, head, pr.HeadRepo.OwnerID); err != nil {
		return err
	}
	entry.MirrorSHA, entry.MirrorNextSHA = head, ""
	if _, err := db.GetEngine(ctx).ID(entry.ID).Cols("mirror_sha", "mirror_next_sha").Update(entry); err != nil {
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
		branch := entry.MirrorBranch
		if err := globallock.LockAndDo(ctx, issues_model.StackMirrorLockKey(repo.ID, branch), func(ctx context.Context) error {
			// unstacking or deleting the stack hands the branch over, so it is no longer ours to delete
			current, err := issues_model.GetStackByID(ctx, stack.ID)
			if errors.Is(err, issues_model.ErrStackNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			if current.State != issues_model.StackStateOpen && current.State != issues_model.StackStateComplete {
				return nil
			}
			if has, err := db.GetEngine(ctx).ID(entry.ID).NoAutoCondition().Get(entry); err != nil || !has || entry.MirrorBranch != branch {
				return err
			}
			targets, err := issues_model.GetUnmergedPullRequestsByBaseInfo(ctx, repo.ID, entry.MirrorBranch)
			if err != nil || len(targets) > 0 {
				return err
			}
			tip, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+entry.MirrorBranch)
			if err != nil && !git.IsErrNotExist(err) {
				return err
			}
			// a tip the stack did not write belongs to someone else; stop managing the branch but keep it
			if tip != "" && (tip == entry.MirrorSHA || tip == entry.MirrorNextSHA) {
				if err := updateStackMirrorRef(ctx, repo, entry.MirrorBranch, tip, ""); err != nil {
					return err
				}
				if err := git_model.MarkBranchAsDeleted(ctx, repo.ID, entry.MirrorBranch, doerID); err != nil && !git_model.IsErrBranchNotExist(err) {
					return err
				}
			}
			entry.MirrorBranch, entry.MirrorSHA, entry.MirrorNextSHA = "", "", ""
			_, err = db.GetEngine(ctx).ID(entry.ID).Cols("mirror_branch", "mirror_sha", "mirror_next_sha").Update(entry)
			return err
		}); err != nil {
			log.Error("prune stack mirror %s: %v", branch, err)
		}
	}
}

// ReconcileStackMirrors catches up every mirror of open stacks, recovering syncs interrupted by a restart.
func ReconcileStackMirrors(ctx context.Context) error {
	var stackIDs []int64
	if err := db.GetEngine(ctx).Table("stack_entry").Distinct("stack_entry.stack_id").
		Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
		Where("pull_request_stack.state = ? AND stack_entry.mirror_branch <> ''", issues_model.StackStateOpen).
		Find(&stackIDs); err != nil {
		return err
	}
	for _, id := range stackIDs {
		syncStackMirrors(ctx, id)
	}
	return nil
}
