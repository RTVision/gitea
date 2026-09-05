// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	api "gitea.dev/modules/structs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteLeaseCanAdvance(t *testing.T) {
	serverRewrite := &api.PullRequestStackEntry{HeadSHA: "server-head"}
	assert.True(t, remoteLeaseCanAdvance("old-local", "server-head", serverRewrite))
	assert.True(t, remoteLeaseCanAdvance("unchanged", "unchanged", serverRewrite))
	assert.False(t, remoteLeaseCanAdvance("old-local", "developer-head", serverRewrite))
	assert.False(t, remoteLeaseCanAdvance("old-local", "", serverRewrite))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return string(output)
}

func runCLI(t *testing.T, binary, dir string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", output)
	return string(output), exitErr.ExitCode()
}

func buildCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	binary := filepath.Join(t.TempDir(), "gitea-stack")
	command := exec.Command("go", "build", "-o", binary, "./contrib/gitea-stack")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return binary
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func commitGit(t *testing.T, dir, message string) string {
	t.Helper()
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.name=Stack Test", "-c", "user.email=stack@example.test", "commit", "-m", message)
	return trimLine(runGit(t, dir, "rev-parse", "HEAD"))
}

func trimLine(value string) string {
	for len(value) != 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

func TestCompiledCLIReleasesLockAndContinuesConflict(t *testing.T) {
	binary := buildCLI(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "-b", "main", repo)
	runGit(t, repo, "remote", "add", "origin", remote)
	writeFile(t, filepath.Join(repo, "content"), "base\n")
	commitGit(t, repo, "base")
	runGit(t, repo, "switch", "-c", "feature")
	writeFile(t, filepath.Join(repo, "content"), "feature\n")
	commitGit(t, repo, "feature")

	_, code := runCLI(t, binary, repo, "init")
	assert.Equal(t, 2, code)
	output, code := runCLI(t, binary, repo, "init", "--trunk", "main", "--remote", "origin", "feature")
	require.Equal(t, 0, code, output)
	assert.NoFileExists(t, filepath.Join(repo, ".git", "gitea-stack", "operation.lock"))

	runGit(t, repo, "switch", "main")
	writeFile(t, filepath.Join(repo, "content"), "trunk\n")
	newTrunk := commitGit(t, repo, "trunk")
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", newTrunk)
	runGit(t, repo, "switch", "feature")
	output, code = runCLI(t, binary, repo, "restack")
	require.Equal(t, 5, code, output)
	assert.FileExists(t, filepath.Join(repo, ".git", "gitea-stack", "restack.json"))
	assert.NoFileExists(t, filepath.Join(repo, ".git", "gitea-stack", "operation.lock"))

	writeFile(t, filepath.Join(repo, "content"), "resolved\n")
	runGit(t, repo, "add", "content")
	output, code = runCLI(t, binary, repo, "restack", "--continue")
	require.Equal(t, 0, code, output)
	assert.NoFileExists(t, filepath.Join(repo, ".git", "gitea-stack", "restack.json"))
	featureHead := trimLine(runGit(t, repo, "rev-parse", "feature"))
	_, err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", newTrunk, featureHead).CombinedOutput()
	assert.NoError(t, err)
}
