// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package hybrid implements a Forgejo code indexer that fans out to both
// Zoekt (keyword/structural, embedded in-process as of Forgejo v16) and an
// embedding sidecar (semantic), then fuses the results with Reciprocal Rank
// Fusion (RRF).
package hybrid

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/charset"
	"forgejo.org/modules/git"
	"forgejo.org/modules/gitrepo"
	"forgejo.org/modules/indexer/code/internal"
	indexer_internal "forgejo.org/modules/indexer/internal"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	"forgejo.org/modules/typesniffer"
	"github.com/go-enry/go-enry/v2"

	zoektindexer "forgejo.org/modules/indexer/code/zoekt"
)

// Indexer is the top-level hybrid code indexer.
// It satisfies the code/internal.Indexer interface.
type Indexer struct {
	indexer_internal.Indexer // embedded dummy for base Init/Ping/Close

	// zoekt is Forgejo's own native, in-process Zoekt indexer (embedded as of
	// v16 — no external zoekt-webserver/zoekt-indexserver HTTP round-trip).
	// It writes its shards to setting.Indexer.RepoPath, same as bleve would.
	zoekt   *zoektindexer.Indexer
	sidecar *sidecarClient

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

// Index fans out to Zoekt and the embedding sidecar in parallel.
// Zoekt is always called; the sidecar is called for each updated file.
// If either fails: Zoekt failure is returned (hard error); sidecar failures
// are logged and the pipeline continues (fail-open).
func (h *Indexer) Index(ctx context.Context, repo *repo_model.Repository, branch, sha string, changes *internal.RepoChanges) error {
	ctx, span := tracer.Start(ctx, "hybrid.index")
	defer span.End()

	indexStart := time.Now()
	defer func() {
		indexDuration.Observe(time.Since(indexStart).Seconds())
		indexedFiles.Observe(float64(len(changes.Updates)))
	}()
	var wg sync.WaitGroup
	var zoektErr error

	// ── Fan-out: Zoekt (async, in-process — no network round-trip) ────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.zoekt.Index(ctx, repo, branch, sha, changes); err != nil {
			zoektErr = err
		}
	}()

	// ── Sidecar /index (sync, per file) ────────────────────────────────────
	if len(changes.Updates) > 0 {
		if err := h.indexFilesInSidecar(ctx, repo, branch, sha, changes.Updates); err != nil {
			// Fail-open: log sidecar errors, keep the pipeline alive
			log.Warn("hybrid: sidecar index error for %s@%s (branch %s): %v", repo.FullName(), sha, branch, err)
		}
	}

	// ── Sidecar DELETE /index (removed / renamed files) ───────────────────
	if len(changes.RemovedFilenames) > 0 {
		for _, path := range changes.RemovedFilenames {
			if err := h.sidecar.deleteIndexedFile(ctx, repo.FullName(), branch, path); err != nil {
				log.Warn("hybrid: sidecar delete %s/%s: %v", repo.FullName(), path, err)
				// fail-open
			}
		}
	}

	// ── Sidecar /graph/resolve (Phase 5b resolution phase, story 060) ────
	// Once per push, after every file's /index above has completed — not
	// per file, since a call in one file may resolve against a symbol a
	// different file in the same push just added
	// (graph-rag-implementation-plan.md §3).
	if len(changes.Updates) > 0 || len(changes.RemovedFilenames) > 0 {
		if resolved, err := h.sidecar.graphResolve(ctx, repo.ID, branch); err != nil {
			log.Warn("hybrid: sidecar graph resolve %s@%s (branch %s): %v", repo.FullName(), sha, branch, err)
			// fail-open
		} else {
			log.Trace(
				"hybrid: graph resolve %s@%s (branch %s): %d calls, %d imports, %d inherits promoted to heuristic",
				repo.FullName(), sha, branch, resolved.CallsResolved, resolved.ImportsResolved, resolved.InheritsResolved,
			)
		}
	}

	wg.Wait()
	return zoektErr
}

