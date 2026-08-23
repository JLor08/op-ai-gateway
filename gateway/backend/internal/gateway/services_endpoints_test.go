// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
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

// Task 4 (spec §7): the HTTP surface for services + service tokens. All 9
// routes sit behind requireWebScope(gateway:use); the OBJECT-level
// authorization (admin / Full-Delegate / Token-Delegate, spec §6.1) happens
// inside the portal.Service methods Task 3 built — these tests exercise the
// wiring + the writePortalServiceError mapping, not the authorization logic
// itself (that is covered by internal/portal/service_services_test.go).

const (
	svcAdminSecret    = "svc-admin-secret"
	svcFullSecret     = "svc-full-secret"     // Full-Delegate: can_manage_settings=true
	svcTokenSecret    = "svc-token-secret"    // Token-Delegate: can_manage_settings=false
	svcStrangerSecret = "svc-stranger-secret" // not a delegate at all
)

// svcTestAdminGroupID is the fixed id of the admin-tier group
// newServicesTestFixture seeds, owned by + membered by usr_svc_admin --
// Phase C (spec 2026-08-10) requires CreateService's admin_group_ids to
// reference an existing admin-tier group; svcAdminSecret's principal is
// system-scoped so it doesn't strictly NEED to manage it, but every literal
// request body below references it anyway for realism (mirrors
// testAdminGroupID/NewTestServerWithGroups, server_test.go).
const svcTestAdminGroupID = "ugrp_svcgwtest_admin"

// newServicesTestFixture builds a *Server with four bearer-token principals —
// an admin, a Full-Delegate, a Token-Delegate, and an unrelated stranger —
// mirroring newBenchmarkActiveFixture's construction (benchmark_active_test.go).
// No service exists yet; each test creates what it needs via the admin token.
func newServicesTestFixture(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_svc_admin", Email: "admin@example.test", DisplayName: "Admin", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_svc_full", Email: "full@example.test", DisplayName: "Full Delegate", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_svc_tok", Email: "tok@example.test", DisplayName: "Token Delegate", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_svc_stranger", Email: "stranger@example.test", DisplayName: "Stranger", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	mustToken := func(id, userID, secret string, scopes string) {
		if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: id, UserID: userID, Name: id, Status: store.TokenStatusActive, Scopes: scopes, CreatedAt: now, UpdatedAt: now}, secret); err != nil {
			t.Fatalf("CreatePlainToken(%s): %v", id, err)
		}
	}
	// "system" is added alongside "admin" so this test principal mirrors an
	// ELEVATED system_admin session (sessionPrincipal composes both scopes
	// together for role==system_admin && elevated -- internal/gateway/auth.go)
	// rather than a raw API bearer token (which validateTokenScopes forbids
	// from ever carrying "system"). Needed since the admin-group permissions
	// Phase C rewrite (internal/portal/service_services.go) removed the
	// global HasScope("admin") bypass from authorizeServiceRead/Settings/
	// ListServices in favor of HasScope("system") + delegate/group-manager
	// checks -- mirrors the analogous "system" addition already applied to
	// NewTestServerWithTokenScopes for the Phase B server-authorization
	// rewrite (see server_test.go).
	mustToken("tok_svc_admin", "usr_svc_admin", svcAdminSecret, `["gateway:use","admin","system"]`)
	mustToken("tok_svc_full", "usr_svc_full", svcFullSecret, `["gateway:use"]`)
	mustToken("tok_svc_tok", "usr_svc_tok", svcTokenSecret, `["gateway:use"]`)
	mustToken("tok_svc_stranger", "usr_svc_stranger", svcStrangerSecret, `["gateway:use"]`)

	// Admin-group-linkage seed (Phase C, spec 2026-08-10): CreateService now
	// requires admin_group_ids to reference a REAL, existing admin-tier
	// group -- see svcTestAdminGroupID.
	sysGroupID := "ugrp_svcgwtest_sys"
	if err := dir.CreateUserGroup(context.Background(), store.UserGroup{ID: sysGroupID, Tier: store.GroupTierSystem, Name: "SVC GW Test System", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed system group: %v", err)
	}
	if err := dir.CreateUserGroup(context.Background(), store.UserGroup{ID: svcTestAdminGroupID, Tier: store.GroupTierAdmin, Name: "SVC GW Test Admin", ParentGroupID: sysGroupID, OwnerUserID: "usr_svc_admin", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed admin group: %v", err)
	}
	if err := dir.SetUserGroupMember(context.Background(), svcTestAdminGroupID, "usr_svc_admin", store.GroupStateMember, ""); err != nil {
		t.Fatalf("seed admin group owner membership: %v", err)
	}

	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Groups: dir, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }})
	return New(ServerDeps{
		Tokens: tokens,
		Usage:  recorder,
		Routes: routeStore,
		Portal: svc,
	})
}

