// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"net/http"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/routers/api/v1/utils"
	"gitea.dev/services/context"
	"gitea.dev/services/convert"
	pull_service "gitea.dev/services/pull"
)

func checkStackAPIPullRead(ctx *context.APIContext, pr *issues_model.PullRequest) error {
	for _, repoID := range []int64{pr.BaseRepoID, pr.HeadRepoID} {
		repo, err := repo_model.GetRepositoryByID(ctx, repoID)
		if err != nil {
			return err
		}
		if !ctx.TokenCanAccessRepo(repo) {
			return util.ErrNotExist
		}
		permission, err := access_model.GetDoerRepoPermission(ctx, repo, ctx.Doer)
		if err != nil {
			return err
		}
		if !permission.CanRead(unit.TypeCode) || repoID == pr.BaseRepoID && !permission.CanRead(unit.TypePullRequests) {
			return util.ErrNotExist
		}
	}
	return nil
}

func checkStackAPIRead(ctx *context.APIContext, stack *issues_model.PullRequestStack) error {
	entries, err := issues_model.GetStackEntries(ctx, stack.ID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pr, err := issues_model.GetPullRequestByID(ctx, entry.PullRequestID)
		if err != nil {
			return err
		}
		if err := checkStackAPIPullRead(ctx, pr); err != nil {
			return err
		}
	}
	return nil
}

func getRepositoryStack(ctx *context.APIContext) *issues_model.PullRequestStack {
	stack, err := issues_model.GetStackByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.APIErrorAuto(err)
		return nil
	}
	if stack.RepoID != ctx.Repo.Repository.ID {
		ctx.APIErrorNotFound()
		return nil
	}
	if err := checkStackAPIRead(ctx, stack); err != nil {
		ctx.APIErrorAuto(err)
		return nil
	}
	return stack
}

