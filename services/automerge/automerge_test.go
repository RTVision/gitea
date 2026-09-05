// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package automerge

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { unittest.MainTest(m) }

func TestScheduleAutoMergeRejectsStackedPreferences(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(1, pr.HeadBranch)}))
	for _, message := range []string{"", "custom merge message"} {
		for _, deleteBranch := range []bool{false, true} {
			scheduled, err := ScheduleAutoMerge(ctx, actor, pr, repo_model.MergeStyleSquash, message, deleteBranch)
			require.ErrorIs(t, err, ErrStackAutoMergeUnsupported)
			assert.False(t, scheduled)
		}
	}
	operations, err := issues_model.GetStackOperations(ctx, stack.ID)
	require.NoError(t, err)
	assert.Empty(t, operations)
	scheduled, _, err := pull_model.GetScheduledMergeByPullID(ctx, pr.ID)
	require.NoError(t, err)
	assert.False(t, scheduled)
}
