// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"
)

func AddOpenIDAssociation(ctx context.Context, x base.EngineMigration) error {
	type OpenIDAssociation struct {
		State       string `xorm:"pk varchar(64)"`
		UserID      int64
		Provider    string             `xorm:"TEXT"`
		Identity    string             `xorm:"TEXT"`
		ExpiresUnix timeutil.TimeStamp `xorm:"INDEX"`
	}
	return x.Sync(new(OpenIDAssociation))
}