func stackServiceError(ctx *context.APIContext, stackID int64, err error) {
	if errors.Is(err, issues_model.ErrStackRevision) {
		stack, getErr := issues_model.GetStackByID(ctx, stackID)
		if getErr == nil {
			ctx.JSON(http.StatusConflict, map[string]int64{"revision": stack.Revision})
			return
		}
		ctx.APIError(http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pull_service.ErrNoPermissionToMerge) {
		ctx.APIError(http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, issues_model.ErrStackNotExist) || errors.Is(err, util.ErrNotExist) {
		ctx.APIErrorNotFound()
		return
	}
	if message, status := util.ErrorUnwrapForUser(err); message != "" {
		ctx.APIError(status, message)
		return
	}
	ctx.APIError(http.StatusUnprocessableEntity, err.Error())
}

func resolveStackPullRequestIDs(ctx *context.APIContext, indexes []int64, references []api.PullRequestReference) ([]int64, bool) {
	if len(indexes) != 0 && len(references) != 0 || len(indexes) == 0 && len(references) == 0 {
		ctx.APIError(http.StatusUnprocessableEntity, "provide either pull_requests or pull_request_refs")
		return nil, false
	}
	for _, index := range indexes {
		references = append(references, api.PullRequestReference{PullRequest: index})
	}
	ids := make([]int64, 0, len(references))
	for _, reference := range references {
		repoID := reference.RepositoryID
		if repoID == 0 {
			repoID = ctx.Repo.Repository.ID
		}
		if reference.PullRequest <= 0 || repoID < 0 {
			ctx.APIError(http.StatusUnprocessableEntity, "invalid pull request reference")
			return nil, false
		}
		repo, err := repo_model.GetRepositoryByID(ctx, repoID)
		if err != nil {
			ctx.APIErrorAuto(err)
			return nil, false
		}
		if !ctx.TokenCanAccessRepo(repo) {
			ctx.APIErrorNotFound()
			return nil, false
		}
		permission, err := access_model.GetDoerRepoPermission(ctx, repo, ctx.Doer)
		if err != nil {
			ctx.APIErrorInternal(err)
			return nil, false
		}
		if !permission.CanRead(unit.TypeCode) || !permission.CanRead(unit.TypePullRequests) {
			ctx.APIErrorNotFound()
			return nil, false
		}
		pr, err := issues_model.GetPullRequestByIndex(ctx, repoID, reference.PullRequest)
		if err != nil {
			ctx.APIErrorAuto(err)
			return nil, false
		}
		if err := checkStackAPIPullRead(ctx, pr); err != nil {
			ctx.APIErrorAuto(err)
			return nil, false
		}
		ids = append(ids, pr.ID)
	}
	return ids, true
}

func writeAPIStack(ctx *context.APIContext, status int, stack *issues_model.PullRequestStack) {
	converted, err := convert.ToAPIPullRequestStack(ctx, stack, ctx.Doer)
	if errors.Is(err, util.ErrNotExist) {
		ctx.APIErrorNotFound()
		return
	}
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(status, converted)
}

// StackCapabilities returns stacked pull request capabilities.
func StackCapabilities(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/stacks/capabilities repository repoGetStackCapabilities
	// ---
	// summary: Get stacked pull request capabilities
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackCapabilities"
	modes := []api.StackMode{api.StackModeRebase, api.StackModeMerge}
	styles := make(map[string][]string, len(modes))
	for _, mode := range modes {
		for _, style := range pull_service.StackLandingStyles(string(mode)) {
			styles[string(mode)] = append(styles[string(mode)], string(style))
		}
	}
	ctx.JSON(http.StatusOK, &api.PullRequestStackCapabilities{
		Enabled:         setting.Repository.PullRequest.EnableStacks,
		Operations:      []string{"land", "rebase", "update", "insert"},
		Modes:           modes,
		MergeStyles:     styles[issues_model.StackModeRebase],
		ModeMergeStyles: styles,
	})
}

// ListPullRequestStacks lists a repository's pull request stacks.
func ListPullRequestStacks(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/stacks repository repoListPullRequestStacks
	// ---
	// summary: List a repository's pull request stacks
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: page
	//   description: Page number
	//   in: query
	//   type: integer
	// - name: limit
	//   description: Page size
	//   in: query
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackList"
	//   "500":
	//     "$ref": "#/responses/error"
	opts := utils.GetListOptions(ctx)
	stacks, _, err := issues_model.ListStacks(ctx, ctx.Repo.Repository.ID, db.ListOptionsAll)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	readable := make([]*issues_model.PullRequestStack, 0, len(stacks))
	for _, stack := range stacks {
		if err := checkStackAPIRead(ctx, stack); err != nil {
			if errors.Is(err, util.ErrNotExist) {
				continue
			}
			ctx.APIErrorAuto(err)
			return
		}
		readable = append(readable, stack)
	}
	skip, take := opts.GetSkipTake()
	total := int64(len(readable))
	converted := make([]*api.PullRequestStack, 0, min(take, len(readable)))
	for _, stack := range readable[min(skip, len(readable)):] {
		apiStack, err := convert.ToAPIPullRequestStack(ctx, stack, ctx.Doer)
		if errors.Is(err, util.ErrNotExist) {
			total--
			continue
		}
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		converted = append(converted, apiStack)
		if len(converted) == take {
			break
		}
	}
	ctx.SetLinkHeader(total, opts.PageSize)
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, converted)
}

// CreatePullRequestStack creates a stack from an existing pull request chain.
func CreatePullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks repository repoCreatePullRequestStack
	// ---
	// summary: Create a pull request stack
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/CreatePullRequestStackOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/PullRequestStack"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "422":
	//     "$ref": "#/responses/validationError"
	form := web.GetForm[*api.CreatePullRequestStackOption](ctx)
	pullIDs, ok := resolveStackPullRequestIDs(ctx, form.PullRequests, form.PullRequestRefs)
	if !ok {
		return
	}
	stack, err := pull_service.CreateStack(ctx, ctx.Doer, ctx.Repo.Repository, pull_service.CreateStackOptions{TrunkBranch: form.Trunk, Mode: string(form.Mode), PullRequestIDs: pullIDs})
	if err != nil {
		stackServiceError(ctx, 0, err)
		return
	}
	writeAPIStack(ctx, http.StatusCreated, stack)
}

// GetPullRequestStack gets a repository pull request stack.
func GetPullRequestStack(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/stacks/{id} repository repoGetPullRequestStack
	// ---
	// summary: Get a pull request stack
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStack"
	//   "404":
	//     "$ref": "#/responses/notFound"
	stack := getRepositoryStack(ctx)
	if stack != nil {
		writeAPIStack(ctx, http.StatusOK, stack)
	}
}

// AppendPullRequestStack appends pull requests to a stack.
func AppendPullRequestStack(ctx *context.APIContext) {
	// swagger:operation PATCH /repos/{owner}/{repo}/stacks/{id} repository repoAppendPullRequestStack
	// ---
	// summary: Append pull requests to a stack
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/EditPullRequestStackOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStack"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	form := web.GetForm[*api.EditPullRequestStackOption](ctx)
	pullIDs, ok := resolveStackPullRequestIDs(ctx, form.PullRequests, form.PullRequestRefs)
	if !ok {
		return
	}
	stackID := stack.ID
	stack, err := pull_service.AppendStack(ctx, ctx.Doer, stackID, form.Revision, pullIDs)
	if err != nil {
		stackServiceError(ctx, stackID, err)
		return
	}
	writeAPIStack(ctx, http.StatusOK, stack)
}

