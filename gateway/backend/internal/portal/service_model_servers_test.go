// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// fakeLoadedModels is a test LoadedModelReader: an appID maps to the set of
// upstream model names currently loaded for it (serverID is ignored). Any other
// appID reports nothing loaded. Mirrors the *gateway.LoadedModelRegistry contract
// without importing internal/gateway (which imports internal/portal -> cycle).
type fakeLoadedModels struct {
	byApp map[string][]string
}

func (f fakeLoadedModels) LoadedAppModels(appID, _ string) []string {
	return f.byApp[appID]
}

// newModelServersTestService builds a Service backed by an in-memory routing
// store and the given (optional) loaded-model reader. It returns the service and
// the store so tests can seed servers/apps/mappings directly.
func newModelServersTestService(t *testing.T, now time.Time, loaded LoadedModelReader) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(context.Background(), store.User{ID: "usr_admin", Email: "admin@example.test", DisplayName: "admin", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Routes: routeStore, LoadedModels: loaded, Clock: func() time.Time { return now }})
	return svc, routeStore
}

// seedOffering creates an active server (name == serverID) with one active
// application and one active mapping (gatewayModel -> appModel) carrying genTPS.
func seedOffering(t *testing.T, routeStore *routing.MemoryStore, now time.Time, serverID, appID, mappingID, gatewayModel, appModel string, genTPS float64) {
	t.Helper()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: appModel, Status: routing.ServerStatusActive, GenTokensPerSecond: genTPS, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
}

// ModelServers returns one row per (server, mapping) that offers the model,
// sorted by server name, enriched with live loaded-state and can_load (admin =
// true for every row). An unknown model resolves to an empty slice.
func TestModelServers(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	loaded := fakeLoadedModels{byApp: map[string][]string{"app-a": {"up-a"}}}
	svc, routeStore := newModelServersTestService(t, now, loaded)
	seedOffering(t, routeStore, now, "srv-a", "app-a", "map-a", "shared", "up-a", 42)
	seedOffering(t, routeStore, now, "srv-b", "app-b", "map-b", "shared", "up-b", 0)

	rows, err := svc.ModelServers(context.Background(), adminToken(), "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (%+v)", len(rows), rows)
	}
	if rows[0].ServerName != "srv-a" || rows[1].ServerName != "srv-b" {
		t.Fatalf("rows not sorted by ServerName: %q, %q", rows[0].ServerName, rows[1].ServerName)
	}
	if !rows[0].Loaded {
		t.Fatalf("rows[0].Loaded = false, want true (up-a loaded on app-a)")
	}
	if rows[1].Loaded {
		t.Fatalf("rows[1].Loaded = true, want false (up-b not loaded)")
	}
	if !rows[0].CanLoad || !rows[1].CanLoad {
		t.Fatalf("admin CanLoad = (%v, %v), want (true, true)", rows[0].CanLoad, rows[1].CanLoad)
	}
	if rows[0].GenTokensPerSecond != 42 {
		t.Fatalf("rows[0].GenTokensPerSecond = %v, want 42", rows[0].GenTokensPerSecond)
	}
	if rows[0].ServerID != "srv-a" || rows[0].ApplicationID != "app-a" || rows[0].MappingID != "map-a" {
		t.Fatalf("rows[0] identity = (%q, %q, %q), want (srv-a, app-a, map-a)", rows[0].ServerID, rows[0].ApplicationID, rows[0].MappingID)
	}

	empty, err := svc.ModelServers(context.Background(), adminToken(), "does-not-exist")
	if err != nil {
		t.Fatalf("ModelServers(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown model rows = %d, want 0", len(empty))
	}
}

