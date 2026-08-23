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
	mgspGroupName   = "grp-detail-prov"
	mgspOwnerSecret = "mgsp-owner-secret"
	mgspOtherSecret = "mgsp-other-secret"
)

// newModelGroupServersProvisioningFixture mirrors newModelGroupServersFixture
// (same two-member model group / two-server layout) but additionally puts the
// SECOND member's offering server ("srv_mgsp_b") into a resource group
// provisioned ONLY for "usr_mgsp_owner" (Resource Groups Phase 2 -- Task 4).
// The first member's server ("srv_mgsp_a") stays a member of NO resource
// group, i.e. unrestricted for everyone. It returns the server, plus the
// owner's bearer secret (provisioned) and a second, non-provisioned user's
// bearer secret, so a test can assert /api/portal/model-group-servers
// differs per caller.
func newModelGroupServersProvisioningFixture(t *testing.T) (s *Server, ownerSecret, otherSecret string) {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_mgsp_owner", Email: "mgsp-owner@example.test", DisplayName: "MGSP Owner", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_mgsp_other", Email: "mgsp-other@example.test", DisplayName: "MGSP Other", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_mgsp_owner", UserID: "usr_mgsp_owner", Name: "MGSP Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, mgspOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_mgsp_other", UserID: "usr_mgsp_other", Name: "MGSP Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, mgspOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}

	routeStore := routing.NewMemoryStore()
	// Member 1 ("mgsp-model-a"): server A, UNRESTRICTED (no resource group).
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_mgsp_a", Name: "MGSP Host A", Domain: "mgsp-a.example.test", Provider: routing.ProviderMock, Endpoint: "mock://mgsp-a", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer A: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_mgsp_a", ServerID: "srv_mgsp_a", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication A: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_mgsp_a", ApplicationID: "app_mgsp_a", GatewayModelName: "mgsp-model-a", AppModelName: "up-mgsp-a", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	// Member 2 ("mgsp-model-b"): server B, RESTRICTED via a resource group
	// provisioned only for usr_mgsp_owner.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_mgsp_b", Name: "MGSP Host B", Domain: "mgsp-b.example.test", Provider: routing.ProviderMock, Endpoint: "mock://mgsp-b", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer B: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_mgsp_b", ServerID: "srv_mgsp_b", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication B: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_mgsp_b", ApplicationID: "app_mgsp_b", GatewayModelName: "mgsp-model-b", AppModelName: "up-mgsp-b", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}

	if err := routeStore.CreateResourceGroup(ctx, routing.ResourceGroup{ID: "rgrp_mgsp", Name: "RG MGSP", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := routeStore.SetResourceGroupServer(ctx, "rgrp_mgsp", "srv_mgsp_b"); err != nil {
		t.Fatalf("SetResourceGroupServer: %v", err)
	}
	if err := routeStore.SetResourceGroupProvision(ctx, "rgrp_mgsp", routing.ProvisionKindUser, "usr_mgsp_owner"); err != nil {
		t.Fatalf("SetResourceGroupProvision: %v", err)
	}

	if err := routeStore.CreateModelGroup(ctx, routing.ModelGroup{ID: "grp_mgsp", GatewayModelName: mgspGroupName, DisplayName: "Group Detail Provisioned", Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if err := routeStore.SetGroupMembers(ctx, "grp_mgsp", []routing.GroupMember{
		{MemberGatewayName: "mgsp-model-a", Priority: 0},
		{MemberGatewayName: "mgsp-model-b", Priority: 1},
	}); err != nil {
		t.Fatalf("SetGroupMembers: %v", err)
	}

	groupReg := NewGroupRegistry(routeStore)
	if err := groupReg.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore})
	s = New(ServerDeps{
		Tokens: tokens,
		Usage:  recorder,
		Routes: routeStore,
		Portal: svc,
		Groups: groupReg,
	})
	return s, mgspOwnerSecret, mgspOtherSecret
}

// TestModelGroupServersEndpointFiltersByProvisioning proves the Resource
// Groups Phase 2 (Task 4) visibility filter reaches
// GET /api/portal/model-group-servers: the provisioned owner sees BOTH
// members' rows (server A unrestricted, server B provisioned for them), while
// a non-provisioned caller sees ONLY the unrestricted member A's row -- the
// restricted member B's row (server_mgsp_b) never appears for them, closing
// the detail/ranking leak a raw GET of a known group name would otherwise
// expose.
func TestModelGroupServersEndpointFiltersByProvisioning(t *testing.T) {
	s, ownerSecret, otherSecret := newModelGroupServersProvisioningFixture(t)

	fetch := func(secret string) []portal.GroupModelServerDTO {
		req := httptest.NewRequest(http.MethodGet, "/api/portal/model-group-servers?name="+url.QueryEscape(mgspGroupName), nil)
		req.Header.Set("Authorization", "Bearer "+secret)
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
		return out.Data
	}

	ownerRows := fetch(ownerSecret)
	if len(ownerRows) != 2 {
		t.Fatalf("provisioned owner: len(data) = %d, want 2 (both members visible): %+v", len(ownerRows), ownerRows)
	}
	seenModels := map[string]bool{}
	for _, row := range ownerRows {
		seenModels[row.Model] = true
	}
	if !seenModels["mgsp-model-a"] || !seenModels["mgsp-model-b"] {
		t.Fatalf("provisioned owner rows = %+v, want both mgsp-model-a and mgsp-model-b", ownerRows)
	}

	otherRows := fetch(otherSecret)
	if len(otherRows) != 1 {
		t.Fatalf("non-provisioned caller: len(data) = %d, want 1 (only the unrestricted member): %+v", len(otherRows), otherRows)
	}
	if otherRows[0].Model != "mgsp-model-a" || otherRows[0].ServerID != "srv_mgsp_a" {
		t.Fatalf("non-provisioned caller row = %+v, want mgsp-model-a on srv_mgsp_a", otherRows[0])
	}
	for _, row := range otherRows {
		if row.ServerID == "srv_mgsp_b" {
			t.Fatalf("non-provisioned caller saw the restricted server's row: %+v", otherRows)
		}
	}
}
