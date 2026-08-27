// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
)

// validRuntimeReportBody is a minimal, well-formed file-mode runtime report
// (the WS/POST `data`), carrying one plaintext-secret env value the gateway
// MUST redact before it is ever stored (design spec §10.2 defense in depth).
const validRuntimeReportBody = `{"source":"file","collected_at":"2026-08-20T09:00:00Z","config":{"router_listen":8081,"max_processes":2,"gpu_budgets":[{"index":0,"budget_mb":24000}],"specs":[{"id":"local-spec-1","model":"qwen-coder","upstream_model":"qwen2.5-coder-32b","binary":"/usr/bin/vllm","args":["--port","9001"],"env":{"HF_TOKEN":"hf_ThisIsAPlaintextSecretDoNotStore"},"listen_port":9001,"health_path":"/health","health_timeout_seconds":5,"startup_timeout_seconds":180,"idle_timeout_seconds":900,"admission_wait_timeout_seconds":0,"pinned":false,"admin_state":""}],"coresident":[]}}`

func TestIngestRuntimeReport(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path stores canonical JSON and masks env values", func(t *testing.T) {
		srv := NewTestServer()
		if err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", json.RawMessage(validRuntimeReportBody)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		report, ok, err := srv.Routes.ServerRuntimeReportByServer(ctx, "mock-host-qwen")
		if err != nil || !ok {
			t.Fatalf("ServerRuntimeReportByServer ok=%v err=%v", ok, err)
		}
		if !strings.Contains(report.ReportJSON, `"source":"file"`) {
			t.Fatalf("report json = %s", report.ReportJSON)
		}
		// The plaintext secret must never appear anywhere in the stored blob.
		if strings.Contains(report.ReportJSON, "hf_ThisIsAPlaintextSecretDoNotStore") {
			t.Fatalf("stored blob leaked the plaintext secret: %s", report.ReportJSON)
		}
		if !strings.Contains(report.ReportJSON, runtimeReportEnvMask) {
			t.Fatalf("stored blob missing the redaction mask %q: %s", runtimeReportEnvMask, report.ReportJSON)
		}
		// The canonical blob must re-parse as an agentRuntimeReport, with the
		// key visible and the value masked (keys are NOT secrets -- only
		// values are).
		var got agentRuntimeReport
		if err := json.Unmarshal([]byte(report.ReportJSON), &got); err != nil {
			t.Fatalf("canonical json not parseable: %v", err)
		}
		var cfg agentRuntimeReportConfig
		if err := json.Unmarshal(got.Config, &cfg); err != nil {
			t.Fatalf("config not parseable: %v", err)
		}
		if len(cfg.Specs) != 1 || cfg.Specs[0].Env["HF_TOKEN"] != runtimeReportEnvMask {
			t.Fatalf("decoded specs = %#v", cfg.Specs)
		}
		if cfg.Specs[0].Binary != "/usr/bin/vllm" || cfg.Specs[0].Model != "qwen-coder" {
			t.Fatalf("non-secret fields not preserved: %#v", cfg.Specs[0])
		}
	})

	t.Run("invalid json -> errAgentRuntimeReportInvalid", func(t *testing.T) {
		srv := NewTestServer()
		err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", json.RawMessage(`{bad`))
		if !errors.Is(err, errAgentRuntimeReportInvalid) {
			t.Fatalf("err = %v, want errAgentRuntimeReportInvalid", err)
		}
	})

	t.Run("unknown server -> errAgentUnknownServer", func(t *testing.T) {
		srv := NewTestServer()
		err := srv.ingestRuntimeReport(ctx, "ghost-host", json.RawMessage(validRuntimeReportBody))
		if !errors.Is(err, errAgentUnknownServer) {
			t.Fatalf("err = %v, want errAgentUnknownServer", err)
		}
	})

	t.Run("malformed config does not reject the report", func(t *testing.T) {
		srv := NewTestServer()
		raw := json.RawMessage(`{"source":"file","parse_error":"json_syntax","config":"not-an-object"}`)
		if err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		report, ok, _ := srv.Routes.ServerRuntimeReportByServer(ctx, "mock-host-qwen")
		if !ok {
			t.Fatal("report must still be stored")
		}
		// parse_error is an allow-listed classification code and survives
		// verbatim; the unusable config alongside it is dropped, not fatal.
		if !strings.Contains(report.ReportJSON, `"parse_error":"json_syntax"`) {
			t.Fatalf("parse_error classification not preserved: %s", report.ReportJSON)
		}
	})

	// TestIngestRuntimeReport/parse_error redaction: a secret embedded in a
	// config-loader's error string (the exact leak class the env mask exists
	// to prevent, arriving through a NEIGHBORING field -- see
	// redactRuntimeReportParseError's doc) must never reach the stored blob.
	// A compromised or buggy agent CAN send free text here; the allow-list is
	// what makes that harmless.
	t.Run("parse_error redaction strips a secret quoted after the classification", func(t *testing.T) {
		srv := NewTestServer()
		raw := json.RawMessage(`{"source":"file","parse_error":"yaml: line 12: found character that cannot start any token near HF_TOKEN=sk-superSecretValue123"}`)
		if err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		report, ok, _ := srv.Routes.ServerRuntimeReportByServer(ctx, "mock-host-qwen")
		if !ok {
			t.Fatal("report must still be stored")
		}
		if strings.Contains(report.ReportJSON, "sk-superSecretValue123") {
			t.Fatalf("stored blob leaked a secret embedded in parse_error: %s", report.ReportJSON)
		}
		if !strings.Contains(report.ReportJSON, `"parse_error":"`+runtimeReportParseErrorGeneric+`"`) {
			t.Fatalf("free text must degrade to the generic constant: %s", report.ReportJSON)
		}
	})

	t.Run("SetFileMode flips on source", func(t *testing.T) {
		srv := NewTestServer()
		if srv.RuntimeStatus.IsFileMode("mock-host-qwen") {
			t.Fatal("must not be file-mode before any report")
		}
		if err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", json.RawMessage(validRuntimeReportBody)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if !srv.RuntimeStatus.IsFileMode("mock-host-qwen") {
			t.Fatal("source:file report must flip IsFileMode to true")
		}
		gatewaySourced := strings.Replace(validRuntimeReportBody, `"source":"file"`, `"source":"gateway"`, 1)
		if err := srv.ingestRuntimeReport(ctx, "mock-host-qwen", json.RawMessage(gatewaySourced)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if srv.RuntimeStatus.IsFileMode("mock-host-qwen") {
			t.Fatal("a later source:gateway report must flip IsFileMode back to false")
		}
	})
}

