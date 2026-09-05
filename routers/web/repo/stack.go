// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/svg"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
	pull_service "gitea.dev/services/pull"
)

const (
	tplPullStacks      templates.TplName = "repo/stack/list"
	tplPullStack       templates.TplName = "repo/stack/view"
	tplPullStackNew    templates.TplName = "repo/stack/new"
	tplPullStackStatus templates.TplName = "repo/stack/status"
)

type pullStackEntryData struct {
	Entry     *issues_model.StackEntry
	Pull      *issues_model.PullRequest
	Readiness string
}

type pullStackData struct {
	Stack      *issues_model.PullRequestStack
	Entries    []*pullStackEntryData
	Operation  *issues_model.StackOperation
	Operations []*issues_model.StackOperation
}

func canManagePullStack(ctx *context.Context) bool {
	return ctx.Repo.Repository.CanContentChange() && ctx.Repo.Permission.CanWrite(unit.TypeCode)
}

func stackEntryReadiness(ctx *context.Context, pr *issues_model.PullRequest) string {
	if pr.HasMerged || pr.Issue.IsClosed {
		return ""
	}
	if pr.IsWorkInProgress(ctx) {
		return string(ctx.Tr("repo.pulls.cannot_merge_work_in_progress"))
	}
	if pr.IsChecking() {
		return string(ctx.Tr("repo.pulls.is_checking"))
	}
	if pr.IsFilesConflicted() {
		return string(ctx.Tr("repo.pulls.files_conflicted"))
	}
	if err := pull_service.CheckPullBranchProtections(ctx, pr, false); err != nil {
		return err.Error()
	}
	return ""
}

func loadPullStackData(ctx *context.Context, stack *issues_model.PullRequestStack) (*pullStackData, error) {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	data := &pullStackData{Stack: stack, Entries: make([]*pullStackEntryData, 0, len(entries))}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		if err := pr.Issue.LoadRepo(ctx); err != nil {
			return nil, err
		}
		data.Entries = append(data.Entries, &pullStackEntryData{Entry: entry, Pull: pr, Readiness: stackEntryReadiness(ctx, pr)})
	}
	data.Operations, err = issues_model.GetStackOperations(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	if stack.ActiveOperationID != 0 {
		data.Operation, err = issues_model.GetStackOperation(ctx, stack.ActiveOperationID)
		if err != nil {
			return nil, err
		}
	} else if len(data.Operations) > 0 {
		data.Operation = data.Operations[0]
	}
	return data, nil
}

func attachPullStackData(ctx *context.Context, issue *issues_model.Issue) {
	if issue.PullRequest == nil {
		return
	}
	stack, err := issues_model.GetPullRequestStack(ctx, issue.PullRequest.ID)
	if err != nil {
		ctx.ServerError("GetPullRequestStack", err)
		return
	}
	if stack == nil {
		return
	}
	data, err := loadPullStackData(ctx, stack)
	if err != nil {
		ctx.ServerError("loadPullStackData", err)
		return
	}
	ctx.Data["PullStackData"] = data
	if mergeData, ok := ctx.Data["PullMergeBoxData"].(*pullMergeBoxData); ok && !issue.PullRequest.HasMerged && !issue.IsClosed {
		mergeData.MergeFormProps = nil
		mergeData.ShowUpdatePullInfo = false
		mergeData.InfoSections = append([]*pullInfoSection{{InfoItems: []*pullMergeBoxInfoItem{{
			SvgIconHTML: svg.RenderHTML("octicon-info"),
			InfoHTML:    ctx.Locale.Tr("repo.pulls.stack_merge_disabled", stack.ID),
		}}}}, mergeData.InfoSections...)
	}
}

func PullStacks(ctx *context.Context) {
	page := max(ctx.FormInt("page"), 1)
	stacks, count, err := issues_model.ListStacks(ctx, ctx.Repo.Repository.ID, db.ListOptions{Page: page, PageSize: setting.UI.IssuePagingNum})
	if err != nil {
		ctx.ServerError("ListStacks", err)
		return
	}
	data := make([]*pullStackData, 0, len(stacks))
	for _, stack := range stacks {
		stackData, err := loadPullStackData(ctx, stack)
		if err != nil {
			ctx.ServerError("loadPullStackData", err)
			return
		}
		data = append(data, stackData)
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.stacks")
	ctx.Data["Stacks"] = data
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(count).PerPageLimit(setting.UI.IssuePagingNum).CurPage(page).Build()
	ctx.Data["CanCreateStack"] = setting.Repository.PullRequest.EnableStacks && canManagePullStack(ctx)
	ctx.HTML(http.StatusOK, tplPullStacks)
}

func getPullStack(ctx *context.Context) *issues_model.PullRequestStack {
	stack, err := issues_model.GetStackByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.NotFoundOrServerError("GetStackByID", func(err error) bool { return errors.Is(err, issues_model.ErrStackNotExist) }, err)
		return nil
	}
	if stack.RepoID != ctx.Repo.Repository.ID {
		ctx.NotFound(nil)
		return nil
	}
	return stack
}

