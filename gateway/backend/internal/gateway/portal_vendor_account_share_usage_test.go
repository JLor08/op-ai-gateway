// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
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

// A new account shares its usage with members unless the creator opts out; an
// account stored earlier with the opt-in off keeps it across a read and an edit
// (spec 10.1: no data migration).
func TestCreateVendorAccountSharesUsageByDefault(t *testing.T) {
	srv, routes := newVendorAccountTestServer(t, true)
	const path = "/api/portal/vendor-accounts"

	dto := vaCreate(t, srv, vaOwnerSecret, "default")
	if !dto.ShareUsageWithMembers {
		t.Errorf("create without the field: share_usage_with_members = false, want true")
	}

	body := strings.Replace(vaCreateBody("opt out"), `"name"`, `"share_usage_with_members":false,"name"`, 1)
	rec := vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create opt-out = %d, body = %s", rec.Code, rec.Body.String())
	}
	if vaDecode(t, rec).ShareUsageWithMembers {
		t.Error("create with false: share_usage_with_members = true, want false")
	}

	body = strings.Replace(vaCreateBody("opt in"), `"name"`, `"share_usage_with_members":true,"name"`, 1)
	rec = vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, body)
	if rec.Code != http.StatusCreated || !vaDecode(t, rec).ShareUsageWithMembers {
		t.Errorf("create with true = %d, body = %s", rec.Code, rec.Body.String())
	}

	// An account that predates the default (stored with the opt-in off) stays off.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := routes.CreateVendorAccount(context.Background(), routing.VendorAccount{
		ID: "va_legacy", OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: "legacy", Status: routing.VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed legacy account: %v", err)
	}
	rec = vaDo(t, srv, http.MethodGet, path+"/va_legacy", vaOwnerSecret, "")
	if rec.Code != http.StatusOK || vaDecode(t, rec).ShareUsageWithMembers {
		t.Errorf("GET legacy = %d, body = %s, want share_usage_with_members false", rec.Code, rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodPatch, path+"/va_legacy", vaOwnerSecret, `{"name":"legacy renamed"}`)
	if rec.Code != http.StatusOK || vaDecode(t, rec).ShareUsageWithMembers {
		t.Errorf("PATCH legacy = %d, body = %s, want share_usage_with_members false", rec.Code, rec.Body.String())
	}
	if acc, err := routes.VendorAccountByID(context.Background(), "va_legacy"); err != nil || acc.ShareUsageWithMembers {
		t.Errorf("stored legacy opt-in = %v (err %v), want false", acc.ShareUsageWithMembers, err)
	}
}

func decisionEmulationPath(id string) string {
	return "/api/portal/vendor-accounts/" + id + "/decision-emulation"
}

// PUT .../decision-emulation (#380): owner-only, OpenAI subscription accounts only.
func TestVendorAccountDecisionEmulationOverHTTP(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	sub := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")
	path := decisionEmulationPath(sub.ID)

	if rec := vaDo(t, srv, http.MethodGet, path, vaOwnerSecret, ""); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
		t.Errorf("GET = %d (Allow %q), want 405 with Allow: PUT", rec.Code, rec.Header().Get("Allow"))
	}
	requireStatusCode(t, "missing enabled", vaDo(t, srv, http.MethodPut, path, vaOwnerSecret, `{}`), http.StatusBadRequest, codeRequestInvalidJSON)
	requireStatusCode(t, "stranger", vaDo(t, srv, http.MethodPut, path, vaSystemSecret, `{"enabled":true}`), http.StatusNotFound, "vendor_account.not_found")

	rec := vaDo(t, srv, http.MethodPut, path, vaOwnerSecret, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT = %d, body = %s", rec.Code, rec.Body.String())
	}
	if dto := vaDecode(t, rec); !dto.DecisionEmulation || dto.ID != sub.ID {
		t.Errorf("dto = %+v, want decision_emulation true", dto)
	}
	if !strings.Contains(rec.Body.String(), `"decision_emulation":true`) {
		t.Errorf("body lacks decision_emulation: %s", rec.Body.String())
	}

	key := vaCreate(t, srv, vaOwnerSecret, "plain key")
	requireStatusCode(t, "api key account", vaDo(t, srv, http.MethodPut, decisionEmulationPath(key.ID), vaOwnerSecret, `{"enabled":true}`), http.StatusConflict, "vendor_account.decision_emulation_unsupported")
	ant := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")
	requireStatusCode(t, "anthropic subscription", vaDo(t, srv, http.MethodPut, decisionEmulationPath(ant.ID), vaOwnerSecret, `{"enabled":true}`), http.StatusConflict, "vendor_account.decision_emulation_unsupported")
}
