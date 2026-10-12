// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"fmt"
	"net/http"
	"strings"

	issues_model "gitea.dev/models/issues"
	perm_model "gitea.dev/models/perm"
	access_model "gitea.dev/models/perm/access"
	"gitea.dev/models/unit"
	"gitea.dev/services/context"
)

// redirectMovedIssue sends a missing index to the repository the issue moved to, keeping the rest of the path.
func redirectMovedIssue(ctx *context.Context) bool {
	index := ctx.PathParamInt64("index")
	issue, err := issues_model.GetRedirectedIssue(ctx, ctx.Repo.Repository.ID, index)
	if err != nil {
		return false
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return false
	}
	if allowed, err := access_model.HasAccessUnit(ctx, ctx.Doer, issue.Repo, unit.TypePullRequests, perm_model.AccessModeRead); err != nil || !allowed {
		return false
	}
	path := ctx.Req.URL.EscapedPath()
	marker := fmt.Sprintf("/%s/%d", ctx.PathParam("type"), index)
	_, suffix, _ := strings.CutLast(path, marker)
	if ctx.Req.URL.RawQuery != "" {
		suffix += "?" + ctx.Req.URL.RawQuery
	}
	ctx.Redirect(issue.Link()+suffix, http.StatusMovedPermanently)
	return true
}
