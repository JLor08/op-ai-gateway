// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file targets portal_token_endpoints.go's request-shape/dispatch
// branches carrying NEW uncovered lines: the collection's own
// json.Unmarshal-into-typed-struct failure and method-not-allowed default,
// handlePortalTokenItem's unknown-subpath 404, handlePortalTokenSingle's
// empty-id 404 and PATCH-invalid-JSON/method-not-allowed branches, and
// handlePortalTokenRotate's empty-id 404. None need a REAL token id.

func TestHandlePortalTokensInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalTokensMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/portal/tokens", ""))
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

// TestHandlePortalTokenItemUnknownSubPathReturns404 proves a sub-path other
// than the bare id or "/{id}/rotate" 404s at the dispatcher itself, before
// ever resolving a principal.
func TestHandlePortalTokenItemUnknownSubPathReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/tokens/any-id/bogus", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}

// TestHandlePortalTokenSingleEmptyIDReturns404 proves a trailing-slash path
// with no id segment (GET /api/portal/tokens/) reaches handlePortalTokenSingle
// with id=="" and 404s AFTER resolving the principal (requireWebScope runs
// first), not before.
func TestHandlePortalTokenSingleEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/", `{}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}

func TestHandlePortalTokenSinglePatchInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalTokenSingleMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/tokens/any-id", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "PATCH, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "PATCH, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

// TestHandlePortalTokenRotateEmptyIDReturns404 proves an empty id segment
// before "/rotate" 404s AFTER the method check, mirroring
// handlePortalTokenSingle's ordering. This calls handlePortalTokenRotate
// directly with id=="" rather than routing a literal "//rotate" URL through
// ServeHTTP: net/http.ServeMux cleans a double-slash path and 307-redirects
// before the handler ever runs, which would test the mux's path-cleaning
// behavior instead of this handler's own empty-id branch.
func TestHandlePortalTokenRotateEmptyIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.handlePortalTokenRotate(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens//rotate", ""), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "portal.token_not_found" {
		t.Fatalf("error code = %q, want portal.token_not_found", code)
	}
}
