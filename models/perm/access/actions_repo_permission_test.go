// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package access

import (
	"testing"

	actions_model "gitea.dev/models/actions"
	"gitea.dev/models/db"
	perm_model "gitea.dev/models/perm"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetActionsUserRepoPermission(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	// Use fixtures for repos and users
	repo4 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 4})   // Public, Owner 5, has Actions unit
	repo2 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})   // Private, Owner 2, no Actions unit in fixtures
	repo15 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 15}) // Private, Owner 2, no Actions unit in fixtures
	owner2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	actionsUser := user_model.NewActionsUser()

	// Ensure repo2 and repo15 have Actions units for testing configuration
	for _, r := range []*repo_model.Repository{repo2, repo15} {
		require.NoError(t, db.Insert(ctx, &repo_model.RepoUnit{
			RepoID: r.ID,
			Type:   unit.TypeActions,
			Config: &repo_model.ActionsConfig{},
		}))
	}

	t.Run("SameRepo_Public", func(t *testing.T) {
		task47 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})
		require.Equal(t, repo4.ID, task47.RepoID)

		perm, err := GetActionsUserRepoPermission(ctx, repo4, actionsUser, task47.ID)
		require.NoError(t, err)

		// Public repo, bot should have Read access even if not collaborator
		assert.Equal(t, perm_model.AccessModeNone, perm.AccessMode)
		assert.True(t, perm.CanRead(unit.TypeCode))
	})

	t.Run("SameRepo_Private", func(t *testing.T) {
		// Use Task 53 which is already in Repo 2 (Private)
		task53 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 53})
		require.Equal(t, repo2.ID, task53.RepoID)

		perm, err := GetActionsUserRepoPermission(ctx, repo2, actionsUser, task53.ID)
		require.NoError(t, err)

		// Private repo, bot has no base access, but gets Write from effective tokens perms (Permissive by default)
		assert.Equal(t, perm_model.AccessModeNone, perm.AccessMode)
		assert.True(t, perm.CanWrite(unit.TypeCode))
	})

	t.Run("CrossRepo_Denied_None", func(t *testing.T) {
		task53 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 53})

		// Set owner policy to nil allowed repos (None)
		cfg := actions_model.OwnerActionsConfig{}
		require.NoError(t, actions_model.SetOwnerActionsConfig(ctx, owner2.ID, cfg))

		perm, err := GetActionsUserRepoPermission(ctx, repo15, actionsUser, task53.ID)
		require.NoError(t, err)

		// Should NOT have access to the private repo.
		assert.False(t, perm.CanRead(unit.TypeCode))
	})

	// setForkPR marks task 53 and its run 795 as triggered by a fork PR via the given event
	setForkPR := func(t *testing.T, isFork bool, triggerEvent string) {
		t.Helper()
		task53 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 53})
		task53.IsForkPullRequest = isFork
		require.NoError(t, actions_model.UpdateTask(ctx, task53, "is_fork_pull_request"))
		_, err := db.GetEngine(ctx).Table("action_run").Where("id = ?", 795).Update(map[string]any{"is_fork_pull_request": isFork, "trigger_event": triggerEvent})
		require.NoError(t, err)
	}

	t.Run("ForkPR_NoCrossRepo", func(t *testing.T) {
		// Policy contains repo15
		cfg := actions_model.OwnerActionsConfig{
			AllowedCrossRepoIDs: []int64{repo15.ID},
		}
		require.NoError(t, actions_model.SetOwnerActionsConfig(ctx, owner2.ID, cfg))
		defer setForkPR(t, false, "push")

		for _, tc := range []struct {
			isFork       bool
			triggerEvent string
			allowed      bool
		}{
			{isFork: true, triggerEvent: "pull_request", allowed: false},
			{isFork: true, triggerEvent: "pull_request_target", allowed: true}, // runs in the base branch's context
			{isFork: false, triggerEvent: "pull_request", allowed: true},
		} {
			setForkPR(t, tc.isFork, tc.triggerEvent)
			perm, err := GetActionsUserRepoPermission(ctx, repo15, actionsUser, 53)
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, perm.CanRead(unit.TypeCode), "fork=%v event=%s", tc.isFork, tc.triggerEvent)
			assert.False(t, perm.CanWrite(unit.TypeCode))

			run := &actions_model.ActionRun{RepoID: repo2.ID, Repo: repo2, IsForkPullRequest: tc.isFork, TriggerEvent: tc.triggerEvent}
			allowed, err := CanReadWorkflowCrossRepo(ctx, repo15, run)
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, allowed, "fork=%v event=%s", tc.isFork, tc.triggerEvent)
		}

		publicCaller := *repo2
		publicCaller.IsPrivate = false
		run := &actions_model.ActionRun{RepoID: repo2.ID, Repo: &publicCaller}
		allowed, err := CanReadWorkflowCrossRepo(ctx, repo15, run)
		require.NoError(t, err)
		assert.False(t, allowed)
	})

	t.Run("CollaborativeOwner_ForkPR_Denied", func(t *testing.T) {
		// Target repo15 trusts repo2's owner as a collaborative owner.
		repo15ActionsUnit := repo15.MustGetUnit(ctx, unit.TypeActions)
		repo15ActionsUnit.ActionsConfig().AddCollaborativeOwner(owner2.ID)
		require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repo15ActionsUnit))

		// Owner cross-repo policy does not allow repo15, so the only branch that
		// could grant access is the collaborative-owner one.
		require.NoError(t, actions_model.SetOwnerActionsConfig(ctx, owner2.ID, actions_model.OwnerActionsConfig{}))

		// Non-fork task is legitimately granted code-read via collaborative owner.
		setForkPR(t, false, "pull_request")
		perm, err := GetActionsUserRepoPermission(ctx, repo15, actionsUser, 53)
		require.NoError(t, err)
		assert.True(t, perm.CanRead(unit.TypeCode))

		// Fork PR must NOT be able to read a third private repo through the
		// collaborative-owner branch.
		setForkPR(t, true, "pull_request")
		perm, err = GetActionsUserRepoPermission(ctx, repo15, actionsUser, 53)
		require.NoError(t, err)
		assert.False(t, perm.CanRead(unit.TypeCode))

		setForkPR(t, true, "pull_request_target")
		perm, err = GetActionsUserRepoPermission(ctx, repo15, actionsUser, 53)
		require.NoError(t, err)
		assert.True(t, perm.CanRead(unit.TypeCode))

		// Reusable workflows use a separate authorization path and must enforce
		// the same fork-PR restriction.
		run := &actions_model.ActionRun{RepoID: repo2.ID, IsForkPullRequest: true}
		allowed, err := CanReadWorkflowCrossRepo(ctx, repo15, run)
		require.NoError(t, err)
		assert.False(t, allowed)

		run.TriggerEvent = "pull_request_target"
		allowed, err = CanReadWorkflowCrossRepo(ctx, repo15, run)
		require.NoError(t, err)
		assert.True(t, allowed)

		run.IsForkPullRequest = false
		run.TriggerEvent = ""
		allowed, err = CanReadWorkflowCrossRepo(ctx, repo15, run)
		require.NoError(t, err)
		assert.True(t, allowed)

		// Restore state for subsequent subtests.
		setForkPR(t, false, "push")
		repo15ActionsUnit.ActionsConfig().RemoveCollaborativeOwner(owner2.ID)
		require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repo15ActionsUnit))
	})

	t.Run("Inheritance_And_Clamping", func(t *testing.T) {
		task53 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 53})
		task53.IsForkPullRequest = false
		require.NoError(t, actions_model.UpdateTask(ctx, task53, "is_fork_pull_request"))

		// Owner policy: Restricted mode (Read-only Code)
		ownerCfg := actions_model.OwnerActionsConfig{
			TokenPermissionMode: repo_model.ActionsTokenPermissionModeRestricted,
			MaxTokenPermissions: &repo_model.ActionsTokenPermissions{
				UnitAccessModes: map[unit.Type]perm_model.AccessMode{
					unit.TypeCode: perm_model.AccessModeRead,
				},
			},
		}
		require.NoError(t, actions_model.SetOwnerActionsConfig(ctx, owner2.ID, ownerCfg))

		// Repo policy: OverrideOwnerConfig = false (should inherit owner's restricted mode)
		repo2ActionsUnit := repo2.MustGetUnit(ctx, unit.TypeActions)
		repo2ActionsCfg := repo2ActionsUnit.ActionsConfig()
		repo2ActionsCfg.OverrideOwnerConfig = false
		require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repo2ActionsUnit))

		perm, err := GetActionsUserRepoPermission(ctx, repo2, actionsUser, task53.ID)
		require.NoError(t, err)

		// Should be clamped to Read-only
		assert.Equal(t, perm_model.AccessModeRead, perm.UnitAccessMode(unit.TypeCode))
		assert.False(t, perm.CanWrite(unit.TypeCode))
	})

	t.Run("RepoOverride_Clamping", func(t *testing.T) {
		task53 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 53})

		// Owner policy: Permissive (Write access)
		ownerCfg := actions_model.OwnerActionsConfig{
			TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
		}
		require.NoError(t, actions_model.SetOwnerActionsConfig(ctx, owner2.ID, ownerCfg))

		// Repo policy: OverrideOwnerConfig = true, MaxTokenPermissions = Read
		repo2ActionsUnit := repo2.MustGetUnit(ctx, unit.TypeActions)
		repo2ActionsCfg := repo2ActionsUnit.ActionsConfig()
		repo2ActionsCfg.OverrideOwnerConfig = true
		repo2ActionsCfg.TokenPermissionMode = repo_model.ActionsTokenPermissionModeRestricted
		repo2ActionsCfg.MaxTokenPermissions = &repo_model.ActionsTokenPermissions{
			UnitAccessModes: map[unit.Type]perm_model.AccessMode{
				unit.TypeCode: perm_model.AccessModeRead,
			},
		}
		require.NoError(t, repo_model.UpdateRepoUnitConfig(ctx, repo2ActionsUnit))

		perm, err := GetActionsUserRepoPermission(ctx, repo2, actionsUser, task53.ID)
		require.NoError(t, err)

		// Should be clamped to Read-only
		assert.Equal(t, perm_model.AccessModeRead, perm.UnitAccessMode(unit.TypeCode))
	})
}
