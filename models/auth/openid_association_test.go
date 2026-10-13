// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"testing"
	"time"

	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenIDAssociationState(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	state, err := CreateOpenIDAssociation(t.Context(), 2, "https://example.com/Identity")
	require.NoError(t, err)
	for _, tc := range []struct {
		uid      int64
		identity string
	}{
		{1, "https://example.com/Identity"},
		{2, "https://example.com/identity"},
	} {
		consumed, err := ConsumeOpenIDAssociation(t.Context(), state, tc.uid, tc.identity)
		require.NoError(t, err)
		assert.False(t, consumed)
	}
	consumed, err := ConsumeOpenIDAssociation(t.Context(), state, 2, "https://example.com/Identity")
	require.NoError(t, err)
	assert.True(t, consumed)
	consumed, err = ConsumeOpenIDAssociation(t.Context(), state, 2, "https://example.com/Identity")
	require.NoError(t, err)
	assert.False(t, consumed)

	require.NoError(t, db.Insert(t.Context(), &OpenIDAssociation{State: "expired", UserID: 2, Identity: "https://example.com/Identity", ExpiresUnix: timeutil.TimeStampNow().AddDuration(-time.Hour)}))
	consumed, err = ConsumeOpenIDAssociation(t.Context(), "expired", 2, "https://example.com/Identity")
	require.NoError(t, err)
	assert.False(t, consumed)
	_, err = CreateOpenIDAssociation(t.Context(), 2, "https://example.com/Identity")
	require.NoError(t, err)
	exists, err := db.GetEngine(t.Context()).Where("state = ?", "expired").Exist(new(OpenIDAssociation))
	require.NoError(t, err)
	assert.False(t, exists)
}
