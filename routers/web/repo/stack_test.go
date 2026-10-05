// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm/contexts"
)

func TestSuggestForkStackSelection(t *testing.T) {
	ctx, _ := contexttest.MockContext(t, "/user2/repo1/pulls/stacks/new?pull=20&start=10&global_ids=true")
	ctx.Repo.Repository = &repo_model.Repository{ID: 1, DefaultBranch: "main"}
	bottom := &issues_model.PullRequest{ID: 10, Index: 1, BaseRepoID: 1, BaseBranch: "main", HeadRepoID: 2, HeadBranch: "a"}
	top := &issues_model.PullRequest{ID: 20, Index: 1, BaseRepoID: 2, BaseBranch: "a", HeadRepoID: 2, HeadBranch: "b"}
	chain, start, valid := suggestPullStack(ctx, issues_model.PullRequestList{top, bottom})
	require.True(t, valid)
	assert.Equal(t, []*issues_model.PullRequest{bottom, top}, chain)
	assert.Zero(t, start)
	ctx.Req.Form.Set("start", "1")
	_, _, valid = suggestPullStack(ctx, issues_model.PullRequestList{top, bottom})
	assert.False(t, valid)
	ctx.Req.Form.Set("start", "20")
	_, start, valid = suggestPullStack(ctx, issues_model.PullRequestList{top, bottom})
	assert.False(t, valid, "an upstream stack cannot start from a fork-owned base")
	assert.Zero(t, start)
}

func TestStackMemberReadPermission(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx, _ := contexttest.MockContext(t, "/user2/repo1/pulls/stacks")
	contexttest.LoadRepo(t, ctx, 1)
	public := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	private := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 6})
	visible, err := readableStackCandidates(ctx, issues_model.PullRequestList{public, private})
	require.NoError(t, err)
	require.Len(t, visible, 1)
	assert.Equal(t, public.ID, visible[0].ID)
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen}
	require.NoError(t, db.Insert(ctx, stack))
	for i, pr := range []*issues_model.PullRequest{public, private} {
		require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: i + 1}))
	}
	_, err = loadPullStackEntries(ctx, stack.ID)
	assert.ErrorIs(t, err, issues_model.ErrStackNotExist)
}

type stackEntryStatusQueryFailure struct {
	ctx     context.Context
	query   string
	enabled atomic.Bool
	fired   atomic.Bool
}

func (h *stackEntryStatusQueryFailure) BeforeProcess(c *contexts.ContextHook) (context.Context, error) {
	if h.enabled.Load() && c.Ctx == h.ctx && strings.Contains(strings.NewReplacer("`", "", `"`, "").Replace(c.SQL), h.query) {
		h.fired.Store(true)
		return nil, errors.New("optional stack query failed")
	}
	return c.Ctx, nil
}

func (*stackEntryStatusQueryFailure) AfterProcess(*contexts.ContextHook) error { return nil }

