// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"fmt"
	"net/http"

	issues_model "gitea.dev/models/issues"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
	"gitea.dev/services/convert"
	pull_service "gitea.dev/services/pull"
	"gitea.dev/services/repository/files"
)

// RevertPullRequest opens a pull request reverting a merged pull request.
func RevertPullRequest(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/pulls/{index}/revert repository repoRevertPullRequest
	// ---
	// summary: Open a pull request reverting a merged pull request
	// description: Requires repository code write access. Creates a new branch; never modifies the target branch. Conflicts or an unknown historical merge range return 409.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   type: string
	//   required: true
	// - name: index
	//   in: path
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "201":
	//     "$ref": "#/responses/PullRequest"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/error"
	//   "423":
	//     "$ref": "#/responses/repoArchivedError"
	repo := ctx.Repo.Repository
	pr, err := issues_model.GetPullRequestByIndex(ctx, repo.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrPullRequestNotExist(err) {
			ctx.APIErrorNotFound()
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	if err := pr.LoadIssue(ctx); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	if user_model.IsUserBlockedBy(ctx, ctx.Doer, repo.OwnerID) {
		ctx.APIError(http.StatusForbidden, user_model.ErrBlockedUser.Error())
		return
	}
	branch := fmt.Sprintf("revert-%d-%s", pr.Index, util.CryptoRandomString(12))
	title := util.TruncateRunes(fmt.Sprintf("Revert %q", pr.Issue.Title), 255)
	body := fmt.Sprintf("Reverts #%d.", pr.Index)
	opts := &files.ApplyDiffPatchOptions{OldBranch: pr.BaseBranch, NewBranch: branch, Message: title + "\n\n" + body}
	if _, err := files.RevertPullRequest(ctx, repo, ctx.Doer, pr, opts); err != nil {
		if errors.Is(err, util.ErrPermissionDenied) {
			ctx.APIError(http.StatusForbidden, err.Error())
		} else if errors.Is(err, files.ErrPullRevertUnavailable) || files.IsErrCommitIDDoesNotMatch(err) {
			ctx.APIError(http.StatusConflict, err.Error())
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	reverted := &issues_model.PullRequest{HeadRepoID: repo.ID, BaseRepoID: repo.ID, HeadRepo: repo, BaseRepo: repo, HeadBranch: branch, BaseBranch: pr.BaseBranch, MergeBase: opts.LastCommitID, Type: issues_model.PullRequestGitea}
	issue := &issues_model.Issue{RepoID: repo.ID, Title: title, PosterID: ctx.Doer.ID, Poster: ctx.Doer, IsPull: true, Content: body}
	if err := pull_service.NewPullRequest(ctx, &pull_service.NewPullRequestOptions{Repo: repo, Issue: issue, PullRequest: reverted}); err != nil {
		ctx.APIError(http.StatusInternalServerError, fmt.Sprintf("Revert branch %s was created, but opening its pull request failed: %v", branch, err))
		return
	}
	ctx.JSON(http.StatusCreated, convert.ToAPIPullRequest(ctx, reverted, ctx.Doer))
}
