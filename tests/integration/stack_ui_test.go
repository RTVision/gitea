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

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/queue"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/tests"

	"github.com/PuerkitoBio/goquery"
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

func TestPullListGroupsStacks(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	session := loginUser(t, "user2")
	testPullCreate(t, session, "user2", "repo1", false, "master", "DefaultBranch", "unstacked pull")
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", Mode: issues_model.StackModeMerge, State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	for i, pullID := range []int64{1, 2, 5} { // fixture pulls #2 (merged), #3 and #5
		require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pullID, Position: i + 1}))
	}
	op := &issues_model.StackOperation{StackID: stack.ID, Kind: "land", State: "blocked"}
	require.NoError(t, db.Insert(t.Context(), op))
	_, err := db.GetEngine(t.Context()).ID(stack.ID).Cols("active_operation_id").Update(&issues_model.PullRequestStack{ActiveOperationID: op.ID})
	require.NoError(t, err)
	stackLink := fmt.Sprintf(`a[href="/user2/repo1/pulls/stacks/%d"]`, stack.ID)
	list := func(query string) *HTMLDoc {
		return NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, "/user2/repo1/pulls"+query), http.StatusOK).Body)
	}

	doc := list("")
	assert.Equal(t, 2, doc.Find("#issue-list > .item").Length())
	stackRow := doc.Find("#issue-list > .item:has(details)")
	assert.Equal(t, "issue3", strings.TrimSpace(stackRow.Find(".item-header").First().Text())) // lowest open layer; #2 has merged
	assert.Equal(t, fmt.Sprintf("Stack #%d", stack.ID), strings.TrimSpace(stackRow.Find(".item-body "+stackLink).First().Text()))
	assert.Contains(t, stackRow.Find(".item-body").First().Text(), "Operation: blocked")
	assert.Equal(t, 3, stackRow.Find(".stack-status-bar > span").Length())
	assert.Equal(t, "3 of 3 layers", strings.TrimSpace(stackRow.Find("details[open] > summary").Text())) // few layers start expanded
	assert.Equal(t, 3, stackRow.Find("details .index").Length())
	assert.Equal(t, "4 Open", strings.Join(strings.Fields(doc.Find(".small-menu-items .item").First().Text()), " ")) // counts individual pulls

	doc = list("?view=flat")
	assert.Equal(t, 4, doc.Find("#issue-list > .item").Length())
	assert.Equal(t, 3, doc.Find("#issue-list > .item "+stackLink).Length())
	assert.Equal(t, 4, list("").Find("#issue-list > .item").Length(), "flat view is remembered")

	doc = list("?view=grouped&milestone=3")
	assert.Equal(t, 1, doc.Find("#issue-list > .item").Length())
	assert.Equal(t, "1 of 3 match", strings.TrimSpace(doc.Find("#issue-list details[open] > summary").Text()))
	layers := doc.Find("#issue-list details[open] .index")
	assert.Equal(t, []string{"#3"}, layers.Map(func(_ int, s *goquery.Selection) string { return strings.TrimSpace(s.Text()) }))

	defer test.MockVariableValue(&setting.UI.IssuePagingNum, 1)()
	indexes := func(doc *HTMLDoc) []string {
		return doc.Find("#issue-list .index").Map(func(_ int, s *goquery.Selection) string { return strings.TrimSpace(s.Text()) })
	}
	doc = list("?view=grouped&page=1")
	assert.Equal(t, []string{"#6"}, indexes(doc))
	assert.Positive(t, doc.Find(`.pagination a[href*="page=2"]`).Length())
	assert.Zero(t, doc.Find(`.pagination a[href*="page=3"]`).Length(), "a stack takes one page slot")
	assert.Equal(t, []string{"#2", "#3", "#5"}, indexes(list("?view=grouped&page=2")))
}
