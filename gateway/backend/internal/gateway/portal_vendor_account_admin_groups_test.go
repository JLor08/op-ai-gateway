// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// PUT /api/portal/vendor-accounts/{id}/admin-groups and GET
// /api/portal/vendor-account-admin-group-candidates (#183, spec 2026-10-15 sections 6
// and 9): the management-group assignment of a shared vendor account. portal.Service
// decides everything (owner-only at `shared`, owner + CM at `full`, >= 1 group, the
// caller's manage set, the immutable root); these tests pin the wire: the routes, the
// bodies, the status and code of every refusal.

const (
	vaCMSecret             = "va-cm-secret"
	vendorAdminGroupsCands = "/api/portal/vendor-account-admin-group-candidates"
)

func adminGroupsPath(id string) string { return "/api/portal/vendor-accounts/" + id + "/admin-groups" }

// adminGroupsFixture is the vendor-account test server with a group store and the
// sharing flags on, two system groups and their admin groups, and a CM:
//
//	SG  -> AG, AG2   (the account's root; the CM, usr_va_cm, manages vendor accounts in AG)
//	SG2 -> AGX       (another root)
//
// The system user (usr_va_sys, vaSystemSecret) owns the groups and, as an ADMIN, the
// accounts the tests share.
type adminGroupsFixture struct {
	srv          *Server
	routes       *routing.MemoryStore
	ag, ag2, agX string
	sgID, sg2ID  string
	sys          auth.Token
}

func newAdminGroupsFixture(t *testing.T) *adminGroupsFixture {
	t.Helper()
	srv, routes, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(d *portal.ServiceDeps) {
		dir, ok := d.Users.(*portal.MemoryDirectory)
		if !ok {
			t.Fatalf("test directory is %T, want *portal.MemoryDirectory", d.Users)
		}
		d.Groups = dir
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
		dir.AddUser(store.User{ID: "usr_va_cm", Email: "cm@example.test", DisplayName: "CM", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
		if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_va_cm", UserID: "usr_va_cm", Name: "CM Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, vaCMSecret); err != nil {
			t.Fatalf("CreatePlainToken cm: %v", err)
		}
	})
	enableVendorAccountsFlag(t, srv)
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)

	f := &adminGroupsFixture{srv: srv, routes: routes, sys: auth.Token{UserID: "usr_va_sys", Scopes: []string{"gateway:use", "admin", "system"}}}
	ctx := context.Background()
	group := func(tier, name, parent string) string {
		g, err := srv.Portal.CreateGroup(ctx, f.sys, portal.CreateGroupInput{Tier: tier, Name: name, ParentGroupID: parent})
		if err != nil {
			t.Fatalf("create %s group %s: %v", tier, name, err)
		}
		return g.ID
	}
	f.sgID = group(store.GroupTierSystem, "SG", "")
	f.ag = group(store.GroupTierAdmin, "AG", f.sgID)
	f.ag2 = group(store.GroupTierAdmin, "AG2", f.sgID)
	f.sg2ID = group(store.GroupTierSystem, "SG2", "")
	f.agX = group(store.GroupTierAdmin, "AGX", f.sg2ID)
	for _, step := range []struct {
		group string
		err   func() error
	}{
		{f.sgID, func() error { return srv.Portal.AddGroupMembers(ctx, f.sys, f.sgID, []string{"usr_va_cm"}) }},
		{f.ag, func() error { return srv.Portal.AddGroupMembers(ctx, f.sys, f.ag, []string{"usr_va_cm"}) }},
		{f.ag, func() error {
			return srv.Portal.PromoteManager(ctx, f.sys, f.ag, "usr_va_cm", portal.ManagerPermissions{CanManageVendorAccounts: true})
		}},
	} {
		if err := step.err(); err != nil {
			t.Fatalf("set up the CM in %s: %v", step.group, err)
		}
	}

	return f
}

// accountAt creates an api_key account owned by the system admin and raises it to
// level (through the endpoint) with AG as its management group; level none leaves it
// private. It returns the account's id.
func (f *adminGroupsFixture) accountAt(t *testing.T, level string) string {
	t.Helper()
	acc := vaCreate(t, f.srv, vaSystemSecret, "to assign "+level)
	if level == routing.VendorShareLevelNone {
		return acc.ID
	}
	body := `{"level":"` + level + `","admin_group_ids":["` + f.ag + `"]}`
	if rec := vaDo(t, f.srv, http.MethodPut, shareLevelPath(acc.ID), vaSystemSecret, body); rec.Code != http.StatusOK {
		t.Fatalf("raise to %s = %d, body = %s", level, rec.Code, rec.Body.String())
	}
	return acc.ID
}

