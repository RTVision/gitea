// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull_test

import (
	"testing"
	"time"

	pull_model "gitea.dev/models/pull"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateReviewStateSameSecond(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer timeutil.MockSet(time.Unix(1_700_000_000, 0))()
	ctx := t.Context()
	const userID, pullID = 2, 2
	const a, b, c = "834ac18b22efc05c8eb85db2638cfe750fa8c315", "d7ec063eeb734f0968f4c98fc6ab900aeb269c10", "f00d000000000000000000000000000000000000" // a sorts first
	write := func(commit string, state pull_model.ViewedState) {
		_, err := pull_model.UpdateReviewState(ctx, userID, pullID, commit, map[string]pull_model.ViewedState{"file": state})
		require.NoError(t, err)
	}
	assertNewest := func(commit string, state pull_model.ViewedState) {
		newest, err := pull_model.GetNewestReviewState(ctx, userID, pullID)
		require.NoError(t, err)
		assert.Equal(t, commit, newest.CommitSHA)
		assert.Equal(t, state, newest.UpdatedFiles["file"])
	}

	write(a, pull_model.Viewed)
	write(a, pull_model.HasChanged) // pushed to b: the sync marks a's review
	write(b, pull_model.Viewed)     // then the file is marked at b
	assertNewest(b, pull_model.Viewed)

	write(b, pull_model.HasChanged) // force-pushed back to a
	write(a, pull_model.Viewed)
	assertNewest(a, pull_model.Viewed)

	write(c, pull_model.Viewed) // pushed to c: seeded from a
	assertNewest(c, pull_model.Viewed)
}
