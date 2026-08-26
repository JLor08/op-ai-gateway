// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// seedRuntimeSpecMapping seeds a server owned by usr_dev (NewTestServer's
// default principal), a server_agent-typed application on it, and a mapping
// on that application, returning the mapping id the runtime-spec endpoints
// are addressed by.
func seedRuntimeSpecMapping(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	const serverID = "srv_runtime_spec"
	const appID = "app_runtime_spec"
	const mappingID = "map_runtime_spec"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.SetServerOwners(ctx, serverID, []string{"usr_dev"}); err != nil {
		t.Fatalf("set owners: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: "runtime-model", AppModelName: "runtime-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	return mappingID
}

func TestHandlePortalMappingRuntimeSpecGetUnconfigured(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/mappings/"+mappingID+"/runtime-spec", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.RuntimeSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if dto.Configured {
		t.Fatalf("configured = true, want false, body = %s", rec.Body.String())
	}
	if dto.MappingID != mappingID {
		t.Fatalf("mapping_id = %q, want %q", dto.MappingID, mappingID)
	}
	if dto.GPUs == nil {
		t.Fatalf("gpus = nil, want non-nil empty")
	}
}

func TestHandlePortalMappingRuntimeSpecPutValid(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/local/bin/llama-server","args":["--model","/models/q.gguf"],"gpus":[{"index":0,"vram_estimate_mb":8000}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.RuntimeSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !dto.Configured || dto.Binary != "/usr/local/bin/llama-server" {
		t.Fatalf("dto = %#v", dto)
	}
	if dto.HealthPath != "/health" || dto.StartupTimeoutSeconds != 180 {
		t.Fatalf("defaults not applied: dto = %#v", dto)
	}
	if len(dto.GPUs) != 1 || dto.GPUs[0].VRAMEstimateMB != 8000 {
		t.Fatalf("gpus = %#v", dto.GPUs)
	}
}

func TestHandlePortalMappingRuntimeSpecPutBadAdminStateReturns400(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	body := `{"binary":"/usr/local/bin/llama-server","admin_state":"bogus"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_spec.admin_state_invalid" {
		t.Fatalf("error code = %q, want runtime_spec.admin_state_invalid", code)
	}
}

func TestHandlePortalMappingRuntimeSpecDeleteReturnsOKTrue(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", `{"binary":"/usr/local/bin/llama-server"}`))
	if putRec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d, body = %s", putRec.Code, putRec.Body.String())
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/portal/mappings/"+mappingID+"/runtime-spec", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !body["ok"] {
		t.Fatalf("body = %#v, want {ok:true}", body)
	}

	// Deleting again with nothing left to delete surfaces the domain
	// not-found sentinel, not a leaked 500 / different shape.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, newJSONRequest(http.MethodDelete, "/api/portal/mappings/"+mappingID+"/runtime-spec", ""))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want 404, body = %s", rec2.Code, rec2.Body.String())
	}
	if code := errorBodyOf(t, rec2); code != "runtime_spec.not_found" {
		t.Fatalf("second DELETE error code = %q, want runtime_spec.not_found", code)
	}
}

func TestHandlePortalMappingRuntimeSpecMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/mappings/"+mappingID+"/runtime-spec", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, PUT, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalMappingRuntimeSpecUnknownMappingReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/mappings/does-not-exist/runtime-spec", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "mapping.not_found" {
		t.Fatalf("error code = %q, want mapping.not_found", code)
	}
}

func TestHandlePortalMappingRuntimeSpecPutInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	mappingID := seedRuntimeSpecMapping(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+mappingID+"/runtime-spec", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}
