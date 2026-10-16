// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"testing"
)

// PUT /api/portal/vendor-accounts/{id}/share-level (#183, spec 2026-10-15 sections 6, 7,
// 9, 11 and 13): the endpoint of the share-level lifecycle. portal.Service decides
// everything (who may raise, who may lower, the subscription cap, the flags); these
// tests pin the wire: the route, the body, the status and code of every refusal.

const (
	vendorSharingDisabledCode             = "vendor_accounts.sharing_disabled"
	vendorSubscriptionSharingDisabledCode = "vendor_accounts.subscription_sharing_disabled"
)

func shareLevelPath(id string) string { return "/api/portal/vendor-accounts/" + id + "/share-level" }

// newShareLevelTestServer is the vendor-account test server with a GROUP store (the
// raise validates its management groups against real groups), the module flag ON and
// the sharing flags at their defaults (OFF).
func newShareLevelTestServer(t *testing.T) (*Server, *routing.MemoryStore) {
	t.Helper()
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(d *portal.ServiceDeps) {
		dir, ok := d.Users.(*portal.MemoryDirectory)
		if !ok {
			t.Fatalf("test directory is %T, want *portal.MemoryDirectory", d.Users)
		}
		d.Groups = dir
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore
}

func putVendorSystemSetting(t *testing.T, srv *Server, body string) {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings %s = %d, body = %s", body, rec.Code, rec.Body.String())
	}
}

