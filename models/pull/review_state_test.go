// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull_test

import (
	"testing"

	"gitea.dev/models/db"
	pull_model "gitea.dev/models/pull"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetNewestReviewStateSameSecond(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	const userID, pullID = 2, 2
	const oldHead, newHead = "834ac18b22efc05c8eb85db2638cfe750fa8c315", "d7ec063eeb734f0968f4c98fc6ab900aeb269c10" // old head sorts first
	_, err := pull_model.UpdateReviewState(ctx, userID, pullID, oldHead, map[string]pull_model.ViewedState{"a": pull_model.Unviewed})
	require.NoError(t, err)
	_, err = pull_model.UpdateReviewState(ctx, userID, pullID, oldHead, map[string]pull_model.ViewedState{"a": pull_model.HasChanged})
	require.NoError(t, err)
	_, err = pull_model.UpdateReviewState(ctx, userID, pullID, newHead, map[string]pull_model.ViewedState{"a": pull_model.Viewed})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).Exec("UPDATE review_state SET updated_unix = 1 WHERE user_id = ? AND pull_id = ?", userID, pullID) // the sync and the mark landing in one second
	require.NoError(t, err)

	newest, err := pull_model.GetNewestReviewState(ctx, userID, pullID, newHead)
	require.NoError(t, err)
	assert.Equal(t, newHead, newest.CommitSHA)
	assert.Equal(t, pull_model.Viewed, newest.UpdatedFiles["a"])

	// force-pushed back to the old head: the mark there is what landed last
	newest, err = pull_model.GetNewestReviewState(ctx, userID, pullID, oldHead)
	require.NoError(t, err)
	assert.Equal(t, oldHead, newest.CommitSHA)
}
