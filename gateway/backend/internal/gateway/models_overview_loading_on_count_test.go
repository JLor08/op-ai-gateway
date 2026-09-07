// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"sync/atomic"
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

// --- Fix round 1: efficiency (the Task-5 review's Important finding) -------
//
// The original injectLoadingOnCounts called Portal.ModelServers once per
// DISTINCT MODEL ROW, and ModelServers re-walks the entire server/app/mapping
// join from scratch every time -- an N+1 against SQLite that multiplied the
// base listing's query cost by the model count. The fix inverts the loop:
// startingServersByModel walks the runtime-status registry ONCE per request
// (bounded by the number of servers with a published snapshot) instead of
// once per model row. countingPortalSpy/countingRouteSpy below pin that
// property directly: they count calls to the exact methods the N+1 used
// (Portal.ModelServers) and the exact methods the replacement uses
// (Portal.AllowedServerIDs, Routes.RuntimeSpecByID, Routes.MappingByID), so a
// regression back to a per-row store walk fails the test even though the
// counts it PRODUCES would still be correct.

// countingPortalSpy wraps a real portal.API and counts calls to the two
// methods relevant to the loading_on_count injection: ModelServers (the OLD,
// now-forbidden per-row path) and AllowedServerIDs (the NEW, once-per-request
// visibility check). Every other method is the embedded real implementation.
type countingPortalSpy struct {
	portal.API
	modelServersCalls     int32
	allowedServerIDsCalls int32
}

func (p *countingPortalSpy) ModelServers(ctx context.Context, token auth.Token, gatewayModelName string) ([]portal.ModelServerDTO, error) {
	atomic.AddInt32(&p.modelServersCalls, 1)
	return p.API.ModelServers(ctx, token, gatewayModelName)
}

func (p *countingPortalSpy) AllowedServerIDs(ctx context.Context, token auth.Token, serverIDs []string) (map[string]bool, error) {
	atomic.AddInt32(&p.allowedServerIDsCalls, 1)
	return p.API.AllowedServerIDs(ctx, token, serverIDs)
}

// countingRouteSpy wraps a real routing.Store and counts calls to the two
// point-read methods startingServersByModel uses to resolve a starting spec
// to its gateway model name: RuntimeSpecByID and MappingByID. Every other
// method is the embedded real store.
type countingRouteSpy struct {
	routing.Store
	runtimeSpecByIDCalls int32
	mappingByIDCalls     int32
}

func (r *countingRouteSpy) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	atomic.AddInt32(&r.runtimeSpecByIDCalls, 1)
	return r.Store.RuntimeSpecByID(ctx, id)
}

func (r *countingRouteSpy) MappingByID(ctx context.Context, id string) (routing.ModelMapping, error) {
	atomic.AddInt32(&r.mappingByIDCalls, 1)
	return r.Store.MappingByID(ctx, id)
}

// TestStartingServersByModelDoesNotScaleWithModelCount seeds manyModelsN
// (>5) DISTINCT gateway models, each offered by its own server, with exactly
// ONE server's ONE spec published as "starting". It asserts the row counts
// are correct (only the one backing model gets loading_on_count == 1) AND
// -- the actual regression pin -- that the cost of computing them does not
// grow with the model count:
//   - Portal.ModelServers is called ZERO times (the old N+1 path must never
//     run again).
//   - Portal.AllowedServerIDs is called EXACTLY ONCE for the whole request
//     (one call covering every distinct candidate server), not once per model.
//   - Routes.RuntimeSpecByID / Routes.MappingByID are each called EXACTLY
//     ONCE -- bounded by the number of STARTING specs (1), never by the
//     number of models in rows (manyModelsN).
//
// If a future change reverts to a per-row Portal.ModelServers call, or adds a
// per-row AllowedServerIDs/RuntimeSpecByID/MappingByID call, this test fails
// on the call counts even though the produced counts would still happen to
// be correct.
func TestStartingServersByModelDoesNotScaleWithModelCount(t *testing.T) {
	const manyModelsN = 6
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()

	var startingModel string
	var startingSpecID = "rspec_many_0"
	for i := 0; i < manyModelsN; i++ {
		serverID := fmt.Sprintf("srv_many_%d", i)
		appID := fmt.Sprintf("app_many_%d", i)
		mappingID := fmt.Sprintf("map_many_%d", i)
		model := fmt.Sprintf("many-model-%d", i)
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", serverID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: model, AppModelName: model, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if i == 0 {
			startingModel = model
			if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: startingSpecID, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("UpsertRuntimeSpec: %v", err)
			}
		}
	}

	routeSpy := &countingRouteSpy{Store: routeStore}
	svc := portal.NewService(portal.ServiceDeps{Routes: routeStore})
	portalSpy := &countingPortalSpy{API: svc}

	rtStatus := NewRuntimeStatusRegistry()
	rtStatus.publish("srv_many_0", []RuntimeStatusDTO{{SpecID: startingSpecID, State: "starting"}})

	s := &Server{Routes: routeSpy, Portal: portalSpy, RuntimeStatus: rtStatus}

	rows := make([]portal.ModelDTO, manyModelsN)
	for i := 0; i < manyModelsN; i++ {
		rows[i] = portal.ModelDTO{ID: fmt.Sprintf("many-model-%d", i)}
	}
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: "usr_many"}, rows)

	for i, row := range rows {
		want := 0
		if row.ID == startingModel {
			want = 1
		}
		if row.LoadingOnCount != want {
			t.Fatalf("row[%d] (%s) LoadingOnCount = %d, want %d", i, row.ID, row.LoadingOnCount, want)
		}
	}

	if got := atomic.LoadInt32(&portalSpy.modelServersCalls); got != 0 {
		t.Fatalf("Portal.ModelServers called %d times across %d models, want 0 (the old per-row N+1 path must not run)", got, manyModelsN)
	}
	if got := atomic.LoadInt32(&portalSpy.allowedServerIDsCalls); got != 1 {
		t.Fatalf("Portal.AllowedServerIDs called %d times, want exactly 1 for the whole request regardless of the %d models in the response", got, manyModelsN)
	}
	if got := atomic.LoadInt32(&routeSpy.runtimeSpecByIDCalls); got != 1 {
		t.Fatalf("Routes.RuntimeSpecByID called %d times, want exactly 1 (bounded by the 1 starting spec, not the %d models)", got, manyModelsN)
	}
	if got := atomic.LoadInt32(&routeSpy.mappingByIDCalls); got != 1 {
		t.Fatalf("Routes.MappingByID called %d times, want exactly 1 (bounded by the 1 starting spec, not the %d models)", got, manyModelsN)
	}
}

