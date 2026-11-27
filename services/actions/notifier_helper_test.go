// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"errors"
	"testing"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	actions_module "gitea.dev/modules/actions"
	"gitea.dev/modules/actions/jobparser"
	"gitea.dev/modules/git"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	webhook_module "gitea.dev/modules/webhook"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIfNeedApproval(t *testing.T) {
	alwaysWrite := func(_ context.Context, _ *repo_model.Repository, _ *user_model.User) (bool, error) {
		return true, nil
	}
	neverWrite := func(_ context.Context, _ *repo_model.Repository, _ *user_model.User) (bool, error) {
		return false, nil
	}
	hasMerged := func(_ context.Context, _, _ int64) (bool, error) { return true, nil }
	noMerged := func(_ context.Context, _, _ int64) (bool, error) { return false, nil }
	errPerm := errors.New("perm error")
	errMerge := errors.New("merge error")

	forkRun := &actions_model.ActionRun{IsForkPullRequest: true, TriggerEvent: actions_module.GithubEventPullRequest}
	nonForkRun := &actions_model.ActionRun{IsForkPullRequest: false, TriggerEvent: actions_module.GithubEventPullRequest}
	prTargetRun := &actions_model.ActionRun{IsForkPullRequest: true, TriggerEvent: actions_module.GithubEventPullRequestTarget}

	repo := &repo_model.Repository{ID: 1}
	normalUser := &user_model.User{ID: 10}
	restrictedUser := &user_model.User{ID: 11, IsRestricted: true}

	for _, tt := range []struct {
		name      string
		run       *actions_model.ActionRun
		user      *user_model.User
		canWrite  func(context.Context, *repo_model.Repository, *user_model.User) (bool, error)
		hasMerged func(context.Context, int64, int64) (bool, error)
		check     assert.BoolAssertionFunc
	}{
		{"not a fork PR never needs approval", nonForkRun, normalUser, alwaysWrite, hasMerged, assert.False},
		{"pull_request_target never needs approval even when fork", prTargetRun, normalUser, alwaysWrite, hasMerged, assert.False},
		{"restricted user always needs approval", forkRun, restrictedUser, alwaysWrite, hasMerged, assert.True},
		{"fork PR with write permission does not need approval", forkRun, normalUser, alwaysWrite, noMerged, assert.False},
		{"fork PR with merged PR but no write permission does not need approval", forkRun, normalUser, neverWrite, hasMerged, assert.False},
		{"fork PR with no write and no merged PR needs approval", forkRun, normalUser, neverWrite, noMerged, assert.True},
	} {
		t.Run(tt.name, func(t *testing.T) {
			need, err := ifNeedApprovalWith(t.Context(), tt.run, repo, tt.user, tt.canWrite, tt.hasMerged)
			require.NoError(t, err)
			tt.check(t, need)
		})
	}

	t.Run("canWriteActions error is propagated", func(t *testing.T) {
		failWrite := func(_ context.Context, _ *repo_model.Repository, _ *user_model.User) (bool, error) {
			return false, errPerm
		}
		_, err := ifNeedApprovalWith(t.Context(), forkRun, repo, normalUser, failWrite, noMerged)
		require.ErrorIs(t, err, errPerm)
	})

	t.Run("hasMergedPR error is propagated", func(t *testing.T) {
		failMerge := func(_ context.Context, _, _ int64) (bool, error) { return false, errMerge }
		_, err := ifNeedApprovalWith(t.Context(), forkRun, repo, normalUser, neverWrite, failMerge)
		require.ErrorIs(t, err, errMerge)
	})

	t.Run("restricted user skips permission check entirely", func(t *testing.T) {
		// The perm and merge functions must not be called for a restricted user.
		called := false
		trackWrite := func(_ context.Context, _ *repo_model.Repository, _ *user_model.User) (bool, error) {
			called = true
			return true, nil
		}
		need, err := ifNeedApprovalWith(t.Context(), forkRun, repo, restrictedUser, trackWrite, noMerged)
		require.NoError(t, err)
		assert.True(t, need)
		assert.False(t, called, "permission check must not run for restricted user")
	})
}