// InsertPullRequestStack inserts a pull request into a stack above the layer its base branch names.
func InsertPullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/insert repository repoInsertPullRequestStack
	// ---
	// summary: Insert a pull request into a stack above the layer its base branch names, retargeting the layer above it
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/InsertPullRequestStackOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStack"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	form := web.GetForm[*api.InsertPullRequestStackOption](ctx)
	var indexes []int64
	var references []api.PullRequestReference
	if form.PullRequest != 0 {
		indexes = []int64{form.PullRequest}
	}
	if form.PullRequestRef != nil {
		references = []api.PullRequestReference{*form.PullRequestRef}
	}
	pullIDs, ok := resolveStackPullRequestIDs(ctx, indexes, references)
	if !ok {
		return
	}
	stackID := stack.ID
	stack, err := pull_service.InsertStackLayer(ctx, ctx.Doer, stackID, form.Revision, pullIDs[0])
	if err != nil {
		stackServiceError(ctx, stackID, err)
		return
	}
	writeAPIStack(ctx, http.StatusOK, stack)
}

// DeletePullRequestStack removes active stack membership without changing pull requests or branches.
func DeletePullRequestStack(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/stacks/{id} repository repoDeletePullRequestStack
	// ---
	// summary: Unstack a pull request stack
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/PullRequestStackRevisionOption"
	// responses:
	//   "204":
	//     description: The pull requests were unstacked
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	form := web.GetForm[*api.PullRequestStackRevisionOption](ctx)
	if err := pull_service.Unstack(ctx, ctx.Doer, stack.ID, form.Revision); err != nil {
		stackServiceError(ctx, stack.ID, err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

func startPullRequestStackOperation(ctx *context.APIContext, kind string) {
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	form := web.GetForm[*api.PullRequestStackOperationOption](ctx)
	op, err := pull_service.StartStackOperation(ctx, ctx.Doer, pull_service.StackOperationOptions{
		StackID: stack.ID, ExpectedRevision: form.Revision, ThroughPosition: form.ThroughPosition,
		Kind: kind, MergeStyle: repo_model.MergeStyle(form.MergeStyle),
	})
	if err != nil {
		stackServiceError(ctx, stack.ID, err)
		return
	}
	ctx.JSON(http.StatusAccepted, convert.ToAPIPullRequestStackOperation(op))
}

// RebasePullRequestStack starts a durable stack rebase operation.
func RebasePullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/rebase repository repoRebasePullRequestStack
	// ---
	// summary: Start a pull request stack rebase
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/PullRequestStackOperationOption"
	// responses:
	//   "202":
	//     "$ref": "#/responses/PullRequestStackOperation"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	startPullRequestStackOperation(ctx, "rebase")
}

// UpdatePullRequestStack starts a durable merge-mode stack update.
func UpdatePullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/update repository repoUpdatePullRequestStack
	// ---
	// summary: Start a merge-mode pull request stack update, merging each parent into its layer
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/PullRequestStackOperationOption"
	// responses:
	//   "202":
	//     "$ref": "#/responses/PullRequestStackOperation"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	startPullRequestStackOperation(ctx, "update")
}

// LandPullRequestStack starts ordered landing of a stack prefix.
func LandPullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/land repository repoLandPullRequestStack
	// ---
	// summary: Start ordered landing of a pull request stack prefix
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/PullRequestStackOperationOption"
	// responses:
	//   "202":
	//     "$ref": "#/responses/PullRequestStackOperation"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	startPullRequestStackOperation(ctx, "land")
}

