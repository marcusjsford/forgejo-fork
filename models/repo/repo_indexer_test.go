// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo_test

import (
	"testing"

	"forgejo.org/models/db"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIndexerStatusPerBranch verifies that RepoIndexerStatus tracks a separate
// commit sha per (repo, indexer type, branch): indexing two branches of the same
// repo must not have one branch's push overwrite the other's stored "last indexed
// commit," which would corrupt the incremental diff base for whichever branch got
// stomped. See planning/stories/020-branch-aware-indexing/findings.md.
func TestIndexerStatusPerBranch(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

	// Fresh repo object per branch query — GetIndexerStatus's in-memory cache field
	// only ever holds one branch's status, mirroring how a real indexing job always
	// loads its own fresh Repository per (repo, branch) job.
	defaultStatus, err := repo_model.GetIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "")
	require.NoError(t, err)
	assert.Empty(t, defaultStatus.CommitSha)

	require.NoError(t, repo_model.UpdateIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "", "default-branch-sha"))
	require.NoError(t, repo_model.UpdateIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "feature/x", "feature-branch-sha"))

	repo = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	defaultStatus, err = repo_model.GetIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "")
	require.NoError(t, err)
	assert.Equal(t, "default-branch-sha", defaultStatus.CommitSha)

	repo = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	featureStatus, err := repo_model.GetIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "feature/x")
	require.NoError(t, err)
	assert.Equal(t, "feature-branch-sha", featureStatus.CommitSha)

	// Updating the feature branch again must not disturb the default branch's row.
	require.NoError(t, repo_model.UpdateIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "feature/x", "feature-branch-sha-2"))
	repo = unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	defaultStatus, err = repo_model.GetIndexerStatus(db.DefaultContext, repo, repo_model.RepoIndexerTypeCode, "")
	require.NoError(t, err)
	assert.Equal(t, "default-branch-sha", defaultStatus.CommitSha, "updating feature/x must not overwrite the default branch's stored sha")
}
