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
	"strings"
	"testing"
	"time"
)

// These endpoint tests cover the HTTP layer of the resource-group
// PROVISIONING management path (Resource Groups Phase 2, Task 5, spec
// 2026-08-12-resource-groups-phase-2-provisioning): GET/PUT
// "/{id}/provisions" routing dispatch, response shapes, the 400
// resource_group.provision_target_invalid mapping, the 404-no-leak
// mapping, method handling, and the GET "/{id}/provision-candidates"
// combined picker. The validation/candidate SCOPING logic itself (which
// targets a NON-system caller may see/provision-for, across all four
// kinds) is unit-tested at the service layer
// (portal.service_resource_group_provisions_test.go) against a richer
// group/service fixture; this file only needs a system-scope principal
// (bypasses authorizeResourceGroup's group-scoping branch entirely, per
// authorizeResourceGroup's own early return) plus a plain "stranger" token
// tied to nothing, which is enough to exercise every route/status-code/
// error-mapping path below.

const (
	rgProvSystemSecret   = "rgprov-system-secret"
	rgProvStrangerSecret = "rgprov-stranger-secret"
	rgProvGroupID        = "rgrp_prov_test"
)

// newResourceGroupProvisionsTestServer builds a *Server with a system-scope
// token (usr_prov_sys) and a plain "stranger" token (usr_prov_stranger, no
// group/ownership ties at all) plus ONE resource group (rgProvGroupID,
// deliberately left UNLINKED to any admin group -- authorizeResourceGroup
// denies every non-system caller for such a group regardless of s.groups'
// nilness, mirroring newBenchmarkActiveFixture's "no Groups store needed"
// shape). A Groups store IS wired (portal.MemoryDirectory implements both
// Users and Groups) because ListGroups -- called by
// resourceGroupProvisionVisibleTargets for EVERY principal, including
// system-scope -- unconditionally calls s.groups.ListUserGroupsByTier; with
// no groups ever created, every tier reads back empty, which is exactly the
// "no candidates beyond self" baseline these tests exercise.
func newResourceGroupProvisionsTestServer(t *testing.T) (*Server, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_prov_sys", Email: "sys@example.test", DisplayName: "System", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_prov_stranger", Email: "stranger@example.test", DisplayName: "Stranger", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_prov_sys", UserID: "usr_prov_sys", Name: "System Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, rgProvSystemSecret); err != nil {
		t.Fatalf("CreatePlainToken system: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_prov_stranger", UserID: "usr_prov_stranger", Name: "Stranger Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, rgProvStrangerSecret); err != nil {
		t.Fatalf("CreatePlainToken stranger: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateResourceGroup(context.Background(), routing.ResourceGroup{ID: rgProvGroupID, Name: "RG Prov", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Groups: dir, Usage: recorder, Routes: routeStore})
	srv := New(ServerDeps{
		Tokens: tokens,
		Usage:  recorder,
		Routes: routeStore,
		Portal: svc,
	})
	return srv, routeStore
}

func rgProvRequest(t *testing.T, srv *Server, method, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

type provisionsListResponse struct {
	Data []struct {
		Kind       string `json:"kind"`
		TargetID   string `json:"target_id"`
		TargetName string `json:"target_name"`
	} `json:"data"`
}

// GET as a system-scope manager on a fresh (never-provisioned) resource
// group -> 200 with an EMPTY {data:[]} envelope (proves the route reaches
// ResourceGroupProvisionsView, and that the service returns a non-nil
// empty slice rather than JSON null).
func TestResourceGroupProvisionsEndpointManagerEmptyList(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	rec := rgProvRequest(t, srv, http.MethodGet, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvSystemSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var out provisionsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if out.Data == nil || len(out.Data) != 0 {
		t.Fatalf("data = %+v, want non-nil empty slice", out.Data)
	}
}

// GET as a stranger with no admin-group tie to the (unlinked) resource
// group -> 404 resource_group.not_found (no existence leak -- a stranger
// and an unknown id read identically).
func TestResourceGroupProvisionsEndpointNonManagerNoLeak(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	rec := rgProvRequest(t, srv, http.MethodGet, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvStrangerSecret, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.not_found" {
		t.Fatalf("error code = %q, want resource_group.not_found", code)
	}
}

// PUT with a target the caller cannot see (a nonexistent user id -- even a
// system-scope caller's VisibleUserIDs only contains REAL users) -> 400
// resource_group.provision_target_invalid, AND the store is left
// UNCHANGED (a subsequent GET still reads back empty) -- proves the
// all-or-nothing validate-before-write ordering.
func TestResourceGroupProvisionsEndpointPutInvalidTargetRejected(t *testing.T) {
	srv, routeStore := newResourceGroupProvisionsTestServer(t)
	body := `{"provisions":[{"kind":"user","target_id":"usr_ghost_does_not_exist"}]}`
	rec := rgProvRequest(t, srv, http.MethodPut, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvSystemSecret, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.provision_target_invalid" {
		t.Fatalf("error code = %q, want resource_group.provision_target_invalid", code)
	}
	got, err := routeStore.ResourceGroupProvisions(context.Background(), rgProvGroupID)
	if err != nil {
		t.Fatalf("ResourceGroupProvisions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("store mutated on a rejected PUT: %+v, want unchanged/empty", got)
	}
}

// PUT with a valid target (the system-scope caller's OWN user id -- always
// a member of its own VisibleUserIDs) -> 200 {ok:true}, and a follow-up GET
// round-trips exactly that one entry with its resolved display name.
func TestResourceGroupProvisionsEndpointPutValidRoundTrip(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	body := `{"provisions":[{"kind":"user","target_id":"usr_prov_sys"}]}`
	putRec := rgProvRequest(t, srv, http.MethodPut, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvSystemSecret, body)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body = %s", putRec.Code, putRec.Body.String())
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(putRec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("unmarshal PUT response: %v (%s)", err, putRec.Body.String())
	}
	if !ok.OK {
		t.Fatalf("PUT response ok = false, want true (%s)", putRec.Body.String())
	}

	getRec := rgProvRequest(t, srv, http.MethodGet, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvSystemSecret, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body = %s", getRec.Code, getRec.Body.String())
	}
	var out provisionsListResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal GET response: %v (%s)", err, getRec.Body.String())
	}
	if len(out.Data) != 1 {
		t.Fatalf("data = %+v, want exactly 1 entry", out.Data)
	}
	got := out.Data[0]
	if got.Kind != "user" || got.TargetID != "usr_prov_sys" || got.TargetName != "System" {
		t.Fatalf("round-tripped entry = %+v, want {kind:user, target_id:usr_prov_sys, target_name:System}", got)
	}
}

// A method the route doesn't support (POST on /provisions) -> 405 with an
// Allow header listing GET+PUT.
func TestResourceGroupProvisionsEndpointMethodNotAllowed(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	rec := rgProvRequest(t, srv, http.MethodPost, "/api/portal/resource-groups/"+rgProvGroupID+"/provisions", rgProvSystemSecret, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodPut) {
		t.Fatalf("Allow header = %q, want to contain GET and PUT", allow)
	}
}

// GET the combined candidates endpoint as the system-scope manager -> 200,
// with a non-nil "users" list containing every real user (system scope
// bypasses group-scoping in VisibleUserIDs), and non-nil-but-empty group/
// service lists (nothing else exists in this fixture).
func TestResourceGroupProvisionCandidatesEndpointManager(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	rec := rgProvRequest(t, srv, http.MethodGet, "/api/portal/resource-groups/"+rgProvGroupID+"/provision-candidates", rgProvSystemSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Users []struct {
			ID string `json:"id"`
		} `json:"users"`
		UserGroups  []any `json:"user_groups"`
		AdminGroups []any `json:"admin_groups"`
		Services    []any `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if out.UserGroups == nil || out.AdminGroups == nil || out.Services == nil {
		t.Fatalf("candidate lists = %+v, want non-nil empty slices", out)
	}
	found := map[string]bool{}
	for _, u := range out.Users {
		found[u.ID] = true
	}
	if !found["usr_prov_sys"] || !found["usr_prov_stranger"] {
		t.Fatalf("users = %+v, want both usr_prov_sys and usr_prov_stranger (system scope sees every user)", out.Users)
	}
}

// GET the combined candidates endpoint as a stranger with no tie to the
// (unlinked) resource group -> 404 resource_group.not_found (the candidate
// picker is gated by the SAME authorizeResourceGroup choke-point as the
// provisions route itself -- no separate leak surface).
func TestResourceGroupProvisionCandidatesEndpointNonManagerNoLeak(t *testing.T) {
	srv, _ := newResourceGroupProvisionsTestServer(t)
	rec := rgProvRequest(t, srv, http.MethodGet, "/api/portal/resource-groups/"+rgProvGroupID+"/provision-candidates", rgProvStrangerSecret, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.not_found" {
		t.Fatalf("error code = %q, want resource_group.not_found", code)
	}
}
