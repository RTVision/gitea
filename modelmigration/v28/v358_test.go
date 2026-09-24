// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"

	"github.com/stretchr/testify/require"
)

func TestAddModeToPullRequestStack(t *testing.T) {
	type PullRequestStack struct {
		ID          int64  `xorm:"pk autoincr"`
		TrunkBranch string `xorm:"NOT NULL"`
	}
	x, cleanup := migrationtest.PrepareTestEnv(t, 0, new(PullRequestStack))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&PullRequestStack{TrunkBranch: "main"})
	require.NoError(t, err)
	require.NoError(t, AddModeToPullRequestStack(t.Context(), x))
	var mode string
	found, err := x.Table("pull_request_stack").Cols("mode").Get(&mode)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "rebase", mode, "existing stacks keep rebase behavior")
}
