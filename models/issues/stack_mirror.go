// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"fmt"
	"slices"

	"gitea.dev/models/db"
	"gitea.dev/modules/globallock"

	"xorm.io/builder"
)

// StackMirrorLockKey serializes writers of a stack mirror branch with the paths that release it.
func StackMirrorLockKey(repoID int64, branch string) string {
	return fmt.Sprintf("stack_mirror_%d_%s", repoID, branch)
}

// LockStackMirrors holds the mirror locks of the stacks; callers take them before a transaction that releases mirrors.
func LockStackMirrors(ctx context.Context, cond builder.Cond) (func(), error) {
	var mirrors []struct {
		RepoID       int64
		MirrorBranch string
	}
	if err := db.GetEngine(ctx).Table("stack_entry").
		Join("INNER", "pull_request_stack", "pull_request_stack.id = stack_entry.stack_id").
		Where(cond).And("stack_entry.mirror_branch <> ''").
		Select("pull_request_stack.repo_id, stack_entry.mirror_branch").Find(&mirrors); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(mirrors))
	for _, mirror := range mirrors {
		keys = append(keys, StackMirrorLockKey(mirror.RepoID, mirror.MirrorBranch))
	}
	slices.Sort(keys) // a fixed order keeps two multi-mirror holders from deadlocking
	var releasers []globallock.ReleaseFunc
	release := func() {
		for _, releaser := range slices.Backward(releasers) {
			releaser()
		}
	}
	for _, key := range slices.Compact(keys) {
		releaser, err := globallock.Lock(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		releasers = append(releasers, releaser)
	}
	return release, nil
}

// LockPullStackMirrors holds the mirror locks of every stack a pull request belongs to.
func LockPullStackMirrors(ctx context.Context, pullID int64) (func(), error) {
	return LockStackMirrors(ctx, builder.In("stack_entry.stack_id", builder.Select("stack_id").From("stack_entry").Where(builder.Eq{"pull_request_id": pullID})))
}
