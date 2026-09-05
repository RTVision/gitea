// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package convert

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/structs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullRequest_APIFormat(t *testing.T) {
	// with HeadRepo
	assert.NoError(t, unittest.PrepareTestDatabase())
	headRepo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	assert.NoError(t, pr.LoadAttributes(t.Context()))
	assert.NoError(t, pr.LoadIssue(t.Context()))
	apiPullRequest := ToAPIPullRequest(t.Context(), pr, nil)
	assert.NotNil(t, apiPullRequest)
	assert.Equal(t, &structs.PRBranchInfo{
		Name:       "branch1",
		Ref:        "refs/pull/2/head",
		Sha:        "4a357436d925b5c974181ff12a994538ddc5a269",
		RepoID:     1,
		Repository: ToRepo(t.Context(), headRepo, access_model.Permission{AccessMode: perm.AccessModeRead}),
	}, apiPullRequest.Head)

	// withOut HeadRepo
	pr = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	assert.NoError(t, pr.LoadIssue(t.Context()))
	assert.NoError(t, pr.LoadAttributes(t.Context()))
	// simulate fork deletion
	pr.HeadRepo = nil
	pr.HeadRepoID = 100000
	apiPullRequest = ToAPIPullRequest(t.Context(), pr, nil)
	assert.NotNil(t, apiPullRequest)
	assert.Nil(t, apiPullRequest.Head.Repository)
	assert.EqualValues(t, -1, apiPullRequest.Head.RepoID)

	apiPullRequests, err := ToAPIPullRequests(t.Context(), pr.BaseRepo, []*issues_model.PullRequest{pr}, nil)
	assert.NoError(t, err)
	assert.Len(t, apiPullRequests, 1)
	assert.NotNil(t, apiPullRequests[0])
	assert.Nil(t, apiPullRequests[0].Head.Repository)
	assert.EqualValues(t, -1, apiPullRequests[0].Head.RepoID)
}

func TestPullRequest_APIFormatIncludesStackTrunk(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	assert.NoError(t, pr.LoadIssue(t.Context()))
	assert.NoError(t, pr.LoadAttributes(t.Context()))
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1, CreatedByID: 1}
	_, err := db.GetEngine(t.Context()).Insert(stack)
	assert.NoError(t, err)
	_, err = db.GetEngine(t.Context()).Insert(
		&issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, HeadSHA: "layer-head"},
		&issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(1, pr.HeadBranch)},
	)
	assert.NoError(t, err)

	converted := ToAPIPullRequest(t.Context(), pr, nil)
	assert.NotNil(t, converted.Stack)
	assert.Equal(t, stack.ID, converted.Stack.Number)
	assert.Equal(t, 1, converted.Stack.Size)
	assert.Equal(t, 1, converted.Stack.Position)
	assert.Equal(t, "master", converted.Stack.Base.Ref)
	assert.NotEmpty(t, converted.Stack.Base.Sha)
	assert.Equal(t, "master", converted.Base.Ref)

	_, err = db.GetEngine(t.Context()).Where("stack_id = ?", stack.ID).Delete(new(issues_model.StackBranchClaim))
	assert.NoError(t, err)
	converted = ToAPIPullRequest(t.Context(), pr, nil)
	assert.Nil(t, converted.Stack)

	merged := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 1})
	assert.NoError(t, merged.LoadIssue(t.Context()))
	assert.NoError(t, merged.LoadAttributes(t.Context()))
	history := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "deleted-trunk", State: issues_model.StackStateComplete, Revision: 2, CreatedByID: 1}
	_, err = db.GetEngine(t.Context()).Insert(history)
	assert.NoError(t, err)
	_, err = db.GetEngine(t.Context()).Insert(
		&issues_model.StackEntry{StackID: history.ID, PullRequestID: merged.ID, Position: 1, HeadSHA: "merged-head", LandedCommitSHA: "landed-sha"},
		&issues_model.StackBranchClaim{StackID: history.ID, PullRequestID: merged.ID, BranchKey: "historical-claim"},
	)
	assert.NoError(t, err)
	converted = ToAPIPullRequest(t.Context(), merged, nil)
	require.NotNil(t, converted.Stack)
	assert.Equal(t, "deleted-trunk", converted.Stack.Base.Ref)
	assert.Equal(t, "landed-sha", converted.Stack.Base.Sha)
}
