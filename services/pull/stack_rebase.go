// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
	"strings"

	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/git/gitrepo"
	repo_module "gitea.dev/modules/repository"
)

type stackLayerJournal struct {
	EntryID           int64  `json:"entry_id"`
	PullID            int64  `json:"pull_id"`
	Position          int    `json:"position"`
	HeadBranch        string `json:"head_branch"`
	ExpectedHead      string `json:"expected_head"`
	OldParent         string `json:"old_parent"`
	NewParent         string `json:"new_parent,omitempty"`
	NewHead           string `json:"new_head,omitempty"`
	LandedSHA         string `json:"landed_sha,omitempty"`
	Phase             string `json:"phase"`
	LandingBaseSHA    string `json:"landing_base_sha,omitempty"`
	MergeCandidateSHA string `json:"merge_candidate_sha,omitempty"`
}

type StackRebaseConflict struct {
	Position int
	Files    []string
}

func (e StackRebaseConflict) Error() string {
	return fmt.Sprintf("stack layer %d has rebase conflicts: %s; cancel this operation, resolve locally, then synchronize the stack", e.Position, strings.Join(e.Files, ", "))
}

func replayStackLayer(ctx context.Context, repo git.RepositoryFacade, env []string, key *git.SigningKey, oldHead, oldParent, newParent string, position int) (string, error) {
	if oldHead == "" || oldParent == "" || newParent == "" {
		return "", fmt.Errorf("layer %d has no saved ancestry boundary", position)
	}
	if err := gitcmd.NewCommand("merge-base", "--is-ancestor").AddDynamicArguments(oldParent, oldHead).WithRepo(repo).Run(ctx); err != nil {
		return "", fmt.Errorf("layer %d saved parent is not an ancestor; repair the boundary explicitly: %w", position, err)
	}
	merges, _, err := gitcmd.NewCommand("rev-list", "--merges").AddDynamicArguments(oldParent + ".." + oldHead).WithRepo(repo).RunStdString(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(merges) != "" {
		return "", fmt.Errorf("layer %d contains merge commits; rebase locally to a linear layer first", position)
	}
	if oldParent == newParent {
		return oldHead, nil
	}
	if err := gitcmd.NewCommand("checkout", "--force", "--detach").AddDynamicArguments(oldHead).WithRepo(repo).WithEnv(env).Run(ctx); err != nil {
		return "", err
	}
	cmd := gitcmd.NewCommand("rebase", "--no-fork-point", "--reapply-cherry-picks", "--empty=keep", "--onto").AddDynamicArguments(newParent, oldParent, oldHead)
	addCommitSigningOptions(cmd, key)
	if err := cmd.WithRepo(repo).WithEnv(env).RunWithStderr(ctx); err != nil {
		files, _, _ := gitcmd.NewCommand("diff", "--name-only", "--diff-filter=U", "-z").WithRepo(repo).RunStdString(ctx)
		if files != "" {
			return "", StackRebaseConflict{Position: position, Files: strings.Split(strings.TrimSuffix(files, "\x00"), "\x00")}
		}
		return "", fmt.Errorf("rebase layer %d: %w: %s", position, err, err.Stderr())
	}
	return git.GetFullCommitID(ctx, repo, "HEAD")
}

func stackCandidateRef(opID, entryID int64) string {
	return fmt.Sprintf("refs/stack-operations/%d/%d", opID, entryID)
}

func buildStackRebase(ctx context.Context, op *issues_model.StackOperation, layers []*stackLayerJournal, parent string, doer *user_model.User) error {
	if len(layers) == 0 {
		return nil
	}
	pr, err := issues_model.GetPullRequestByID(ctx, layers[0].PullID)
	if err != nil {
		return err
	}
	tmp, cancel, err := createTemporaryRepoForMerge(ctx, pr, doer, layers[0].ExpectedHead)
	if err != nil {
		return err
	}
	defer cancel()
	// Every candidate must build successfully before any source branch is published.
	for _, layer := range layers {
		pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
		if err != nil {
			return err
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return err
		}
		if layer.OldParent == parent {
			layer.NewParent, layer.NewHead = parent, layer.ExpectedHead
			parent = layer.NewHead
			continue
		}
		push, force, err := isUserAllowedToPushOrForcePushInRepoBranch(ctx, doer, pr.HeadRepo, pr.HeadBranch)
		if err != nil {
			return err
		}
		if !push || !force {
			return ErrNoPermissionToMerge
		}
		pb, err := git_model.GetFirstMatchProtectedBranchRule(ctx, pr.HeadRepoID, pr.HeadBranch)
		if err != nil {
			return err
		}
		if pb != nil && pb.RequireSignedCommits && tmp.signKey == nil {
			return fmt.Errorf("layer %d requires signed commits; perform a signed local rebase", layer.Position)
		}
		layer.NewParent = parent
		layer.NewHead, err = replayStackLayer(ctx, tmp.tmpRepo, tmp.env, tmp.signKey, layer.ExpectedHead, layer.OldParent, parent, layer.Position)
		if err != nil {
			return err
		}
		parent = layer.NewHead
	}
	for _, layer := range layers {
		if err := gitcmd.NewCommand("fetch", "--no-tags", "--no-write-fetch-head").AddDashesAndList(tmp.tmpBasePath, "+"+layer.NewHead+":"+stackCandidateRef(op.ID, layer.EntryID)).WithRepo(pr.BaseRepo).Run(ctx); err != nil {
			return err
		}
		layer.Phase = "rebuilt"
	}
	return nil
}

func publishStackLayer(ctx context.Context, op *issues_model.StackOperation, layer *stackLayerJournal, doer *user_model.User) error {
	pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
	if err != nil {
		return err
	}
	if err := validateStackOperation(ctx, pr, op.ID, doer.ID); err != nil {
		return err
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	actual, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
	if err != nil {
		return err
	}
	if actual == layer.NewHead {
		return nil
	}
	if actual != layer.ExpectedHead {
		return ErrSHADoesNotMatch{GivenSHA: layer.ExpectedHead, CurrentSHA: actual}
	}
	push, force, err := isUserAllowedToPushOrForcePushInRepoBranch(ctx, doer, pr.HeadRepo, pr.HeadBranch)
	if err != nil {
		return err
	}
	if !push || !force {
		return ErrNoPermissionToMerge
	}
	if err := pr.HeadRepo.LoadOwner(ctx); err != nil {
		return err
	}
	cmd := gitcmd.NewCommand("push").AddOptionFormat("--force-with-lease=%s", git.BranchPrefix+pr.HeadBranch+":"+layer.ExpectedHead).
		AddDashesAndList(gitrepo.RepoLocalPath(pr.HeadRepo.CodeStorageRepo()), layer.NewHead+":"+git.BranchPrefix+pr.HeadBranch)
	return cmd.WithRepo(pr.HeadRepo).WithEnv(repo_module.FullPushingEnvironment(pr.HeadRepo.Owner, doer, pr.HeadRepo, pr.HeadRepo.Name, 0, 0)).Run(ctx)
}
