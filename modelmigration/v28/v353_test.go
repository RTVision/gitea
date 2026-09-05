// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"

	"github.com/stretchr/testify/require"
)

func TestAddPullRequestStacks(t *testing.T) {
	x, cleanup := migrationtest.PrepareTestEnv(t, 0)
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	require.NoError(t, AddPullRequestStacks(t.Context(), x))
	for _, table := range []string{"pull_request_stack", "stack_entry", "stack_branch_claim", "stack_operation"} {
		exists, err := x.IsTableExist(table)
		require.NoError(t, err)
		require.True(t, exists, table)
	}
	require.NoError(t, AddPullRequestStacks(t.Context(), x))
}
