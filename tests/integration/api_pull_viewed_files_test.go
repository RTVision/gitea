// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"testing"

	auth_model "gitea.dev/models/auth"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	"gitea.dev/models/unittest"
	api "gitea.dev/modules/structs"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIPullViewedFiles(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	require.NoError(t, issue.LoadAttributes(t.Context()))
	session := loginUser(t, "user2")
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
	url := fmt.Sprintf("/api/v1/repos/%s/pulls/%d/viewed-files", issue.Repo.FullName(), issue.Index)
	read := func() api.PullRequestViewedFiles {
		return DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, url).AddTokenAuth(token), http.StatusOK), api.PullRequestViewedFiles{})
	}
	initial := read()
	require.NotEmpty(t, initial.HeadSHA)
	require.NotEmpty(t, initial.Files)
	path := initial.Files[0].Path
	require.NoError(t, issue.LoadPullRequest(t.Context()))
	_, err := pull_model.UpdateReviewState(t.Context(), 2, issue.PullRequest.ID, initial.HeadSHA, map[string]pull_model.ViewedState{path: pull_model.Viewed})
	require.NoError(t, err)
	assert.True(t, read().Files[0].Viewed)
	for _, viewed := range []bool{false, true} {
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPut, url, api.UpdatePullRequestViewedFilesOptions{HeadSHA: initial.HeadSHA, Files: map[string]bool{path: viewed}}).AddTokenAuth(token), http.StatusNoContent)
		assert.Equal(t, viewed, read().Files[0].Viewed)
		stored, _, err := pull_model.GetReviewState(t.Context(), 2, issue.PullRequest.ID, initial.HeadSHA)
		require.NoError(t, err)
		assert.Equal(t, viewed, stored.UpdatedFiles[path] == pull_model.Viewed)
	}
	otherToken := getTokenForLoggedInUser(t, loginUser(t, "user1"), auth_model.AccessTokenScopeReadRepository)
	other := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, url).AddTokenAuth(otherToken), http.StatusOK), api.PullRequestViewedFiles{})
	assert.False(t, other.Files[0].Viewed)
	MakeRequest(t, NewRequest(t, http.MethodGet, url), http.StatusUnauthorized)
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPut, url, api.UpdatePullRequestViewedFilesOptions{HeadSHA: "outdated", Files: map[string]bool{path: false}}).AddTokenAuth(token), http.StatusConflict)
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPut, url, api.UpdatePullRequestViewedFilesOptions{HeadSHA: initial.HeadSHA, Files: map[string]bool{"missing-file": true}}).AddTokenAuth(token), http.StatusUnprocessableEntity)
	assert.True(t, read().Files[0].Viewed)
}

func TestAPIPullRequestChangesWithInlineComments(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	require.NoError(t, issue.LoadAttributes(t.Context()))
	token := getTokenForLoggedInUser(t, loginUser(t, "user2"), auth_model.AccessTokenScopeWriteRepository)
	url := fmt.Sprintf("/api/v1/repos/%s/pulls/%d/reviews", issue.Repo.FullName(), issue.Index)
	options := api.CreatePullReviewOptions{Event: "REQUEST_CHANGES", Comments: []api.CreatePullReviewComment{{Path: "README.md", Body: "Please adjust this line.", NewLineNum: 1}}}
	review := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, url, options).AddTokenAuth(token), http.StatusOK), api.PullReview{})
	assert.EqualValues(t, "REQUEST_CHANGES", review.State)
	assert.Empty(t, review.Body)
	assert.Equal(t, 1, review.CodeCommentsCount)
	options.Comments = nil
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, url, options).AddTokenAuth(token), http.StatusUnprocessableEntity)
}
