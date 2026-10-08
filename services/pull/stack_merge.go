// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/json"
	"gitea.dev/modules/timeutil"
)

type stackMergeContextKey struct{}

type stackMergePublication struct {
	op      *issues_model.StackOperation
	journal *stackJournal
	layer   *stackLayerJournal
}

func (p *stackMergePublication) prepare(ctx context.Context, tmp *mergeContext, base, candidate string) error {
	if base != p.layer.LandingBaseSHA {
		return ErrSHADoesNotMatch{GivenSHA: p.layer.LandingBaseSHA, CurrentSHA: base}
	}
	ref := fmt.Sprintf("refs/stack-operations/%d/merge-%d", p.op.ID, p.layer.EntryID)
	if err := gitcmd.NewCommand("fetch", "--no-tags", "--no-write-fetch-head").AddDashesAndList(tmp.tmpBasePath, "+"+candidate+":"+ref).WithRepo(tmp.target.Repo).Run(ctx); err != nil {
		return err
	}
	p.layer.MergeCandidateSHA = candidate
	return saveStackJournal(ctx, p.op, p.journal)
}

// CheckStackMergePublication fences the final Git publication against the durable intent.
func CheckStackMergePublication(ctx context.Context, pullID, actorID int64, branch, oldSHA, newSHA string, repoIDs ...int64) error {
	pr, err := issues_model.GetPullRequestByID(ctx, pullID)
	if err != nil {
		return err
	}
	stack, err := issues_model.GetPullRequestStack(ctx, pullID)
	if err != nil || stack == nil {
		return err
	}
	if stack.TrunkBranch != branch || (len(repoIDs) > 0 && stack.RepoID != repoIDs[0]) {
		return ErrPullRequestStacked
	}
	if err := validateStackOperation(ctx, pr, stack.ActiveOperationID, actorID); err != nil {
		return err
	}
	if err := checkStackMergeOrder(ctx, pr); err != nil {
		return err
	}
	op, err := issues_model.GetStackOperation(ctx, stack.ActiveOperationID)
	if err != nil {
		return err
	}
	journal := new(stackJournal)
	if err := json.Unmarshal([]byte(op.JournalJSON), journal); err != nil {
		return err
	}
	layers := remainingStackLayers(journal)
	if journal.Stage != "confirm" || len(layers) == 0 {
		return issues_model.ErrStackRevision
	}
	layer := layers[0]
	if (layer.HeadRepoID != 0 && layer.HeadRepoID != pr.HeadRepoID) || (layer.LandingRepoID != 0 && (layer.LandingRepoID != stack.RepoID || layer.LandingBranch != branch)) {
		return issues_model.ErrStackRevision
	}
	if layer.PullID != pullID || layer.Phase != "merging" || layer.LandingBaseSHA != oldSHA || layer.MergeCandidateSHA != newSHA {
		return issues_model.ErrStackRevision
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return err
	}
	head, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
	if err != nil {
		return err
	}
	if head != layer.ExpectedHead {
		return ErrSHADoesNotMatch{GivenSHA: layer.ExpectedHead, CurrentSHA: head}
	}
	return nil
}

func PrepareStackMergedReceipt(ctx context.Context, pr *issues_model.PullRequest, repoID int64, branch, oldSHA, newSHA string) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil || stack == nil {
		return err
	}
	if stack.RepoID != repoID || stack.TrunkBranch != branch {
		return ErrPullRequestStacked
	}
	op, err := issues_model.GetStackOperation(ctx, stack.ActiveOperationID)
	if err != nil {
		return err
	}
	journal := new(stackJournal)
	if err := json.Unmarshal([]byte(op.JournalJSON), journal); err != nil {
		return err
	}
	layers := remainingStackLayers(journal)
	if journal.Stage != "confirm" || len(layers) == 0 {
		return issues_model.ErrStackRevision
	}
	layer := layers[0]
	if layer.PullID != pr.ID || layer.Phase != "merging" || layer.LandingBaseSHA != oldSHA || layer.MergeCandidateSHA != newSHA {
		return issues_model.ErrStackRevision
	}
	pr.MergedRepoID, pr.MergedBranch, pr.MergedBaseCommitID, pr.MergeBase = repoID, branch, oldSHA, layer.OldParent
	return nil
}

func reconcileStackMerge(ctx context.Context, layer *stackLayerJournal, pr *issues_model.PullRequest, actor *user_model.User) (bool, error) {
	repoID, branch := layer.LandingRepoID, layer.LandingBranch
	if repoID == 0 {
		repoID, branch = pr.BaseRepoID, pr.BaseBranch
	}
	if pr.HasMerged {
		repo, targetBranch, err := pr.GetMergedTarget(ctx)
		if err != nil {
			return false, err
		}
		if repo.ID != repoID || targetBranch != branch || (layer.MergeCandidateSHA != "" && pr.MergedCommitID != layer.MergeCandidateSHA) {
			return false, issues_model.ErrStackRevision
		}
		return pr.MergedCommitID != "", nil
	}
	if layer.MergeCandidateSHA == "" {
		return false, nil
	}
	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return false, err
	}
	err = gitcmd.NewCommand("merge-base", "--is-ancestor").AddDynamicArguments(layer.MergeCandidateSHA, git.BranchPrefix+branch).WithRepo(repo).Run(ctx)
	if err != nil {
		if gitcmd.IsErrorExitCode(err, 1) {
			return false, nil
		}
		return false, err
	}
	source, err := stackLayerSource(ctx, layer)
	if err != nil {
		return false, err
	}
	head, err := git.GetFullCommitID(ctx, source, git.BranchPrefix+layer.HeadBranch)
	if err != nil {
		return false, err
	}
	if head != layer.ExpectedHead {
		return false, ErrSHADoesNotMatch{GivenSHA: layer.ExpectedHead, CurrentSHA: head}
	}
	pr.MergedRepoID, pr.MergedBranch, pr.MergedBaseCommitID, pr.MergeBase = repoID, branch, layer.LandingBaseSHA, layer.OldParent
	merged, err := MarkAsMerged(ctx, pr, layer.MergeCandidateSHA, timeutil.TimeStampNow(), actor, pr.Status)
	if err != nil {
		return false, err
	}
	if merged {
		return true, nil
	}
	return false, nil
}
