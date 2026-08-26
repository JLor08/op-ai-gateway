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

// --- Task 6: co-residency matrix + runtime warnings -------------------------

// seedCoResidencyApp seeds a server owned by usr_dev (NewTestServer's default
// principal), a server_agent application on it, and TWO mappings on that
// application, returning the application id and both mapping ids -- the
// fixture the co-residency and runtime-warnings endpoints need.
func seedCoResidencyApp(t *testing.T, srv *Server) (appID, mapping1ID, mapping2ID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	const serverID = "srv_coresidency"
	const localAppID = "app_coresidency"
	const m1ID = "map_coresidency_1"
	const m2ID = "map_coresidency_2"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.SetServerOwners(ctx, serverID, []string{"usr_dev"}); err != nil {
		t.Fatalf("set owners: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{ID: localAppID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, TimeoutMS: 30000, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: m1ID, ApplicationID: localAppID, GatewayModelName: "m1", AppModelName: "m1", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping1: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: m2ID, ApplicationID: localAppID, GatewayModelName: "m2", AppModelName: "m2", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping2: %v", err)
	}
	return localAppID, m1ID, m2ID
}

func TestHandlePortalApplicationCoResidencyGetEmpty(t *testing.T) {
	srv := NewTestServer()
	appID, _, _ := seedCoResidencyApp(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/runtime/coresidency", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.CoResidencyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if dto.Pairs == nil || len(dto.Pairs) != 0 {
		t.Fatalf("pairs = %#v, want non-nil empty", dto.Pairs)
	}
}

func TestHandlePortalApplicationCoResidencyPutCanonicalizesAndRoundTrips(t *testing.T) {
	srv := NewTestServer()
	appID, m1, m2 := seedCoResidencyApp(t, srv)
	a, b := m1, m2
	if a > b {
		a, b = b, a
	}
	// Submit the pair reversed -- the endpoint must canonicalize server-side.
	body := `{"pairs":[["` + b + `","` + a + `"]]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/applications/"+appID+"/runtime/coresidency", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto portal.CoResidencyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if len(dto.Pairs) != 1 || dto.Pairs[0][0] != a || dto.Pairs[0][1] != b {
		t.Fatalf("pairs = %#v, want canonical [[%q,%q]]", dto.Pairs, a, b)
	}

	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/runtime/coresidency", ""))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body = %s", getRec.Code, getRec.Body.String())
	}
	var got portal.CoResidencyDTO
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, getRec.Body.String())
	}
	if len(got.Pairs) != 1 || got.Pairs[0][0] != a || got.Pairs[0][1] != b {
		t.Fatalf("GET pairs = %#v, want canonical [[%q,%q]]", got.Pairs, a, b)
	}
}

func TestHandlePortalApplicationCoResidencyPutDuplicateAfterNormalizationReturns400(t *testing.T) {
	srv := NewTestServer()
	appID, m1, m2 := seedCoResidencyApp(t, srv)
	body := `{"pairs":[["` + m1 + `","` + m2 + `"],["` + m2 + `","` + m1 + `"]]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/applications/"+appID+"/runtime/coresidency", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "runtime_coresidency.pair_invalid" {
		t.Fatalf("error code = %q, want runtime_coresidency.pair_invalid", code)
	}
}

func TestHandlePortalApplicationCoResidencyMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	appID, _, _ := seedCoResidencyApp(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/runtime/coresidency", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, PUT")
	}
}

func TestHandlePortalApplicationCoResidencyUnknownApplicationReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/does-not-exist/runtime/coresidency", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "application.not_found" {
		t.Fatalf("error code = %q, want application.not_found", code)
	}
}

func TestHandlePortalApplicationRuntimeUnknownSubpathReturns404(t *testing.T) {
	srv := NewTestServer()
	appID, _, _ := seedCoResidencyApp(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/runtime/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "application.not_found" {
		t.Fatalf("error code = %q, want application.not_found", code)
	}
}

func TestHandlePortalApplicationWarningsGet(t *testing.T) {
	srv := NewTestServer()
	appID, m1, _ := seedCoResidencyApp(t, srv)

	// No runtime spec yet -- no warning.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/runtime/warnings", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if empty.Warnings == nil || len(empty.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want non-nil empty", empty.Warnings)
	}

	// Enable a runtime spec on m1 whose startup timeout (60s) exceeds the
	// application's TimeoutMS (30000ms, seeded above) -- must now warn.
	specBody := `{"binary":"/usr/local/bin/llama-server","enabled":true,"startup_timeout_seconds":60}`
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, newJSONRequest(http.MethodPut, "/api/portal/mappings/"+m1+"/runtime-spec", specBody))
	if putRec.Code != http.StatusOK {
		t.Fatalf("seed runtime spec status = %d, body = %s", putRec.Code, putRec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/runtime/warnings", ""))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec2.Code, rec2.Body.String())
	}
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec2.Body.String())
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "timeout_ms_below_startup_timeout" {
		t.Fatalf("warnings = %#v, want [timeout_ms_below_startup_timeout]", got.Warnings)
	}
}

func TestHandlePortalApplicationWarningsMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	appID, _, _ := seedCoResidencyApp(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/applications/"+appID+"/runtime/warnings", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
}

// --- Task 6: per-GPU VRAM budgets -------------------------------------------

func seedGPUBudgetServer(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	const serverID = "srv_gpu_budgets"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.SetServerOwners(ctx, serverID, []string{"usr_dev"}); err != nil {
		t.Fatalf("set owners: %v", err)
	}
	return serverID
}

func TestHandlePortalServerGPUBudgetsGetEmpty(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/"+serverID+"/gpu-budgets", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Budgets []portal.GPUBudgetDTO `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if body.Budgets == nil || len(body.Budgets) != 0 {
		t.Fatalf("budgets = %#v, want non-nil empty", body.Budgets)
	}
}

func TestHandlePortalServerGPUBudgetsPutRoundTrip(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	body := `{"budgets":[{"index":0,"budget_mb":8000},{"index":1,"budget_mb":4000}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/"+serverID+"/gpu-budgets", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var put struct {
		Budgets []portal.GPUBudgetDTO `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if len(put.Budgets) != 2 || put.Budgets[0].BudgetMB != 8000 || put.Budgets[1].BudgetMB != 4000 {
		t.Fatalf("budgets = %#v", put.Budgets)
	}

	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, newJSONRequest(http.MethodGet, "/api/portal/servers/"+serverID+"/gpu-budgets", ""))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body = %s", getRec.Code, getRec.Body.String())
	}
	var got struct {
		Budgets []portal.GPUBudgetDTO `json:"budgets"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, getRec.Body.String())
	}
	if len(got.Budgets) != 2 || got.Budgets[0].BudgetMB != 8000 || got.Budgets[1].BudgetMB != 4000 {
		t.Fatalf("GET budgets = %#v", got.Budgets)
	}
}

func TestHandlePortalServerGPUBudgetsPutInvalidReturns400(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	body := `{"budgets":[{"index":-1,"budget_mb":100}]}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/"+serverID+"/gpu-budgets", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.gpu_budget_invalid" {
		t.Fatalf("error code = %q, want server.gpu_budget_invalid", code)
	}
}

func TestHandlePortalServerGPUBudgetsMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/gpu-budgets", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, PUT" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, PUT")
	}
}

func TestHandlePortalServerGPUBudgetsUnknownServerReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/does-not-exist/gpu-budgets", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// --- Task 6: server runtime flags + managed-runtime-only gate ---------------

func TestHandlePortalCreateApplicationManagedRuntimeOnlyReturns409(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	createBody := `{"name":"Managed","domain":"managed.example.test","owner_ids":["usr_dev"],"admin_group_ids":["` + testAdminGroupID + `"],"managed_runtime_only":true}`
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers", createBody))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create server status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID                  string `json:"id"`
		ManagedRuntimeOnly  bool   `json:"managed_runtime_only"`
		RuntimeMaxProcesses int    `json:"runtime_max_processes"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, createRec.Body.String())
	}
	if !created.ManagedRuntimeOnly {
		t.Fatalf("created = %#v, want managed_runtime_only=true", created)
	}

	appRec := httptest.NewRecorder()
	appBody := `{"type":"vllm","port":8000,"scheme":"https"}`
	srv.ServeHTTP(appRec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+created.ID+"/applications", appBody))
	if appRec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", appRec.Code, appRec.Body.String())
	}
	if code := errorBodyOf(t, appRec); code != "application.managed_runtime_only" {
		t.Fatalf("error code = %q, want application.managed_runtime_only", code)
	}
}

func TestHandlePortalServerRuntimeMaxProcessesNegativeReturns400(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	createBody := `{"name":"S","domain":"s.example.test","owner_ids":["usr_dev"],"admin_group_ids":["` + testAdminGroupID + `"],"runtime_max_processes":-1}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers", createBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.runtime_limit_invalid" {
		t.Fatalf("error code = %q, want server.runtime_limit_invalid", code)
	}
}
