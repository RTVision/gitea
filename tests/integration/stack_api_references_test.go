// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/require"
)

func TestStackAPIQualifiedReferencesRejectInvalidInputs(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		session := loginUser(t, "user2")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
		private := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 13})
		require.True(t, private.IsPrivate)
		require.NoError(t, private.LoadOwner(t.Context()))
		pull := &issues_model.PullRequest{HeadRepoID: private.ID, BaseRepoID: private.ID, HeadBranch: "private-head", BaseBranch: "private-base"}
		require.NoError(t, issues_model.NewPullRequest(t.Context(), private, &issues_model.Issue{RepoID: private.ID, PosterID: private.OwnerID, Poster: private.Owner, Title: "Private pull request"}, nil, nil, pull))
		cases := []struct {
			name   string
			option api.CreatePullRequestStackOption
			status int
		}{
			{"empty", api.CreatePullRequestStackOption{Trunk: "master"}, http.StatusUnprocessableEntity},
			{"mixed forms", api.CreatePullRequestStackOption{Trunk: "master", PullRequests: []int64{3}, PullRequestRefs: []api.PullRequestReference{{RepositoryID: 1, PullRequest: 3}}}, http.StatusUnprocessableEntity},
			{"negative repository", api.CreatePullRequestStackOption{Trunk: "master", PullRequestRefs: []api.PullRequestReference{{RepositoryID: -1, PullRequest: 3}}}, http.StatusUnprocessableEntity},
			{"unreadable repository", api.CreatePullRequestStackOption{Trunk: "master", PullRequestRefs: []api.PullRequestReference{{RepositoryID: private.ID, PullRequest: pull.Index}}}, http.StatusNotFound},
			{"duplicate reference", api.CreatePullRequestStackOption{Trunk: "master", PullRequestRefs: []api.PullRequestReference{{RepositoryID: 1, PullRequest: 3}, {RepositoryID: 1, PullRequest: 3}}}, http.StatusUnprocessableEntity},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/user2/repo1/stacks", &tc.option).AddTokenAuth(token), tc.status)
			})
		}
		count, err := db.GetEngine(t.Context()).Count(new(issues_model.PullRequestStack))
		require.NoError(t, err)
		require.Zero(t, count)
	})
}
