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

// fakeAppHealth is a test AppHealthReader. An id present in known maps to the
// (reachable, at) it carries; any other id is treated as never-probed and
// reported reachable with no timestamp, mirroring the lenient registry default.
type fakeAppHealth struct {
	reachable map[string]bool
	at        map[string]time.Time
}

func (f fakeAppHealth) known(appID string) bool {
	_, ok := f.reachable[appID]
	return ok
}

func (f fakeAppHealth) Reachable(appID string) bool {
	if r, ok := f.reachable[appID]; ok {
		return r
	}
	return true
}

func (f fakeAppHealth) ApplicationHealth(appID string) (bool, time.Time, bool) {
	if !f.known(appID) {
		return true, time.Time{}, false
	}
	return f.reachable[appID], f.at[appID], true
}

// newReachabilityTestService builds a Service backed by an in-memory routing
// store and the given (optional) reachability reader. It returns the service
// and the store so tests can seed servers/apps/mappings directly.
func newReachabilityTestService(t *testing.T, now time.Time, reader AppHealthReader) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(context.Background(), store.User{ID: "usr_admin", Email: "admin@example.test", DisplayName: "admin", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Routes: routeStore, Reachability: reader, Clock: func() time.Time { return now }})
	return svc, routeStore
}

// seedServerAppMapping creates an active server with one active application and
// one active mapping (gatewayModel -> appModel) in the store.
func seedServerAppMapping(t *testing.T, routeStore *routing.MemoryStore, now time.Time, serverID, appID, gatewayModel string) {
	t.Helper()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_" + appID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: gatewayModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", appID, err)
	}
}

func modelIDs(resp ModelsResponse) map[string]bool {
	out := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		out[m.ID] = true
	}
	return out
}

// Models() and every activeMappingViews consumer must drop the models of an
// application whose reachability probe is failing.
func TestModelsExcludesUnreachableApplication(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reader := fakeAppHealth{reachable: map[string]bool{"app_down": false}}
	svc, routeStore := newReachabilityTestService(t, now, reader)
	seedServerAppMapping(t, routeStore, now, "srv_up", "app_up", "model-up")
	seedServerAppMapping(t, routeStore, now, "srv_down", "app_down", "model-down")

	got := modelIDs(svc.Models(context.Background(), adminToken()))
	if !got["model-up"] {
		t.Fatalf("Models() missing model-up (reachable app), got %v", got)
	}
	if got["model-down"] {
		t.Fatalf("Models() includes model-down (unreachable app), got %v", got)
	}

	// ModelsForFlavor shares the same gate.
	openai := svc.ModelsForFlavor(context.Background(), adminToken(), routing.APIFlavorOpenAI)
	for _, id := range openai {
		if id == "model-down" {
			t.Fatalf("ModelsForFlavor(openai) includes model-down, got %v", openai)
		}
	}
}

// A nil reader is lenient: no application is excluded.
func TestModelsNilReaderIncludesAll(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newReachabilityTestService(t, now, nil)
	seedServerAppMapping(t, routeStore, now, "srv_up", "app_up", "model-up")
	seedServerAppMapping(t, routeStore, now, "srv_down", "app_down", "model-down")

	got := modelIDs(svc.Models(context.Background(), adminToken()))
	if !got["model-up"] || !got["model-down"] {
		t.Fatalf("nil reader must include all models, got %v", got)
	}
}

// ListApplications enriches the DTO with reachable + last_checked_at from a
// reader that has probed the application.
func TestListApplicationsEnrichesReachabilityKnown(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	checkedAt := now.Add(-30 * time.Second)
	reader := fakeAppHealth{
		reachable: map[string]bool{"app_down": false},
		at:        map[string]time.Time{"app_down": checkedAt},
	}
	svc, routeStore := newReachabilityTestService(t, now, reader)
	seedServerAppMapping(t, routeStore, now, "srv_down", "app_down", "model-down")

	resp, err := svc.ListApplications(context.Background(), systemAdminToken(), "srv_down")
	if err != nil {
		t.Fatalf("ListApplications: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("ListApplications len = %d, want 1", len(resp.Data))
	}
	dto := resp.Data[0]
	if dto.Reachable {
		t.Fatalf("Reachable = true, want false (probed unreachable)")
	}
	if dto.LastCheckedAt == nil {
		t.Fatalf("LastCheckedAt = nil, want %v", checkedAt)
	}
	if !dto.LastCheckedAt.Equal(checkedAt) {
		t.Fatalf("LastCheckedAt = %v, want %v", *dto.LastCheckedAt, checkedAt)
	}
}

// An unknown (never-probed) application, or a nil reader, reports
// reachable=true with a nil last_checked_at.
func TestListApplicationsReachabilityUnknownAndNil(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// Unknown: reader present but this app was never probed.
	unknownReader := fakeAppHealth{reachable: map[string]bool{"other": false}}
	svc, routeStore := newReachabilityTestService(t, now, unknownReader)
	seedServerAppMapping(t, routeStore, now, "srv_up", "app_up", "model-up")
	resp, err := svc.ListApplications(context.Background(), systemAdminToken(), "srv_up")
	if err != nil {
		t.Fatalf("ListApplications (unknown): %v", err)
	}
	if got := resp.Data[0]; !got.Reachable || got.LastCheckedAt != nil {
		t.Fatalf("unknown app enrichment = (reachable=%v, last=%v), want (true, nil)", got.Reachable, got.LastCheckedAt)
	}

	// Nil reader: same lenient defaults.
	svcNil, routeStoreNil := newReachabilityTestService(t, now, nil)
	seedServerAppMapping(t, routeStoreNil, now, "srv_up", "app_up", "model-up")
	respNil, err := svcNil.ListApplications(context.Background(), systemAdminToken(), "srv_up")
	if err != nil {
		t.Fatalf("ListApplications (nil): %v", err)
	}
	if got := respNil.Data[0]; !got.Reachable || got.LastCheckedAt != nil {
		t.Fatalf("nil reader enrichment = (reachable=%v, last=%v), want (true, nil)", got.Reachable, got.LastCheckedAt)
	}
}
