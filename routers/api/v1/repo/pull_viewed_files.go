// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"
	"slices"
	"strings"

	issues_model "gitea.dev/models/issues"
	pull_model "gitea.dev/models/pull"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitcmd"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/web"
	"gitea.dev/services/context"
	"gitea.dev/services/gitdiff"
)

func pullRequestViewedDiff(ctx *context.APIContext) (*issues_model.PullRequest, string, *gitdiff.Diff, bool) {
	pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		ctx.APIErrorAuto(err)
		return nil, "", nil, false
	}
	head, err := ctx.Repo.GitRepo.GetRefCommitID(ctx, pr.GetGitHeadRefName())
	if err != nil {
		ctx.APIErrorInternal(err)
		return nil, "", nil, false
	}
	base := pr.MergeBase
	if !pr.HasMerged {
		base, err = git.MergeBase(ctx, ctx.Repo.Repository, pr.BaseBranch, head)
		if err != nil {
			ctx.APIErrorInternal(err)
			return nil, "", nil, false
		}
	}
	names, _, err := gitcmd.NewCommand("diff", "--name-only", "-z").
		AddOptionFormat("--find-renames=%s", setting.Git.DiffRenameSimilarityThreshold).
		AddDynamicArguments(base, head).AddDashesAndList().WithRepo(ctx.Repo.GitRepo).RunStdString(ctx)
	if err != nil {
		ctx.APIErrorInternal(err)
		return nil, "", nil, false
	}
	paths := strings.Split(strings.TrimSuffix(names, "\x00"), "\x00")
	if names == "" {
		paths = nil
	}
	diff := &gitdiff.Diff{Files: make([]*gitdiff.DiffFile, 0, len(paths))}
	for _, path := range paths {
		diff.Files = append(diff.Files, &gitdiff.DiffFile{Name: path})
	}
	if _, err = gitdiff.SyncUserSpecificDiff(ctx, ctx.Doer.ID, pr, ctx.Repo.GitRepo, diff, &gitdiff.DiffOptions{AfterCommitID: head}); err != nil {
		ctx.APIErrorInternal(err)
		return nil, "", nil, false
	}
	return pr, head, diff, true
}

func GetPullRequestViewedFiles(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/pulls/{index}/viewed-files repository repoGetPullRequestViewedFiles
	// ---
	// summary: Get the authenticated user's viewed files for a pull request
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
	//   "200":
	//     "$ref": "#/responses/PullRequestViewedFiles"
	//   "404":
	//     "$ref": "#/responses/notFound"
	_, head, diff, ok := pullRequestViewedDiff(ctx)
	if !ok {
		return
	}
	result := api.PullRequestViewedFiles{HeadSHA: head, Files: make([]api.PullRequestViewedFile, 0, len(diff.Files))}
	for _, file := range diff.Files {
		result.Files = append(result.Files, api.PullRequestViewedFile{Path: file.Name, Viewed: file.IsViewed})
	}
	ctx.JSON(http.StatusOK, result)
}

func UpdatePullRequestViewedFiles(ctx *context.APIContext) {
	// swagger:operation PUT /repos/{owner}/{repo}/pulls/{index}/viewed-files repository repoUpdatePullRequestViewedFiles
	// ---
	// summary: Mark or unmark files as viewed for the authenticated user
	// consumes:
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
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/UpdatePullRequestViewedFilesOptions"
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "409":
	//     description: The pull request head changed
	//   "422":
	//     description: Invalid file selection
	//   "404":
	//     "$ref": "#/responses/notFound"
	form := web.GetForm[*api.UpdatePullRequestViewedFilesOptions](ctx)
	if len(form.Files) == 0 || len(form.Files) > 1000 {
		ctx.APIError(http.StatusUnprocessableEntity, "provide between 1 and 1000 files")
		return
	}
	pr, head, diff, ok := pullRequestViewedDiff(ctx)
	if !ok {
		return
	}
	if form.HeadSHA != head {
		ctx.APIError(http.StatusConflict, "pull request head changed; refresh the diff")
		return
	}
	files := make(map[string]pull_model.ViewedState, len(form.Files))
	for path, viewed := range form.Files {
		if !slices.ContainsFunc(diff.Files, func(file *gitdiff.DiffFile) bool { return file.Name == path }) {
			ctx.APIError(http.StatusUnprocessableEntity, "file is not in the pull request diff")
			return
		}
		state := pull_model.Unviewed
		if viewed {
			state = pull_model.Viewed
		}
		files[path] = state
	}
	if _, err := pull_model.UpdateReviewState(ctx, ctx.Doer.ID, pr.ID, head, files); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}
