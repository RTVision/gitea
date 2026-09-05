// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gitea.dev/contrib/gitea-stack/internal/gitx"
	"gitea.dev/contrib/gitea-stack/internal/localstate"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	binary := buildCLI(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "-b", "main", repo)
	runGit(t, repo, "config", "user.name", "Stack Test")
	runGit(t, repo, "config", "user.email", "stack@example.test")
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
	store, err := localstate.Open(gitx.Repo{Dir: repo})
	require.NoError(t, err)
	state, err := store.Load()
	require.NoError(t, err)
	output, code = runCLI(t, binary, repo, "--json", "--stack", "S2", "sync")
	require.Equal(t, 3, code, output)
	assert.Contains(t, output, `"code":"stack_unsubmitted"`)
	state.Stack = 1
	require.NoError(t, store.Save(state))
	host, err := os.Hostname()
	require.NoError(t, err)
	writeFile(t, filepath.Join(store.Dir, "operation.lock"), "{\"host\":\"foreign.example.test\",\"pid\":99999999}\n")
	output, code = runCLI(t, binary, repo, "--json", "status")
	require.Equal(t, 3, code, output)
	assert.Contains(t, output, `"code":"lock_foreign_host"`)
	assert.Contains(t, output, filepath.Join(".git", "gitea-stack", "operation.lock"))
	require.NoError(t, os.Remove(filepath.Join(store.Dir, "operation.lock")))
	writeFile(t, filepath.Join(store.Dir, "operation.lock"), fmt.Sprintf("{\"host\":%q,\"pid\":%d}\n", host, os.Getpid()))
	output, code = runCLI(t, binary, repo, "--json", "--stack", "S2", "sync")
	require.Equal(t, 3, code, output)
	assert.Contains(t, output, `"code":"stack_mismatch"`)
	require.NoError(t, os.Remove(filepath.Join(store.Dir, "operation.lock")))
	runGit(t, repo, "remote", "set-url", "origin", "git@code.example.test:/srv/git/acme/widget.git")
	t.Setenv("GITEA_TOKEN", "test-token")
	output, code = runCLI(t, binary, repo, "--json", "--stack", "S1", "status")
	require.Equal(t, 3, code, output)
	assert.Contains(t, output, `"code":"url_ambiguous"`)
	t.Setenv("GITEA_TOKEN", "")
	runGit(t, repo, "remote", "set-url", "origin", remote)

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
	_, err = exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", newTrunk, featureHead).CombinedOutput()
	assert.NoError(t, err)
}

