// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These endpoint tests cover the HTTP layer of the server-owner self-service
// resource-group path (spec 2026-08-11-resource-groups-server-owner-self-service):
// routing dispatch (GET/PUT/DELETE), response shapes, the no-leak 404 mapping, and
// method handling. The grant logic (eligibility, member flag, same-system-group)
// is unit-tested at the service layer (portal.service_resource_group_owner_test.go)
// and end-to-end in e2e:resource-groups; newPerfTestServer wires a portal Service
// with NO groups store, so no admin-group membership exists here (every list is
// empty and every join is ineligible) -- exactly what these routing/mapping tests
// need.

func rgOwnerRequest(t *testing.T, method, path, secret string) *httptest.ResponseRecorder {
	t.Helper()
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// GET as the server owner -> 200 with a {data:[]} envelope (no eligible groups
// without a groups store), proving the GET route reaches ServerOwnerResourceGroups.
func TestServerOwnerResourceGroupsEndpointOwnerEmptyList(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodGet, "/api/portal/servers/"+perfServerID+"/resource-groups", perfOwnerSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		Data []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Member bool   `json:"member"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if len(dto.Data) != 0 {
		t.Fatalf("data = %+v, want empty", dto.Data)
	}
}

// GET as a non-owner -> 404 server.not_found (no existence leak).
func TestServerOwnerResourceGroupsEndpointNonOwner404(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodGet, "/api/portal/servers/"+perfServerID+"/resource-groups", perfOtherSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// PUT to an unknown resource group as the owner -> 404 resource_group.not_found
// (proves the PUT route reaches AddServerToResourceGroup and maps ErrResourceGroupNotFound).
func TestServerOwnerJoinEndpointUnknownGroup404(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodPut, "/api/portal/servers/"+perfServerID+"/resource-groups/rg_missing", perfOwnerSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.not_found" {
		t.Fatalf("error code = %q, want resource_group.not_found", code)
	}
}

// PUT as a non-owner -> 404 server.not_found (owner gate runs first, no leak).
func TestServerOwnerJoinEndpointNonOwner404(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodPut, "/api/portal/servers/"+perfServerID+"/resource-groups/rg_missing", perfOtherSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// DELETE to an unknown resource group as the owner -> 404 resource_group.not_found
// (proves the DELETE route reaches RemoveServerFromResourceGroup).
func TestServerOwnerLeaveEndpointUnknownGroup404(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodDelete, "/api/portal/servers/"+perfServerID+"/resource-groups/rg_missing", perfOwnerSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "resource_group.not_found" {
		t.Fatalf("error code = %q, want resource_group.not_found", code)
	}
}

// POST on the collection path -> 405 (only GET is allowed there).
func TestServerOwnerResourceGroupsEndpointCollectionMethodNotAllowed(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodPost, "/api/portal/servers/"+perfServerID+"/resource-groups", perfOwnerSecret)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
}

// PATCH on the item path -> 405 (only PUT/DELETE are allowed there).
func TestServerOwnerResourceGroupsEndpointItemMethodNotAllowed(t *testing.T) {
	rec := rgOwnerRequest(t, http.MethodPatch, "/api/portal/servers/"+perfServerID+"/resource-groups/rg_x", perfOwnerSecret)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
}
