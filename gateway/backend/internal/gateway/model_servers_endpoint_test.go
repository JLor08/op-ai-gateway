// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"sync"
	"testing"
	"time"
)

const (
	msOwnerSecret = "ms-owner-secret"
	msServerID    = "srv_ms"
	msAppID       = "app_ms"
	msMappingID   = "map_ms"
	msModel       = "shared/model" // a gateway model name may contain '/'
	msAppModel    = "up-ms"
)

// newModelServersEndpointFixture builds a *Server whose Portal service AND
// LoadedModelRegistry share ONE registry instance, so a SetGatewayProbe on the
// server's registry both fires the SSE change broker and is visible to the
// recompute. It seeds one active server / application / mapping offering msModel.
func newModelServersEndpointFixture(t *testing.T) (*Server, *LoadedModelRegistry) {
	t.Helper()
	return newModelServersEndpointFixtureWrapped(t, func(st routing.Store) routing.Store { return st })
}

// newModelServersEndpointFixtureWrapped is newModelServersEndpointFixture with a
// hook around the routing store, so a test can gate a store read and thereby
// control exactly when the SSE handler is inside its snapshot computation.
func newModelServersEndpointFixtureWrapped(t *testing.T, wrapRoutes func(routing.Store) routing.Store) (*Server, *LoadedModelRegistry) {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_ms", Email: "ms@example.test", DisplayName: "MS User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_ms", UserID: "usr_ms", Name: "MS Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, msOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: msServerID, Name: "MS Host", Domain: "ms.example.test", Provider: routing.ProviderMock, Endpoint: "mock://ms", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: msAppID, ServerID: msServerID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: msMappingID, ApplicationID: msAppID, GatewayModelName: msModel, AppModelName: msAppModel, Status: routing.ServerStatusActive, GenTokensPerSecond: 42, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	reg := NewLoadedModelRegistry()
	recorder := usage.NewRecorder()
	routes := wrapRoutes(routeStore)
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routes, LoadedModels: reg})
	s := New(ServerDeps{
		Tokens:       tokens,
		Usage:        recorder,
		Routes:       routes,
		Portal:       svc,
		LoadedModels: reg,
	})
	return s, reg
}

// TestModelServersEndpointList: GET /api/portal/model-servers?name=<model> with a
// valid gateway:use session returns 200 and {"data":[...]} carrying the one
// offering row (with the mapping's metrics). The model name rides as a query
// parameter because it may contain '/'.
func TestModelServersEndpointList(t *testing.T) {
	s, _ := newModelServersEndpointFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-servers?name="+url.QueryEscape(msModel), nil)
	req.Header.Set("Authorization", "Bearer "+msOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1 (%+v)", len(out.Data), out.Data)
	}
	row := out.Data[0]
	if row.ServerID != msServerID || row.ApplicationID != msAppID || row.MappingID != msMappingID {
		t.Fatalf("row identity = (%q, %q, %q), want (%s, %s, %s)", row.ServerID, row.ApplicationID, row.MappingID, msServerID, msAppID, msMappingID)
	}
	if row.GenTokensPerSecond != 42 {
		t.Fatalf("gen_tokens_per_second = %v, want 42", row.GenTokensPerSecond)
	}
	if row.Loaded {
		t.Fatalf("row.Loaded = true, want false (nothing probed yet)")
	}
	if row.Priority < 1 {
		t.Fatalf("row.Priority = %d, want >= 1 (the single candidate must rank first)", row.Priority)
	}
}

// newModelServersEndpointFixtureTwoServers mirrors newModelServersEndpointFixture but seeds TWO
// servers/applications/mappings offering the SAME gateway model, with the second application given
// a strictly higher Priority (the dominant scorer term besides health/load) so the live rank has an
// unambiguous winner to assert against.
func newModelServersEndpointFixtureTwoServers(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_ms2", Email: "ms2@example.test", DisplayName: "MS2 User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_ms2", UserID: "usr_ms2", Name: "MS2 Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, msOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	// Server/app/mapping A: low priority.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_ms_a", Name: "MS Host A", Domain: "ms-a.example.test", Provider: routing.ProviderMock, Endpoint: "mock://ms-a", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer A: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_ms_a", ServerID: "srv_ms_a", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication A: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_ms_a", ApplicationID: "app_ms_a", GatewayModelName: msModel, AppModelName: msAppModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	// Server/app/mapping B: high priority — must rank first.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_ms_b", Name: "MS Host B", Domain: "ms-b.example.test", Provider: routing.ProviderMock, Endpoint: "mock://ms-b", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer B: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_ms_b", ServerID: "srv_ms_b", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 100, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication B: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_ms_b", ApplicationID: "app_ms_b", GatewayModelName: msModel, AppModelName: msAppModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}
	reg := NewLoadedModelRegistry()
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, LoadedModels: reg})
	s := New(ServerDeps{
		Tokens:       tokens,
		Usage:        recorder,
		Routes:       routeStore,
		Portal:       svc,
		LoadedModels: reg,
	})
	return s
}