// SynchronizePullRequestStack records validated boundaries after an explicit local restack.
func SynchronizePullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/sync repository repoSynchronizePullRequestStack
	// ---
	// summary: Synchronize locally restacked pull request heads and boundaries
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/SynchronizePullRequestStackOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStack"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/StackRevisionConflict"
	//   "422":
	//     "$ref": "#/responses/validationError"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	form := web.GetForm[*api.SynchronizePullRequestStackOption](ctx)
	stackID := stack.ID
	expectations := make([]pull_service.StackHeadExpectation, 0, len(form.Heads))
	for _, head := range form.Heads {
		ids, ok := resolveStackPullRequestIDs(ctx, nil, []api.PullRequestReference{{RepositoryID: head.RepositoryID, PullRequest: head.PullRequest}})
		if !ok {
			return
		}
		expectations = append(expectations, pull_service.StackHeadExpectation{PullRequestID: ids[0], HeadSHA: head.HeadSHA, ParentSHA: head.ParentSHA})
	}
	stack, err := pull_service.SynchronizeStack(ctx, ctx.Doer, stackID, form.Revision, expectations)
	if err != nil {
		stackServiceError(ctx, stackID, err)
		return
	}
	writeAPIStack(ctx, http.StatusOK, stack)
}

// ListPullRequestStackOperations lists recent operations for a stack.
func ListPullRequestStackOperations(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/stacks/{id}/operations repository repoListPullRequestStackOperations
	// ---
	// summary: List pull request stack operations
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: page
	//   description: Page number
	//   in: query
	//   type: integer
	// - name: limit
	//   description: Page size
	//   in: query
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackOperationList"
	//   "404":
	//     "$ref": "#/responses/notFound"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	ops, err := issues_model.GetStackOperations(ctx, stack.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	listOpts := utils.GetListOptions(ctx)
	total := int64(len(ops))
	start := min((listOpts.Page-1)*listOpts.PageSize, len(ops))
	end := min(start+listOpts.PageSize, len(ops))
	if listOpts.IsListAll() {
		start, end = 0, len(ops)
	}
	converted := make([]*api.PullRequestStackOperation, 0, end-start)
	for _, op := range ops[start:end] {
		converted = append(converted, convert.ToAPIPullRequestStackOperation(op))
	}
	ctx.SetLinkHeader(total, listOpts.PageSize)
	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, converted)
}

func getPullRequestStackOperation(ctx *context.APIContext, stack *issues_model.PullRequestStack) *issues_model.StackOperation {
	op, err := issues_model.GetStackOperation(ctx, ctx.PathParamInt64("operation"))
	if err != nil {
		ctx.APIErrorAuto(err)
		return nil
	}
	if op.StackID != stack.ID {
		ctx.APIErrorNotFound()
		return nil
	}
	return op
}

// GetPullRequestStackOperation gets durable stack operation progress.
func GetPullRequestStackOperation(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/stacks/{id}/operations/{operation} repository repoGetPullRequestStackOperation
	// ---
	// summary: Get a pull request stack operation
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
	//   description: Stack operation ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackOperation"
	//   "404":
	//     "$ref": "#/responses/notFound"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	op := getPullRequestStackOperation(ctx, stack)
	if op != nil {
		ctx.JSON(http.StatusOK, convert.ToAPIPullRequestStackOperation(op))
	}
}

// CancelPullRequestStackOperation cancels a waiting or queued operation.
func CancelPullRequestStackOperation(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/operations/{operation}/cancel repository repoCancelPullRequestStackOperation
	// ---
	// summary: Cancel a pull request stack operation
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
	//   description: Stack operation ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// responses:
	//   "204":
	//     description: The operation was cancelled
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "422":
	//     "$ref": "#/responses/validationError"
	stack := getRepositoryStack(ctx)
	if stack == nil || getPullRequestStackOperation(ctx, stack) == nil {
		return
	}
	opID := ctx.PathParamInt64("operation")
	if err := pull_service.CancelStackOperation(ctx, ctx.Doer, opID); err != nil {
		stackServiceError(ctx, stack.ID, err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// RetryPullRequestStackOperation retries a blocked operation from its persisted progress.
func RetryPullRequestStackOperation(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/operations/{operation}/retry repository repoRetryPullRequestStackOperation
	// ---
	// summary: Retry a pull request stack operation
	// parameters:
	// - name: owner
	//   description: Repository owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   description: Repository name
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   description: Stack ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
	//   description: Stack operation ID
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// responses:
	//   "202":
	//     "$ref": "#/responses/PullRequestStackOperation"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "422":
	//     "$ref": "#/responses/validationError"
	stack := getRepositoryStack(ctx)
	if stack == nil {
		return
	}
	op := getPullRequestStackOperation(ctx, stack)
	if op == nil {
		return
	}
	if err := pull_service.ResumeStackOperation(ctx, ctx.Doer, op.ID); err != nil {
		stackServiceError(ctx, stack.ID, err)
		return
	}
	op, err := issues_model.GetStackOperation(ctx, op.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusAccepted, convert.ToAPIPullRequestStackOperation(op))
}
