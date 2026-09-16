// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// seedKeylessAPITokenServer creates a mock AI server owned by the NewTestServer
// dev user, so an application can be POSTed/PATCHed onto it. NewTestServer's
// portal.Service is built with no Cipher and SettingsVolatile false (the
// ServiceDeps zero value) -- the exact "disk store, no key" shape
// capture.SealSecret fails closed on for any non-empty api_token.
func seedKeylessAPITokenServer(t *testing.T, srv *Server, serverID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: serverID, Name: serverID, Domain: serverID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + serverID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.SetServerOwners(ctx, serverID, []string{"usr_dev"}); err != nil {
		t.Fatalf("set owners: %v", err)
	}
}

// This file targets portal_application_endpoints.go's request-shape/
// dispatch branches carrying NEW uncovered lines: the empty-appID 404, the
// mappings sub-route's own json.Unmarshal failure and method-not-allowed
// default, the unknown-subpath 404, and the item route's PATCH-invalid-JSON
// and method-not-allowed branches. None need a REAL application id — every
// check here runs before any portal.Service lookup.

func TestHandlePortalApplicationItemEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "application.not_found" {
		t.Fatalf("error code = %q, want application.not_found", code)
	}
}

func TestHandlePortalApplicationMappingsInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/any-id/mappings", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalApplicationMappingsMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/portal/applications/any-id/mappings", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, POST")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalApplicationItemUnknownSubPathReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/any-id/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "application.not_found" {
		t.Fatalf("error code = %q, want application.not_found", code)
	}
}

func TestHandlePortalApplicationItemPatchInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalApplicationItemMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/any-id", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, PATCH, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, PATCH, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

// TestHandlePortalApplicationCreateKeylessStoreAPITokenReturns400 pins the #85
// fix: an api_token on CREATE, on a keyless disk store, reaches
// CreateApplication's capture.SealSecret and returns capture.ErrKeyRequired,
// which must surface as 400 application.api_token_key_required -- the
// application-surface twin of the runtime-spec sentinel, not the mapper's 500
// application.request_failed fallback it fell through to before.
func TestHandlePortalApplicationCreateKeylessStoreAPITokenReturns400(t *testing.T) {
	srv := NewTestServer()
	seedKeylessAPITokenServer(t, srv, "srv_apptoken_create")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/srv_apptoken_create/applications",
		`{"type":"vllm","port":8300,"scheme":"http","api_token":"secret-token"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "application.api_token_key_required" {
		t.Fatalf("error code = %q, want application.api_token_key_required", code)
	}
}

// TestHandlePortalApplicationUpdateKeylessStoreAPITokenReturns400 pins the same
// fix on the UPDATE path: create an application with no token (which seals
// nothing and succeeds on a keyless store), then PATCH an api_token onto it --
// UpdateApplication's seal returns capture.ErrKeyRequired the same way, and must
// also surface as 400, not 500.
func TestHandlePortalApplicationUpdateKeylessStoreAPITokenReturns400(t *testing.T) {
	srv := NewTestServer()
	seedKeylessAPITokenServer(t, srv, "srv_apptoken_update")
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers/srv_apptoken_update/applications",
		`{"type":"vllm","port":8301,"scheme":"http"}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create body: %v, body = %s", err, createRec.Body.String())
	}
	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID,
		`{"api_token":"secret-token"}`))
	if patchRec.Code != http.StatusBadRequest {
		t.Fatalf("patch status = %d, want 400, body = %s", patchRec.Code, patchRec.Body.String())
	}
	if code := errorBodyOf(t, patchRec); code != "application.api_token_key_required" {
		t.Fatalf("error code = %q, want application.api_token_key_required", code)
	}
}
