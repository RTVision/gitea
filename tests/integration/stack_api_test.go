// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeStackAPI(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		session := loginUser(t, "user2")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
		readToken := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeReadRepository)
		const base = "/api/v1/repos/user2/repo1"
		testEditFileToNewBranch(t, session, "user2", "repo1", "master", "api-stack-layer", "README.md", "stack API layer\n")
		pull := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/pulls", &api.CreatePullRequestOption{
			Head: "api-stack-layer", Base: "master", Title: "Stack API layer",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		testEditFileToNewBranch(t, session, "user2", "repo1", "api-stack-layer", "api-stack-upper", "README.md", "stack API upper\n")
		upper := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/pulls", &api.CreatePullRequestOption{
			Head: "api-stack-upper", Base: "api-stack-layer", Title: "Stack API upper",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		testEditFileToNewBranch(t, session, "user2", "repo1", "master", "api-stack-retarget", "README.md", "stack API retarget\n")
		create := &api.CreatePullRequestStackOption{Trunk: "master", PullRequests: []int64{pull.Index, upper.Index}}
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(readToken), http.StatusForbidden)
		stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
		require.Len(t, stack.Entries, 2)
		assert.Equal(t, pull.Index, stack.Entries[0].PullRequest.Index)
		assert.Equal(t, upper.Index, stack.Entries[1].PullRequest.Index)
		assert.EqualValues(t, 1, stack.Revision)
		path := fmt.Sprintf("%s/stacks/%d", base, stack.Number)
		MakeRequest(t, NewRequest(t, http.MethodGet, "/api/v1/repos/user12/repo10/stacks/capabilities").AddTokenAuth(token), http.StatusOK)
		MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/api/v1/repos/user12/repo10/stacks/%d", stack.Number)).AddTokenAuth(token), http.StatusNotFound)
		listed := MakeRequest(t, NewRequest(t, http.MethodGet, base+"/stacks?page=1&limit=1").AddTokenAuth(readToken), http.StatusOK)
		assert.Equal(t, "1", listed.Header().Get("X-Total-Count"))
		assert.Len(t, DecodeJSON(t, listed, []*api.PullRequestStack{}), 1)
		MakeRequest(t, NewRequest(t, http.MethodDelete, base+"/branches/api-stack-layer").AddTokenAuth(token), http.StatusConflict)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPatch, base+"/branches/api-stack-layer", &api.RenameBranchRepoOption{Name: "renamed-layer"}).AddTokenAuth(token), http.StatusConflict)
		before := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("%s/pulls/%d", base, pull.Index)).AddTokenAuth(readToken), http.StatusOK), &api.PullRequest{})
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", base, pull.Index), &api.EditPullRequestOption{Base: "api-stack-retarget"}).AddTokenAuth(token), http.StatusConflict)
		MakeRequest(t, NewRequest(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/update?style=merge", base, pull.Index)).AddTokenAuth(token), http.StatusConflict)
		after := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("%s/pulls/%d", base, pull.Index)).AddTokenAuth(readToken), http.StatusOK), &api.PullRequest{})
		assert.Equal(t, before.Base.Ref, after.Base.Ref)
		assert.Equal(t, before.Base.Sha, after.Base.Sha)
		assert.Equal(t, before.Head.Ref, after.Head.Ref)
		assert.Equal(t, before.Head.Sha, after.Head.Sha)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/merge", base, upper.Index), map[string]any{"do": "merge"}).AddTokenAuth(token), http.StatusConflict)
		missing := &api.SynchronizePullRequestStackOption{Revision: 1, Heads: []api.PullRequestStackHead{
			{PullRequest: pull.Index, HeadSHA: "wrong", ParentSHA: stack.Entries[0].ParentSHA},
			{PullRequest: upper.Index, HeadSHA: stack.Entries[1].HeadSHA, ParentSHA: stack.Entries[1].ParentSHA},
		}}
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/sync", missing).AddTokenAuth(token), http.StatusConflict)
		sync := &api.SynchronizePullRequestStackOption{Revision: 1, Heads: []api.PullRequestStackHead{
			{PullRequest: pull.Index, HeadSHA: stack.Entries[0].HeadSHA, ParentSHA: stack.Entries[0].ParentSHA},
			{PullRequest: upper.Index, HeadSHA: stack.Entries[1].HeadSHA, ParentSHA: stack.Entries[1].ParentSHA},
		}}
		stack = DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/sync", sync).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
		assert.EqualValues(t, 2, stack.Revision)
		stale := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodDelete, path, &api.PullRequestStackRevisionOption{Revision: 1}).AddTokenAuth(token), http.StatusConflict), map[string]int64{})
		assert.EqualValues(t, 2, stale["revision"])
		setting.Repository.PullRequest.EnableStacks = false
		MakeRequest(t, NewRequest(t, http.MethodGet, path).AddTokenAuth(readToken), http.StatusOK)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusForbidden)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodDelete, path, &api.PullRequestStackRevisionOption{Revision: 2}).AddTokenAuth(token), http.StatusNoContent)
		updated := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("%s/pulls/%d", base, pull.Index)).AddTokenAuth(readToken), http.StatusOK), &api.PullRequest{})
		assert.Nil(t, updated.Stack)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPatch, base+"/branches/api-stack-layer", &api.RenameBranchRepoOption{Name: "renamed-layer"}).AddTokenAuth(token), http.StatusNoContent)
	})
}
