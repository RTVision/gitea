// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"

	"github.com/stretchr/testify/require"
)

func TestAddPullRequestMergedTarget(t *testing.T) {
	type PullRequest struct {
		ID             int64 `xorm:"pk autoincr"`
		BaseRepoID     int64
		MergedCommitID string
	}
	x, cleanup := migrationtest.PrepareTestEnv(t, 0, new(PullRequest))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&PullRequest{BaseRepoID: 1, MergedCommitID: "legacy"})
	require.NoError(t, err)
	require.NoError(t, AddPullRequestMergedTarget(t.Context(), x))
	var target struct {
		MergedRepoID   int64
		MergedBranch   string
		MergedCommitID string
	}
	found, err := x.Table("pull_request").Get(&target)
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, target.MergedRepoID)
	require.Empty(t, target.MergedBranch)
	require.Equal(t, "legacy", target.MergedCommitID)
}
