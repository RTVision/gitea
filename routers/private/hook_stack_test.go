// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package private

import (
	"fmt"
	"net/http"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/private"
	repo_module "gitea.dev/modules/repository"
	gitea_context "gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostReceiveForkStackLanding(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	main := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 10})
	fork := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 11})
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 3})
	pr.BaseRepoID = fork.ID
	pr.BaseBranch = "lower"
	_, err := db.GetEngine(t.Context()).ID(pr.ID).Cols("base_repo_id", "base_branch").Update(pr)
	require.NoError(t, err)
	_, err = db.GetEngine(t.Context()).ID(pr.IssueID).Cols("repo_id").Update(&issues_model.Issue{RepoID: fork.ID})
	require.NoError(t, err)
	stack := &issues_model.PullRequestStack{RepoID: main.ID, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	require.NoError(t, db.Insert(t.Context(), &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, OldParentSHA: "parent"}))
	require.NoError(t, db.Insert(t.Context(), &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(fork.ID, pr.HeadBranch)}))
	op := &issues_model.StackOperation{StackID: stack.ID, ExpectedRevision: 1, Kind: "land", State: "running", JournalJSON: fmt.Sprintf(`{"stage":"confirm","layers":[{"pull_id":%d,"phase":"merging","landing_base_sha":"before","merge_candidate_sha":"after","old_parent":"parent"}]}`, pr.ID)}
	require.NoError(t, issues_model.CreateStackOperation(t.Context(), op))
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	opts := &private.HookOptions{PullRequestID: pr.ID, UserID: actor.ID}
	updates := []*repo_module.PushUpdateOptions{{RefFullName: git.RefNameFromBranch(stack.TrunkBranch), OldCommitID: "before", NewCommitID: "after"}}
	ctx, response := contexttest.MockPrivateContext(t, "/")
	ctx.Doer = actor
	assert.False(t, hookPostReceiveHandlePullRequestMerging(ctx, opts, fork, updates))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID}).HasMerged)
	assert.False(t, unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: pr.IssueID}).IsClosed)
	ctx, response = contexttest.MockPrivateContext(t, "/")
	ctx.Doer = actor
	require.True(t, hookPostReceiveHandlePullRequestMerging(ctx, opts, main, updates))
	assert.Empty(t, response.Body.String())
	merged := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID})
	assert.True(t, merged.HasMerged)
	assert.Equal(t, fork.ID, merged.BaseRepoID)
	assert.Equal(t, "lower", merged.BaseBranch)
	assert.Equal(t, main.ID, merged.MergedRepoID)
	assert.Equal(t, stack.TrunkBranch, merged.MergedBranch)
	assert.Equal(t, "after", merged.MergedCommitID)
	assert.Equal(t, "before", merged.MergedBaseCommitID)
	assert.Equal(t, "parent", merged.MergeBase)
}

func TestPreReceiveStackBranchDeletion(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "release", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	require.NoError(t, db.Insert(t.Context(), &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: 1, BranchKey: issues_model.StackBranchKey(repo.ID, "layer")}))
	for _, branch := range []string{"release", "layer", "unrelated"} {
		t.Run(branch, func(t *testing.T) {
			mock, response := contexttest.MockPrivateContext(t, "/")
			mock.Repo = &gitea_context.Repository{Repository: repo}
			ctx := &preReceiveContext{PrivateContext: mock, opts: &private.HookOptions{}, canWriteCodeUnitCached: new(true)}
			preReceiveBranch(ctx, "1111111111111111111111111111111111111111", mock.Repo.GetObjectFormat().EmptyObjectID().String(), git.RefNameFromBranch(branch))
			if branch == "unrelated" {
				assert.False(t, ctx.Written())
			} else {
				assert.Equal(t, http.StatusForbidden, response.Code)
				assert.Contains(t, response.Body.String(), "finish or unstack")
			}
		})
	}
}
