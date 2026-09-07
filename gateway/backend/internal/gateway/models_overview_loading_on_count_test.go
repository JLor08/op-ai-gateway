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
	// ONE shared app-health registry behind BOTH the portal service's
	// reachability gate and the gateway's own, exactly as cmd/gateway/main.go
	// wires it (`Reachability: appHealth` next to `AppHealth: appHealth`). The
	// offering conditions the loading count re-applies must be answered from
	// the same source the offered count reads, or the agreement between the
	// two columns would only be tested against two registries that happen to
	// hold the same thing.
	appHealth := NewAppHealthRegistry(nil)
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, LoadedModels: reg, Reachability: appHealth})
	return New(ServerDeps{
		Tokens:       tokens,
		Usage:        recorder,
		Routes:       routeStore,
		Portal:       svc,
		LoadedModels: reg,
		AppHealth:    appHealth,
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
	s.injectLoadingOnCounts(context.Background(), auth.Token{UserID: "usr_x"}, rows, true)
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

// countingRouteSpy wraps a real routing.Store and counts calls to the four
// point-read methods startingServersByModel uses to resolve a starting spec
// to its gateway model name and decide whether its server actually OFFERS
// that model: RuntimeSpecByID, MappingByID, and (offeringServerName)
// ApplicationByID + AIServerByID. Every other method is the embedded real
// store.
type countingRouteSpy struct {
	routing.Store
	runtimeSpecByIDCalls int32
	mappingByIDCalls     int32
	applicationByIDCalls int32
	aiServerByIDCalls    int32
}

func (r *countingRouteSpy) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	atomic.AddInt32(&r.runtimeSpecByIDCalls, 1)
	return r.Store.RuntimeSpecByID(ctx, id)
}

func (r *countingRouteSpy) MappingByID(ctx context.Context, id string) (routing.ModelMapping, error) {
	atomic.AddInt32(&r.mappingByIDCalls, 1)
	return r.Store.MappingByID(ctx, id)
}

func (r *countingRouteSpy) ApplicationByID(ctx context.Context, id string) (routing.Application, error) {
	atomic.AddInt32(&r.applicationByIDCalls, 1)
	return r.Store.ApplicationByID(ctx, id)
}

func (r *countingRouteSpy) AIServerByID(ctx context.Context, id string) (routing.AIServer, error) {
	atomic.AddInt32(&r.aiServerByIDCalls, 1)
	return r.Store.AIServerByID(ctx, id)
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
//   - Routes.RuntimeSpecByID / Routes.MappingByID -- and, since the offering
//     restriction (Fix round 2) was added, Routes.ApplicationByID /
//     Routes.AIServerByID -- are each called EXACTLY ONCE, bounded by the
//     number of STARTING specs (1), never by the number of models in rows
//     (manyModelsN). Pinning the two offering reads matters as much as the
//     first two: the obvious way to check "does this server offer the model"
//     is to ask the portal for the model's server list, which is exactly the
//     per-row full-join walk this design exists to avoid.
//
// If a future change reverts to a per-row Portal.ModelServers call, or adds a
// per-row AllowedServerIDs/RuntimeSpecByID/MappingByID/ApplicationByID/
// AIServerByID call, this test fails on the call counts even though the
// produced counts would still happen to be correct.
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
	// true = the principal-facing branch, the one that pays for the
	// AllowedServerIDs call this test counts.
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: "usr_many"}, rows, true)

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
	if got := atomic.LoadInt32(&routeSpy.applicationByIDCalls); got != 1 {
		t.Fatalf("Routes.ApplicationByID called %d times, want exactly 1 (the offering check is a point read per starting spec, not a join per request)", got)
	}
	if got := atomic.LoadInt32(&routeSpy.aiServerByIDCalls); got != 1 {
		t.Fatalf("Routes.AIServerByID called %d times, want exactly 1 (the offering check is a point read per starting spec, not a join per request)", got)
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
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: testUser}, rows, true)

	if rows[0].LoadingOnCount != 1 {
		t.Fatalf("loading_on_count = %d, want 1 (server A only -- server C is starting too but restricted to usr_other_vis, not usr_test_vis)", rows[0].LoadingOnCount)
	}

	// Sanity: the SAME principal, if provisioned as otherUser, sees both.
	rows2 := []portal.ModelDTO{{ID: visModel}}
	s.injectLoadingOnCounts(ctx, auth.Token{UserID: otherUser}, rows2, true)
	if rows2[0].LoadingOnCount != 2 {
		t.Fatalf("loading_on_count for the provisioned user = %d, want 2 (both A and C visible to usr_other_vis)", rows2[0].LoadingOnCount)
	}
}

// --- Fix round 2: the count must agree with the column beside it -----------
//
// Two findings from the final whole-branch review, both of the same shape:
// loading_on_count is read against its sibling counts on the SAME row
// (offered_on_count, loaded_on), so it has to be computed under the same rules
// they are -- otherwise the yellow "Lädt" number contradicts the "Angeboten"
// number next to it. The tests below pin both halves.

