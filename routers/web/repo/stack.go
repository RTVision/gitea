// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	perm_model "gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
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

type pullStackHeaderData struct {
	Repo    *repo_model.Repository
	Stack   *issues_model.PullRequestStack
	Entries []*pullStackEntryData
}

type pullStackData struct {
	Repo          *repo_model.Repository
	Stack         *issues_model.PullRequestStack
	Entries       []*pullStackEntryData
	Operation     *issues_model.StackOperation
	Operations    []*issues_model.StackOperation
	LandingStyles []repo_model.MergeStyle
}

func (data *pullStackHeaderData) TopDownEntries() iter.Seq2[int, *pullStackEntryData] {
	return slices.Backward(data.Entries)
}

func (data *pullStackData) TopDownEntries() iter.Seq2[int, *pullStackEntryData] {
	return slices.Backward(data.Entries)
}

func canOpenPullStackForm(ctx *context.Context) bool {
	return ctx.IsSigned && ctx.Repo.Repository.CanContentChange() && ctx.Repo.Permission.CanRead(unit.TypeCode) && ctx.Repo.Permission.CanRead(unit.TypePullRequests)
}

func setPullStackCapabilities(ctx *context.Context, data *pullStackData) error {
	canManage, err := pull_service.CanManageStack(ctx, ctx.Doer, data.Stack)
	if err != nil {
		return err
	}
	canLand := false
	if data.Repo.CanContentChange() {
		canLand, err = pull_service.CanOperateStack(ctx, ctx.Doer, data.Stack, "land")
		if err != nil {
			return err
		}
	}
	canOperate := canManage
	if data.Operation != nil && data.Operation.Kind == "land" {
		canOperate = canLand
	}
	ctx.Data["CanManageStack"] = canManage
	ctx.Data["CanLandStack"] = canLand
	ctx.Data["CanOperateStack"] = canOperate
	return nil
}

func preparePullStackCreation(ctx *context.Context, pr *issues_model.PullRequest) error {
	ctx.Data["CanCreateStack"] = false
	if !ctx.IsSigned || !setting.Repository.PullRequest.EnableStacks || ctx.Data["PullStackData"] != nil {
		return nil
	}
	repo := ctx.Repo.Repository
	baseLayer, err := issues_model.GetOpenStackLayerByBranch(ctx, pr.BaseRepoID, pr.BaseBranch)
	if err != nil {
		return err
	}
	if repo.IsFork && pr.HeadRepoID == repo.ID {
		upstream, err := readableStackRepository(ctx, repo.ForkID)
		if err != nil && !errors.Is(err, issues_model.ErrStackNotExist) {
			return err
		}
		if upstream != nil {
			if baseLayer != nil {
				parentStack, err := issues_model.GetStackByID(ctx, baseLayer.StackID)
				if err != nil {
					return err
				}
				if parentStack.RepoID == upstream.ID {
					repo = upstream
				}
			} else {
				candidates, err := issues_model.FindStackCandidatePulls(ctx, upstream.ID)
				if err != nil {
					return err
				}
				candidates, err = readableStackCandidates(ctx, candidates)
				if err != nil {
					return err
				}
				chain, _ := pull_service.SuggestStackChainByID(candidates, pr.ID, upstream.DefaultBranch, upstream.ID)
				if len(chain) > 0 && chain[0].BaseRepoID == upstream.ID {
					repo = upstream
				}
			}
		}
	}
	allowed, err := pull_service.CanCreateStack(ctx, ctx.Doer, repo, []int64{pr.ID})
	if err != nil || !allowed {
		return err
	}
	ctx.Data["CanCreateStack"] = true
	ctx.Data["CreateStackURL"] = fmt.Sprintf("%s/pulls/stacks/new?pull=%d&global_ids=true", repo.Link(), pr.ID)
	if baseLayer == nil || !baseLayer.IsTop {
		return nil
	}
	stack, err := issues_model.GetStackByID(ctx, baseLayer.StackID)
	if err != nil {
		return err
	}
	allowed, err = pull_service.CanManageStack(ctx, ctx.Doer, stack, pr.ID)
	if err != nil || !allowed {
		return err
	}
	repo, err = readableStackRepository(ctx, stack.RepoID)
	if errors.Is(err, issues_model.ErrStackNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx.Data["AppendStackID"] = stack.ID
	ctx.Data["AppendStackRepoLink"] = repo.Link()
	return nil
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

func readableStackRepository(ctx *context.Context, repoID int64) (*repo_model.Repository, error) {
	repo, err := repo_model.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return nil, err
	}
	allowed, err := access_model.HasAccessUnit(ctx, ctx.Doer, repo, unit.TypePullRequests, perm_model.AccessModeRead)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, issues_model.ErrStackNotExist
	}
	return repo, nil
}

