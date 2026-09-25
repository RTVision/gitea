// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues_test

import (
	"slices"
	"testing"

	issues_model "gitea.dev/models/issues"

	"github.com/stretchr/testify/assert"
)

func TestOpenStackLeads(t *testing.T) {
	stacks := issues_model.NewOpenStacks([]*issues_model.StackLayer{
		{StackID: 1, Position: 1, IssueID: 10, HasMerged: true, IsClosed: true},
		{StackID: 1, Position: 2, IssueID: 11},
		{StackID: 1, Position: 3, IssueID: 12},
		{StackID: 2, Position: 1, IssueID: 20, IsClosed: true},
		{StackID: 2, Position: 2, IssueID: 21},
		{StackID: 3, Position: 1, IssueID: 30, HasMerged: true, IsClosed: true},
	})
	list := []int64{12, 5, 21, 11, 6, 7} // matching issues in list order
	members := slices.DeleteFunc(slices.Clone(list), func(id int64) bool { return stacks.Layers[id] == nil })
	leads := stacks.Leads(members)
	assert.Equal(t, []int64{11}, leads.Hidden)
	visible := slices.DeleteFunc(slices.Clone(list), func(id int64) bool { return slices.Contains(leads.Hidden, id) })
	var rows []issues_model.IssueGroup
	for page := range slices.Chunk(visible, 2) { // the database pages the list without hidden layers
		rows = append(rows, leads.Rows(page)...)
	}
	assert.Equal(t, []issues_model.IssueGroup{
		{StackID: 1, IssueIDs: []int64{11, 12}},
		{IssueIDs: []int64{5}},
		{StackID: 2, IssueIDs: []int64{21}},
		{IssueIDs: []int64{6}},
		{IssueIDs: []int64{7}},
	}, rows)
	assert.Empty(t, stacks.Leads(nil).Groups)

	first, second, landed := stacks.Stacks[1], stacks.Stacks[2], stacks.Stacks[3]
	assert.Equal(t, [3]int{1, 2, 0}, [3]int{first.Merged, first.Open, first.Closed})
	assert.Len(t, first.Layers, 3)
	assert.EqualValues(t, 11, first.Bottom.IssueID)
	assert.Equal(t, [3]int{0, 1, 1}, [3]int{second.Merged, second.Open, second.Closed})
	assert.EqualValues(t, 21, second.Bottom.IssueID)
	assert.EqualValues(t, 30, landed.Bottom.IssueID)
	assert.Same(t, first, stacks.Layers[12].Stack)
}
