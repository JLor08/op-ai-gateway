// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// validIngestAgentBody is a minimal, well-formed telemetry payload (no
// server_id; the server id is passed to ingest directly). Named distinctly
// from agent_listener_test.go's package-level validAgentBody const (an
// existing, unrelated NetBird-agent-listener test fixture) to avoid a
// redeclaration in this package.
const validIngestAgentBody = `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{},"host":{"cpu_util_pct":42}}`

func ingestReq(t *testing.T, raw string) (agentTelemetryRequest, json.RawMessage) {
	t.Helper()
	var req agentTelemetryRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return req, json.RawMessage(raw)
}

func TestIngestTelemetrySampleErrorMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("mismatch", func(t *testing.T) {
		srv := NewTestServer()
		req, raw := ingestReq(t, `{"server_id":"other-host","host":{"cpu_util_pct":1}}`)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		if !errors.Is(err, ErrAgentServerMismatch) {
			t.Fatalf("err = %v, want ErrAgentServerMismatch", err)
		}
	})

	t.Run("invalid payload carries detail", func(t *testing.T) {
		srv := NewTestServer()
		// A negative cpu_util_pct derives a negative cpu_load, which telemetryFromRequest rejects.
		req, raw := ingestReq(t, `{"host":{"cpu_util_pct":-5}}`)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		if !errors.Is(err, errAgentTelemetryInvalid) {
			t.Fatalf("err = %v, want errAgentTelemetryInvalid", err)
		}
		if err.Error() != "cpu_load must be non-negative" {
			t.Fatalf("err detail = %q, want %q", err.Error(), "cpu_load must be non-negative")
		}
	})

	t.Run("unknown server", func(t *testing.T) {
		srv := NewTestServer()
		req, raw := ingestReq(t, validIngestAgentBody)
		// A server id with no AIServer row.
		err := srv.ingestTelemetrySample(ctx, "ghost-host", req, raw)
		if !errors.Is(err, errAgentUnknownServer) {
			t.Fatalf("err = %v, want errAgentUnknownServer", err)
		}
	})

	t.Run("store error keeps the specific code", func(t *testing.T) {
		srv := NewTestServer()
		srv.Routes = failingSampleStore{srv.Routes}
		req, raw := ingestReq(t, validIngestAgentBody)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		var se *storeTelemetryError
		if !errors.As(err, &se) {
			t.Fatalf("err = %v, want *storeTelemetryError", err)
		}
		if se.code != "agent.telemetry_sample_failed" {
			t.Fatalf("code = %q, want agent.telemetry_sample_failed", se.code)
		}
	})

	t.Run("happy path fans out + reports presence", func(t *testing.T) {
		srv := NewTestServer()
		_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
		defer unsub()
		req, raw := ingestReq(t, validIngestAgentBody)
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		select {
		case s := <-ch:
			if s.ServerID != "mock-host-qwen" || s.CPUUtilPct != 42 {
				t.Fatalf("fanned = %+v", s)
			}
		default:
			t.Fatal("no fanned-out sample")
		}
		if !srv.AgentPresence.Reporting("mock-host-qwen") {
			t.Fatal("agent presence not reported")
		}
	})
}
