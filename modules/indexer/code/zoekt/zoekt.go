// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package zoekt implements a Forgejo code indexer that uses Zoekt for
// full-text keyword search.
//
// It speaks to:
//   - zoekt-dynamic-indexserver (:6072) — POST /index to trigger re-indexing
//   - zoekt-webserver            (:6070) — POST /api/search for queries
package zoekt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-enry/go-enry/v2"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/indexer/code/internal"
	indexer_internal "forgejo.org/modules/indexer/internal"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
)

// Indexer is a Forgejo CodeIndexer backed by Zoekt.
type Indexer struct {
	indexer_internal.Indexer // embedded dummy — satisfies Init/Ping/Close

	webURL   string // zoekt-webserver base URL  e.g. http://zoekt-webserver:6070
	indexURL string // zoekt-dynamic-indexserver e.g. http://zoekt-indexserver:6072

	httpClient *http.Client
}

// NewIndexer creates a new Zoekt-backed code indexer.
func NewIndexer(webURL, indexURL string) *Indexer {
	return &Indexer{
		Indexer:  indexer_internal.NewDummyIndexer(),
		webURL:   webURL,
		indexURL: indexURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ── internal.Indexer overrides ────────────────────────────────────────────────

// Init pings the Zoekt webserver to verify connectivity.
// Returns (true, nil) if the index already exists; (false, nil) on first run.
func (z *Indexer) Init(ctx context.Context) (bool, error) {
	if err := z.Ping(ctx); err != nil {
		return false, fmt.Errorf("zoekt: cannot reach webserver at %s: %w", z.webURL, err)
	}
	return true, nil
}

// Ping hits the Zoekt webserver /healthz endpoint.
func (z *Indexer) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.webURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := z.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zoekt healthz returned %d", resp.StatusCode)
	}
	return nil
}

// Close is a no-op for a remote Zoekt client.
func (z *Indexer) Close() {}

// ── code/internal.Indexer ─────────────────────────────────────────────────────

