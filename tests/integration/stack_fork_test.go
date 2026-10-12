// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	activities_model "gitea.dev/models/activities"
	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
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
				assert.Equal(t, main.ID, stack.Repository.ID)
				assert.Equal(t, main.ID, stack.Entries[1].PullRequest.Base.Repository.ID)
				assert.Equal(t, "stack/pr-1", stack.Entries[1].PullRequest.Base.Ref)

				// the fork layer now lives in the main repository and keeps its discussion
				upperModel = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				assert.Equal(t, originalIssueID, upperModel.IssueID)
				assert.Equal(t, main.ID, upperModel.BaseRepoID)
				assert.Equal(t, fork.ID, upperModel.HeadRepoID)
				assert.EqualValues(t, 2, upperModel.Index)
				moved := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: originalIssueID})
				assert.Equal(t, main.ID, moved.RepoID)
				assert.Equal(t, originalIssueID, unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: comment.ID}).IssueID)
				assert.Equal(t, 2, unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: main.ID}).NumPulls)
				assert.Equal(t, 0, unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: fork.ID}).NumPulls)
				unittest.AssertExistsAndLoadBean(t, &activities_model.Action{RepoID: main.ID, OpType: activities_model.ActionCreatePullRequest, Content: "2|Upper layer"})
				_, err = git.GetFullCommitID(t.Context(), fork, "refs/pull/1/head")
				assert.Error(t, err)

				oldLink := fmt.Sprintf("/%s/pulls/%d", fork.FullName(), upper.Index)
				newLink := fmt.Sprintf("/%s/pulls/2", main.FullName())
				resp := session.MakeRequest(t, NewRequest(t, http.MethodGet, oldLink+"/files?style=split"), http.StatusMovedPermanently)
				assert.Equal(t, newLink+"/files?style=split", resp.Header().Get("Location"))
				resp = MakeRequest(t, NewRequest(t, http.MethodGet, upperPath).AddTokenAuth(token), http.StatusMovedPermanently)
				assert.Equal(t, setting.AppURL+"api/v1/repos/"+main.FullName()+"/pulls/2", resp.Header().Get("Location"))

				mirrorHead := func() string {
					t.Helper()
					head, err := git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-1")
					require.NoError(t, err)
					return head
				}
				lowerHead, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/lower")
				require.NoError(t, err)
				assert.Equal(t, lowerHead, mirrorHead())
				testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "lower"}, map[string]string{"lower-fix.txt": "fix\n"})
				require.NoError(t, queue.GetManager().FlushAll(t.Context(), 10*time.Second))
				lowerHead, err = git.GetFullCommitID(t.Context(), fork, "refs/heads/lower")
				require.NoError(t, err)
				assert.Equal(t, lowerHead, mirrorHead())
				_, err = createFileInBranch(actor, main, createFileInBranchOptions{OldBranch: "stack/pr-1"}, map[string]string{"bypass.txt": "no\n"})
				assert.ErrorContains(t, err, "mirrors a fork layer")

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
				assert.Equal(t, main.ID, upperModel.BaseRepoID)
				assert.Equal(t, "main", upperModel.BaseBranch)
				exist, err := git_model.IsBranchExist(t.Context(), main.ID, "stack/pr-1")
				require.NoError(t, err)
				assert.False(t, exist)
				list := session.MakeRequest(t, NewRequest(t, http.MethodGet, "/"+main.FullName()+"/pulls"), http.StatusOK).Body.String()
				assert.Contains(t, list, fmt.Sprintf(`href="%s"`, newLink))

				land(2, repo_model.MergeStyleMerge)
				upperModel = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
				assert.True(t, upperModel.HasMerged)
				assert.Equal(t, originalIssueID, upperModel.IssueID)
				for _, name := range []string{"lower", "lower-fix", "upper"} {
					content, _, err := gitcmd.NewCommand("show").AddDynamicArguments("refs/heads/main:" + name + ".txt").WithRepo(main).RunStdString(t.Context())
					require.NoError(t, err)
					assert.NotEmpty(t, content)
				}
				trunk, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/main")
				require.NoError(t, err)
				assert.Equal(t, originalTrunk, trunk)
				body := session.MakeRequest(t, NewRequest(t, http.MethodGet, newLink), http.StatusOK).Body.String()
				assert.Contains(t, body, main.Link()+"/commit/"+upperModel.MergedCommitID)
				assert.NotEqual(t, originalURL, setting.AppURL+newLink[1:])
			})
		})
	}
}

