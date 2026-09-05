// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitea.dev/models/db"
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
	"gitea.dev/modules/log"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
)

type StackOperationOptions struct {
	StackID          int64
	ExpectedRevision int64
	ThroughPosition  int
	Kind             string
	MergeStyle       repo_model.MergeStyle
}

type stackJournal struct {
	Stage  string               `json:"stage"`
	Layers []*stackLayerJournal `json:"layers"`
}

var stackOperationQueue *queue.WorkerPoolQueue[int64]

func stackActorPermission(ctx context.Context, stack *issues_model.PullRequestStack, actor *user_model.User) error {
	if actor == nil {
		return ErrNoPermissionToMerge
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	if repo.IsArchived || repo.IsMirror {
		return errors.New("stack operations require a writable, unarchived repository")
	}
	perm, err := access_model.GetDoerRepoPermission(ctx, repo, actor)
	if err != nil {
		return err
	}
	allowed, err := isUserAllowedToMergeInRepoBranch(ctx, repo.ID, stack.TrunkBranch, perm, actor)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNoPermissionToMerge
	}
	return nil
}

func StartStackOperation(ctx context.Context, actor *user_model.User, opts StackOperationOptions) (*issues_model.StackOperation, error) {
	if opts.Kind != "land" && opts.Kind != "rebase" {
		return nil, fmt.Errorf("unknown stack operation %q", opts.Kind)
	}
	if opts.Kind == "land" && opts.MergeStyle != repo_model.MergeStyleMerge && opts.MergeStyle != repo_model.MergeStyleSquash && opts.MergeStyle != repo_model.MergeStyleRebase {
		return nil, errors.New("stacks support merge, squash and rebase landing")
	}
	stack, err := issues_model.GetStackByID(ctx, opts.StackID)
	if err != nil {
		return nil, err
	}
	if err := stackActorPermission(ctx, stack, actor); err != nil {
		return nil, err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return nil, err
	}
	prUnit, err := repo.GetUnit(ctx, unit.TypePullRequests)
	if err != nil {
		return nil, err
	}
	if opts.Kind == "land" && !prUnit.PullRequestsConfig().IsMergeStyleAllowed(opts.MergeStyle) {
		return nil, ErrInvalidMergeStyle{ID: repo.ID, Style: opts.MergeStyle}
	}
	if opts.Kind == "rebase" && !prUnit.PullRequestsConfig().AllowRebaseUpdate {
		return nil, errors.New("server rebase updates are disabled for this repository")
	}
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	journal := &stackJournal{Stage: "restack"}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if pr.HasMerged {
			continue
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		if pr.Issue.IsClosed {
			return nil, ErrIsClosed
		}
		if err := pr.LoadHeadRepo(ctx); err != nil {
			return nil, err
		}
		head, err := git.GetFullCommitID(ctx, pr.HeadRepo, git.BranchPrefix+pr.HeadBranch)
		if err != nil {
			return nil, err
		}
		journal.Layers = append(journal.Layers, &stackLayerJournal{EntryID: entry.ID, PullID: pr.ID, Position: entry.Position, HeadBranch: pr.HeadBranch, ExpectedHead: head, OldParent: entry.OldParentSHA, Phase: "ready"})
	}
	if len(journal.Layers) == 0 {
		return nil, ErrHasMerged
	}
	if opts.ThroughPosition == 0 {
		opts.ThroughPosition = journal.Layers[0].Position
	}
	if opts.ThroughPosition < journal.Layers[0].Position || opts.ThroughPosition > journal.Layers[len(journal.Layers)-1].Position {
		return nil, errors.New("selected prefix is outside the open stack")
	}
	op := &issues_model.StackOperation{StackID: stack.ID, ActorID: actor.ID, ExpectedRevision: opts.ExpectedRevision, Kind: opts.Kind, MergeStyle: string(opts.MergeStyle), ThroughPosition: opts.ThroughPosition, State: "queued"}
	data, err := json.Marshal(journal)
	if err != nil {
		return nil, err
	}
	op.JournalJSON = string(data)
	if err := issues_model.CreateStackOperation(ctx, op); err != nil {
		return nil, err
	}
	enqueueStackOperation(op.ID)
	return op, nil
}

