// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitea.dev/models/db"
	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/modules/util"
	notify_service "gitea.dev/services/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStackLifecycle(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	outsider := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Stack Test", "GIT_AUTHOR_EMAIL=stack@example.com", "GIT_COMMITTER_NAME=Stack Test", "GIT_COMMITTER_EMAIL=stack@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	run("checkout", "-b", "branch2")
	run("commit", "--allow-empty", "-m", "first")
	run("checkout", "-b", "pr-to-update")
	run("commit", "--allow-empty", "-m", "second")
	require.NoError(t, os.MkdirAll(filepath.Dir(gitrepo.RepoLocalPath(repo)), 0o755))
	run("clone", "--bare", work, gitrepo.RepoLocalPath(repo))
	_, err := db.GetEngine(ctx).ID(2).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
	require.NoError(t, err)
	opts := CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2}}
	_, err = CreateStack(ctx, outsider, repo, opts)
	require.ErrorIs(t, err, util.ErrPermissionDenied)
	_, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{5, 2}})
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	_, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2, 2}})
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	stack, err := CreateStack(ctx, owner, repo, opts)
	require.NoError(t, err)
	_, err = CreateStack(ctx, owner, repo, opts)
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	_, err = AppendStack(ctx, owner, stack.ID, 0, []int64{5})
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	stack, err = AppendStack(ctx, owner, stack.ID, 1, []int64{5})
	require.NoError(t, err)
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, entries[0].HeadSHA, entries[1].OldParentSHA)
	assert.Equal(t, int64(2), entries[1].ParentPullRequestID)
	upper := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 5})
	branch, err := issues_model.ResolvePullRequestPolicyBranch(ctx, upper)
	require.NoError(t, err)
	assert.Equal(t, "release", branch)
	assert.Equal(t, "branch2", upper.BaseBranch)
	run("checkout", "branch2")
	run("commit", "--amend", "--allow-empty", "-m", "first revised")
	newParent := run("rev-parse", "HEAD")
	run("rebase", "--onto", newParent, entries[1].OldParentSHA, "pr-to-update")
	expected := []StackHeadExpectation{
		{PullRequestID: 2, HeadSHA: newParent, ParentSHA: entries[0].OldParentSHA},
		{PullRequestID: 5, HeadSHA: run("rev-parse", "HEAD"), ParentSHA: newParent},
	}
	run("push", "--force-with-lease=refs/heads/branch2:"+entries[0].HeadSHA, "--force-with-lease=refs/heads/pr-to-update:"+entries[1].HeadSHA, gitrepo.RepoLocalPath(repo), "branch2", "pr-to-update")
	_, err = SynchronizeStack(ctx, outsider, stack.ID, 2, expected)
	require.ErrorIs(t, err, util.ErrPermissionDenied)
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected[:1])
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	wrong := append([]StackHeadExpectation(nil), expected...)
	wrong[1].ParentSHA = entries[1].OldParentSHA
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, wrong)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	collector := &stackSyncCollector{heads: make(map[int64][2]string)}
	notify_service.RegisterNotifier(collector)
	t.Cleanup(func() { collector.heads = nil })
	stack, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected)
	require.NoError(t, err)
	assert.EqualValues(t, 3, stack.Revision)
	assert.Equal(t, [2]string{entries[0].HeadSHA, expected[0].HeadSHA}, collector.heads[2])
	assert.Equal(t, [2]string{entries[1].HeadSHA, expected[1].HeadSHA}, collector.heads[5])
	synced, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Equal(t, newParent, synced[1].OldParentSHA)
	assert.Equal(t, expected[1].HeadSHA, synced[1].HeadSHA)
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, expected)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	require.ErrorIs(t, Unstack(ctx, outsider, stack.ID, 3), util.ErrPermissionDenied)
	require.NoError(t, Unstack(ctx, owner, stack.ID, 3))
	membership, err := issues_model.GetPullRequestStack(ctx, 5)
	require.NoError(t, err)
	assert.Nil(t, membership)
	historical, err := issues_model.GetStackEntries(ctx, stack.ID)
	require.NoError(t, err)
	assert.Len(t, historical, 2)

	stack, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: []int64{2, 5}})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).ID(2).Cols("has_merged").Update(&issues_model.PullRequest{HasMerged: true})
	require.NoError(t, err)
	require.NoError(t, Unstack(ctx, owner, stack.ID, 1))
	membership, err = issues_model.GetPullRequestStack(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, membership)
	assert.Equal(t, stack.ID, membership.ID)
	membership, err = issues_model.GetPullRequestStack(ctx, 5)
	require.NoError(t, err)
	assert.Nil(t, membership)
}

