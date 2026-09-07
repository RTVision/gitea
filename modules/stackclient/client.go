// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package stackclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Owner   string
	Repo    string
}

type ErrRevision struct{ Current int64 }

func (e ErrRevision) Error() string {
	return fmt.Sprintf("stack revision changed; server revision is %d", e.Current)
}

type ErrForbidden struct{ Message string }

func (e ErrForbidden) Error() string { return e.Message }

type ErrNotFound struct{ Message string }

func (e ErrNotFound) Error() string { return e.Message }

type ErrDisabled struct{}

func (ErrDisabled) Error() string { return "stacked pull requests are disabled on this server" }

type ErrAmbiguousRemoteURL struct{ Remote string }

func (e ErrAmbiguousRemoteURL) Error() string {
	return fmt.Sprintf("cannot derive the Gitea URL from %s.\nSet GITEA_URL to the server root, for example https://gitea.example.com.", e.Remote)
}

type ErrAPI struct {
	Status  int
	Message string
}

func (e ErrAPI) Error() string { return fmt.Sprintf("Gitea API returned %d: %s", e.Status, e.Message) }

func New(baseURL, owner, repo, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("Gitea URL must be an http(s) URL without embedded credentials")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	if owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/\\") {
		return nil, errors.New("remote must identify an owner and repository")
	}
	originScheme, originHost := u.Scheme, u.Host
	httpClient := &http.Client{Timeout: 30 * time.Second}
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) != 0 && (!strings.EqualFold(req.URL.Host, originHost) || !strings.EqualFold(req.URL.Scheme, originScheme)) {
			return errors.New("refusing to send a Gitea request to a redirected host")
		}
		return nil
	}
	return &Client{BaseURL: strings.TrimSuffix(u.String(), "/"), Token: token, HTTP: httpClient, Owner: owner, Repo: repo}, nil
}

func FromRemote(remoteURL, token string) (*Client, error) {
	var host, path, inferredBase string
	if strings.Contains(remoteURL, "://") {
		u, err := url.Parse(remoteURL)
		if err != nil {
			return nil, err
		}
		host, path = u.Hostname(), strings.Trim(u.Path, "/")
		if u.Scheme == "http" || u.Scheme == "https" {
			inferredBase = u.Scheme + "://" + u.Host
		}
	} else {
		at := strings.LastIndex(remoteURL, "@")
		colon := strings.Index(remoteURL[at+1:], ":")
		if colon < 0 {
			return nil, errors.New("unsupported git remote URL")
		}
		colon += at + 1
		host, path = remoteURL[at+1:colon], strings.Trim(remoteURL[colon+1:], "/")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || host == "" {
		return nil, errors.New("git remote URL does not identify an owner and repository")
	}
	owner, repo := parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git")
	baseURL := os.Getenv("GITEA_URL")
	if baseURL == "" {
		if inferredBase == "" && len(parts) > 2 {
			return nil, ErrAmbiguousRemoteURL{Remote: remoteURL}
		}
		baseURL = inferredBase
		if baseURL == "" {
			baseURL = "https://" + host
		}
		if len(parts) > 2 && strings.Contains(remoteURL, "://") {
			baseURL += "/" + strings.Join(parts[:len(parts)-2], "/")
		}
	}
	return New(baseURL, owner, repo, token)
}

func (c *Client) repositoryPath(suffix string) string {
	return "/api/v1/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + suffix
}

func (c *Client) request(ctx context.Context, method, path string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "token "+c.Token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt+1 < attempts {
				continue
			}
			return err
		}
		responseData, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if result == nil || len(responseData) == 0 {
				return nil
			}
			return json.Unmarshal(responseData, result)
		}
		if method == http.MethodGet && resp.StatusCode >= 500 && attempt+1 < attempts {
			continue
		}
		var apiError struct {
			Message  string `json:"message"`
			Revision int64  `json:"revision"`
		}
		_ = json.Unmarshal(responseData, &apiError)
		if apiError.Message == "" {
			apiError.Message = strings.TrimSpace(string(responseData))
		}
		switch resp.StatusCode {
		case http.StatusConflict:
			return ErrRevision{Current: apiError.Revision}
		case http.StatusUnauthorized, http.StatusForbidden:
			return ErrForbidden{Message: apiError.Message}
		case http.StatusNotFound:
			return ErrNotFound{Message: apiError.Message}
		default:
			return ErrAPI{Status: resp.StatusCode, Message: apiError.Message}
		}
	}
	return errors.New("request failed")
}

func (c *Client) Capabilities(ctx context.Context) (*api.PullRequestStackCapabilities, error) {
	result := new(api.PullRequestStackCapabilities)
	if err := c.request(ctx, http.MethodGet, c.repositoryPath("/stacks/capabilities"), nil, result); err != nil {
		return nil, err
	}
	if !result.Enabled {
		return result, ErrDisabled{}
	}
	return result, nil
}

func (c *Client) ListStacks(ctx context.Context, page, limit int) ([]*api.PullRequestStack, error) {
	result := make([]*api.PullRequestStack, 0)
	query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	err := c.request(ctx, http.MethodGet, c.repositoryPath("/stacks")+"?"+query.Encode(), nil, &result)
	return result, err
}

