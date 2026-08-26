// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func agentFeaturesRequest(secret, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/features", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

// TestAgentFeaturesEndpoint pins the static-list contract (spec §9): an
// authed agent gets back {"features":["runtime_manager"]} plus a stable
// ETag, and a repeat request with that ETag answers 304 with no body.
func TestAgentFeaturesEndpoint(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentFeaturesRequest("features-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != `{"features":["runtime_manager"]}` {
		t.Fatalf("body = %s, want the static feature list, no etag field in-body", body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag header = %q, want a quoted value", etag)
	}

	// A repeat request carrying that ETag is unchanged -> 304, no body, same
	// ETag header.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, agentFeaturesRequest("features-secret", etag))
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("304 status = %d, body_bytes = %d", rec2.Code, rec2.Body.Len())
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Fatalf("304 must still carry the ETag; got %q, want %q", got, etag)
	}
}

// TestAgentFeaturesEndpointAuthAndMethod pins the shared agent-endpoint
// skeleton: Cache-Control:no-store even on a failure, 401 with no bearer
// (never reaching the feature list), and 405 on a non-GET.
func TestAgentFeaturesEndpointAuthAndMethod(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		srv := NewTestServer()
		seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentFeaturesRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store even on an auth failure", got)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		srv := NewTestServer()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentFeaturesRequest("not-a-token", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		srv := NewTestServer()
		seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")
		req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/features", nil)
		req.Header.Set("Authorization", "Bearer features-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}
