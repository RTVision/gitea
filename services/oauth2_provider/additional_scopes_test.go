// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package oauth2_provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrantAdditionalScopes(t *testing.T) {
	tests := []struct {
		grantScopes    string
		expectedScopes string
	}{
		{"", "all"}, // for old tokens without scope, treat it as "all"
		{"openid profile email", "all"},
		{"openid profile email groups", "all"},
		{"openid profile email all", "all"},
		{"openid profile email read:user all", "all"},
		{"openid profile email groups read:user", "read:user"},
		{"read:user read:repository", "read:repository,read:user"},
		{"read:user write:issue public-only", "public-only,write:issue,read:user"},
		{"openid profile email read:user", "read:user"},
		{"no-token-scope", "no-token-scope"},
		{"openid no-token-scope", "no-token-scope"},
		{"no-token-scope read:user", "read:user"},
		{"read:invalid_scope", "no-token-scope"},
		{"read:invalid_scope,write:scope_invalid,just-plain-wrong", "no-token-scope"},
		{"openid invalid:scope", "no-token-scope"},
		{"openid ,,", "no-token-scope"},
		{"read:user invalid:scope", "read:user"},
		{"read:user,invalid:scope public-only", "public-only,read:user"},
	}

	for _, test := range tests {
		t.Run("scope:"+test.grantScopes, func(t *testing.T) {
			result := GrantAdditionalScopes(test.grantScopes)
			assert.Equal(t, test.expectedScopes, string(result))
		})
	}
}

func TestGrantScopeMetadata(t *testing.T) {
	for _, tc := range []struct {
		grant, normalized, effective string
	}{
		{"", "", "all"},
		{"openid", "openid", "openid all"},
		{"openid profile email", "openid profile email", "openid profile email all"},
		{"openid all read:user", "openid all", "openid all"},
		{"openid read:user read:repository", "openid read:repository read:user", "openid read:repository read:user"},
		{"openid invalid:scope", "openid no-token-scope", "openid no-token-scope"},
		{",,", "no-token-scope", "no-token-scope"},
		{"openid no-token-scope", "openid no-token-scope", "openid no-token-scope"},
		{"openid read:user invalid:scope", "openid read:user", "openid read:user"},
	} {
		t.Run(tc.grant, func(t *testing.T) {
			assert.Equal(t, tc.normalized, NormalizeGrantScope(tc.grant))
			assert.Equal(t, tc.effective, EffectiveGrantScope(tc.grant))
		})
	}
}
