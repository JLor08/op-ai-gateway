// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const vendorModuleDisabledCode = "vendor_accounts.module_disabled"

// With the vendor_accounts_enabled master flag at its default (OFF) every
// vendor-account endpoint answers 409 vendor_accounts.module_disabled, for the
// owner and a stranger alike (the disabled area does not even say whether an
// account exists), and nothing is written.
func TestVendorAccountEndpointsAre409WhileTheMasterFlagIsOff(t *testing.T) {
	srv, routeStore, _ := newVendorAccountSettingsTestServer(t, true)

	cases := []struct {
		name         string
		method, path string
		secret, body string
	}{
		{"list", http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, ""},
		{"create", http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaCreateBody("Should not exist")},
		{"get", http.MethodGet, "/api/portal/vendor-accounts/va_anything", vaOwnerSecret, ""},
		{"patch", http.MethodPatch, "/api/portal/vendor-accounts/va_anything", vaOwnerSecret, `{"name":"x"}`},
		{"delete", http.MethodDelete, "/api/portal/vendor-accounts/va_anything", vaOwnerSecret, ""},
		{"connect import", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/import", vaOwnerSecret, `{"access_token":"t"}`},
		{"connect begin", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/begin", vaOwnerSecret, ""},
		{"connect complete", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/complete", vaOwnerSecret, `{"code":"c"}`},
		{"check", http.MethodPost, "/api/portal/vendor-accounts/va_anything/check", vaOwnerSecret, ""},
		{"connect device begin", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/device/begin", vaOwnerSecret, ""},
		{"connect device poll", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/device/poll", vaOwnerSecret, ""},
		{"connect begin as another user", http.MethodPost, "/api/portal/vendor-accounts/va_anything/connect/begin", vaOtherSecret, ""},
		{"list as another user", http.MethodGet, "/api/portal/vendor-accounts", vaOtherSecret, ""},
		// Even a system principal is refused: the flag is a module switch, not an ACL.
		{"list as system", http.MethodGet, "/api/portal/vendor-accounts", vaSystemSecret, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, tc.method, tc.path, tc.secret, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != vendorModuleDisabledCode {
				t.Fatalf("code = %q, want %q", code, vendorModuleDisabledCode)
			}
		})
	}
	if rows, err := routeStore.VendorAccounts(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("rows after refused calls = %#v, %v, want none persisted", rows, err)
	}

	// Authentication still comes first: an anonymous caller is a 401, not a 409.
	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list while disabled = %d, want 401", rec.Code)
	}
}

// Flipping the flag through the system settings endpoint switches the area on
// and off live: ON -> the endpoints work, OFF again -> 409, and the account
// created in between survives the round trip.
func TestVendorAccountEndpointsFollowTheMasterFlagLive(t *testing.T) {
	srv, _, _ := newVendorAccountSettingsTestServer(t, true)

	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, ""); rec.Code != http.StatusConflict {
		t.Fatalf("list before enabling = %d, want 409", rec.Code)
	}

	enableVendorAccountsFlag(t, srv)
	created := vaCreate(t, srv, vaOwnerSecret, "Works when on")
	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+created.ID, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("get while enabled = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable flag = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+created.ID, vaOwnerSecret, "")
	if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorModuleDisabledCode {
		t.Fatalf("get after disabling = %d %s, want 409 %s", rec.Code, rec.Body.String(), vendorModuleDisabledCode)
	}

	enableVendorAccountsFlag(t, srv)
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list after re-enabling = %d %s, want 200 with the account kept", rec.Code, rec.Body.String())
	}
}

// GET /api/portal/vendor-accounts/enabled is the boolean-only module flag the
// portal shell reads (any authenticated user, no config leak) to show or hide
// the "Anbieter" menu item. It answers while the module is off (that is its
// whole point) and is not shadowed by the {id} item route.
func TestVendorAccountsEnabledEndpoint(t *testing.T) {
	srv, _, _ := newVendorAccountSettingsTestServer(t, true)
	const path = "/api/portal/vendor-accounts/enabled"

	moduleEnabled := func(secret string) bool {
		t.Helper()
		rec := vaDo(t, srv, http.MethodGet, path, secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200, body = %s", path, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
		}
		if len(body) != 1 {
			t.Fatalf("body = %v, want exactly the module_enabled boolean", body)
		}
		enabled, ok := body["module_enabled"].(bool)
		if !ok {
			t.Fatalf("module_enabled = %#v, want a boolean (%s)", body["module_enabled"], rec.Body.String())
		}
		return enabled
	}

	if moduleEnabled(vaOwnerSecret) {
		t.Fatal("module_enabled = true by default, want false (opt-in)")
	}
	enableVendorAccountsFlag(t, srv)
	if !moduleEnabled(vaOwnerSecret) || !moduleEnabled(vaOtherSecret) {
		t.Fatal("module_enabled = false after enabling, want true for every authenticated user")
	}

	if rec := vaDo(t, srv, http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d, want 401", rec.Code)
	}
	rec := vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, `{}`)
	if rec.Code != http.StatusMethodNotAllowed || perfErrorCode(t, rec.Body.Bytes()) != codeRequestMethodNotAllowed {
		t.Fatalf("POST = %d %s, want 405 %s", rec.Code, rec.Body.String(), codeRequestMethodNotAllowed)
	}
}

// The two settings round-trip through the system settings endpoint, the defaults
// are OFF / vendor_first, and an unknown routing mode is a 400 that stores nothing.
func TestSystemSettingsVendorAccountSettings(t *testing.T) {
	srv, _, settings := newVendorAccountSettingsTestServer(t, true)

	get := func() map[string]any {
		t.Helper()
		rec := vaDo(t, srv, http.MethodGet, "/api/system/settings", vaSystemSecret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return body
	}
	if body := get(); body["vendor_accounts_enabled"] != false || body["vendor_account_routing_mode"] != "vendor_first" {
		t.Fatalf("defaults = %v/%v, want false/vendor_first", body["vendor_accounts_enabled"], body["vendor_account_routing_mode"])
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true,"vendor_account_routing_mode":"fallback_only"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := get(); body["vendor_accounts_enabled"] != true || body["vendor_account_routing_mode"] != "fallback_only" {
		t.Fatalf("after PUT = %v/%v, want true/fallback_only", body["vendor_accounts_enabled"], body["vendor_account_routing_mode"])
	}

	rec = vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":false,"vendor_account_routing_mode":"sideways"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT unknown mode = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "system.vendor_account_routing_mode_invalid" {
		t.Fatalf("code = %q, want system.vendor_account_routing_mode_invalid", code)
	}
	// The rejected request applied nothing: not the mode, and not the valid flag beside it.
	if body := get(); body["vendor_accounts_enabled"] != true || body["vendor_account_routing_mode"] != "fallback_only" {
		t.Fatalf("after rejected PUT = %v/%v, want the earlier true/fallback_only kept", body["vendor_accounts_enabled"], body["vendor_account_routing_mode"])
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil || values["vendor_account_routing_mode"] != "fallback_only" {
		t.Fatalf("stored = %v, %v, want fallback_only kept", values, err)
	}

	// A non-system principal cannot read or write them.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaOwnerSecret, `{"vendor_accounts_enabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as a plain user = %d, want 403", rec.Code)
	}
}

// vendor_openai_codex_client_version round-trips through the system settings
// endpoint: the default is the built-in Codex app version, a PUT takes effect on
// the very next GET, blank resets to the default, a malformed value is a 400 that
// stores nothing (not even the valid field beside it), and only a system
// principal can write it.
func TestSystemSettingsVendorOpenAICodexClientVersion(t *testing.T) {
	srv, _, settings := newVendorAccountSettingsTestServer(t, true)
	const key = "vendor_openai_codex_client_version"

	get := func() map[string]any {
		t.Helper()
		rec := vaDo(t, srv, http.MethodGet, "/api/system/settings", vaSystemSecret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return body
	}
	if got := get()[key]; got != "26.930.61225" {
		t.Fatalf("default %s = %v, want 26.930.61225", key, got)
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+key+`":" 27.101.40000 "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body = %s", rec.Code, rec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if putBody[key] != "27.101.40000" {
		t.Fatalf("PUT response %s = %v, want the trimmed 27.101.40000", key, putBody[key])
	}
	if got := get()[key]; got != "27.101.40000" {
		t.Fatalf("GET after PUT %s = %v, want 27.101.40000", key, got)
	}

	// A PUT that omits the field leaves it alone.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT other field = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[key]; got != "27.101.40000" {
		t.Fatalf("GET after an unrelated PUT %s = %v, want the kept 27.101.40000", key, got)
	}

	// A malformed value is a 400 with its stable code; nothing in the request is applied.
	rec = vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":false,"`+key+`":"v1 2"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT malformed = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "system.vendor_openai_codex_client_version_invalid" {
		t.Fatalf("code = %q, want system.vendor_openai_codex_client_version_invalid", code)
	}
	body := get()
	if body[key] != "27.101.40000" || body["vendor_accounts_enabled"] != true {
		t.Fatalf("after rejected PUT = %v/%v, want the earlier 27.101.40000/true kept", body[key], body["vendor_accounts_enabled"])
	}
	if values, err := settings.SystemSettings(context.Background()); err != nil || values[key] != "27.101.40000" {
		t.Fatalf("stored = %v, %v, want 27.101.40000 kept", values, err)
	}

	// Blank resets to the built-in default.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+key+`":"  "}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT blank = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[key]; got != "26.930.61225" {
		t.Fatalf("GET after blank PUT %s = %v, want the default 26.930.61225", key, got)
	}

	// A non-system principal cannot write it.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaOwnerSecret, `{"`+key+`":"27.101.40000"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as a plain user = %d, want 403", rec.Code)
	}
}
