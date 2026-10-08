// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package private

import (
	"net/http"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/git"
	"gitea.dev/modules/private"
	gitea_context "gitea.dev/services/context"
	"gitea.dev/services/contexttest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
