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
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	actions_module "gitea.dev/modules/actions"
	"gitea.dev/modules/actions/jobparser"
	api "gitea.dev/modules/structs"
	webhook_module "gitea.dev/modules/webhook"
	"gitea.dev/services/convert"

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

	t.Run("not a fork PR never needs approval", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), nonForkRun, repo, normalUser, alwaysWrite, hasMerged)
		require.NoError(t, err)
		assert.False(t, need)
	})

	t.Run("pull_request_target never needs approval even when fork", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), prTargetRun, repo, normalUser, alwaysWrite, hasMerged)
		require.NoError(t, err)
		assert.False(t, need)
	})

	t.Run("restricted user always needs approval", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), forkRun, repo, restrictedUser, alwaysWrite, hasMerged)
		require.NoError(t, err)
		assert.True(t, need)
	})

	t.Run("fork PR with write permission does not need approval", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), forkRun, repo, normalUser, alwaysWrite, noMerged)
		require.NoError(t, err)
		assert.False(t, need)
	})

	t.Run("fork PR with merged PR but no write permission does not need approval", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), forkRun, repo, normalUser, neverWrite, hasMerged)
		require.NoError(t, err)
		assert.False(t, need)
	})

	t.Run("fork PR with no write and no merged PR needs approval", func(t *testing.T) {
		need, err := ifNeedApprovalWith(t.Context(), forkRun, repo, normalUser, neverWrite, noMerged)
		require.NoError(t, err)
		assert.True(t, need)
	})

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
	}
	input := newPullRequestReviewNotifyInput(&repo_model.Repository{ID: 1}, &user_model.User{ID: 2}, actions_module.GithubEventPullRequest, "refs/pull/1/head", forkPR)

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

func TestForkStackWorkflowDestination(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 4})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: pr.BaseRepoID})
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)}))
	payload := &api.PullRequestPayload{Repository: &api.Repository{ID: repo.ID}, PullRequest: &api.PullRequest{
		Base:  &api.PRBranchInfo{RepoID: repo.ID, Ref: pr.BaseBranch},
		Head:  &api.PRBranchInfo{Sha: "event-source-sha"},
		Stack: &api.PullRequestStackRef{Base: &api.PullRequestStackBase{Repository: &api.Repository{ID: stack.RepoID}, Ref: stack.TrunkBranch, Sha: "trunk-sha"}},
	}}
	input := newNotifyInput(repo, doer, webhook_module.HookEventPullRequest).WithPayload(payload).WithPullRequest(pr)
	resolved, err := resolvePullRequestWorkflowInput(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, stack.RepoID, resolved.Repo.ID)
	assert.Equal(t, issues_model.StackHeadRefName(pr.ID), resolved.Ref.String())
	commitRef, err := pullRequestWorkflowCommitRef(resolved, resolved.Ref)
	require.NoError(t, err)
	assert.Equal(t, payload.PullRequest.Head.Sha, commitRef.String())
	assert.False(t, pr.IsFromFork())
	assert.True(t, isForkPullRequestInput(resolved))
	assert.Equal(t, repo.ID, input.Repo.ID)
	assert.Equal(t, pr.GetGitHeadRefName(), input.Ref.String())
	assert.Equal(t, repo.ID, payload.Repository.ID)
	resolvedPayload, ok := resolved.Payload.(*api.PullRequestPayload)
	require.True(t, ok)
	assert.Equal(t, stack.RepoID, resolvedPayload.Repository.ID)
	assert.Equal(t, repo.ID, resolvedPayload.PullRequest.Base.RepoID)
	require.NoError(t, issues_model.ReleaseStackBranchClaims(ctx, stack.ID))
	pr.HasMerged, pr.MergedRepoID, pr.MergedBranch = true, stack.RepoID, stack.TrunkBranch
	input.WithRef("landed-commit")
	resolved, err = resolvePullRequestWorkflowInput(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, stack.RepoID, resolved.Repo.ID)
	assert.Equal(t, "landed-commit", resolved.Ref.String())
	commitRef, err = pullRequestWorkflowCommitRef(resolved, resolved.Ref)
	require.NoError(t, err)
	assert.Equal(t, "landed-commit", commitRef.String())
}

func TestPrivateForkStackWorkflowDestination(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: pr.BaseRepoID})
	main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	require.True(t, main.IsPrivate)
	doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
	stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: main.DefaultBranch, State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx,
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1},
		&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)},
	))
	anonymous := convert.ToAPIPullRequest(ctx, pr, nil)
	require.NotNil(t, anonymous)
	assert.Nil(t, anonymous.Stack)
	converted := convert.ToAPIPullRequestForNotification(ctx, pr)
	require.NotNil(t, converted)
	require.NotNil(t, converted.Stack)
	assert.Equal(t, main.ID, converted.Stack.Base.Repository.ID)
	assert.NotEmpty(t, converted.Stack.Base.Sha)
	payload := &api.PullRequestPayload{Repository: anonymous.Base.Repository, PullRequest: converted}
	resolved, err := resolvePullRequestWorkflowInput(ctx, newNotifyInput(repo, doer, webhook_module.HookEventPullRequest).WithPayload(payload).WithPullRequest(pr))
	require.NoError(t, err)
	assert.Equal(t, main.ID, resolved.Repo.ID)
	assert.Equal(t, issues_model.StackHeadRefName(pr.ID), resolved.Ref.String())
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
