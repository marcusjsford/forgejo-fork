// Copyright 2017 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"context"
	"fmt"

	"forgejo.org/models/db"

	"xorm.io/builder"
)

// RepoIndexerType specifies the repository indexer type
type RepoIndexerType int //revive:disable-line:exported

const (
	// RepoIndexerTypeCode code indexer
	RepoIndexerTypeCode RepoIndexerType = iota // 0
	// RepoIndexerTypeStats repository stats indexer
	RepoIndexerTypeStats // 1
)

// RepoIndexerStatus status of a repo's entry in the repo indexer, one row per
// (RepoID, IndexerType, Branch). Branch == "" means the default branch — this is
// also what every row predating the Branch column implicitly means, so the zero
// value preserves old rows' meaning without a backfill.
type RepoIndexerStatus struct { //revive:disable-line:exported
	ID          int64           `xorm:"pk autoincr"`
	RepoID      int64           `xorm:"INDEX(s)"`
	Branch      string          `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	CommitSha   string          `xorm:"VARCHAR(64)"`
	IndexerType RepoIndexerType `xorm:"INDEX(s) NOT NULL DEFAULT 0"`
}

func init() {
	db.RegisterModel(new(RepoIndexerStatus))
}

// GetUnindexedRepos returns repos which do not have an indexer status
func GetUnindexedRepos(ctx context.Context, indexerType RepoIndexerType, maxRepoID int64, page, pageSize int) ([]int64, error) {
	ids := make([]int64, 0, 50)
	cond := builder.Cond(builder.IsNull{
		"repo_indexer_status.id",
	}).And(builder.Eq{
		"repository.is_empty": false,
	})
	sess := db.GetEngine(ctx).Table("repository").Join("LEFT OUTER", "repo_indexer_status", "repository.id = repo_indexer_status.repo_id AND repo_indexer_status.indexer_type = ?", indexerType)
	if maxRepoID > 0 {
		cond = builder.And(cond, builder.Lte{
			"repository.id": maxRepoID,
		})
	}
	if page >= 0 && pageSize > 0 {
		start := 0
		if page > 0 {
			start = (page - 1) * pageSize
		}
		sess.Limit(pageSize, start)
	}

	sess.Where(cond).Cols("repository.id").Desc("repository.id")
	err := sess.Find(&ids)
	return ids, err
}

// GetIndexerStatus loads a repo's indexer status for the given branch ("" = default branch).
// The in-memory cache on repo only ever holds one branch's status at a time — safe because each
// indexing job loads its own fresh Repository and only ever queries one branch within that job.
func GetIndexerStatus(ctx context.Context, repo *Repository, indexerType RepoIndexerType, branch string) (*RepoIndexerStatus, error) {
	switch indexerType {
	case RepoIndexerTypeCode:
		if repo.CodeIndexerStatus != nil && repo.CodeIndexerStatus.Branch == branch {
			return repo.CodeIndexerStatus, nil
		}
	case RepoIndexerTypeStats:
		if repo.StatsIndexerStatus != nil && repo.StatsIndexerStatus.Branch == branch {
			return repo.StatsIndexerStatus, nil
		}
	}
	status := &RepoIndexerStatus{}
	// Explicit conditions, not xorm's implicit non-zero-field matching: branch == "" (the
	// default-branch sentinel) is a legitimate value to filter on but is also Go's zero value
	// for string, which xorm's struct-based Get() silently skips — that would match any branch's
	// row for this repo instead of specifically the default-branch one.
	has, err := db.GetEngine(ctx).Where("`repo_id` = ? AND `indexer_type` = ? AND `branch` = ?", repo.ID, indexerType, branch).Get(status)
	if err != nil {
		return nil, err
	} else if !has {
		status = &RepoIndexerStatus{RepoID: repo.ID, Branch: branch, IndexerType: indexerType, CommitSha: ""}
	}
	switch indexerType {
	case RepoIndexerTypeCode:
		repo.CodeIndexerStatus = status
	case RepoIndexerTypeStats:
		repo.StatsIndexerStatus = status
	}
	return status, nil
}

// UpdateIndexerStatus updates indexer status for the given branch ("" = default branch).
func UpdateIndexerStatus(ctx context.Context, repo *Repository, indexerType RepoIndexerType, branch, sha string) error {
	status, err := GetIndexerStatus(ctx, repo, indexerType, branch)
	if err != nil {
		return fmt.Errorf("UpdateIndexerStatus: Unable to getIndexerStatus for repo: %s Error: %w", repo.FullName(), err)
	}

	if status.ID == 0 {
		status.CommitSha = sha
		if err := db.Insert(ctx, status); err != nil {
			return fmt.Errorf("UpdateIndexerStatus: Unable to insert repoIndexerStatus for repo: %s Sha: %s Error: %w", repo.FullName(), sha, err)
		}
		return nil
	}
	status.CommitSha = sha
	_, err = db.GetEngine(ctx).ID(status.ID).Cols("commit_sha").
		Update(status)
	if err != nil {
		return fmt.Errorf("UpdateIndexerStatus: Unable to update repoIndexerStatus for repo: %s Sha: %s Error: %w", repo.FullName(), sha, err)
	}
	return nil
}
