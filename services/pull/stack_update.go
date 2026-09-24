// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"strings"

	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
)

type StackMergeConflict struct {
	Position int
	Files    []string
	Merges   []string // unpublished lower candidates exist only on the server, so the user repeats their merges too
}

func (e StackMergeConflict) Error() string {
	return fmt.Sprintf("stack layer %d has merge conflicts: %s; locally merge %s, push, then retry", e.Position, strings.Join(e.Files, ", "), strings.Join(e.Merges, ", then "))
}

func stackMergeStep(parentBranch, branch string) string {
	return parentBranch + " into " + branch
}

func stackUpdateMessage(parentBranch, branch string) string {
	return fmt.Sprintf("Merge branch '%s' into %s", parentBranch, branch)
}

// mergeStackLayer merges parent into head; with keepTree the result records parent without taking its content.
func mergeStackLayer(ctx context.Context, repo git.RepositoryFacade, env []string, key *git.SigningKey, head, parent, parentBranch, branch string, position int, keepTree bool) (string, error) {
	if err := gitcmd.NewCommand("checkout", "--force", "--detach").AddDynamicArguments(head).WithRepo(repo).WithEnv(env).Run(ctx); err != nil {
		return "", err
	}
	cmd := gitcmd.NewCommand("merge", "--no-ff", "--no-edit").AddOptionValues("-m", stackUpdateMessage(parentBranch, branch))
	if keepTree {
		cmd.AddArguments("--strategy=ours")
	}
	addCommitSigningOptions(cmd, key)
	if err := cmd.AddDynamicArguments(parent).WithRepo(repo).WithEnv(env).RunWithStderr(ctx); err != nil {
		files, _, _ := gitcmd.NewCommand("diff", "--name-only", "--diff-filter=U", "-z").WithRepo(repo).RunStdString(ctx)
		if files != "" {
			return "", StackMergeConflict{Position: position, Files: strings.Split(strings.TrimSuffix(files, "\x00"), "\x00"), Merges: []string{stackMergeStep(parentBranch, branch)}}
		}
		return "", fmt.Errorf("merge into layer %d: %w: %s", position, err, err.Stderr())
	}
	return git.GetFullCommitID(ctx, repo, "HEAD")
}

// landedStackLayerEquivalent reports whether a landed commit carries exactly the content of a layer head
// already inside head, as a squash of a layer that contained the trunk does.
func landedStackLayerEquivalent(ctx context.Context, repo git.RepositoryFacade, landed *issues_model.StackEntry, head string) (bool, error) {
	if landed.LandedCommitSHA == "" || landed.HeadSHA == "" {
		return false, nil
	}
	if merged, err := stackAncestor(ctx, repo, landed.LandedCommitSHA, head); err != nil || merged {
		return false, err
	}
	if contained, err := stackAncestor(ctx, repo, landed.HeadSHA, head); err != nil || !contained {
		return false, err
	}
	trees, _, err := gitcmd.NewCommand("rev-parse").AddDynamicArguments(landed.LandedCommitSHA+"^{tree}", landed.HeadSHA+"^{tree}").WithRepo(repo).RunStdString(ctx)
	if err != nil {
		return false, err
	}
	pair := strings.Fields(trees)
	return len(pair) == 2 && pair[0] == pair[1], nil
}

// buildStackUpdate merges each layer's new parent into it; candidates only fast-forward the layers.
func buildStackUpdate(ctx context.Context, op *issues_model.StackOperation, layers []*stackLayerJournal, landed []*issues_model.StackEntry, trunkBranch, parent string, doer *user_model.User) error {
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
	parentBranch := trunkBranch
	var merges []string
	for i, layer := range layers {
		pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
		if err != nil {
			return err
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return err
		}
		head := layer.ExpectedHead
		contained, err := stackAncestor(ctx, tmp.tmpRepo, parent, head)
		if err != nil {
			return err
		}
		if !contained {
			push, _, err := isUserAllowedToPushOrForcePushInRepoBranch(ctx, doer, pr.HeadRepo, pr.HeadBranch)
			if err != nil {
				return err
			}
			if !push {
				return ErrNoPermissionToMerge
			}
			pb, err := git_model.GetFirstMatchProtectedBranchRule(ctx, pr.HeadRepoID, pr.HeadBranch)
			if err != nil {
				return err
			}
			if pb != nil && pb.RequireSignedCommits && tmp.signKey == nil {
				return fmt.Errorf("layer %d requires signed commits; merge %s into %s locally with signing", layer.Position, parentBranch, pr.HeadBranch)
			}
			merges = append(merges, stackMergeStep(parentBranch, pr.HeadBranch))
		}
		// Only the lowest open layer sits on the trunk, where squashed layers landed.
		for _, entry := range landed {
			if i > 0 || contained {
				break
			}
			equivalent, err := landedStackLayerEquivalent(ctx, tmp.tmpRepo, entry, head)
			if err != nil {
				return err
			}
			if !equivalent {
				continue
			}
			if head, err = mergeStackLayer(ctx, tmp.tmpRepo, tmp.env, tmp.signKey, head, entry.LandedCommitSHA, parentBranch, pr.HeadBranch, layer.Position, true); err != nil {
				return err
			}
			if contained, err = stackAncestor(ctx, tmp.tmpRepo, parent, head); err != nil {
				return err
			}
		}
		if !contained {
			if head, err = mergeStackLayer(ctx, tmp.tmpRepo, tmp.env, tmp.signKey, head, parent, parentBranch, pr.HeadBranch, layer.Position, false); err != nil {
				if conflict, ok := errors.AsType[StackMergeConflict](err); ok {
					conflict.Merges = merges
					return conflict
				}
				return err
			}
		}
		layer.NewParent, layer.NewHead = parent, head
		parent, parentBranch = head, pr.HeadBranch
	}
	return storeStackCandidates(ctx, op, layers, tmp, pr)
}
