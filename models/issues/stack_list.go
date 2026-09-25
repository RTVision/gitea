// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"cmp"
	"context"
	"slices"

	"gitea.dev/models/db"
)

// StackLayer is one pull request of an open stack, as the pull request list shows it.
type StackLayer struct {
	StackID   int64
	Mode      string
	Position  int
	IssueID   int64
	Index     int64
	Title     string
	IsClosed  bool
	HasMerged bool
	Stack     *StackSummary `xorm:"-"`
}

// StackSummary aggregates every layer of an open stack, including layers outside the listed page.
type StackSummary struct {
	ID     int64
	Mode   string
	Size   int
	Merged int
	Open   int
	Closed int
	Bottom *StackLayer // lowest open layer, which lands next; the first layer when none is open
}

// OpenStacks indexes a repository's open stacks by stack and by layer issue.
type OpenStacks struct {
	Layers map[int64]*StackLayer
	Stacks map[int64]*StackSummary
}

// IssueGroup is one pull list row: a lone issue, or the listed layers of one stack in landing order.
type IssueGroup struct {
	StackID  int64
	IssueIDs []int64
}

// FindOpenStackLayers lists every layer of a repository's open stacks with one query.
func FindOpenStackLayers(ctx context.Context, repoID int64) ([]*StackLayer, error) {
	layers := make([]*StackLayer, 0)
	err := db.GetEngine(ctx).Table("stack_entry").
		Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
		Join("INNER", "pull_request", "pull_request.id = stack_entry.pull_request_id").
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Where("pull_request_stack.repo_id = ? AND pull_request_stack.state = ?", repoID, StackStateOpen).
		Select("stack_entry.stack_id, pull_request_stack.mode, stack_entry.position, issue.id AS issue_id, issue.`index`, issue.name AS title, issue.is_closed, pull_request.has_merged").
		Asc("stack_entry.stack_id", "stack_entry.position").
		Find(&layers)
	return layers, err
}

// NewOpenStacks summarizes layers sorted by stack and position.
func NewOpenStacks(layers []*StackLayer) *OpenStacks {
	stacks := &OpenStacks{Layers: make(map[int64]*StackLayer, len(layers)), Stacks: make(map[int64]*StackSummary)}
	for _, layer := range layers {
		summary := stacks.Stacks[layer.StackID]
		if summary == nil {
			summary = &StackSummary{ID: layer.StackID, Mode: layer.Mode, Bottom: layer}
			stacks.Stacks[layer.StackID] = summary
		}
		summary.Size++
		switch {
		case layer.HasMerged:
			summary.Merged++
		case layer.IsClosed:
			summary.Closed++
		default:
			summary.Open++
			if summary.Open == 1 {
				summary.Bottom = layer
			}
		}
		layer.Stack = summary
		stacks.Layers[layer.IssueID] = layer
	}
	return stacks
}

// Group collapses sorted issue IDs into rows, placing each stack at its first listed layer.
func (s *OpenStacks) Group(issueIDs []int64) []IssueGroup {
	groups := make([]IssueGroup, 0, len(issueIDs))
	stackRows := make(map[int64]int)
	for _, id := range issueIDs {
		layer := s.Layers[id]
		if layer == nil {
			groups = append(groups, IssueGroup{IssueIDs: []int64{id}})
			continue
		}
		if row, ok := stackRows[layer.StackID]; ok {
			groups[row].IssueIDs = append(groups[row].IssueIDs, id)
			continue
		}
		stackRows[layer.StackID] = len(groups)
		groups = append(groups, IssueGroup{StackID: layer.StackID, IssueIDs: []int64{id}})
	}
	for _, group := range groups {
		if group.StackID == 0 {
			continue
		}
		slices.SortFunc(group.IssueIDs, func(a, b int64) int { return cmp.Compare(s.Layers[a].Position, s.Layers[b].Position) })
	}
	return groups
}
