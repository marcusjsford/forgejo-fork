// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// sidecarClient is a typed HTTP client for the Rust embedding sidecar.
type sidecarClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newSidecarClient(baseURL, token string) *sidecarClient {
	return &sidecarClient{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			// otelhttp.NewTransport wraps the default transport with W3C
			// traceparent injection so every sidecar call is a child span.
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   60 * time.Second,
		},
	}
}

// ── POST /index ───────────────────────────────────────────────────────────────

type indexRequest struct {
	RepoFullName string `json:"repo_full_name"`
	RepoID       int64  `json:"repo_id"`
	Branch       string `json:"branch"`
	Path         string `json:"path"`
	Content      string `json:"content"`
}

type indexResponse struct {
	ChunksIndexed   int `json:"chunks_indexed"`
	SymbolsWritten  int `json:"symbols_written"`
	EdgesWritten    int `json:"edges_written"`
	EdgesUnresolved int `json:"edges_unresolved"`
}

// index chunks+embeds a file into Qdrant and extracts+upserts its
// Symbol/CALLS/IMPORTS/IMPLEMENTS/EXTENDS into Memgraph, in one call.
// Used to be two separate HTTP calls (embed, then graphUpsert) against an
// identical request shape — the sidecar now runs both concurrently
// server-side instead of this client serializing them (story 160,
// planning/stories/160-unified-sidecar-index-endpoint/).
func (c *sidecarClient) index(ctx context.Context, req indexRequest) (*indexResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/index", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar /index: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sidecar /index HTTP %d: %s", resp.StatusCode, b)
	}

	var out indexResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sidecar /index decode: %w", err)
	}
	return &out, nil
}

// ── DELETE /index ────────────────────────────────────────────────────────────

type deleteIndexRequest struct {
	RepoFullName string `json:"repo_full_name"`
	Branch       string `json:"branch"`
	Path         string `json:"path"`
}

// deleteIndexedFile removes a file's Qdrant vectors and Memgraph graph nodes
// in one call (used to be deleteFile + deleteGraphFile, two calls against an
// identical request shape — story 160).
func (c *sidecarClient) deleteIndexedFile(ctx context.Context, repoFullName, branch, path string) error {
	body, err := json.Marshal(deleteIndexRequest{RepoFullName: repoFullName, Branch: branch, Path: path})
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/index", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sidecar DELETE /index: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sidecar DELETE /index HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

// ── POST /graph/resolve ──────────────────────────────────────────────────────

type graphResolveRequest struct {
	RepoID int64  `json:"repo_id"`
	Branch string `json:"branch"`
}

type graphResolveResponse struct {
	CallsResolved    int `json:"calls_resolved"`
	ImportsResolved  int `json:"imports_resolved"`
	InheritsResolved int `json:"inherits_resolved"`
}

// graphResolve runs the Phase 5b resolution-phase job (story 060) for one
// repo+branch, promoting unresolved CALLS/IMPORTS/IMPLEMENTS/EXTENDS
// placeholder edges written during this push's /index calls. Called once
// per push, after every file's index call has completed — not per file,
// since a call in one file may resolve against a symbol a different file
// in the same push just added.
func (c *sidecarClient) graphResolve(ctx context.Context, repoID int64, branch string) (*graphResolveResponse, error) {
	body, err := json.Marshal(graphResolveRequest{RepoID: repoID, Branch: branch})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/graph/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar /graph/resolve: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sidecar /graph/resolve HTTP %d: %s", resp.StatusCode, b)
	}

	var out graphResolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sidecar /graph/resolve decode: %w", err)
	}
	return &out, nil
}

// ── DELETE /embed/repo ───────────────────────────────────────────────────────

type deleteEmbedByRepoRequest struct {
	RepoID int64 `json:"repo_id"`
}

// deleteByRepoID removes all Qdrant vectors AND all Memgraph File/Symbol/
// Package nodes+edges for an entire repository from the sidecar, via the
// dedicated repo-scoped delete endpoint (distinct from the per-file DELETE
// /index — a repo delete has no path/branch to scope by). Story 170 extended
// the sidecar's own /embed/repo handler to fan out to both stores
// server-side (matching /index's own do_embed+do_graph_upsert shape) — this
// is still exactly one HTTP call, same as before; the sidecar, not this Go
// code, knows there are two stores behind it.
func (c *sidecarClient) deleteByRepoID(ctx context.Context, repoID int64) error {
	body, err := json.Marshal(deleteEmbedByRepoRequest{RepoID: repoID})
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/embed/repo", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sidecar DELETE /embed/repo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sidecar DELETE /embed/repo HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

// ── /search ───────────────────────────────────────────────────────────────────

type sidecarSearchRequest struct {
	Query   string  `json:"query"`
	RepoIDs []int64 `json:"repo_ids,omitempty"` // ACL-filtered set of allowed repo IDs
	Limit   int     `json:"limit,omitempty"`
}

type sidecarSearchResult struct {
	RepoID       int64   `json:"repo_id"`
	RepoFullName string  `json:"repo_full_name"`
	Path         string  `json:"path"`
	Branch       string  `json:"branch"`
	StartLine    uint32  `json:"start_line"`
	EndLine      uint32  `json:"end_line"`
	Language     string  `json:"language"`
	Content      string  `json:"content"`
	Score        float32 `json:"score"`
}

type sidecarSearchResponse struct {
	Results []sidecarSearchResult `json:"results"`
}

func (c *sidecarClient) search(ctx context.Context, query string, repoIDs []int64, limit int) ([]sidecarSearchResult, error) {
	reqBody := sidecarSearchRequest{
		Query:   query,
		RepoIDs: repoIDs,
		Limit:   limit,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sidecar /search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sidecar /search HTTP %d: %s", resp.StatusCode, b)
	}

	var out sidecarSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sidecar /search decode: %w", err)
	}
	return out.Results, nil
}

func (c *sidecarClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Indexer-Token", c.token)
	}
}

// ping calls GET /health (no auth required) to verify the sidecar is alive.
func (c *sidecarClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sidecar /health returned %d", resp.StatusCode)
	}
	return nil
}
