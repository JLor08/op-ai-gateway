// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
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
	"testing"
	"time"
)

const (
	mgsOwnerSecret = "mgs-owner-secret"
	mgsGroupName   = "grp-detail"
)

// newModelGroupServersFixture seeds an ACTIVE model group ("grp-detail") whose flattened
// members are two leaf gateway models, each offered by its own server/application/mapping
// (so a group-detail rank has an unambiguous per-model row to assert against). It builds a
// GroupRegistry from the SAME store and refreshes it, wiring it as ServerDeps.Groups.
func newModelGroupServersFixture(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_mgs", Email: "mgs@example.test", DisplayName: "MGS User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_mgs", UserID: "usr_mgs", Name: "MGS Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, mgsOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}

	routeStore := routing.NewMemoryStore()
	// Member 1 ("mgs-model-a"): server A / app A / mapping A.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_mgs_a", Name: "MGS Host A", Domain: "mgs-a.example.test", Provider: routing.ProviderMock, Endpoint: "mock://mgs-a", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer A: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_mgs_a", ServerID: "srv_mgs_a", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication A: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_mgs_a", ApplicationID: "app_mgs_a", GatewayModelName: "mgs-model-a", AppModelName: "up-mgs-a", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	// Member 2 ("mgs-model-b"): server B / app B / mapping B.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_mgs_b", Name: "MGS Host B", Domain: "mgs-b.example.test", Provider: routing.ProviderMock, Endpoint: "mock://mgs-b", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer B: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_mgs_b", ServerID: "srv_mgs_b", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication B: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_mgs_b", ApplicationID: "app_mgs_b", GatewayModelName: "mgs-model-b", AppModelName: "up-mgs-b", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}

	if err := routeStore.CreateModelGroup(ctx, routing.ModelGroup{ID: "grp_mgs", GatewayModelName: mgsGroupName, DisplayName: "Group Detail", Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if err := routeStore.SetGroupMembers(ctx, "grp_mgs", []routing.GroupMember{
		{MemberGatewayName: "mgs-model-a", Priority: 0},
		{MemberGatewayName: "mgs-model-b", Priority: 1},
	}); err != nil {
		t.Fatalf("SetGroupMembers: %v", err)
	}

	groupReg := NewGroupRegistry(routeStore)
	if err := groupReg.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore})
	s := New(ServerDeps{
		Tokens: tokens,
		Usage:  recorder,
		Routes: routeStore,
		Portal: svc,
		Groups: groupReg,
	})
	return s
}

// TestModelGroupServersEndpointList: GET /api/portal/model-group-servers?name=<group> returns
// 200 and one row per (model, server) the group's flattened members offer — carrying the leaf
// model name, a 1-based display priority with no gaps/dupes, and MANUAL traversal order
// (member 1's row before member 2's row, since both are equally available single-server
// offerings). The rank does not model the group's selection settings.
func TestModelGroupServersEndpointList(t *testing.T) {
	s := newModelGroupServersFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-group-servers?name="+url.QueryEscape(mgsGroupName), nil)
	req.Header.Set("Authorization", "Bearer "+mgsOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.GroupModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 (%+v)", len(out.Data), out.Data)
	}

	seenPriority := map[int]bool{}
	byModel := map[string]portal.GroupModelServerDTO{}
	for _, row := range out.Data {
		if row.Priority < 1 {
			t.Fatalf("row %+v has priority %d, want >= 1", row, row.Priority)
		}
		if seenPriority[row.Priority] {
			t.Fatalf("duplicate priority %d across rows: %+v", row.Priority, out.Data)
		}
		seenPriority[row.Priority] = true
		if row.Model == "" {
			t.Fatalf("row %+v carries no model name", row)
		}
		byModel[row.Model] = row
	}
	if len(seenPriority) != 2 || !seenPriority[1] || !seenPriority[2] {
		t.Fatalf("priorities across rows = %v, want exactly {1,2} (contiguous, no gaps/dupes)", seenPriority)
	}

	rowA, ok := byModel["mgs-model-a"]
	if !ok {
		t.Fatalf("no row for member mgs-model-a: %+v", out.Data)
	}
	if rowA.ServerID != "srv_mgs_a" || rowA.MappingID != "map_mgs_a" {
		t.Fatalf("mgs-model-a row identity = (%q, %q), want (srv_mgs_a, map_mgs_a)", rowA.ServerID, rowA.MappingID)
	}
	rowB, ok := byModel["mgs-model-b"]
	if !ok {
		t.Fatalf("no row for member mgs-model-b: %+v", out.Data)
	}
	if rowB.ServerID != "srv_mgs_b" || rowB.MappingID != "map_mgs_b" {
		t.Fatalf("mgs-model-b row identity = (%q, %q), want (srv_mgs_b, map_mgs_b)", rowB.ServerID, rowB.MappingID)
	}
	// Both candidates are single-offering and equally healthy/available, so the tie-break
	// falls to the flattened MANUAL traversal order: member 1 (mgs-model-a) must rank
	// before member 2.
	if rowA.Priority != 1 || rowB.Priority != 2 {
		t.Fatalf("manual traversal order not honored: mgs-model-a priority=%d, mgs-model-b priority=%d, want 1 and 2", rowA.Priority, rowB.Priority)
	}
}

// TestModelGroupServersEndpointUnknownGroup: an unknown/inactive group name yields an empty
// data array, never an error.
func TestModelGroupServersEndpointUnknownGroup(t *testing.T) {
	s := newModelGroupServersFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-group-servers?name="+url.QueryEscape("does-not-exist"), nil)
	req.Header.Set("Authorization", "Bearer "+mgsOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []portal.GroupModelServerDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 0 {
		t.Fatalf("len(data) = %d, want 0 for an unknown group", len(out.Data))
	}
}

// TestModelGroupServersEndpointRequiresAuth: the auth/scope gate runs before the handler body.
func TestModelGroupServersEndpointRequiresAuth(t *testing.T) {
	s := newModelGroupServersFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-group-servers?name="+url.QueryEscape(mgsGroupName), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer should be 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestModelGroupServersEndpointRejectsNonGet: a non-GET method is rejected with 405.
func TestModelGroupServersEndpointRejectsNonGet(t *testing.T) {
	s := newModelGroupServersFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/model-group-servers?name="+url.QueryEscape(mgsGroupName), nil)
	req.Header.Set("Authorization", "Bearer "+mgsOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
