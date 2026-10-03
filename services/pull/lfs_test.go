// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"

	git_model "gitea.dev/models/git"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/lfs"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/storage"
	"gitea.dev/modules/test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLFSPublicationLandingTarget(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()
	objectStorage, err := storage.NewLocalStorage(ctx, &setting.Storage{Path: t.TempDir()})
	require.NoError(t, err)
	defer test.MockVariableValue(&storage.LFS, objectStorage)()
	content := "fork object"
	pointer := lfs.Pointer{Oid: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), Size: int64(len(content))}
	require.NoError(t, lfs.NewContentStore().Put(pointer, strings.NewReader(content)))
	_, err = git_model.NewLFSMetaObject(ctx, 1, pointer)
	require.NoError(t, err)
	batch := fmt.Sprintf("%040d blob %d\n%s\n", 0, len(pointer.StringContent()), pointer.StringContent())
	pr := &issues_model.PullRequest{BaseRepoID: 1, HeadRepoID: 1}
	target := &Target{Repo: &repo_model.Repository{ID: 2}, Branch: "main"}
	require.NoError(t, createLFSMetaObjectsFromCatFileBatch(ctx, io.NopCloser(strings.NewReader(batch)), pr, target))
	_, err = git_model.GetLFSMetaObjectByOid(ctx, 2, pointer.Oid)
	require.NoError(t, err)
	pr.HeadRepoID = 3
	target.Repo.ID = 4
	require.NoError(t, createLFSMetaObjectsFromCatFileBatch(ctx, io.NopCloser(strings.NewReader(batch)), pr, target))
	_, err = git_model.GetLFSMetaObjectByOid(ctx, 4, pointer.Oid)
	assert.ErrorIs(t, err, git_model.ErrLFSObjectNotExist)
}
