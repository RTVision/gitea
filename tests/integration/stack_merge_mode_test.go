// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeStackMergeMode(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		ctx := t.Context()
		session := loginUser(t, "user2")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		const base = "/api/v1/repos/user2/repo1"
		repoUnit := unittest.AssertExistsAndLoadBean(t, &repo_model.RepoUnit{RepoID: repo.ID, Type: unit.TypePullRequests})
		repoUnit.PullRequestsConfig().AllowFastForwardOnly = true
		require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repoUnit))
		_, err := db.GetEngine(ctx).Insert(&git_model.ProtectedBranch{RepoID: repo.ID, RuleName: "stack-*", CanPush: true})
		require.NoError(t, err)
		head := func(ref string) string {
			sha, err := git.GetFullCommitID(ctx, repo, git.BranchPrefix+ref)
			require.NoError(t, err)
			return sha
		}
		ancestor := func(older, newer string) bool {
			return gitcmd.NewCommand("merge-base", "--is-ancestor").AddDynamicArguments(older, newer).WithRepo(repo).Run(ctx) == nil
		}
		gitOutput := func(cmd *gitcmd.Command) string {
			out, _, err := cmd.WithRepo(repo).RunStdString(ctx)
			require.NoError(t, err)
			return strings.TrimSpace(out)
		}
		advanceTrunk := func(name string) {
			testCreateFileInBranch(t, owner, repo, createFileInBranchOptions{OldBranch: "release"}, map[string]string{name: name + "\n"})
		}
		branches := []string{"stack-lower", "stack-middle", "stack-upper"}
		pulls := make([]int64, 0, len(branches))
		updateLayers := func() {
			for _, index := range pulls {
				MakeRequest(t, NewRequest(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/update?style=merge", base, index)).AddTokenAuth(token), http.StatusOK)
			}
		}
		path := ""
		runOperation := func(action string, option *api.PullRequestStackOperationOption) {
			stack := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, path).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
			option.Revision = stack.Revision
			op := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/"+action, option).AddTokenAuth(token), http.StatusAccepted), &api.PullRequestStackOperation{})
			settled := waitForStackOperation(t, op.Number, "completed", "blocked")
			require.Equal(t, "completed", settled.State, settled.LastError)
		}

		testCreateBranch(t, session, "user2", "repo1", "branch/master", "release", http.StatusSeeOther)
		parent, readme := "release", ""
		for _, branch := range branches {
			readme += branch + "\n"
			testEditFileToNewBranch(t, session, "user2", "repo1", parent, branch, "README.md", readme)
			pull := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/pulls", &api.CreatePullRequestOption{Head: branch, Base: parent, Title: branch}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
			pulls = append(pulls, pull.Index)
			parent = branch
		}
		advanceTrunk("trunk-1.txt")
		updateLayers()

		create := &api.CreatePullRequestStackOption{Trunk: "release", PullRequests: pulls}
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusUnprocessableEntity)
		create.Mode = api.StackModeMerge
		stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
		assert.Equal(t, api.StackModeMerge, stack.Mode)
		assert.Equal(t, api.StackModeMerge, stack.Entries[1].PullRequest.Stack.Mode)
		capabilities := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, base+"/stacks/capabilities").AddTokenAuth(token), http.StatusOK), &api.PullRequestStackCapabilities{})
		assert.Equal(t, []api.StackMode{api.StackModeRebase, api.StackModeMerge}, capabilities.Modes)
		assert.Contains(t, capabilities.Operations, "update")
		assert.Equal(t, []string{"merge", "squash", "fast-forward-only"}, capabilities.ModeMergeStyles["merge"])
		path = fmt.Sprintf("%s/stacks/%d", base, stack.Number)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/rebase", &api.PullRequestStackOperationOption{Revision: 1}).AddTokenAuth(token), http.StatusUnprocessableEntity)
		MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/land", &api.PullRequestStackOperationOption{Revision: 1, MergeStyle: string(repo_model.MergeStyleRebase)}).AddTokenAuth(token), http.StatusUnprocessableEntity)

		advanceTrunk("trunk-2.txt")
		MakeRequest(t, NewRequest(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/update?style=rebase", base, pulls[0])).AddTokenAuth(token), http.StatusForbidden) // force-push is disabled on the layers
		updateLayers()

		advanceTrunk("trunk-3.txt")
		before := make([]string, 0, len(branches))
		for _, branch := range branches {
			before = append(before, head(branch))
		}
		runOperation("update", &api.PullRequestStackOperationOption{})
		for i, branch := range branches {
			assert.True(t, ancestor(before[i], head(branch)), "%s only moves forward", branch)
			assert.True(t, ancestor(head("release"), head(branch)), "%s contains the trunk", branch)
		}

		trunk := head("release")
		runOperation("land", &api.PullRequestStackOperationOption{ThroughPosition: 1, MergeStyle: string(repo_model.MergeStyleSquash)})
		assert.Equal(t, trunk, gitOutput(gitcmd.NewCommand("rev-parse", "release^@")), "the squash is a single commit on the trunk")
		assert.True(t, ancestor(head("release"), head("stack-middle")), "the layer above absorbs the squash")
		assert.Equal(t, "README.md", gitOutput(gitcmd.NewCommand("diff", "--name-only", "release", "stack-middle")))

		trunk = head("release")
		middle := head("stack-middle")
		runOperation("land", &api.PullRequestStackOperationOption{ThroughPosition: 2, MergeStyle: string(repo_model.MergeStyleMerge)})
		assert.Equal(t, trunk+"\n"+middle, gitOutput(gitcmd.NewCommand("rev-parse", "release^@")))
		assert.True(t, ancestor(head("release"), head("stack-upper")), "the layer above gets the merge")

		runOperation("land", &api.PullRequestStackOperationOption{ThroughPosition: 3, MergeStyle: string(repo_model.MergeStyleFastForwardOnly)})
		assert.Equal(t, head("stack-upper"), head("release"))
		landed := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, path).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
		assert.Equal(t, "complete", landed.State)
		assert.Equal(t, readme, gitOutput(gitcmd.NewCommand("show", "release:README.md"))+"\n")
	})
}
