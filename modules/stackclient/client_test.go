// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package stackclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, body string, header http.Header) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}
}

func TestClientStackRequestsAndRevisionConflict(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		assert.Equal(t, "token secret", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gitea/api/v1/repos/acme/widget/stacks/12":
			return response(http.StatusOK, `{"number":12,"trunk":"main","revision":4,"entries":[]}`, nil), nil
		case r.Method == http.MethodPatch && r.URL.Path == "/gitea/api/v1/repos/acme/widget/stacks/12":
			return response(http.StatusConflict, `{"revision":5}`, nil), nil
		default:
			return response(http.StatusNotFound, `{"message":"missing"}`, nil), nil
		}
	})

	client, err := New("https://code.example.test/gitea", "acme", "widget", "secret")
	require.NoError(t, err)
	client.HTTP.Transport = transport
	stack, err := client.GetStack(context.Background(), 12)
	require.NoError(t, err)
	assert.Equal(t, int64(4), stack.Revision)
	_, err = client.AppendStack(context.Background(), 12, 4, []int64{9})
	var revision ErrRevision
	require.ErrorAs(t, err, &revision)
	assert.Equal(t, int64(5), revision.Current)
	assert.Equal(t, 2, requests, "mutations must not be retried")
}

func TestClientGetRetriesServerFailure(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return response(http.StatusServiceUnavailable, "", nil), nil
		}
		return response(http.StatusOK, `{"number":3,"entries":[]}`, nil), nil
	})
	client, err := New("https://code.example.test", "acme", "widget", "")
	require.NoError(t, err)
	client.HTTP.Transport = transport
	_, err = client.GetStack(t.Context(), 3)
	require.NoError(t, err)
	assert.Equal(t, 2, requests)
}

func TestFromRemote(t *testing.T) {
	t.Setenv("GITEA_URL", "https://code.example.test/gitea")
	client, err := FromRemote("git@code.example.test:acme/widget.git", "secret")
	require.NoError(t, err)
	assert.Equal(t, "https://code.example.test/gitea", client.BaseURL)
	assert.Equal(t, "acme", client.Owner)
	assert.Equal(t, "widget", client.Repo)
	_, err = New("ssh://code.example.test", "acme", "widget", "")
	assert.Error(t, err)
	t.Setenv("GITEA_URL", "")
	client, err = FromRemote("http://localhost:3000/gitea/acme/widget.git", "secret")
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:3000/gitea", client.BaseURL)
}

func TestFromRemoteRequiresBaseURLForAmbiguousSSHPath(t *testing.T) {
	for _, remote := range []string{"git@code.example.test:/srv/git/acme/widget.git", "ssh://git@code.example.test/srv/git/acme/widget.git"} {
		t.Run(remote, func(t *testing.T) {
			t.Setenv("GITEA_URL", "")
			_, err := FromRemote(remote, "secret")
			var ambiguous ErrAmbiguousRemoteURL
			require.ErrorAs(t, err, &ambiguous)
			assert.Contains(t, err.Error(), "Set GITEA_URL")

			t.Setenv("GITEA_URL", "https://code.example.test/gitea")
			client, err := FromRemote(remote, "secret")
			require.NoError(t, err)
			assert.Equal(t, "https://code.example.test/gitea", client.BaseURL)
			assert.Equal(t, "acme", client.Owner)
			assert.Equal(t, "widget", client.Repo)
		})
	}
}

func TestClientRejectsRedirectToAnotherHost(t *testing.T) {
	requests := 0
	client, err := New("https://code.example.test", "acme", "widget", "secret")
	require.NoError(t, err)
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusFound, "", http.Header{"Location": {"https://evil.example.test/steal"}}), nil
	})
	_, err = client.GetStack(t.Context(), 1)
	assert.Error(t, err)
	assert.Equal(t, 2, requests, "the idempotent GET retries, but neither request reaches the redirected host")
	var apiErr ErrAPI
	assert.NotErrorAs(t, err, &apiErr)
}
