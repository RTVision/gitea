// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"gitea.dev/modelmigration/migrationtest"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAddPullRequestMergedBaseCommitID(t *testing.T) {
	type PullRequest struct {
		ID             int64  `xorm:"pk"`
		MergedCommitID string `xorm:"VARCHAR(64)"`
	}
	x, cleanup := migrationtest.PrepareTestEnv(t, 0, new(PullRequest))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&PullRequest{ID: 1, MergedCommitID: "historical-merge"})
	require.NoError(t, err)
	require.NoError(t, AddPullRequestMergedBaseCommitID(t.Context(), x))
	var result struct {
		MergedCommitID     string
		MergedBaseCommitID string
	}
	found, err := x.Table("pull_request").Where("id = ?", 1).Get(&result)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "historical-merge", result.MergedCommitID)
	require.Empty(t, result.MergedBaseCommitID)
}