func PullStack(ctx *context.Context) {
	stack := getPullStack(ctx)
	if ctx.Written() {
		return
	}
	data, err := loadPullStackData(ctx, stack)
	if err != nil {
		ctx.ServerError("loadPullStackData", err)
		return
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.stack_number", stack.ID)
	ctx.Data["PullStackData"] = data
	ctx.Data["CanManageStack"] = canManagePullStack(ctx)
	ctx.HTML(http.StatusOK, tplPullStack)
}

func pullStackNumbers(ctx *context.Context) ([]int64, error) {
	values := strings.FieldsFunc(ctx.FormString("pulls"), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
	if len(values) == 0 {
		return nil, issues_model.ErrInvalidStack
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		index, err := strconv.ParseInt(strings.TrimPrefix(value, "#"), 10, 64)
		if err != nil {
			return nil, issues_model.ErrInvalidStack
		}
		pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, index)
		if err != nil {
			return nil, err
		}
		ids = append(ids, pr.ID)
	}
	return ids, nil
}

func PullStackNew(ctx *context.Context) {
	if !setting.Repository.PullRequest.EnableStacks || !canManagePullStack(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.new_stack")
	ctx.Data["DefaultBranch"] = ctx.Repo.Repository.DefaultBranch
	ctx.Data["Pulls"] = ctx.Req.URL.Query().Get("pull")
	ctx.HTML(http.StatusOK, tplPullStackNew)
}

func PullStackNewPost(ctx *context.Context) {
	if !setting.Repository.PullRequest.EnableStacks || !canManagePullStack(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	ids, err := pullStackNumbers(ctx)
	if err == nil {
		_, err = pull_service.CreateStack(ctx, ctx.Doer, ctx.Repo.Repository, pull_service.CreateStackOptions{TrunkBranch: ctx.FormTrim("trunk"), PullRequestIDs: ids})
	}
	if err != nil {
		ctx.Flash.Error(ctx.Tr("repo.pulls.stack_create_error", err))
		ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks/new")
		return
	}
	ctx.Flash.Success(ctx.Tr("repo.pulls.stack_created"))
	ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks")
}

func PullStackAction(ctx *context.Context) {
	stack := getPullStack(ctx)
	if ctx.Written() {
		return
	}
	if !canManagePullStack(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	revision := ctx.FormInt64("stack_version")
	action := ctx.PathParam("action")
	var err error
	switch action {
	case "append":
		if !setting.Repository.PullRequest.EnableStacks {
			ctx.HTTPError(http.StatusForbidden)
			return
		}
		var ids []int64
		ids, err = pullStackNumbers(ctx)
		if err == nil {
			_, err = pull_service.AppendStack(ctx, ctx.Doer, stack.ID, revision, ids)
		}
	case "unstack":
		err = pull_service.Unstack(ctx, ctx.Doer, stack.ID, revision)
	case "land", "rebase":
		through := ctx.FormInt("through")
		style := repo_model.MergeStyle(ctx.FormTrim("merge_style"))
		if style == "" {
			style = repo_model.MergeStyleMerge
		}
		_, err = pull_service.StartStackOperation(ctx, ctx.Doer, pull_service.StackOperationOptions{StackID: stack.ID, ExpectedRevision: revision, ThroughPosition: through, Kind: action, MergeStyle: style})
	case "retry":
		err = pull_service.ResumeStackOperation(ctx, ctx.Doer, ctx.FormInt64("operation"))
	case "cancel":
		err = pull_service.CancelStackOperation(ctx, ctx.Doer, ctx.FormInt64("operation"))
	default:
		ctx.NotFound(nil)
		return
	}
	if err != nil {
		if errors.Is(err, issues_model.ErrStackRevision) {
			ctx.Flash.Warning(ctx.Tr("repo.pulls.stack_changed"))
		} else {
			ctx.Flash.Error(fmt.Sprintf("%v", err))
		}
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks/" + strconv.FormatInt(stack.ID, 10))
}

func PullStackStatus(ctx *context.Context) {
	stack := getPullStack(ctx)
	if ctx.Written() {
		return
	}
	data, err := loadPullStackData(ctx, stack)
	if err != nil {
		ctx.ServerError("loadPullStackData", err)
		return
	}
	ctx.Data["PullStackData"] = data
	ctx.Data["CanManageStack"] = canManagePullStack(ctx)
	ctx.HTML(http.StatusOK, tplPullStackStatus)
}
