// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package hybrid implements a Forgejo code indexer that fans out to both
// Zoekt (keyword/structural, embedded in-process as of Forgejo v16) and an
// embedding sidecar (semantic), then fuses the results with Reciprocal Rank
// Fusion (RRF).
package hybrid

import (
	"context"
	"fmt"
	"sync"
	"time"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/indexer/code/internal"
	indexer_internal "forgejo.org/modules/indexer/internal"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	"github.com/go-enry/go-enry/v2"

	zoektindexer "forgejo.org/modules/indexer/code/zoekt"

	"xorm.io/builder"
)

// Indexer is the top-level hybrid code indexer.
// It satisfies the code/internal.Indexer interface.
type Indexer struct {
	indexer_internal.Indexer // embedded dummy for base Init/Ping/Close

	// zoekt is Forgejo's own native, in-process Zoekt indexer (embedded as of
	// v16 — no external zoekt-webserver/zoekt-indexserver HTTP round-trip).
	// It writes its shards to setting.Indexer.RepoPath, same as bleve would.
	zoekt    *zoektindexer.Indexer
	sidecar  *sidecarClient
	notifier *changeNotifier

	rrfK          int
	semanticLimit int
}

// NewIndexer constructs a HybridIndexer from the global Forgejo settings.
func NewIndexer() *Indexer {
	cfg := setting.Indexer
	// initTracer is a no-op if OTEL_EXPORTER_OTLP_ENDPOINT is unset.
	// We intentionally ignore the shutdown fn here — Forgejo manages its own
	// lifecycle and the batch exporter will flush on process exit.
	initTracer(context.Background()) //nolint:errcheck
	return &Indexer{
		Indexer:       indexer_internal.NewDummyIndexer(),
		zoekt:         zoektindexer.NewIndexer(cfg.RepoPath),
		sidecar:       newSidecarClient(cfg.HybridSidecarURL, cfg.HybridSidecarToken),
		notifier:      newChangeNotifier(cfg.HybridEventsURL, cfg.HybridEventsSecret),
		rrfK:          cfg.HybridRRFK,
		semanticLimit: cfg.HybridSemanticLimit,
	}
}

// ── internal.Indexer overrides ────────────────────────────────────────────────

// Init initialises both back-ends and waits for them to be ready.
// Returns (true, nil) if the index already contains data.
func (h *Indexer) Init(ctx context.Context) (bool, error) {
	existed, err := h.zoekt.Init(ctx)
	if err != nil {
		return false, fmt.Errorf("hybrid: zoekt init: %w", err)
	}
	// Sidecar is stateless; ping to verify it's alive
	if err := h.sidecar.ping(ctx); err != nil {
		return false, fmt.Errorf("hybrid: sidecar ping: %w", err)
	}
	return existed, nil
}

// Ping checks both back-ends.
func (h *Indexer) Ping(ctx context.Context) error {
	if err := h.zoekt.Ping(ctx); err != nil {
		return fmt.Errorf("hybrid/zoekt: %w", err)
	}
	return h.sidecar.ping(ctx)
}

// Close closes both back-ends.
func (h *Indexer) Close() {
	h.zoekt.Close()
}

// Formatter reports that hybrid results have no per-match highlight ranges
// to format specially — the semantic (sidecar) half of a fused result has no
// token-level match positions, so there's nothing precise to offer beyond
// Forgejo's generic fallback formatter. Mirrors bleve's own choice here.
func (h *Indexer) Formatter() internal.ResultFormatter {
	return nil
}

// ── code/internal.Indexer ─────────────────────────────────────────────────────

// Index notifies the control plane that this repo/ref now points at sha, and
// keeps Forgejo's own embedded Zoekt current until story 100 removes it.
// The semantic sidecar is not called at all: it no longer receives file
// content from Forgejo, and the control plane fetches and indexes the head
// itself (platform story 070).
func (h *Indexer) Index(ctx context.Context, repo *repo_model.Repository, branch, sha string, changes *internal.RepoChanges) error {
	ctx, span := tracer.Start(ctx, "hybrid.index")
	defer span.End()

	indexStart := time.Now()
	defer func() {
		indexDuration.Observe(time.Since(indexStart).Seconds())
		indexedFiles.Observe(float64(len(changes.Updates)))
	}()

	var wg sync.WaitGroup
	var zoektErr, notifyErr error

	// Forgejo's search UI still reads from the embedded Zoekt shards until
	// story 100 moves all search behind the control plane. Its failure remains
	// the hard error, exactly as before.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.zoekt.Index(ctx, repo, branch, sha, changes); err != nil {
			zoektErr = err
		}
	}()

	// One signed notification per completed push/mirror sync. No blob reads,
	// no per-file calls, no deletes, no graph-resolution request.
	if h.notifier != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			notifyErr = h.notifier.notifyChange(ctx, repo, branch, sha)
		}()
	}

	wg.Wait()
	if zoektErr != nil {
		return zoektErr
	}
	if notifyErr != nil {
		// Returned unhandled so Forgejo's persistent queue retries it after 5 s
		// (modules/queue/workergroup.go); the notification is the only signal
		// the platform gets, so losing it is not fail-open.
		return fmt.Errorf("hybrid: change notification: %w", notifyErr)
	}
	return nil
}

