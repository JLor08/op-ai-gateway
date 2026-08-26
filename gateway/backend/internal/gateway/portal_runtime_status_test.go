// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

const (
	runtimeEventsOwnerSecret = "runtime-owner-secret"
	runtimeEventsOtherSecret = "runtime-other-secret"
	runtimeEventsServerID    = "srv_runtime_events"
)

// newRuntimeEventsTestServer mirrors newPerfTestServer (perf_endpoints_test.go):
// a *Server with a memory route store holding one server (srv_runtime_events)
// owned by usr_owner, plain bearer tokens for the owner and a non-owner, and
// the default (nil-default-constructed) RuntimeStatus registry New() always
// wires up.
func newRuntimeEventsTestServer(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_owner", Email: "owner@example.test", DisplayName: "Owner", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_owner", UserID: "usr_owner", Name: "Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, runtimeEventsOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other", UserID: "usr_other", Name: "Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, runtimeEventsOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{
		ID: runtimeEventsServerID, Name: "Runtime Events", Domain: "runtime-events.example.test", Provider: routing.ProviderMock,
		Endpoint: "mock://runtime-events", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), runtimeEventsServerID, []string{"usr_owner"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }})
	return New(ServerDeps{Tokens: tokens, Usage: recorder, Routes: routeStore, Portal: svc})
}

// --- Task 9: GET /api/portal/servers/{id}/runtime/report --------------------

func TestHandleRuntimeReportViewAbsent(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/report", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.ServerRuntimeReportViewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if dto.Available {
		t.Fatalf("available = true, want false: %#v", dto)
	}
	if dto.AgentVersion != "" || len(dto.AgentFeatures) != 0 {
		t.Fatalf("expected empty agent_version/agent_features with no telemetry: %#v", dto)
	}
	if dto.AgentFeatures == nil {
		t.Fatal("agent_features must be a non-nil empty slice, not null")
	}
}

func TestHandleRuntimeReportViewFound(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	now := time.Now().UTC()
	if err := srv.Routes.UpsertTelemetry(context.Background(), routing.ServerTelemetry{
		ServerID: runtimeEventsServerID, ReportedAt: now, AgentVersion: "1.4.0",
		Capabilities:   `{"features":["runtime_manager","proxy_status"]}`,
		ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	// Goes through the real ingest (not a raw store seed) so the redaction
	// guarantee is exercised end-to-end for the view too: the plaintext
	// secret in validRuntimeReportBody must never reach this response.
	if err := srv.ingestRuntimeReport(context.Background(), runtimeEventsServerID, json.RawMessage(validRuntimeReportBody)); err != nil {
		t.Fatalf("ingestRuntimeReport: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/report", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.ServerRuntimeReportViewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !dto.Available || dto.CollectedAt == "" || dto.UpdatedAt == "" {
		t.Fatalf("dto = %#v, want available with timestamps", dto)
	}
	if dto.AgentVersion != "1.4.0" {
		t.Fatalf("agent_version = %q, want 1.4.0", dto.AgentVersion)
	}
	if len(dto.AgentFeatures) != 2 || dto.AgentFeatures[0] != "runtime_manager" {
		t.Fatalf("agent_features = %#v", dto.AgentFeatures)
	}
	if strings.Contains(string(dto.Report), "hf_ThisIsAPlaintextSecretDoNotStore") {
		t.Fatalf("report view leaked the plaintext secret: %s", dto.Report)
	}
	if !strings.Contains(string(dto.Report), runtimeReportEnvMask) {
		t.Fatalf("report view missing the redaction mask: %s", dto.Report)
	}
}

func TestHandleRuntimeReportViewForeignUserReturns404(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/report", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found (no existence leak)", code)
	}
}

// --- Task 9: GET /api/portal/servers/{id}/runtime/events (SSE) --------------

func TestRuntimeEventsSnapshotAndUpdate(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	// Pre-seed the snapshot so the first frame carries it.
	srv.RuntimeStatus.publish(runtimeEventsServerID, []RuntimeStatusDTO{{SpecID: "spec-1", State: "running"}})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+runtimeEventsServerID+"/runtime/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap runtimeStatusEventDTO
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if len(snap.Runtimes) != 1 || snap.Runtimes[0].SpecID != "spec-1" || snap.Runtimes[0].State != "running" {
		t.Fatalf("snapshot = %#v, want one entry spec-1/running", snap.Runtimes)
	}

	// A live publish arrives as an `update` frame, carrying the FULL replaced
	// list (two entries now, not a delta appended to the first).
	srv.RuntimeStatus.publish(runtimeEventsServerID, []RuntimeStatusDTO{
		{SpecID: "spec-1", State: "running"},
		{SpecID: "spec-2", State: "starting"},
	})
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "update" {
		t.Fatalf("delta event = %q, want update", event)
	}
	var upd runtimeStatusEventDTO
	if err := json.Unmarshal([]byte(data), &upd); err != nil {
		t.Fatalf("unmarshal update: %v (%s)", err, data)
	}
	if len(upd.Runtimes) != 2 || upd.Runtimes[1].SpecID != "spec-2" {
		t.Fatalf("update = %#v, want two entries incl. spec-2", upd.Runtimes)
	}
}

func TestRuntimeEventsForbidden(t *testing.T) {
	srv := newRuntimeEventsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+runtimeEventsServerID+"/runtime/events", nil)
	req.Header.Set("Authorization", "Bearer "+runtimeEventsOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found (no existence leak)", code)
	}
}