func (f *adminGroupsFixture) put(t *testing.T, id, secret string, groups ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string][]string{"admin_group_ids": groups})
	if err != nil {
		t.Fatal(err)
	}
	return vaDo(t, f.srv, http.MethodPut, adminGroupsPath(id), secret, string(body))
}

func requireStatusCode(t *testing.T, label string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || (code != "" && perfErrorCode(t, rec.Body.Bytes()) != code) {
		t.Errorf("%s = %d %s, want %d %s", label, rec.Code, rec.Body.String(), status, code)
	}
}

// While vendor_account_sharing_enabled is off (its default) both endpoints are a 409,
// for the owner, a stranger, system scope and an id that does not exist alike, and
// nothing is written. The flag is global, so the answer says nothing about any account.
func TestVendorAccountAdminGroupsEndpointsAre409WhileSharingIsOff(t *testing.T) {
	srv, routes := newShareLevelTestServer(t)
	acc := vaCreate(t, srv, vaOwnerSecret, "private")

	for _, secret := range []string{vaOwnerSecret, vaOtherSecret, vaSystemSecret} {
		for _, id := range []string{acc.ID, "va_missing"} {
			rec := vaDo(t, srv, http.MethodPut, adminGroupsPath(id), secret, `{"admin_group_ids":["ugrp_x"]}`)
			requireStatusCode(t, "PUT "+id+" as "+secret, rec, http.StatusConflict, vendorSharingDisabledCode)
		}
		rec := vaDo(t, srv, http.MethodGet, vendorAdminGroupsCands, secret, "")
		requireStatusCode(t, "GET candidates as "+secret, rec, http.StatusConflict, vendorSharingDisabledCode)
	}
	requireShareLevelState(t, routes, acc.ID, routing.VendorShareLevelNone, "")

	// Turning the flag on switches both on live; off again, both are a 409 again.
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	if rec := vaDo(t, srv, http.MethodGet, vendorAdminGroupsCands, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("candidates while on = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if rec := vaDo(t, srv, http.MethodPut, adminGroupsPath(acc.ID), vaOwnerSecret, `{"admin_group_ids":["ugrp_x"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("assign to a private account while on = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":false}`)
	if rec := vaDo(t, srv, http.MethodGet, vendorAdminGroupsCands, vaOwnerSecret, ""); rec.Code != http.StatusConflict {
		t.Fatalf("candidates after switching off = %d, want 409", rec.Code)
	}
}

// The wire contract of every refusal that needs no CM: method, authentication, body,
// existence, ownership, level, the group set. Each is answered after the 404/403 gate,
// so none confirms an account to a caller who may not see it.
func TestVendorAccountAdminGroupsEndpointRefusals(t *testing.T) {
	f := newAdminGroupsFixture(t)
	shared := f.accountAt(t, routing.VendorShareLevelShared)
	private := f.accountAt(t, routing.VendorShareLevelNone)
	plain := vaCreate(t, f.srv, vaOwnerSecret, "owned by a plain user")

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, f.srv, method, adminGroupsPath(shared), vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
			t.Errorf("%s = %d (Allow %q), want 405 with Allow: PUT", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := f.put(t, shared, "", f.ag); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous PUT = %d, want 401", rec.Code)
	}

	cases := []struct {
		name   string
		id     string
		secret string
		body   string
		status int
		code   string
	}{
		{"malformed JSON", shared, vaSystemSecret, `{"admin_group_ids":`, http.StatusBadRequest, codeRequestInvalidJSON},
		{"unknown id", "va_missing", vaSystemSecret, `{"admin_group_ids":["` + f.ag + `"]}`, http.StatusNotFound, "vendor_account.not_found"},
		{"someone else's account", shared, vaOtherSecret, `{"admin_group_ids":["` + f.ag + `"]}`, http.StatusNotFound, "vendor_account.not_found"},
		{"a plain user's account is no one else's", plain.ID, vaSystemSecret, `{"admin_group_ids":["` + f.ag + `"]}`, http.StatusNotFound, "vendor_account.not_found"},
		{"an account that is not shared", private, vaSystemSecret, `{"admin_group_ids":["` + f.ag + `"]}`, http.StatusBadRequest, "vendor_account.not_shared"},
		{"a plain owner's private account", plain.ID, vaOwnerSecret, `{"admin_group_ids":["ugrp_x"]}`, http.StatusBadRequest, "vendor_account.not_shared"},
		{"no body field", shared, vaSystemSecret, `{}`, http.StatusBadRequest, "vendor_account.admin_group_required"},
		{"an empty set", shared, vaSystemSecret, `{"admin_group_ids":[]}`, http.StatusBadRequest, "vendor_account.admin_group_required"},
		{"a missing group", shared, vaSystemSecret, `{"admin_group_ids":["ugrp_missing"]}`, http.StatusBadRequest, "vendor_account.admin_group_invalid"},
		{"a system-tier group", shared, vaSystemSecret, `{"admin_group_ids":["` + f.sgID + `"]}`, http.StatusBadRequest, "vendor_account.admin_group_invalid"},
		{"groups under different parents", shared, vaSystemSecret, `{"admin_group_ids":["` + f.ag + `","` + f.agX + `"]}`, http.StatusBadRequest, "vendor_account.admin_group_parent_mismatch"},
		{"a group under another root", shared, vaSystemSecret, `{"admin_group_ids":["` + f.agX + `"]}`, http.StatusBadRequest, "vendor_account.root_mismatch"},
	}
	for _, tc := range cases {
		requireStatusCode(t, tc.name, vaDo(t, f.srv, http.MethodPut, adminGroupsPath(tc.id), tc.secret, tc.body), tc.status, tc.code)
	}
	requireShareLevelState(t, f.routes, shared, routing.VendorShareLevelShared, f.sgID, f.ag)
	requireShareLevelState(t, f.routes, private, routing.VendorShareLevelNone, "")
	requireShareLevelState(t, f.routes, plain.ID, routing.VendorShareLevelNone, "")
}

// The assignment over HTTP as the owner: the response is the credential-free account
// and the STORE shows the groups and the unchanged root and level at every step.
func TestVendorAccountAdminGroupsReplaceOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	id := f.accountAt(t, routing.VendorShareLevelShared)

	rec := f.put(t, id, vaSystemSecret, f.ag, f.ag2)
	if rec.Code != http.StatusOK {
		t.Fatalf("add AG2 = %d, body = %s", rec.Code, rec.Body.String())
	}
	if dto := vaDecode(t, rec); dto.ID != id || !dto.APIKeySet {
		t.Errorf("response = %+v, want the account %s as a credential-free DTO", dto, id)
	}
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelShared, f.sgID, f.ag, f.ag2)

	if rec := f.put(t, id, vaSystemSecret, f.ag2); rec.Code != http.StatusOK {
		t.Fatalf("drop AG = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelShared, f.sgID, f.ag2)

	// The root cannot move, and a refusal writes nothing.
	requireStatusCode(t, "move to another root", f.put(t, id, vaSystemSecret, f.agX), http.StatusBadRequest, "vendor_account.root_mismatch")
	requireStatusCode(t, "empty set", f.put(t, id, vaSystemSecret), http.StatusBadRequest, "vendor_account.admin_group_required")
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelShared, f.sgID, f.ag2)
}

// Owner-only at `shared`, owner + CM at `full`, over HTTP: a CM of a shared account is a
// 403 (the account is no secret to them) and changes nothing; at `full` the same CM
// may assign, within the groups he manages; a stranger is a 404 at both levels.
func TestVendorAccountAdminGroupsAuthorizationOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	id := f.accountAt(t, routing.VendorShareLevelShared)

	requireStatusCode(t, "CM at shared", f.put(t, id, vaCMSecret, f.ag), http.StatusForbidden, "vendor_account.forbidden")
	requireStatusCode(t, "stranger at shared", f.put(t, id, vaOtherSecret, f.ag), http.StatusNotFound, "vendor_account.not_found")
	f.put(t, id, vaSystemSecret, f.ag, f.ag2)
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelShared, f.sgID, f.ag, f.ag2)
	requireStatusCode(t, "CM at shared, dropping a group", f.put(t, id, vaCMSecret, f.ag), http.StatusForbidden, "vendor_account.forbidden")
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelShared, f.sgID, f.ag, f.ag2)

	// Raise to full: the CM may now assign, but only groups he manages (AG, not AG2).
	if rec := vaDo(t, f.srv, http.MethodPut, shareLevelPath(id), vaSystemSecret, `{"level":"full","admin_group_ids":["`+f.ag+`","`+f.ag2+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("raise to full = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireStatusCode(t, "CM at full naming a group he does not manage", f.put(t, id, vaCMSecret, f.ag2), http.StatusBadRequest, "vendor_account.admin_group_invalid")
	requireStatusCode(t, "stranger at full", f.put(t, id, vaOtherSecret, f.ag), http.StatusNotFound, "vendor_account.not_found")
	if rec := f.put(t, id, vaCMSecret, f.ag); rec.Code != http.StatusOK {
		t.Fatalf("CM at full = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireShareLevelState(t, f.routes, id, routing.VendorShareLevelFull, f.sgID, f.ag)
}

// The candidates over HTTP: system scope sees every admin-tier group, the CM only the
// groups he manages vendor accounts through, a plain user an empty list (JSON [], not
// null), and each candidate carries its parent system group.
func TestVendorAccountAdminGroupCandidatesOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)

	type candidate struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		ParentGroupID   string `json:"parent_group_id"`
		ParentGroupName string `json:"parent_group_name"`
	}
	get := func(secret string) []candidate {
		t.Helper()
		rec := vaDo(t, f.srv, http.MethodGet, vendorAdminGroupsCands, secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET candidates as %s = %d, body = %s", secret, rec.Code, rec.Body.String())
		}
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		if string(raw.Data) == "null" || len(raw.Data) == 0 {
			t.Fatalf("data = %s, want a JSON array", raw.Data)
		}
		var out []candidate
		if err := json.Unmarshal(raw.Data, &out); err != nil {
			t.Fatalf("decode data %s: %v", raw.Data, err)
		}
		return out
	}

	sys := get(vaSystemSecret)
	if len(sys) != 3 || sys[0].Name != "AG" || sys[1].Name != "AG2" || sys[2].Name != "AGX" {
		t.Fatalf("system candidates = %+v, want AG, AG2, AGX", sys)
	}
	if sys[0].ParentGroupID != f.sgID || sys[0].ParentGroupName != "SG" || sys[2].ParentGroupID != f.sg2ID || sys[2].ParentGroupName != "SG2" {
		t.Errorf("system candidates carry the wrong parents: %+v", sys)
	}
	if cm := get(vaCMSecret); len(cm) != 1 || cm[0].ID != f.ag {
		t.Errorf("CM candidates = %+v, want only AG", cm)
	}
	if plain := get(vaOwnerSecret); len(plain) != 0 {
		t.Errorf("plain user candidates = %+v, want none", plain)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, f.srv, method, vendorAdminGroupsCands, vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s = %d (Allow %q), want 405 with Allow: GET", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := vaDo(t, f.srv, http.MethodGet, vendorAdminGroupsCands, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", rec.Code)
	}
}