// TestIngestRuntimeReportParseErrorRedactionShapes is C2's gateway-side pin.
// parse_error is now an ALLOW-LIST over the agent's closed set of
// classification codes (runtime.ParseErrorCode): a code in the set survives
// verbatim, EVERYTHING else degrades to runtimeReportParseErrorGeneric, and
// an empty field stays empty.
//
// The table keeps every leak shape the two earlier rules failed on, so the
// history stays covered, and adds the two failures that motivated the
// closed set: the degenerate "runtime" output (the ONE thing the previous
// rule could ever keep, for every malformed file an operator could write),
// and the empty field -- which the previous rule rewrote into a fabricated
// "config parse error" on every healthy file-mode report.
//
// Each case asserts on the STORED blob (not just the pure function), because
// that is what the report view actually serves.
func TestIngestRuntimeReportParseErrorRedactionShapes(t *testing.T) {
	longToken := strings.Repeat("x", 80)
	generic := `"parse_error":"` + runtimeReportParseErrorGeneric + `"`
	cases := []struct {
		name           string
		parseError     string
		wantStored     string
		mustNotContain []string
	}{
		{
			name:       "json_syntax code kept",
			parseError: "json_syntax",
			wantStored: `"parse_error":"json_syntax"`,
		},
		{
			name:       "duplicate_spec_id code kept",
			parseError: "duplicate_spec_id",
			wantStored: `"parse_error":"duplicate_spec_id"`,
		},
		{
			// The whole reason for the closed set: the previous rule kept
			// this, and it was the only non-generic value ANY real parse
			// failure could produce.
			name:           "the degenerate package prefix is not a code",
			parseError:     "runtime",
			wantStored:     generic,
			mustNotContain: []string{`"parse_error":"runtime"`},
		},
		{
			name:           "a real ParseConfig error text is not a code",
			parseError:     `runtime: parse config: invalid character 'n' looking for beginning of value`,
			wantStored:     generic,
			mustNotContain: []string{"invalid character", "looking for beginning"},
		},
		{
			// The agent's own defensive floor is deliberately NOT allowed:
			// "could not classify" and "the gateway did not recognise it"
			// deserve the same, single generic rendering.
			name:       "the agent's unclassified floor degrades to generic",
			parseError: "unclassified",
			wantStored: generic,
		},
		{
			// Round 1 kept the text up to the first colon and passed a
			// colon-less string through VERBATIM.
			name:           "no colon at all",
			parseError:     `unexpected token "HF_TOKEN=sk-abc123" in config`,
			wantStored:     generic,
			mustNotContain: []string{"sk-abc123", "HF_TOKEN"},
		},
		{
			// The secret sits BEFORE the first colon: round 1's
			// "keep the prefix" rule preserved exactly the wrong half.
			name:           "secret before the first colon",
			parseError:     `"HF_TOKEN=sk-abc123": invalid value`,
			wantStored:     generic,
			mustNotContain: []string{"sk-abc123", "HF_TOKEN"},
		},
		{
			name:           "long token-shaped prefix",
			parseError:     longToken + `: something failed`,
			wantStored:     generic,
			mustNotContain: []string{longToken},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			raw, err := json.Marshal(map[string]any{"source": "file", "parse_error": tc.parseError})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := srv.ingestRuntimeReport(context.Background(), "mock-host-qwen", raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			report, ok, err := srv.Routes.ServerRuntimeReportByServer(context.Background(), "mock-host-qwen")
			if err != nil || !ok {
				t.Fatalf("lookup ok=%v err=%v", ok, err)
			}
			if !strings.Contains(report.ReportJSON, tc.wantStored) {
				t.Fatalf("stored blob = %s, want to contain %q", report.ReportJSON, tc.wantStored)
			}
			for _, bad := range tc.mustNotContain {
				if strings.Contains(report.ReportJSON, bad) {
					t.Fatalf("stored blob leaked %q: %s", bad, report.ReportJSON)
				}
			}
		})
	}
}