func (c *Client) GetStack(ctx context.Context, number int64) (*api.PullRequestStack, error) {
	result := new(api.PullRequestStack)
	err := c.request(ctx, http.MethodGet, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)), nil, result)
	return result, err
}

func (c *Client) CreateStack(ctx context.Context, trunk string, pullRequests []int64) (*api.PullRequestStack, error) {
	result := new(api.PullRequestStack)
	err := c.request(ctx, http.MethodPost, c.repositoryPath("/stacks"), &api.CreatePullRequestStackOption{Trunk: trunk, PullRequests: pullRequests}, result)
	return result, err
}

func (c *Client) AppendStack(ctx context.Context, number, revision int64, pullRequests []int64) (*api.PullRequestStack, error) {
	result := new(api.PullRequestStack)
	err := c.request(ctx, http.MethodPatch, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)), &api.EditPullRequestStackOption{Revision: revision, PullRequests: pullRequests}, result)
	return result, err
}

func (c *Client) SynchronizeStack(ctx context.Context, number, revision int64, heads []api.PullRequestStackHead) (*api.PullRequestStack, error) {
	result := new(api.PullRequestStack)
	err := c.request(ctx, http.MethodPost, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)+"/sync"), &api.SynchronizePullRequestStackOption{Revision: revision, Heads: heads}, result)
	return result, err
}

func (c *Client) Unstack(ctx context.Context, number, revision int64) error {
	return c.request(ctx, http.MethodDelete, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)), &api.PullRequestStackRevisionOption{Revision: revision}, nil)
}

func (c *Client) startOperation(ctx context.Context, number, revision int64, path string, through int, mergeStyle string) (*api.PullRequestStackOperation, error) {
	result := new(api.PullRequestStackOperation)
	body := &api.PullRequestStackOperationOption{Revision: revision, ThroughPosition: through, MergeStyle: mergeStyle}
	err := c.request(ctx, http.MethodPost, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)+path), body, result)
	return result, err
}

func (c *Client) StartLand(ctx context.Context, number, revision int64, through int, mergeStyle string) (*api.PullRequestStackOperation, error) {
	return c.startOperation(ctx, number, revision, "/land", through, mergeStyle)
}

func (c *Client) StartRebase(ctx context.Context, number, revision int64, through int) (*api.PullRequestStackOperation, error) {
	return c.startOperation(ctx, number, revision, "/rebase", through, "")
}

func (c *Client) ListOperations(ctx context.Context, number int64, page, limit int) ([]*api.PullRequestStackOperation, error) {
	result := make([]*api.PullRequestStackOperation, 0)
	query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	err := c.request(ctx, http.MethodGet, c.repositoryPath("/stacks/"+strconv.FormatInt(number, 10)+"/operations")+"?"+query.Encode(), nil, &result)
	return result, err
}

func (c *Client) GetOperation(ctx context.Context, number, operation int64) (*api.PullRequestStackOperation, error) {
	result := new(api.PullRequestStackOperation)
	err := c.request(ctx, http.MethodGet, c.repositoryPath(fmt.Sprintf("/stacks/%d/operations/%d", number, operation)), nil, result)
	return result, err
}

func (c *Client) CancelOperation(ctx context.Context, number, operation int64) error {
	return c.request(ctx, http.MethodPost, c.repositoryPath(fmt.Sprintf("/stacks/%d/operations/%d/cancel", number, operation)), nil, nil)
}

func (c *Client) RetryOperation(ctx context.Context, number, operation int64) (*api.PullRequestStackOperation, error) {
	result := new(api.PullRequestStackOperation)
	err := c.request(ctx, http.MethodPost, c.repositoryPath(fmt.Sprintf("/stacks/%d/operations/%d/retry", number, operation)), nil, result)
	return result, err
}

func (c *Client) WaitOperation(ctx context.Context, number, operation int64, poll time.Duration) (*api.PullRequestStackOperation, error) {
	if poll <= 0 {
		poll = 3 * time.Second
	}
	for {
		op, err := c.GetOperation(ctx, number, operation)
		if err != nil {
			return nil, err
		}
		switch op.State {
		case "completed", "cancelled", "blocked", "failed":
			return op, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) GetPull(ctx context.Context, index int64) (*api.PullRequest, error) {
	result := new(api.PullRequest)
	err := c.request(ctx, http.MethodGet, c.repositoryPath("/pulls/"+strconv.FormatInt(index, 10)), nil, result)
	return result, err
}

func (c *Client) CreatePull(ctx context.Context, option api.CreatePullRequestOption) (*api.PullRequest, error) {
	result := new(api.PullRequest)
	err := c.request(ctx, http.MethodPost, c.repositoryPath("/pulls"), &option, result)
	return result, err
}

func (c *Client) EditPull(ctx context.Context, index int64, option api.EditPullRequestOption) (*api.PullRequest, error) {
	result := new(api.PullRequest)
	err := c.request(ctx, http.MethodPatch, c.repositoryPath("/pulls/"+strconv.FormatInt(index, 10)), &option, result)
	return result, err
}
