// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"

	"xorm.io/xorm"
)

func AddModeToPullRequestStack(_ context.Context, x base.EngineMigration) error {
	type PullRequestStack struct {
		Mode string `xorm:"VARCHAR(20) NOT NULL DEFAULT 'rebase'"`
	}

	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(PullRequestStack))
	return err
}