func TestForkStackEntryStatus(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx, _ := contexttest.MockContext(t, "/user13/repo11/pulls/1")
	contexttest.LoadUser(t, ctx, 13)
	require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{RepoID: 11, Type: unit.TypePullRequests, Config: &repo_model.PullRequestsConfig{}}))
	contexttest.LoadRepo(t, ctx, 11)
	fork := ctx.Repo.Repository
	main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 10})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 3})
	_, err := db.GetEngine(ctx).ID(pr.ID).NoAutoTime().Cols("base_repo_id").Update(&issues_model.PullRequest{BaseRepoID: fork.ID})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(pr.IssueID).Cols("repo_id").Update(&issues_model.Issue{RepoID: fork.ID})
	require.NoError(t, err)
	require.NotEqual(t, pr.Index, pr.ID)
	require.NotEqual(t, pr.ID, pr.IssueID)
	require.NoError(t, unittest.SyncDirs(filepath.Join(setting.GetGiteaTestSourceRoot(), "tests", "gitea-repositories-meta", fork.FullName()+".git"), gitrepo.RepoLocalPath(fork)))
	gitRepo, err := git.OpenRepository(ctx, fork)
	require.NoError(t, err)
	defer gitRepo.Close()
	head, err := gitRepo.GetBranchCommit(ctx, pr.HeadBranch)
	require.NoError(t, err)
	boundary, err := git.MergeBase(ctx, gitRepo, git.RefNameFromBranch(pr.BaseBranch).String(), head.ID.String())
	require.NoError(t, err)
	refDir := filepath.Join(gitrepo.RepoLocalPath(fork), "refs", "pull", strconv.FormatInt(pr.Index, 10))
	require.NoError(t, os.MkdirAll(refDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(refDir, "head"), []byte(head.ID.String()+"\n"), 0o644))
	stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "master", State: issues_model.StackStateOpen}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx,
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, HeadSHA: head.ID.String(), OldParentSHA: boundary},
		&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(fork.ID, pr.HeadBranch)},
	))
	seedStatus := func(repo *repo_model.Repository, context string, state commitstatus.CommitStatusState) {
		t.Helper()
		require.NoError(t, git_model.NewCommitStatus(ctx, git_model.NewCommitStatusOptions{
			Repo: repo, Creator: ctx.Doer, SHA: head.ID,
			CommitStatus: &git_model.CommitStatus{Context: context, State: state},
		}))
	}
	seedStatus(main, "ci", commitstatus.CommitStatusFailure)
	seedStatus(main, "ci", commitstatus.CommitStatusSuccess)
	seedStatus(fork, "ci", commitstatus.CommitStatusFailure)
	for i, review := range []*issues_model.Review{
		{Type: issues_model.ReviewTypeApprove, Official: true},
		{Type: issues_model.ReviewTypeApprove, Official: true, Stale: true},
		{Type: issues_model.ReviewTypeApprove},
		{Type: issues_model.ReviewTypeApprove, Official: true, Dismissed: true},
		{Type: issues_model.ReviewTypeReject, Official: true},
		{Type: issues_model.ReviewTypeRequest, Official: true},
		{Type: issues_model.ReviewTypePending, Official: true},
	} {
		review.IssueID, review.ReviewerID = pr.IssueID, int64(i+1)
		require.NoError(t, db.Insert(ctx, review))
	}
	for _, check := range []struct {
		state     commitstatus.CommitStatusState
		localeKey string
	}{
		{commitstatus.CommitStatusPending, "repo.pulls.status_checking"},
		{commitstatus.CommitStatusFailure, "repo.pulls.status_checks_failure"},
		{commitstatus.CommitStatusSuccess, "repo.pulls.status_checks_success"},
	} {
		seedStatus(main, "lint", check.state)
		entries, err := loadPullStackEntries(ctx, stack.ID)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		entry := entries[0]
		assert.Equal(t, pr.ID, entry.Pull.ID)
		assert.Equal(t, fork.ID, entry.Pull.BaseRepoID)
		require.Len(t, entry.CommitStatuses, 2)
		states := make(map[string]commitstatus.CommitStatusState, 2)
		for _, status := range entry.CommitStatuses {
			assert.Equal(t, main.ID, status.RepoID)
			assert.Equal(t, head.ID.String(), status.SHA)
			states[status.Context] = status.State
		}
		assert.Equal(t, map[string]commitstatus.CommitStatusState{"ci": commitstatus.CommitStatusSuccess, "lint": check.state}, states)
		require.NotNil(t, entry.CommitStatus)
		assert.Equal(t, check.state, entry.CommitStatus.State)
		assert.Equal(t, check.localeKey, entry.CheckStatusLocaleKey())
		assert.EqualValues(t, 2, entry.ApprovalCount)
		assert.EqualValues(t, 1, entry.ChangeRequestCount)
		assert.EqualValues(t, 1, entry.WaitingReviewCount)
	}
	for _, failedLookup := range []string{"statuses", "reviews"} {
		t.Run(failedLookup, func(t *testing.T) {
			query := "FROM review"
			if failedLookup == "statuses" {
				query = "FROM repository WHERE id IN"
			}
			hook := &stackEntryStatusQueryFailure{ctx: ctx, query: query}
			hook.enabled.Store(true)
			unittest.GetXORMEngine().AddHook(hook)
			t.Cleanup(func() { hook.enabled.Store(false) })
			entries, err := loadPullStackEntries(ctx, stack.ID)
			require.True(t, hook.fired.Load(), "the optional query must fail")
			require.NoError(t, err)
			require.Len(t, entries, 1)
			entry := entries[0]
			assert.Equal(t, pr.ID, entry.Pull.ID)
			assert.Equal(t, pr.IssueID, entry.Pull.Issue.ID)
			assert.Equal(t, fork.ID, entry.Pull.BaseRepo.ID)
			if failedLookup == "statuses" {
				assert.Empty(t, entry.CommitStatuses)
				assert.Nil(t, entry.CommitStatus)
				assert.EqualValues(t, 2, entry.ApprovalCount)
				assert.EqualValues(t, 1, entry.ChangeRequestCount)
				assert.EqualValues(t, 1, entry.WaitingReviewCount)
			} else {
				require.Len(t, entry.CommitStatuses, 2)
				require.NotNil(t, entry.CommitStatus)
				assert.Equal(t, commitstatus.CommitStatusSuccess, entry.CommitStatus.State)
				assert.Zero(t, entry.ApprovalCount)
				assert.Zero(t, entry.ChangeRequestCount)
				assert.Zero(t, entry.WaitingReviewCount)
			}
		})
	}
}

