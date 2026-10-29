// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"forgejo.org/modules/log"

	"github.com/gobwas/glob"
)

// Indexer settings
var Indexer = struct {
	IssueType        string
	IssuePath        string
	IssueConnStr     string
	IssueConnAuth    string
	IssueIndexerName string
	StartupTimeout   time.Duration

	RepoIndexerEnabled     bool
	RepoIndexerRepoTypes   []string
	RepoIndexerEnableFuzzy bool
	RepoType               string
	RepoPath               string
	RepoConnStr            string
	RepoIndexerName        string
	MaxIndexerFileSize     int64
	IncludePatterns        []Glob
	ExcludePatterns        []Glob
	ExcludeVendored        bool

	// Hybrid indexer settings (REPO_INDEXER_TYPE = hybrid). Zoekt itself needs
	// no separate config here — the hybrid indexer reuses RepoPath/RepoIndexerName
	// above and Forgejo's native in-process Zoekt indexer, same as REPO_INDEXER_TYPE
	// = zoekt does. Only the sidecar (semantic search) and RRF fusion are hybrid-specific.
	HybridSidecarURL    string // URL of the Rust embedding sidecar
	HybridSidecarToken  string // X-Indexer-Token secret
	HybridRRFK          int    // RRF rank fusion constant k (default 60)
	HybridSemanticLimit int    // Max sidecar (semantic) results before RRF (default 20)
	// The control plane's change-notification endpoint, e.g.
	// http://control-plane:8090/events/local-forgejo. The last path segment
	// must be the source id configured in the control plane. Empty disables
	// the notifier (platform story 070).
	HybridEventsURL string
	// HMAC-SHA256 secret shared with that source. The body is signed exactly as
	// sent, in X-Forgejo-Signature, same as Forgejo's own webhooks.
	HybridEventsSecret string
}{
	IssueType:        "bleve",
	IssuePath:        "indexers/issues.bleve",
	IssueConnStr:     "",
	IssueConnAuth:    "",
	IssueIndexerName: "gitea_issues",

	RepoIndexerEnabled:     false,
	RepoIndexerRepoTypes:   []string{"sources", "forks", "mirrors", "templates"},
	RepoIndexerEnableFuzzy: false,
	RepoType:               "bleve",
	RepoPath:               "indexers/repos.bleve",
	RepoConnStr:            "",
	RepoIndexerName:        "gitea_codes",
	MaxIndexerFileSize:     1024 * 1024,
	ExcludeVendored:        true,

	HybridSidecarURL:    "http://sidecar:8080",
	HybridSidecarToken:  "",
	HybridRRFK:          60,
	HybridSemanticLimit: 20,
}

type Glob struct {
	glob    glob.Glob
	pattern string
}

func (g *Glob) Match(s string) bool {
	return g.glob.Match(s)
}

func (g *Glob) Pattern() string {
	return g.pattern
}

func loadIndexerFrom(rootCfg ConfigProvider) {
	sec := rootCfg.Section("indexer")
	Indexer.IssueType = sec.Key("ISSUE_INDEXER_TYPE").MustString("bleve")
	Indexer.IssuePath = filepath.ToSlash(sec.Key("ISSUE_INDEXER_PATH").MustString(filepath.ToSlash(filepath.Join(AppDataPath, "indexers/issues.bleve"))))
	if !filepath.IsAbs(Indexer.IssuePath) {
		Indexer.IssuePath = filepath.ToSlash(filepath.Join(AppWorkPath, Indexer.IssuePath))
	}
	Indexer.IssueConnStr = sec.Key("ISSUE_INDEXER_CONN_STR").MustString(Indexer.IssueConnStr)

	if Indexer.IssueType == "meilisearch" {
		u, err := url.Parse(Indexer.IssueConnStr)
		if err != nil {
			log.Warn("Failed to parse ISSUE_INDEXER_CONN_STR: %v", err)
			u = &url.URL{}
		}
		Indexer.IssueConnAuth, _ = u.User.Password()
		u.User = nil
		Indexer.IssueConnStr = u.String()
	}

	Indexer.IssueIndexerName = sec.Key("ISSUE_INDEXER_NAME").MustString(Indexer.IssueIndexerName)

	Indexer.RepoIndexerEnabled = sec.Key("REPO_INDEXER_ENABLED").MustBool(false)
	Indexer.RepoIndexerRepoTypes = strings.Split(sec.Key("REPO_INDEXER_REPO_TYPES").MustString("sources,forks,mirrors,templates"), ",")
	Indexer.RepoIndexerEnableFuzzy = sec.Key("REPO_INDEXER_FUZZY_ENABLED").MustBool(false)
	Indexer.RepoType = sec.Key("REPO_INDEXER_TYPE").MustString("bleve")
	Indexer.RepoPath = filepath.ToSlash(sec.Key("REPO_INDEXER_PATH").MustString(filepath.ToSlash(filepath.Join(AppDataPath, "indexers", "repos."+Indexer.RepoType))))
	if !filepath.IsAbs(Indexer.RepoPath) {
		Indexer.RepoPath = filepath.ToSlash(filepath.Join(AppWorkPath, Indexer.RepoPath))
	}
	Indexer.RepoConnStr = sec.Key("REPO_INDEXER_CONN_STR").MustString("")
	Indexer.RepoIndexerName = sec.Key("REPO_INDEXER_NAME").MustString("gitea_codes")

	Indexer.IncludePatterns = IndexerGlobFromString(sec.Key("REPO_INDEXER_INCLUDE").MustString(""))
	Indexer.ExcludePatterns = IndexerGlobFromString(sec.Key("REPO_INDEXER_EXCLUDE").MustString(""))
	Indexer.ExcludeVendored = sec.Key("REPO_INDEXER_EXCLUDE_VENDORED").MustBool(true)
	Indexer.MaxIndexerFileSize = sec.Key("MAX_FILE_SIZE").MustInt64(1024 * 1024)
	Indexer.StartupTimeout = sec.Key("STARTUP_TIMEOUT").MustDuration(30 * time.Second)

	if Indexer.RepoType == "hybrid" {
		// Section name uses underscore to match GITEA__indexer_hybrid__* env var format.
		hsec := rootCfg.Section("indexer_hybrid")
		Indexer.HybridSidecarURL = hsec.Key("SIDECAR_URL").MustString(Indexer.HybridSidecarURL)
		Indexer.HybridSidecarToken = hsec.Key("SIDECAR_TOKEN").MustString("")
		Indexer.HybridRRFK = hsec.Key("RRF_K").MustInt(Indexer.HybridRRFK)
		Indexer.HybridSemanticLimit = hsec.Key("SEMANTIC_LIMIT").MustInt(Indexer.HybridSemanticLimit)
		Indexer.HybridEventsURL = hsec.Key("EVENTS_URL").MustString("")
		Indexer.HybridEventsSecret = hsec.Key("EVENTS_SECRET").MustString("")
	}
}

// IndexerGlobFromString parses a comma separated list of patterns and returns a glob.Glob slice suited for repo indexing
func IndexerGlobFromString(globstr string) []Glob {
	extarr := make([]Glob, 0, 10)
	for expr := range strings.SplitSeq(strings.ToLower(globstr), ",") {
		expr = strings.TrimSpace(expr)
		if expr != "" {
			if g, err := glob.Compile(expr, '.', '/'); err != nil {
				log.Warn("Invalid glob expression '%s' (skipped): %v", expr, err)
			} else {
				extarr = append(extarr, Glob{glob: g, pattern: expr})
			}
		}
	}
	return extarr
}
