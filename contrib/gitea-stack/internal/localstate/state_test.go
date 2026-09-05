// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package localstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockRecoversDeadOwnerAndReleases(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("99999999\n"), 0o600))
	unlock, err := store.Lock()
	require.NoError(t, err)
	assert.FileExists(t, lockPath)
	unlock()
	assert.NoFileExists(t, lockPath)
}

func TestStateWriteRoundTrip(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	want := &State{Remote: "origin", Trunk: "main", Stack: 7, Layers: []Layer{{Branch: "feature", PullRequest: 3, ParentSHA: "parent", HeadSHA: "head"}}}
	require.NoError(t, store.Save(want))
	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