func saveStackJournal(ctx context.Context, op *issues_model.StackOperation, journal *stackJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	op.JournalJSON = string(data)
	return issues_model.SaveStackOperation(ctx, op)
}

func remainingStackLayers(journal *stackJournal) []*stackLayerJournal {
	for i, layer := range journal.Layers {
		if layer.Phase != "landed" {
			return journal.Layers[i:]
		}
	}
	return nil
}

func stackMergeFailedBeforePublication(err error) bool {
	for _, known := range []error{ErrNoPermissionToMerge, ErrNotReadyToMerge, ErrIsClosed, ErrIsWorkInProgress, ErrIsChecking, ErrNotMergeableState, ErrDependenciesLeft, ErrHeadCommitsNotAllVerified, ErrPullRequestStacked} {
		if errors.Is(err, known) {
			return true
		}
	}
	return IsErrSHADoesNotMatch(err) || IsErrRebaseConflicts(err) || IsErrMergeConflicts(err) || IsErrInvalidMergeStyle(err) || IsErrMergeUnrelatedHistories(err) || git.IsErrPushOutOfDate(err)
}

func recordStackLayer(ctx context.Context, op *issues_model.StackOperation, layer *stackLayerJournal, landed bool) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		stack, err := issues_model.GetStackByID(ctx, op.StackID)
		if err != nil {
			return err
		}
		if stack.ActiveOperationID != op.ID || stack.Revision != op.ExpectedRevision {
			return issues_model.ErrStackRevision
		}
		entry := &issues_model.StackEntry{HeadSHA: layer.ExpectedHead, OldParentSHA: layer.OldParent, LandedCommitSHA: layer.LandedSHA}
		cols := []string{"head_sha", "old_parent_sha"}
		if landed {
			cols = append(cols, "landed_commit_sha")
		}
		_, err = db.GetEngine(ctx).Where("id = ? AND stack_id = ?", layer.EntryID, op.StackID).Cols(cols...).Update(entry)
		return err
	})
}

func checkStackLayerHeads(ctx context.Context, journal *stackJournal, repo *repo_model.Repository) error {
	for _, layer := range remainingStackLayers(journal) {
		actual, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+layer.HeadBranch)
		if err != nil {
			return err
		}
		expected := layer.ExpectedHead
		if layer.Phase == "published" {
			expected = layer.NewHead
		}
		if layer.Phase == "publishing" && actual == layer.NewHead {
			continue
		}
		if actual != expected {
			return ErrSHADoesNotMatch{GivenSHA: expected, CurrentSHA: actual}
		}
	}
	return nil
}

