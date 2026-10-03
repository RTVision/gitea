// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues_test

import (
	"slices"
	"testing"

	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/optional"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/builder"
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

func TestForeignStackListScope(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	stack := &issues_model.PullRequestStack{RepoID: 1, TrunkBranch: "master", State: issues_model.StackStateOpen, Revision: 1}
	require.NoError(t, db.Insert(ctx, stack))
	for i, pullID := range []int64{1, 6} {
		require.NoError(t, db.Insert(ctx, &issues_model.StackEntry{StackID: stack.ID, PullRequestID: pullID, Position: i + 1}))
	}
	layers, err := issues_model.FindOpenStackLayers(ctx, 1)
	require.NoError(t, err)
	require.Len(t, layers, 2)
	assert.EqualValues(t, 3, layers[1].RepoID)
	stacks := issues_model.NewOpenStacks(layers)
	assert.EqualValues(t, 12, stacks.Stacks[stack.ID].Bottom.IssueID)
	opts := &issues_model.IssuesOptions{RepoCond: builder.Eq{"issue.repo_id": 1}.Or(builder.In("issue.id", []int64{12})), IsPull: optional.Some(true), IsClosed: optional.Some(false)}
	ids, _, err := issues_model.IssueIDs(ctx, opts)
	require.NoError(t, err)
	assert.Contains(t, ids, int64(12))
	assert.NotContains(t, ids, int64(8))
	leads := stacks.Leads([]int64{12})
	assert.Equal(t, []issues_model.IssueGroup{{StackID: stack.ID, IssueIDs: []int64{12}}}, leads.Rows([]int64{12}))
	stats, err := issues_model.GetIssueStats(ctx, opts)
	require.NoError(t, err)
	assert.EqualValues(t, len(ids), stats.OpenCount)
}
