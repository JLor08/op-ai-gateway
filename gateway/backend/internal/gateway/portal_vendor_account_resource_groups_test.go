// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
)

// The resource-group link endpoints of a shared vendor account (#183, spec 2026-10-15
// sections 6, 7 and 9):
//
//	GET    /api/portal/vendor-accounts/{id}/resource-groups          the owner's reachable groups + member flag
//	PUT    /api/portal/vendor-accounts/{id}/resource-groups/{rgId}   the owner joins (idempotent)
//	DELETE /api/portal/vendor-accounts/{id}/resource-groups/{rgId}   the owner leaves (idempotent)
//	PUT    /api/portal/resource-groups/{id}/vendor-accounts          the group manager replaces the set
//	GET    /api/portal/resource-groups/{id}/vendor-account-candidates  what the manager may add
//
// portal.Service decides everything (owner + shared/full + membership; manager + operate +
// full + root); these tests pin the wire: the routes, the bodies, the status and code of
// every refusal.

const resourceGroupNotFoundCode = "resource_group.not_found"

func vaResourceGroupsPath(accountID string, rest ...string) string {
	p := "/api/portal/vendor-accounts/" + accountID + "/resource-groups"
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

func rgVendorAccountsPath(rgID string) string {
	return "/api/portal/resource-groups/" + rgID + "/vendor-accounts"
}

func rgVendorAccountCandidatesPath(rgID string) string {
	return "/api/portal/resource-groups/" + rgID + "/vendor-account-candidates"
}

// resourceGroup creates a resource group linked to adminGroups (as the system user) and
// returns its id.
func (f *adminGroupsFixture) resourceGroup(t *testing.T, name string, adminGroups ...string) string {
	t.Helper()
	rg, err := f.srv.Portal.CreateResourceGroup(context.Background(), f.sys, portal.CreateResourceGroupRequest{Name: name, AdminGroupIDs: adminGroups})
	if err != nil {
		t.Fatalf("create resource group %s: %v", name, err)
	}
	return rg.ID
}

func (f *adminGroupsFixture) rgAccounts(t *testing.T, rgID string) []string {
	t.Helper()
	ids, err := f.routes.ResourceGroupVendorAccounts(context.Background(), rgID)
	if err != nil {
		t.Fatalf("ResourceGroupVendorAccounts(%s): %v", rgID, err)
	}
	slices.Sort(ids)
	return ids
}

func (f *adminGroupsFixture) requireRGAccounts(t *testing.T, label, rgID string, want ...string) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	if got := f.rgAccounts(t, rgID); !slices.Equal(got, want) {
		t.Errorf("%s: accounts linked into %s = %v, want %v", label, rgID, got, want)
	}
}

type vaRGListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Member bool   `json:"member"`
}

func decodeVaRGList(t *testing.T, rec *httptest.ResponseRecorder) []vaRGListEntry {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || string(body.Data) == "null" || len(body.Data) == 0 {
		t.Fatalf("list body = %s (%v), want {\"data\":[...]}", rec.Body.String(), err)
	}
	var out []vaRGListEntry
	if err := json.Unmarshal(body.Data, &out); err != nil {
		t.Fatalf("decode %s: %v", body.Data, err)
	}
	return out
}

// With vendor_account_sharing_enabled off (its default) all five endpoints are a 409,
// for the owner, a stranger and system scope alike and whatever the ids, and nothing is
// written; the flag is global, so the answer says nothing about any account or group.
func TestVendorAccountResourceGroupEndpointsAre409WhileSharingIsOff(t *testing.T) {
	srv, _ := newShareLevelTestServer(t)
	acc := vaCreate(t, srv, vaOwnerSecret, "private")

	calls := []struct {
		name         string
		method, path string
		body         string
	}{
		{"owner list", http.MethodGet, vaResourceGroupsPath(acc.ID), ""},
		{"owner join", http.MethodPut, vaResourceGroupsPath(acc.ID, "rgrp_x"), ""},
		{"owner leave", http.MethodDelete, vaResourceGroupsPath(acc.ID, "rgrp_x"), ""},
		{"owner list of an unknown id", http.MethodGet, vaResourceGroupsPath("va_missing"), ""},
		{"manager set", http.MethodPut, rgVendorAccountsPath("rgrp_x"), `{"vendor_account_ids":["` + acc.ID + `"]}`},
		{"manager candidates", http.MethodGet, rgVendorAccountCandidatesPath("rgrp_x"), ""},
	}
	for _, secret := range []string{vaOwnerSecret, vaOtherSecret, vaSystemSecret} {
		for _, c := range calls {
			rec := vaDo(t, srv, c.method, c.path, secret, c.body)
			requireStatusCode(t, c.name+" as "+secret, rec, http.StatusConflict, vendorSharingDisabledCode)
		}
	}
	// Anonymous is still a 401: authentication comes before the flag.
	for _, c := range calls {
		if rec := vaDo(t, srv, c.method, c.path, "", c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", c.name, rec.Code)
		}
	}

	// The flag switches them on live.
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)
	if rec := vaDo(t, srv, http.MethodGet, vaResourceGroupsPath(acc.ID), vaOwnerSecret, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("list of a private account while on = %d, want 400 (not shared), body = %s", rec.Code, rec.Body.String())
	}
}

