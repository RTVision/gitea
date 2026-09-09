// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

type PullRequestViewedFile struct {
	Path   string `json:"path"`
	Viewed bool   `json:"viewed"`
}

type PullRequestViewedFiles struct {
	HeadSHA string                  `json:"head_sha"`
	Files   []PullRequestViewedFile `json:"files"`
}

type UpdatePullRequestViewedFilesOptions struct {
	// required: true
	HeadSHA string `json:"head_sha" binding:"Required"`
	// required: true
	Files map[string]bool `json:"files" binding:"Required"`
}
