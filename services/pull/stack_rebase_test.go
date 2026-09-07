// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.dev/modules/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stackTestGit(t *testing.T, path string) func(...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
}

func TestStackReplaySavedBoundary(t *testing.T) {
	work := t.TempDir()
	run := stackTestGit(t, work)
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	run("checkout", "-b", "lower")
	require.NoError(t, os.WriteFile(filepath.Join(work, "lower.txt"), []byte("first\n"), 0o600))
	run("add", ".")
	run("commit", "-m", "lower one")
	require.NoError(t, os.WriteFile(filepath.Join(work, "lower.txt"), []byte("second\n"), 0o600))
	run("commit", "-am", "lower two")
	boundary := run("rev-parse", "HEAD")
	run("checkout", "-b", "upper")
	require.NoError(t, os.WriteFile(filepath.Join(work, "upper.txt"), []byte("layer only\n"), 0o600))
	run("add", ".")
	run("commit", "-m", "upper only")
	head := run("rev-parse", "HEAD")
	run("checkout", "release")
	run("merge", "--squash", "lower")
	run("commit", "-m", "squashed lower")
	trunk := run("rev-parse", "HEAD")
	repo, err := git.OpenRepositoryLocal(t.Context(), work)
	require.NoError(t, err)
	defer repo.Close()
	env := append(os.Environ(), "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")
	replayed, err := replayStackLayer(t.Context(), repo, env, nil, head, boundary, trunk, 2)
	require.NoError(t, err)
	assert.Equal(t, "1", run("rev-list", "--count", trunk+".."+replayed))
	assert.Equal(t, "upper only", run("log", "-1", "--format=%s", replayed))
	assert.Equal(t, "second", run("show", replayed+":lower.txt"))
	assert.Equal(t, head, run("rev-parse", "upper"))
	assert.Equal(t, boundary, run("rev-parse", "lower"))
	_, err = replayStackLayer(t.Context(), repo, env, nil, head, trunk, boundary, 2)
	require.ErrorContains(t, err, "saved parent is not an ancestor")

	run("checkout", "lower")
	require.NoError(t, os.WriteFile(filepath.Join(work, "lower.txt"), []byte("review correction\n"), 0o600))
	run("commit", "-a", "--amend", "--no-edit")
	newParent := run("rev-parse", "HEAD")
	replayed, err = replayStackLayer(t.Context(), repo, env, nil, head, boundary, newParent, 2)
	require.NoError(t, err)
	assert.Equal(t, "1", run("rev-list", "--count", newParent+".."+replayed))
	assert.Equal(t, "review correction", run("show", replayed+":lower.txt"))
}

func TestStackReplayConflictLeavesSourceUntouched(t *testing.T) {
	work := t.TempDir()
	run := stackTestGit(t, work)
	run("init", "--initial-branch=release")
	require.NoError(t, os.WriteFile(filepath.Join(work, "file"), []byte("base\n"), 0o600))
	run("add", ".")
	run("commit", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("checkout", "-b", "layer")
	require.NoError(t, os.WriteFile(filepath.Join(work, "file"), []byte("layer\n"), 0o600))
	run("commit", "-am", "layer")
	head := run("rev-parse", "HEAD")
	run("checkout", "release")
	require.NoError(t, os.WriteFile(filepath.Join(work, "file"), []byte("trunk\n"), 0o600))
	run("commit", "-am", "trunk")
	trunk := run("rev-parse", "HEAD")
	repo, err := git.OpenRepositoryLocal(t.Context(), work)
	require.NoError(t, err)
	defer repo.Close()
	_, err = replayStackLayer(t.Context(), repo, os.Environ(), nil, head, base, trunk, 1)
	var conflict StackRebaseConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, []string{"file"}, conflict.Files)
	assert.Equal(t, head, run("rev-parse", "layer"))
	assert.Equal(t, trunk, run("rev-parse", "release"))
}
