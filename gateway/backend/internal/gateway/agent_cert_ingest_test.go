// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The four cert_* report fields ride the SAME ingest core both transports funnel
// through, so a report over the POST path lands in the registry.
func TestIngestTelemetryStoresCertReport(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"cert_fingerprint":"` + certFPa +
		`","cert_not_after":"2026-11-01T00:00:00Z","cert_mode":"files","cert_ca_fingerprints":["` + certFPb + `"]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rep, ok := srv.AgentCertReports.Get("mock-host-qwen")
	if !ok {
		t.Fatal("no cert report recorded for the reporting server")
	}
	if rep.Fingerprint != certFPa || rep.Mode != "files" {
		t.Fatalf("report = %+v", rep)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !rep.NotAfter.Equal(want) {
		t.Fatalf("not_after = %v, want %v", rep.NotAfter, want)
	}
	if len(rep.CAFingerprints) != 1 || rep.CAFingerprints[0] != certFPb {
		t.Fatalf("ca fingerprints = %v", rep.CAFingerprints)
	}
}

// A mode=off agent can still carry durable gateway trust roots. The shared
// ingest path must retain that evidence even though it has no installed leaf.
func TestIngestTelemetryRetainsTrustOnlyReport(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"cert_mode":"off","cert_ca_fingerprints":["` + certFPb + `"]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rep, ok := srv.AgentCertReports.Get("mock-host-qwen")
	if !ok || rep.Fingerprint != "" || rep.Mode != "off" || len(rep.CAFingerprints) != 1 || rep.CAFingerprints[0] != certFPb {
		t.Fatalf("root-only report was not retained: ok=%v report=%+v", ok, rep)
	}
}

// Sanitization happens in the ingest core (not per-transport), so a garbage
// fingerprint cannot produce a false "installed".
func TestIngestTelemetrySanitizesCertReport(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"cert_fingerprint":"not-a-digest","cert_mode":"banana","cert_ca_fingerprints":["nope"]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if rep, ok := srv.AgentCertReports.Get("mock-host-qwen"); ok {
		t.Fatalf("a garbage report was stored: %+v", rep)
	}
}

// A legacy agent (no cert_* fields at all) must ingest exactly as before and leave
// any previous report untouched -- the additive-field no-op invariant.
func TestIngestTelemetryLegacyPayloadLeavesCertReport(t *testing.T) {
	srv := NewTestServer()
	srv.AgentCertReports.Report("mock-host-qwen", AgentCertReport{Fingerprint: certFPa, Mode: "files"})

	req, raw := ingestReq(t, validIngestAgentBody) // no cert_* keys
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rep, ok := srv.AgentCertReports.Get("mock-host-qwen")
	if !ok || rep.Fingerprint != certFPa {
		t.Fatalf("a legacy payload disturbed the stored report: ok=%v rep=%+v", ok, rep)
	}
}

// The WebSocket transport must record the report too. It funnels through the same
// ingestTelemetrySample, and this pins that: if the cert fields were handled in the
// POST handler instead of the shared core, this test would fail.
func TestAgentStreamStoresCertReport(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_cert_ws", "mock-host-qwen", "cert-ws-secret")

	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "cert-ws-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	frame := streamFrame{Type: "telemetry", Data: json.RawMessage(
		`{"host":{"cpu_util_pct":7},"cert_fingerprint":"` + certFPa + `","cert_mode":"files"}`)}
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if rep, ok := srv.AgentCertReports.Get("mock-host-qwen"); ok {
			if rep.Fingerprint != certFPa || rep.Mode != "files" {
				t.Fatalf("report = %+v", rep)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no cert report recorded from the WebSocket transport")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// gateway.New must default a nil registry so every consumer can call it unguarded.
func TestNewDefaultsAgentCertReportRegistry(t *testing.T) {
	srv := NewTestServer()
	if srv.AgentCertReports == nil {
		t.Fatal("Server.AgentCertReports is nil -- New must default it")
	}
}
