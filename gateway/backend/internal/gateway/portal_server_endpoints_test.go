// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlePortalServersInvalidJSONReturns400 proves POST /api/portal/servers
// maps a body that decodes as valid JSON but fails to unmarshal into
// portal.CreateServerRequest (a JSON array where an object is expected) to the
// handler's own 400 request.invalid_json branch, not the generic
// readRawJSON/writeJSONDecodeError path (which a syntactically-broken body
// would hit instead, one line earlier in the call chain).
func TestHandlePortalServersInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalServerItemPatchInvalidJSONReturns400 mirrors the create-path
// check for PATCH /api/portal/servers/{id}: the body must fail
// portal.UpdateServerRequest's own json.Unmarshal, not the earlier raw-body read.
func TestHandlePortalServerItemPatchInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/servers/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalServerItemPatchUnknownServerReturns404 proves a PATCH with
// well-formed JSON against an id that does not exist maps through
// writePortalServerError's store.ErrNotFound row (authorizeServer ->
// ErrServerNotFound) to 404 server.not_found, for any principal (system scope
// is not required to observe the not-found — a plain admin-scoped bearer
// token that owns nothing gets the identical no-leak response).
func TestHandlePortalServerItemPatchUnknownServerReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/servers/srv_does_not_exist", `{}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerItemEmptyIDReturns404 proves a request against the
// collection path with a trailing slash and no id segment (no sub-resource
// match either) falls through to the bare 404 server.not_found at the bottom
// of handlePortalServerItem, distinct from the id-present dispatch below it.
func TestHandlePortalServerItemEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerItemMethodNotAllowedTable covers the four
// method-not-allowed default branches inside portal_server_endpoints.go that
// carry new, previously-unexercised lines: the servers collection, a single
// server item, a server's applications sub-collection, and its agent-token
// sub-resource. Each asserts both the exact status/code AND the Allow header
// the handler sets right before writing the response.
func TestHandlePortalServerItemMethodNotAllowedTable(t *testing.T) {
	srv := NewTestServer()
	cases := []struct {
		name      string
		method    string
		path      string
		wantAllow string
	}{
		{"servers collection PUT", http.MethodPut, "/api/portal/servers", "GET, POST"},
		{"server item POST", http.MethodPost, "/api/portal/servers/any-id", "GET, PATCH, DELETE"},
		{"server applications PUT", http.MethodPut, "/api/portal/servers/any-id/applications", "GET, POST"},
		{"server agent-token PATCH", http.MethodPatch, "/api/portal/servers/any-id/agent-token", "GET, POST, DELETE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(tc.method, tc.path, ""))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tc.wantAllow {
				t.Fatalf("Allow header = %q, want %q", got, tc.wantAllow)
			}
			if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
				t.Fatalf("error code = %q, want request.method_not_allowed", code)
			}
		})
	}
}

// TestHandlePortalServerItemPerfUnknownSubPathReturns404 proves a
// .../perf/{suffix} path with a suffix other than "events" (and not the
// bare .../perf history route) falls through to the dedicated 404
// server.not_found inside the "perf" sub-path branch.
func TestHandlePortalServerItemPerfUnknownSubPathReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/any-id/perf/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerItemBenchmarkStatusUnknownServerReturns404 proves the
// benchmark status read (GET .../benchmark/status) is gated on server
// ownership via a GetServer call BEFORE reading Benchmarks.Status, so an
// unknown/unowned id maps through writePortalServerError to the same 404
// server.not_found a stranger would see for any other server route (no
// existence leak via the benchmark status side-channel).
func TestHandlePortalServerItemBenchmarkStatusUnknownServerReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/srv_does_not_exist/benchmark/status", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerItemBenchmarkUnknownSubPathReturns404 proves a
// .../benchmark/{suffix} path with a suffix other than "status"/"events"
// falls through to the benchmark branch's own 404 server.not_found.
func TestHandlePortalServerItemBenchmarkUnknownSubPathReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/any-id/benchmark/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerNetbirdSetupKeyUnknownServerReturns404 proves
// POST .../netbird/setup-key runs the authorizeServer choke-point (via
// RegenerateNetbirdKey) before ever consulting the NetBird module config, so
// an unknown server id 404s the same way regardless of whether NetBird is
// configured in this test server (it is not).
func TestHandlePortalServerNetbirdSetupKeyUnknownServerReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/srv_does_not_exist/netbird/setup-key", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerEnergyInvalidJSONReturns400 proves PUT
// .../energy validates its anonymous request body struct the same way as the
// server create/update paths: a JSON array fails to unmarshal into it.
func TestHandlePortalServerEnergyInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/any-id/energy", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalServerEnergyNegativeValueReturns400 proves the energy
// endpoint's own validation (ErrServerEnergyConfigInvalid, portalServerErrRows)
// rejects a negative wattage on a server the caller genuinely owns/manages —
// distinguishing the real SetServerEnergyConfig validation-failure branch
// (writePortalServerError, line 308) from the earlier not-found/invalid-JSON
// ones, using the real ownership graph (newServerOverrideGatewayEnv).
func TestHandlePortalServerEnergyNegativeValueReturns400(t *testing.T) {
	env := newServerOverrideGatewayEnv(t)
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/"+env.srvHealthy+"/energy",
		`{"estimated_watts":-1,"idle_watts":0,"price_per_kwh":0,"pue":1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.energy_config_invalid" {
		t.Fatalf("error code = %q, want server.energy_config_invalid", code)
	}
}

// TestHandlePortalServerAdminGroupsInvalidJSONReturns400 proves PUT
// .../admin-groups validates its own anonymous request body struct.
func TestHandlePortalServerAdminGroupsInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/any-id/admin-groups", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalServerAdminGroupsUnknownServerReturns404 proves
// SetServerAdminGroups runs the authorizeServer choke-point before touching
// admin-group validation, so an unknown server id 404s even with a non-empty
// admin_group_ids payload.
func TestHandlePortalServerAdminGroupsUnknownServerReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPut, "/api/portal/servers/srv_does_not_exist/admin-groups", `{"admin_group_ids":["ugrp_x"]}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// TestHandlePortalServerApplicationsInvalidJSONReturns400 proves POST
// .../applications validates portal.CreateApplicationRequest before ever
// calling into CreateApplication (so an unknown serverID in the path does not
// matter for this branch).
func TestHandlePortalServerApplicationsInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/any-id/applications", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandleServerAdminGroupCandidatesReturnsSeededAdminGroup proves the
// standalone GET /api/portal/server-admin-group-candidates endpoint is wired
// end to end for a system-scoped caller: it returns every admin-tier group
// (ServerAdminGroupCandidates's isSystem branch), including the one
// NewTestServerWithGroups seeds (testAdminGroupID) with its real parent-name
// enrichment.
func TestHandleServerAdminGroupCandidatesReturnsSeededAdminGroup(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin", "system"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/server-admin-group-candidates", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			ParentGroupName string `json:"parent_group_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	var found bool
	for _, g := range body.Data {
		if g.ID == testAdminGroupID {
			found = true
			if g.Name != "GW Test Admin" {
				t.Fatalf("group name = %q, want %q", g.Name, "GW Test Admin")
			}
			if g.ParentGroupName != "GW Test System" {
				t.Fatalf("parent group name = %q, want %q", g.ParentGroupName, "GW Test System")
			}
		}
	}
	if !found {
		t.Fatalf("seeded admin group %q not in candidates: %+v", testAdminGroupID, body.Data)
	}
}
