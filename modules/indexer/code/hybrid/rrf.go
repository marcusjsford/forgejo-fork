// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"sort"

	"github.com/google/uuid"

	"forgejo.org/modules/indexer/code/internal"
)

// PROJECT_NS is the project-specific UUIDv5 namespace committed to this binary.
// Used to derive deterministic point IDs that match the Qdrant sidecar.
// Value: urn:forgejo:code-indexer:hybrid (custom namespace).
var projectNS = uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c9")

// fileKey is the deduplication key for RRF fusion at the file level.
// Zoekt has no start_line, so dedup is per (repoID, path) — not per chunk.
type fileKey struct {
	repoID int64
	path   string
}

// rankedResult holds a candidate search result with its rank from each source.
type rankedResult struct {
	result       *internal.SearchResult
	zoektRank    int // 0 = not present
	semanticRank int // 0 = not present
}

// rrfScore computes the Reciprocal Rank Fusion score for a candidate.
// score(d) = Σ 1/(k + rank_i(d))
// k=60 is the standard constant recommended in the original RRF paper.
func rrfScore(r rankedResult, k int) float64 {
	score := 0.0
	if r.zoektRank > 0 {
		score += 1.0 / float64(k+r.zoektRank)
	}
	if r.semanticRank > 0 {
		score += 1.0 / float64(k+r.semanticRank)
	}
	return score
}

// merge performs RRF fusion of Zoekt and semantic (sidecar) search results.
//
// Deduplication is at the file level: two results are considered the same
// if they share (repoID, filename/path). This is intentional — Zoekt does not
// return start_line, so chunk-level dedup would never produce the "both" badge.
//
// Returns results sorted by descending RRF score, capped at `limit`.
func merge(
	zoektResults []*internal.SearchResult,
	semanticResults []*internal.SearchResult,
	k, limit int,
) []*internal.SearchResult {
	candidates := make(map[fileKey]*rankedResult)

	// Assign zoekt ranks (1-based)
	for i, r := range zoektResults {
		key := fileKey{repoID: r.RepoID, path: r.Filename}
		if _, ok := candidates[key]; !ok {
			candidates[key] = &rankedResult{result: r}
		}
		candidates[key].zoektRank = i + 1
	}

	// Assign semantic ranks (1-based)
	for i, r := range semanticResults {
		key := fileKey{repoID: r.RepoID, path: r.Filename}
		if _, ok := candidates[key]; !ok {
			candidates[key] = &rankedResult{result: r}
		}
		candidates[key].semanticRank = i + 1
		// Prefer the semantic result's content (has line numbers)
		if candidates[key].semanticRank == i+1 && candidates[key].zoektRank > 0 {
			// Keep zoekt result as base but note semantic rank for scoring
		} else if candidates[key].zoektRank == 0 {
			candidates[key].result = r
		}
	}

	// Sort by RRF score descending
	ranked := make([]*rankedResult, 0, len(candidates))
	for _, c := range candidates {
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool {
		si := rrfScore(*ranked[i], k)
		sj := rrfScore(*ranked[j], k)
		return si > sj
	})

	// Cap at limit
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	out := make([]*internal.SearchResult, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.result)
	}
	return out
}

// pointID computes the UUIDv5 ID that the Rust sidecar uses for Qdrant points.
// Input key: "{owner}/{repo_name}:{path}:{branch}"
// This allows the hybrid indexer to correlate Qdrant entries with Forgejo repos.
func pointID(repoFullName, path, branch string) uuid.UUID {
	key := repoFullName + ":" + path + ":" + branch
	return uuid.NewSHA1(projectNS, []byte(key))
}

// mergeLanguages combines and de-duplicates language facets from both sources.
func mergeLanguages(
	a []*internal.SearchResultLanguages,
	b []*internal.SearchResultLanguages,
) []*internal.SearchResultLanguages {
	counts := make(map[string]int)
	for _, l := range a {
		counts[l.Language] += l.Count
	}
	for _, l := range b {
		counts[l.Language] += l.Count
	}

	out := make([]*internal.SearchResultLanguages, 0, len(counts))
	for lang, count := range counts {
		out = append(out, &internal.SearchResultLanguages{Language: lang, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Count > out[j].Count
	})
	return out
}
