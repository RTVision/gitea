// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPullRequestMergeCheck(t *testing.T,
	targetFunc func(ctx context.Context, pr *issues_model.PullRequest) error,
	pr *issues_model.PullRequest,
	expectedStatus issues_model.PullRequestStatus,
	expectedConflictedFiles []string,
	expectedChangedProtectedFiles []string,
) {
	assert.NoError(t, pr.LoadIssue(t.Context()))
	assert.NoError(t, pr.LoadBaseRepo(t.Context()))
	assert.NoError(t, pr.LoadHeadRepo(t.Context()))
	pr.Status = issues_model.PullRequestStatusChecking
	pr.ConflictedFiles = []string{"unrelated-conflicted-file"}
	pr.ChangedProtectedFiles = []string{"unrelated-protected-file"}
	pr.MergeBase = ""
	pr.HeadCommitID = ""
	err := targetFunc(t.Context(), pr)
	require.NoError(t, err)
	assert.Equal(t, expectedStatus, pr.Status)
	assert.Equal(t, expectedConflictedFiles, pr.ConflictedFiles)
	assert.Equal(t, expectedChangedProtectedFiles, pr.ChangedProtectedFiles)
	assert.NotEmpty(t, pr.MergeBase)
	assert.NotEmpty(t, pr.HeadCommitID)
}

func TestPullRequestMergeable(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	t.Run("NoConflict-MergeTree", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByMergeTree, pr, issues_model.PullRequestStatusMergeable, nil, nil)
	})
	t.Run("NoConflict-TmpRepo", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByTmpRepo, pr, issues_model.PullRequestStatusMergeable, nil, nil)
	})

	pr.BaseBranch, pr.HeadBranch = "test-merge-tree-conflict-base", "test-merge-tree-conflict-head"
	conflictFiles := createConflictBranches(t, pr.BaseRepo, pr.BaseBranch, pr.HeadBranch)
	t.Run("Conflict-MergeTree", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByMergeTree, pr, issues_model.PullRequestStatusConflict, conflictFiles, nil)
	})
	t.Run("Conflict-TmpRepo", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByTmpRepo, pr, issues_model.PullRequestStatusConflict, conflictFiles, nil)
	})

	pr.BaseBranch, pr.HeadBranch = "test-merge-tree-empty-base", "test-merge-tree-empty-head"
	createEmptyBranches(t, pr.BaseRepo, pr.BaseBranch, pr.HeadBranch)
	t.Run("Empty-MergeTree", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByMergeTree, pr, issues_model.PullRequestStatusEmpty, nil, nil)
	})
	t.Run("Empty-TmpRepo", func(t *testing.T) {
		testPullRequestMergeCheck(t, checkPullRequestMergeableByTmpRepo, pr, issues_model.PullRequestStatusEmpty, nil, nil)
	})
}

func createConflictBranches(t *testing.T, repo gitrepo.RepositoryFacade, baseBranch, headBranch string) []string {
	conflictFile := "conflict.txt"
	stdin := fmt.Sprintf(
		`reset refs/heads/%[1]s
from refs/heads/master

commit refs/heads/%[1]s
mark :1
committer Test <test@example.com> 0 +0000
data 17
add conflict file
M 100644 inline %[3]s
data 4
base

commit refs/heads/%[1]s
mark :2
committer Test <test@example.com> 0 +0000
data 11
base change
from :1
M 100644 inline %[3]s
data 11
base change

reset refs/heads/%[2]s
from :1

commit refs/heads/%[2]s
mark :3
committer Test <test@example.com> 0 +0000
data 11
head change
from :1
M 100644 inline %[3]s
data 11
head change
`, baseBranch, headBranch, conflictFile)
	err := gitcmd.NewCommand("fast-import").WithRepo(repo).WithStdinBytes([]byte(stdin)).RunWithStderr(t.Context())
	require.NoError(t, err)
	return []string{conflictFile}
}