func readableStackCandidates(ctx *context.Context, candidates issues_model.PullRequestList) (issues_model.PullRequestList, error) {
	visible := make(issues_model.PullRequestList, 0, len(candidates))
	for _, pr := range candidates {
		repo, err := readableStackRepository(ctx, pr.BaseRepoID)
		if errors.Is(err, issues_model.ErrStackNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		pr.Issue.Repo, pr.BaseRepo = repo, repo
		visible = append(visible, pr)
	}
	return visible, nil
}

func creatableStackCandidates(ctx *context.Context, candidates issues_model.PullRequestList) (issues_model.PullRequestList, error) {
	visible := make(issues_model.PullRequestList, 0, len(candidates))
	for _, pr := range candidates {
		allowed, err := pull_service.CanCreateStack(ctx, ctx.Doer, ctx.Repo.Repository, []int64{pr.ID})
		if err != nil {
			return nil, err
		}
		if allowed {
			visible = append(visible, pr)
		}
	}
	return visible, nil
}

func loadPullStackEntries(ctx *context.Context, stackID int64) ([]*pullStackEntryData, error) {
	entries, err := issues_model.GetStackEntries(ctx, stackID)
	if err != nil {
		return nil, err
	}
	data := make([]*pullStackEntryData, 0, len(entries))
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return nil, err
		}
		if err := pr.LoadIssue(ctx); err != nil {
			return nil, err
		}
		repo, err := readableStackRepository(ctx, pr.BaseRepoID)
		if err != nil {
			return nil, err
		}
		pr.Issue.Repo, pr.BaseRepo = repo, repo
		data = append(data, &pullStackEntryData{Entry: entry, Pull: pr})
	}
	return data, nil
}

