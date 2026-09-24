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
	for _, style := range []repo_model.MergeStyle{repo_model.MergeStyleMerge, repo_model.MergeStyleSquash, repo_model.MergeStyleFastForwardOnly} {
		t.Run(string(style), func(t *testing.T) {
			onGiteaRun(t, func(t *testing.T, _ *url.URL) {
				testNativeStackMergeMode(t, style)
			})
		})
	}
}

func testNativeStackMergeMode(t *testing.T, style repo_model.MergeStyle) {
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
	advanceTrunk := func(name string) {
		testCreateFileInBranch(t, owner, repo, createFileInBranchOptions{OldBranch: "release"}, map[string]string{name: name + "\n"})
	}
	updateBranch := func(index int64, style string, status int) {
		MakeRequest(t, NewRequest(t, http.MethodPost, fmt.Sprintf("%s/pulls/%d/update?style=%s", base, index, style)).AddTokenAuth(token), status)
	}

	testCreateBranch(t, session, "user2", "repo1", "branch/master", "release", http.StatusSeeOther)
	testEditFileToNewBranch(t, session, "user2", "repo1", "release", "stack-lower", "README.md", "lower layer\n")
	testEditFileToNewBranch(t, session, "user2", "repo1", "stack-lower", "stack-upper", "README.md", "lower layer\nupper layer\n")
	lower := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/pulls", &api.CreatePullRequestOption{Head: "stack-lower", Base: "release", Title: "lower"}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
	upper := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/pulls", &api.CreatePullRequestOption{Head: "stack-upper", Base: "stack-lower", Title: "upper"}).AddTokenAuth(token), http.StatusCreated), &api.PullRequest{})
	advanceTrunk("trunk-1.txt")
	updateBranch(lower.Index, "merge", http.StatusOK)
	updateBranch(upper.Index, "merge", http.StatusOK)

	create := &api.CreatePullRequestStackOption{Trunk: "release", PullRequests: []int64{lower.Index, upper.Index}}
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusUnprocessableEntity)
	create.Mode = api.StackModeMerge
	stack := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, base+"/stacks", create).AddTokenAuth(token), http.StatusCreated), &api.PullRequestStack{})
	assert.Equal(t, api.StackModeMerge, stack.Mode)
	assert.Equal(t, api.StackModeMerge, stack.Entries[1].PullRequest.Stack.Mode)
	capabilities := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, base+"/stacks/capabilities").AddTokenAuth(token), http.StatusOK), &api.PullRequestStackCapabilities{})
	assert.Equal(t, []string{"rebase", "merge"}, capabilities.Modes)
	assert.Contains(t, capabilities.Operations, "update")
	assert.Equal(t, []string{"merge", "squash", "fast-forward-only"}, capabilities.ModeMergeStyles["merge"])
	path := fmt.Sprintf("%s/stacks/%d", base, stack.Number)
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/rebase", &api.PullRequestStackOperationOption{Revision: 1}).AddTokenAuth(token), http.StatusUnprocessableEntity)
	MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/land", &api.PullRequestStackOperationOption{Revision: 1, MergeStyle: string(repo_model.MergeStyleRebase)}).AddTokenAuth(token), http.StatusUnprocessableEntity)

	advanceTrunk("trunk-2.txt")
	updateBranch(lower.Index, "rebase", http.StatusForbidden) // force-push is disabled on the layers
	updateBranch(lower.Index, "merge", http.StatusOK)
	updateBranch(upper.Index, "merge", http.StatusOK)

	advanceTrunk("trunk-3.txt")
	lowerHead, upperHead := head("stack-lower"), head("stack-upper")
	op := DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/update", &api.PullRequestStackOperationOption{Revision: 1}).AddTokenAuth(token), http.StatusAccepted), &api.PullRequestStackOperation{})
	settled := waitForStackOperation(t, op.Number, "completed", "blocked")
	require.Equal(t, "completed", settled.State, settled.LastError)
	assert.True(t, ancestor(lowerHead, head("stack-lower")) && ancestor(head("release"), head("stack-lower")))
	assert.True(t, ancestor(upperHead, head("stack-upper")) && ancestor(head("stack-lower"), head("stack-upper")))

	trunk, upperHead := head("release"), head("stack-upper")
	op = DecodeJSON(t, MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, path+"/land", &api.PullRequestStackOperationOption{Revision: 2, ThroughPosition: 2, MergeStyle: string(style)}).AddTokenAuth(token), http.StatusAccepted), &api.PullRequestStackOperation{})
	settled = waitForStackOperation(t, op.Number, "completed", "blocked")
	require.Equal(t, "completed", settled.State, settled.LastError)
	assert.Equal(t, 2, settled.Completed)
	landed := DecodeJSON(t, MakeRequest(t, NewRequest(t, http.MethodGet, path).AddTokenAuth(token), http.StatusOK), &api.PullRequestStack{})
	assert.Equal(t, "complete", landed.State)
	assert.True(t, ancestor(upperHead, head("stack-upper")), "layers only move forward")
	content, _, err := gitcmd.NewCommand("show").AddDynamicArguments("refs/heads/release:README.md").WithRepo(repo).RunStdString(ctx)
	require.NoError(t, err)
	assert.Equal(t, "lower layer\nupper layer\n", content)
	landedCommits, _, err := gitcmd.NewCommand("rev-list", "--first-parent").AddDynamicArguments(trunk + "..refs/heads/release").WithRepo(repo).RunStdString(ctx)
	require.NoError(t, err)
	switch style {
	case repo_model.MergeStyleFastForwardOnly:
		assert.Equal(t, head("stack-upper"), head("release"))
	default:
		assert.Len(t, strings.Fields(landedCommits), 2, "one trunk commit per layer")
		if style == repo_model.MergeStyleSquash {
			merges, _, err := gitcmd.NewCommand("rev-list", "--merges").AddDynamicArguments(trunk + "..refs/heads/release").WithRepo(repo).RunStdString(ctx)
			require.NoError(t, err)
			assert.Empty(t, merges, "squashed layers leave a linear trunk")
		}
	}
}
