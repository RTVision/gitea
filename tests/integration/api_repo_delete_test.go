// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"strconv"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/organization"
	"gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	repo_service "gitea.dev/services/repository"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIRepositoryDelete(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	t.Run("AdminNoPermToDeleteRepo", func(t *testing.T) {
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		org := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 3})
		doer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 28})
		teams, err := organization.GetUserOrgTeams(t.Context(), org.ID, doer.ID)
		require.NoError(t, err)
		require.Len(t, teams, 1)

		team := teams[0]
		assert.Equal(t, perm.AccessModeAdmin, team.AccessMode)
		assert.True(t, team.CanCreateOrgRepo)
		token := getUserToken(t, doer.Name, auth_model.AccessTokenScopeWriteRepository)

		unrelatedRepo, err := repo_service.CreateRepository(t.Context(), owner, org, repo_service.CreateRepoOptions{Name: "unrelated-admin-team"})
		require.NoError(t, err)

		targetRepo, err := repo_service.CreateRepository(t.Context(), owner, org, repo_service.CreateRepoOptions{Name: "target-admin-team"})
		require.NoError(t, err)
		require.NoError(t, repo_service.TeamAddRepository(t.Context(), team, targetRepo))

		req := NewRequest(t, "DELETE", "/api/v1/repos/"+unrelatedRepo.FullName()).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusForbidden)
		unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: unrelatedRepo.ID})

		req = NewRequest(t, "DELETE", "/api/v1/repos/"+targetRepo.FullName()).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusNoContent)
		unittest.AssertNotExistsBean(t, &repo_model.Repository{ID: targetRepo.ID})
	})

	t.Run("ActiveStack", func(t *testing.T) {
		main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 10})
		fork := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 11})
		pull := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 3, BaseRepoID: main.ID, HeadRepoID: fork.ID})
		stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
		require.NoError(t, db.Insert(t.Context(), stack))
		entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pull.ID, Position: 1}
		require.NoError(t, db.Insert(t.Context(), entry))
		op := &issues_model.StackOperation{StackID: stack.ID, ActorID: 1, ExpectedRevision: stack.Revision, Kind: "land", State: "queued"}
		require.NoError(t, issues_model.CreateStackOperation(t.Context(), op))
		session := loginUser(t, "user1")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
		message := "Finish or cancel the stack operation before deleting this repository."
		for _, target := range []*repo_model.Repository{main, fork} {
			response := MakeRequest(t, NewRequest(t, http.MethodDelete, "/api/v1/repos/"+target.FullName()).AddTokenAuth(token), http.StatusConflict)
			assert.Equal(t, message, (*DecodeJSON(t, response, &map[string]string{}))["message"])
			for _, req := range []*RequestWrapper{
				NewRequestWithValues(t, http.MethodPost, "/"+target.FullName()+"/settings", map[string]string{"action": "delete", "repo_name": target.FullName()}),
				NewRequestWithValues(t, http.MethodPost, "/-/admin/repos/delete", map[string]string{"id": strconv.FormatInt(target.ID, 10)}),
			} {
				response := session.MakeRequest(t, req, http.StatusConflict)
				body := DecodeJSON(t, response, &map[string]string{})
				assert.Equal(t, message, (*body)["errorMessage"])
				assert.Equal(t, "text", (*body)["renderFormat"])
			}
			for _, repo := range []*repo_model.Repository{main, fork} {
				unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repo.ID})
			}
			unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID, ActiveOperationID: op.ID, Revision: stack.Revision})
			unittest.AssertExistsAndLoadBean(t, &issues_model.StackEntry{ID: entry.ID})
			unittest.AssertExistsAndLoadBean(t, &issues_model.StackOperation{ID: op.ID, State: "queued"})
			assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: pull.IssueID}).IsClosed, "blocked deletion keeps the fork pull request open")
		}
	})
}
