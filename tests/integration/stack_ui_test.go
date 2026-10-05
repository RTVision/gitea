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
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/commitstatus"
	"gitea.dev/modules/git"
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
		stackPage := NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, fmt.Sprintf("/user2/repo1/pulls/stacks/%d", lower.ID)), http.StatusOK).Body)
		assert.Equal(t, 1, stackPage.Find(fmt.Sprintf(`.menu .item[data-value="%d"]`, top.ID)).Length(), "the append link lands on an insert picker offering the pull request")
		resp := session.MakeRequest(t, NewRequestWithValues(t, http.MethodPost, "/user2/repo1/pulls/stacks/new", map[string]string{"pull": strconv.FormatInt(top.Index, 10), "start": "999", "mode": "merge"}), http.StatusSeeOther)
		assert.Equal(t, fmt.Sprintf("/user2/repo1/pulls/stacks/new?mode=merge&pull=%d&start=999", top.Index), test.RedirectURL(resp))
		assert.Equal(t, "The stack changed while you were viewing it. Review the current list and try again.", session.GetCookieFlashMessage().ErrorMsg)
		resp = session.MakeRequest(t, NewRequestWithValues(t, http.MethodPost, "/user2/repo1/pulls/stacks/new", map[string]string{"pull": strconv.FormatInt(top.Index, 10), "mode": "unknown"}), http.StatusSeeOther)
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
	assert.Zero(t, stackRow.Find("details[open]").Length(), "unfiltered stacks start collapsed")
	assert.Equal(t, "3 of 3 layers", strings.TrimSpace(stackRow.Find("details > summary").Text()))
	assert.Equal(t, 3, stackRow.Find("details .index").Length())
	assert.Zero(t, stackRow.Find("details "+stackLink).Length(), "grouped layers do not repeat the stack badge")
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
	assert.Equal(t, []string{"#5", "#3", "#2"}, indexes(list("?view=grouped&page=2")))
}