func TestSyncRemoteLeaseContentSafety(t *testing.T) {
	for _, scenario := range []string{"added-content", "whitespace", "topology-only"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			work, remote := filepath.Join(root, "work"), filepath.Join(root, "owner", "repo.git")
			runGit(t, root, "init", "--bare", remote)
			runGit(t, root, "init", "-b", "main", work)
			runGit(t, work, "config", "user.name", "Stack Test")
			runGit(t, work, "config", "user.email", "stack@example.test")
			writeFile(t, filepath.Join(work, "base.txt"), "base\n")
			trunk := commitGit(t, work, "trunk")
			writeFile(t, filepath.Join(work, "parent.txt"), "parent\n")
			parent := commitGit(t, work, "parent")
			runGit(t, work, "switch", "-c", "feature")
			writeFile(t, filepath.Join(work, "feature.txt"), "feature\n")
			accepted := commitGit(t, work, "feature")
			runGit(t, work, "remote", "add", "origin", "file://localhost"+filepath.ToSlash(remote))
			runGit(t, work, "push", "origin", "main", "feature")
			writeFile(t, filepath.Join(work, "local.txt"), "unpublished\n")
			local := commitGit(t, work, "unpublished local change")

			var serverHead string
			if scenario == "topology-only" {
				newTrunk := trimLine(runGit(t, work, "commit-tree", parent+"^{tree}", "-p", trunk, "-m", "squashed parent"))
				serverHead = trimLine(runGit(t, work, "commit-tree", accepted+"^{tree}", "-p", newTrunk, "-m", "server rebase"))
				runGit(t, work, "push", "origin", newTrunk+":refs/heads/main", "--force")
			} else {
				runGit(t, work, "switch", "--detach", accepted)
				if scenario == "whitespace" {
					writeFile(t, filepath.Join(work, "feature.txt"), "feature \n")
				} else {
					writeFile(t, filepath.Join(work, "other.txt"), "other developer\n")
				}
				serverHead = commitGit(t, work, "other developer change")
				runGit(t, work, "switch", "feature")
			}
			runGit(t, work, "push", "origin", serverHead+":refs/heads/feature", "--force")
			entry := &api.PullRequestStackEntry{HeadSHA: serverHead, PullRequest: &api.PullRequest{Index: 1}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/owner/repo/stacks/1" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(&api.PullRequestStack{Number: 1, Revision: 2, Entries: []*api.PullRequestStackEntry{entry}})
			}))
			defer server.Close()
			t.Setenv("GITEA_URL", server.URL)
			t.Setenv("GITEA_TOKEN", "test-token")
			repo := gitx.Repo{Dir: work}
			store, err := localstate.Open(repo)
			require.NoError(t, err)
			state := &localstate.State{Stack: 1, Remote: "origin", Trunk: "main", Layers: []localstate.Layer{{Branch: "feature", PullRequest: 1, HeadSHA: accepted, RemoteSHA: accepted, ParentSHA: parent}}}
			require.NoError(t, store.Save(state))
			app := &application{repo: repo, store: store, jsonOutput: true, quiet: true}
			require.NoError(t, app.sync(t.Context()))
			state, err = store.Load()
			require.NoError(t, err)
			assert.Equal(t, local, trimLine(runGit(t, work, "rev-parse", "feature")), "sync preserves unpublished local commits")
			if scenario != "topology-only" {
				assert.Equal(t, accepted, state.Layers[0].RemoteSHA)
				err := app.pushLayers(t.Context(), state, 1)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "remote branch feature moved")
				assert.Equal(t, serverHead, trimLine(runGit(t, remote, "rev-parse", "feature")))
				return
			}
			assert.Equal(t, serverHead, state.Layers[0].RemoteSHA)
			assert.False(t, remoteLeaseCanAdvance(repo, local, "", serverHead, entry))
			assert.False(t, remoteLeaseCanAdvance(repo, local, "missing-object", serverHead, entry))
			assert.False(t, remoteLeaseCanAdvance(repo, local, accepted, serverHead, nil))
			assert.False(t, remoteLeaseCanAdvance(repo, local, accepted, "missing-object", &api.PullRequestStackEntry{HeadSHA: "missing-object"}))
			assert.True(t, remoteLeaseCanAdvance(repo, serverHead, "", serverHead, nil))
			require.NoError(t, app.restack(t.Context(), []string{"--no-sign"}))
			state, err = store.Load()
			require.NoError(t, err)
			require.NoError(t, app.pushLayers(t.Context(), state, 1))
			assert.Equal(t, "unpublished\n", runGit(t, remote, "show", "feature:local.txt"))
			assert.Equal(t, "feature\n", runGit(t, remote, "show", "feature:feature.txt"))
			assert.Equal(t, "parent\n", runGit(t, remote, "show", "feature:parent.txt"))
			assert.Equal(t, "2", trimLine(runGit(t, remote, "rev-list", "--count", "main..feature")), "only feature and unpublished commits are replayed")
		})
	}
}