// TestIngestRuntimeReportHealthyFileModeReportHasNoParseError is the second
// C2 defect, found while fixing the first and reproduced against 86a287b:
// parse_error is omitempty, so a file-mode agent whose local config parsed
// PERFECTLY sends no such field -- and the old redaction rule, which had no
// empty-input case, rewrote that "" into the generic "config parse error".
//
// The consequence was not cosmetic. The portal renders parse_error as a
// warning alert AND uses it to decide whether to render the reported config
// at all (RuntimeAdminSection.tsx: `fileMode && !parseError ? ... : null`),
// so EVERY healthy file-mode server permanently displayed "the agent could
// not parse its local configuration file" with its config view suppressed.
func TestIngestRuntimeReportHealthyFileModeReportHasNoParseError(t *testing.T) {
	srv := NewTestServer()
	// Exactly what runtime.BuildReport emits when the local file parsed:
	// no parse_error key at all.
	raw := []byte(`{"source":"file","collected_at":"2026-07-11T12:00:00Z","config":{"router_listen":9000,"max_processes":2,"gpu_budgets":[],"specs":[],"coresident":[]}}`)
	if err := srv.ingestRuntimeReport(context.Background(), "mock-host-qwen", raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	report, ok, err := srv.Routes.ServerRuntimeReportByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	if strings.Contains(report.ReportJSON, "parse_error") {
		t.Fatalf("a healthy file-mode report was stored with a fabricated parse error: %s", report.ReportJSON)
	}
}

// TestHandleAgentRuntimeReportPOST proves the POST endpoint is registered on
// the public mux, authenticates via the agent bearer token, and calls the
// SAME ingestRuntimeReport (parity with the WS transport) -- mirrors
// TestHandleAgentSystemReportPOST.
func TestHandleAgentRuntimeReportPOST(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/runtime-report", strings.NewReader(validRuntimeReportBody))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accepted bool   `json:"accepted"`
		ServerID string `json:"server_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Accepted || body.ServerID != "mock-host-qwen" {
		t.Fatalf("body = %#v", body)
	}
	if _, ok, _ := srv.Routes.ServerRuntimeReportByServer(context.Background(), "mock-host-qwen"); !ok {
		t.Fatal("report not stored")
	}
}

func TestHandleAgentRuntimeReportPOSTRejectsBadToken(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/runtime-report", strings.NewReader(validRuntimeReportBody))
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A syntactically MALFORMED body (e.g. "{bad") is rejected earlier, by the
// shared readRawJSON decode check, with request.invalid_json -- before
// ingestRuntimeReport (and its errAgentRuntimeReportInvalid) is ever reached
// (mirrors handleAgentTelemetry/handleAgentSystemReport's identical
// prologue). This test instead uses a payload that is syntactically valid
// JSON but fails to unmarshal into agentRuntimeReport's typed fields (a
// string where CollectedAt expects RFC3339), the same payload the WS
// skip-invalid-frame test below uses to exercise this exact branch.
func TestHandleAgentRuntimeReportPOSTInvalidPayload(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/runtime-report", strings.NewReader(`{"collected_at":"nope"}`))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "agent.runtime_report_invalid" {
		t.Fatalf("error code = %q, want agent.runtime_report_invalid", code)
	}
}

// Note: a POST/WS-level "unknown server" test is deliberately NOT included
// here, for the SAME reason system_report_gate_test.go gives right above its
// own equivalent note: both stores enforce, at UpsertAgentToken, that the
// token's server id already exists -- and MemoryStore.DeleteAIServer also
// cascades the delete onto that server's agent token, so there is no way to
// reach errAgentUnknownServer through an authenticated transport call. The
// errAgentUnknownServer branch is exercised at the ingestRuntimeReport unit
// level instead (TestIngestRuntimeReport, "unknown server").

// TestIngestRuntimeReportDropsInjectedFields is the redaction-is-defense-in-
// depth proof (Task 9 brief): even if a buggy/compromised agent sends the
// plaintext secret through some OTHER, unmodeled field, the canonical
// re-marshal (a typed struct with a fixed field set) never round-trips an
// unknown key -- and an env value under a KNOWN field is still masked.
// Mirrors TestIngestSystemReportDropsInjectedSerialFields.
func TestIngestRuntimeReportDropsInjectedFields(t *testing.T) {
	srv := NewTestServer()
	raw := json.RawMessage(`{"source":"file","injected_field":"leak-me-top-level","config":{"specs":[{"id":"s1","env":{"HF_TOKEN":"plaintext-secret-value"},"unknown_spec_field":"leak-me-nested"}]}}`)
	if err := srv.ingestRuntimeReport(context.Background(), "mock-host-qwen", raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	report, ok, err := srv.Routes.ServerRuntimeReportByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	for _, leaked := range []string{"leak-me-top-level", "leak-me-nested", "plaintext-secret-value", "injected_field", "unknown_spec_field"} {
		if strings.Contains(report.ReportJSON, leaked) {
			t.Fatalf("canonical blob leaked %q: %s", leaked, report.ReportJSON)
		}
	}
	if !strings.Contains(report.ReportJSON, runtimeReportEnvMask) {
		t.Fatalf("expected the env mask to be present: %s", report.ReportJSON)
	}
}

// TestAgentStreamRuntimeReportIngestsFrame drives a valid "runtime_report" WS
// frame end-to-end, proving the case "runtime_report" wiring in
// handleAgentStream's read loop -- mirrors
// TestAgentStreamSystemReportIngestsFrame (system_report_gate_test.go)
// exactly, including its telemetry-frame synchronization trick (the read
// loop processes frames strictly sequentially in one goroutine, so once the
// telemetry frame's effect is observed via the ServerPerf subscription, the
// runtime_report frame above is guaranteed to be fully ingested).
func TestAgentStreamRuntimeReportIngestsFrame(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws_runtimereport_ok", "mock-host-qwen", "agent-secret")
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

	if err := wsjson.Write(ctx, conn, streamFrame{Type: "runtime_report", Data: json.RawMessage(validRuntimeReportBody)}); err != nil {
		t.Fatalf("write runtime_report: %v", err)
	}
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":6}}`)}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 6 {
			t.Fatalf("cpu = %v, want 6", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("telemetry not ingested after the runtime_report frame (WS wiring broken)")
	}

	report, ok, err := srv.Routes.ServerRuntimeReportByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("report not stored via WS: ok=%v err=%v (the runtime_report WS frame was not ingested)", ok, err)
	}
	if strings.Contains(report.ReportJSON, "hf_ThisIsAPlaintextSecretDoNotStore") {
		t.Fatalf("WS-ingested blob leaked the plaintext secret: %s", report.ReportJSON)
	}
	if !srv.RuntimeStatus.IsFileMode("mock-host-qwen") {
		t.Fatal("source:file runtime_report over WS must flip IsFileMode")
	}
}