func svcAuthedRequest(method, path, body, secret string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// createServiceWithDelegates creates a service (as the admin) with usr_svc_full
// as a Full-Delegate and usr_svc_tok as a Token-Delegate, returning the new
// service id. Fails the test on any non-201 response.
func createServiceWithDelegates(t *testing.T, s *Server) string {
	t.Helper()
	body := `{"name":"Nightly Batch","description":"batch jobs","delegates":[` +
		`{"user_id":"usr_svc_full","can_manage_settings":true},` +
		`{"user_id":"usr_svc_tok","can_manage_settings":false}],` +
		`"admin_group_ids":["` + svcTestAdminGroupID + `"]}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services", body, svcAdminSecret))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created service: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created service has empty id: %s", rec.Body.String())
	}
	return created.ID
}

func TestServicesEndpointsCreateListGetUpdateDeleteLifecycle(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)

	// GET /api/portal/services (admin) -> {data:[...]} incl. the new service.
	listRec := httptest.NewRecorder()
	s.ServeHTTP(listRec, svcAuthedRequest(http.MethodGet, "/api/portal/services", "", svcAdminSecret))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, item := range list.Data {
		if item.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("created service %s not in admin list: %#v", id, list.Data)
	}

	// GET /api/portal/services/{id} (admin) -> 200.
	getRec := httptest.NewRecorder()
	s.ServeHTTP(getRec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id, "", svcAdminSecret))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var got struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if got.Name != "Nightly Batch" {
		t.Fatalf("name = %q, want %q", got.Name, "Nightly Batch")
	}

	// PUT /api/portal/services/{id} (admin, Settings gate) -> 200, name changes.
	putRec := httptest.NewRecorder()
	s.ServeHTTP(putRec, svcAuthedRequest(http.MethodPut, "/api/portal/services/"+id, `{"name":"Nightly Batch v2"}`, svcAdminSecret))
	if putRec.Code != http.StatusOK {
		t.Fatalf("put status = %d, body = %s", putRec.Code, putRec.Body.String())
	}
	var updated struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(putRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal put: %v", err)
	}
	if updated.Name != "Nightly Batch v2" {
		t.Fatalf("updated name = %q, want %q", updated.Name, "Nightly Batch v2")
	}

	// DELETE /api/portal/services/{id} (admin, Settings gate) -> 200 {ok:true}.
	delRec := httptest.NewRecorder()
	s.ServeHTTP(delRec, svcAuthedRequest(http.MethodDelete, "/api/portal/services/"+id, "", svcAdminSecret))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
	var delBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(delRec.Body.Bytes(), &delBody); err != nil {
		t.Fatalf("unmarshal delete: %v", err)
	}
	if !delBody.OK {
		t.Fatalf("delete body = %#v", delBody)
	}

	// GET after delete -> 404 service.not_found.
	afterRec := httptest.NewRecorder()
	s.ServeHTTP(afterRec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id, "", svcAdminSecret))
	if afterRec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404, body = %s", afterRec.Code, afterRec.Body.String())
	}
	assertErrorCode(t, afterRec, "service.not_found")
}

func TestServicesEndpointsCreateRequiresAdmin(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services", `{"name":"X"}`, svcFullSecret))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service.forbidden")
}

func TestServicesEndpointsCreateValidationErrorMapsTo400(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	// An empty name fails CreateService's validation (ErrServiceValidation).
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services", `{"name":""}`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service.validation_failed")
}

func TestServicesEndpointsInvalidJSONReturns400(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodPost, "/api/portal/services", `{not json`, svcAdminSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "request.invalid_json")
}

// TestServicesEndpointsGatePropagation proves the three object-gates (§6.1)
// are actually wired end to end through the HTTP layer, not just reachable
// at the portal.Service layer:
//   - a stranger (not a delegate at all) gets 404 on GET (the Read gate,
//     admin-or-any-delegate) -- no existence leak, same as an unknown id;
//   - a Token-Delegate gets 404 on PUT (the Settings gate needs a
//     Full-Delegate) even though they ARE a delegate;
//   - a Token-Delegate DOES get through the Tokens gate (list/create/rotate/
//     delete tokens) since that gate is admin-or-ANY-delegate;
//   - a Full-Delegate gets through both Read/Settings and Tokens.
func TestServicesEndpointsGatePropagation(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)

	// Stranger: GET -> 404 (Read gate; no existence leak).
	strangerGet := httptest.NewRecorder()
	s.ServeHTTP(strangerGet, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id, "", svcStrangerSecret))
	if strangerGet.Code != http.StatusNotFound {
		t.Fatalf("stranger GET status = %d, want 404, body = %s", strangerGet.Code, strangerGet.Body.String())
	}
	assertErrorCode(t, strangerGet, "service.not_found")

	// Stranger: GET tokens -> 404 (Tokens gate; still admin-or-delegate).
	strangerTokens := httptest.NewRecorder()
	s.ServeHTTP(strangerTokens, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/tokens", "", svcStrangerSecret))
	if strangerTokens.Code != http.StatusNotFound {
		t.Fatalf("stranger tokens-list status = %d, want 404, body = %s", strangerTokens.Code, strangerTokens.Body.String())
	}

	// Token-Delegate: GET (Read) -> 200 (any delegate tier reads).
	tokGet := httptest.NewRecorder()
	s.ServeHTTP(tokGet, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id, "", svcTokenSecret))
	if tokGet.Code != http.StatusOK {
		t.Fatalf("token-delegate GET status = %d, want 200, body = %s", tokGet.Code, tokGet.Body.String())
	}

	// Token-Delegate: PUT (Settings) -> 404 (needs Full-Delegate).
	tokPut := httptest.NewRecorder()
	s.ServeHTTP(tokPut, svcAuthedRequest(http.MethodPut, "/api/portal/services/"+id, `{"name":"Renamed"}`, svcTokenSecret))
	if tokPut.Code != http.StatusNotFound {
		t.Fatalf("token-delegate PUT status = %d, want 404, body = %s", tokPut.Code, tokPut.Body.String())
	}
	assertErrorCode(t, tokPut, "service.not_found")

	// Token-Delegate: DELETE service (Settings) -> 404 too.
	tokDelete := httptest.NewRecorder()
	s.ServeHTTP(tokDelete, svcAuthedRequest(http.MethodDelete, "/api/portal/services/"+id, "", svcTokenSecret))
	if tokDelete.Code != http.StatusNotFound {
		t.Fatalf("token-delegate DELETE service status = %d, want 404, body = %s", tokDelete.Code, tokDelete.Body.String())
	}

	// Token-Delegate: POST tokens (Tokens gate) -> 201 (any delegate tier
	// manages tokens).
	tokCreate := httptest.NewRecorder()
	s.ServeHTTP(tokCreate, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", `{"name":"batch-runner"}`, svcTokenSecret))
	if tokCreate.Code != http.StatusCreated {
		t.Fatalf("token-delegate token-create status = %d, want 201, body = %s", tokCreate.Code, tokCreate.Body.String())
	}

	// Full-Delegate: PUT (Settings) -> 200 (Full-Delegate manages settings).
	fullPut := httptest.NewRecorder()
	s.ServeHTTP(fullPut, svcAuthedRequest(http.MethodPut, "/api/portal/services/"+id, `{"name":"Renamed By Full"}`, svcFullSecret))
	if fullPut.Code != http.StatusOK {
		t.Fatalf("full-delegate PUT status = %d, want 200, body = %s", fullPut.Code, fullPut.Body.String())
	}
}

func TestServicesEndpointsTokenCreateAndRotateReturnSecretOnce(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)

	createRec := httptest.NewRecorder()
	s.ServeHTTP(createRec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", `{"name":"first-token"}`, svcAdminSecret))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, want 201, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
		Token  struct {
			ID     string `json:"id"`
			Secret bool   `json:"secret"`
		} `json:"token"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.Secret == "" {
		t.Fatalf("create response carries no plaintext secret: %s", createRec.Body.String())
	}
	if created.Token.ID == "" {
		t.Fatalf("create response carries no token id: %s", createRec.Body.String())
	}

	// GET /tokens (list) -> {data:[...]} incl. the new token, NEVER the secret.
	listRec := httptest.NewRecorder()
	s.ServeHTTP(listRec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/tokens", "", svcAdminSecret))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list tokens status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	if !json.Valid(listRec.Body.Bytes()) {
		t.Fatalf("list tokens body is not valid JSON: %s", listRec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal token list: %v", err)
	}
	foundToken := false
	for _, tk := range list.Data {
		if tk.ID == created.Token.ID {
			foundToken = true
		}
	}
	if !foundToken {
		t.Fatalf("created token %s not in list: %#v", created.Token.ID, list.Data)
	}
	if !bytesContainNoSecret(listRec.Body.Bytes(), created.Secret) {
		t.Fatalf("token list body leaks the plaintext secret: %s", listRec.Body.String())
	}

	// POST /tokens/{tid}/rotate -> 200 + a FRESH plaintext secret.
	rotateRec := httptest.NewRecorder()
	s.ServeHTTP(rotateRec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens/"+created.Token.ID+"/rotate", "", svcAdminSecret))
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("unmarshal rotate: %v", err)
	}
	if rotated.Secret == "" {
		t.Fatalf("rotate response carries no plaintext secret: %s", rotateRec.Body.String())
	}
	if rotated.Secret == created.Secret {
		t.Fatalf("rotate returned the SAME secret as create: %q", rotated.Secret)
	}

	// DELETE /tokens/{tid} -> 200 {ok:true}; then GET /tokens no longer lists it.
	deleteRec := httptest.NewRecorder()
	s.ServeHTTP(deleteRec, svcAuthedRequest(http.MethodDelete, "/api/portal/services/"+id+"/tokens/"+created.Token.ID, "", svcAdminSecret))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete token status = %d, body = %s", deleteRec.Code, deleteRec.Body.String())
	}
	afterRec := httptest.NewRecorder()
	s.ServeHTTP(afterRec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/tokens", "", svcAdminSecret))
	var afterList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(afterRec.Body.Bytes(), &afterList); err != nil {
		t.Fatalf("unmarshal token list after delete: %v", err)
	}
	for _, tk := range afterList.Data {
		if tk.ID == created.Token.ID {
			t.Fatalf("deleted token %s still listed: %#v", created.Token.ID, afterList.Data)
		}
	}
}

// TestServicesEndpointsCrossServiceTokenAccessDenied proves a token belonging
// to service A cannot be rotated/deleted via service B's path (spec §6.3 "kein
// Cross-Service-Zugriff") -- the portal layer's serviceTokenByID guard, surfaced
// here as a 404 via writePortalServiceError.
func TestServicesEndpointsCrossServiceTokenAccessDenied(t *testing.T) {
	s := newServicesTestFixture(t)
	idA := createServiceWithDelegates(t, s)

	// A second, unrelated service (also admin-owned, no delegates needed).
	createB := httptest.NewRecorder()
	s.ServeHTTP(createB, svcAuthedRequest(http.MethodPost, "/api/portal/services", `{"name":"Other Service","admin_group_ids":["`+svcTestAdminGroupID+`"]}`, svcAdminSecret))
	if createB.Code != http.StatusCreated {
		t.Fatalf("create service B status = %d, body = %s", createB.Code, createB.Body.String())
	}
	var svcB struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createB.Body.Bytes(), &svcB); err != nil {
		t.Fatalf("unmarshal service B: %v", err)
	}

	tokenRec := httptest.NewRecorder()
	s.ServeHTTP(tokenRec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+idA+"/tokens", `{"name":"a-token"}`, svcAdminSecret))
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
	}
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created token: %v", err)
	}

	// Rotate service A's token via service B's path -> 404 (no cross-service).
	crossRotate := httptest.NewRecorder()
	s.ServeHTTP(crossRotate, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+svcB.ID+"/tokens/"+created.Token.ID+"/rotate", "", svcAdminSecret))
	if crossRotate.Code != http.StatusNotFound {
		t.Fatalf("cross-service rotate status = %d, want 404, body = %s", crossRotate.Code, crossRotate.Body.String())
	}

	// Delete service A's token via service B's path -> 404 (no cross-service).
	crossDelete := httptest.NewRecorder()
	s.ServeHTTP(crossDelete, svcAuthedRequest(http.MethodDelete, "/api/portal/services/"+svcB.ID+"/tokens/"+created.Token.ID, "", svcAdminSecret))
	if crossDelete.Code != http.StatusNotFound {
		t.Fatalf("cross-service delete status = %d, want 404, body = %s", crossDelete.Code, crossDelete.Body.String())
	}
}

