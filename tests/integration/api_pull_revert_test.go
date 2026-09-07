// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	pull_service "gitea.dev/services/pull"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIPullRevert(t *testing.T) {
	for _, style := range []repo_model.MergeStyle{repo_model.MergeStyleMerge, repo_model.MergeStyleSquash, repo_model.MergeStyleRebase, repo_model.MergeStyleRebaseMerge, repo_model.MergeStyleFastForwardOnly} {
		t.Run(string(style), func(t *testing.T) {
			onGiteaRun(t, func(t *testing.T, _ *url.URL) {
				session := loginUser(t, "user2")
				token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)
				repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
				gitRepo, err := git.OpenRepository(t.Context(), repo)
				require.NoError(t, err)
				defer gitRepo.Close()
				before, err := gitRepo.GetBranchCommit(t.Context(), "master")
				require.NoError(t, err)
				testEditFileToNewBranch(t, session, "user2", "repo1", "master", "to-revert", "README.md", "first change\n")
				testEditFile(t, session, "user2", "repo1", "to-revert", "README.md", "second change\n")
				session.MakeRequest(t, NewRequestWithJSON(t, "POST", "/api/v1/repos/user2/repo1/contents/data.bin", &api.CreateFileOptions{
					FileOptions:   api.FileOptions{BranchName: "to-revert"},
					ContentBase64: base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255}),
				}).AddTokenAuth(token), http.StatusCreated)
				resp := session.MakeRequest(t, NewRequestWithJSON(t, "POST", "/api/v1/repos/user2/repo1/pulls", &api.CreatePullRequestOption{Base: "master", Head: "to-revert", Title: "Revert all changes"}).AddTokenAuth(token), http.StatusCreated)
				pr := DecodeJSON(t, resp, &api.PullRequest{})
				endpoint := fmt.Sprintf("/api/v1/repos/user2/repo1/pulls/%d/revert", pr.Index)
				session.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(token), http.StatusConflict)
				testPullMerge(t, session, "user2", "repo1", strconv.FormatInt(pr.Index, 10), MergeOptions{Style: style})
				merged := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: pr.ID})
				assert.Equal(t, before.ID.String(), merged.MergedBaseCommitID)
				if style == repo_model.MergeStyleSquash || style == repo_model.MergeStyleRebase {
					actor := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: merged.MergerID})
					candidate := merged.MergedCommitID
					_, err := db.GetEngine(t.Context()).ID(merged.IssueID).Cols("is_closed", "closed_unix").Update(&issues_model.Issue{IsClosed: false})
					require.NoError(t, err)
					merged.HasMerged = false
					merged.MergedCommitID, merged.MergedBaseCommitID = "", ""
					require.NoError(t, merged.UpdateCols(t.Context(), "has_merged", "merged_commit_id", "merged_base_commit_id"))
					stack := &issues_model.PullRequestStack{RepoID: repo.ID, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
					require.NoError(t, db.Insert(t.Context(), stack))
					entry := &issues_model.StackEntry{StackID: stack.ID, PullRequestID: merged.ID, Position: 1, HeadSHA: pr.Head.Sha, OldParentSHA: before.ID.String()}
					require.NoError(t, db.Insert(t.Context(), entry))
					require.NoError(t, db.Insert(t.Context(), &issues_model.StackBranchClaim{StackID: stack.ID, PullRequestID: merged.ID, BranchKey: issues_model.StackBranchKey(repo.ID, merged.HeadBranch)}))
					journal, err := json.Marshal(map[string]any{"stage": "confirm", "layers": []map[string]any{{
						"entry_id": entry.ID, "pull_id": merged.ID, "position": 1, "head_branch": merged.HeadBranch,
						"expected_head": entry.HeadSHA, "old_parent": entry.OldParentSHA, "phase": "merging",
						"landing_base_sha": before.ID.String(), "merge_candidate_sha": candidate,
					}}})
					require.NoError(t, err)
					op := &issues_model.StackOperation{StackID: stack.ID, ActorID: merged.MergerID, ExpectedRevision: 1, Kind: "land", MergeStyle: string(style), ThroughPosition: 1, State: "blocked", JournalJSON: string(journal)}
					require.NoError(t, issues_model.CreateStackOperation(t.Context(), op))
					require.NoError(t, pull_service.ResumeStackOperation(t.Context(), actor, op.ID))
					op = waitForStackOperation(t, op.ID, "completed", "blocked")
					require.Equal(t, "completed", op.State, op.LastError)
					merged = unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: merged.ID})
					require.True(t, merged.HasMerged)
					assert.Equal(t, before.ID.String(), merged.MergedBaseCommitID)
				}

				testCreateFile(t, session, "user2", "repo1", "master", "master", "later.txt", "keep this later change\n")
				target, err := gitRepo.GetBranchCommit(t.Context(), "master")
				require.NoError(t, err)
				reader := loginUser(t, "user4")
				readerToken := getTokenForLoggedInUser(t, reader, auth_model.AccessTokenScopeWriteRepository)
				reader.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(readerToken), http.StatusForbidden)
				resp = session.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(token), http.StatusCreated)
				reverted := DecodeJSON(t, resp, &api.PullRequest{})
				assert.Equal(t, "master", reverted.Base.Name)
				assert.NotEqual(t, "master", reverted.Head.Name)
				afterTarget, err := gitRepo.GetBranchCommitID(t.Context(), "master")
				require.NoError(t, err)
				assert.Equal(t, target.ID.String(), afterTarget)
				result, err := gitRepo.GetBranchCommit(t.Context(), reverted.Head.Name)
				require.NoError(t, err)
				originalFile, err := before.GetTreeEntryByPath(t.Context(), gitRepo, "README.md")
				require.NoError(t, err)
				revertedFile, err := result.GetTreeEntryByPath(t.Context(), gitRepo, "README.md")
				require.NoError(t, err)
				assert.Equal(t, originalFile.ID, revertedFile.ID)
				_, err = result.GetTreeEntryByPath(t.Context(), gitRepo, "data.bin")
				assert.True(t, git.IsErrNotExist(err))
				_, err = result.GetTreeEntryByPath(t.Context(), gitRepo, "later.txt")
				require.NoError(t, err)
				if style == repo_model.MergeStyleMerge {
					testEditFile(t, session, "user2", "repo1", "master", "README.md", "a conflicting later edit\n")
					session.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(token), http.StatusConflict)
				}
				// An old single-parent merge without recorded metadata must not guess its range.
				if style == repo_model.MergeStyleRebase || style == repo_model.MergeStyleSquash || style == repo_model.MergeStyleFastForwardOnly {
					merged.MergedBaseCommitID = ""
					require.NoError(t, merged.UpdateCols(t.Context(), "merged_base_commit_id"))
					session.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(token), http.StatusConflict)
				}
				merged.MergedBaseCommitID = ""
				merged.Status = issues_model.PullRequestStatusManuallyMerged
				require.NoError(t, merged.UpdateCols(t.Context(), "merged_base_commit_id", "status"))
				session.MakeRequest(t, NewRequest(t, "POST", endpoint).AddTokenAuth(token), http.StatusConflict)
			})
		})
	}
}