func TestMoveLegacyForkStackLayers(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		main, err := repo_service.CreateRepository(t.Context(), actor, actor, repo_service.CreateRepoOptions{
			Name: "legacy-stack-main", DefaultBranch: "main", AutoInit: true, Readme: "Default",
		})
		require.NoError(t, err)
		fork, err := repo_service.ForkRepository(t.Context(), actor, actor, repo_service.ForkRepoOptions{BaseRepo: main, Name: "legacy-stack-source"})
		require.NoError(t, err)
		testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "main", NewBranch: "lower"}, map[string]string{"lower.txt": "lower\n"})
		testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "lower", NewBranch: "upper"}, map[string]string{"upper.txt": "upper\n"})
		token := getTokenForLoggedInUser(t, loginUser(t, actor.Name), auth_model.AccessTokenScopeWriteRepository)
		lower := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/"+main.FullName()+"/pulls", &api.CreatePullRequestOption{
			Head: fork.FullName() + ":lower", Base: "main", Title: "Lower layer",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		upper := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/"+fork.FullName()+"/pulls", &api.CreatePullRequestOption{
			Head: "upper", Base: "lower", Title: "Upper layer",
		}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})

		// a stack recorded before adoption moved fork layers
		stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "main", Mode: issues_model.StackModeMerge, State: issues_model.StackStateOpen, Revision: 1, CreatedByID: actor.ID}
		require.NoError(t, db.Insert(t.Context(), stack))
		for i, pull := range []*api.PullRequest{lower, upper} {
			require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pull.ID, Position: i + 1}))
			require.NoError(t, db.Insert(t.Context(), &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pull.ID, BranchKey: issues_model.StackBranchKey(fork.ID, pull.Head.Ref)}))
		}

		require.NoError(t, pull_service.MoveForkStackLayers(t.Context()))
		moved := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID})
		assert.Equal(t, main.ID, moved.BaseRepoID)
		assert.Equal(t, "stack/pr-1", moved.BaseBranch)
		assert.EqualValues(t, 2, moved.Index)
		assert.EqualValues(t, 2, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID}).Revision)
		lowerHead, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/lower")
		require.NoError(t, err)
		mirror, err := git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-1")
		require.NoError(t, err)
		assert.Equal(t, lowerHead, mirror)
		redirect := unittest.AssertExistsAndLoadBean(t, &issues_model.IssueRedirect{IssueID: moved.IssueID})
		assert.False(t, redirect.Pending)

		// a move interrupted before its new repository was notified is finished by the next run
		_, err = db.GetEngine(t.Context()).ID(redirect.ID).Cols("pending").Update(&issues_model.IssueRedirect{Pending: true})
		require.NoError(t, err)
		require.NoError(t, pull_service.MoveForkStackLayers(t.Context()))
		assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.IssueRedirect{ID: redirect.ID}).Pending)
		assert.EqualValues(t, 2, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequestStack{ID: stack.ID}).Revision)
	})
}