// startBothFixtureServersStarting publishes a "starting" runtime spec for BOTH
// of newLoadingOnCountFixture's servers, so a test can take exactly ONE of them
// out of "offering" (or out of the principal's visibility) and see whether the
// count still attributes it.
func startBothFixtureServersStarting(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, seed := range []struct{ serverID, mappingID, specID string }{
		{mlServerA, mlMappingA, "rspec_off_a"},
		{mlServerB, mlMappingB, "rspec_off_b"},
	} {
		if err := s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: seed.specID, MappingID: seed.mappingID, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", seed.specID, err)
		}
		s.RuntimeStatus.publish(seed.serverID, []RuntimeStatusDTO{
			{SpecID: seed.specID, Model: mlAppModel, State: "starting"},
		})
	}
}

// TestLoadingOnCountCountsOnlyOfferingServers is the offering fix's proof.
// startingServersByModel used to attribute EVERY "starting" spec to its
// mapping's gateway model after only the visibility check, while
// OfferedOnCount counts a server only when its mapping survives
// activeMappingViews' conditions (server active + not unhealthy, application
// active + reachable, mapping active). A child does not stop when an operator
// disables its mapping, takes its application down, or its server goes
// unhealthy -- so the count could attribute, and even EXCEED, servers that do
// not offer the model: "Lädt 2" beside "Angeboten 1".
//
// Each case below removes server B from "offering" in one of those ways, with
// BOTH servers publishing a starting spec. The assertions are the invariant
// itself (loading <= offered) plus the exact expected pair, so a fix that
// merely clamps the number would not pass.
func TestLoadingOnCountCountsOnlyOfferingServers(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		deOffer func(t *testing.T, s *Server)
		wantWhy string
	}{
		{
			name: "mapping disabled",
			deOffer: func(t *testing.T, s *Server) {
				m, err := s.Routes.MappingByID(ctx, mlMappingB)
				if err != nil {
					t.Fatalf("MappingByID: %v", err)
				}
				m.Status = routing.ServerStatusDisabled
				if err := s.Routes.UpdateMapping(ctx, m); err != nil {
					t.Fatalf("UpdateMapping: %v", err)
				}
			},
			wantWhy: "server B's mapping is disabled, so B does not offer the model",
		},
		{
			name: "application disabled",
			deOffer: func(t *testing.T, s *Server) {
				app, err := s.Routes.ApplicationByID(ctx, mlAppB)
				if err != nil {
					t.Fatalf("ApplicationByID: %v", err)
				}
				app.Status = routing.ServerStatusDisabled
				if err := s.Routes.UpdateApplication(ctx, app); err != nil {
					t.Fatalf("UpdateApplication: %v", err)
				}
			},
			wantWhy: "server B's application is disabled, so B does not offer the model",
		},
		{
			name: "application unreachable",
			deOffer: func(t *testing.T, s *Server) {
				s.AppHealth.Set(mlAppB, false, time.Now(), "probe failed")
			},
			wantWhy: "server B's application is failing its reachability probe, so B does not offer the model",
		},
		{
			name: "server unhealthy",
			deOffer: func(t *testing.T, s *Server) {
				if err := s.Routes.SetServerHealth(ctx, mlServerB, routing.HealthUnhealthy); err != nil {
					t.Fatalf("SetServerHealth: %v", err)
				}
			},
			wantWhy: "server B is unhealthy, so it is not selectable and does not offer the model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newLoadingOnCountFixture(t)
			startBothFixtureServersStarting(t, s)
			tc.deOffer(t, s)

			byID := fetchModelsOverview(t, s, "/api/portal/models")
			dto, ok := byID[mlModel]
			if !ok {
				t.Fatalf("model %q missing from overview: %#v", mlModel, byID)
			}
			if dto.OfferedOnCount != 1 {
				t.Fatalf("offered_on_count = %d, want 1 (%s)", dto.OfferedOnCount, tc.wantWhy)
			}
			if dto.LoadingOnCount != 1 {
				t.Fatalf("loading_on_count = %d, want 1 -- %s, so only server A may be counted", dto.LoadingOnCount, tc.wantWhy)
			}
			if dto.LoadingOnCount > dto.OfferedOnCount {
				t.Fatalf("loading_on_count (%d) > offered_on_count (%d): the yellow count must never exceed the offered count it is read against", dto.LoadingOnCount, dto.OfferedOnCount)
			}
		})
	}
}