// indexFilesInSidecar reads blob content from git and calls POST /index for
// each updated file, using the same batch-reader pattern as the bleve indexer.
func (h *Indexer) indexFilesInSidecar(
	ctx context.Context,
	repo *repo_model.Repository,
	branch, sha string,
	updates []internal.FileUpdate,
) error {
	r, err := gitrepo.OpenRepository(ctx, repo)
	if err != nil {
		return err
	}
	defer r.Close()

	gitBatch, err := r.NewBatch(ctx)
	if err != nil {
		return err
	}
	defer gitBatch.Close()

	for _, update := range updates {
		if setting.Indexer.ExcludeVendored {
			// Reuse Forgejo's vendor-path detection if available
			if strings.Contains(update.Filename, "/vendor/") ||
				strings.HasPrefix(update.Filename, "vendor/") {
				continue
			}
		}

		content, err := readBlobContent(ctx, r, gitBatch.Writer, gitBatch.Reader, update)
		if err != nil {
			log.Warn("hybrid: reading blob %s/%s: %v", repo.FullName(), update.Filename, err)
			continue
		}
		if content == "" {
			continue
		}

		if _, err := h.sidecar.index(ctx, indexRequest{
			RepoFullName: repo.FullName(),
			RepoID:       repo.ID,
			Branch:       branch,
			Path:         update.Filename,
			Content:      content,
		}); err != nil {
			log.Warn("hybrid: sidecar index %s/%s: %v", repo.FullName(), update.Filename, err)
			// continue — fail-open
		}
	}
	return nil
}

// readBlobContent reads a file's text content from git via cat-file batch.
// Returns an empty string for binary files or files exceeding MaxIndexerFileSize.
func readBlobContent(
	ctx context.Context,
	_ *git.Repository,
	batchWriter git.WriteCloserError,
	batchReader *bufio.Reader,
	update internal.FileUpdate,
) (string, error) {
	size := update.Size
	var err error

	if !update.Sized {
		var stdout string
		stdout, _, err = git.NewCommand(ctx, "cat-file", "-s").
			AddDynamicArguments(update.BlobSha).
			RunStdString(&git.RunOpts{})
		if err != nil {
			return "", err
		}
		if size, err = strconv.ParseInt(strings.TrimSpace(stdout), 10, 64); err != nil {
			return "", fmt.Errorf("misformatted git cat-file output: %w", err)
		}
	}

	if size > setting.Indexer.MaxIndexerFileSize {
		return "", nil
	}

	if _, err := batchWriter.Write([]byte(update.BlobSha + "\n")); err != nil {
		return "", err
	}

	_, _, size, err = git.ReadBatchLine(batchReader)
	if err != nil {
		return "", err
	}

	fileBytes, err := io.ReadAll(io.LimitReader(batchReader, size))
	if err != nil {
		return "", err
	}
	if _, err := batchReader.Discard(1); err != nil {
		return "", err
	}

	if !typesniffer.DetectContentType(fileBytes, update.Filename).IsText() {
		return "", nil
	}

	return string(charset.ToUTF8DropErrors(fileBytes, charset.ConvertOpts{})), nil
}

// Delete removes the repository's chunks from the sidecar (both Qdrant and,
// as of story 170, Memgraph — the sidecar's own /embed/repo handler fans out
// to both stores server-side, so this stays a single call) and from the
// Zoekt index.
func (h *Indexer) Delete(ctx context.Context, repoID int64) error {
	// Sidecar delete: repo-scoped delete across Qdrant + Memgraph, one call.
	if err := h.sidecar.deleteByRepoID(ctx, repoID); err != nil {
		log.Warn("hybrid: sidecar delete repoID=%d: %v", repoID, err)
	}
	return h.zoekt.Delete(ctx, repoID)
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
	// A nil slice means admin/unrestricted; an empty slice means no access.
	go func() {
		if opts.Mode == internal.CodeSearchModeExact {
			sCh <- semanticOut{}
			return
		}
		t := time.Now()
		raw, err := h.sidecar.search(ctx, opts.Keyword, opts.RepoIDs, h.semanticLimit)
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