func loadPullStackData(ctx *context.Context, stack *issues_model.PullRequestStack) (*pullStackData, error) {
	entries, err := loadPullStackEntries(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	return completePullStackData(ctx, stack, entries)
}

func completePullStackData(ctx *context.Context, stack *issues_model.PullRequestStack, entries []*pullStackEntryData) (*pullStackData, error) {
	repo, err := readableStackRepository(ctx, stack.RepoID)
	if err != nil {
		return nil, err
	}
	data := &pullStackData{Repo: repo, Stack: stack, Entries: entries}
	for _, entry := range entries {
		entry.Readiness = stackEntryReadiness(ctx, entry.Pull)
	}
	prConfig := repo.MustGetUnit(ctx, unit.TypePullRequests).PullRequestsConfig()
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

// attachPullStackHeader loads what every pull request tab shows in its title, without the stack box's per-layer merge checks.
func attachPullStackHeader(ctx *context.Context, pr *issues_model.PullRequest) {
	stack, err := issues_model.GetPullRequestStack(ctx, pr.ID)
	if err != nil {
		ctx.ServerError("GetPullRequestStack", err)
		return
	}
	if stack == nil {
		return
	}
	entries, err := loadPullStackEntries(ctx, stack.ID)
	if errors.Is(err, issues_model.ErrStackNotExist) {
		return
	}
	if err != nil {
		ctx.ServerError("loadPullStackEntries", err)
		return
	}
	repo, err := readableStackRepository(ctx, stack.RepoID)
	if errors.Is(err, issues_model.ErrStackNotExist) {
		return
	}
	if err != nil {
		ctx.ServerError("readableStackRepository", err)
		return
	}
	ctx.Data["PullStackHeader"] = &pullStackHeaderData{Repo: repo, Stack: stack, Entries: entries}
}

func attachPullStackData(ctx *context.Context, issue *issues_model.Issue) {
	header, _ := ctx.Data["PullStackHeader"].(*pullStackHeaderData)
	if header == nil {
		return
	}
	data, err := completePullStackData(ctx, header.Stack, header.Entries)
	if err != nil {
		ctx.ServerError("completePullStackData", err)
		return
	}
	ctx.Data["PullStackData"] = data
	if err := setPullStackCapabilities(ctx, data); err != nil {
		ctx.ServerError("setPullStackCapabilities", err)
		return
	}
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
			InfoHTML:    ctx.Locale.Tr("repo.pulls.stack_merge_disabled", header.Stack.ID),
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
		if errors.Is(err, issues_model.ErrStackNotExist) {
			continue
		}
		if err != nil {
			ctx.NotFoundOrServerError("loadPullStackData", func(err error) bool { return errors.Is(err, issues_model.ErrStackNotExist) }, err)
			return
		}
		data = append(data, stackData)
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.stacks")
	ctx.Data["Stacks"] = data
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(count).PerPageLimit(setting.UI.IssuePagingNum).CurPage(page).Build()
	ctx.Data["CanCreateStack"] = setting.Repository.PullRequest.EnableStacks && canOpenPullStackForm(ctx)
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
		ctx.NotFoundOrServerError("loadPullStackData", func(err error) bool { return errors.Is(err, issues_model.ErrStackNotExist) }, err)
		return
	}
	ctx.Data["Title"] = ctx.Tr("repo.pulls.stack_number", stack.ID)
	ctx.Data["PullStackData"] = data
	if err := setPullStackCapabilities(ctx, data); err != nil {
		ctx.ServerError("setPullStackCapabilities", err)
		return
	}
	canManage, _ := ctx.Data["CanManageStack"].(bool)
	if canManage && setting.Repository.PullRequest.EnableStacks && stack.State == issues_model.StackStateOpen && stack.ActiveOperationID == 0 {
		candidates, err := pull_service.StackInsertCandidates(ctx, stack, 50)
		if err != nil {
			ctx.ServerError("StackInsertCandidates", err)
			return
		}
		visible := candidates[:0]
		for _, candidate := range candidates {
			prs, err := readableStackCandidates(ctx, issues_model.PullRequestList{candidate.Pull})
			if err != nil {
				ctx.ServerError("readableStackCandidates", err)
				return
			}
			if len(prs) > 0 {
				allowed, err := pull_service.CanManageStack(ctx, ctx.Doer, stack, candidate.Pull.ID)
				if err != nil {
					ctx.ServerError("CanManageStack", err)
					return
				}
				if allowed {
					visible = append(visible, candidate)
				}
			}
		}
		candidates = visible
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
	top := ctx.FormInt64("pull")
	if !ctx.FormBool("global_ids") {
		index := top
		top = 0
		for _, pr := range candidates {
			if pr.BaseRepoID == ctx.Repo.Repository.ID && pr.Index == index {
				top = pr.ID
				break
			}
		}
	}
	chain, start := pull_service.SuggestStackChainByID(candidates, top, ctx.Repo.Repository.DefaultBranch, ctx.Repo.Repository.ID)
	if start > 0 && chain[start].BaseRepoID != ctx.Repo.Repository.ID {
		start = 0
	}
	wanted := ctx.FormInt64("start")
	if wanted == 0 {
		return chain, start, true
	}
	i := slices.IndexFunc(chain, func(pr *issues_model.PullRequest) bool {
		return (ctx.FormBool("global_ids") && pr.ID == wanted) || (!ctx.FormBool("global_ids") && pr.BaseRepoID == ctx.Repo.Repository.ID && pr.Index == wanted)
	})
	if i < 0 || chain[i].BaseRepoID != ctx.Repo.Repository.ID {
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
	if !setting.Repository.PullRequest.EnableStacks || !canOpenPullStackForm(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	candidates, err := issues_model.FindStackCandidatePulls(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("FindStackCandidatePulls", err)
		return
	}
	candidates, err = readableStackCandidates(ctx, candidates)
	if err != nil {
		ctx.ServerError("readableStackCandidates", err)
		return
	}
	candidates, err = creatableStackCandidates(ctx, candidates)
	if err != nil {
		ctx.ServerError("creatableStackCandidates", err)
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
	if i := slices.IndexFunc(candidates, func(pr *issues_model.PullRequest) bool {
		return (ctx.FormBool("global_ids") && pr.ID == top) || (!ctx.FormBool("global_ids") && pr.BaseRepoID == ctx.Repo.Repository.ID && pr.Index == top)
	}); i >= 0 {
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
	if !setting.Repository.PullRequest.EnableStacks || !canOpenPullStackForm(ctx) {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	candidates, err := issues_model.FindStackCandidatePulls(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.ServerError("FindStackCandidatePulls", err)
		return
	}
	candidates, err = readableStackCandidates(ctx, candidates)
	if err != nil {
		ctx.ServerError("readableStackCandidates", err)
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
	allowed, err := pull_service.CanCreateStack(ctx, ctx.Doer, ctx.Repo.Repository, ids)
	if err != nil {
		ctx.ServerError("CanCreateStack", err)
		return
	}
	if !allowed {
		ctx.HTTPError(http.StatusForbidden)
		return
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
	if ctx.FormBool("global_ids") {
		query.Set("global_ids", "true")
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/pulls/stacks/new?" + query.Encode())
}

func PullStackAction(ctx *context.Context) {
	stack := getPullStack(ctx)
	if ctx.Written() {
		return
	}
	if !ctx.Repo.Repository.CanContentChange() {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
	revision := ctx.FormInt64("stack_version")
	action := ctx.PathParam("action")
	kind := action
	if action == "retry" || action == "cancel" {
		op, err := issues_model.GetStackOperation(ctx, ctx.FormInt64("operation"))
		if err != nil {
			ctx.NotFoundOrServerError("GetStackOperation", db.IsErrNotExist, err)
			return
		}
		if op.StackID != stack.ID {
			ctx.NotFound(nil)
			return
		}
		kind = op.Kind
	}
	allowed, err := pull_service.CanOperateStack(ctx, ctx.Doer, stack, kind)
	if err != nil {
		ctx.ServerError("CanOperateStack", err)
		return
	}
	if !allowed {
		ctx.HTTPError(http.StatusForbidden)
		return
	}
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
		if ctx.FormBool("global_ids") {
			pr, err = issues_model.GetPullRequestByID(ctx, ctx.FormInt64("pull"))
		} else {
			pr, err = issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, ctx.FormInt64("pull"))
		}
		if err == nil {
			_, err = readableStackRepository(ctx, pr.BaseRepoID)
		}
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
		ctx.NotFoundOrServerError("loadPullStackData", func(err error) bool { return errors.Is(err, issues_model.ErrStackNotExist) }, err)
		return
	}
	ctx.Data["PullStackData"] = data
	if err := setPullStackCapabilities(ctx, data); err != nil {
		ctx.ServerError("setPullStackCapabilities", err)
		return
	}
	ctx.HTML(http.StatusOK, tplPullStackStatus)
}
