// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
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
	pull_service "gitea.dev/services/pull"

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
			_, err := pull_service.CreateStack(t.Context(), owner, repo, pull_service.CreateStackOptions{TrunkBranch: "release", Mode: mode, PullRequestIDs: []int64{pr.ID}})
			require.NoError(t, err)
		}
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
