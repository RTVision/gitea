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
	StackID        int64
	Mode           string
	Position       int
	IssueID        int64
	Index          int64
	Title          string
	IsClosed       bool
	HasMerged      bool
	OperationState string
	Stack          *StackSummary `xorm:"-"`
}

// StackSummary aggregates every layer of an open stack, including layers outside the listed page.
type StackSummary struct {
	ID        int64
	Mode      string
	Operation string // state of the active operation, if any
	Layers    []*StackLayer
	Merged    int
	Open      int
	Closed    int
	Bottom    *StackLayer // lowest open layer, which lands next; the first layer when none is open
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

// StackLeads places each stack at its first listed layer, the lead, and hides its other listed layers.
type StackLeads struct {
	Groups map[int64]IssueGroup // by lead issue ID
	Hidden []int64
}

// FindOpenStackLayers lists every layer of a repository's open stacks with one query.
func FindOpenStackLayers(ctx context.Context, repoID int64) ([]*StackLayer, error) {
	layers := make([]*StackLayer, 0)
	err := db.GetEngine(ctx).Table("stack_entry").
		Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
		Join("INNER", "pull_request", "pull_request.id = stack_entry.pull_request_id").
		Join("INNER", "issue", "issue.id = pull_request.issue_id").
		Join("LEFT", "stack_operation", "stack_operation.id = pull_request_stack.active_operation_id").
		Where("pull_request_stack.repo_id = ? AND pull_request_stack.state = ?", repoID, StackStateOpen).
		Select("stack_entry.stack_id, pull_request_stack.mode, stack_entry.position, issue.id AS issue_id, issue.`index`, issue.name AS title, issue.is_closed, pull_request.has_merged, stack_operation.state AS operation_state").
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
			summary = &StackSummary{ID: layer.StackID, Mode: layer.Mode, Operation: layer.OperationState, Bottom: layer}
			stacks.Stacks[layer.StackID] = summary
		}
		summary.Layers = append(summary.Layers, layer)
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

// Leads groups the listed layers of open stacks, given in list order.
func (s *OpenStacks) Leads(memberIDs []int64) *StackLeads {
	leads := &StackLeads{Groups: make(map[int64]IssueGroup)}
	leadOf := make(map[int64]int64)
	for _, id := range memberIDs {
		stackID := s.Layers[id].StackID
		lead, ok := leadOf[stackID]
		if !ok {
			leadOf[stackID] = id
			leads.Groups[id] = IssueGroup{StackID: stackID, IssueIDs: []int64{id}}
			continue
		}
		group := leads.Groups[lead]
		group.IssueIDs = append(group.IssueIDs, id)
		leads.Groups[lead] = group
		leads.Hidden = append(leads.Hidden, id)
	}
	for _, group := range leads.Groups {
		slices.SortFunc(group.IssueIDs, func(a, b int64) int { return cmp.Compare(s.Layers[a].Position, s.Layers[b].Position) })
	}
	return leads
}

// Rows expands a page of list IDs, which excludes hidden layers, into rows.
func (l *StackLeads) Rows(pageIDs []int64) []IssueGroup {
	rows := make([]IssueGroup, 0, len(pageIDs))
	for _, id := range pageIDs {
		group, ok := l.Groups[id]
		if !ok {
			group = IssueGroup{IssueIDs: []int64{id}}
		}
		rows = append(rows, group)
	}
	return rows
}

// IssueIDs lists the open stack layers' issues.
func (s *OpenStacks) IssueIDs() []int64 {
	ids := make([]int64, 0, len(s.Layers))
	for id := range s.Layers {
		ids = append(ids, id)
	}
	return ids
}
