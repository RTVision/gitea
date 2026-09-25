// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/log"
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
	Stack         *issues_model.PullRequestStack
	Entries       []*pullStackEntryData
	Operation     *issues_model.StackOperation
	Operations    []*issues_model.StackOperation
	LandingStyles []repo_model.MergeStyle
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
	prConfig := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig()
	for _, style := range pull_service.StackLandingStyles(stack.Mode) {
		if prConfig.IsMergeStyleAllowed(style) {
			data.LandingStyles = append(data.LandingStyles, style)
		}
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
		mergeData.ShowUpdatePullInfo = mergeData.ShowUpdatePullInfo && pull_service.CheckStackUpdateByMerge(ctx, issue.PullRequest) == nil
		if mergeData.ShowUpdatePullInfo {
			mergeData.UpdateStyleOptions = slices.DeleteFunc(mergeData.UpdateStyleOptions, func(action *pullUpdateAction) bool { return !strings.HasSuffix(action.URL, "style=merge") })
			mergeData.ShowUpdatePullInfo = len(mergeData.UpdateStyleOptions) > 0
		}
		if mergeData.ShowUpdatePullInfo {
			mergeData.UpdatePrimaryAction = mergeData.UpdateStyleOptions[0]
			mergeData.UpdatePrimaryAction.Selected = true
		}
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
	canManage := canManagePullStack(ctx)
	ctx.Data["CanManageStack"] = canManage
	if canManage && setting.Repository.PullRequest.EnableStacks && stack.State == issues_model.StackStateOpen && stack.ActiveOperationID == 0 {
		candidates, err := pull_service.StackInsertCandidates(ctx, stack, 50)
		if err != nil {
			ctx.ServerError("StackInsertCandidates", err)
			return
		}
		ctx.Data["CanInsertStack"] = true
		ctx.Data["InsertCandidates"] = candidates
	}
	ctx.HTML(http.StatusOK, tplPullStack)
}

