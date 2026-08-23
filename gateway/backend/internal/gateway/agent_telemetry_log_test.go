// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// failingSampleStore delegates everything to the embedded store but fails the
// rich-sample insert, simulating e.g. a missing server_telemetry_samples table.
type failingSampleStore struct {
	routing.Store
}

func (failingSampleStore) InsertTelemetrySample(context.Context, routing.TelemetrySample) error {
	return errors.New("no such table: server_telemetry_samples")
}

// withCapturedSlog swaps the process-global slog default for a Debug-level
// handler backed by a fresh logbuffer.Buffer (the same wiring buildGatewayServer
// installs), returning the buffer and a restore func. handleAgentTelemetry logs
// via slog.Debug/Warn on the default logger, so this lets a test observe the
// records the portal Logs view would surface.
func withCapturedSlog(t *testing.T) (*logbuffer.Buffer, func()) {
	t.Helper()
	buf := logbuffer.NewBuffer(200, slog.LevelDebug)
	prev := slog.Default()
	slog.SetDefault(slog.New(buf.Handler(io.Discard)))
	return buf, func() { slog.SetDefault(prev) }
}

func findLogRecord(recs []logbuffer.Record, level, substr string) bool {
	for _, r := range recs {
		if r.Level == level && strings.Contains(r.Msg, substr) {
			return true
		}
	}
	return false
}

// assertNoSecretInLogs is the CRITICAL guard: the bearer/agent token must never
// land in any log Record (msg or a string attr).
func assertNoSecretInLogs(t *testing.T, recs []logbuffer.Record, secret string) {
	t.Helper()
	for _, r := range recs {
		if strings.Contains(r.Msg, secret) {
			t.Fatalf("secret %q leaked into log msg: %q", secret, r.Msg)
		}
		for k, v := range r.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Fatalf("secret %q leaked into log attr %q: %q", secret, k, s)
			}
		}
	}
}

func TestAgentTelemetryLogsDebugOnReceive(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_log", "mock-host-qwen", "agent-secret")
	body := `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{},"host":{"cpu_util_pct":42},"gpus":[{"index":0,"name":"RTX 4090","util_pct":88,"mem_used_bytes":1000,"mem_total_bytes":2000}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("agent-secret", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	recs := buf.Snapshot()
	var found bool
	for _, r := range recs {
		if r.Level == "DEBUG" && strings.Contains(r.Msg, "agent telemetry received") {
			if r.Attrs["server_id"] != "mock-host-qwen" {
				t.Fatalf("server_id attr = %v, want mock-host-qwen", r.Attrs["server_id"])
			}
			if r.Attrs["has_host"] != true {
				t.Fatalf("has_host attr = %v, want true", r.Attrs["has_host"])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no DEBUG 'agent telemetry received' record; got %+v", recs)
	}
	assertNoSecretInLogs(t, recs, "agent-secret")
}

func TestAgentTelemetryLogsWarnOnMismatch(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_log", "mock-host-qwen", "agent-secret")
	mismatchBody := `{"server_id":"mock-host-comp","agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("agent-secret", mismatchBody))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", "mismatch") {
		t.Fatalf("no WARN mismatch record; got %+v", recs)
	}
	assertNoSecretInLogs(t, recs, "agent-secret")
}

func TestAgentTelemetryLogsErrorOnStoreFailure(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_log", "mock-host-qwen", "agent-secret")
	// Wrap the store so the sample insert fails AFTER auth/parse succeed — the
	// exact shape of the user's HTTP 500 (accepted, then store write fails).
	srv.Routes = failingSampleStore{srv.Routes}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("agent-secret", `{"host":{"cpu_util_pct":5},"gpus":[{"index":0,"name":"RTX 4090","util_pct":10,"mem_used_bytes":1,"mem_total_bytes":2}]}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	recs := buf.Snapshot()
	// The 500 must be diagnosable: an ERROR record naming the store failure.
	if !findLogRecord(recs, "ERROR", "sample insert failed") {
		t.Fatalf("no ERROR 'sample insert failed' record; got %+v", recs)
	}
	// The exact store error message must be surfaced to the portal (validating the
	// error-attr coercion end-to-end), not "{}".
	var sawMsg bool
	for _, r := range recs {
		if r.Level == "ERROR" {
			if e, ok := r.Attrs["err"].(string); ok && strings.Contains(e, "server_telemetry_samples") {
				sawMsg = true
			}
		}
	}
	if !sawMsg {
		t.Fatalf("the underlying store error message is not in the ERROR record's err attr; got %+v", recs)
	}
	assertNoSecretInLogs(t, recs, "agent-secret")
}

func TestAgentTelemetryLogsWarnOnUnknownToken(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_log", "mock-host-qwen", "agent-secret")
	// A real agent hitting the gateway with a WRONG/unknown token — the most
	// common connection misconfiguration. It must be visible in the Logs view.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("wrong-token-xyz", `{"host":{"cpu_util_pct":1}}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", "unknown agent token") {
		t.Fatalf("no WARN 'unknown agent token' record; got %+v", recs)
	}
	// Neither the wrong token nor the real seeded token may appear in any record.
	assertNoSecretInLogs(t, recs, "wrong-token-xyz")
	assertNoSecretInLogs(t, recs, "agent-secret")
}
