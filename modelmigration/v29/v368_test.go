// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v29

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/require"
)

type openIDAssociationAfterV368 struct {
	State       string `xorm:"pk varchar(64)"`
	UserID      int64
	Identity    string             `xorm:"TEXT"`
	ExpiresUnix timeutil.TimeStamp `xorm:"INDEX"`
}

func (openIDAssociationAfterV368) TableName() string { return "open_id_association" }

func TestAddOpenIDAssociation(t *testing.T) {
	x, cleanup := migrationtest.PrepareTestEnv(t, 0)
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	require.NoError(t, AddOpenIDAssociation(t.Context(), x))
	state := &openIDAssociationAfterV368{State: "test", UserID: 2, Identity: "https://example.com/identity", ExpiresUnix: 1234}
	_, err := x.Insert(state)
	require.NoError(t, err)
	loaded := new(openIDAssociationAfterV368)
	has, err := x.ID("test").Get(loaded)
	require.NoError(t, err)
	require.True(t, has)
	require.Equal(t, state, loaded)
}
