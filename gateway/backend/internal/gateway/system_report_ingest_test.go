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
)

// A minimal, well-formed system-report payload (the WS/POST `data`).
const validSystemReportBody = `{"collected_at":"2026-08-04T09:00:00Z","agent_version":"1.2.3","os":"linux","arch":"amd64","cpu":{"model":"Test CPU","vendor":"GenuineIntel","physical_cores":8,"logical_threads":16,"base_mhz":3200},"memory":{"total_bytes":34359738368},"mainboard":{"vendor":"ACME","product":"X1","version":"1.0"},"bios":{"vendor":"ACME","version":"2.0"},"gpus":[{"index":0,"name":"E2E-GPU","uuid":"GPU-abc","driver_version":"550.1","memory_total_bytes":16000000000}]}`

func TestIngestSystemReport(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path stores canonical JSON", func(t *testing.T) {
		srv := NewTestServer()
		if err := srv.ingestSystemReport(ctx, "mock-host-qwen", json.RawMessage(validSystemReportBody)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		hw, ok, err := srv.Routes.ServerHardwareByServer(ctx, "mock-host-qwen")
		if err != nil || !ok {
			t.Fatalf("ServerHardwareByServer ok=%v err=%v", ok, err)
		}
		if !strings.Contains(hw.ReportJSON, `"agent_version":"1.2.3"`) {
			t.Fatalf("report json = %s", hw.ReportJSON)
		}
		// The canonical blob must re-parse as an agentSystemReport.
		var got agentSystemReport
		if err := json.Unmarshal([]byte(hw.ReportJSON), &got); err != nil {
			t.Fatalf("canonical json not parseable: %v", err)
		}
		if got.CPU.Model != "Test CPU" || len(got.GPUs) != 1 || got.GPUs[0].DriverVersion != "550.1" {
			t.Fatalf("decoded = %#v", got)
		}
	})

	t.Run("invalid json -> errAgentSystemReportInvalid", func(t *testing.T) {
		srv := NewTestServer()
		err := srv.ingestSystemReport(ctx, "mock-host-qwen", json.RawMessage(`{bad`))
		if !errors.Is(err, errAgentSystemReportInvalid) {
			t.Fatalf("err = %v, want errAgentSystemReportInvalid", err)
		}
	})

	t.Run("unknown server -> errAgentUnknownServer", func(t *testing.T) {
		srv := NewTestServer()
		err := srv.ingestSystemReport(ctx, "ghost-host", json.RawMessage(validSystemReportBody))
		if !errors.Is(err, errAgentUnknownServer) {
			t.Fatalf("err = %v, want errAgentUnknownServer", err)
		}
	})

	t.Run("sanitize clamps negatives", func(t *testing.T) {
		srv := NewTestServer()
		raw := json.RawMessage(`{"agent_version":"x","cpu":{"physical_cores":-4},"memory":{"total_bytes":-9},"gpus":[{"index":0,"name":"G","memory_total_bytes":-1}]}`)
		if err := srv.ingestSystemReport(ctx, "mock-host-qwen", raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		hw, _, _ := srv.Routes.ServerHardwareByServer(ctx, "mock-host-qwen")
		var got agentSystemReport
		if err := json.Unmarshal([]byte(hw.ReportJSON), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.CPU.PhysicalCores != 0 || got.Memory.TotalBytes != 0 || got.GPUs[0].MemoryTotalBytes != 0 {
			t.Fatalf("negatives not clamped: %#v", got)
		}
	})
}

// TestHandleAgentSystemReportPOST proves the POST endpoint is registered on the
// public mux, authenticates via the agent bearer token, and calls the SAME
// ingestSystemReport (parity with the WS transport).
func TestHandleAgentSystemReportPOST(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/system-report", strings.NewReader(validSystemReportBody))
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
	if _, ok, _ := srv.Routes.ServerHardwareByServer(context.Background(), "mock-host-qwen"); !ok {
		t.Fatal("hardware not stored")
	}
}

func TestHandleAgentSystemReportPOSTRejectsBadToken(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/system-report", strings.NewReader(validSystemReportBody))
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestIngestSystemReportDropsInjectedSerialFields is the privacy guarantee (D4)
// proof: even if a future/malicious agent injects serial/UUID/MAC-shaped fields
// that the agentSystemReport schema does not model, the canonical re-marshal
// (a typed struct with no such fields) drops them entirely -- the stored blob
// can never contain them. The one exception is the GPU UUID, which the schema
// DOES carry (allowed per the design) and must survive untouched.
func TestIngestSystemReportDropsInjectedSerialFields(t *testing.T) {
	srv := NewTestServer()
	raw := json.RawMessage(`{"agent_version":"1.0","serial":"SN-12345","board_serial":"BS-9999","chassis_uuid":"11111111-2222-3333-4444-555555555555","mac_address":"AA:BB:CC:DD:EE:FF","cpu":{"model":"X"},"mainboard":{"vendor":"ACME","serial_number":"MB-SERIAL-1"},"gpus":[{"index":0,"name":"G","uuid":"GPU-uuid-ok","serial":"GPU-SERIAL-1"}]}`)
	if err := srv.ingestSystemReport(context.Background(), "mock-host-qwen", raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	hw, ok, err := srv.Routes.ServerHardwareByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	for _, leaked := range []string{
		"SN-12345", "BS-9999", "11111111-2222-3333-4444-555555555555",
		"AA:BB:CC:DD:EE:FF", "MB-SERIAL-1", "GPU-SERIAL-1",
		"serial", "chassis_uuid", "mac_address", "board_serial",
	} {
		if strings.Contains(hw.ReportJSON, leaked) {
			t.Fatalf("canonical blob leaked %q: %s", leaked, hw.ReportJSON)
		}
	}
	if !strings.Contains(hw.ReportJSON, "GPU-uuid-ok") {
		t.Fatalf("expected the allowed GPU uuid to survive: %s", hw.ReportJSON)
	}
}