func TestForkPullMergedTargetDeleted(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx, _ := contexttest.MockContext(t, "/user13/repo11/pulls/1")
	contexttest.LoadRepo(t, ctx, 11)
	pull := &issues_model.PullRequest{
		BaseRepoID: 11, BaseRepo: ctx.Repo.Repository, BaseBranch: "parent",
		HeadRepoID: 11, HeadRepo: ctx.Repo.Repository, HeadBranch: "layer",
		HasMerged: true, MergedRepoID: 10, MergedBranch: "master", MergedCommitID: "1234567890abcdef",
	}
	_, err := db.GetEngine(ctx).ID(10).Delete(&repo_model.Repository{})
	require.NoError(t, err)
	_, _, err = pull.GetMergedTarget(ctx)
	require.True(t, repo_model.IsErrRepoNotExist(err))
	info := &pullRequestViewInfo{issue: &issues_model.Issue{PullRequest: pull}}
	info.setTemplateDataMergeTarget(ctx)
	assert.False(t, ctx.Written())
	assert.Equal(t, "master", ctx.Data["BaseTarget"])
	assert.Empty(t, ctx.Data["BaseBranchLink"])
	assert.Empty(t, ctx.Data["MergedCommitLink"])
	assert.Equal(t, int64(11), pull.BaseRepoID)
	assert.Equal(t, "parent", pull.BaseBranch)
	assert.Equal(t, int64(10), pull.MergedRepoID)
}

func TestForkStackWebCapabilities(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx, _ := contexttest.MockContext(t, "/user12/repo10/pulls/stacks")
	contexttest.LoadUser(t, ctx, 13)
	contexttest.LoadRepo(t, ctx, 10)
	_, err := db.GetEngine(ctx).ID(8).Cols("poster_id").Update(&issues_model.Issue{PosterID: ctx.Doer.ID})
	require.NoError(t, err)
	stack := &issues_model.PullRequestStack{RepoID: 10, TrunkBranch: "master", State: issues_model.StackStateOpen}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 3, Position: 1}))
	op := &issues_model.StackOperation{StackID: stack.ID, Kind: "land", State: "blocked"}
	require.NoError(t, db.Insert(ctx, op))
	data := &pullStackData{Repo: ctx.Repo.Repository, Stack: stack, Operation: op}
	assert.True(t, canOpenPullStackForm(ctx))
	require.NoError(t, setPullStackCapabilities(ctx, data))
	assert.Equal(t, true, ctx.Data["CanManageStack"])
	assert.Equal(t, false, ctx.Data["CanLandStack"])
	assert.Equal(t, false, ctx.Data["CanOperateStack"])
	op.Kind = "update"
	require.NoError(t, setPullStackCapabilities(ctx, data))
	assert.Equal(t, true, ctx.Data["CanOperateStack"])

	retryCtx, resp := contexttest.MockContext(t, "/user12/repo10/pulls/stacks")
	contexttest.LoadUser(t, retryCtx, 13)
	contexttest.LoadRepo(t, retryCtx, 10)
	retryCtx.SetPathParam("id", strconv.FormatInt(stack.ID, 10))
	retryCtx.SetPathParam("action", "retry")
	retryCtx.Req.Form.Set("operation", strconv.FormatInt(op.ID, 10))
	PullStackAction(retryCtx)
	assert.Equal(t, http.StatusForbidden, resp.Code, "retry uses the stored landing kind")

	contexttest.LoadUser(t, ctx, 12)
	contexttest.LoadRepo(t, ctx, 10)
	require.NoError(t, setPullStackCapabilities(ctx, data))
	assert.Equal(t, true, ctx.Data["CanManageStack"])
	assert.Equal(t, true, ctx.Data["CanLandStack"])
	contexttest.LoadUser(t, ctx, 11)
	contexttest.LoadRepo(t, ctx, 10)
	require.NoError(t, setPullStackCapabilities(ctx, data))
	assert.Equal(t, false, ctx.Data["CanManageStack"])
	assert.Equal(t, false, ctx.Data["CanLandStack"])
}

