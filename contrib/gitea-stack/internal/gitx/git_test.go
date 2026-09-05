// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return string(output)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func commit(t *testing.T, dir, message string) string {
	t.Helper()
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=Stack Test", "-c", "user.email=stack@example.test", "commit", "-m", message)
	return stringTrim(git(t, dir, "rev-parse", "HEAD"))
}

func stringTrim(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

func TestPushLeaseRejectsConcurrentRemoteUpdate(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	other := filepath.Join(root, "other")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "-b", "main", work)
	write(t, filepath.Join(work, "file"), "main\n")
	commit(t, work, "main")
	git(t, work, "remote", "add", "origin", remote)
	git(t, work, "push", "-u", "origin", "main")
	git(t, work, "switch", "-c", "feature")
	write(t, filepath.Join(work, "file"), "feature\n")
	firstHead := commit(t, work, "feature")
	repo := Repo{Dir: work}
	require.Empty(t, mustRemoteHead(t, repo, "origin", "feature"))
	require.NoError(t, repo.PushLease("origin", "feature", ""))
	assert.Equal(t, firstHead, mustRemoteHead(t, repo, "origin", "feature"))

	git(t, root, "clone", remote, other)
	git(t, other, "switch", "feature")
	write(t, filepath.Join(other, "other"), "concurrent\n")
	commit(t, other, "concurrent")
	git(t, other, "push", "origin", "feature")
	write(t, filepath.Join(work, "local"), "local\n")
	commit(t, work, "local")
	assert.Error(t, repo.PushLease("origin", "feature", firstHead))
}

func mustRemoteHead(t *testing.T, repo Repo, remote, branch string) string {
	t.Helper()
	head, err := repo.RemoteHead(remote, branch)
	require.NoError(t, err)
	return head
}

func TestRebaseUsesSavedLayerBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(dir), "init", "-b", "main", dir)
	write(t, filepath.Join(dir, "base"), "base\n")
	oldBase := commit(t, dir, "base")
	git(t, dir, "switch", "-c", "feature")
	write(t, filepath.Join(dir, "feature"), "feature\n")
	oldHead := commit(t, dir, "feature")
	git(t, dir, "switch", "main")
	write(t, filepath.Join(dir, "trunk"), "trunk\n")
	newBase := commit(t, dir, "trunk")

	repo := Repo{Dir: dir}
	require.NoError(t, repo.Rebase(oldBase, newBase, "feature", ""))
	newHead, err := repo.Head("feature")
	require.NoError(t, err)
	assert.NotEqual(t, oldHead, newHead)
	require.NoError(t, repo.IsAncestor(newBase, newHead))
	assert.Equal(t, "feature\n", string(mustRead(t, filepath.Join(dir, "feature"))))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}
