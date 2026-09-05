// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"testing"

	"gitea.dev/modelmigration/migrationtest"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/require"
)

func TestAddReviewIDToReaction(t *testing.T) {
	type Reaction struct {
		ID               int64              `xorm:"pk autoincr"`
		Type             string             `xorm:"INDEX UNIQUE(s) NOT NULL"`
		IssueID          int64              `xorm:"INDEX UNIQUE(s) NOT NULL"`
		CommentID        int64              `xorm:"INDEX UNIQUE(s)"`
		UserID           int64              `xorm:"INDEX UNIQUE(s) NOT NULL"`
		OriginalAuthorID int64              `xorm:"INDEX UNIQUE(s) NOT NULL DEFAULT(0)"`
		OriginalAuthor   string             `xorm:"INDEX UNIQUE(s)"`
		CreatedUnix      timeutil.TimeStamp `xorm:"INDEX created"`
	}
	x, deferable := migrationtest.PrepareTestEnv(t, 0, new(Reaction))
	defer deferable()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Exec("INSERT INTO reaction (id, type, issue_id, comment_id, user_id, original_author_id, original_author, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", 42, "heart", 1, 0, 1, 0, "migrated", 123)
	require.NoError(t, err)
	require.NoError(t, AddReviewIDToReaction(t.Context(), x))

	type migratedReaction struct {
		ID             int64
		OriginalAuthor string
		CreatedUnix    timeutil.TimeStamp
		ReviewID       int64
	}
	var migrated migratedReaction
	has, err := x.SQL("SELECT id, original_author, created_unix, review_id FROM reaction WHERE id = ?", 42).Get(&migrated)
	require.NoError(t, err)
	require.True(t, has)
	require.EqualValues(t, 42, migrated.ID)
	require.Equal(t, "migrated", migrated.OriginalAuthor)
	require.Equal(t, timeutil.TimeStamp(123), migrated.CreatedUnix)
	require.Zero(t, migrated.ReviewID)

	// The same user may react with the same emoji to an issue, a comment, and a review.
	// A second reaction to the same review must remain prohibited by the composite key.
	_, err = x.Exec("INSERT INTO reaction (type, issue_id, comment_id, review_id, user_id, original_author_id, original_author, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", "eyes", 2, 0, 0, 2, 0, "", 200)
	require.NoError(t, err)
	_, err = x.Exec("INSERT INTO reaction (type, issue_id, comment_id, review_id, user_id, original_author_id, original_author, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", "eyes", 2, 8, 0, 2, 0, "", 201)
	require.NoError(t, err)
	_, err = x.Exec("INSERT INTO reaction (type, issue_id, comment_id, review_id, user_id, original_author_id, original_author, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", "eyes", 2, 0, 7, 2, 0, "", 202)
	require.NoError(t, err)
	_, err = x.Exec("INSERT INTO reaction (type, issue_id, comment_id, review_id, user_id, original_author_id, original_author, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", "eyes", 2, 0, 7, 2, 0, "", 203)
	require.Error(t, err)
}
