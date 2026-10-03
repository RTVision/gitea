// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	auth_model "gitea.dev/models/auth"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	"gitea.dev/services/forms"
	repo_service "gitea.dev/services/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkContributorManagesOwnStack(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		maintainer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
		contributor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		main, err := repo_service.CreateRepository(t.Context(), maintainer, maintainer, repo_service.CreateRepoOptions{
			Name: "contributor-stack-main", DefaultBranch: "main", AutoInit: true, Readme: "Default",
		})
		require.NoError(t, err)
		fork, err := repo_service.ForkRepository(t.Context(), contributor, contributor, repo_service.ForkRepoOptions{BaseRepo: main, Name: "contributor-stack-fork"})
		require.NoError(t, err)
		originalTrunk, err := git.GetFullCommitID(t.Context(), main, "refs/heads/main")
		require.NoError(t, err)
		for _, layer := range []struct{ base, head string }{{"main", "lower"}, {"lower", "upper"}, {"upper", "foreign-upper"}} {
			testCreateFileInBranch(t, contributor, fork, createFileInBranchOptions{OldBranch: layer.base, NewBranch: layer.head}, map[string]string{layer.head + ".txt": layer.head + "\n"})
		}
		session := loginUser(t, contributor.Name)
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
		maintainerToken := getTokenForLoggedInUser(t, loginUser(t, maintainer.Name), auth_model.AccessTokenScopeWriteRepository)
		mainAPI, forkAPI := "/api/v1/repos/"+main.FullName(), "/api/v1/repos/"+fork.FullName()
		lower := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, mainAPI+"/pulls", &api.CreatePullRequestOption{
			Head: fork.FullName() + ":lower", Base: "main", Title: "Contributor lower layer",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		upper := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, forkAPI+"/pulls", &api.CreatePullRequestOption{
			Head: "upper", Base: "lower", Title: "Contributor upper layer",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		foreign := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, forkAPI+"/pulls", &api.CreatePullRequestOption{
			Head: "foreign-upper", Base: "upper", Title: "Another author's layer",
		}).AddTokenAuth(maintainerToken), http.StatusCreated), &api.PullRequest{})
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/merge", mainAPI, lower.Index), &forms.MergePullRequestForm{Do: "merge"}).AddTokenAuth(token), http.StatusMethodNotAllowed)
		stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, mainAPI+"/stacks", &api.CreatePullRequestStackOption{
			Trunk: "main", PullRequestRefs: []api.PullRequestReference{{RepositoryID: main.ID, PullRequest: lower.Index}},
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
		stackAPI := fmt.Sprintf("%s/stacks/%d", mainAPI, stack.Number)
		stack = DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPatch, stackAPI, &api.EditPullRequestStackOption{
			Revision: stack.Revision, PullRequestRefs: []api.PullRequestReference{{RepositoryID: fork.ID, PullRequest: upper.Index}},
		}).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
		require.Len(t, stack.Entries, 2)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPatch, stackAPI, &api.EditPullRequestStackOption{
			Revision: stack.Revision, PullRequestRefs: []api.PullRequestReference{{RepositoryID: fork.ID, PullRequest: foreign.Index}},
		}).AddTokenAuth(token), http.StatusForbidden)
		heads := make([]api.PullRequestStackHead, len(stack.Entries))
		for i, entry := range stack.Entries {
			heads[i] = api.PullRequestStackHead{RepositoryID: entry.PullRequest.Base.Repository.ID, PullRequest: entry.PullRequest.Index, HeadSHA: entry.HeadSHA, ParentSHA: entry.ParentSHA}
		}
		stack = DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, stackAPI+"/sync", &api.SynchronizePullRequestStackOption{
			Revision: stack.Revision, Heads: heads,
		}).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
		operation := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, stackAPI+"/rebase", &api.PullRequestStackOperationOption{Revision: stack.Revision}).AddTokenAuth(token), http.StatusAccepted), &api.PullRequestStackOperation{})
		completed := waitForStackOperation(t, operation.Number, "completed", "blocked")
		require.Equal(t, "completed", completed.State, completed.LastError)
		stack = DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, stackAPI).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, stackAPI+"/land", &api.PullRequestStackOperationOption{Revision: stack.Revision, MergeStyle: "merge"}).AddTokenAuth(token), http.StatusForbidden)
		for _, pull := range []*api.PullRequest{lower, upper} {
			stored := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pull.ID})
			assert.False(t, stored.HasMerged)
		}
		trunk, err := git.GetFullCommitID(t.Context(), main, "refs/heads/main")
		require.NoError(t, err)
		assert.Equal(t, originalTrunk, trunk)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodDelete, stackAPI, &api.PullRequestStackRevisionOption{Revision: stack.Revision}).AddTokenAuth(token), http.StatusNoContent)
	})
}
