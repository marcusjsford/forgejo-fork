// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import "code.forgejo.org/xorm/xorm"

func init() {
	registerMigration(&Migration{
		Description: "add branch to repo_indexer_status for per-branch incremental code indexing",
		Upgrade:     addBranchToRepoIndexerStatus,
	})
}

func addBranchToRepoIndexerStatus(x *xorm.Engine) error {
	// Empty string means "the default branch" — existing rows predate this column and
	// implicitly tracked only the default branch, so leaving them at the zero value keeps
	// their meaning unchanged rather than requiring a backfill join to repository.default_branch.
	//
	// Deliberately no INDEX tag here: the live table already has a composite index named "s"
	// covering (repo_id, indexer_type) declared on the full model in models/repo/repo_indexer.go.
	// Syncing this minimal partial struct only knows about the Branch column, so giving it the
	// same "s" group name would make xorm try to reconcile an index group it can't see the other
	// members of. The table is one row per (repo, indexer_type, branch) — small enough that an
	// unindexed branch lookup is not a real cost.
	type RepoIndexerStatus struct {
		Branch string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	}

	_, err := x.SyncWithOptions(
		xorm.SyncOptions{IgnoreDropIndices: true},
		new(RepoIndexerStatus),
	)
	return err
}
