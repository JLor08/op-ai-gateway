// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestParseServerMemory(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		format string
		want   ServerMemory
	}{
		{
			name: "prometheus deferred + processing",
			body: "# HELP llamacpp:requests_deferred\nllamacpp:requests_deferred 2\nllamacpp:requests_processing 4\n",
			want: ServerMemory{RequestsDeferred: 2, RequestsProcessing: 4, OK: true},
		},
		{
			name: "prometheus kv-cache ratio (opportunistic)",
			body: "# HELP\nllamacpp:kv_cache_usage_ratio 0.9\n",
			want: ServerMemory{RequestsDeferred: -1, RequestsProcessing: -1, KVCacheUsageRatio: 0.9, OK: true},
		},
		{
			name: "json props total_slots",
			body: `{"total_slots":8}`,
			want: ServerMemory{RequestsDeferred: -1, RequestsProcessing: -1, TotalSlots: 8, OK: true},
		},
		{
			name: "json slots array",
			body: `[{"is_processing":true},{"is_processing":false}]`,
			want: ServerMemory{RequestsDeferred: -1, RequestsProcessing: 1, TotalSlots: 2, OK: true},
		},
		{
			name: "junk -> not ok",
			body: "not parseable {",
			want: ServerMemory{RequestsDeferred: -1, RequestsProcessing: -1, OK: false},
		},
	}
	for _, tc := range cases {
		got := parseServerMemory([]byte(tc.body), tc.format)
		if got != tc.want {
			t.Fatalf("%s: parseServerMemory = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestProbeServerMemoryClient(t *testing.T) {
	t.Run("GETs the probe path and parses metrics", func(t *testing.T) {
		var gotPath string
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("llamacpp:requests_deferred 1\nllamacpp:requests_processing 3\n"))
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeServerMemory(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "/metrics", "")
		if err != nil {
			t.Fatalf("ProbeServerMemory err: %v", err)
		}
		if gotPath != "/metrics" {
			t.Fatalf("GET path = %q, want /metrics", gotPath)
		}
		if gotAuth != "" {
			t.Fatalf("probe sent an Authorization header %q, want none", gotAuth)
		}
		if !got.OK || got.RequestsDeferred != 1 || got.RequestsProcessing != 3 {
			t.Fatalf("ProbeServerMemory = %+v, want deferred 1 / processing 3 / OK", got)
		}
	})

	t.Run("blank probe path is a no-op with no HTTP request", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeServerMemory(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "", "")
		if err != nil || got.OK {
			t.Fatalf("blank probe path = (%+v, %v), want ({}, nil)", got, err)
		}
		if hits != 0 {
			t.Fatalf("blank probe path made %d HTTP request(s), want 0", hits)
		}
	})

	t.Run("non-2xx yields an error (dropped signal, fail-open at the caller)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		if _, err := client.ProbeServerMemory(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "/metrics", ""); err == nil {
			t.Fatalf("ProbeServerMemory on 404 = nil error, want an error")
		}
	})
}