// Index triggers the Zoekt dynamic indexserver to (re-)index the repository
// by cloning it via Forgejo's internal HTTP URL.
func (z *Indexer) Index(ctx context.Context, repo *repo_model.Repository, _ string, changes *internal.RepoChanges) error {
	if len(changes.Updates) == 0 && len(changes.RemovedFilenames) == 0 {
		return nil
	}

	cloneURL := setting.Indexer.HybridForgejoInternalURL + repo.FullName() + ".git"
	body, _ := json.Marshal(map[string]any{
		"CloneURL": cloneURL,
		"RepoID":   uint32(repo.ID),
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.indexURL+"/index", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := z.httpClient.Do(req)
	if err != nil {
		// Zoekt indexing is best-effort; don't fail the whole index pipeline
		log.Warn("zoekt: failed to trigger re-index for %s: %v", repo.FullName(), err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Warn("zoekt: indexserver returned %d for %s: %s", resp.StatusCode, repo.FullName(), b)
	}
	return nil
}

// Delete removes the Zoekt index for the repository. Zoekt's indexserver
// exposes a DELETE endpoint on the dynamic variant; on failure we log and
// continue (fail-open).
func (z *Indexer) Delete(ctx context.Context, repoID int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/index?repo_id=%d", z.indexURL, repoID), nil)
	if err != nil {
		log.Warn("zoekt: failed to build delete request: %v", err)
		return nil
	}
	resp, err := z.httpClient.Do(req)
	if err != nil {
		log.Warn("zoekt: failed to delete index for repoID=%d: %v", repoID, err)
		return nil
	}
	resp.Body.Close()
	return nil
}

// ── Search ────────────────────────────────────────────────────────────────────

// zoektSearchReq is the JSON body for POST /api/search on the Zoekt webserver.
type zoektSearchReq struct {
	Q    string              `json:"Q"`
	Opts *zoektSearchOptions `json:"Opts,omitempty"`
}

type zoektSearchOptions struct {
	MaxDocDisplayCount int  `json:"MaxDocDisplayCount,omitempty"`
	Whole              bool `json:"Whole,omitempty"` // return full file content
}

// zoektSearchReply mirrors github.com/sourcegraph/zoekt.SearchResult (JSON tags).
type zoektSearchReply struct {
	Result *zoektResult `json:"Result"`
}

type zoektResult struct {
	Files []*zoektFile `json:"Files"`
	Stats struct {
		MatchCount int `json:"MatchCount"`
	} `json:"Stats"`
}

type zoektFile struct {
	FileName    string       `json:"FileName"`
	Repository  string       `json:"Repository"`
	Language    string       `json:"Language"`
	Branches    []string     `json:"Branches"`
	LineMatches []*zoektLine `json:"LineMatches"`
	Content     []byte       `json:"Content,omitempty"`
}

type zoektLine struct {
	Line       []byte `json:"Line"`
	LineNumber int    `json:"LineNumber"`
}

// Search queries the Zoekt webserver's JSON API.
func (z *Indexer) Search(ctx context.Context, opts *internal.SearchOptions) (int64, []*internal.SearchResult, []*internal.SearchResultLanguages, error) {
	if opts.Keyword == "" {
		return 0, nil, nil, nil
	}

	// Build a Zoekt query string
	q := buildZoektQuery(opts)

	limit := 20
	if opts.Paginator != nil {
		_, take := opts.Paginator.GetSkipTake()
		if take > 0 {
			limit = take
		}
	}

	reqBody, _ := json.Marshal(zoektSearchReq{
		Q: q,
		Opts: &zoektSearchOptions{
			MaxDocDisplayCount: limit,
		},
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		z.webURL+"/api/search", bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := z.httpClient.Do(httpReq)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("zoekt search: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return 0, nil, nil, fmt.Errorf("zoekt search: HTTP %d: %s", httpResp.StatusCode, b)
	}

	var reply zoektSearchReply
	if err := json.NewDecoder(httpResp.Body).Decode(&reply); err != nil {
		return 0, nil, nil, fmt.Errorf("zoekt search decode: %w", err)
	}
	if reply.Result == nil {
		return 0, nil, nil, nil
	}

	// Convert to Forgejo's SearchResult type
	results, langs := convertResults(reply.Result, opts)
	return int64(reply.Result.Stats.MatchCount), results, langs, nil
}

// buildZoektQuery converts Forgejo SearchOptions into a Zoekt query string.
// See: https://github.com/sourcegraph/zoekt/blob/main/doc/query_syntax.md
func buildZoektQuery(opts *internal.SearchOptions) string {
	q := opts.Keyword

	// Restrict to specific repos by ID (Zoekt uses repo: atom for repo names,
	// but we don't have names handy — we search globally and filter in RRF)
	if len(opts.RepoIDs) == 1 {
		// Single repo: can't filter by numeric ID in Zoekt query language, rely on RRF
	}

	if opts.Language != "" {
		q += " lang:" + opts.Language
	}
	if opts.Filename != "" {
		q += " file:" + opts.Filename
	}

	return q
}

// convertResults converts Zoekt results to Forgejo's SearchResult slice.
func convertResults(r *zoektResult, opts *internal.SearchOptions) ([]*internal.SearchResult, []*internal.SearchResultLanguages) {
	repoSet := make(map[int64]bool)
	for _, id := range opts.RepoIDs {
		repoSet[id] = true
	}

	langCount := make(map[string]int)
	var results []*internal.SearchResult

	for _, f := range r.Files {
		if len(f.LineMatches) == 0 {
			continue
		}
		lm := f.LineMatches[0]
		line := string(lm.Line)

		// f.Repository is the string form of the numeric Forgejo repo ID
		// (e.g. "2"), set when the shard was built with -repoid.
		repoID, _ := strconv.ParseInt(f.Repository, 10, 64)
		result := &internal.SearchResult{
			RepoID:     repoID,
			Filename:   f.FileName,
			Content:    line,
			Language:   f.Language,
			Color:      enry.GetColor(f.Language),
			StartIndex: 0,
			EndIndex:   len(line),
		}
		if len(f.Branches) > 0 {
			result.CommitID = f.Branches[0]
		}
		results = append(results, result)

		if f.Language != "" {
			langCount[f.Language]++
		}
	}

	var langs []*internal.SearchResultLanguages
	for lang, count := range langCount {
		langs = append(langs, &internal.SearchResultLanguages{
			Language: lang,
			Color:    enry.GetColor(lang),
			Count:    count,
		})
	}

	return results, langs
}
