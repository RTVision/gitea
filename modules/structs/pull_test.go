// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullRequestTrackingFieldsNullable(t *testing.T) {
	var pullRequest PullRequest
	require.NoError(t, json.Unmarshal([]byte(`{"review_decision":null,"checks_state":null}`), &pullRequest))
	assert.Nil(t, pullRequest.ReviewDecision)
	assert.Nil(t, pullRequest.ChecksState)

	reviewDecision := PullRequestReviewApproved
	checksState := PullRequestChecksPassing
	pullRequest.ReviewDecision = &reviewDecision
	pullRequest.ChecksState = &checksState
	payload, err := json.Marshal(&pullRequest)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"review_decision":"approved"`)
	assert.Contains(t, string(payload), `"checks_state":"passing"`)
}
