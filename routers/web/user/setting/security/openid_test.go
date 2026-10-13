// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package security

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	auth_model "gitea.dev/models/auth"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/test"
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/web"
	"gitea.dev/services/contexttest"
	"gitea.dev/services/forms"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenIDAssociation(t *testing.T) {
	unittest.PrepareTestEnv(t)
	defer test.MockVariableValue(&setting.AppURL, "https://gitea.example/")()
	defer test.MockVariableValue(&setting.AppSubURL, "")()
	defer test.MockVariableValue(&setting.Security.AllowedHostList, "127.0.0.1:*")()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			assert.Equal(t, "check_authentication", r.FormValue("openid.mode"))
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:true\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><link rel="openid2.provider" href="http://%s/provider"></head></html>`, r.Host)
	}))
	defer provider.Close()
	identity := provider.URL + "/identity"
	ctx, resp := contexttest.MockContext(t, "POST /user/settings/security/openid")
	contexttest.LoadUser(t, ctx, 2)
	web.SetForm(ctx, &forms.AddOpenIDForm{Openid: identity})
	OpenIDPost(ctx)
	require.Equal(t, http.StatusSeeOther, resp.Code)
	redirect, err := url.Parse(resp.Header().Get("Location"))
	require.NoError(t, err)
	callback, err := url.Parse(redirect.Query().Get("openid.return_to"))
	require.NoError(t, err)
	state := callback.Query().Get("state")
	require.NotEmpty(t, state)
	ctx, resp = contexttest.MockContext(t, "POST /user/settings/security/openid")
	contexttest.LoadUser(t, ctx, 2)
	web.SetForm(ctx, &forms.AddOpenIDForm{Openid: identity})
	OpenIDPost(ctx)
	secondRedirect, err := url.Parse(resp.Header().Get("Location"))
	require.NoError(t, err)
	secondCallback, err := url.Parse(secondRedirect.Query().Get("openid.return_to"))
	require.NoError(t, err)
	assert.NotEqual(t, state, secondCallback.Query().Get("state"))
	require.NoError(t, db.Insert(t.Context(), &auth_model.OpenIDAssociation{State: "expired", UserID: 2, Identity: identity, ExpiresUnix: timeutil.TimeStampNow().AddDuration(-time.Hour)}))

	params := url.Values{
		"openid.ns":             {"http://specs.openid.net/auth/2.0"},
		"openid.mode":           {"id_res"},
		"openid.op_endpoint":    {provider.URL + "/provider"},
		"openid.claimed_id":     {identity},
		"openid.identity":       {identity},
		"openid.return_to":      {callback.String()},
		"openid.response_nonce": {time.Now().UTC().Format("2006-01-02T15:04:05Z") + "association"},
		"openid.assoc_handle":   {"test-association"},
		"openid.signed":         {"op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"test-signature"},
	}
	for _, tc := range []struct {
		name  string
		uid   int64
		state string
	}{
		{"unsolicited", 2, ""},
		{"wrong state", 2, "wrong"},
		{"other user", 1, state},
		{"expired", 2, "expired"},
		{"other purpose", 2, "reset_password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params.Set("state", tc.state)
			params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+tc.name)
			ctx, resp := contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
			contexttest.LoadUser(t, ctx, tc.uid)
			Security(ctx)
			assert.Equal(t, http.StatusForbidden, resp.Code)
			unittest.AssertNotExistsBean(t, &user_model.UserOpenID{URI: identity})
		})
	}

	params.Set("state", state)
	params.Set("openid.claimed_id", provider.URL+"/substitute")
	params.Set("openid.identity", provider.URL+"/substitute")
	params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+"substitute")
	ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
	contexttest.LoadUser(t, ctx, 2)
	Security(ctx)
	assert.Equal(t, http.StatusForbidden, resp.Code)
	unittest.AssertNotExistsBean(t, &user_model.UserOpenID{URI: provider.URL + "/substitute"})

	params.Set("openid.claimed_id", identity)
	params.Set("openid.identity", identity)
	params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+"valid")
	ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
	contexttest.LoadUser(t, ctx, 2)
	Security(ctx)
	require.Equal(t, http.StatusSeeOther, resp.Code)
	unittest.AssertExistsAndLoadBean(t, &user_model.UserOpenID{UID: 2, URI: identity})

	params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+"fresh-replay")
	params.Set("openid.claimed_id", provider.URL+"/another-identity")
	params.Set("openid.identity", provider.URL+"/another-identity")
	ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
	contexttest.LoadUser(t, ctx, 2)
	Security(ctx)
	assert.Equal(t, http.StatusForbidden, resp.Code)
	unittest.AssertNotExistsBean(t, &user_model.UserOpenID{URI: provider.URL + "/another-identity"})
}

func TestDeleteOpenIDReturnsNotFoundForOtherUsersAddress(t *testing.T) {
	unittest.PrepareTestEnv(t)
	ctx, _ := contexttest.MockContext(t, "POST /user/settings/security?id=1")
	contexttest.LoadUser(t, ctx, 2)
	DeleteOpenID(ctx)
	assert.Equal(t, http.StatusNotFound, ctx.Resp.WrittenStatus())
}

func TestToggleOpenIDVisibilityReturnsNotFoundForOtherUsersAddress(t *testing.T) {
	unittest.PrepareTestEnv(t)
	ctx, _ := contexttest.MockContext(t, "POST /user/settings/security?id=1")
	contexttest.LoadUser(t, ctx, 2)
	ToggleOpenIDVisibility(ctx)
	assert.Equal(t, http.StatusNotFound, ctx.Resp.WrittenStatus())
}

func TestOpenIDAssociationDiscovery(t *testing.T) {
	defer test.MockVariableValue(&setting.AppURL, "https://gitea.example/")()
	defer test.MockVariableValue(&setting.AppSubURL, "")()
	defer test.MockVariableValue(&setting.Security.AllowedHostList, "127.0.0.1:*")()
	for _, kind := range []string{"canonical", "delegated", "selection"} {
		t.Run(kind, func(t *testing.T) {
			unittest.PrepareTestEnv(t)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprint(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:true\n")
					return
				}
				if r.URL.Path == "/input" && kind == "canonical" {
					http.Redirect(w, r, "/claimed", http.StatusMovedPermanently)
					return
				}
				if r.URL.Path == "/input" && kind == "selection" {
					w.Header().Set("Content-Type", "application/xrds+xml")
					fmt.Fprintf(w, `<XRDS><XRD><Service><Type>http://specs.openid.net/auth/2.0/server</Type><URI>http://%s/provider</URI></Service></XRD></XRDS>`, r.Host)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				endpoint := "provider"
				if r.URL.Path == "/unrelated" {
					endpoint = "other-provider"
				}
				fmt.Fprintf(w, `<html><head><link rel="openid2.provider" href="http://%s/%s">`, r.Host, endpoint)
				if kind == "delegated" {
					fmt.Fprintf(w, `<link rel="openid2.local_id" href="http://%s/local">`, r.Host)
				}
				fmt.Fprint(w, "</head></html>")
			}))
			defer provider.Close()
			ctx, resp := contexttest.MockContext(t, "POST /user/settings/security/openid")
			contexttest.LoadUser(t, ctx, 2)
			web.SetForm(ctx, &forms.AddOpenIDForm{Openid: provider.URL + "/input"})
			OpenIDPost(ctx)
			require.Equal(t, http.StatusSeeOther, resp.Code)
			redirect, err := url.Parse(resp.Header().Get("Location"))
			require.NoError(t, err)
			callback, err := url.Parse(redirect.Query().Get("openid.return_to"))
			require.NoError(t, err)
			claimed := provider.URL + "/claimed"
			local := claimed
			if kind == "delegated" {
				claimed = provider.URL + "/input"
				local = provider.URL + "/local"
			}
			params := url.Values{
				"state":                 {callback.Query().Get("state")},
				"openid.ns":             {"http://specs.openid.net/auth/2.0"},
				"openid.mode":           {"id_res"},
				"openid.op_endpoint":    {provider.URL + "/provider"},
				"openid.claimed_id":     {claimed},
				"openid.identity":       {local},
				"openid.return_to":      {callback.String()},
				"openid.response_nonce": {time.Now().UTC().Format("2006-01-02T15:04:05Z") + kind},
				"openid.assoc_handle":   {"test"},
				"openid.signed":         {"op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
				"openid.sig":            {"test"},
			}
			if kind == "selection" {
				assert.Equal(t, "http://specs.openid.net/auth/2.0/identifier_select", redirect.Query().Get("openid.claimed_id"))
				params.Set("openid.claimed_id", provider.URL+"/unrelated")
				params.Set("openid.identity", provider.URL+"/unrelated")
				params.Set("openid.op_endpoint", provider.URL+"/other-provider")
				ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
				contexttest.LoadUser(t, ctx, 2)
				Security(ctx)
				assert.Equal(t, http.StatusForbidden, resp.Code)
				unittest.AssertNotExistsBean(t, &user_model.UserOpenID{URI: provider.URL + "/unrelated"})
				params.Set("openid.claimed_id", claimed)
				params.Set("openid.identity", local)
				params.Set("openid.op_endpoint", provider.URL+"/provider")
				params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+kind+"valid")
			}
			ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
			contexttest.LoadUser(t, ctx, 2)
			Security(ctx)
			require.Equal(t, http.StatusSeeOther, resp.Code)
			unittest.AssertExistsAndLoadBean(t, &user_model.UserOpenID{UID: 2, URI: claimed})
			params.Set("openid.response_nonce", time.Now().UTC().Format("2006-01-02T15:04:05Z")+kind+"replay")
			ctx, resp = contexttest.MockContext(t, "GET /user/settings/security?"+params.Encode())
			contexttest.LoadUser(t, ctx, 2)
			Security(ctx)
			assert.Equal(t, http.StatusForbidden, resp.Code)
		})
	}
}