// TestLoadingOnCountFilteringMatchesItsSiblingsPerBranch is the branch fix's
// proof. injectLoadingOnCounts runs on BOTH branches of handlePortalModels,
// but the two branches compute their OTHER counts differently:
// Service.Models is modelsResponse(suppress=true), which builds
// offered_on_count from visibleMappingViews (resource-group filtered), while
// the admin ?manage=1 Service.ManageModels is modelsResponse(suppress=false),
// reading the token-less activeMappingViews and therefore deliberately
// UNFILTERED. Applying the AllowedServerIDs filter on both left the manage
// view with one filtered column beside unfiltered neighbours -- an
// under-reporting "Lädt" next to an "Angeboten" that counts everything.
//
// Fixture: both servers offer the model and both are starting; server B is
// restricted to a resource group provisioned for a DIFFERENT user. The same
// admin token must then see 1/1 on the plain listing and 2/2 on ?manage=1 --
// the point being that loading and offered move TOGETHER on each branch.
func TestLoadingOnCountFilteringMatchesItsSiblingsPerBranch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := newLoadingOnCountFixture(t)
	startBothFixtureServersStarting(t, s)

	const rgID = "rgrp_ml_branch"
	if err := s.Routes.CreateResourceGroup(ctx, routing.ResourceGroup{ID: rgID, Name: "RG restricted", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := s.Routes.SetResourceGroupServer(ctx, rgID, mlServerB); err != nil {
		t.Fatalf("SetResourceGroupServer: %v", err)
	}
	// Provisioned for somebody else: the fixture's admin token (usr_ml) is not
	// allowed to USE server B, and AllowedServerIDs has no admin bypass.
	if err := s.Routes.SetResourceGroupProvision(ctx, rgID, routing.ProvisionKindUser, "usr_someone_else"); err != nil {
		t.Fatalf("SetResourceGroupProvision: %v", err)
	}

	plain := fetchModelsOverview(t, s, "/api/portal/models")[mlModel]
	if plain.OfferedOnCount != 1 {
		t.Fatalf("plain offered_on_count = %d, want 1 (server B is provisioned to another user)", plain.OfferedOnCount)
	}
	if plain.LoadingOnCount != 1 {
		t.Fatalf("plain loading_on_count = %d, want 1 -- the principal-facing listing filters both counts the same way", plain.LoadingOnCount)
	}

	manage := fetchModelsOverview(t, s, "/api/portal/models?manage=1")[mlModel]
	if manage.OfferedOnCount != 2 {
		t.Fatalf("manage offered_on_count = %d, want 2 (sanity: the admin management listing is unfiltered by design)", manage.OfferedOnCount)
	}
	if manage.LoadingOnCount != 2 {
		t.Fatalf("manage loading_on_count = %d, want 2 -- on the unfiltered admin listing the loading count must not be the one filtered column", manage.LoadingOnCount)
	}
}

// TestLoadingOnCountFollowsAnOfferedOverrideAlias covers the alias half of the
// group/alias gap the review flagged. modelsResponse's per-token alias overlay
// copies the target's whole listing payload onto the alias row (loaded/
// loaded_on, offered_on_count, context size, vision, is_group) so the alias
// row "looks exactly like its target's row, just filed under a different
// name". The loading count belongs with them.
//
// Aliases exist only on the principal-facing listing, so the manage branch
// must NOT repoint a row's count even when a rule happens to be named like a
// real model -- asserted here with the same rule on both branch settings.
func TestLoadingOnCountFollowsAnOfferedOverrideAlias(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := newLoadingOnCountFixture(t)
	const specID = "rspec_ml_alias"
	if err := s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: specID, MappingID: mlMappingA, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	s.RuntimeStatus.publish(mlServerA, []RuntimeStatusDTO{{SpecID: specID, Model: mlAppModel, State: "starting"}})

	token := auth.Token{UserID: "usr_ml", ModelOverrideRules: map[string]auth.ModelOverrideRule{
		"team-fast": {To: mlModel, Offer: true},
	}}
	// The listing the principal gets: the alias row plus its target's row.
	rows := []portal.ModelDTO{{ID: "team-fast"}, {ID: mlModel}}
	s.injectLoadingOnCounts(ctx, token, rows, true)
	if rows[0].LoadingOnCount != 1 {
		t.Fatalf("alias row loading_on_count = %d, want 1 (the alias carries its target's row data)", rows[0].LoadingOnCount)
	}
	if rows[1].LoadingOnCount != 1 {
		t.Fatalf("target row loading_on_count = %d, want 1 (the alias must not move the count off the target)", rows[1].LoadingOnCount)
	}

	// The admin management branch: no alias overlay at all, so a row named
	// like the rule keeps its OWN (here: absent) count.
	manageRows := []portal.ModelDTO{{ID: "team-fast"}}
	s.injectLoadingOnCounts(ctx, token, manageRows, false)
	if manageRows[0].LoadingOnCount != 0 {
		t.Fatalf("manage-branch row loading_on_count = %d, want 0 (the manage listing shows real models; a token's alias rule must not repoint a row there)", manageRows[0].LoadingOnCount)
	}
}