func TestStackInsertLayer(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.Repository.PullRequest.EnableStacks, true)()
	defer test.MockVariableValue(&setting.RepoRootPath, t.TempDir())()
	ctx := t.Context()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	work := t.TempDir()
	run := stackTestGit(t, work)
	branch := func(name, from string) {
		run("checkout", "-b", name, from)
		run("commit", "--allow-empty", "-m", name)
	}
	run("init", "--initial-branch=release")
	run("commit", "--allow-empty", "-m", "trunk")
	branch("under", "release")
	branch("branch2", "under")
	branch("pr-to-update", "branch2")
	branch("upper", "pr-to-update")
	branch("inserted", "pr-to-update")
	branch("bottom", "release")
	branch("fresh", "release")
	bare := gitrepo.RepoLocalPath(repo)
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	run("clone", "--bare", work, bare)
	bareRun := stackTestGit(t, bare)
	newPull := func(head, base string) *issues_model.PullRequest {
		pr := &issues_model.PullRequest{HeadRepoID: repo.ID, BaseRepoID: repo.ID, HeadBranch: head, BaseBranch: base}
		require.NoError(t, issues_model.NewPullRequest(ctx, repo, &issues_model.Issue{RepoID: repo.ID, PosterID: owner.ID, Poster: owner, Title: head}, nil, nil, pr))
		return pr
	}
	_, err := db.GetEngine(ctx).ID(2).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "release"})
	require.NoError(t, err)
	lower := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2})
	upper := newPull("upper", "pr-to-update")
	inserted := newPull("inserted", "pr-to-update")
	bottom := newPull("bottom", "release")
	under := newPull("under", "release")
	for _, pr := range []*issues_model.PullRequest{lower, upper} { // retargeted layers
		bareRun("update-ref", pr.GetGitHeadRefName(), "refs/heads/"+pr.HeadBranch)
	}
	for _, name := range []string{"inserted", "bottom", "fresh"} { // retarget destinations
		require.NoError(t, db.Insert(ctx, &git_model.Branch{RepoID: repo.ID, Name: name, CommitID: bareRun("rev-parse", name), PusherID: owner.ID}))
	}
	chain := []int64{2, 5, upper.ID}

	stack, err := CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", PullRequestIDs: chain})
	require.NoError(t, err)
	_, err = InsertStackLayer(ctx, owner, stack.ID, 1, inserted.ID)
	require.ErrorIs(t, err, issues_model.ErrInvalidStack)
	assert.ErrorContains(t, err, fmt.Sprintf("#%d must contain the current head of #%d (inserted); rebase upper onto it first", upper.Index, inserted.Index))
	originalUpper := bareRun("rev-parse", "upper")
	run("rebase", "--onto", "inserted", "pr-to-update", "upper")
	run("push", "-f", bare, "upper")
	_, err = InsertStackLayer(ctx, owner, stack.ID, 1, inserted.ID)
	require.NoError(t, err)
	expect := func(id int64, head, parent string) StackHeadExpectation {
		return StackHeadExpectation{PullRequestID: id, HeadSHA: bareRun("rev-parse", head), ParentSHA: bareRun("rev-parse", parent)}
	}
	_, err = SynchronizeStack(ctx, owner, stack.ID, 2, []StackHeadExpectation{expect(2, "branch2", "release"), expect(5, "pr-to-update", "branch2"), expect(inserted.ID, "inserted", "pr-to-update"), expect(upper.ID, "upper", "inserted")})
	require.NoError(t, err, "layers restacked before the insert synchronize afterwards")
	require.NoError(t, Unstack(ctx, owner, stack.ID, 3))
	run("push", "-f", bare, originalUpper+":refs/heads/upper")
	_, err = db.GetEngine(ctx).ID(upper.ID).Cols("base_branch").Update(&issues_model.PullRequest{BaseBranch: "pr-to-update"})
	require.NoError(t, err)

	stack, err = CreateStack(ctx, owner, repo, CreateStackOptions{TrunkBranch: "release", Mode: issues_model.StackModeMerge, PullRequestIDs: chain})
	require.NoError(t, err)
	candidates, err := StackInsertCandidates(ctx, stack, 0)
	require.NoError(t, err)
	offered := map[int64]int64{}
	for _, candidate := range candidates {
		offered[candidate.Pull.ID] = 0
		if candidate.After != nil {
			offered[candidate.Pull.ID] = candidate.After.ID
		}
	}
	assert.Equal(t, map[int64]int64{under.ID: 0, inserted.ID: 5}, offered, "a trunk pull request unrelated to the bottom layer isn't offered")
	_, err = InsertStackLayer(ctx, owner, stack.ID, 0, inserted.ID)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	_, err = InsertStackLayer(ctx, owner, stack.ID, 1, upper.ID)
	require.ErrorContains(t, err, "already belongs to stack")
	_, err = InsertStackLayer(ctx, owner, stack.ID, 1, newPull("stray", "elsewhere").ID)
	require.ErrorContains(t, err, "must target release or the branch of an open layer")
	_, err = db.GetEngine(ctx).ID(stack.ID).Cols("active_operation_id").Update(&issues_model.PullRequestStack{ActiveOperationID: 1})
	require.NoError(t, err)
	_, err = InsertStackLayer(ctx, owner, stack.ID, 1, inserted.ID)
	require.ErrorIs(t, err, issues_model.ErrStackRevision)
	_, err = db.GetEngine(ctx).ID(stack.ID).Cols("active_operation_id").Update(&issues_model.PullRequestStack{})
	require.NoError(t, err)

	assertChain := func(pullIDs ...int64) {
		t.Helper()
		entries, err := issues_model.GetStackEntries(ctx, stack.ID)
		require.NoError(t, err)
		require.Len(t, entries, len(pullIDs))
		for i, entry := range entries {
			assert.Equal(t, i+1, entry.Position)
			assert.Equal(t, pullIDs[i], entry.PullRequestID)
			if i > 0 {
				assert.Equal(t, pullIDs[i-1], entry.ParentPullRequestID)
			}
		}
	}
	inserted2, err := InsertStackLayer(ctx, owner, stack.ID, 1, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, stack.ID, inserted2.ID)
	assert.EqualValues(t, 2, inserted2.Revision)
	assertChain(2, 5, inserted.ID, upper.ID)
	assert.Equal(t, "inserted", unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: upper.ID}).BaseBranch, "a merge-mode layer may be behind the inserted parent")

	_, err = InsertStackLayer(ctx, owner, stack.ID, 2, bottom.ID)
	require.NoError(t, err)
	assertChain(bottom.ID, 2, 5, inserted.ID, upper.ID)
	assert.Equal(t, "bottom", unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2}).BaseBranch)

	// A bottom layer whose base still names a landed branch is retargeted by position.
	_, err = db.GetEngine(ctx).ID(bottom.ID).Cols("has_merged").Update(&issues_model.PullRequest{HasMerged: true})
	require.NoError(t, err)
	_, err = InsertStackLayer(ctx, owner, stack.ID, 3, newPull("fresh", "release").ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh", unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: 2}).BaseBranch)

	_, err = db.GetEngine(ctx).ID(5).Cols("has_merged").Update(&issues_model.PullRequest{HasMerged: true})
	require.NoError(t, err)
	_, err = InsertStackLayer(ctx, owner, stack.ID, 4, newPull("late", "branch2").ID)
	require.ErrorContains(t, err, "would be placed below landed pull request")
}