// shareLevelGroup creates a system group and an admin group under it (as the system
// admin, who owns the admin group), returning the admin group's and its parent's id.
func shareLevelGroup(t *testing.T, srv *Server) (adminGroupID, systemGroupID string) {
	t.Helper()
	sys := auth.Token{UserID: "usr_va_sys", Scopes: []string{"gateway:use", "admin", "system"}}
	sg, err := srv.Portal.CreateGroup(context.Background(), sys, portal.CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	if err != nil {
		t.Fatalf("create system group: %v", err)
	}
	ag, err := srv.Portal.CreateGroup(context.Background(), sys, portal.CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	if err != nil {
		t.Fatalf("create admin group: %v", err)
	}
	return ag.ID, sg.ID
}

func requireShareLevelState(t *testing.T, routes *routing.MemoryStore, id, level, root string, groups ...string) {
	t.Helper()
	acc, err := routes.VendorAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.ShareLevel != level || acc.SystemGroupID != root {
		t.Errorf("share_level/system_group_id = %q/%q, want %q/%q", acc.ShareLevel, acc.SystemGroupID, level, root)
	}
	got, err := routes.VendorAccountAdminGroups(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountAdminGroups: %v", err)
	}
	slices.Sort(got)
	want := slices.Clone(groups)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("management groups = %v, want %v", got, want)
	}
}

// While vendor_account_sharing_enabled is off (its default) the endpoint is a 409,
// for the owner, a stranger, system scope and an id that does not exist alike, and
// nothing is written. The flag is global, so the answer says nothing about any
// account.
func TestVendorAccountShareLevelIs409WhileSharingIsOff(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	acc := vaCreate(t, srv, vaOwnerSecret, "private")

	for _, secret := range []string{vaOwnerSecret, vaOtherSecret, vaSystemSecret} {
		for _, id := range []string{acc.ID, "va_missing"} {
			for _, body := range []string{`{"level":"shared","admin_group_ids":["ugrp_x"]}`, `{"level":"none"}`, `{"level":"bogus"}`} {
				rec := vaDo(t, srv, http.MethodPut, shareLevelPath(id), secret, body)
				if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorSharingDisabledCode {
					t.Fatalf("PUT %s %s as %s = %d %s, want 409 %s", id, body, secret, rec.Code, rec.Body.String(), vendorSharingDisabledCode)
				}
			}
		}
	}
	requireShareLevelState(t, routes, acc.ID, routing.VendorShareLevelNone, "")

	// Turning the flag on switches the endpoint on live; off again, it is a 409 again.
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	if rec := vaDo(t, srv, http.MethodPut, shareLevelPath(acc.ID), vaOwnerSecret, `{"level":"none"}`); rec.Code != http.StatusOK {
		t.Fatalf("lower while on = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":false}`)
	if rec := vaDo(t, srv, http.MethodPut, shareLevelPath(acc.ID), vaOwnerSecret, `{"level":"none"}`); rec.Code != http.StatusConflict {
		t.Fatalf("lower after switching off = %d, want 409", rec.Code)
	}
}

// The wire contract of every refusal that needs no group: method, authentication,
// body, level, existence, ownership, admin role, same level.
func TestVendorAccountShareLevelEndpointRefusals(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	owned := vaCreate(t, srv, vaOwnerSecret, "owned by a plain user")
	path := shareLevelPath(owned.ID)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, srv, method, path, vaOwnerSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
			t.Errorf("%s = %d (Allow %q), want 405 with Allow: PUT", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := vaDo(t, srv, http.MethodPut, path, "", `{"level":"none"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous PUT = %d, want 401", rec.Code)
	}

	cases := []struct {
		name   string
		path   string
		secret string
		body   string
		status int
		code   string
	}{
		{"malformed JSON", path, vaOwnerSecret, `{"level":`, http.StatusBadRequest, codeRequestInvalidJSON},
		{"no level", path, vaOwnerSecret, `{}`, http.StatusBadRequest, "vendor_account.share_level_invalid"},
		{"unknown level", path, vaOwnerSecret, `{"level":"everyone"}`, http.StatusBadRequest, "vendor_account.share_level_invalid"},
		{"unknown id", shareLevelPath("va_missing"), vaOwnerSecret, `{"level":"none"}`, http.StatusNotFound, "vendor_account.not_found"},
		{"someone else's account", path, vaOtherSecret, `{"level":"none"}`, http.StatusNotFound, "vendor_account.not_found"},
		{"system scope is a stranger", path, vaSystemSecret, `{"level":"none"}`, http.StatusNotFound, "vendor_account.not_found"},
		// A plain-user owner can see the account, so refusing the raise is a 403.
		{"raise by a non-admin owner", path, vaOwnerSecret, `{"level":"shared","admin_group_ids":["ugrp_x"]}`, http.StatusForbidden, "vendor_account.forbidden"},
	}
	for _, tc := range cases {
		rec := vaDo(t, srv, http.MethodPut, tc.path, tc.secret, tc.body)
		if rec.Code != tc.status || perfErrorCode(t, rec.Body.Bytes()) != tc.code {
			t.Errorf("%s = %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
	requireShareLevelState(t, routes, owned.ID, routing.VendorShareLevelNone, "")

	// A raise by an admin owner must name a group.
	adminOwned := vaCreate(t, srv, vaSystemSecret, "owned by the system admin")
	rec := vaDo(t, srv, http.MethodPut, shareLevelPath(adminOwned.ID), vaSystemSecret, `{"level":"shared"}`)
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.admin_group_required" {
		t.Errorf("raise without a group = %d %s, want 400 vendor_account.admin_group_required", rec.Code, rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodPut, shareLevelPath(adminOwned.ID), vaSystemSecret, `{"level":"shared","admin_group_ids":["ugrp_missing"]}`)
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.admin_group_invalid" {
		t.Errorf("raise into a missing group = %d %s, want 400 vendor_account.admin_group_invalid", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, adminOwned.ID, routing.VendorShareLevelNone, "")

	// Lowering an account that is already private is a 200 (idempotent).
	if rec := vaDo(t, srv, http.MethodPut, path, vaOwnerSecret, `{"level":"none"}`); rec.Code != http.StatusOK {
		t.Errorf("lower a private account = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// The whole lifecycle over HTTP, as an admin owner: raise to shared, raise to full,
// lower to none; the response is the credential-free account and the STORE shows the
// level, the containment root and the management groups at every step.
func TestVendorAccountShareLevelRaiseAndLowerOverHTTP(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	agID, sgID := shareLevelGroup(t, srv)
	acc := vaCreate(t, srv, vaSystemSecret, "to share")
	path := shareLevelPath(acc.ID)

	rec := vaDo(t, srv, http.MethodPut, path, vaSystemSecret, `{"level":"shared","admin_group_ids":["`+agID+`"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("raise to shared = %d, body = %s", rec.Code, rec.Body.String())
	}
	if dto := vaDecode(t, rec); dto.ID != acc.ID || !dto.APIKeySet {
		t.Errorf("response = %+v, want the account %s as a credential-free DTO", dto, acc.ID)
	}
	requireShareLevelState(t, routes, acc.ID, routing.VendorShareLevelShared, sgID, agID)

	// The same level again is a 400, not a silent no-op.
	rec = vaDo(t, srv, http.MethodPut, path, vaSystemSecret, `{"level":"shared","admin_group_ids":["`+agID+`"]}`)
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.share_level_unchanged" {
		t.Errorf("shared -> shared = %d %s, want 400 vendor_account.share_level_unchanged", rec.Code, rec.Body.String())
	}

	if rec := vaDo(t, srv, http.MethodPut, path, vaSystemSecret, `{"level":"full","admin_group_ids":["`+agID+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("raise to full = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, acc.ID, routing.VendorShareLevelFull, sgID, agID)

	if rec := vaDo(t, srv, http.MethodPut, path, vaSystemSecret, `{"level":"none"}`); rec.Code != http.StatusOK {
		t.Fatalf("lower to none = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, acc.ID, routing.VendorShareLevelNone, "")
}

// A subscription account over HTTP: capped at shared (400), and only while the
// subscription flag is on (409); an api_key account never meets either.
func TestVendorAccountShareLevelSubscriptionOverHTTP(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	agID, sgID := shareLevelGroup(t, srv)

	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaSystemSecret, `{"vendor":"anthropic","auth_type":"subscription","name":"sub"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create subscription account = %d, body = %s", rec.Code, rec.Body.String())
	}
	sub := vaDecode(t, rec)
	raise := func(level string) *httptest.ResponseRecorder {
		return vaDo(t, srv, http.MethodPut, shareLevelPath(sub.ID), vaSystemSecret, `{"level":"`+level+`","admin_group_ids":["`+agID+`"]}`)
	}

	// Subscription sharing is off by default.
	if rec := raise("shared"); rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorSubscriptionSharingDisabledCode {
		t.Fatalf("subscription -> shared, flag off = %d %s, want 409 %s", rec.Code, rec.Body.String(), vendorSubscriptionSharingDisabledCode)
	}
	if rec := raise("full"); rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.subscription_share_capped" {
		t.Fatalf("subscription -> full = %d %s, want 400 vendor_account.subscription_share_capped", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, sub.ID, routing.VendorShareLevelNone, "")

	putVendorSystemSetting(t, srv, `{"vendor_subscription_sharing_enabled":true}`)
	if rec := raise("full"); rec.Code != http.StatusBadRequest {
		t.Fatalf("subscription -> full with the flag on = %d, want 400 (still capped)", rec.Code)
	}
	if rec := raise("shared"); rec.Code != http.StatusOK {
		t.Fatalf("subscription -> shared with the flag on = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, sub.ID, routing.VendorShareLevelShared, sgID, agID)

	// Un-sharing never depends on the subscription flag.
	putVendorSystemSetting(t, srv, `{"vendor_subscription_sharing_enabled":false}`)
	if rec := vaDo(t, srv, http.MethodPut, shareLevelPath(sub.ID), vaSystemSecret, `{"level":"none"}`); rec.Code != http.StatusOK {
		t.Fatalf("lower with the subscription flag off = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, routes, sub.ID, routing.VendorShareLevelNone, "")
}

// Delete follows the credential gate now: a plain owner deletes their own account at
// any level over HTTP, and a stranger still gets the 404.
func TestVendorAccountDeleteOverHTTPStillOwnerOnlyForAStranger(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	acc := vaCreate(t, srv, vaOwnerSecret, "to delete")
	if rec := vaDo(t, srv, http.MethodDelete, "/api/portal/vendor-accounts/"+acc.ID, vaOtherSecret, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger delete = %d, want 404", rec.Code)
	}
	if rec := vaDo(t, srv, http.MethodDelete, "/api/portal/vendor-accounts/"+acc.ID, vaSystemSecret, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("system-scope delete = %d, want 404 (system scope never bypasses a write)", rec.Code)
	}
	if _, err := routes.VendorAccountByID(context.Background(), acc.ID); err != nil {
		t.Fatalf("account lost after refused deletes: %v", err)
	}
	if rec := vaDo(t, srv, http.MethodDelete, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("owner delete = %d, want 200", rec.Code)
	}
}

// Every sentinel the lifecycle can answer has its row: status and code.
func TestVendorAccountShareLevelErrorRows(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{portal.ErrVendorAccountSharingDisabled, http.StatusConflict, "vendor_accounts.sharing_disabled"},
		{portal.ErrVendorSubscriptionSharingDisabled, http.StatusConflict, "vendor_accounts.subscription_sharing_disabled"},
		{portal.ErrVendorAccountSubscriptionShareCapped, http.StatusBadRequest, "vendor_account.subscription_share_capped"},
		{portal.ErrVendorAccountShareLevelInvalid, http.StatusBadRequest, "vendor_account.share_level_invalid"},
		{portal.ErrVendorAccountShareLevelUnchanged, http.StatusBadRequest, "vendor_account.share_level_unchanged"},
		{portal.ErrVendorAccountAdminGroupRequired, http.StatusBadRequest, "vendor_account.admin_group_required"},
		{portal.ErrVendorAccountAdminGroupInvalid, http.StatusBadRequest, "vendor_account.admin_group_invalid"},
		{portal.ErrVendorAccountAdminGroupParentMismatch, http.StatusBadRequest, "vendor_account.admin_group_parent_mismatch"},
		{portal.ErrVendorAccountRootMismatch, http.StatusBadRequest, "vendor_account.root_mismatch"},
		{portal.ErrVendorAccountForbidden, http.StatusForbidden, "vendor_account.forbidden"},
		{portal.ErrVendorAccountNotFound, http.StatusNotFound, portal.CodeVendorAccountNotFound},
		// A store failure is still the endpoint's own 500.
		{errors.New("boom"), http.StatusInternalServerError, codeVendorAccountShareLevelFailed},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writePortalVendorAccountError(rec, tc.err, codeVendorAccountShareLevelFailed)
		if rec.Code != tc.status || perfErrorCode(t, rec.Body.Bytes()) != tc.code {
			t.Errorf("%v -> %d %s, want %d %s", tc.err, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
}
