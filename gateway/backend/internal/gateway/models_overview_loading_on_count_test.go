// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

const (
	mlOwnerSecret = "ml-owner-secret"
	mlModel       = "shared-model" // offered by BOTH servers below
	mlAppModel    = "up-ml"
	mlServerA     = "srv_ml_a"
	mlServerB     = "srv_ml_b"
	mlAppA        = "app_ml_a"
	mlAppB        = "app_ml_b"
	mlMappingA    = "map_ml_a"
	mlMappingB    = "map_ml_b"
)

// newLoadingOnCountFixture seeds TWO active servers/applications, each with an
// active mapping offering the SAME gateway model (mlModel) under a distinct
// upstream name (mlAppModel), so a test can publish a "starting" RuntimeStatus
// against ONE server's managed spec and assert the models-overview DTO's
// loading_on_count distinguishes it from the other, still-idle server. The
// token carries both gateway:use and admin so the same fixture also drives the
// ?manage=1 (ManageModels) branch of handlePortalModels.
func newLoadingOnCountFixture(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_ml", Email: "ml@example.test", DisplayName: "ML User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_ml", UserID: "usr_ml", Name: "ML Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, mlOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	seedServerAppMapping := func(serverID, appID, mappingID string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", serverID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: mlModel, AppModelName: mlAppModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
	}
	seedServerAppMapping(mlServerA, mlAppA, mlMappingA)
	seedServerAppMapping(mlServerB, mlAppB, mlMappingB)

	reg := NewLoadedModelRegistry()
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, LoadedModels: reg})
	return New(ServerDeps{
		Tokens:       tokens,
		Usage:        recorder,
		Routes:       routeStore,
		Portal:       svc,
		LoadedModels: reg,
	})
}

// fetchModelsOverview issues path (either "/api/portal/models" or
// "/api/portal/models?manage=1") and returns the response's ModelDTO rows
// indexed by ID.
func fetchModelsOverview(t *testing.T, s *Server, path string) map[string]portal.ModelDTO {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+mlOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.ModelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	byID := map[string]portal.ModelDTO{}
	for _, m := range out.Data {
		byID[m.ID] = m
	}
	return byID
}

// TestPortalModelsInjectsLoadingOnCount: with two servers offering the same
// gateway model, publishing a "starting" RuntimeStatus for ONE server's
// managed spec makes that model's overview DTO report loading_on_count == 1
// -- proving the gateway layer injects it post-hoc (Service.Models has no
// access to the runtime-status registry), mirroring injectRuntimeModelState's
// Priority/State injection for ModelServerDTO but at the overview's
// per-model-aggregate grain. No runtime_model_probe capability is declared by
// either server here -- State (the loading signal) is valid for any
// runtime_manager agent and must inject regardless, exactly like
// injectRowRuntimeState's unconditional State field.
func TestPortalModelsInjectsLoadingOnCount(t *testing.T) {
	s := newLoadingOnCountFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const specID = "rspec_ml_a"
	if err := s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: specID, MappingID: mlMappingA, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	s.RuntimeStatus.publish(mlServerA, []RuntimeStatusDTO{
		{SpecID: specID, Model: mlAppModel, State: "starting"},
	})

	byID := fetchModelsOverview(t, s, "/api/portal/models")
	dto, ok := byID[mlModel]
	if !ok {
		t.Fatalf("model %q missing from overview: %#v", mlModel, byID)
	}
	if dto.OfferedOnCount != 2 {
		t.Fatalf("offered_on_count = %d, want 2 (sanity: both servers offer it)", dto.OfferedOnCount)
	}
	if dto.LoadingOnCount != 1 {
		t.Fatalf("loading_on_count = %d, want 1 (only server A is starting)", dto.LoadingOnCount)
	}
}

// TestPortalModelsLoadingOnCountZeroWithNoStartingServer: with no runtime spec
// or published RuntimeStatus for either offering server, loading_on_count is
// 0 -- best-effort, never an error, when nothing is known to be starting.
func TestPortalModelsLoadingOnCountZeroWithNoStartingServer(t *testing.T) {
	s := newLoadingOnCountFixture(t)
	// Deliberately: no UpsertRuntimeSpec, no RuntimeStatus.publish for either server.

	byID := fetchModelsOverview(t, s, "/api/portal/models")
	dto, ok := byID[mlModel]
	if !ok {
		t.Fatalf("model %q missing from overview: %#v", mlModel, byID)
	}
	if dto.LoadingOnCount != 0 {
		t.Fatalf("loading_on_count = %d, want 0 (no starting server)", dto.LoadingOnCount)
	}
}

// TestPortalModelsManageAlsoInjectsLoadingOnCount: the admin ?manage=1 branch
// (ManageModels(), the unsuppressed listing) goes through the SAME
// injection as the plain GET -- both branches inside handlePortalModels must
// call the gateway's loading_on_count injection, not just one of them.
func TestPortalModelsManageAlsoInjectsLoadingOnCount(t *testing.T) {
	s := newLoadingOnCountFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const specID = "rspec_ml_manage_a"
	if err := s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: specID, MappingID: mlMappingA, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	s.RuntimeStatus.publish(mlServerA, []RuntimeStatusDTO{
		{SpecID: specID, Model: mlAppModel, State: "starting"},
	})

	byID := fetchModelsOverview(t, s, "/api/portal/models?manage=1")
	dto, ok := byID[mlModel]
	if !ok {
		t.Fatalf("model %q missing from manage overview: %#v", mlModel, byID)
	}
	if dto.LoadingOnCount != 1 {
		t.Fatalf("manage loading_on_count = %d, want 1", dto.LoadingOnCount)
	}
}

// TestInjectLoadingOnCountsNilSafeOnBareServer: a bare &Server{} (no Routes, no
// RuntimeStatus, no Portal, as constructed directly rather than via New in
// several sibling tests across this package) must leave every row's
// LoadingOnCount at its zero value rather than panic -- the registry-absent
// best-effort path the CONTROLLER ruling calls out explicitly.
func TestInjectLoadingOnCountsNilSafeOnBareServer(t *testing.T) {
	s := &Server{}
	rows := []portal.ModelDTO{{ID: "some-model", OfferedOnCount: 2}}
	s.injectLoadingOnCounts(context.Background(), auth.Token{UserID: "usr_x"}, rows)
	if rows[0].LoadingOnCount != 0 {
		t.Fatalf("LoadingOnCount = %d, want 0 on a bare *Server with no registry", rows[0].LoadingOnCount)
	}
}
