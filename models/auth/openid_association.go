// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"time"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/util"

	"xorm.io/builder"
)

type OpenIDAssociation struct {
	State       string `xorm:"pk varchar(64)"`
	UserID      int64
	Identity    string             `xorm:"TEXT"`
	ExpiresUnix timeutil.TimeStamp `xorm:"INDEX"`
}

func init() {
	db.RegisterModel(new(OpenIDAssociation))
}

func CreateOpenIDAssociation(ctx context.Context, userID int64, identity string) (string, error) {
	now := timeutil.TimeStampNow()
	if _, err := db.GetEngine(ctx).Where("expires_unix <= ?", now).Delete(new(OpenIDAssociation)); err != nil {
		return "", err
	}
	association := &OpenIDAssociation{
		State: util.CryptoRandomString(64), UserID: userID, Identity: identity,
		ExpiresUnix: now.AddDuration(10 * time.Minute),
	}
	_, err := db.GetEngine(ctx).Insert(association)
	return association.State, err
}

func GetOpenIDAssociation(ctx context.Context, state string, userID int64) (*OpenIDAssociation, bool, error) {
	return db.Get[OpenIDAssociation](ctx, builder.Eq{"state": state, "user_id": userID}.And(builder.Gt{"expires_unix": timeutil.TimeStampNow()}))
}

func ConsumeOpenIDAssociation(ctx context.Context, state string, userID int64, identity string) (bool, error) {
	association, has, err := GetOpenIDAssociation(ctx, state, userID)
	if err != nil || !has || association.Identity != identity {
		return false, err
	}
	affected, err := db.GetEngine(ctx).Where("state = ? AND user_id = ? AND expires_unix > ?", state, userID, timeutil.TimeStampNow()).Delete(new(OpenIDAssociation))
	return affected == 1, err
}
