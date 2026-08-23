// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file targets group_endpoints.go's request-shape/dispatch branches that
// carry NEW uncovered lines: each handler's own json.Unmarshal-into-typed-
// struct failure (a syntactically valid `[]` body that fails to decode into
// the specific struct, not the earlier generic readRawJSON path — see
// auth_error_branches_test.go's file comment for why), the two
// method-not-allowed defaults not already exercised by
// TestGroupEndpointsMethodNotAllowed (which only covers the /groups
// collection), and handlePortalGroupItem's empty-id / unknown-subpath 404s.
// None of these need a REAL group id: the JSON-decode and method checks all
// run before any portal.Service lookup.

// newGroupErrBranchServer returns a *Server plus a valid session cookie for a
// plain logged-in user — all the branches below run before any group-tier
// authorization check, so a plain "user" session is sufficient.
func newGroupErrBranchServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_grperr", "grperr@example.test", "password-1", "user")
	return srv, loginSession(t, srv, "grperr@example.test", "password-1")
}

func TestHandlePortalGroupsInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/groups", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandleAdminOwnerCandidatesMethodNotAllowed(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/admin-owner-candidates", strings.NewReader(`{}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow header = %q, want %q", got, http.MethodGet)
	}
	if code := errorCode(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalGroupItemEmptyIDReturns404(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/groups/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "group.not_found" {
		t.Fatalf("error code = %q, want group.not_found", code)
	}
}

func TestHandlePortalGroupItemUnknownSubPathReturns404(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/groups/any-id/bogus", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "group.not_found" {
		t.Fatalf("error code = %q, want group.not_found", code)
	}
}

func TestHandlePortalGroupSinglePatchInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/portal/groups/any-id", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalGroupSingleMethodNotAllowed proves the item route is
// PATCH/DELETE-only — there is deliberately no GET (spec §11: a single
// group's detail is exposed only via the ListGroups landscape).
func TestHandlePortalGroupSingleMethodNotAllowed(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/groups/any-id", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "PATCH, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "PATCH, DELETE")
	}
	if code := errorCode(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalGroupMembersInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/groups/any-id/members", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalGroupMembersMethodNotAllowed(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/portal/groups/any-id/members", nil)
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, POST")
	}
	if code := errorCode(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

func TestHandlePortalGroupManagersInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/groups/any-id/managers", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalGroupManagerSinglePatchInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newGroupErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/portal/groups/any-id/managers/any-user", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}