func TestWorkflowMatchPayloadUsesStackTrunkWithoutChangingEvent(t *testing.T) {
	payload := &api.PullRequestPayload{PullRequest: &api.PullRequest{
		Base:  &api.PRBranchInfo{Ref: "feature-parent", Sha: "parent-sha"},
		Stack: &api.PullRequestStackRef{Base: &api.PullRequestStackBase{Ref: "release", Sha: "trunk-sha"}},
	}}

	matched, ok := workflowMatchPayload(payload).(*api.PullRequestPayload)
	require.True(t, ok)
	assert.Equal(t, "release", matched.PullRequest.Base.Ref)
	assert.Equal(t, "trunk-sha", matched.PullRequest.Base.Sha)
	assert.Equal(t, "feature-parent", payload.PullRequest.Base.Ref)
	assert.Equal(t, "parent-sha", payload.PullRequest.Base.Sha)
}

func TestValidateStackWorkflowPayloadFailsClosed(t *testing.T) {
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "release"}
	payload := &api.PullRequestPayload{PullRequest: &api.PullRequest{Stack: &api.PullRequestStackRef{Base: &api.PullRequestStackBase{Ref: "release", Sha: "trunk-sha"}}}}
	assert.Error(t, validateStackWorkflowPayload(stack, payload))
	payload.PullRequest.Stack.Base.Repository = &api.Repository{ID: 2}
	assert.Error(t, validateStackWorkflowPayload(stack, payload))
	payload.PullRequest.Stack.Base.Repository.ID = stack.RepoID
	assert.NoError(t, validateStackWorkflowPayload(stack, payload))
	payload.PullRequest.Stack.Base.Sha = ""
	assert.Error(t, validateStackWorkflowPayload(stack, payload))
	payload.PullRequest.Stack.Base.Sha = "trunk-sha"
	payload.PullRequest.Stack.Base.Ref = "feature-parent"
	assert.Error(t, validateStackWorkflowPayload(stack, payload))
	payload.PullRequest.Stack = nil
	assert.Error(t, validateStackWorkflowPayload(stack, payload))
	assert.Error(t, validateStackWorkflowPayload(stack, &api.PullRequestPayload{}))
	assert.Error(t, validateStackWorkflowPayload(stack, nil))
	assert.Error(t, validateStackWorkflowPayload(stack, &api.IssuePayload{}))
}

func TestValidateStackWorkflowPayloadAllowsComments(t *testing.T) {
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "release"}
	payload := &api.IssueCommentPayload{
		Action:  api.HookIssueCommentCreated,
		Issue:   &api.Issue{Index: 1},
		Comment: &api.Comment{Body: "/apply"},
		IsPull:  true,
	}
	assert.NoError(t, validateStackWorkflowPayload(stack, payload))
	assert.Equal(t, payload, workflowMatchPayload(payload), "comment payloads must not be rewritten to the stack trunk")
}

func TestGetApprovalUsersAddsForkPullRequestAuthorUnlessDefaultBranchWorkflow(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	approvalUsers, err := getApprovalUsers(t.Context(), &notifyInput{Doer: doer, PullRequest: pr, Event: webhook_module.HookEventPullRequest}, true)
	require.NoError(t, err)
	require.Len(t, approvalUsers, 2)
	assert.Equal(t, []int64{doer.ID, pr.Issue.PosterID}, []int64{approvalUsers[0].ID, approvalUsers[1].ID})

	approvalUsers, err = getApprovalUsers(t.Context(), &notifyInput{Doer: doer, PullRequest: pr, Event: webhook_module.HookEventIssueComment}, true)
	require.NoError(t, err)
	assert.Equal(t, []*user_model.User{doer}, approvalUsers)
}

