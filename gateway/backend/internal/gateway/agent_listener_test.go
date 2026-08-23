// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// validAgentBody is a minimal, well-formed agent telemetry payload for
// "mock-host-qwen" (the server seedGatewayTestRoutes creates); it carries no
// server_id so the intake derives the target from the token.
const validAgentBody = `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`

// newAgentGateTestServer builds a *Server with a system-scoped dev bearer token
// ("dev-secret"), a valid agent token bound to "mock-host-qwen" ("agent-secret"),
// and a settings store seeded so netbird_only reflects netbirdOnly and
// netbird_gateway_peer_id reflects gatewayPeerID. AgentListenerActive starts
// false — the caller sets it to model a bound NetBird listener.
func newAgentGateTestServer(t *testing.T, netbirdOnly bool, gatewayPeerID string) *Server {
	t.Helper()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal([]string{"gateway:use", "admin", "system"})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	settings := portal.NewMemorySystemSettings()
	if netbirdOnly {
		_ = settings.SetSystemSetting(context.Background(), "netbird_only", "true", now)
	}
	if gatewayPeerID != "" {
		_ = settings.SetSystemSetting(context.Background(), "netbird_gateway_peer_id", gatewayPeerID, now)
	}
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock(), SystemSettings: settings})
	srv := New(ServerDeps{Tokens: tokens, Usage: recorder, Provider: provider.NewMock(), Routes: routeStore, Portal: svc})
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")
	return srv
}

// (i) netbird_only OFF: the agent route serves on BOTH the public listener
// (ServeHTTP → main mux) and the NetBird listener (AgentHandler → agent mux),
// even with an agent listener active (OFF is the reason, not the fail-safe).
func TestAgentGateOffServesBothListeners(t *testing.T) {
	srv := newAgentGateTestServer(t, false, "")
	srv.SetAgentListener(true, "")

	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentTelemetryRequest("agent-secret", validAgentBody))
	if mainRec.Code != http.StatusOK {
		t.Fatalf("public listener (netbird_only off) = %d, want 200; body=%s", mainRec.Code, mainRec.Body.String())
	}

	agentRec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(agentRec, newAgentTelemetryRequest("agent-secret", validAgentBody))
	if agentRec.Code != http.StatusOK {
		t.Fatalf("agent listener (netbird_only off) = %d, want 200; body=%s", agentRec.Code, agentRec.Body.String())
	}
}

// (ii) netbird_only ON + an agent listener active: the public listener rejects
// with 403 netbird.only (BEFORE agent auth — an unauthenticated probe is also
// rejected), while the NetBird listener still serves.
func TestAgentGateOnRejectsPublicServesAgent(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	srv.SetAgentListener(true, "")

	// Public listener with a valid token → 403 netbird.only (the gate wins).
	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentTelemetryRequest("agent-secret", validAgentBody))
	assertAgentTelemetryError(t, mainRec, http.StatusForbidden, "netbird.only")

	// The gate runs BEFORE handleAgentTelemetry's auth: an UNAUTHENTICATED probe
	// on the public path is rejected with the same 403 (not a 401), proving the
	// isolation cannot be probed around by omitting the token.
	unauthRec := httptest.NewRecorder()
	srv.ServeHTTP(unauthRec, newAgentTelemetryRequest("", validAgentBody))
	assertAgentTelemetryError(t, unauthRec, http.StatusForbidden, "netbird.only")

	// The NetBird listener always serves the agent route (never gated).
	agentRec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(agentRec, newAgentTelemetryRequest("agent-secret", validAgentBody))
	if agentRec.Code != http.StatusOK {
		t.Fatalf("agent listener (netbird_only on) = %d, want 200; body=%s", agentRec.Code, agentRec.Body.String())
	}
}

// (iii) FAIL-SAFE: netbird_only ON but NO agent listener active (AgentListenerActive
// false) → the public listener still serves the agent route (a UI toggle can never
// cut off ALL agent reporting).
func TestAgentGateFailSafeNoListenerServesPublic(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	// AgentListenerActive left false.

	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentTelemetryRequest("agent-secret", validAgentBody))
	if mainRec.Code == http.StatusForbidden {
		t.Fatalf("fail-safe: public listener returned 403 with no agent listener; body=%s", mainRec.Body.String())
	}
	if mainRec.Code != http.StatusOK {
		t.Fatalf("fail-safe: public listener = %d, want 200 (serves normally); body=%s", mainRec.Code, mainRec.Body.String())
	}
}

