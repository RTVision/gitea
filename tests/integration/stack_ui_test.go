// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeStackUpdateBranchButton(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, _ *url.URL) {
		defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
		session := loginUser(t, "user2")
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		testCreateBranch(t, session, "user2", "repo1", "branch/master", "release", http.StatusSeeOther)
		for _, mode := range []string{issues_model.StackModeMerge, issues_model.StackModeRebase} {
			branch := "stack-button-" + mode
			testEditFileToNewBranch(t, session, "user2", "repo1", "release", branch, "README.md", mode+"\n")
			testPullCreate(t, session, "user2", "repo1", true, "release", branch, mode)
			pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: branch})
			session.MakeRequest(t, NewRequestWithValues(t, http.MethodPost, "/user2/repo1/pulls/stacks/new", map[string]string{"pull": strconv.FormatInt(pr.Index, 10), "mode": mode}), http.StatusSeeOther)
			stack, err := issues_model.GetPullRequestStack(t.Context(), pr.ID)
			require.NoError(t, err)
			assert.Equal(t, "release", stack.TrunkBranch, "the trunk follows from the start layer")
		}
		testEditFileToNewBranch(t, session, "user2", "repo1", "stack-button-merge", "stack-button-top", "README.md", "top\n")
		testPullCreate(t, session, "user2", "repo1", true, "stack-button-merge", "stack-button-top", "top")
		top := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: "stack-button-top"})
		lower, err := issues_model.GetPullRequestStack(t.Context(), unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: "stack-button-merge"}).ID)
		require.NoError(t, err)
		body := session.MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/user2/repo1/pulls/%d", top.Index)), http.StatusOK).Body.String()
		assert.Contains(t, body, fmt.Sprintf(`href="/user2/repo1/pulls/stacks/%d"`, lower.ID))
		resp := session.MakeRequest(t, NewRequestWithValues(t, http.MethodPost, "/user2/repo1/pulls/stacks/new", map[string]string{"pull": strconv.FormatInt(top.Index, 10), "mode": "unknown"}), http.StatusSeeOther)
		assert.Equal(t, fmt.Sprintf("/user2/repo1/pulls/stacks/new?mode=unknown&pull=%d&start=", top.Index), test.RedirectURL(resp))
		assert.Equal(t, "Could not create stack: unknown stack mode &#34;unknown&#34;", session.GetCookieFlashMessage().ErrorMsg)
		testCreateFileInBranch(t, owner, repo, createFileInBranchOptions{OldBranch: "release"}, map[string]string{"trunk.txt": "trunk\n"})
		require.NoError(t, queue.GetManager().FlushAll(t.Context(), 10*time.Second))
		for mode, offered := range map[string]bool{issues_model.StackModeMerge: true, issues_model.StackModeRebase: false} {
			pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{BaseRepoID: repo.ID, HeadBranch: "stack-button-" + mode})
			body := session.MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/user2/repo1/pulls/%d", pr.Index)), http.StatusOK).Body.String()
			assert.Equal(t, offered, strings.Contains(body, "/update?style=merge"), mode)
			assert.NotContains(t, body, "/update?style=rebase", mode)
		}
	})
}
