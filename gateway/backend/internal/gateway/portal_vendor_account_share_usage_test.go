// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/routing"
	"testing"
)

// PUT /api/portal/vendor-accounts/{id}/share-usage (#407): the owner's opt-in to show
// the account's usage figures to everyone allowed to use it. portal.Service decides who
// may; these tests pin the route, the body and the status of each refusal.

func shareUsagePath(id string) string { return "/api/portal/vendor-accounts/" + id + "/share-usage" }

func TestVendorAccountShareUsageOverHTTP(t *testing.T) {
	f, id := vaMemberFixture(t, routing.VendorShareLevelShared)
	path := shareUsagePath(id)

	stored := func() bool {
		t.Helper()
		acc, err := f.routes.VendorAccountByID(context.Background(), id)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		return acc.ShareUsageWithMembers
	}

	// A member (403) and a stranger (404) change nothing.
	requireStatusCode(t, "member PUT", vaDo(t, f.srv, http.MethodPut, path, vaOtherSecret, `{"enabled":true}`), http.StatusForbidden, "vendor_account.forbidden")
	requireStatusCode(t, "stranger PUT", vaDo(t, f.srv, http.MethodPut, path, vaOwnerSecret, `{"enabled":true}`), http.StatusNotFound, "vendor_account.not_found")
	if stored() {
		t.Fatal("a refused PUT wrote the opt-in")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, f.srv, method, path, vaSystemSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
			t.Errorf("%s = %d (Allow %q), want 405 with Allow: PUT", method, rec.Code, rec.Header().Get("Allow"))
		}
	}

	// A malformed body is a 400 before anything is written.
	requireStatusCode(t, "malformed body", vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{"enabled":`), http.StatusBadRequest, codeRequestInvalidJSON)

	// A body without `enabled` is a 400 and writes nothing (it must not read as false).
	if rec := vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d", rec.Code)
	}
	requireStatusCode(t, "missing enabled", vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{}`), http.StatusBadRequest, codeRequestInvalidJSON)
	if !stored() {
		t.Fatal("a PUT without enabled switched the opt-in off")
	}
	if rec := vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("reset PUT = %d", rec.Code)
	}

	// The owner turns it on: 200 with the management DTO, and the store agrees.
	rec := vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT = %d, body = %s", rec.Code, rec.Body.String())
	}
	if dto := vaDecode(t, rec); dto.ID != id || !dto.ShareUsageWithMembers || !dto.IsOwner || !stored() {
		t.Errorf("owner PUT: dto %+v, stored %v, want the management DTO with share_usage_with_members", dto, stored())
	}

	// The member now reads the figures, and the flag that unlocked them.
	_, body, raw := vaGetRaw(t, f, vaOtherSecret, id)
	if body["usage"] == nil {
		t.Errorf("member read after opt-in lacks usage: %s", raw)
	}
	if body["share_usage_with_members"] != true {
		t.Errorf("member read reports share_usage_with_members = %v, want true: %s", body["share_usage_with_members"], raw)
	}

	// And off again: the member loses usage and reads the flag false.
	defer func() {
		_, body, raw := vaGetRaw(t, f, vaOtherSecret, id)
		if body["usage"] != nil || body["share_usage_with_members"] != false {
			t.Errorf("member read after opt-out: %s", raw)
		}
	}()
	rec = vaDo(t, f.srv, http.MethodPut, path, vaSystemSecret, `{"enabled":false}`)
	if rec.Code != http.StatusOK || vaDecode(t, rec).ShareUsageWithMembers || stored() {
		t.Errorf("owner PUT off = %d %s, stored %v", rec.Code, rec.Body.String(), stored())
	}
}