// can_load is per-row: for a non-admin, true only on a server the caller owns.
// The row SET stays global (both servers appear regardless of ownership).
func TestModelServersCanLoadOwnership(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-a", "app-a", "map-a", "shared", "up-a", 0)
	seedOffering(t, routeStore, now, "srv-b", "app-b", "map-b", "shared", "up-b", 0)
	if err := routeStore.SetServerOwners(context.Background(), "srv-a", []string{"user-1"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}

	principal := auth.Token{UserID: "user-1", Scopes: []string{"gateway:use"}}
	rows, err := svc.ModelServers(context.Background(), principal, "shared")
	if err != nil {
		t.Fatalf("ModelServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (global, not owner-filtered)", len(rows))
	}
	byServer := map[string]ModelServerDTO{}
	for _, r := range rows {
		byServer[r.ServerID] = r
	}
	if !byServer["srv-a"].CanLoad {
		t.Fatalf("owner's server-a CanLoad = false, want true")
	}
	if byServer["srv-b"].CanLoad {
		t.Fatalf("non-owned server-b CanLoad = true, want false")
	}
}

// TestModelServersHiddenLockedSuppression: a non-admin gateway:use principal
// gets an EMPTY slice from ModelServers for a hidden or locked model --
// exactly the same suppression Models() already applies to the top-level
// listing, now closing the by-name detail-view leak too (security fix; see
// the VISIBILITY-SURFACE MATRIX doc-comment on visibleMappingViews in
// service.go). A shown model is unaffected. An admin bypasses the
// suppression entirely -- the ModelServersSection management flow, same as
// ManageModels() -- and sees every row regardless of visibility.
func TestModelServersHiddenLockedSuppression(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newModelServersTestService(t, now, fakeLoadedModels{})
	seedOffering(t, routeStore, now, "srv-hidden", "app-hidden", "map-hidden", "hidden-model", "hidden-up", 0)
	seedOffering(t, routeStore, now, "srv-locked", "app-locked", "map-locked", "locked-model", "locked-up", 0)
	seedOffering(t, routeStore, now, "srv-shown", "app-shown", "map-shown", "shown-model", "shown-up", 0)
	if err := routeStore.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: "hidden-model", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting hidden: %v", err)
	}
	if err := routeStore.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: "locked-model", Visibility: "locked", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting locked: %v", err)
	}

	nonAdmin := auth.Token{UserID: "usr_plain_ms", Scopes: []string{"gateway:use"}}

	rowsHidden, err := svc.ModelServers(context.Background(), nonAdmin, "hidden-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, hidden-model): %v", err)
	}
	if len(rowsHidden) != 0 {
		t.Fatalf("ModelServers(non-admin, hidden-model) = %+v, want empty", rowsHidden)
	}

	rowsLocked, err := svc.ModelServers(context.Background(), nonAdmin, "locked-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, locked-model): %v", err)
	}
	if len(rowsLocked) != 0 {
		t.Fatalf("ModelServers(non-admin, locked-model) = %+v, want empty", rowsLocked)
	}

	rowsShown, err := svc.ModelServers(context.Background(), nonAdmin, "shown-model")
	if err != nil {
		t.Fatalf("ModelServers(non-admin, shown-model): %v", err)
	}
	if len(rowsShown) != 1 {
		t.Fatalf("ModelServers(non-admin, shown-model) = %+v, want 1 row (unaffected)", rowsShown)
	}

	admin := adminToken()
	rowsHiddenAdmin, err := svc.ModelServers(context.Background(), admin, "hidden-model")
	if err != nil {
		t.Fatalf("ModelServers(admin, hidden-model): %v", err)
	}
	if len(rowsHiddenAdmin) != 1 {
		t.Fatalf("ModelServers(admin, hidden-model) = %+v, want 1 row (admin bypass, unfiltered)", rowsHiddenAdmin)
	}

	rowsLockedAdmin, err := svc.ModelServers(context.Background(), admin, "locked-model")
	if err != nil {
		t.Fatalf("ModelServers(admin, locked-model): %v", err)
	}
	if len(rowsLockedAdmin) != 1 {
		t.Fatalf("ModelServers(admin, locked-model) = %+v, want 1 row (admin bypass, unfiltered)", rowsLockedAdmin)
	}
}
