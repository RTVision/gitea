// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
	"strings"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/globallock"
	"gitea.dev/modules/graceful"
	"gitea.dev/modules/json"
	"gitea.dev/modules/timeutil"
)

// stackMergingLayers returns the layers that one pending trunk push publishes, bottom first.
func stackMergingLayers(journal *stackJournal) []*stackLayerJournal {
	layers := remainingStackLayers(journal)
	for i, layer := range layers {
		if layer.Phase != "merging" {
			return layers[:i]
		}
	}
	return layers
}

// stackLayersReady reports whether every layer already contains the trunk and the layer below,
// so landing them publishes exactly the heads their checks ran on.
func stackLayersReady(ctx context.Context, repo *repo_model.Repository, mode, trunk string, layers []*stackLayerJournal) (bool, error) {
	parent := trunk
	for _, layer := range layers {
		if mode != issues_model.StackModeMerge {
			if layer.OldParent != parent {
				return false, nil
			}
		} else if contained, err := stackAncestor(ctx, repo, parent, layer.ExpectedHead); err != nil || !contained {
			return false, err
		}
		parent = layer.ExpectedHead
	}
	return true, nil
}

// landStackLayers merges each layer onto the unpublished merge of the layer below and publishes them all with one
// trunk push, so no layer is rebuilt on a landed predecessor and has to pass its checks again.
func landStackLayers(ctx context.Context, op *issues_model.StackOperation, journal *stackJournal, batch []*stackLayerJournal, trunk string, target *Target, actor *user_model.User) error {
	for _, layer := range batch {
		release, err := globallock.Lock(ctx, getPullWorkingLockKey(layer.PullID))
		if err != nil {
			return err
		}
		defer release()
	}
	style := repo_model.MergeStyle(op.MergeStyle)
	if !target.Repo.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig().IsMergeStyleAllowed(style) {
		return ErrInvalidMergeStyle{ID: target.Repo.ID, Style: style}
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, target.Repo, actor)
	if err != nil {
		return err
	}
	batchCtx := withStackLandingBatch(ctx, batch)
	pulls := make([]*issues_model.PullRequest, len(batch))
	for i, layer := range batch {
		if pulls[i], err = issues_model.GetPullRequestByID(ctx, layer.PullID); err != nil {
			return err
		}
		if err := validateStackOperation(ctx, pulls[i], op.ID, actor.ID); err != nil {
			return err
		}
		if err := CheckPullMergeable(batchCtx, actor, &perm, pulls[i], MergeCheckTypeGeneral, style, false, target); err != nil {
			return fmt.Errorf("#%d: %w", pulls[i].Index, err)
		}
	}

	ctx = graceful.GetManager().HammerContext() // don't abandon a publication halfway when the worker stops
	base := trunk
	var top *mergeContext
	for i, layer := range batch {
		mergeCtx, cancel, err := createTemporaryRepoForMerge(ctx, pulls[i], actor, layer.ExpectedHead, &Target{Repo: target.Repo, Branch: target.Branch, baseCommit: base})
		if err != nil {
			return err
		}
		defer cancel()
		if layer.LandingBaseSHA, layer.MergeCandidateSHA, err = mergeStackLayerOnto(mergeCtx, op, layer, style, i > 0); err != nil {
			return err
		}
		base, top = layer.MergeCandidateSHA, mergeCtx
	}

	for _, layer := range batch {
		layer.Phase = "merging"
	}
	journal.Stage = "confirm"
	if err := saveStackJournal(ctx, op, journal); err != nil {
		return err
	}
	for i, layer := range batch {
		if err := PrepareStackMergedReceipt(ctx, pulls[i], target.Repo.ID, target.Branch, layer.LandingBaseSHA, layer.MergeCandidateSHA); err != nil {
			return err
		}
		if err := recordMergeIntent(ctx, pulls[i], actor, layer.MergeCandidateSHA, false); err != nil {
			return err
		}
	}
	if err := pushMergeToTarget(top, actor, trunk); err != nil {
		return err
	}
	for _, pr := range pulls {
		if _, err := MarkAsMerged(ctx, pr, pr.MergedCommitID, pr.MergedUnix, actor, pr.Status); err != nil {
			return err
		}
		addTestPullRequestTaskAfterWebOperation(pr, actor)
	}
	return nil
}