// TestAgentStreamGateOnRejectsPublicUngatedOnAgentMux mirrors
// TestAgentGateOnRejectsPublicServesAgent for the WebSocket STREAM route
// (agent_stream.go): the /api/agent/v1/stream registration in routes() carries the
// SAME netbird_only gate as the telemetry route, applied BEFORE the WebSocket
// upgrade. There was no prior test for this — neutering the gate on the stream
// route left every existing test green.
//
// (a) netbird_only ON + an agent listener active: the public listener rejects the
// dial with 403 netbird.only, and the handshake never upgrades (no *websocket.Conn).
// (b) The SAME route on the NetBird (agent) listener is UNGATED: a request with no
// bearer reaches handleAgentStream's own auth and is rejected with 401 -- proving
// the netbird_only gate does not run on this mux at all (a 403 here would mean the
// gate leaked onto the mesh listener).
func TestAgentStreamGateOnRejectsPublicUngatedOnAgentMux(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	srv.SetAgentListener(true, "")

	// (a) Public listener, valid bearer -> 403 netbird.only, no upgrade (the gate
	// wins before handleAgentStream's own auth ever runs).
	ts := httptest.NewServer(srv)
	defer ts.Close()
	conn, resp, err := dialAgentStream(t, context.Background(), wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err == nil {
		conn.CloseNow()
		t.Fatal("dial succeeded on the public listener under netbird_only; want a 403 handshake rejection (no upgrade)")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("public listener status = %v, want 403", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "netbird.only") {
		t.Fatalf("public listener body = %s, want netbird.only", body)
	}

	// (b) The NetBird (agent) listener never runs the gate: an UNAUTHENTICATED probe
	// reaches handleAgentStream's own auth prologue and gets 401 auth.invalid_token,
	// NOT the gate's 403 netbird.only.
	agentTS := httptest.NewServer(srv.AgentHandler())
	defer agentTS.Close()
	agentConn, agentResp, err := dialAgentStream(t, context.Background(), wsURL(agentTS.URL, "/api/agent/v1/stream"), "")
	if err == nil {
		agentConn.CloseNow()
		t.Fatal("dial succeeded without a bearer on the agent listener; want a 401 handshake rejection")
	}
	if agentResp == nil || agentResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("agent listener status = %v, want 401 (ungated: rejected by auth, not the netbird.only gate)", agentResp)
	}
}

// TestAgentStreamGateFailSafeNoListenerServesPublic mirrors
// TestAgentGateFailSafeNoListenerServesPublic for the WebSocket STREAM route:
// netbird_only ON but NO agent listener active (AgentListenerActive false) -> the
// public listener still serves (and upgrades) /api/agent/v1/stream, since a UI
// toggle alone can never cut off ALL agent connectivity.
func TestAgentStreamGateFailSafeNoListenerServesPublic(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	// AgentListenerActive left false.

	ts := httptest.NewServer(srv)
	defer ts.Close()
	conn, resp, err := dialAgentStream(t, context.Background(), wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("fail-safe: dial failed (status=%v): %v, want a successful upgrade", resp, err)
	}
	defer conn.CloseNow()
}

// (iv) GET /api/system/netbird/status (system scope) returns the transport flags.
func TestSystemNetbirdStatus(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "peer-xyz")
	srv.SetAgentListener(true, "100.92.0.7:8081")

	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/status", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto systemNetbirdStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !dto.AgentListenerActive {
		t.Errorf("agent_listener_active = false, want true")
	}
	if dto.AgentListenerAddr != "100.92.0.7:8081" {
		t.Errorf("agent_listener_addr = %q, want 100.92.0.7:8081", dto.AgentListenerAddr)
	}
	if !dto.NetbirdOnly {
		t.Errorf("netbird_only = false, want true")
	}
	if dto.GatewayPeerID != "peer-xyz" {
		t.Errorf("gateway_peer_id = %q, want peer-xyz", dto.GatewayPeerID)
	}
	// Module is not configured (no url/token) → connected best-effort is false.
	if dto.GatewayPeerConnected {
		t.Errorf("gateway_peer_connected = true, want false (module off)")
	}
}

// The status endpoint is system-scoped: a non-system principal is forbidden.
func TestSystemNetbirdStatusRequiresSystemScope(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin"}) // no "system"
	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/status", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status without system scope = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}
