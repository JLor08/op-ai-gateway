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

// These endpoint tests cover the HTTP layer of the service-side resource-group
// self-service path (#183 Phase 0, spec §9a):
//
//	GET    /api/portal/services/{id}/resource-groups
//	PUT    /api/portal/services/{id}/resource-groups/{rgId}
//	DELETE /api/portal/services/{id}/resource-groups/{rgId}
//
// routing dispatch, response shapes, the scope gate, the no-leak 404 mapping
// and method handling. The grant logic is unit-tested at the service layer
// (portal.service_service_resource_groups_test.go).

const (
	svcRGManagerSecret  = "svc-rg-manager-secret"  // owner of AG -> manages svc_rg, reaches rgrp_reach
	svcRGPlainSecret    = "svc-rg-plain-secret"    // plain member of AG: reaches the RG, manages nothing
	svcRGStrangerSecret = "svc-rg-stranger-secret" // in no group at all
	svcRGNoScopeSecret  = "svc-rg-noscope-secret"  // the manager, but a token WITHOUT gateway:use

	svcRGServiceID = "svc_rg"
	svcRGReachID   = "rgrp_reach"
	svcRGHiddenID  = "rgrp_hidden"
)

type svcRGFixture struct {
	srv    *Server
	routes *routing.MemoryStore
}

// newServiceRGFixture seeds SG -> AG (owner usr_rg_manager, plain member
// usr_rg_plain), a service svc_rg linked to AG, a resource group rgrp_reach
// linked to AG and a resource group rgrp_hidden linked to an admin group (AG2)
// nobody in the fixture belongs to.
func newServiceRGFixture(t *testing.T) svcRGFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	for _, u := range []struct{ id, role string }{
		{"usr_rg_manager", "admin"},
		{"usr_rg_plain", "admin"},
		{"usr_rg_stranger", "user"},
	} {
		dir.AddUser(store.User{ID: u.id, Email: u.id + "@example.test", DisplayName: u.id, Role: u.role, Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	}
	mustToken := func(id, userID, secret, scopes string) {
		t.Helper()
		if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: id, UserID: userID, Name: id, Status: store.TokenStatusActive, Scopes: scopes, CreatedAt: now, UpdatedAt: now}, secret); err != nil {
			t.Fatalf("CreatePlainToken(%s): %v", id, err)
		}
	}
	mustToken("tok_rg_manager", "usr_rg_manager", svcRGManagerSecret, `["gateway:use","admin"]`)
	mustToken("tok_rg_plain", "usr_rg_plain", svcRGPlainSecret, `["gateway:use","admin"]`)
	mustToken("tok_rg_stranger", "usr_rg_stranger", svcRGStrangerSecret, `["gateway:use"]`)
	mustToken("tok_rg_noscope", "usr_rg_manager", svcRGNoScopeSecret, `["admin"]`)

	mustGroup := func(g store.UserGroup) {
		t.Helper()
		g.CreatedAt, g.UpdatedAt = now, now
		if err := dir.CreateUserGroup(ctx, g); err != nil {
			t.Fatalf("CreateUserGroup(%s): %v", g.ID, err)
		}
	}
	mustMember := func(groupID, userID string) {
		t.Helper()
		if err := dir.SetUserGroupMember(ctx, groupID, userID, store.GroupStateMember, ""); err != nil {
			t.Fatalf("SetUserGroupMember(%s, %s): %v", groupID, userID, err)
		}
	}
	mustGroup(store.UserGroup{ID: "ugrp_rg_sys", Tier: store.GroupTierSystem, Name: "RG SG"})
	mustGroup(store.UserGroup{ID: "ugrp_rg_ag", Tier: store.GroupTierAdmin, Name: "RG AG", ParentGroupID: "ugrp_rg_sys", OwnerUserID: "usr_rg_manager"})
	mustGroup(store.UserGroup{ID: "ugrp_rg_ag2", Tier: store.GroupTierAdmin, Name: "RG AG2", ParentGroupID: "ugrp_rg_sys", OwnerUserID: "usr_rg_other"})
	mustMember("ugrp_rg_ag", "usr_rg_manager")
	mustMember("ugrp_rg_ag", "usr_rg_plain")

	routes := routing.NewMemoryStore()
	if err := routes.CreateService(ctx, routing.Service{ID: svcRGServiceID, Name: "RG Service", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if err := routes.SetServiceAdminGroup(ctx, svcRGServiceID, "ugrp_rg_ag"); err != nil {
		t.Fatalf("SetServiceAdminGroup: %v", err)
	}
	for _, rg := range []struct{ id, name, adminGroup string }{
		{svcRGReachID, "Reachable RG", "ugrp_rg_ag"},
		{svcRGHiddenID, "Hidden RG", "ugrp_rg_ag2"},
	} {
		if err := routes.CreateResourceGroup(ctx, routing.ResourceGroup{ID: rg.id, Name: rg.name, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateResourceGroup(%s): %v", rg.id, err)
		}
		if err := routes.SetResourceGroupAdminGroup(ctx, rg.id, rg.adminGroup); err != nil {
			t.Fatalf("SetResourceGroupAdminGroup(%s): %v", rg.id, err)
		}
	}

	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Groups: dir, Usage: recorder, Routes: routes, Clock: func() time.Time { return now }})
	return svcRGFixture{srv: New(ServerDeps{Tokens: tokens, Usage: recorder, Routes: routes, Portal: svc}), routes: routes}
}

