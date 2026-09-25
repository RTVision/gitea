// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues_test

import (
	"testing"

	issues_model "gitea.dev/models/issues"

	"github.com/stretchr/testify/assert"
)

func TestOpenStacksGroup(t *testing.T) {
	stacks := issues_model.NewOpenStacks([]*issues_model.StackLayer{
		{StackID: 1, Position: 1, IssueID: 10, HasMerged: true, IsClosed: true},
		{StackID: 1, Position: 2, IssueID: 11},
		{StackID: 1, Position: 3, IssueID: 12},
		{StackID: 2, Position: 1, IssueID: 20, IsClosed: true},
		{StackID: 2, Position: 2, IssueID: 21},
		{StackID: 3, Position: 1, IssueID: 30, HasMerged: true, IsClosed: true},
	})
	assert.Equal(t, []issues_model.IssueGroup{
		{StackID: 1, IssueIDs: []int64{11, 12}},
		{IssueIDs: []int64{5}},
		{StackID: 2, IssueIDs: []int64{21}},
		{IssueIDs: []int64{6}},
	}, stacks.Group([]int64{12, 5, 21, 11, 6}))
	assert.Empty(t, stacks.Group(nil))

	first, second, landed := stacks.Stacks[1], stacks.Stacks[2], stacks.Stacks[3]
	assert.Equal(t, [3]int{1, 2, 0}, [3]int{first.Merged, first.Open, first.Closed})
	assert.Equal(t, 3, first.Size)
	assert.EqualValues(t, 11, first.Bottom.IssueID)
	assert.Equal(t, [3]int{0, 1, 1}, [3]int{second.Merged, second.Open, second.Closed})
	assert.EqualValues(t, 21, second.Bottom.IssueID)
	assert.EqualValues(t, 30, landed.Bottom.IssueID)
	assert.Same(t, first, stacks.Layers[12].Stack)
}
