// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"errors"
	"net/http"

	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/routers/api/v1/utils"
	"gitea.dev/services/context"
	"gitea.dev/services/convert"
	pull_service "gitea.dev/services/pull"
)

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

func resolveStackPullRequestIDs(ctx *context.APIContext, indexes []int64) ([]int64, bool) {
	ids := make([]int64, 0, len(indexes))
	for _, index := range indexes {
		pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, index)
		if err != nil {
			ctx.APIErrorAuto(err)
			return nil, false
		}
		ids = append(ids, pr.ID)
	}
	return ids, true
}

func writeAPIStack(ctx *context.APIContext, status int, stack *issues_model.PullRequestStack) {
	converted, err := convert.ToAPIPullRequestStack(ctx, stack, ctx.Doer)
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackCapabilities"
	ctx.JSON(http.StatusOK, &api.PullRequestStackCapabilities{
		Enabled:     setting.Repository.PullRequest.EnableStacks,
		Operations:  []string{"land", "rebase"},
		MergeStyles: []string{string(repo_model.MergeStyleMerge), string(repo_model.MergeStyleSquash), string(repo_model.MergeStyleRebase)},
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: page
	//   in: query
	//   type: integer
	// - name: limit
	//   in: query
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/PullRequestStackList"
	//   "500":
	//     "$ref": "#/responses/error"
	opts := utils.GetListOptions(ctx)
	stacks, total, err := issues_model.ListStacks(ctx, ctx.Repo.Repository.ID, opts)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	converted := make([]*api.PullRequestStack, 0, len(stacks))
	for _, stack := range stacks {
		apiStack, err := convert.ToAPIPullRequestStack(ctx, stack, ctx.Doer)
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		converted = append(converted, apiStack)
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
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
	pullIDs, ok := resolveStackPullRequestIDs(ctx, form.PullRequests)
	if !ok {
		return
	}
	stack, err := pull_service.CreateStack(ctx, ctx.Doer, ctx.Repo.Repository, pull_service.CreateStackOptions{TrunkBranch: form.Trunk, PullRequestIDs: pullIDs})
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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
	pullIDs, ok := resolveStackPullRequestIDs(ctx, form.PullRequests)
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

// DeletePullRequestStack removes active stack membership without changing pull requests or branches.
func DeletePullRequestStack(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/stacks/{id} repository repoDeletePullRequestStack
	// ---
	// summary: Unstack a pull request stack
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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

// LandPullRequestStack starts ordered landing of a stack prefix.
func LandPullRequestStack(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/stacks/{id}/land repository repoLandPullRequestStack
	// ---
	// summary: Start ordered landing of a pull request stack prefix
	// consumes:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
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
		pr, err := issues_model.GetPullRequestByIndex(ctx, ctx.Repo.Repository.ID, head.PullRequest)
		if err != nil {
			ctx.APIErrorAuto(err)
			return
		}
		expectations = append(expectations, pull_service.StackHeadExpectation{PullRequestID: pr.ID, HeadSHA: head.HeadSHA, ParentSHA: head.ParentSHA})
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: page
	//   in: query
	//   type: integer
	// - name: limit
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
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
	//   in: path
	//   required: true
	//   type: string
	// - name: repo
	//   in: path
	//   required: true
	//   type: string
	// - name: id
	//   in: path
	//   required: true
	//   type: integer
	//   format: int64
	// - name: operation
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
