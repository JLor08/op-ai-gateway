// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file targets project_endpoints.go's request-shape/dispatch branches
// carrying NEW uncovered lines: each handler's own
// json.Unmarshal-into-typed-struct failure (a syntactically valid `[]` body,
// see auth_error_branches_test.go's file comment) and the two
// method-not-allowed defaults. None need a REAL project id: both checks run
// before any portal.Service lookup.

// newProjectErrBranchServer returns a *Server plus a valid session cookie for
// a plain logged-in user.
func newProjectErrBranchServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_prjerr", "prjerr@example.test", "password-1", "user")
	return srv, loginSession(t, srv, "prjerr@example.test", "password-1")
}

func TestHandlePortalProjectsInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/projects", strings.NewReader(`[]`))
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

func TestHandlePortalProjectsMethodNotAllowed(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/portal/projects", nil)
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

func TestHandlePortalProjectSinglePatchInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/portal/projects/any-id", strings.NewReader(`[]`))
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

// TestHandlePortalProjectSingleMethodNotAllowed proves the item route is
// PATCH/DELETE-only — there is deliberately no GET (a single project's detail
// is read via the ListProjects landscape, mirroring the user-groups surface).
func TestHandlePortalProjectSingleMethodNotAllowed(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/projects/any-id", nil)
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

func TestHandlePortalProjectMembersInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/projects/any-id/members", strings.NewReader(`[]`))
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

func TestHandlePortalProjectMembersMethodNotAllowed(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/portal/projects/any-id/members", nil)
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

func TestHandlePortalProjectGroupsInvalidJSONReturns400(t *testing.T) {
	srv, cookie := newProjectErrBranchServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/projects/any-id/groups", strings.NewReader(`[]`))
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