func executeStackOperation(ctx context.Context, op *issues_model.StackOperation) error {
	stack, err := issues_model.GetStackByID(ctx, op.StackID)
	if err != nil {
		return err
	}
	if stack.ActiveOperationID != op.ID || stack.Revision != op.ExpectedRevision {
		return issues_model.ErrStackRevision
	}
	actor, err := user_model.GetUserByID(ctx, op.ActorID)
	if err != nil {
		return err
	}
	if err := stackActorPermission(ctx, stack, actor); err != nil {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	journal := new(stackJournal)
	if err := json.Unmarshal([]byte(op.JournalJSON), journal); err != nil {
		return err
	}
	op.State, op.LastError = "running", ""
	if err := saveStackJournal(ctx, op, journal); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		actor, err = user_model.GetUserByID(ctx, op.ActorID)
		if err != nil {
			return err
		}
		if err := stackActorPermission(ctx, stack, actor); err != nil {
			return err
		}
		layers := remainingStackLayers(journal)
		if journal.Stage != "confirm" && journal.Stage != "finish" {
			if err := checkStackLayerHeads(ctx, journal, repo); err != nil {
				return err
			}
		}
		switch journal.Stage {
		case "restack":
			if len(layers) == 0 {
				journal.Stage = "finish"
				break
			}
			trunk, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+stack.TrunkBranch)
			if err != nil {
				return err
			}
			if err := buildStackRebase(ctx, op, layers, trunk, actor); err != nil {
				return err
			}
			journal.Stage = "publish"
		case "publish":
			for _, layer := range layers {
				if layer.Phase == "published" {
					continue
				}
				layer.Phase = "publishing"
				if err := saveStackJournal(ctx, op, journal); err != nil {
					return err
				}
				if err := publishStackLayer(ctx, op, layer, actor); err != nil {
					return err
				}
				layer.Phase = "published"
				if err := saveStackJournal(ctx, op, journal); err != nil {
					return err
				}
			}
			for _, layer := range layers {
				layer.ExpectedHead, layer.OldParent, layer.Phase = layer.NewHead, layer.NewParent, "ready"
				if err := recordStackLayer(ctx, op, layer, false); err != nil {
					return err
				}
			}
			if len(layers) > 0 {
				first, err := issues_model.GetPullRequestByID(ctx, layers[0].PullID)
				if err != nil {
					return err
				}
				if err := first.LoadIssue(ctx); err != nil {
					return err
				}
				if first.BaseBranch != stack.TrunkBranch {
					if err := changeTargetBranchForStack(ctx, first, actor, stack.TrunkBranch, op.ID); err != nil {
						return err
					}
				}
			}
			journal.Stage = "land"
			if op.Kind == "rebase" || len(layers) == 0 || layers[0].Position > op.ThroughPosition {
				journal.Stage = "finish"
			}
		case "land":
			if len(layers) == 0 {
				journal.Stage = "finish"
				break
			}
			layer := layers[0]
			pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
			if err != nil {
				return err
			}
			if err := pr.LoadIssue(ctx); err != nil {
				return err
			}
			if err := pr.LoadBaseRepo(ctx); err != nil {
				return err
			}
			if pr.BaseBranch != stack.TrunkBranch {
				if err := changeTargetBranchForStack(ctx, pr, actor, stack.TrunkBranch, op.ID); err != nil {
					return err
				}
				pr, err = issues_model.GetPullRequestByID(ctx, layer.PullID)
				if err != nil {
					return err
				}
				if err := pr.LoadBaseRepo(ctx); err != nil {
					return err
				}
			}
			if pr.HasMerged && pr.MergedCommitID != "" {
				layer.Phase, journal.Stage = "merging", "confirm"
				break
			}
			trunk, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+stack.TrunkBranch)
			if err != nil {
				return err
			}
			if trunk != layer.OldParent {
				journal.Stage = "restack"
				break
			}
			perm, err := access_model.GetDoerRepoPermission(ctx, repo, actor)
			if err != nil {
				return err
			}
			if err := CheckPullMergeable(ctx, actor, &perm, pr, MergeCheckTypeGeneral, repo_model.MergeStyle(op.MergeStyle), false); err != nil {
				return err
			}
			baseRepo, closer, err := git.RepositoryFromContextOrOpen(ctx, repo)
			if err != nil {
				return err
			}
			message, body, err := GetDefaultMergeMessage(ctx, baseRepo, pr, repo_model.MergeStyle(op.MergeStyle))
			closer.Close()
			if err != nil {
				return err
			}
			if body != "" {
				message += "\n\n" + body
			}
			layer.LandingBaseSHA, layer.MergeCandidateSHA = trunk, ""
			layer.Phase, journal.Stage = "merging", "confirm"
			if err := saveStackJournal(ctx, op, journal); err != nil {
				return err
			}
			mergeErr := mergeWithStackOperation(pr, actor, repo_model.MergeStyle(op.MergeStyle), layer.ExpectedHead, message, false, op.ID, &stackMergePublication{op: op, journal: journal, layer: layer})
			confirmed, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
			if err != nil {
				return err
			}
			if !confirmed.HasMerged || confirmed.MergedCommitID == "" {
				if layer.MergeCandidateSHA == "" || stackMergeFailedBeforePublication(mergeErr) {
					layer.Phase, journal.Stage = "ready", "land"
					if err := saveStackJournal(ctx, op, journal); err != nil {
						return err
					}
					return mergeErr
				}
				return fmt.Errorf("landing result requires reconciliation; no merge will be repeated automatically: %v", mergeErr)
			}
			continue
		case "confirm":
			if len(layers) == 0 {
				return issues_model.ErrInvalidStack
			}
			layer := layers[0]
			pr, err := issues_model.GetPullRequestByID(ctx, layer.PullID)
			if err != nil {
				return err
			}
			merged, err := reconcileStackMerge(ctx, layer, pr, actor)
			if err != nil {
				return err
			}
			if !merged {
				if layer.LandingBaseSHA == "" {
					return errors.New("waiting for the recorded merge result; inspect the PR and complete merge reconciliation before retrying")
				}
				layer.Phase, journal.Stage = "ready", "land"
				break
			}
			if pr.BaseBranch != stack.TrunkBranch {
				return issues_model.ErrInvalidStack
			}
			layer.LandedSHA, layer.Phase = pr.MergedCommitID, "landed"
			if err := recordStackLayer(ctx, op, layer, true); err != nil {
				return err
			}
			op.Completed++
			journal.Stage = "restack"
		case "finish":
			op.State = "completed"
			if err := issues_model.FinishStackOperation(ctx, op); err != nil {
				return err
			}
			cleanupStackCandidates(ctx, repo, op.ID)
			return nil

		default:
			return fmt.Errorf("unknown persisted stack stage %q", journal.Stage)
		}
		if err := saveStackJournal(ctx, op, journal); err != nil {
			return err
		}
	}
}

