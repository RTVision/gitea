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
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackAPIPublicOnlyToken(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	_, err := db.GetEngine(t.Context()).ID(13).Cols("visibility").Update(&user_model.User{Visibility: api.VisibleTypeLimited})
	require.NoError(t, err)
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
		"get": GetPullRequestStack, "list": ListPullRequestStacks,
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
	stored := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID})
	assert.EqualValues(t, 1, stored.Revision)
}
