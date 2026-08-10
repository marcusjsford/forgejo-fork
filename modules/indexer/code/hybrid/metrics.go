// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All metrics use the forgejo_hybrid_indexer_ namespace so they are easy
// to find alongside zoekt_* and gitea_* metrics in Prometheus/Grafana.

var (
	// searchDuration tracks wall-clock time for each backend fan-out and the
	// total Search() call.  label: backend = "zoekt" | "sidecar" | "total"
	searchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "forgejo",
		Subsystem: "hybrid_indexer",
		Name:      "search_duration_seconds",
		Help:      "Search latency by backend (zoekt, sidecar) and total.",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"backend"})

	// searchResults tracks how many results each backend returned per query.
	// label: backend = "zoekt" | "sidecar" | "fused"
	searchResults = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "forgejo",
		Subsystem: "hybrid_indexer",
		Name:      "search_results_returned",
		Help:      "Number of results returned by each backend per search.",
		Buckets:   []float64{0, 1, 5, 10, 20, 50, 100},
	}, []string{"backend"})

	// searchErrors counts backend failures.
	// label: backend = "zoekt" | "sidecar"
	searchErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "forgejo",
		Subsystem: "hybrid_indexer",
		Name:      "search_errors_total",
		Help:      "Total search errors by backend.",
	}, []string{"backend"})

	// indexDuration tracks time to index a single repository push.
	indexDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "forgejo",
		Subsystem: "hybrid_indexer",
		Name:      "index_duration_seconds",
		Help:      "Time to index a repository update (Zoekt + sidecar embed).",
		Buckets:   []float64{.01, .05, .1, .5, 1, 2.5, 5, 10, 30},
	})

	// indexedFiles tracks how many files were embedded per index call.
	indexedFiles = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "forgejo",
		Subsystem: "hybrid_indexer",
		Name:      "indexed_files_per_push",
		Help:      "Number of files sent to the sidecar embedder per push.",
		Buckets:   []float64{0, 1, 5, 10, 25, 50, 100, 250},
	})
)