func createEmptyBranches(t *testing.T, repo gitrepo.RepositoryFacade, baseBranch, headBranch string) {
	emptyFile := "empty.txt"
	stdin := fmt.Sprintf(`reset refs/heads/%[1]s
from refs/heads/master

commit refs/heads/%[1]s
mark :1
committer Test <test@example.com> 0 +0000
data 14
add empty file
M 100644 inline %[3]s
data 4
base

reset refs/heads/%[2]s
from :1

commit refs/heads/%[2]s
mark :2
committer Test <test@example.com> 0 +0000
data 17
change empty file
from :1
M 100644 inline %[3]s
data 6
change

commit refs/heads/%[2]s
mark :3
committer Test <test@example.com> 0 +0000
data 17
revert empty file
from :2
M 100644 inline %[3]s
data 4
base
`, baseBranch, headBranch, emptyFile)
	err := gitcmd.NewCommand("fast-import").WithRepo(repo).WithStdinBytes([]byte(stdin)).RunWithStderr(t.Context())
	require.NoError(t, err)
}

func TestStackPatchCheckerPreservesComparisonBase(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	require.NoError(t, pr.LoadBaseRepo(ctx))
	require.NoError(t, pr.LoadHeadRepo(ctx))
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	require.NoError(t, unittest.SyncDirs(filepath.Join(setting.GetGiteaTestSourceRoot(), "tests/gitea-repositories-meta", pr.BaseRepo.FullName()+".git"), gitrepo.RepoLocalPath(pr.BaseRepo)))
	pr.BaseBranch, pr.HeadBranch = "stack-old-parent", "stack-new-head"
	input := `commit refs/heads/stack-old-parent
mark :1
committer Test <test@example.com> 0 +0000
data 1
a
from refs/heads/master
M 100644 inline parent.txt
data 6
parent

commit refs/heads/stack-squashed-parent
mark :2
committer Test <test@example.com> 0 +0000
data 1
s
from refs/heads/master
M 100644 inline parent.txt
data 6
parent

commit refs/heads/stack-new-head
mark :3
committer Test <test@example.com> 0 +0000
data 1
b
from :2
M 100644 inline layer.txt
data 5
layer
`
	require.NoError(t, gitcmd.NewCommand("fast-import").WithRepo(pr.BaseRepo).WithStdinBytes([]byte(input)).Run(ctx))
	boundary, err := git.GetFullCommitID(ctx, pr.BaseRepo, "refs/heads/stack-squashed-parent")
	require.NoError(t, err)
	head, err := git.GetFullCommitID(ctx, pr.BaseRepo, "refs/heads/stack-new-head")
	require.NoError(t, err)
	require.NoError(t, git.UpdateRef(ctx, pr.BaseRepo, pr.GetGitHeadRefName(), head))
	stack := &issues_model.PullRequestStack{RepoID: pr.BaseRepoID, TrunkBranch: "master", State: issues_model.StackStateOpen}
	require.NoError(t, db.Insert(ctx, stack))
	require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pr.ID, Position: 1, OldParentSHA: boundary, HeadSHA: head}))
	require.NoError(t, db.Insert(ctx, &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: pr.ID, BranchKey: issues_model.StackBranchKey(pr.HeadRepoID, pr.HeadBranch)}))
	require.NoError(t, db.Insert(ctx, &git_model.ProtectedBranch{RepoID: pr.BaseRepoID, RuleName: "master", ProtectedFilePatterns: "parent.txt;layer.txt"}))
	for name, check := range map[string]func(context.Context, *issues_model.PullRequest) error{"merge-tree": checkPullRequestMergeableByMergeTree, "temporary-repo": checkPullRequestMergeableByTmpRepo} {
		t.Run(name, func(t *testing.T) {
			pr.MergeBase, pr.Status = "", issues_model.PullRequestStatusChecking
			require.NoError(t, check(ctx, pr))
			assert.Equal(t, boundary, pr.MergeBase)
			assert.Equal(t, []string{"layer.txt"}, pr.ChangedProtectedFiles)
			assert.Equal(t, issues_model.PullRequestStatusMergeable, pr.Status)
			var diff bytes.Buffer
			require.NoError(t, DownloadDiffOrPatch(ctx, pr, &diff, false, false))
			assert.Contains(t, diff.String(), "layer.txt")
			assert.NotContains(t, diff.String(), "parent.txt")
		})
	}
}