func TestForkPullStackCreationDestination(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	require.NoError(t, db.Insert(t.Context(), &repo_model.RepoUnit{RepoID: 11, Type: unit.TypePullRequests, Config: &repo_model.PullRequestsConfig{}}))
	ctx, _ := contexttest.MockContext(t, "/user13/repo11/pulls/1")
	contexttest.LoadUser(t, ctx, 13)
	contexttest.LoadRepo(t, ctx, 11)
	_, err := db.GetEngine(ctx).ID(8).Cols("poster_id").Update(&issues_model.Issue{PosterID: ctx.Doer.ID})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(9).Cols("repo_id", "poster_id").Update(&issues_model.Issue{RepoID: 11, PosterID: ctx.Doer.ID})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(4).Cols("base_repo_id", "head_repo_id", "base_branch").Update(&issues_model.PullRequest{BaseRepoID: 11, HeadRepoID: 11, BaseBranch: "branch2"})
	require.NoError(t, err)
	upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 4})
	require.NoError(t, preparePullStackCreation(ctx, upper))
	assert.Equal(t, true, ctx.Data["CanCreateStack"])
	assert.Equal(t, fmt.Sprintf("/user12/repo10/pulls/stacks/new?pull=%d&global_ids=true", upper.ID), ctx.Data["CreateStackURL"])

	mainCtx, _ := contexttest.MockContext(t, "/user12/repo10/pulls/stacks/new")
	contexttest.LoadUser(t, mainCtx, 13)
	contexttest.LoadRepo(t, mainCtx, 10)
	lower := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 3})
	foreign := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	visible, err := creatableStackCandidates(mainCtx, issues_model.PullRequestList{lower, upper, foreign})
	require.NoError(t, err)
	assert.Equal(t, issues_model.PullRequestList{lower, upper}, visible)
	upper.BaseBranch = "master"
	_, err = db.GetEngine(ctx).ID(upper.ID).Cols("base_branch").Update(upper)
	require.NoError(t, err)
	require.NoError(t, preparePullStackCreation(ctx, upper))
	assert.Equal(t, fmt.Sprintf("/user13/repo11/pulls/stacks/new?pull=%d&global_ids=true", upper.ID), ctx.Data["CreateStackURL"])
	upper.BaseBranch = "branch2"
	_, err = db.GetEngine(ctx).ID(upper.ID).Cols("base_branch").Update(upper)
	require.NoError(t, err)

	stack := &issues_model.PullRequestStack{RepoID: 10, TrunkBranch: "master", State: issues_model.StackStateOpen}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx,
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: lower.ID, Position: 1},
		&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: lower.ID, BranchKey: issues_model.StackBranchKey(lower.HeadRepoID, lower.HeadBranch)},
	))
	require.NoError(t, preparePullStackCreation(ctx, upper))
	assert.Equal(t, stack.ID, ctx.Data["AppendStackID"])
	assert.Equal(t, "/user12/repo10", ctx.Data["AppendStackRepoLink"])
}
