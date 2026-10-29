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

// ── /search ───────────────────────────────────────────────────────────────────

// sidecarScope is the repo scope every sidecar read requires (code-indexer
// platform story 010). Forgejo only ever sends the repo_ids form: the
// unrestricted form ({"all": true}) needs the sidecar's operator credential,
// which Forgejo deliberately does not hold.
type sidecarScope struct {
	// No omitempty: an empty list must reach the sidecar as [] ("this user may
	// see nothing", which returns nothing). Omitting it used to drop the field,
	// and the sidecar read an absent repo_ids as "unrestricted".
	RepoIDs []int64 `json:"repo_ids"`
}

type sidecarSearchRequest struct {
	Query string       `json:"query"`
	Scope sidecarScope `json:"scope"` // ACL-filtered set of allowed repo IDs
	Limit int          `json:"limit,omitempty"`
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
	if repoIDs == nil {
		// A nil slice marshals as null, which the sidecar rejects as a scope
		// with neither form. The caller resolves "unrestricted" to real ids
		// before this point (Indexer.Search), so nil here can only mean none.
		repoIDs = []int64{}
	}
	reqBody := sidecarSearchRequest{
		Query: query,
		Scope: sidecarScope{RepoIDs: repoIDs},
		Limit: limit,
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