func TestFilteredWorkflowCommitStatusForForkPullRequest(t *testing.T) {
	forkPR := &issues_model.PullRequest{
		Flow:       issues_model.PullRequestFlowGithub,
		BaseRepoID: 1,
		HeadRepoID: 2,
		Index:      1,
	}
	input := newNotifyInput(&repo_model.Repository{ID: 1}, &user_model.User{ID: 2}, actions_module.GithubEventPullRequest).WithPullRequest(forkPR)

	assert.True(t, isForkPullRequestInput(input))
	forkPR.BaseRepoID = forkPR.HeadRepoID
	assert.False(t, forkPR.IsFromFork())
	assert.True(t, isForkPullRequestInput(input), "a fork-owned upper PR still crosses the workflow destination")
	assert.Equal(t, "refs/pull/1/head", input.Ref.String())
	assert.False(t, shouldCreateSkippedCommitStatusForFilteredWorkflow(input, &actions_module.DetectedWorkflow{
		TriggerEvent: &jobparser.Event{Name: actions_module.GithubEventPullRequest},
	}))
	assert.True(t, shouldCreateSkippedCommitStatusForFilteredWorkflow(input, &actions_module.DetectedWorkflow{
		TriggerEvent: &jobparser.Event{Name: actions_module.GithubEventPullRequestTarget},
	}))

	assert.True(t, shouldCreateSkippedCommitStatusForFilteredWorkflow(newNotifyInput(&repo_model.Repository{ID: 1}, &user_model.User{ID: 2}, actions_module.GithubEventPullRequest), &actions_module.DetectedWorkflow{
		TriggerEvent: &jobparser.Event{Name: actions_module.GithubEventPullRequest},
	}))
}

func TestScopedWorkflowFiltersUseStackTrunk(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	source := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	branch, err := git_model.GetBranchExisting(t.Context(), source.ID, source.DefaultBranch)
	require.NoError(t, err)
	t.Cleanup(func() { scopedWorkflowCache.Remove(source.ID) })
	payload := &api.PullRequestPayload{Action: api.HookIssueOpened, PullRequest: &api.PullRequest{
		Base:  &api.PRBranchInfo{Ref: "feature-parent", Sha: "parent-sha"},
		Stack: &api.PullRequestStackRef{Base: &api.PullRequestStackBase{Ref: "release", Sha: "trunk-sha"}},
	}}
	for _, tc := range []struct {
		filter  string
		matched bool
	}{
		{"branches: [release]", true},
		{"branches: [feature-parent]", false},
		{"branches-ignore: [release]", false},
		{"branches-ignore: [feature-parent]", true},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			content := []byte("on:\n  pull_request:\n    " + tc.filter + "\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
			events, err := actions_module.GetEventsFromContent(content)
			require.NoError(t, err)
			scopedWorkflowCache.Add(source.ID, &cachedScopedWorkflows{sha: branch.CommitID, parsed: []*actions_module.ParsedScopedWorkflow{{EntryName: "check.yml", Content: content, Events: events}}})
			matched, filtered, err := detectScopedWorkflowsForSource(t.Context(), &notifyInput{Event: actions_module.GithubEventPullRequest, Payload: payload}, nil, nil, source)
			require.NoError(t, err)
			assert.Equal(t, tc.matched, len(matched) == 1)
			assert.Equal(t, !tc.matched, len(filtered) == 1)
			assert.Equal(t, "feature-parent", payload.PullRequest.Base.Ref)
			assert.Equal(t, "parent-sha", payload.PullRequest.Base.Sha)
		})
	}
}

func TestDetectAndHandleSchedulesDropsRemovedWorkflows(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, actions_model.ReplaceScheduleTasksByRepo(t.Context(), repo.ID, []*actions_model.ActionSchedule{{RepoID: repo.ID, WorkflowID: "removed.yml"}}))

	require.NoError(t, DetectAndHandleSchedules(t.Context(), repo))
	unittest.AssertNotExistsBean(t, &actions_model.ActionSchedule{RepoID: repo.ID})
}

