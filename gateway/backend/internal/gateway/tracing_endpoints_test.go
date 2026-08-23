// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/tracing"
	"strings"
	"testing"
)

// newTracingTestServer builds a *Server whose dev token carries the "system"
// scope and whose Tracing provider is a real (disabled) tracing.Provider. The
// bearer secret is "dev-secret" (from NewTestServerWithTokenScopes).
func newTracingTestServer(t *testing.T) *Server {
	t.Helper()
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	logs := logbuffer.NewBuffer(200, slog.LevelInfo)
	provider, err := tracing.Setup(tracing.Options{Enabled: false, SampleRatio: 1.0}, logs)
	if err != nil {
		t.Fatalf("tracing.Setup: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	srv.Logs = logs
	srv.Tracing = provider
	return srv
}

func TestSystemTracingGetReportsDisabled(t *testing.T) {
	srv := newTracingTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/system/tracing", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto tracingStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if dto.Enabled {
		t.Fatalf("enabled = true, want false")
	}
}

func TestSystemTracingPutFlipsEnabled(t *testing.T) {
	srv := newTracingTestServer(t)

	putReq := httptest.NewRequest(http.MethodPut, "/api/system/tracing", strings.NewReader(`{"enabled":true}`))
	putReq.Header.Set("Authorization", "Bearer dev-secret")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)

	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", putRec.Code, putRec.Body.String())
	}
	var dto tracingStatusDTO
	if err := json.Unmarshal(putRec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if !dto.Enabled {
		t.Fatalf("PUT response enabled = false, want true")
	}
	if !srv.Tracing.Enabled() {
		t.Fatalf("provider.Enabled() = false after PUT, want true")
	}

	// GET now reflects the flipped state.
	getReq := httptest.NewRequest(http.MethodGet, "/api/system/tracing", nil)
	getReq.Header.Set("Authorization", "Bearer dev-secret")
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	var got tracingStatusDTO
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("GET enabled = false after PUT, want true")
	}
}

func TestSystemTracingForbiddenForNonSystem(t *testing.T) {
	// NewTestServer's token has gateway:use + admin but NOT system.
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/system/tracing", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSystemTracingNilProviderReportsDisabled(t *testing.T) {
	// A system-scoped token but no Tracing provider wired -> nil-safe: 200,
	// disabled, PUT is a no-op (never a panic/500).
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	req := httptest.NewRequest(http.MethodGet, "/api/system/tracing", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto tracingStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if dto.Enabled {
		t.Fatalf("enabled = true, want false with nil provider")
	}

	putReq := httptest.NewRequest(http.MethodPut, "/api/system/tracing", strings.NewReader(`{"enabled":true}`))
	putReq.Header.Set("Authorization", "Bearer dev-secret")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body = %s", putRec.Code, putRec.Body.String())
	}
}
