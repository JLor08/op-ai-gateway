// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

const hardwareReportJSON = `{"agent_version":"9.9","os":"linux","arch":"amd64","cpu":{"model":"Test CPU","vendor":"GenuineIntel","physical_cores":8,"logical_threads":16,"base_mhz":3200},"memory":{"total_bytes":34359738368},"mainboard":{"vendor":"ACME","product":"X1","version":"1.0"},"bios":{"vendor":"ACME","version":"2.0"},"gpus":[{"index":0,"name":"E2E-GPU","memory_total_bytes":16000000000}]}`

func TestServerHardwareEndpoint(t *testing.T) {
	srv, routeStore := newPerfTestServer(t)
	now := time.Now().UTC()
	if err := routeStore.UpsertServerHardware(context.Background(), routing.ServerHardware{
		ServerID: perfServerID, CollectedAt: now, ReportJSON: hardwareReportJSON, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertServerHardware: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/hardware", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		Available   bool            `json:"available"`
		CollectedAt string          `json:"collected_at"`
		Report      json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !dto.Available || dto.CollectedAt == "" {
		t.Fatalf("dto = %#v", dto)
	}
	var report map[string]any
	if err := json.Unmarshal(dto.Report, &report); err != nil {
		t.Fatalf("report not structured JSON: %v", err)
	}
	if report["agent_version"] != "9.9" {
		t.Fatalf("report = %#v", report)
	}
}

func TestServerHardwareEndpointEmpty(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/hardware", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no report yet), body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dto.Available {
		t.Fatalf("available = true, want false when no report")
	}
}

func TestServerHardwareEndpointForbidden(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/hardware", nil)
	req.Header.Set("Authorization", "Bearer "+perfOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no leak), body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}