func TestMergeQueueWorkflowEvents(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	doer := repo.MustOwner(ctx)
	notifier := new(actionsNotifier)

	defer test.MockVariableValue(&EmitJobsIfReadyByRun, func(_runID int64) error { return nil })()
	require.NoError(t, db.Insert(ctx, &git_model.ProtectedBranch{
		RepoID: repo.ID, RuleName: repo.DefaultBranch,
		ProtectedBranchConfig: git_model.ProtectedBranchConfig{EnableStatusCheck: true, StatusCheckContexts: []string{"*"}},
	}))

	const groupRef = "refs/heads/gitea-merge-queue/master/pr-1"
	require.NoError(t, git.ForceFastImport(ctx, repo, []git.FastImportCommit{{
		Ref: groupRef,
		Files: []git.FastImportFile{{Path: ".gitea/workflows/queue.yml", Content: `
on: {push: {}, create: {}, merge_group: {types: [checks_requested, destroyed]}, pull_request: {types: [dequeued]}}
concurrency: {group: '${{ github.ref }}', cancel-in-progress: true}
jobs: {check: {runs-on: linux, steps: [{run: echo done}]}}
`}},
	}}))
	headSHA, err := git.GetFullCommitID(ctx, repo, groupRef)
	require.NoError(t, err)

	entry := &pull_model.MergeQueueEntry{GroupRef: groupRef, GroupHeadSHA: headSHA, BaseBranch: repo.DefaultBranch}
	notifier.MergeGroupChanged(ctx, doer, repo, entry, api.HookMergeGroupChecksRequested, "")
	runs, err := db.Find[actions_model.ActionRun](ctx, actions_model.FindRunOptions{RepoID: repo.ID, Ref: groupRef})
	require.NoError(t, err)
	require.Len(t, runs, 3)
	assert.Equal(t, webhook_module.HookEventMergeGroup, runs[0].Event)
	assert.Equal(t, webhook_module.HookEventCreate, runs[1].Event)
	assert.Equal(t, webhook_module.HookEventPush, runs[2].Event)
	assert.Equal(t, actions_model.StatusWaiting, runs[0].Status)
	push, err := runs[2].GetPushEventPayload()
	require.NoError(t, err)
	assert.Equal(t, 1, push.TotalCommits)
	unittest.AssertExistsAndLoadBean(t, &git_model.CommitStatus{RepoID: repo.ID, SHA: headSHA, Context: "queue.yml / check (merge_group)"})
	require.NoError(t, git.RemoveRef(ctx, repo, groupRef))
	notifier.MergeGroupChanged(ctx, doer, repo, entry, api.HookMergeGroupDestroyed, "merged")
	unittest.AssertCount(t, &actions_model.ActionRun{RepoID: repo.ID, Ref: groupRef}, 3)

	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadIssue(ctx))
	pr.BaseBranch = "deleted"
	require.NoError(t, notify(ctx, newNotifyInput(repo, doer, webhook_module.HookEventPullRequest).
		WithPayload(&api.PullRequestPayload{Action: api.HookIssueDequeued}).
		WithPullRequest(pr)))

	pr.BaseBranch = repo.DefaultBranch
	pr.HasMerged, pr.Issue.IsClosed, pr.MergedCommitID = true, true, headSHA
	notifier.PullRequestMergeQueueChanged(ctx, doer, pr, api.HookIssueDequeued, issues_model.MergeQueueRemovedMerged)
	unittest.AssertCount(t, &actions_model.ActionRun{
		RepoID: repo.ID, WorkflowID: "queue.yml", Event: webhook_module.HookEventPullRequest,
		Ref: pr.GetGitHeadRefName(), CommitSHA: headSHA, WorkflowCommitSHA: headSHA,
	}, 1)
}
