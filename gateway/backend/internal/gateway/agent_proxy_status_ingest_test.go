// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"testing"
)

// TestTelemetryIngestStoresProxyRoutes pins the shared ingest core's proxy_routes
// handling: a posted sample carrying proxy_routes lands in AgentProxyStatus,
// mirroring how the cert_* fields land in AgentCertReports (agent_cert_ingest_test.go).
func TestTelemetryIngestStoresProxyRoutes(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"proxy_routes":[{"listen":8600,"tls_active":true}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := srv.AgentProxyStatus.Status("mock-host-qwen")
	want := []ProxyRouteStatus{{Listen: 8600, TLSActive: true}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("AgentProxyStatus.Status = %+v, want %+v", got, want)
	}
}

// TestTelemetryIngestKeepsProxyRouteState pins the field the gateway used to
// drop on the floor. The agent has always sent `state` (its own
// proxy.RouteState: pending_leaf / invalid_upstream / pending_bind_host /
// bind_failed / active); the gateway decoded only listen+tls_active, so the
// REASON a listener was down never left the agent's own log. It is load-bearing
// now: the https-auto-switch no longer downgrades an application to plaintext
// on tls_active=false, and this string is what the alert it raises instead says
// about WHY.
func TestTelemetryIngestKeepsProxyRouteState(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"proxy_routes":[{"listen":8600,"tls_active":false,"state":"bind_failed"}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := srv.AgentProxyStatus.Status("mock-host-qwen")
	want := []ProxyRouteStatus{{Listen: 8600, TLSActive: false, State: "bind_failed"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("AgentProxyStatus.Status = %+v, want %+v", got, want)
	}
}

// An agent that reports no state at all (an older build, or a route that never
// reached a state) must ingest exactly as before and read back as "" -- not as
// a distinguishable cause.
func TestTelemetryIngestProxyRouteWithoutStateIsEmpty(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"proxy_routes":[{"listen":8600,"tls_active":false}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := srv.AgentProxyStatus.Status("mock-host-qwen")
	if len(got) != 1 || got[0].State != "" {
		t.Fatalf("AgentProxyStatus.Status = %+v, want one route with an empty State", got)
	}
}

// A legacy agent (no proxy_routes key at all, e.g. cert_mode=off/files) must
// ingest exactly as before and report no routes — the additive-field,
// byte-neutral invariant the brief requires.
func TestTelemetryIngestLegacyPayloadHasNoProxyRoutes(t *testing.T) {
	srv := NewTestServer()
	req, raw := ingestReq(t, validIngestAgentBody) // no proxy_routes key
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := srv.AgentProxyStatus.Status("mock-host-qwen"); got != nil {
		t.Fatalf("legacy payload produced proxy routes: %+v", got)
	}
}

// gateway.New must default a nil registry so every consumer can call it unguarded.
func TestNewDefaultsAgentProxyStatusRegistry(t *testing.T) {
	srv := NewTestServer()
	if srv.AgentProxyStatus == nil {
		t.Fatal("Server.AgentProxyStatus is nil -- New must default it")
	}
}