// TestAgentStreamRuntimeReportSkipsInvalidFrameKeepsConnection mirrors
// TestAgentStreamSystemReportSkipsInvalidFrameKeepsConnection: an invalid
// runtime_report payload must be SKIPPED (continue), not close the
// connection.
func TestAgentStreamRuntimeReportSkipsInvalidFrameKeepsConnection(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws_runtimereport_bad", "mock-host-qwen", "agent-secret")
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

	// A syntactically malformed frame can't even be sent as a json.RawMessage
	// WS frame (the client-side marshal itself rejects it before anything
	// reaches the server -- see the equivalent note on the system_report
	// test), so this uses a syntactically valid-but-schema-mismatched payload
	// json.Unmarshal rejects with a type error instead.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "runtime_report", Data: json.RawMessage(`{"collected_at":"nope"}`)}); err != nil {
		t.Fatalf("write bad runtime_report: %v", err)
	}
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":13}}`)}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 13 {
			t.Fatalf("cpu = %v, want 13 (bad runtime_report frame must have been skipped)", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection dropped after an invalid runtime_report frame")
	}
}

// newAgentRuntimeReportRequest mirrors newAgentSystemReportRequest
// (system_report_gate_test.go) for the runtime-report POST route.
func newAgentRuntimeReportRequest(secret, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/runtime-report", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req
}

// TestAgentRuntimeReportGateOnRejectsPublicServesAgent mirrors
// TestAgentSystemReportGateOnRejectsPublicServesAgent for the runtime-report
// POST route: the /api/agent/v1/runtime-report registration in
// setupAgentRoutes's agentRoutes table must carry the SAME netbird_only gate
// as every other agent endpoint, applied BEFORE handleAgentRuntimeReport's
// own auth.
func TestAgentRuntimeReportGateOnRejectsPublicServesAgent(t *testing.T) {
	srv := newAgentGateTestServer(t, true, "")
	srv.SetAgentListener(true, "")

	mainRec := httptest.NewRecorder()
	srv.ServeHTTP(mainRec, newAgentRuntimeReportRequest("agent-secret", validRuntimeReportBody))
	assertAgentTelemetryError(t, mainRec, http.StatusForbidden, "netbird.only")

	agentOKRec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(agentOKRec, newAgentRuntimeReportRequest("agent-secret", validRuntimeReportBody))
	if agentOKRec.Code != http.StatusOK {
		t.Fatalf("agent listener (netbird_only on) = %d, want 200; body=%s", agentOKRec.Code, agentOKRec.Body.String())
	}
}