func TestServicesEndpointsUnknownServiceReturns404(t *testing.T) {
	s := newServicesTestFixture(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/services/svc_missing", "", svcAdminSecret))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service.not_found")
}

func TestServicesEndpointsUnknownSubPathReturns404(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/bogus", "", svcAdminSecret))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServicesEndpointsMethodNotAllowed(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)

	cases := []struct {
		name      string
		method    string
		path      string
		wantAllow string
	}{
		{"collection PUT", http.MethodPut, "/api/portal/services", "GET, POST"},
		{"single POST", http.MethodPost, "/api/portal/services/" + id, "GET, PUT, DELETE"},
		{"tokens collection PUT", http.MethodPut, "/api/portal/services/" + id + "/tokens", "GET, POST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, svcAuthedRequest(tc.method, tc.path, "", svcAdminSecret))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tc.wantAllow {
				t.Fatalf("Allow header = %q, want %q", got, tc.wantAllow)
			}
			assertErrorCode(t, rec, "request.method_not_allowed")
		})
	}
}

func TestServicesEndpointsTokenRotateOnlyAllowsPost(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	createRec := httptest.NewRecorder()
	s.ServeHTTP(createRec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", `{"name":"t"}`, svcAdminSecret))
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/tokens/"+created.Token.ID+"/rotate", "", svcAdminSecret))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow header = %q, want %q", got, http.MethodPost)
	}
}