func cleanupStackCandidates(ctx context.Context, repo *repo_model.Repository, operationID int64) {
	refs, _, err := gitcmd.NewCommand("for-each-ref", "--format=%(refname)").AddDynamicArguments(fmt.Sprintf("refs/stack-operations/%d/", operationID)).WithRepo(repo).RunStdString(ctx)
	if err != nil {
		log.Warn("list completed stack operation %d candidate refs: %v", operationID, err)
		return
	}
	for ref := range strings.FieldsSeq(refs) {
		if err := gitcmd.NewCommand("update-ref", "-d").AddDynamicArguments(ref).WithRepo(repo).Run(ctx); err != nil {
			log.Warn("remove completed stack candidate %s: %v", ref, err)
		}
	}
}

func enqueueStackOperation(id int64) {
	if stackOperationQueue == nil {
		return
	}
	if err := stackOperationQueue.Push(id); err != nil && !errors.Is(err, queue.ErrAlreadyInQueue) {
		log.Error("queue stack operation %d: %v", id, err)
	}
}

func runStackOperation(ctx context.Context, id int64) {
	err := globallock.LockAndDo(ctx, fmt.Sprintf("stack-operation:%d", id), func(ctx context.Context) error {
		op, err := issues_model.GetStackOperation(ctx, id)
		if err != nil {
			return err
		}
		if op.State == "cancelling" {
			stack, err := issues_model.GetStackByID(ctx, op.StackID)
			if err != nil {
				return err
			}
			repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
			if err != nil {
				return err
			}
			_, actor, err := user_model.GetPossibleUserByID(ctx, op.ActorID)
			if err != nil {
				return err
			}
			if err := reconcileStackCancellation(ctx, op, actor, repo); err != nil {
				return err
			}
			cleanupStackCandidates(ctx, repo, op.ID)
			return nil
		}
		if op.State != "queued" && op.State != "running" && op.State != "waiting" {
			return nil
		}
		if err = executeStackOperation(ctx, op); err == nil {
			return nil
		}
		if errors.Is(err, issues_model.ErrStackRevision) {
			return err
		}
		op.State, op.LastError = "blocked", err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			op.State = "queued"
		}
		if errors.Is(err, ErrNotReadyToMerge) || errors.Is(err, ErrIsChecking) || errors.Is(err, ErrNotMergeableState) {
			op.State = "waiting"
		}
		return issues_model.SaveStackOperation(ctx, op)
	})
	if err != nil {
		log.Error("stack operation %d: %v", id, err)
	}
}