func (f svcRGFixture) do(method, path, secret string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, svcAuthedRequest(method, path, "", secret))
	return rec
}

func svcRGPath(rest string) string {
	return "/api/portal/services/" + svcRGServiceID + "/resource-groups" + rest
}

type svcRGListBody struct {
	Data []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Member bool   `json:"member"`
	} `json:"data"`
}

func (f svcRGFixture) list(t *testing.T, secret string) svcRGListBody {
	t.Helper()
	rec := f.do(http.MethodGet, svcRGPath(""), secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body svcRGListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal list: %v (%s)", err, rec.Body.String())
	}
	return body
}

func (f svcRGFixture) provisioned(t *testing.T) []string {
	t.Helper()
	got, err := f.routes.ResourceGroupIDsByProvisionTargets(context.Background(), routing.ProvisionKindService, []string{svcRGServiceID})
	if err != nil {
		t.Fatalf("ResourceGroupIDsByProvisionTargets: %v", err)
	}
	return got
}

// GET -> {data:[...]} of the reachable groups only; PUT joins; GET shows
// member:true; DELETE leaves; GET shows member:false again.
func TestServiceResourceGroupsEndpointManagerJoinLeaveRoundTrip(t *testing.T) {
	f := newServiceRGFixture(t)

	body := f.list(t, svcRGManagerSecret)
	if len(body.Data) != 1 || body.Data[0].ID != svcRGReachID || body.Data[0].Name != "Reachable RG" || body.Data[0].Member {
		t.Fatalf("initial data = %+v, want only [{%s Reachable RG member:false}]", body.Data, svcRGReachID)
	}

	rec := f.do(http.MethodPut, svcRGPath("/"+svcRGReachID), svcRGManagerSecret)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("PUT = %d %q, want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
	if got := f.provisioned(t); len(got) != 1 || got[0] != svcRGReachID {
		t.Fatalf("service provisions after PUT = %v, want [%s]", got, svcRGReachID)
	}
	if body = f.list(t, svcRGManagerSecret); len(body.Data) != 1 || !body.Data[0].Member {
		t.Fatalf("data after PUT = %+v, want member:true", body.Data)
	}

	rec = f.do(http.MethodDelete, svcRGPath("/"+svcRGReachID), svcRGManagerSecret)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("DELETE = %d %q, want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
	if got := f.provisioned(t); len(got) != 0 {
		t.Fatalf("service provisions after DELETE = %v, want empty", got)
	}
	if body = f.list(t, svcRGManagerSecret); len(body.Data) != 1 || body.Data[0].Member {
		t.Fatalf("data after DELETE = %+v, want member:false", body.Data)
	}
}

// A caller who reaches the RG (member of its admin group) but does not manage
// the service, a stranger, and an unknown service id all get the SAME 404
// service.not_found on every verb (no existence leak), and nothing is written.
func TestServiceResourceGroupsEndpointNonManagerAndStranger404(t *testing.T) {
	f := newServiceRGFixture(t)
	cases := []struct{ name, secret, serviceID string }{
		{"plain member", svcRGPlainSecret, svcRGServiceID},
		{"stranger", svcRGStrangerSecret, svcRGServiceID},
		{"unknown service id", svcRGManagerSecret, "svc_missing"},
	}
	for _, tc := range cases {
		base := "/api/portal/services/" + tc.serviceID + "/resource-groups"
		for _, req := range []struct{ method, path string }{
			{http.MethodGet, base},
			{http.MethodPut, base + "/" + svcRGReachID},
			{http.MethodDelete, base + "/" + svcRGReachID},
		} {
			rec := f.do(req.method, req.path, tc.secret)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s %s: status = %d, want 404, body = %s", tc.name, req.method, req.path, rec.Code, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != "service.not_found" {
				t.Fatalf("%s %s: error code = %q, want service.not_found", tc.name, req.method, code)
			}
		}
	}
	if got := f.provisioned(t); len(got) != 0 {
		t.Fatalf("service provisions after rejected calls = %v, want empty", got)
	}
}

// A manager PUT/DELETE on a resource group they cannot reach (linked only to an
// admin group they are not a member of) or that does not exist -> the same 404
// resource_group.not_found.
func TestServiceResourceGroupsEndpointUnreachableOrUnknownGroup404(t *testing.T) {
	f := newServiceRGFixture(t)
	for _, rgID := range []string{svcRGHiddenID, "rgrp_missing"} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := f.do(method, svcRGPath("/"+rgID), svcRGManagerSecret)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s: status = %d, want 404, body = %s", method, rgID, rec.Code, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.not_found" {
				t.Fatalf("%s %s: error code = %q, want resource_group.not_found", method, rgID, code)
			}
		}
	}
}