// The owner self-service over HTTP: list the reachable groups (with the member flag), join,
// see Member flip, leave. The STORE shows every step; the response of a write is {"ok":true}.
func TestVendorAccountResourceGroupSelfServiceOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	f.resourceGroup(t, "RG under another root", f.agX) // never offered: another root
	acc := f.accountAt(t, routing.VendorShareLevelShared)

	rec := vaDo(t, f.srv, http.MethodGet, vaResourceGroupsPath(acc), vaSystemSecret, "")
	if got := decodeVaRGList(t, rec); !slices.Equal(got, []vaRGListEntry{{ID: rg, Name: "RG"}}) {
		t.Fatalf("list = %+v, want only RG with member=false", got)
	}

	rec = vaDo(t, f.srv, http.MethodPut, vaResourceGroupsPath(acc, rg), vaSystemSecret, "")
	var ok struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); rec.Code != http.StatusOK || err != nil || !ok.OK {
		t.Fatalf("join = %d %q (%v), want 200 {\"ok\":true}", rec.Code, rec.Body.String(), err)
	}
	f.requireRGAccounts(t, "joined", rg, acc)
	// Joining again is not an error.
	if rec := vaDo(t, f.srv, http.MethodPut, vaResourceGroupsPath(acc, rg), vaSystemSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("join again = %d, want 200", rec.Code)
	}
	rec = vaDo(t, f.srv, http.MethodGet, vaResourceGroupsPath(acc), vaSystemSecret, "")
	if got := decodeVaRGList(t, rec); !slices.Equal(got, []vaRGListEntry{{ID: rg, Name: "RG", Member: true}}) {
		t.Fatalf("list after join = %+v, want RG with member=true", got)
	}

	for range 2 {
		if rec := vaDo(t, f.srv, http.MethodDelete, vaResourceGroupsPath(acc, rg), vaSystemSecret, ""); rec.Code != http.StatusOK {
			t.Fatalf("leave = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	f.requireRGAccounts(t, "left", rg)
}

// The wire contract of every refusal of the owner self-service: method, authentication,
// path shape, existence, ownership, the CM's 403, level, root, reach.
func TestVendorAccountResourceGroupSelfServiceRefusals(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	rgX := f.resourceGroup(t, "RG under another root", f.agX)
	shared := f.accountAt(t, routing.VendorShareLevelShared)
	private := f.accountAt(t, routing.VendorShareLevelNone)

	for _, tc := range []struct {
		method, path string
		allow        string
	}{
		{http.MethodPost, vaResourceGroupsPath(shared), http.MethodGet},
		{http.MethodPut, vaResourceGroupsPath(shared), http.MethodGet},
		{http.MethodDelete, vaResourceGroupsPath(shared), http.MethodGet},
		{http.MethodPost, vaResourceGroupsPath(shared, rg), http.MethodPut + ", " + http.MethodDelete},
		{http.MethodGet, vaResourceGroupsPath(shared, rg), http.MethodPut + ", " + http.MethodDelete},
		{http.MethodPatch, vaResourceGroupsPath(shared, rg), http.MethodPut + ", " + http.MethodDelete},
	} {
		rec := vaDo(t, f.srv, tc.method, tc.path, vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d (Allow %q), want 405 with Allow: %s", tc.method, tc.path, rec.Code, rec.Header().Get("Allow"), tc.allow)
		}
	}
	if rec := vaDo(t, f.srv, http.MethodPut, vaResourceGroupsPath(shared, rg), "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous PUT = %d, want 401", rec.Code)
	}
	// A path deeper than {rgId} is the 404 of an unknown id.
	requireStatusCode(t, "a deeper path", vaDo(t, f.srv, http.MethodGet, vaResourceGroupsPath(shared, rg, "more"), vaSystemSecret, ""), http.StatusNotFound, "vendor_account.not_found")

	cases := []struct {
		name         string
		method, path string
		secret       string
		status       int
		code         string
	}{
		{"unknown account", http.MethodPut, vaResourceGroupsPath("va_missing", rg), vaSystemSecret, http.StatusNotFound, "vendor_account.not_found"},
		{"someone else's account (list)", http.MethodGet, vaResourceGroupsPath(shared), vaOtherSecret, http.StatusNotFound, "vendor_account.not_found"},
		{"someone else's account (join)", http.MethodPut, vaResourceGroupsPath(shared, rg), vaOtherSecret, http.StatusNotFound, "vendor_account.not_found"},
		{"someone else's account (leave)", http.MethodDelete, vaResourceGroupsPath(shared, rg), vaOtherSecret, http.StatusNotFound, "vendor_account.not_found"},
		{"a CM sees the account but is not its owner", http.MethodPut, vaResourceGroupsPath(shared, rg), vaCMSecret, http.StatusForbidden, "vendor_account.forbidden"},
		{"a private account (list)", http.MethodGet, vaResourceGroupsPath(private), vaSystemSecret, http.StatusBadRequest, "vendor_account.not_shared"},
		{"a private account (join)", http.MethodPut, vaResourceGroupsPath(private, rg), vaSystemSecret, http.StatusBadRequest, "vendor_account.not_shared"},
		{"a private account (leave)", http.MethodDelete, vaResourceGroupsPath(private, rg), vaSystemSecret, http.StatusBadRequest, "vendor_account.not_shared"},
		{"an unknown group (join)", http.MethodPut, vaResourceGroupsPath(shared, "rgrp_missing"), vaSystemSecret, http.StatusNotFound, resourceGroupNotFoundCode},
		{"an unknown group (leave)", http.MethodDelete, vaResourceGroupsPath(shared, "rgrp_missing"), vaSystemSecret, http.StatusNotFound, resourceGroupNotFoundCode},
		{"a group under another root", http.MethodPut, vaResourceGroupsPath(shared, rgX), vaSystemSecret, http.StatusBadRequest, "resource_group.vendor_account_system_group_mismatch"},
	}
	for _, tc := range cases {
		requireStatusCode(t, tc.name, vaDo(t, f.srv, tc.method, tc.path, tc.secret, ""), tc.status, tc.code)
	}
	f.requireRGAccounts(t, "nothing written (RG)", rg)
	f.requireRGAccounts(t, "nothing written (RG under another root)", rgX)
}

// The resource-group manager side over HTTP: the manager (system scope, who owns the
// account and so operates it) adds a `full` account and the response is the group with its
// accounts listed; replacing the set drops the surplus, an empty set clears it.
func TestResourceGroupVendorAccountsPutOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	a := f.accountAt(t, routing.VendorShareLevelFull)
	b := f.accountAt(t, routing.VendorShareLevelFull)

	put := func(ids ...string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string][]string{"vendor_account_ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		return vaDo(t, f.srv, http.MethodPut, rgVendorAccountsPath(rg), vaSystemSecret, string(body))
	}
	type response struct {
		ID             string `json:"id"`
		VendorAccounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"vendor_accounts"`
	}
	decode := func(rec *httptest.ResponseRecorder) response {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out response
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return out
	}

	got := decode(put(a, b))
	if got.ID != rg || len(got.VendorAccounts) != 2 {
		t.Fatalf("response = %+v, want the group with two accounts", got)
	}
	f.requireRGAccounts(t, "added", rg, a, b)
	if got := decode(put(a)); len(got.VendorAccounts) != 1 || got.VendorAccounts[0].ID != a {
		t.Fatalf("after dropping b: %+v", got)
	}
	f.requireRGAccounts(t, "replaced", rg, a)
	raw := vaDo(t, f.srv, http.MethodPut, rgVendorAccountsPath(rg), vaSystemSecret, `{}`)
	if got := decode(raw); got.VendorAccounts == nil || len(got.VendorAccounts) != 0 {
		t.Fatalf("after clearing: %+v (want vendor_accounts [])", got)
	}
	f.requireRGAccounts(t, "cleared", rg)
}

// The wire contract of every refusal of the manager side.
func TestResourceGroupVendorAccountsPutRefusals(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	rgX := f.resourceGroup(t, "RG under another root", f.agX)
	shared := f.accountAt(t, routing.VendorShareLevelShared)
	private := f.accountAt(t, routing.VendorShareLevelNone)
	full := f.accountAt(t, routing.VendorShareLevelFull)
	plain := vaCreate(t, f.srv, vaOwnerSecret, "owned by a plain user")

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, f.srv, method, rgVendorAccountsPath(rg), vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
			t.Errorf("%s = %d (Allow %q), want 405 with Allow: PUT", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, f.srv, method, rgVendorAccountCandidatesPath(rg), vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s candidates = %d (Allow %q), want 405 with Allow: GET", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := vaDo(t, f.srv, http.MethodPut, rgVendorAccountsPath(rg), "", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous PUT = %d, want 401", rec.Code)
	}

	body := func(ids ...string) string {
		b, err := json.Marshal(map[string][]string{"vendor_account_ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		name   string
		rg     string
		secret string
		body   string
		status int
		code   string
	}{
		{"malformed JSON", rg, vaSystemSecret, `{"vendor_account_ids":`, http.StatusBadRequest, codeRequestInvalidJSON},
		{"unknown group", "rgrp_missing", vaSystemSecret, body(full), http.StatusNotFound, resourceGroupNotFoundCode},
		{"a CM who does not manage the group", rg, vaCMSecret, body(full), http.StatusNotFound, resourceGroupNotFoundCode},
		{"a stranger", rg, vaOtherSecret, body(full), http.StatusNotFound, resourceGroupNotFoundCode},
		{"an unknown account", rg, vaSystemSecret, body("va_missing"), http.StatusNotFound, "vendor_account.not_found"},
		{"a plain user's account is no one else's, system scope included", rg, vaSystemSecret, body(plain.ID), http.StatusNotFound, "vendor_account.not_found"},
		{"a shared account is owner self-service only", rg, vaSystemSecret, body(shared), http.StatusForbidden, "vendor_account.forbidden"},
		{"a private account", rg, vaSystemSecret, body(private), http.StatusBadRequest, "vendor_account.not_shared"},
		{"a group under another root", rgX, vaSystemSecret, body(full), http.StatusBadRequest, "resource_group.vendor_account_system_group_mismatch"},
	}
	for _, tc := range cases {
		requireStatusCode(t, tc.name, vaDo(t, f.srv, http.MethodPut, rgVendorAccountsPath(tc.rg), tc.secret, tc.body), tc.status, tc.code)
	}
	f.requireRGAccounts(t, "nothing written (RG)", rg)
	f.requireRGAccounts(t, "nothing written (RG under another root)", rgX)
}

// The candidates over HTTP: what the caller may add (their own `full` accounts under the
// group's root), the group's own 404 for a caller who does not manage it, and a JSON [],
// not null, when there is nothing to offer.
func TestResourceGroupVendorAccountCandidatesOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	rgX := f.resourceGroup(t, "RG under another root", f.agX)
	full := f.accountAt(t, routing.VendorShareLevelFull)
	f.accountAt(t, routing.VendorShareLevelShared)
	f.accountAt(t, routing.VendorShareLevelNone)

	type candidate struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	get := func(rgID, secret string) []candidate {
		t.Helper()
		rec := vaDo(t, f.srv, http.MethodGet, rgVendorAccountCandidatesPath(rgID), secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET candidates of %s as %s = %d, body = %s", rgID, secret, rec.Code, rec.Body.String())
		}
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || string(raw.Data) == "null" || len(raw.Data) == 0 {
			t.Fatalf("body = %s (%v), want {\"data\":[...]}", rec.Body.String(), err)
		}
		var out []candidate
		if err := json.Unmarshal(raw.Data, &out); err != nil {
			t.Fatalf("decode data %s: %v", raw.Data, err)
		}
		return out
	}

	if got := get(rg, vaSystemSecret); len(got) != 1 || got[0].ID != full {
		t.Fatalf("candidates = %+v, want only the full account", got)
	}
	if got := get(rgX, vaSystemSecret); len(got) != 0 {
		t.Fatalf("candidates of a group under another root = %+v, want none", got)
	}
	requireStatusCode(t, "a CM who does not manage the group", vaDo(t, f.srv, http.MethodGet, rgVendorAccountCandidatesPath(rg), vaCMSecret, ""), http.StatusNotFound, resourceGroupNotFoundCode)
	requireStatusCode(t, "a stranger", vaDo(t, f.srv, http.MethodGet, rgVendorAccountCandidatesPath(rg), vaOtherSecret, ""), http.StatusNotFound, resourceGroupNotFoundCode)
	requireStatusCode(t, "an unknown group", vaDo(t, f.srv, http.MethodGet, rgVendorAccountCandidatesPath("rgrp_missing"), vaSystemSecret, ""), http.StatusNotFound, resourceGroupNotFoundCode)
	if rec := vaDo(t, f.srv, http.MethodGet, rgVendorAccountCandidatesPath(rg), "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", rec.Code)
	}
}

// The group's own GET lists its vendor accounts after a link, and the other sub-routes of
// a resource group are untouched.
func TestResourceGroupGetListsVendorAccounts(t *testing.T) {
	f := newAdminGroupsFixture(t)
	rg := f.resourceGroup(t, "RG", f.ag)
	full := f.accountAt(t, routing.VendorShareLevelFull)
	if rec := vaDo(t, f.srv, http.MethodPut, vaResourceGroupsPath(full, rg), vaSystemSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("join = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/resource-groups/"+rg, vaSystemSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET group = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		VendorAccounts []struct {
			ID string `json:"id"`
		} `json:"vendor_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil || len(dto.VendorAccounts) != 1 || dto.VendorAccounts[0].ID != full {
		t.Fatalf("GET group = %s (%v), want vendor_accounts [%s]", rec.Body.String(), err, full)
	}
}

// Every sentinel the link endpoints can answer has its row in the mapper of the endpoint
// that returns it: status and code, and a store failure is the endpoint's own 500.
func TestVendorAccountResourceGroupErrorRows(t *testing.T) {
	ownerRows := []struct {
		err    error
		status int
		code   string
	}{
		{portal.ErrVendorAccountsDisabled, http.StatusConflict, "vendor_accounts.module_disabled"},
		{portal.ErrVendorAccountSharingDisabled, http.StatusConflict, "vendor_accounts.sharing_disabled"},
		{portal.ErrVendorAccountNotFound, http.StatusNotFound, portal.CodeVendorAccountNotFound},
		{portal.ErrVendorAccountForbidden, http.StatusForbidden, "vendor_account.forbidden"},
		{portal.ErrVendorAccountNotShared, http.StatusBadRequest, "vendor_account.not_shared"},
		{portal.ErrResourceGroupNotFound, http.StatusNotFound, portal.CodeResourceGroupNotFound},
		{portal.ErrResourceGroupVendorAccountSystemGroupMismatch, http.StatusBadRequest, "resource_group.vendor_account_system_group_mismatch"},
		{errors.New("boom"), http.StatusInternalServerError, codeVendorAccountResourceGroupsFailed},
	}
	for _, tc := range ownerRows {
		rec := httptest.NewRecorder()
		writePortalVendorAccountError(rec, tc.err, codeVendorAccountResourceGroupsFailed)
		requireStatusCode(t, "owner mapper: "+tc.err.Error(), rec, tc.status, tc.code)
	}
	managerRows := append([]struct {
		err    error
		status int
		code   string
	}{}, ownerRows[:len(ownerRows)-1]...)
	managerRows = append(managerRows, struct {
		err    error
		status int
		code   string
	}{errors.New("boom"), http.StatusInternalServerError, "resource_group.update_failed"})
	for _, tc := range managerRows {
		rec := httptest.NewRecorder()
		writePortalResourceGroupError(rec, tc.err, codeResourceGroupUpdateFailed)
		requireStatusCode(t, "manager mapper: "+tc.err.Error(), rec, tc.status, tc.code)
	}
}
