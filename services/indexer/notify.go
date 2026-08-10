// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package indexer

import (
	"context"

	git_model "forgejo.org/models/git"
	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	user_model "forgejo.org/models/user"
	code_indexer "forgejo.org/modules/indexer/code"
	issue_indexer "forgejo.org/modules/indexer/issues"
	stats_indexer "forgejo.org/modules/indexer/stats"
	"forgejo.org/modules/log"
	"forgejo.org/modules/repository"
	"forgejo.org/modules/setting"
	notify_service "forgejo.org/services/notify"
)

// shouldIndexBranch reports whether a push to branch should enqueue code indexing.
// Always true for the default branch. Non-default branches are gated to the hybrid
// indexer only: bleve/elasticsearch/zoekt store a single shard per repo with no
// branch namespacing, so feeding them a non-default branch would silently overwrite
// that shard with whichever branch pushed most recently. Within hybrid, a non-default
// branch is indexed only if it matches an existing branch-protection rule (including
// glob rule names like release/**) — protection is treated as the repo owner's signal
// that the branch matters enough to keep indexed, not "index every branch that
// exists." See planning/stories/020-branch-aware-indexing/findings.md for why this
// reuses branch protection instead of introducing a separate allowlist config.
func shouldIndexBranch(ctx context.Context, repo *repo_model.Repository, branch string) bool {
	if branch == repo.DefaultBranch {
		return true
	}
	if setting.Indexer.RepoType != "hybrid" {
		return false
	}
	protected, err := git_model.IsBranchProtected(ctx, repo.ID, branch)
	if err != nil {
		log.Error("shouldIndexBranch: IsBranchProtected(%d, %s): %v", repo.ID, branch, err)
		return false
	}
	return protected
}

type indexerNotifier struct {
	notify_service.NullNotifier
}

var _ notify_service.Notifier = &indexerNotifier{}

// NewNotifier create a new indexerNotifier notifier
func NewNotifier() notify_service.Notifier {
	return &indexerNotifier{}
}

func (r *indexerNotifier) AdoptRepository(ctx context.Context, doer, u *user_model.User, repo *repo_model.Repository) {
	r.MigrateRepository(ctx, doer, u, repo)
}

func (r *indexerNotifier) CreateIssueComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository,
	issue *issues_model.Issue, comment *issues_model.Comment, mentions []*user_model.User,
) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) NewIssue(ctx context.Context, issue *issues_model.Issue, mentions []*user_model.User) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) NewPullRequest(ctx context.Context, pr *issues_model.PullRequest, mentions []*user_model.User) {
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("LoadIssue: %v", err)
		return
	}
	issue_indexer.UpdateIssueIndexer(ctx, pr.Issue.ID)
}

func (r *indexerNotifier) UpdateComment(ctx context.Context, doer *user_model.User, c *issues_model.Comment, oldContent string) {
	if err := c.LoadIssue(ctx); err != nil {
		log.Error("LoadIssue: %v", err)
		return
	}
	issue_indexer.UpdateIssueIndexer(ctx, c.Issue.ID)
}

func (r *indexerNotifier) DeleteComment(ctx context.Context, doer *user_model.User, comment *issues_model.Comment) {
	if err := comment.LoadIssue(ctx); err != nil {
		log.Error("LoadIssue: %v", err)
		return
	}
	issue_indexer.UpdateIssueIndexer(ctx, comment.Issue.ID)
}

func (r *indexerNotifier) DeleteRepository(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) {
	issue_indexer.DeleteRepoIssueIndexer(ctx, repo.ID)
	if setting.Indexer.RepoIndexerEnabled {
		code_indexer.UpdateRepoIndexer(repo, "")
	}
}

func (r indexerNotifier) DeleteIssue(ctx context.Context, doer *user_model.User, issue *issues_model.Issue) {
	issue_indexer.DeleteIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) MigrateRepository(ctx context.Context, doer, u *user_model.User, repo *repo_model.Repository) {
	issue_indexer.UpdateRepoIndexer(ctx, repo.ID)
	if setting.Indexer.RepoIndexerEnabled && !repo.IsEmpty {
		code_indexer.UpdateRepoIndexer(repo, "")
	}
	if err := stats_indexer.UpdateRepoIndexer(repo); err != nil {
		log.Error("stats_indexer.UpdateRepoIndexer(%d) failed: %v", repo.ID, err)
	}
}

func (r *indexerNotifier) PushCommits(ctx context.Context, pusher *user_model.User, repo *repo_model.Repository, opts *repository.PushUpdateOptions, commits *repository.PushCommits) {
	if !opts.RefFullName.IsBranch() {
		return
	}

	branch := opts.RefFullName.BranchName()
	if setting.Indexer.RepoIndexerEnabled && shouldIndexBranch(ctx, repo, branch) {
		code_indexer.UpdateRepoIndexer(repo, branch)
	}
	if err := stats_indexer.UpdateRepoIndexer(repo); err != nil {
		log.Error("stats_indexer.UpdateRepoIndexer(%d) failed: %v", repo.ID, err)
	}
}

func (r *indexerNotifier) SyncPushCommits(ctx context.Context, pusher *user_model.User, repo *repo_model.Repository, opts *repository.PushUpdateOptions, commits *repository.PushCommits) {
	if !opts.RefFullName.IsBranch() {
		return
	}

	branch := opts.RefFullName.BranchName()
	if setting.Indexer.RepoIndexerEnabled && shouldIndexBranch(ctx, repo, branch) {
		code_indexer.UpdateRepoIndexer(repo, branch)
	}
	if err := stats_indexer.UpdateRepoIndexer(repo); err != nil {
		log.Error("stats_indexer.UpdateRepoIndexer(%d) failed: %v", repo.ID, err)
	}
}

func (r *indexerNotifier) ChangeDefaultBranch(ctx context.Context, repo *repo_model.Repository) {
	if setting.Indexer.RepoIndexerEnabled && !repo.IsEmpty {
		code_indexer.UpdateRepoIndexer(repo, "")
	}
	if err := stats_indexer.UpdateRepoIndexer(repo); err != nil {
		log.Error("stats_indexer.UpdateRepoIndexer(%d) failed: %v", repo.ID, err)
	}
}

func (r *indexerNotifier) IssueChangeContent(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, oldContent string) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeTitle(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, oldTitle string) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeRef(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, oldRef string) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeStatus(ctx context.Context, doer *user_model.User, commitID string, issue *issues_model.Issue, actionComment *issues_model.Comment, closeOrReopen bool) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeAssignee(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, assignee *user_model.User, removed bool, comment *issues_model.Comment) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeMilestone(ctx context.Context, doer *user_model.User, issue *issues_model.Issue, oldMilestoneID int64) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueChangeLabels(ctx context.Context, doer *user_model.User, issue *issues_model.Issue,
	addedLabels, removedLabels []*issues_model.Label,
) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) IssueClearLabels(ctx context.Context, doer *user_model.User, issue *issues_model.Issue) {
	issue_indexer.UpdateIssueIndexer(ctx, issue.ID)
}

func (r *indexerNotifier) MergePullRequest(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) {
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("LoadIssue: %v", err)
		return
	}
	issue_indexer.UpdateIssueIndexer(ctx, pr.Issue.ID)
}

func (r *indexerNotifier) AutoMergePullRequest(ctx context.Context, doer *user_model.User, pr *issues_model.PullRequest) {
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("LoadIssue: %v", err)
		return
	}
	issue_indexer.UpdateIssueIndexer(ctx, pr.Issue.ID)
}