func pullStackNumbers(ctx *context.Context) ([]int64, error) {
	values := strings.FieldsFunc(strings.Join(ctx.FormStrings("pulls"), ","), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
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

type pullStackNewLayer struct {
	Pull      *issues_model.PullRequest
	Invalid   string
	Behind    bool
	HasMerges bool
}

// stackErrorMessage drops the sentinel prefix, which reads as noise in the UI.
func stackErrorMessage(err error) string {
	msg := err.Error()
	for _, sentinel := range []error{issues_model.ErrInvalidStack, issues_model.ErrStackRevision} {
		msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
	}
	return msg
}

// suggestPullStack honors the form's start layer, reporting false when it isn't in the chain.
func suggestPullStack(ctx *context.Context, candidates issues_model.PullRequestList) ([]*issues_model.PullRequest, int, bool) {
	chain, start := pull_service.SuggestStackChain(candidates, ctx.FormInt64("pull"), ctx.Repo.Repository.DefaultBranch)
	wanted := ctx.FormInt64("start")
	if wanted == 0 {
		return chain, start, true
	}
	i := slices.IndexFunc(chain, func(pr *issues_model.PullRequest) bool { return pr.Index == wanted })
	if i < 0 {
		return chain, start, false
	}
	return chain, i, true
}

// checkPullStackLayers checks every layer as a possible start, resuming above a layer that breaks the chain.
func checkPullStackLayers(ctx *context.Context, chain []*issues_model.PullRequest) []*pullStackNewLayer {
	layers := make([]*pullStackNewLayer, len(chain))
	ids := make([]int64, len(chain))
	for i, pr := range chain {
		layers[i], ids[i] = &pullStackNewLayer{Pull: pr}, pr.ID
	}
	for from := 0; from < len(chain); {
		checks, err := pull_service.CheckStackChain(ctx, ctx.Repo.Repository, chain[from].BaseBranch, "", ids[from:], 0)
		for i, check := range checks {
			layer := layers[from+i]
			layer.Behind, layer.HasMerges = check.Behind, check.HasMerges
			if check.Invalid != nil {
				layer.Invalid = pullStackLayerError(ctx, check.Invalid)
			}
		}
		from += len(checks)
		if err != nil {
			layers[from].Invalid = pullStackLayerError(ctx, err)
			from++
		}
	}
	return layers
}

func pullStackLayerError(ctx *context.Context, err error) string {
	if errors.Is(err, issues_model.ErrInvalidStack) || errors.Is(err, issues_model.ErrStackRevision) {
		return stackErrorMessage(err)
	}
	log.Error("CheckStackChain: %v", err)
	return ctx.Locale.TrString("error.occurred")
}

func PullStackNew(ctx *context.Context) {
	if !setting.Repository.PullRequest.EnableStacks || !canManagePullStack(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	candidates, err := issues_model.FindStackCandidatePulls(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("FindStackCandidatePulls", err)
		return
	}
	chain, start, _ := suggestPullStack(ctx, candidates)
	layers := checkPullStackLayers(ctx, chain)
	rebaseBlocked, startBlocked := false, false
	if len(chain) > 0 {
		for _, layer := range layers[start:] {
			rebaseBlocked = rebaseBlocked || layer.Behind || layer.HasMerges
			startBlocked = startBlocked || layer.Invalid != ""
		}
		base := chain[0].BaseBranch
		if base != ctx.Repo.Repository.DefaultBranch {
			baseLayer, err := issues_model.GetOpenStackLayerByBranch(ctx, ctx.Repo.Repository.ID, base)
			if err != nil {
				ctx.ServerError("GetOpenStackLayerByBranch", err)
				return
			}
			ctx.Data["BaseLayer"] = baseLayer
		}
		ctx.Data["Base"] = base
		ctx.Data["Trunk"] = chain[start].BaseBranch
	}
	mode := ctx.FormTrim("mode")
	if mode != issues_model.StackModeRebase || rebaseBlocked {
		mode = issues_model.StackModeMerge
	}
	top := ctx.FormInt64("pull")
	if i := slices.IndexFunc(candidates, func(pr *issues_model.PullRequest) bool { return pr.Index == top }); i >= 0 {
		ctx.Data["TopPull"] = candidates[i]
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.new_stack")
	ctx.Data["Candidates"] = candidates
	ctx.Data["Layers"] = layers
	ctx.Data["Start"] = start
	ctx.Data["Mode"] = mode
	ctx.Data["RebaseBlocked"] = rebaseBlocked
	ctx.Data["StartBlocked"] = startBlocked
	ctx.HTML(http.StatusOK, tplPullStackNew)
}

func PullStackNewPost(ctx *context.Context) {
	if !setting.Repository.PullRequest.EnableStacks || !canManagePullStack(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	candidates, err := issues_model.FindStackCandidatePulls(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("FindStackCandidatePulls", err)
		return
	}
	chain, start, ok := suggestPullStack(ctx, candidates)
	if len(chain) == 0 {
		ctx.Flash.Error(ctx.Tr("repo.pulls.stack_no_chain"))
		ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks/new")
		return
	}
	if !ok {
		ctx.Flash.Error(ctx.Tr("repo.pulls.stack_changed"))
		redirectPullStackNew(ctx)
		return
	}
	ids := make([]int64, 0, len(chain)-start)
	for _, pr := range chain[start:] {
		ids = append(ids, pr.ID)
	}
	// The trunk follows from the start layer, so the form can't post an inconsistent one.
	_, err = pull_service.CreateStack(ctx, ctx.Doer, ctx.Repo.Repository, pull_service.CreateStackOptions{TrunkBranch: chain[start].BaseBranch, Mode: ctx.FormTrim("mode"), PullRequestIDs: ids})
	if err != nil {
		ctx.Flash.Error(ctx.Tr("repo.pulls.stack_create_error", stackErrorMessage(err)))
		redirectPullStackNew(ctx)
		return
	}
	ctx.Flash.Success(ctx.Tr("repo.pulls.stack_created"))
	ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks")
}

func redirectPullStackNew(ctx *context.Context) {
	query := url.Values{"pull": {ctx.FormString("pull")}, "start": {ctx.FormString("start")}, "mode": {ctx.FormString("mode")}}
	ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks/new?" + query.Encode())
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
	case "insert":
		if !setting.Repository.PullRequest.EnableStacks {
			ctx.HTTPError(http.StatusForbidden)
			return
		}
		var pr *issues_model.PullRequest
		pr, err = issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, ctx.FormInt64("pull"))
		if err == nil {
			_, err = pull_service.InsertStackLayer(ctx, ctx.Doer, stack.ID, revision, pr.ID)
		}
		if err == nil && stack.Mode == issues_model.StackModeMerge {
			ctx.Flash.Success(ctx.Tr("repo.pulls.stack_inserted_merge", pr.Index))
		} else if err == nil {
			ctx.Flash.Success(ctx.Tr("repo.pulls.stack_inserted", pr.Index))
		}
	case "unstack":
		err = pull_service.Unstack(ctx, ctx.Doer, stack.ID, revision)
	case "land", "rebase", "update":
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
