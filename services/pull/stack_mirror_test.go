// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPruneStackMirrorsSkipsReleasedBranches(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	tip, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/branch2")
	require.NoError(t, err)
	require.NoError(t, git.UpdateRef(t.Context(), repo, "refs/heads/stack/pr-99", tip))
	t.Cleanup(func() { _ = git.RemoveRef(t.Context(), repo, "refs/heads/stack/pr-99") })

	// an unstacked stack's mirror is an ordinary branch nobody targets, which pruning must leave alone
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "master", State: issues_model.StackStateUnstacked, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 1, Position: 1, MirrorBranch: "stack/pr-99", MirrorSHA: tip}
	require.NoError(t, db.Insert(t.Context(), entry))
	pruneStackMirrors(t.Context(), stack, 2)

	kept, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/stack/pr-99")
	require.NoError(t, err)
	assert.Equal(t, tip, kept)
	assert.Equal(t, "stack/pr-99", unittest.AssertExistsAndLoadBean(t, &issues_model.StackEntry{ID: entry.ID}).MirrorBranch)
}

func TestPruneStackMirrorsKeepsChangedBranches(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	tip, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/branch2")
	require.NoError(t, err)
	require.NoError(t, git.UpdateRef(t.Context(), repo, "refs/heads/stack/pr-98", tip))
	t.Cleanup(func() { _ = git.RemoveRef(t.Context(), repo, "refs/heads/stack/pr-98") })

	// an unused mirror whose tip the stack did not write is no longer managed but kept
	stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(t.Context(), stack))
	entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: 1, Position: 1, MirrorBranch: "stack/pr-98", MirrorSHA: "0123456789012345678901234567890123456789"}
	require.NoError(t, db.Insert(t.Context(), entry))
	pruneStackMirrors(t.Context(), stack, 2)

	kept, err := git.GetFullCommitID(t.Context(), repo, "refs/heads/stack/pr-98")
	require.NoError(t, err)
	assert.Equal(t, tip, kept)
	entry = unittest.AssertExistsAndLoadBean(t, &issues_model.StackEntry{ID: entry.ID})
	assert.Empty(t, entry.MirrorBranch)
	assert.Empty(t, entry.MirrorSHA)
}
