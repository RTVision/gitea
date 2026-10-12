// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"errors"

	"gitea.dev/models/db"
	project_model "gitea.dev/models/project"
	repo_model "gitea.dev/models/repo"

	"xorm.io/builder"
)

// MovePullRequest renumbers an open pull request into repo with a new base branch, keeping its discussion and
// leaving a redirect at its old index. Labels, milestones, projects and pins of the old repository are dropped.
func MovePullRequest(ctx context.Context, pr *PullRequest, repo *repo_model.Repository, baseBranch string) error {
	if !db.InTransaction(ctx) {
		return errors.New("moving a pull request requires a transaction")
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return err
	}
	issue := pr.Issue
	if issue.RepoID == repo.ID || issue.IsClosed || pr.HasMerged {
		return ErrInvalidStack
	}
	oldRepoID, oldIndex := issue.RepoID, issue.Index
	index, err := db.GetNextResourceIndex(ctx, "issue_index", repo.ID)
	if err != nil {
		return err
	}
	if err := db.Insert(ctx, &IssueRedirect{RepoID: oldRepoID, Index: oldIndex, IssueID: issue.ID}); err != nil {
		return err
	}

	var labelIDs []int64
	outside := builder.Select("id").From("label").Where(builder.Neq{"repo_id": repo.ID}.And(builder.Neq{"org_id": repo.OwnerID}))
	if err := db.GetEngine(ctx).Table("issue_label").Where("issue_id = ?", issue.ID).And(builder.In("label_id", outside)).Cols("label_id").Find(&labelIDs); err != nil {
		return err
	}
	if len(labelIDs) > 0 {
		if _, err := db.GetEngine(ctx).Where("issue_id = ?", issue.ID).In("label_id", labelIDs).Delete(new(IssueLabel)); err != nil {
			return err
		}
	}
	var projectIDs []int64
	foreign := builder.Select("id").From("project").Where(builder.Eq{"repo_id": oldRepoID}.Or(builder.Neq{"owner_id": repo.OwnerID}))
	if err := db.GetEngine(ctx).Table("project_issue").Where("issue_id = ?", issue.ID).And(builder.In("project_id", foreign)).Cols("project_id").Find(&projectIDs); err != nil {
		return err
	}
	if len(projectIDs) > 0 {
		if _, err := db.GetEngine(ctx).Where("issue_id = ?", issue.ID).In("project_id", projectIDs).Delete(new(project_model.ProjectIssue)); err != nil {
			return err
		}
	}
	if _, err := db.GetEngine(ctx).Where("issue_id = ?", issue.ID).Delete(new(IssuePin)); err != nil {
		return err
	}
	milestoneID := issue.MilestoneID

	issue.RepoID, issue.Index, issue.MilestoneID, issue.Repo = repo.ID, index, 0, repo
	if _, err := db.GetEngine(ctx).ID(issue.ID).NoAutoTime().Cols("repo_id", "index", "milestone_id").Update(issue); err != nil {
		return err
	}
	pr.Index, pr.BaseRepoID, pr.BaseBranch, pr.BaseRepo = index, repo.ID, baseBranch, repo
	if _, err := db.GetEngine(ctx).ID(pr.ID).Cols("index", "base_repo_id", "base_branch").Update(pr); err != nil {
		return err
	}
	if _, err := db.GetEngine(ctx).Where("issue_id = ?", issue.ID).Cols("repo_id").Update(&repo_model.Attachment{RepoID: repo.ID}); err != nil {
		return err
	}
	if _, err := db.GetEngine(ctx).Table("notification").Where("issue_id = ?", issue.ID).Update(map[string]any{"repo_id": repo.ID}); err != nil {
		return err
	}
	if _, err := db.GetEngine(ctx).Where("ref_issue_id = ?", issue.ID).Cols("ref_repo_id").Update(&Comment{RefRepoID: repo.ID}); err != nil {
		return err
	}

	for _, id := range labelIDs {
		if err := updateLabelCols(ctx, &Label{ID: id}, "num_issues", "num_closed_issue"); err != nil {
			return err
		}
	}
	if milestoneID != 0 {
		if err := UpdateMilestoneCounters(ctx, milestoneID); err != nil {
			return err
		}
	}
	if err := DecrRepoIssueNumbers(ctx, oldRepoID, true, true, false); err != nil {
		return err
	}
	return IncrRepoIssueNumbers(ctx, repo.ID, true, true)
}
