// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// changeNotifier posts the one thing Forgejo still tells the platform: "repo
// R, ref B is now at SHA X". It carries no file content; the control plane
// clones and indexes the head itself (platform story 070).
type changeNotifier struct {
	url    string
	secret string
	http   *http.Client
}

type eventRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Private       bool   `json:"private"`
}

type changeEvent struct {
	Kind  string    `json:"kind"`
	Repo  eventRepo `json:"repo"`
	Ref   string    `json:"ref,omitempty"`
	After string    `json:"after,omitempty"`
}

func newChangeNotifier(url, secret string) *changeNotifier {
	if url == "" {
		return nil
	}
	return &changeNotifier{
		url:    url,
		secret: secret,
		http: &http.Client{
			// otelhttp gives notification delivery the same trace context as
			// every other outbound call.
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   30 * time.Second,
		},
	}
}

// notifyChange sends one push/mirror_sync event for a completed index pass.
// The control plane treats this as a hint: a missed or duplicate event costs
// nothing because the reconciler compares state rather than replaying it.
func (n *changeNotifier) notifyChange(ctx context.Context, repo *repo_model.Repository, branch, sha string) error {
	kind := "push"
	if repo.IsMirror {
		kind = "mirror_sync"
	}
	return n.send(ctx, changeEvent{
		Kind: kind,
		Repo: eventRepo{
			ID:            repo.ID,
			FullName:      repo.FullName(),
			CloneURL:      repo.CloneLink().HTTPS,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.IsPrivate,
		},
		Ref:   "refs/heads/" + branch,
		After: sha,
	})
}

// notifyDeleted tells the control plane the repo is gone. Only the id is
// available to Delete(), which is enough: the control plane resolves it
// through the source alias and tombstones the repo.
func (n *changeNotifier) notifyDeleted(ctx context.Context, repoID int64) error {
	return n.send(ctx, changeEvent{
		Kind: "repo_deleted",
		Repo: eventRepo{ID: repoID},
	})
}

func (n *changeNotifier) send(ctx context.Context, event changeEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forgejo-Event", event.Kind)
	// The control plane accepts either spelling; Forgejo's own notifier sends
	// both. Sign the exact bytes that go on the wire -- re-serialising would
	// change the signature's subject.
	mac := hmac.New(sha256.New, []byte(n.secret))
	mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("X-Forgejo-Signature", signature)
	req.Header.Set("X-Hub-Signature-256", "sha256="+signature)

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("notify %s: %w", n.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	// 4xx is a configuration error (wrong URL/source id/secret). Retrying it
	// forever would fill the queue without ever succeeding, so it is logged and
	// dropped. 5xx and transport failures are transient (control plane down)
	// and are returned so Forgejo's persistent indexer queue retries them.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		log.Error("hybrid: change notification rejected by %s: HTTP %d: %s", n.url, resp.StatusCode, detail)
		return nil
	}
	return fmt.Errorf("notify %s: HTTP %d: %s", n.url, resp.StatusCode, detail)
}
