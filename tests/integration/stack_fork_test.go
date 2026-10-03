// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	auth_model "gitea.dev/models/auth"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"
	pull_service "gitea.dev/services/pull"
	repo_service "gitea.dev/services/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeForkStackLanding(t *testing.T) {
	for _, mode := range []string{issues_model.StackModeRebase, issues_model.StackModeMerge} {
		t.Run(mode, func(t *testing.T) {
			onGiteaRun(t, func(t *testing.T, _ *url.URL) {
				defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
				actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
				main, err := repo_service.CreateRepository(t.Context(), actor, actor, repo_service.CreateRepoOptions{
					Name: "fork-stack-main", DefaultBranch: "main", AutoInit: true, Readme: "Default",
				})
				require.NoError(t, err)
				fork, err := repo_service.ForkRepository(t.Context(), actor, actor, repo_service.ForkRepoOptions{BaseRepo: main, Name: "fork-stack-source"})
				require.NoError(t, err)
				originalTrunk, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/main")
				require.NoError(t, err)
				testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "main", NewBranch: "lower"}, map[string]string{"lower.txt": "lower\n"})
				testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "lower", NewBranch: "upper"}, map[string]string{"upper.txt": "upper\n"})
				session := loginUser(t, actor.Name)
				token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteIssue)
				mainAPI := "/api/v1/repos/" + main.FullName()
				forkAPI := "/api/v1/repos/" + fork.FullName()
				lower := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, mainAPI+"/pulls", &api.CreatePullRequestOption{
					Head: fork.FullName() + ":lower", Base: "main", Title: "Lower layer",
				}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
				upper := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, forkAPI+"/pulls", &api.CreatePullRequestOption{
					Head: "upper", Base: "lower", Title: "Upper layer",
				}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
				require.EqualValues(t, 1, lower.Index)
				require.Equal(t, lower.Index, upper.Index)
				upperPath := fmt.Sprintf("%s/pulls/%d", forkAPI, upper.Index)
				upperModel := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				originalIssueID, originalURL := upperModel.IssueID, upper.HTMLURL
				comment := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", forkAPI, upper.Index), &api.CreateIssueCommentOption{Body: "Keep this discussion"}).AddTokenAuth(token), http.StatusCreated), &api.Comment{})
				require.NoError(t, queue.GetManager().FlushAll(t.Context(), 10*time.Second))
				stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, mainAPI+"/stacks", &api.CreatePullRequestStackOption{
					Trunk: "main", Mode: api.StackMode(mode), PullRequestRefs: []api.PullRequestReference{
						{RepositoryID: main.ID, PullRequest: lower.Index},
						{RepositoryID: fork.ID, PullRequest: upper.Index},
					},
				}).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
				require.Len(t, stack.Entries, 2)
				assert.Equal(t, fork.ID, stack.Entries[1].PullRequest.Base.Repository.ID)
				assert.Equal(t, main.ID, stack.Repository.ID)
				checkDiff := func() {
					t.Helper()
					body := session.MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/%s/pulls/%d.diff", fork.FullName(), upper.Index)), http.StatusOK).Body.String()
					assert.Contains(t, body, "diff --git a/upper.txt b/upper.txt")
					assert.NotContains(t, body, "diff --git a/lower.txt b/lower.txt")
				}
				checkDiff()
				land := func(position int, style repo_model.MergeStyle) {
					t.Helper()
					current, err := issues_model.GetStackByID(t.Context(), stack.Number)
					require.NoError(t, err)
					op, err := pull_service.StartStackOperation(t.Context(), actor, pull_service.StackOperationOptions{
						StackID: current.ID, ExpectedRevision: current.Revision, Kind: "land", ThroughPosition: position, MergeStyle: style,
					})
					require.NoError(t, err)
					op = waitForStackOperation(t, op.ID, "completed", "blocked")
					require.Equal(t, "completed", op.State, op.LastError)
				}
				land(1, repo_model.MergeStyleSquash)
				upperModel = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				assert.False(t, upperModel.HasMerged)
				assert.Equal(t, fork.ID, upperModel.BaseRepoID)
				assert.Equal(t, "lower", upperModel.BaseBranch)
				checkDiff()
				list := session.MakeRequest(t, NewRequest(t, http.MethodGet, "/"+main.FullName()+"/pulls"), http.StatusOK).Body.String()
				assert.Contains(t, list, fmt.Sprintf(`href="/%s/pulls/%d"`, fork.FullName(), upper.Index))
				land(2, repo_model.MergeStyleMerge)
				upperModel = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				assert.True(t, upperModel.HasMerged)
				assert.Equal(t, fork.ID, upperModel.BaseRepoID)
				assert.Equal(t, "lower", upperModel.BaseBranch)
				assert.Equal(t, originalIssueID, upperModel.IssueID)
				assert.Equal(t, main.ID, upperModel.MergedRepoID)
				assert.Equal(t, "main", upperModel.MergedBranch)
				checkDiff()
				updated := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, upperPath).AddTokenAuth(token), http.StatusOK), &api.PullRequest{})
				assert.Equal(t, originalURL, updated.HTMLURL)
				assert.Equal(t, main.ID, updated.MergedRepoID)
				assert.Equal(t, "main", updated.MergedBranch)
				savedComment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: comment.ID})
				assert.Equal(t, originalIssueID, savedComment.IssueID)
				assert.Equal(t, "Keep this discussion", savedComment.Content)
				trunk, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/main")
				require.NoError(t, err)
				assert.Equal(t, originalTrunk, trunk)
				for _, name := range []string{"lower", "upper"} {
					content, _, err := gitcmd.NewCommand("show").AddDynamicArguments("refs/heads/main:" + name + ".txt").WithRepo(main).RunStdString(t.Context())
					require.NoError(t, err)
					assert.Equal(t, name+"\n", content)
				}
				body := session.MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/%s/pulls/%d", fork.FullName(), upper.Index)), http.StatusOK).Body.String()
				assert.Contains(t, body, main.Link()+"/commit/"+upperModel.MergedCommitID)
			})
		})
	}
}
