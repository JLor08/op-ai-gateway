// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
)

// newAgentSystemReportRequest mirrors newAgentTelemetryRequest (server_test.go) for
// the system-report POST route.
func newAgentSystemReportRequest(secret string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/system-report", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req
}

// TestAgentSystemReportGateOnRejectsPublicServesAgent mirrors
// TestAgentGateOnRejectsPublicServesAgent (agent_listener_test.go, telemetry route)
// for the system-report POST route: the /api/agent/v1/system-report registration in
// routes() must carry the SAME netbird_only gate as telemetry/stream, applied BEFORE
// handleAgentSystemReport's own auth. Stripping the gate on this route (e.g.
// registering it directly as s.mux.HandleFunc("/api/agent/v1/system-report",
// s.handleAgentSystemReport) without the netbird_only closure) makes step (a) below
// return 200 instead of 403 -- this test is designed to catch exactly that.
func TestAgentSystemReportGateOnRejectsPublicServesAgent(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	srv.SetAgentListener(true, "")

	// (a) Public listener, valid bearer -> 403 netbird.only (the gate wins before
	// handleAgentSystemReport's own auth ever runs).
	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentSystemReportRequest("agent-secret", validSystemReportBody))
	assertAgentTelemetryError(t, mainRec, http.StatusForbidden, "netbird.only")

	// The gate runs BEFORE auth: an UNAUTHENTICATED probe on the public path is also
	// rejected with the same 403 (not a 401), proving isolation cannot be probed
	// around by omitting the token.
	unauthRec := httptest.NewRecorder()
	srv.ServeHTTP(unauthRec, newAgentSystemReportRequest("", validSystemReportBody))
	assertAgentTelemetryError(t, unauthRec, http.StatusForbidden, "netbird.only")

	// (b) The NetBird (agent) listener never runs the gate: a request with NO bearer
	// reaches handleAgentSystemReport's own auth prologue and gets 401
	// auth.invalid_token, NOT the gate's 403 netbird.only -- proving the netbird_only
	// gate does not leak onto this mux at all.
	agentUnauthRec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(agentUnauthRec, newAgentSystemReportRequest("", validSystemReportBody))
	assertAgentTelemetryError(t, agentUnauthRec, http.StatusUnauthorized, "auth.invalid_token")

	// And with a valid bearer, the agent listener serves normally (200).
	agentOKRec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(agentOKRec, newAgentSystemReportRequest("agent-secret", validSystemReportBody))
	if agentOKRec.Code != http.StatusOK {
		t.Fatalf("agent listener (netbird_only on) = %d, want 200; body=%s", agentOKRec.Code, agentOKRec.Body.String())
	}
}

// TestAgentSystemReportGateFailSafeNoListenerServesPublic mirrors
// TestAgentGateFailSafeNoListenerServesPublic (agent_listener_test.go): netbird_only
// ON but NO agent listener active (AgentListenerActive false) -> the public listener
// still serves /api/agent/v1/system-report (a UI toggle alone can never cut off ALL
// agent reporting).
func TestAgentSystemReportGateFailSafeNoListenerServesPublic(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	// AgentListenerActive left false.

	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentSystemReportRequest("agent-secret", validSystemReportBody))
	if mainRec.Code == http.StatusForbidden {
		t.Fatalf("fail-safe: public listener returned 403 with no agent listener; body=%s", mainRec.Body.String())
	}
	if mainRec.Code != http.StatusOK {
		t.Fatalf("fail-safe: public listener = %d, want 200 (serves normally); body=%s", mainRec.Code, mainRec.Body.String())
	}
}

// TestAgentStreamSystemReportIngestsFrame drives a valid "system_report" WS frame
// end-to-end (not just the ingestSystemReport unit-level tests in
// system_report_ingest_test.go), proving the case "system_report" wiring in
// handleAgentStream's read loop. A telemetry frame sent immediately after is used as
// a synchronization signal: the read loop processes frames strictly sequentially in
// one goroutine, so once the telemetry frame's effect is observed via the ServerPerf
// subscription, the system_report frame above is guaranteed to be fully ingested.
func TestAgentStreamSystemReportIngestsFrame(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws_sysreport_ok", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := wsjson.Write(ctx, conn, streamFrame{Type: "system_report", Data: json.RawMessage(validSystemReportBody)}); err != nil {
		t.Fatalf("write system_report: %v", err)
	}
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":5}}`)}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 5 {
			t.Fatalf("cpu = %v, want 5", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry not ingested after the system_report frame (WS wiring broken)")
	}

	hw, ok, err := srv.Routes.ServerHardwareByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("ServerHardwareByServer ok=%v err=%v (the system_report WS frame was not ingested)", ok, err)
	}
	if !strings.Contains(hw.ReportJSON, `"agent_version":"1.2.3"`) {
		t.Fatalf("report json = %s", hw.ReportJSON)
	}
}

