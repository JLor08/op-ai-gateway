// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAdminUserLimitsRequiresAdminScope proves the route has NO self-service
// path (design spec §7.2): a normal, non-admin user is refused even when the
// {id} in the path IS their own — the gate is on the caller's SCOPE, not on
// whose id is addressed.
func TestAdminUserLimitsRequiresAdminScope(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "user@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "user@example.test", "password-1")

	get := httptest.NewRequest(http.MethodGet, "/api/portal/admin/users/usr_1/limits", nil)
	get.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, get)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET own limits as a normal user = %d, want 403 (no self-service path exists)", rec.Code)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/portal/admin/users/usr_1/limits", strings.NewReader(`{}`))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusForbidden {
		t.Fatalf("PUT own limits as a normal user = %d, want 403", putRec.Code)
	}
}

func TestAdminUserLimitsGetSetRoundTrip(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_1", "user@example.test", "password-1", "user")
	// usr_admin must manage usr_1 (Task 3 fix-round 1: the user-limits endpoint is
	// now scoped to ManageableUserIDs) -- owns an admin group with usr_1 as a member.
	createSystemGroupForTest(t, dir, "ugrp_limits_sg", "Limits SG")
	createAdminGroupForTest(t, dir, "ugrp_limits_ag", "Limits AG", "ugrp_limits_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_limits_ag", "usr_1")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	// A fresh user has no limits yet.
	get := httptest.NewRequest(http.MethodGet, "/api/portal/admin/users/usr_1/limits", nil)
	get.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET limits = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"rate_requests":0`) {
		t.Fatalf("fresh user's limits body = %s, want zero-value limits", rec.Body.String())
	}

	body := `{"rate_requests":5,"rate_window_seconds":10,"request_quota":100,"request_quota_period":"day"}`
	put := httptest.NewRequest(http.MethodPut, "/api/portal/admin/users/usr_1/limits", strings.NewReader(body))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT limits = %d body=%s", putRec.Code, putRec.Body.String())
	}
	if !strings.Contains(putRec.Body.String(), `"rate_requests":5`) || !strings.Contains(putRec.Body.String(), `"request_quota_period":"day"`) {
		t.Fatalf("PUT response = %s, want the just-set limits echoed back", putRec.Body.String())
	}

	get2 := httptest.NewRequest(http.MethodGet, "/api/portal/admin/users/usr_1/limits", nil)
	get2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, get2)
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"rate_requests":5`) {
		t.Fatalf("GET after PUT = %d body=%s, want the persisted limits", rec2.Code, rec2.Body.String())
	}
}

func TestAdminUserLimitsUnknownUser404(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	get := httptest.NewRequest(http.MethodGet, "/api/portal/admin/users/no-such-user/limits", nil)
	get.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, get)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET limits for an unknown user = %d, want 404", rec.Code)
	}
}

func TestAdminUserLimitsValidation400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_1", "user@example.test", "password-1", "user")
	// usr_admin must manage usr_1 (Task 3 fix-round 1) so the PUT reaches the
	// validation check under test, rather than 404-no-leaking before it.
	createSystemGroupForTest(t, dir, "ugrp_limits_val_sg", "Limits Val SG")
	createAdminGroupForTest(t, dir, "ugrp_limits_val_ag", "Limits Val AG", "ugrp_limits_val_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_limits_val_ag", "usr_1")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	put := httptest.NewRequest(http.MethodPut, "/api/portal/admin/users/usr_1/limits", strings.NewReader(`{"rate_requests":-1,"rate_window_seconds":10}`))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, put)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid limits = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestAdminUserLimitsMethodNotAllowed(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_1", "user@example.test", "password-1", "user")
	// usr_admin must manage usr_1 (Task 3 fix-round 1) so the request reaches
	// the method-not-allowed check under test, rather than 404-no-leaking first.
	createSystemGroupForTest(t, dir, "ugrp_limits_405_sg", "Limits 405 SG")
	createAdminGroupForTest(t, dir, "ugrp_limits_405_ag", "Limits 405 AG", "ugrp_limits_405_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_limits_405_ag", "usr_1")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	del := httptest.NewRequest(http.MethodDelete, "/api/portal/admin/users/usr_1/limits", nil)
	del.Header.Set(csrfHeaderName, "1")
	del.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, del)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE limits = %d, want 405", rec.Code)
	}
}

// TestAdminUserLimitsScopedToManageableUserIDs proves the Task 3 fix-round-1
// gate in handlePortalAdminUserLimits: a non-`system` caller's GET and PUT
// both 404-no-leak on a target OUTSIDE their ManageableUserIDs, and succeed
// on one inside it (a member of an admin group the caller owns) -- the exact
// same gate + guarantee as handleAdminUserItem (admin_users.go), now also
// covering the separate /api/portal/admin/users/{id}/limits route/handler in
// server.go. adminA owns an admin group AG containing usr_in as a member;
// usr_out shares no group relationship with adminA at all.
func TestAdminUserLimitsScopedToManageableUserIDs(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_in", "in@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_out", "out@example.test", "password-1", "user")
	createSystemGroupForTest(t, dir, "ugrp_limits_scope_sg", "Limits Scope SG")
	createAdminGroupForTest(t, dir, "ugrp_limits_scope_ag", "Limits Scope AG", "ugrp_limits_scope_sg", "usr_admina")
	addSystemGroupMember(t, dir, "ugrp_limits_scope_ag", "usr_in")
	cookie := loginAs(t, srv, "admina@example.test", "password-1")

	get := func(userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/portal/admin/users/"+userID+"/limits", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	put := func(userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/portal/admin/users/"+userID+"/limits", strings.NewReader(`{"rate_requests":3,"rate_window_seconds":10}`))
		req.Header.Set(csrfHeaderName, "1")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// usr_out is outside adminA's manageable set -> both GET and PUT 404
	// no-leak (the same code as a genuinely nonexistent user).
	if rec := get("usr_out"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "admin.user_not_found") {
		t.Fatalf("GET limits on an unmanageable target should 404 no-leak, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := put("usr_out"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "admin.user_not_found") {
		t.Fatalf("PUT limits on an unmanageable target should 404 no-leak, got %d body=%s", rec.Code, rec.Body.String())
	}

	// usr_in IS inside adminA's manageable set (a member of AG, which adminA
	// owns) -> both GET and PUT succeed.
	if rec := get("usr_in"); rec.Code != http.StatusOK {
		t.Fatalf("GET limits on a manageable target should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := put("usr_in"); rec.Code != http.StatusOK {
		t.Fatalf("PUT limits on a manageable target should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
}