func TestForkStackMirrorChain(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		main, err := repo_service.CreateRepository(t.Context(), actor, actor, repo_service.CreateRepoOptions{
			Name: "mirror-chain-main", DefaultBranch: "main", AutoInit: true, Readme: "Default",
		})
		require.NoError(t, err)
		fork, err := repo_service.ForkRepository(t.Context(), actor, actor, repo_service.ForkRepoOptions{BaseRepo: main, Name: "mirror-chain-source"})
		require.NoError(t, err)
		for _, branch := range [][2]string{{"main", "lower"}, {"lower", "mid"}, {"mid", "top"}, {"lower", "inserted"}} {
			testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: branch[0], NewBranch: branch[1]}, map[string]string{branch[1] + ".txt": branch[1] + "\n"})
		}
		token := getTokenForLoggedInUser(t, loginUser(t, actor.Name), auth_model.AccessTokenScopeWriteRepository)
		openPull := func(repo *repo_model.Repository, head, base string) *api.PullRequest {
			t.Helper()
			return DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/"+repo.FullName()+"/pulls", &api.CreatePullRequestOption{
				Head: head, Base: base, Title: head,
			}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
		}
		lower := openPull(main, fork.FullName()+":lower", "main")
		mid := openPull(fork, "mid", "lower")
		top := openPull(fork, "top", "mid")
		create := &api.CreatePullRequestStackOption{Trunk: "main", Mode: api.StackModeMerge, PullRequestRefs: []api.PullRequestReference{
			{RepositoryID: main.ID, PullRequest: lower.Index}, {RepositoryID: fork.ID, PullRequest: mid.Index}, {RepositoryID: fork.ID, PullRequest: top.Index},
		}}

		// a branch the stack does not own is never overwritten, and mirrors written before the failure are undone
		testCreateFileInBranch(t, actor, main, createFileInBranchOptions{OldBranch: "main", NewBranch: "stack/pr-2"}, map[string]string{"other.txt": "other\n"})
		taken, err := git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-2")
		require.NoError(t, err)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/"+main.FullName()+"/stacks", create).AddTokenAuth(token), http.StatusUnprocessableEntity)
		kept, err := git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-2")
		require.NoError(t, err)
		assert.Equal(t, taken, kept)
		_, err = git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-1")
		assert.Error(t, err)
		assert.Equal(t, fork.ID, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: mid.ID}).BaseRepoID)
		gitRepo, err := git.OpenRepository(t.Context(), main)
		require.NoError(t, err)
		require.NoError(t, repo_service.DeleteBranch(t.Context(), actor, main, gitRepo, "stack/pr-2"))
		gitRepo.Close()

		stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, "/api/v1/repos/"+main.FullName()+"/stacks", create).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
		base := func(pull *api.PullRequest) string {
			t.Helper()
			pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pull.ID})
			assert.Equal(t, main.ID, pr.BaseRepoID)
			return pr.BaseBranch
		}
		assert.Equal(t, "stack/pr-2", base(top))
		assert.Equal(t, "stack/pr-1", base(mid))

		inserted := openPull(fork, "inserted", "lower")
		current, err := issues_model.GetStackByID(t.Context(), stack.Number)
		require.NoError(t, err)
		_, err = pull_service.InsertStackLayer(t.Context(), actor, current.ID, current.Revision, inserted.ID)
		require.NoError(t, err)
		assert.Equal(t, "stack/pr-1", base(inserted))
		assert.Equal(t, "stack/pr-4", base(mid))
		assert.Equal(t, "stack/pr-2", base(top))
		insertedHead, err := git.GetFullCommitID(t.Context(), fork, "refs/heads/inserted")
		require.NoError(t, err)
		mirror, err := git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-4")
		require.NoError(t, err)
		assert.Equal(t, insertedHead, mirror)

		// a mirror changed outside the stack, as after it was released, is never overwritten by a later sync
		outside, err := git.GetFullCommitID(t.Context(), main, "refs/heads/main")
		require.NoError(t, err)
		require.NoError(t, git.UpdateRef(t.Context(), main, "refs/heads/stack/pr-2", outside))
		testCreateFileInBranch(t, actor, fork, createFileInBranchOptions{OldBranch: "mid"}, map[string]string{"mid-fix.txt": "fix\n"})
		require.NoError(t, queue.GetManager().FlushAll(t.Context(), 10*time.Second))
		kept, err = git.GetFullCommitID(t.Context(), main, "refs/heads/stack/pr-2")
		require.NoError(t, err)
		assert.Equal(t, outside, kept)
	})
}
