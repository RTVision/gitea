// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"
	"strconv"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackAPIPublicOnlyToken(t *testing.T) {
	unittest.PrepareTestEnv(t)
	_, err := db.GetEngine(t.Context()).ID(13).Cols("visibility").Update(&user_model.User{Visibility: api.VisibleTypeLimited})
	require.NoError(t, err)
	readable := make([]*issues_model.PullRequestStack, 0, 2)
	for i, branch := range []string{"develop", "feature/1"} {
		issue := &issues_model.Issue{RepoID: 10, Index: int64(i + 2), PosterID: 12, Title: branch, IsPull: true}
		require.NoError(t, db.Insert(t.Context(), issue))
		pull := &issues_model.PullRequest{IssueID: issue.ID, Index: issue.Index, BaseRepoID: 10, HeadRepoID: 10, BaseBranch: "master", HeadBranch: branch}
		require.NoError(t, db.Insert(t.Context(), pull))
		visible := &issues_model.PullRequestStack{RepoID: 10, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
		require.NoError(t, db.Insert(t.Context(), visible))
		require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: visible.ID, PullRequestID: pull.ID, Position: 1}))
		readable = append(readable, visible)
	}
	stack := &issues_model.PullRequestStack{RepoID: 10, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 3, Position: 1}))
	newContext := func(t *testing.T) *context.APIContext {
		t.Helper()
		ctx, _ := contexttest.MockAPIContext(t, "user12/repo10/stacks")
		contexttest.LoadUser(t, ctx, 1)
		contexttest.LoadRepo(t, ctx, 10)
		ctx.PublicOnly = true
		ctx.SetPathParam("id", strconv.FormatInt(stack.ID, 10))
		return ctx
	}
	ctx := newContext(t)
	_, ok := resolveStackPullRequestIDs(ctx, nil, []api.PullRequestReference{{RepositoryID: 10, PullRequest: 1}})
	assert.False(t, ok, "a readable main PR does not authorize its hidden fork source")
	assert.Equal(t, http.StatusNotFound, ctx.Resp.WrittenStatus())
	ctx = newContext(t)
	_, ok = resolveStackPullRequestIDs(ctx, nil, []api.PullRequestReference{{RepositoryID: 11, PullRequest: 1}})
	assert.False(t, ok, "a hidden repository is rejected before looking up its PR")
	assert.Equal(t, http.StatusNotFound, ctx.Resp.WrittenStatus())
	ctx = newContext(t)
	require.ErrorIs(t, checkStackAPIRead(ctx, stack), util.ErrNotExist)
	ctx.PublicOnly = false
	require.NoError(t, checkStackAPIRead(ctx, stack), "the doer retains access with an unrestricted token")

	for name, handler := range map[string]func(*context.APIContext){
		"get":    GetPullRequestStack,
		"append": AppendPullRequestStack, "insert": InsertPullRequestStack,
		"sync": SynchronizePullRequestStack, "unstack": DeletePullRequestStack,
		"rebase": RebasePullRequestStack, "update": UpdatePullRequestStack, "land": LandPullRequestStack,
		"operations": ListPullRequestStackOperations, "operation": GetPullRequestStackOperation,
		"cancel": CancelPullRequestStackOperation, "retry": RetryPullRequestStackOperation,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := newContext(t)
			handler(ctx)
			assert.Equal(t, http.StatusNotFound, ctx.Resp.WrittenStatus())
		})
	}
	t.Run("list", func(t *testing.T) {
		for page := 1; page <= 3; page++ {
			ctx, response := contexttest.MockAPIContext(t, "user12/repo10/stacks?page="+strconv.Itoa(page)+"&limit=1")
			contexttest.LoadUser(t, ctx, 1)
			contexttest.LoadRepo(t, ctx, 10)
			ctx.PublicOnly = true
			ListPullRequestStacks(ctx)
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, "2", response.Header().Get("X-Total-Count"))
			assert.NotContains(t, response.Header().Get("Link"), "page=3")
			var listed []*api.PullRequestStack
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &listed))
			if page <= 2 {
				require.Len(t, listed, 1)
				assert.Equal(t, readable[2-page].ID, listed[0].Number)
				require.Len(t, listed[0].Entries, 1)
				require.NotNil(t, listed[0].Entries[0].PullRequest)
			} else {
				assert.Empty(t, listed)
			}
		}
		_, err := db.GetEngine(t.Context()).In("stack_id", readable[0].ID, readable[1].ID).Delete(new(issues_model.StackEntry))
		require.NoError(t, err)
		_, err = db.GetEngine(t.Context()).In("id", readable[0].ID, readable[1].ID).Delete(new(issues_model.PullRequestStack))
		require.NoError(t, err)
		ctx := newContext(t)
		ListPullRequestStacks(ctx)
		assert.Equal(t, http.StatusOK, ctx.Resp.WrittenStatus())
		assert.Equal(t, "0", ctx.RespHeader().Get("X-Total-Count"))
	})
	stored := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID})
	assert.EqualValues(t, 1, stored.Revision)
}