// mergeStackLayerOnto merges a layer in its temporary repository and stores the candidate in the target repository,
// where the next layer's temporary repository and the final push find it.
func mergeStackLayerOnto(mergeCtx *mergeContext, op *issues_model.StackOperation, layer *stackLayerJournal, style repo_model.MergeStyle, onLanding bool) (base, candidate string, err error) {
	if onLanding && style == repo_model.MergeStyleSquash {
		// the layer below landed as a squash its head lacks; recording it keeps that content out of this squash
		recorded, _, err := gitcmd.NewCommand("commit-tree").AddDynamicArguments(layer.ExpectedHead+"^{tree}").
			AddOptionValues("-p", git.BranchPrefix+tmpRepoTrackingBranch).AddOptionValues("-p", git.BranchPrefix+tmpRepoBaseBranch).
			AddOptionValues("-m", "Record landed stack layer").WithEnv(mergeCtx.env).WithRepo(mergeCtx.tmpRepo).RunStdString(mergeCtx)
		if err != nil {
			return "", "", err
		}
		if err := gitcmd.NewCommand("update-ref").AddDynamicArguments(git.BranchPrefix+tmpRepoTrackingBranch, strings.TrimSpace(recorded)).WithRepo(mergeCtx.tmpRepo).Run(mergeCtx); err != nil {
			return "", "", err
		}
	}
	targetRepo, closer, err := git.RepositoryFromContextOrOpen(mergeCtx, mergeCtx.target.Repo)
	if err != nil {
		return "", "", err
	}
	message, body, err := GetDefaultMergeMessage(mergeCtx, targetRepo, mergeCtx.pr, style, mergeCtx.target)
	closer.Close()
	if err != nil {
		return "", "", err
	}
	if body != "" {
		message += "\n\n" + body
	}
	if err := doMergeStyle(mergeCtx, style, message); err != nil {
		return "", "", err
	}
	if base, candidate, err = publishMergeObjects(mergeCtx); err != nil {
		return "", "", err
	}
	trees, _, err := gitcmd.NewCommand("rev-parse").AddDynamicArguments(candidate+"^{tree}", layer.ExpectedHead+"^{tree}").WithRepo(mergeCtx.tmpRepo).RunStdString(mergeCtx)
	if err != nil {
		return "", "", err
	}
	if pair := strings.Fields(trees); len(pair) != 2 || pair[0] != pair[1] {
		return "", "", fmt.Errorf("landing layer %d would publish content its checks did not run on; update the stack and retry", layer.Position)
	}
	ref := fmt.Sprintf("refs/stack-operations/%d/merge-%d", op.ID, layer.EntryID)
	if err := gitcmd.NewCommand("fetch", "--no-tags", "--no-write-fetch-head").AddDashesAndList(mergeCtx.tmpBasePath, "+"+candidate+":"+ref).WithRepo(mergeCtx.target.Repo).Run(mergeCtx); err != nil {
		return "", "", err
	}
	return base, candidate, nil
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
	op, err := issues_model.GetStackOperation(ctx, stack.ActiveOperationID)
	if err != nil {
		return err
	}
	journal := new(stackJournal)
	if err := json.Unmarshal([]byte(op.JournalJSON), journal); err != nil {
		return err
	}
	layers := stackMergingLayers(journal)
	if journal.Stage != "confirm" || len(layers) == 0 {
		return issues_model.ErrStackRevision
	}
	if top := layers[len(layers)-1]; top.PullID != pullID || layers[0].LandingBaseSHA != oldSHA || top.MergeCandidateSHA != newSHA {
		return issues_model.ErrStackRevision
	}
	ctx = withStackLandingBatch(ctx, layers)
	for i, layer := range layers {
		if i > 0 && layer.LandingBaseSHA != layers[i-1].MergeCandidateSHA {
			return issues_model.ErrStackRevision
		}
		layerPR := pr
		if layer.PullID != pullID {
			if layerPR, err = issues_model.GetPullRequestByID(ctx, layer.PullID); err != nil {
				return err
			}
		}
		if (layer.HeadRepoID != 0 && layer.HeadRepoID != layerPR.HeadRepoID) || (layer.LandingRepoID != 0 && (layer.LandingRepoID != stack.RepoID || layer.LandingBranch != branch)) {
			return issues_model.ErrStackRevision
		}
		if err := checkStackMergeOrder(ctx, layerPR); err != nil {
			return err
		}
		if err := layerPR.LoadHeadRepo(ctx); err != nil {
			return err
		}
		head, err := git.GetFullCommitID(ctx, layerPR.HeadRepo, git.BranchPrefix+layerPR.HeadBranch)
		if err != nil {
			return err
		}
		if head != layer.ExpectedHead {
			return ErrSHADoesNotMatch{GivenSHA: layer.ExpectedHead, CurrentSHA: head}
		}
	}
	return nil
}

// CheckStackPublicationProtections rechecks the reviews and statuses of the layers below the pull request named by a
// stack's trunk push, since that push lands them too.
func CheckStackPublicationProtections(ctx context.Context, pullID int64) error {
	stack, err := issues_model.GetPullRequestStack(ctx, pullID)
	if err != nil || stack == nil {
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
	for _, layer := range stackMergingLayers(journal) {
		if layer.PullID == pullID {
			continue
		}
		pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
		if err != nil {
			return err
		}
		if err := CheckPullBranchProtections(ctx, pr, true); err != nil {
			return fmt.Errorf("#%d: %w", pr.Index, err)
		}
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
	if journal.Stage != "confirm" {
		return issues_model.ErrStackRevision
	}
	for _, layer := range stackMergingLayers(journal) {
		if layer.PullID == pr.ID && layer.LandingBaseSHA == oldSHA && layer.MergeCandidateSHA == newSHA {
			pr.MergedRepoID, pr.MergedBranch, pr.MergedBaseCommitID, pr.MergeBase = repoID, branch, oldSHA, layer.OldParent
			return nil
		}
	}
	return issues_model.ErrStackRevision
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