// Delete tells the control plane the repository is gone, then removes the
// embedded Zoekt shard. The sidecar's repo-scoped delete is no longer called:
// the platform owns that store's lifecycle from story 070 onwards.
func (h *Indexer) Delete(ctx context.Context, repoID int64) error {
	var notifyErr error
	if h.notifier != nil {
		notifyErr = h.notifier.notifyDeleted(ctx, repoID)
	}
	if err := h.zoekt.Delete(ctx, repoID); err != nil {
		return err
	}
	if notifyErr != nil {
		return fmt.Errorf("hybrid: repo_deleted notification: %w", notifyErr)
	}
	return nil
}

// ── Search ────────────────────────────────────────────────────────────────────

// Search fans out to Zoekt and the sidecar in parallel, then fuses with RRF.
func (h *Indexer) Search(ctx context.Context, opts *internal.SearchOptions) (int64, []*internal.SearchResult, []*internal.SearchResultLanguages, error) {
	ctx, span := tracer.Start(ctx, "hybrid.search")
	defer span.End()

	type zoektOut struct {
		total int64
		res   []*internal.SearchResult
		langs []*internal.SearchResultLanguages
		err   error
	}
	type semanticOut struct {
		res   []*internal.SearchResult
		langs []*internal.SearchResultLanguages
		err   error
	}

	zCh := make(chan zoektOut, 1)
	sCh := make(chan semanticOut, 1)

	searchStart := time.Now()
	defer func() { searchDuration.WithLabelValues("total").Observe(time.Since(searchStart).Seconds()) }()

	// Fan-out: Zoekt (in-process call, not an HTTP round-trip)
	go func() {
		t := time.Now()
		total, res, langs, err := h.zoekt.Search(ctx, opts)
		searchDuration.WithLabelValues("zoekt").Observe(time.Since(t).Seconds())
		searchResults.WithLabelValues("zoekt").Observe(float64(len(res)))
		if err != nil {
			searchErrors.WithLabelValues("zoekt").Inc()
		}
		zCh <- zoektOut{total, res, langs, err}
	}()

	// Fan-out: semantic via sidecar — skipped for exact-keyword searches,
	// where the user explicitly wants only literal matches from Zoekt.
	// opts.RepoIDs is the ACL-filtered set Forgejo already computed; pass it
	// directly so Qdrant only returns chunks the requesting user may read.
	// A nil slice means admin/unrestricted (routers/web/explore/code.go); an
	// empty slice means no access.
	go func() {
		if opts.Mode == internal.CodeSearchModeExact {
			sCh <- semanticOut{}
			return
		}
		t := time.Now()
		repoIDs, err := sidecarRepoIDs(ctx, opts.RepoIDs)
		if err != nil {
			searchErrors.WithLabelValues("sidecar").Inc()
			sCh <- semanticOut{err: err}
			return
		}
		raw, err := h.sidecar.search(ctx, opts.Keyword, repoIDs, h.semanticLimit)
		searchDuration.WithLabelValues("sidecar").Observe(time.Since(t).Seconds())
		if err != nil {
			searchErrors.WithLabelValues("sidecar").Inc()
			sCh <- semanticOut{err: err}
			return
		}
		res := convertSidecarResults(raw)
		searchResults.WithLabelValues("sidecar").Observe(float64(len(res)))
		sCh <- semanticOut{res: res}
	}()

	z := <-zCh
	s := <-sCh

	if z.err != nil {
		// Zoekt is canonical; propagate its error
		return 0, nil, nil, z.err
	}
	if s.err != nil {
		// Sidecar failure is soft; log and fall back to Zoekt-only
		log.Warn("hybrid: sidecar search error: %v; falling back to Zoekt-only", s.err)
		return z.total, z.res, z.langs, nil
	}

	fused := merge(z.res, s.res, h.rrfK, 20)
	searchResults.WithLabelValues("fused").Observe(float64(len(fused)))
	langs := mergeLanguages(z.langs, s.langs)

	// Use max(zoektTotal, fusedLen) as approx total
	total := z.total
	if int64(len(fused)) > total {
		total = int64(len(fused))
	}

	return total, fused, langs, nil
}

// convertSidecarResults maps sidecar search results to Forgejo's SearchResult.
func convertSidecarResults(raw []sidecarSearchResult) []*internal.SearchResult {
	out := make([]*internal.SearchResult, 0, len(raw))
	for _, r := range raw {
		out = append(out, &internal.SearchResult{
			RepoID:     r.RepoID,
			Filename:   r.Path,
			Content:    r.Content,
			Language:   r.Language,
			Color:      enry.GetColor(r.Language),
			StartIndex: 0,
			EndIndex:   len(r.Content),
			CommitID:   r.Branch,
		})
	}
	return out
}

// sidecarRepoIDs turns Forgejo's permitted set into the sidecar's scope.
//
// The sidecar requires an explicit repo list on every read and has no
// unrestricted form for Forgejo (code-indexer platform story 010): asking for
// every repo needs an operator credential Forgejo does not hold. So Forgejo's
// own "unrestricted" — a nil RepoIDs, which only an admin's site-wide search
// produces — becomes every repository id this instance has. That is also the
// right answer, not just the permitted one: Forgejo's search shows Forgejo's
// repos, and the sidecar may index repos from other sources that an admin of
// this forge has no claim to.
func sidecarRepoIDs(ctx context.Context, repoIDs []int64) ([]int64, error) {
	if repoIDs != nil {
		return repoIDs, nil
	}
	ids, err := repo_model.SearchRepositoryIDsByCondition(ctx, builder.NewCond())
	if err != nil {
		return nil, fmt.Errorf("hybrid: list repository ids for an unrestricted search: %w", err)
	}
	return ids, nil
}