// TestModelServersEndpointListPriorityOrdersHigherApplicationPriorityFirst: with two servers
// offering the same model, the live rank places the higher-Application-Priority mapping at
// priority 1 and assigns 1..N with no gaps/dupes across the returned rows (proves
// rankModelServers is actually wired into the list handler, not just present on the DTO).
func TestModelServersEndpointListPriorityOrdersHigherApplicationPriorityFirst(t *testing.T) {
	s := newModelServersEndpointFixtureTwoServers(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-servers?name="+url.QueryEscape(msModel), nil)
	req.Header.Set("Authorization", "Bearer "+msOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 (%+v)", len(out.Data), out.Data)
	}
	seen := map[int]bool{}
	var highPrioRank int
	for _, row := range out.Data {
		if row.Priority < 1 {
			t.Fatalf("row %+v has priority %d, want >= 1", row, row.Priority)
		}
		if seen[row.Priority] {
			t.Fatalf("duplicate priority %d across rows: %+v", row.Priority, out.Data)
		}
		seen[row.Priority] = true
		if row.MappingID == "map_ms_b" {
			highPrioRank = row.Priority
		}
	}
	if len(seen) != 2 || !seen[1] || !seen[2] {
		t.Fatalf("priorities across rows = %v, want exactly {1,2}", seen)
	}
	if highPrioRank != 1 {
		t.Fatalf("higher-Application-Priority mapping (map_ms_b) ranked %d, want 1", highPrioRank)
	}
}

// TestModelServersEndpointRequiresAuth: the auth/scope gate runs before the
// handler body, so a request with no bearer (and no session) is rejected 401.
func TestModelServersEndpointRequiresAuth(t *testing.T) {
	s, _ := newModelServersEndpointFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-servers?name="+url.QueryEscape(msModel), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer should be 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestModelServersEndpointEventsRequiresAuth: the SSE endpoint's auth/scope gate
// runs before any stream begins, so a request with no bearer (and no session) is
// rejected 401 and never opens the event stream. Pins the gate independently of
// the list endpoint so a refactor can't silently drop it.
func TestModelServersEndpointEventsRequiresAuth(t *testing.T) {
	s, _ := newModelServersEndpointFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-servers/events?name="+url.QueryEscape(msModel), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer should be 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Fatalf("auth gate must run before the SSE stream opens, got Content-Type %q", ct)
	}
}

// gatedRoutes blocks the FIRST routing-store read until released, so a test can
// hold the SSE handler inside its snapshot computation and act in that window.
// Every later read passes straight through (release is closed, not drained).
type gatedRoutes struct {
	routing.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedRoutes) AIServers(ctx context.Context) ([]routing.AIServer, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return g.Store.AIServers(ctx)
}

// TestModelServersEndpointEventsChangeDuringSnapshotStillArrives pins the
// subscribe/snapshot ORDER, which is what makes this endpoint's delivery
// guarantee real rather than timing-dependent.
//
// The registry's Subscribe documents "no change is fully lost" — but that only
// holds from the moment a subscription exists. If the handler computed and
// flushed its snapshot BEFORE subscribing, any loaded-state change in that
// window reached no subscriber at all and was dropped: the client then sat on a
// stale row until some later change, or the 25s heartbeat. That is also what
// made TestModelServersEndpointEventsSnapshotThenUpdate flaky on CI ("timed out
// after 3s waiting for an SSE frame") — the same window, hit by chance under
// load rather than on purpose.
//
// Here the window is made deterministic: the store read inside the snapshot
// computation is gated, the change fires while the handler is parked in it, and
// the update frame must still arrive afterwards.
func TestModelServersEndpointEventsChangeDuringSnapshotStillArrives(t *testing.T) {
	gate := &gatedRoutes{entered: make(chan struct{}), release: make(chan struct{})}
	s, reg := newModelServersEndpointFixtureWrapped(t, func(st routing.Store) routing.Store {
		gate.Store = st
		return gate
	})
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/model-servers/events?name="+url.QueryEscape(msModel), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+msOwnerSecret)

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, rErr := http.DefaultClient.Do(req)
		done <- result{resp, rErr}
	}()

	// The handler is now parked inside its snapshot computation: it has neither
	// written the snapshot nor (with the ordering fixed) missed anything.
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never reached the gated store read")
	}
	reg.SetGatewayProbe(msAppID, []string{msAppModel})
	close(gate.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Do: %v", got.err)
	}
	defer got.resp.Body.Close()
	if got.resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", got.resp.StatusCode)
	}
	reader := bufio.NewReader(got.resp.Body)

	if event, _ := readPerfSSEFrame(t, reader, 3*time.Second); event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	// The change fired before any subscription could exist under the old
	// ordering; with subscribe-first it is buffered and delivered here.
	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "update" {
		t.Fatalf("second event = %q, want update (a change during the snapshot must not be dropped)", event)
	}
	var upd struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &upd); err != nil {
		t.Fatalf("unmarshal update: %v (%s)", err, data)
	}
	if len(upd.Data) != 1 || !upd.Data[0].Loaded {
		t.Fatalf("update data = %+v, want one loaded row", upd.Data)
	}
}

// TestModelServersEndpointEventsSnapshotThenUpdate: the SSE endpoint emits a
// `snapshot` frame first, then an `update` frame carrying the recomputed list
// whenever the SHARED loaded registry signals a change. The update also reflects
// the new loaded-state, proving the compute() reads the same registry the change
// came from.
func TestModelServersEndpointEventsSnapshotThenUpdate(t *testing.T) {
	s, reg := newModelServersEndpointFixture(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/model-servers/events?name="+url.QueryEscape(msModel), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+msOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	// First frame: snapshot with the (not-loaded) offering row.
	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if len(snap.Data) != 1 || snap.Data[0].Loaded {
		t.Fatalf("snapshot data = %+v, want one not-loaded row", snap.Data)
	}

	// A loaded-state change on the SHARED registry fires an `update` frame whose
	// recomputed row now reports the model loaded.
	reg.SetGatewayProbe(msAppID, []string{msAppModel})
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "update" {
		t.Fatalf("second event = %q, want update", event)
	}
	var upd struct {
		Data []portal.ModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &upd); err != nil {
		t.Fatalf("unmarshal update: %v (%s)", err, data)
	}
	if len(upd.Data) != 1 || !upd.Data[0].Loaded {
		t.Fatalf("update data = %+v, want one loaded row", upd.Data)
	}
}
