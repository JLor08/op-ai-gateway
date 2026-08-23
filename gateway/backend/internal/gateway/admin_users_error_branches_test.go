// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"testing"
)

// This file targets admin_users.go's NEW uncovered lines: the two
// ListUsers/ManageableUserIDs 500 branches in handleAdminUsers/
// handleAdminUserItem (via forced-failure account.API/portal.API doubles,
// mirroring auth_error_branches_test.go's expireSessionOnNthResolve
// pattern), the collection's own invalid-JSON and method-not-allowed
// branches, and the item route's PATCH-invalid-JSON branch.

// failingListUsersAccount wraps a real account.API and forces ListUsers to
// fail, exercising handleAdminUsers' 500 admin.user_list_failed branch.
type failingListUsersAccount struct {
	account.API
	err error
}

func (a failingListUsersAccount) ListUsers(context.Context) ([]store.User, error) {
	return nil, a.err
}

// failingManageableUserIDsPortal wraps a real portal.API and forces
// ManageableUserIDs to fail, exercising the SAME 500 branch reached from the
// non-system-caller filtering path in handleAdminUsers, and the equivalent
// gate in handleAdminUserItem.
type failingManageableUserIDsPortal struct {
	portal.API
	err error
}

func (p failingManageableUserIDsPortal) ManageableUserIDs(context.Context, auth.Token) (map[string]bool, error) {
	return nil, p.err
}

func TestHandleAdminUsersListUsersFailureReturns500(t *testing.T) {
	srv := NewTestServer()
	srv.Account = failingListUsersAccount{API: srv.Account, err: errors.New("store unavailable")}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/admin/users", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "admin.user_list_failed" {
		t.Fatalf("error code = %q, want admin.user_list_failed", code)
	}
}

// TestHandleAdminUsersManageableUserIDsFailureReturns500 uses newAuthTestServer
// (a real Account, needed since s.Account.ListUsers runs and must succeed
// first) with a PLAIN admin session — deliberately no "system" scope — so
// the non-system filtering branch in handleAdminUsers actually runs.
func TestHandleAdminUsersManageableUserIDsFailureReturns500(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_adminfail", "adminfail@example.test", "password-1", "admin")
	cookie := loginAs(t, srv, "adminfail@example.test", "password-1")
	srv.Portal = failingManageableUserIDsPortal{API: srv.Portal, err: errors.New("group store unavailable")}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "admin.user_list_failed" {
		t.Fatalf("error code = %q, want admin.user_list_failed", code)
	}
}

func TestHandleAdminUsersInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/admin/users", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandleAdminUsersMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodDelete, "/api/admin/users", ""))
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

// TestHandleAdminUserItemManageableUserIDsFailureReturns500 hits the analogous
// ManageableUserIDs gate at the top of handleAdminUserItem (a non-system
// caller resolving whether the target id is in their manageable set).
func TestHandleAdminUserItemManageableUserIDsFailureReturns500(t *testing.T) {
	srv := NewTestServer()
	srv.Portal = failingManageableUserIDsPortal{API: srv.Portal, err: errors.New("group store unavailable")}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/admin/users/any-id/tokens", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "admin.user_list_failed" {
		t.Fatalf("error code = %q, want admin.user_list_failed", code)
	}
}

func TestHandleAdminUserItemPatchInvalidJSONReturns400(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/admin/users/any-id", `[]`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}