func TestServicesEndpointsTokenSingleOnlyAllowsDelete(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	createRec := httptest.NewRecorder()
	s.ServeHTTP(createRec, svcAuthedRequest(http.MethodPost, "/api/portal/services/"+id+"/tokens", `{"name":"t"}`, svcAdminSecret))
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, svcAuthedRequest(http.MethodGet, "/api/portal/services/"+id+"/tokens/"+created.Token.ID, "", svcAdminSecret))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != http.MethodDelete {
		t.Fatalf("Allow header = %q, want %q", got, http.MethodDelete)
	}
}

func TestServicesEndpointsAllRequireAuth(t *testing.T) {
	s := newServicesTestFixture(t)
	id := createServiceWithDelegates(t, s)
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/portal/services"},
		{http.MethodPost, "/api/portal/services"},
		{http.MethodGet, "/api/portal/services/" + id},
		{http.MethodPut, "/api/portal/services/" + id},
		{http.MethodDelete, "/api/portal/services/" + id},
		{http.MethodGet, "/api/portal/services/" + id + "/tokens"},
		{http.MethodPost, "/api/portal/services/" + id + "/tokens"},
		{http.MethodPost, "/api/portal/services/" + id + "/tokens/tok_missing/rotate"},
		{http.MethodDelete, "/api/portal/services/" + id + "/tokens/tok_missing"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil) // no Authorization header
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
			t.Fatalf("%s %s unauthenticated status = %d, want a rejection, body = %s", p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v (body=%s)", err, rec.Body.String())
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %q, want %q (body=%s)", body.Error.Code, want, rec.Body.String())
	}
}

func bytesContainNoSecret(body []byte, secret string) bool {
	if secret == "" {
		return true
	}
	return !strings.Contains(string(body), secret)
}