func TestStackNumberSelection(t *testing.T) {
	state := &localstate.State{Stack: 7}
	app := &application{stackFlag: "S8"}
	_, err := app.boundStackNumber(state, false)
	var commandErr commandError
	require.ErrorAs(t, err, &commandErr)
	assert.Equal(t, "stack_mismatch", commandErr.kind)

	app.stackFlag = "S8"
	number, err := app.serverStackNumber(state)
	require.NoError(t, err)
	assert.Equal(t, int64(8), number)

	app.stackFlag = ""
	number, err = app.boundStackNumber(&localstate.State{}, true)
	require.NoError(t, err)
	assert.Zero(t, number, "the first submit remains valid before the local stack has a server id")
	store := &localstate.Store{Dir: t.TempDir()}
	require.NoError(t, store.Save(&localstate.State{}))
	app.store = store
	require.NoError(t, app.preflightStackBinding("submit"))
	_, err = app.boundStackNumber(&localstate.State{}, false)
	require.ErrorAs(t, err, &commandErr)
	assert.Equal(t, "stack_unsubmitted", commandErr.kind)

	err = mapGitContextError("fetch", context.Canceled)
	require.ErrorAs(t, err, &commandErr)
	assert.Equal(t, "git_timeout", commandErr.kind)
	assert.Contains(t, err.Error(), "was canceled")
}

func TestSyncDetectsUpperLayerAfterLowerLocalHeadMoves(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	runGit(t, filepath.Dir(dir), "init", "-b", "main", dir)
	runGit(t, dir, "config", "user.name", "Stack Test")
	runGit(t, dir, "config", "user.email", "stack@example.test")
	writeFile(t, filepath.Join(dir, "base"), "base\n")
	trunk := commitGit(t, dir, "base")
	runGit(t, dir, "switch", "-c", "layer-1")
	writeFile(t, filepath.Join(dir, "lower"), "lower\n")
	lowerRemote := commitGit(t, dir, "lower")
	runGit(t, dir, "switch", "-c", "layer-2")
	writeFile(t, filepath.Join(dir, "upper"), "upper\n")
	upperRemote := commitGit(t, dir, "upper")
	runGit(t, dir, "switch", "layer-1")
	writeFile(t, filepath.Join(dir, "local"), "unpublished\n")
	commitGit(t, dir, "lower local change")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", trunk)
	runGit(t, dir, "update-ref", "refs/remotes/origin/layer-1", lowerRemote)
	runGit(t, dir, "update-ref", "refs/remotes/origin/layer-2", upperRemote)

	state := &localstate.State{Remote: "origin", Trunk: "main", Layers: []localstate.Layer{
		{Branch: "layer-1", PullRequest: 1, ParentSHA: trunk, HeadSHA: lowerRemote, RemoteSHA: lowerRemote},
		{Branch: "layer-2", PullRequest: 2, ParentSHA: lowerRemote, HeadSHA: upperRemote, RemoteSHA: upperRemote},
	}}
	server := &api.PullRequestStack{Entries: []*api.PullRequestStackEntry{
		{PullRequest: &api.PullRequest{Index: 1}, HeadSHA: lowerRemote},
		{PullRequest: &api.PullRequest{Index: 2}, HeadSHA: upperRemote},
	}}
	app := &application{repo: gitx.Repo{Dir: dir}}
	needsRestack, needsReconciliation, err := app.updateSyncState(state, server, trunk)
	require.NoError(t, err)
	assert.Equal(t, []string{"layer-2"}, needsRestack)
	assert.Empty(t, needsReconciliation)
}

func TestSyncStateDoesNotRequireLandedLocalBranch(t *testing.T) {
	state := &localstate.State{Remote: "origin", Layers: []localstate.Layer{{Branch: "deleted-layer", PullRequest: 1}}}
	server := &api.PullRequestStack{Entries: []*api.PullRequestStackEntry{{PullRequest: &api.PullRequest{Index: 1}, LandedSHA: "landed"}}}
	app := &application{repo: gitx.Repo{Dir: t.TempDir()}}

	needsRestack, needsReconciliation, err := app.updateSyncState(state, server, "trunk")
	require.NoError(t, err)
	assert.Empty(t, needsRestack)
	assert.Empty(t, needsReconciliation)
	assert.Equal(t, "landed", state.Layers[0].LandedSHA)
}