// Every sentinel the assignment can answer has its row: status and code, and a store
// failure is the endpoint's own 500 code.
func TestVendorAccountAdminGroupsErrorRows(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{portal.ErrVendorAccountSharingDisabled, http.StatusConflict, "vendor_accounts.sharing_disabled"},
		{portal.ErrVendorAccountNotShared, http.StatusBadRequest, "vendor_account.not_shared"},
		{portal.ErrVendorAccountAdminGroupRequired, http.StatusBadRequest, "vendor_account.admin_group_required"},
		{portal.ErrVendorAccountAdminGroupInvalid, http.StatusBadRequest, "vendor_account.admin_group_invalid"},
		{portal.ErrVendorAccountAdminGroupParentMismatch, http.StatusBadRequest, "vendor_account.admin_group_parent_mismatch"},
		{portal.ErrVendorAccountRootMismatch, http.StatusBadRequest, "vendor_account.root_mismatch"},
		{portal.ErrVendorAccountForbidden, http.StatusForbidden, "vendor_account.forbidden"},
		{portal.ErrVendorAccountNotFound, http.StatusNotFound, portal.CodeVendorAccountNotFound},
		{errors.New("boom"), http.StatusInternalServerError, codeVendorAccountAdminGroupsFailed},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writePortalVendorAccountError(rec, tc.err, codeVendorAccountAdminGroupsFailed)
		requireStatusCode(t, tc.err.Error(), rec, tc.status, tc.code)
	}
}
