// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	repo_model "forgejo.org/models/repo"
	"forgejo.org/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRepo(mirror bool) *repo_model.Repository {
	return &repo_model.Repository{
		ID:            42,
		OwnerName:     "gitadmin",
		Name:          "example",
		DefaultBranch: "main",
		IsPrivate:     true,
		IsMirror:      mirror,
	}
}

func testNotifier(t *testing.T, handler http.HandlerFunc) (*changeNotifier, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return newChangeNotifier(server.URL+"/events/local-forgejo", "hook-secret"), server
}

func TestNotifyChangeSendsOneSignedDesignEvent(t *testing.T) {
	setting.AppURL = "http://forgejo:3000/"
	var body []byte
	var headers http.Header
	notifier, _ := testNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	})

	require.NoError(t, notifier.notifyChange(context.Background(), testRepo(false), "main", "deadbeef"))

	var event changeEvent
	require.NoError(t, json.Unmarshal(body, &event))
	assert.Equal(t, "push", event.Kind)
	assert.Equal(t, int64(42), event.Repo.ID)
	assert.Equal(t, "gitadmin/example", event.Repo.FullName)
	assert.Equal(t, "http://forgejo:3000/gitadmin/example.git", event.Repo.CloneURL)
	assert.Equal(t, "main", event.Repo.DefaultBranch)
	assert.True(t, event.Repo.Private)
	assert.Equal(t, "refs/heads/main", event.Ref)
	assert.Equal(t, "deadbeef", event.After)

	// The signature must cover the exact bytes received, not a re-serialisation.
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write(body)
	assert.Equal(t, hex.EncodeToString(mac.Sum(nil)), headers.Get("X-Forgejo-Signature"))
	assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), headers.Get("X-Hub-Signature-256"))
}

func TestNotifyChangeUsesMirrorSyncKind(t *testing.T) {
	setting.AppURL = "http://forgejo:3000/"
	var event changeEvent
	notifier, _ := testNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&event)
		w.WriteHeader(http.StatusAccepted)
	})

	require.NoError(t, notifier.notifyChange(context.Background(), testRepo(true), "main", "abc"))
	assert.Equal(t, "mirror_sync", event.Kind)
}

func TestNotifyDeletedCarriesOnlyTheRepoID(t *testing.T) {
	var event changeEvent
	notifier, _ := testNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&event)
		w.WriteHeader(http.StatusAccepted)
	})

	require.NoError(t, notifier.notifyDeleted(context.Background(), 42))
	assert.Equal(t, "repo_deleted", event.Kind)
	assert.Equal(t, int64(42), event.Repo.ID)
	assert.Empty(t, event.Ref)
	assert.Empty(t, event.After)
}

func TestNotifyServerErrorIsRetryable(t *testing.T) {
	notifier, _ := testNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	err := notifier.notifyChange(context.Background(), testRepo(false), "main", "abc")
	require.Error(t, err, "5xx must be returned so Forgejo's queue retries")
}

func TestNotifyClientErrorIsPermanent(t *testing.T) {
	notifier, _ := testNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	assert.NoError(t, notifier.notifyChange(context.Background(), testRepo(false), "main", "abc"),
		"4xx is a configuration error; retrying it forever cannot succeed")
}

func TestNotifyTransportErrorIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	notifier := newChangeNotifier(url+"/events/local-forgejo", "s")
	require.Error(t, notifier.notifyChange(context.Background(), testRepo(false), "main", "abc"))
}

func TestEmptyEventsURLDisablesNotifier(t *testing.T) {
	assert.Nil(t, newChangeNotifier("", "secret"))
}
