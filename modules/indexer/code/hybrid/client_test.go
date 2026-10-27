// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package hybrid

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sidecar requires a repo scope on every read and returns nothing for an
// empty one (code-indexer platform story 010). These pin what Forgejo puts on
// the wire for each shape of permitted set.
func TestSearchAlwaysSendsAnExplicitRepoScope(t *testing.T) {
	cases := map[string]struct {
		in   []int64
		want string
	}{
		"permitted set":         {in: []int64{1, 2}, want: `{"repo_ids":[1,2]}`},
		"empty set is not lost": {in: []int64{}, want: `{"repo_ids":[]}`},
		"nil is never null":     {in: nil, want: `{"repo_ids":[]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var body map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				require.NoError(t, json.Unmarshal(raw, &body))
				_, _ = w.Write([]byte(`{"results":[]}`))
			}))
			defer srv.Close()

			_, err := newSidecarClient(srv.URL, "token").search(t.Context(), "q", tc.in, 5)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(body["scope"]))
			_, hasOld := body["repo_ids"]
			assert.False(t, hasOld, "the old top-level repo_ids field must not be sent")
		})
	}
}