func TestPullStackEntryStatus(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	gitRepo, err := git.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "master", Mode: issues_model.StackModeMerge, State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	var parentID int64
	for i, pullID := range []int64{1, 2, 5} {
		pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pullID})
		sha, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
		require.NoError(t, err)
		boundary, err := git.MergeBase(ctx, gitRepo, git.RefNameFromBranch(pr.BaseBranch).String(), sha)
		require.NoError(t, err)
		require.NoError(t, db.Insert(ctx,
			&issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: i + 1, ParentPullRequestID: parentID, HeadSHA: sha, OldParentSHA: boundary},
			&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)},
		))
		parentID = pr.ID
	}
	_, err = db.GetEngine(ctx).In("issue_id", 2, 3, 11).Cols("official").Update(&issues_model.Review{Official: false})
	require.NoError(t, err)
	for i, review := range []*issues_model.Review{
		{Type: issues_model.ReviewTypeApprove},
		{Type: issues_model.ReviewTypeApprove, Stale: true},
		{Type: issues_model.ReviewTypeReject},
		{Type: issues_model.ReviewTypeRequest},
	} {
		review.IssueID, review.ReviewerID, review.Official = 11, int64(i+1), true
		require.NoError(t, db.Insert(ctx, review))
	}
	for _, check := range []struct {
		pullID int64
		state  commitstatus.CommitStatusState
	}{{5, commitstatus.CommitStatusSuccess}, {2, commitstatus.CommitStatusFailure}} {
		pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: check.pullID})
		sha, err := gitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
		require.NoError(t, err)
		require.NoError(t, git_model.NewCommitStatus(ctx, git_model.NewCommitStatusOptions{
			Repo: repo, Creator: owner, SHA: git.MustIDFromString(sha),
			CommitStatus: &git_model.CommitStatus{Context: "ci", State: check.state, TargetURL: "https://example.com/ci"},
		}))
	}
	assertLayers := func(layers *goquery.Selection) {
		t.Helper()
		require.Equal(t, 3, layers.Length())
		links := layers.Map(func(_ int, layer *goquery.Selection) string {
			if layer.Is("a") {
				return layer.AttrOr("href", "")
			}
			return layer.Find("a").First().AttrOr("href", "")
		})
		assert.Equal(t, []string{"/user2/repo1/pulls/5", "/user2/repo1/pulls/3", "/user2/repo1/pulls/2"}, links)
		for i, label := range []string{"All checks were successful", "Some checks failed"} {
			checks := layers.Eq(i).Find(".stack-checks")
			require.Equal(t, 1, checks.Length())
			assert.Equal(t, "img", checks.AttrOr("role", ""))
			assert.Equal(t, label+", 1 check", checks.AttrOr("aria-label", ""))
			assert.Equal(t, label, checks.AttrOr("data-tooltip-content", ""))
			assert.Equal(t, "1 check", strings.TrimSpace(checks.Text()))
		}
		assert.Equal(t, "2 approvals", strings.TrimSpace(layers.First().Find(".approvals").Text()))
		assert.Equal(t, "1 change request", strings.TrimSpace(layers.First().Find(".rejects").Text()))
		assert.Equal(t, "1 waiting review", strings.TrimSpace(layers.First().Find(".waiting").Text()))
		assert.Zero(t, layers.Eq(1).Find(".approvals, .rejects, .waiting").Length())
		assert.Zero(t, layers.Last().Find(".stack-entry-status").Length())
		assert.Zero(t, layers.Find("a a").Length())
	}
	session := loginUser(t, "user2")
	pullPage := NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, "/user2/repo1/pulls/5"), http.StatusOK).Body)
	assertLayers(pullPage.Find(".stack-menu .stack-layer"))
	assertLayers(pullPage.Find(".timeline-item .stack-layer"))
	stackPath := fmt.Sprintf("/user2/repo1/pulls/stacks/%d", stack.ID)
	stackPage := NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, stackPath), http.StatusOK).Body)
	assertLayers(stackPage.Find(".stack-layer-list > .stack-layer"))
	landingSelector := fmt.Sprintf(`form[action="%s/land"]`, stackPath)
	landingForm := stackPage.Find(landingSelector)
	require.Equal(t, 1, landingForm.Length())
	assert.Equal(t, "Layers to land", strings.TrimSpace(landingForm.Find(`label[for="stack-through"]`).Text()))
	through := landingForm.Find(`select[name="through"]`)
	require.Equal(t, 1, through.Length())
	assert.Equal(t, []string{"3", "2"}, through.Find("option").Map(func(_ int, option *goquery.Selection) string {
		return option.AttrOr("value", "")
	}))
	selected := through.Find("option[selected]")
	require.Equal(t, 1, selected.Length())
	assert.Equal(t, "3", selected.AttrOr("value", ""))
	assert.Equal(t, "Whole stack", strings.TrimSpace(selected.Text()))
	assert.Equal(t, "Through #3", strings.TrimSpace(through.Find(`option[value="2"]`).Text()))
	assert.Zero(t, through.Find(`option[value="1"]`).Length())
	styles := landingForm.Find(`select[name="merge_style"]`)
	require.Equal(t, 1, styles.Length())
	assert.Equal(t, []string{"merge", "squash", "fast-forward-only"}, styles.Find("option").Map(func(_ int, option *goquery.Selection) string {
		return option.AttrOr("value", "")
	}))
	assert.Zero(t, landingForm.Find(`input[name="merge_style"]`).Length())

	pullUnit := repo.MustGetUnit(ctx, unit.TypePullRequests)
	pullUnit.Config = &repo_model.PullRequestsConfig{AllowSquash: true}
	require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, pullUnit))
	stackPage = NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, stackPath), http.StatusOK).Body)
	landingForm = stackPage.Find(landingSelector)
	require.Equal(t, 1, landingForm.Length())
	assert.Zero(t, landingForm.Find(`select[name="merge_style"]`).Length())
	style := landingForm.Find(`[name="merge_style"]`)
	require.Equal(t, 1, style.Length())
	assert.True(t, style.Is("input"))
	assert.Equal(t, "hidden", style.AttrOr("type", ""))
	assert.Equal(t, "squash", style.AttrOr("value", ""))
	assert.Equal(t, "Create squash commit", strings.TrimSpace(landingForm.Find("#stack-style").Text()))

	pullUnit.PullRequestsConfig().AllowSquash = false
	require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, pullUnit))
	stackPage = NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, stackPath), http.StatusOK).Body)
	assert.Zero(t, stackPage.Find(landingSelector).Length())
}

func TestPullListShowsForkSuffix(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	_, err := db.GetEngine(t.Context()).ID(2).Cols("is_closed").Update(&issues_model.Issue{IsClosed: true})
	require.NoError(t, err)
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", Mode: issues_model.StackModeMerge, State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	for i, pullID := range []int64{1, 6} {
		require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pullID, Position: i + 1}))
	}
	session := loginUser(t, "user2")
	page := NewHTMLParser(t, session.MakeRequest(t, NewRequest(t, http.MethodGet, "/user2/repo1/pulls?state=open"), http.StatusOK).Body)
	row := page.Find("#issue-list > .item:has(details)")
	require.Equal(t, 1, row.Length())
	assert.Equal(t, "/org3/repo3/pulls/2", row.Find(".item-header a").First().AttrOr("href", ""))
	assert.Equal(t, 1, row.Find(`.stack-layer .index[href="/org3/repo3/pulls/2"]`).Length())
	assert.Zero(t, row.Find(`.stack-layer .index[href="/user2/repo1/pulls/2"]`).Length())
	assert.Contains(t, row.Find(".stack-layer .index").Text(), "org3/repo3#2")
	assert.Zero(t, row.Find(`.stack-layer .issue-checkbox`).Length())
	assert.Equal(t, 2, row.Find(".stack-status-bar > span").Length())
	page = NewHTMLParser(t, MakeRequest(t, NewRequest(t, http.MethodGet, "/user2/repo1/pulls?state=open"), http.StatusOK).Body)
	assert.Zero(t, page.Find("#issue-list > .item:has(details)").Length(), "private fork layers are not disclosed")
	assert.NotContains(t, page.doc.Text(), "org3/repo3")
}