func ResumeStackOperation(ctx context.Context, actor *user_model.User, id int64) error {
	op, err := issues_model.GetStackOperation(ctx, id)
	if err != nil {
		return err
	}
	stack, err := issues_model.GetStackByID(ctx, op.StackID)
	if err != nil {
		return err
	}
	if stack.ActiveOperationID != op.ID {
		return issues_model.ErrStackRevision
	}
	if err := stackActorPermission(ctx, stack, actor); err != nil {
		return err
	}
	if actor.ID != op.ActorID {
		return ErrNoPermissionToMerge
	}
	if op.State != "blocked" && op.State != "waiting" {
		return issues_model.ErrStackRevision
	}
	op.State = "queued"
	if err := issues_model.SaveStackOperation(ctx, op); err != nil {
		return err
	}
	enqueueStackOperation(op.ID)
	return nil
}

func CancelStackOperation(ctx context.Context, actor *user_model.User, id int64) error {
	locked, err := globallock.TryLockAndDo(ctx, fmt.Sprintf("stack-operation:%d", id), func(ctx context.Context) error {
		return cancelStackOperation(ctx, actor, id)
	})
	if err != nil {
		return err
	}
	if !locked {
		return issues_model.ErrStackRevision
	}
	return nil
}

func cancelStackOperation(ctx context.Context, actor *user_model.User, id int64) error {
	op, err := issues_model.GetStackOperation(ctx, id)
	if err != nil {
		return err
	}
	stack, err := issues_model.GetStackByID(ctx, op.StackID)
	if err != nil {
		return err
	}
	if stack.ActiveOperationID != op.ID {
		return issues_model.ErrStackRevision
	}
	if err := stackActorPermission(ctx, stack, actor); err != nil {
		return err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, stack.RepoID)
	if err != nil {
		return err
	}
	if err := reconcileStackCancellation(ctx, op, actor, repo); err != nil {
		return err
	}
	cleanupStackCandidates(ctx, repo, op.ID)
	return nil
}

func WakeStackOperationForPull(ctx context.Context, pullID int64) {
	stack, err := issues_model.GetPullRequestStack(ctx, pullID)
	if err != nil || stack == nil || stack.ActiveOperationID == 0 {
		return
	}
	enqueueStackOperation(stack.ActiveOperationID)
}

func InitStackOperations() error {
	queueSettings, err := setting.GetQueueSettings(setting.CfgProvider, "stack_operation")
	if err != nil {
		return err
	}
	if queueSettings.Type == "immediate" || queueSettings.Type == "dummy" {
		queueSettings.Type = "channel" // Git hooks must never execute the operation recursively.
	}
	stackOperationQueue, err = queue.NewWorkerPoolQueueWithContext(graceful.GetManager().ShutdownContext(), "stack_operation", queueSettings, func(ids ...int64) []int64 {
		for _, id := range ids {
			runStackOperation(graceful.GetManager().HammerContext(), id)
		}
		return nil
	}, true)
	if err != nil {
		return err
	}
	queue.GetManager().AddManagedQueue(stackOperationQueue)
	go graceful.GetManager().RunWithCancel(stackOperationQueue)
	ops, err := issues_model.GetActiveStackOperations(graceful.GetManager().HammerContext())
	if err != nil {
		return err
	}
	for _, op := range ops {
		enqueueStackOperation(op.ID)
	}
	return nil
}