// The route is behind requireWebScope(gateway:use): a token without it -> 403.
func TestServiceResourceGroupsEndpointRequiresGatewayUseScope(t *testing.T) {
	f := newServiceRGFixture(t)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, svcRGPath("")},
		{http.MethodPut, svcRGPath("/" + svcRGReachID)},
		{http.MethodDelete, svcRGPath("/" + svcRGReachID)},
	} {
		rec := f.do(req.method, req.path, svcRGNoScopeSecret)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403, body = %s", req.method, rec.Code, rec.Body.String())
		}
	}
	if got := f.provisioned(t); len(got) != 0 {
		t.Fatalf("service provisions after scope-rejected calls = %v, want empty", got)
	}
}

// POST on the collection path -> 405 (only GET); PATCH/GET on the item path ->
// 405 (only PUT/DELETE); a path with extra segments -> 404 service.not_found.
func TestServiceResourceGroupsEndpointMethodsAndShapes(t *testing.T) {
	f := newServiceRGFixture(t)
	if rec := f.do(http.MethodPost, svcRGPath(""), svcRGManagerSecret); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST collection = %d, want 405", rec.Code)
	}
	for _, method := range []string{http.MethodPatch, http.MethodGet} {
		rec := f.do(method, svcRGPath("/"+svcRGReachID), svcRGManagerSecret)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s item = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "PUT, DELETE" {
			t.Fatalf("%s item Allow = %q, want \"PUT, DELETE\"", method, allow)
		}
	}
	rec := f.do(http.MethodPut, svcRGPath("/"+svcRGReachID+"/extra"), svcRGManagerSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT with extra segment = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "service.not_found" {
		t.Fatalf("extra-segment error code = %q, want service.not_found", code)
	}
}