// TestAgentStreamSystemReportSkipsInvalidFrameKeepsConnection mirrors
// TestAgentStreamSkipsBadFrameKeepsConnection (agent_stream_test.go, "telemetry") for
// the "system_report" case: an invalid payload must be SKIPPED (continue), not close
// the connection. Mutation: changing the errAgentSystemReportInvalid branch in
// handleAgentStream from "continue" to "close+return" makes the trailing telemetry
// frame never arrive -- this test fails on that mutation.
func TestAgentStreamSystemReportSkipsInvalidFrameKeepsConnection(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws_sysreport_bad", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Invalid system_report payload (syntactically valid JSON, but a string where the
	// schema expects an int -- json.Unmarshal rejects it with a type error) -> skipped,
	// connection stays open. (A syntactically MALFORMED payload, e.g. "{bad", can't
	// even be sent as a json.RawMessage WS frame -- the client-side marshal itself
	// rejects it before anything reaches the server.)
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "system_report", Data: json.RawMessage(`{"cpu":{"physical_cores":"nope"}}`)}); err != nil {
		t.Fatalf("write bad system_report: %v", err)
	}
	// A valid telemetry frame right after it still ingests -- proving the read loop
	// (and the connection) survived the invalid system_report frame.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":11}}`)}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 11 {
			t.Fatalf("cpu = %v, want 11 (bad system_report frame must have been skipped)", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection dropped after an invalid system_report frame")
	}
}

// Note: an "unknown server" WS close test (mirroring TestAgentStreamCloseOnServerMismatch)
// is deliberately NOT included here. Both the sqlite and memory stores enforce, at
// UpsertAgentToken, that the token's server id already exists (routing.MemoryStore.
// UpsertAgentToken / store.SQLiteStore.UpsertAgentToken both return ErrNotFound for a
// nonexistent server id) -- so a valid, registered agent token can never point at a
// server id absent from AIServerByID, for EITHER route (telemetry or system-report).
// This is why the existing agent_stream_test.go has no such WS-level test for the
// telemetry case either; the errAgentUnknownServer branch is exercised at the
// ingestSystemReport unit level instead (system_report_ingest_test.go, "unknown server").

// TestSanitizeSystemReportClampsNonFiniteBaseMHz is a data-loss guard: a NaN/Inf
// cpu.base_mhz (unreachable via literal JSON text, since encoding/json rejects those
// tokens -- so this exercises sanitizeSystemReport directly with an in-memory
// non-finite float, the only way to reach the guard) must be CLAMPED to 0, not left
// in place. Leaving it in place makes the subsequent json.Marshal fail (encoding/json
// rejects NaN/Inf floats), and sanitizeSystemReport's fallback returns the EMPTY
// object "{}" -- silently discarding the entire report, not just the bad field.
// Mutation: removing the math.IsNaN/math.IsInf disjuncts from the BaseMHz guard in
// sanitizeSystemReport (leaving only the "< 0" check) makes this fail for NaN and
// +Inf (neither compares < 0), reproducing the exact "{}" data-loss it guards against.
func TestSanitizeSystemReportClampsNonFiniteBaseMHz(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		r := &agentSystemReport{
			AgentVersion: "1.0",
			CPU:          agentCPUInfo{Model: "NaNCPU", BaseMHz: bad},
		}
		canonical, _ := sanitizeSystemReport(r, now)
		if string(canonical) == "{}" {
			t.Fatalf("base_mhz=%v: sanitize fell back to the empty-object marshal-error path -- the NaN/Inf guard did not clamp it", bad)
		}
		var got agentSystemReport
		if err := json.Unmarshal(canonical, &got); err != nil {
			t.Fatalf("base_mhz=%v: canonical blob not parseable: %v (%s)", bad, err, canonical)
		}
		if got.CPU.BaseMHz != 0 {
			t.Fatalf("base_mhz=%v: got.CPU.BaseMHz = %v, want 0 (clamped)", bad, got.CPU.BaseMHz)
		}
		if got.CPU.Model != "NaNCPU" || got.AgentVersion != "1.0" {
			t.Fatalf("base_mhz=%v: the rest of the report was not preserved (full report lost, not just the bad field): %#v", bad, got)
		}
	}
}