type stackSyncCollector struct {
	notify_service.NullNotifier
	heads map[int64][2]string
}

func (n *stackSyncCollector) PullRequestSynchronized(_ context.Context, _ *user_model.User, pr *issues_model.PullRequest, before, after string) {
	if n.heads != nil {
		n.heads[pr.ID] = [2]string{before, after}
	}
}

func TestSuggestStackChain(t *testing.T) {
	release := &issues_model.PullRequest{Index: 10, HeadBranch: "release", BaseBranch: "main"}
	lower := &issues_model.PullRequest{Index: 11, HeadBranch: "lower", BaseBranch: "release"}
	middle := &issues_model.PullRequest{Index: 12, HeadBranch: "middle", BaseBranch: "lower"}
	upper := &issues_model.PullRequest{Index: 13, HeadBranch: "upper", BaseBranch: "middle"}
	chain := []*issues_model.PullRequest{upper, release, middle, lower}
	with := func(extra ...*issues_model.PullRequest) []*issues_model.PullRequest {
		return append(slices.Clone(chain), extra...)
	}
	onRelease := &issues_model.PullRequest{Index: 14, HeadBranch: "other", BaseBranch: "release"}
	onMiddle := &issues_model.PullRequest{Index: 15, HeadBranch: "sibling", BaseBranch: "middle"}
	duplicate := &issues_model.PullRequest{Index: 16, HeadBranch: "lower", BaseBranch: "main"}
	cycle := &issues_model.PullRequest{Index: 17, HeadBranch: "main", BaseBranch: "upper"}
	for _, tc := range []struct {
		name          string
		candidates    []*issues_model.PullRequest
		top           int64
		defaultBranch string
		chain         []int64
		start         int
	}{
		{"stops at the default branch", chain, 13, "release", []int64{11, 12, 13}, 0},
		{"walks down to the trunk", chain, 12, "main", []int64{10, 11, 12}, 0},
		{"starts above a branch several pull requests build on", with(onRelease), 13, "main", []int64{10, 11, 12, 13}, 1},
		{"starts above the highest shared branch", with(onRelease, onMiddle), 13, "main", []int64{10, 11, 12, 13}, 3},
		{"unknown top", chain, 99, "main", nil, 0},
		{"stops at an ambiguous head branch", with(duplicate), 13, "main", []int64{12, 13}, 0},
		{"top sharing its head branch", with(duplicate), 11, "main", nil, 0},
		{"stops before repeating a layer", with(cycle), 13, "", []int64{10, 11, 12, 13}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, start := SuggestStackChain(tc.candidates, tc.top, tc.defaultBranch)
			var indexes []int64
			for _, pr := range got {
				indexes = append(indexes, pr.Index)
			}
			assert.Equal(t, tc.chain, indexes)
			assert.Equal(t, tc.start, start)
		})
	}
}
