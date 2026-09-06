// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package localstate

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockReportsDeadOwnerWithoutRacingTakeover(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	host, err := os.Hostname()
	require.NoError(t, err)
	data, err := json.Marshal(lockOwner{Host: host, PID: 99999999})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lockPath, data, 0o600))
	_, err = store.Lock()
	require.Error(t, err)
	assert.Contains(t, err.Error(), lockPath)
	assert.FileExists(t, lockPath)
}

func TestLockCreatesAndReleases(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	unlock, err := store.Lock()
	require.NoError(t, err)
	assert.FileExists(t, lockPath)
	unlock()
	assert.NoFileExists(t, lockPath)
}

func TestUnlockDoesNotRemoveReplacement(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	unlock, err := store.Lock()
	require.NoError(t, err)
	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, os.WriteFile(lockPath, []byte("replacement\n"), 0o600))

	unlock()
	assert.FileExists(t, lockPath)
}

func TestLockDoesNotRecoverForeignOwner(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	data, err := json.Marshal(lockOwner{Host: "foreign.example.test", PID: 99999999})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lockPath, data, 0o600))

	_, err = store.Lock()
	var foreign ForeignLockError
	require.ErrorAs(t, err, &foreign)
	assert.Equal(t, "foreign.example.test", foreign.Host)
	assert.Equal(t, 99999999, foreign.PID)
	assert.Equal(t, lockPath, foreign.Path)
	assert.FileExists(t, lockPath)
}

func TestLockDoesNotRecoverLiveLocalOwner(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	host, err := os.Hostname()
	require.NoError(t, err)
	data, err := json.Marshal(lockOwner{Host: host, PID: os.Getpid()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lockPath, data, 0o600))

	_, err = store.Lock()
	require.Error(t, err)
	assert.FileExists(t, lockPath)
}

func TestLockDoesNotGuessLegacyOwnerHost(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	lockPath := filepath.Join(store.Dir, "operation.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("99999999\n"), 0o600))

	_, err := store.Lock()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy lock")
	assert.FileExists(t, lockPath)
}

func TestStateWriteRoundTrip(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	want := &State{Remote: "origin", Trunk: "main", Stack: 7, Layers: []Layer{{Branch: "feature", PullRequest: 3, ParentSHA: "parent", HeadSHA: "head"}}}
	require.NoError(t, store.Save(want))
	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