// TestStartingServersByModelRespectsVisibility proves the visibility
// property the review flagged as a must-not-regress: the old
// implementation inherited its server-visibility filtering for free by
// routing every count through Portal.ModelServers (which itself calls
// AllowedServerIDs). The inverted, registry-driven pass bypasses ModelServers
// entirely, so it MUST re-apply AllowedServerIDs itself -- otherwise a
// server the principal cannot see would still contribute to the count,
// leaking its existence (its "starting" state) to a caller who cannot
// otherwise learn it exists.
//
// Fixture: two servers offer the SAME gateway model and are BOTH published
// as "starting". Server A is unrestricted. Server C is a member of a
// resource group provisioned ONLY for a different user (usr_other_vis), so
// the test principal (usr_test_vis) is not allowed to use it (Resource
// Groups Phase 2, opt-in mode -- the default). If the visibility filter were
// missing or wrong, loading_on_count would be 2; with it, it must be 1
// (server A only).
func TestStartingServersByModelRespectsVisibility(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()

	const (
		visModel  = "vis-model"
		serverA   = "srv_vis_a"
		serverC   = "srv_vis_c_restricted"
		appA      = "app_vis_a"
		appC      = "app_vis_c"
		mappingA  = "map_vis_a"
		mappingC  = "map_vis_c"
		specA     = "rspec_vis_a"
		specC     = "rspec_vis_c"
		rgID      = "rgrp_vis_lc"
		testUser  = "usr_test_vis"
		otherUser = "usr_other_vis"
	)
	seed := func(serverID, appID, mappingID, specID string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Provider: routing.ProviderMock, Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", serverID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: visModel, AppModelName: visModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: specID, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", specID, err)
		}
	}
	seed(serverA, appA, mappingA, specA)
	seed(serverC, appC, mappingC, specC)

	// Restrict server C to a resource group provisioned ONLY for otherUser.
	if err := routeStore.CreateResourceGroup(ctx, routing.ResourceGroup{ID: rgID, Name: "RG restricted", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := routeStore.SetResourceGroupServer(ctx, rgID, serverC); err != nil {
		t.Fatalf("SetResourceGroupServer: %v", err)
	}
	if err := routeStore.SetResourceGroupProvision(ctx, rgID, routing.ProvisionKindUser, otherUser); err != nil {
		t.Fatalf("SetResourceGroupProvision: %v", err)
	}

	svc := portal.NewService(portal.ServiceDeps{Routes: routeStore})
	rtStatus := NewRuntimeStatusRegistry()
	rtStatus.publish(serverA, []RuntimeStatusDTO{{SpecID: specA, State: "starting"}})
	rtStatus.publish(serverC, []RuntimeStatusDTO{{SpecID: specC, State: "starting"}})

	s := &Server{Routes: routeStore, Portal: svc, RuntimeStatus: rtStatus}
	rows := []portal.ModelDTO{{ID: visModel}}
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: testUser}, rows)

	if rows[0].LoadingOnCount != 1 {
		t.Fatalf("loading_on_count = %d, want 1 (server A only -- server C is starting too but restricted to usr_other_vis, not usr_test_vis)", rows[0].LoadingOnCount)
	}

	// Sanity: the SAME principal, if provisioned as otherUser, sees both.
	rows2 := []portal.ModelDTO{{ID: visModel}}
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: otherUser}, rows2)
	if rows2[0].LoadingOnCount != 2 {
		t.Fatalf("loading_on_count for the provisioned user = %d, want 2 (both A and C visible to usr_other_vis)", rows2[0].LoadingOnCount)
	}
}
