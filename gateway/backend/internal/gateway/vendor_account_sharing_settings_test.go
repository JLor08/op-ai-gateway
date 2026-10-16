// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const vendorAccountSharingKey = "vendor_account_sharing_enabled"

// vendor_account_sharing_enabled round-trips through the system settings
// endpoint: the default is OFF, a PUT takes effect on the very next GET, a PUT
// that omits it leaves it alone, and only a system principal can write it.
func TestSystemSettingsVendorAccountSharingEnabled(t *testing.T) {
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
	ctx := context.Background()

	if got := get()[vendorAccountSharingKey]; got != false {
		t.Fatalf("default %s = %v, want false (opt-in)", vendorAccountSharingKey, got)
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorAccountSharingKey+`":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT on = %d, body = %s", rec.Code, rec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if putBody[vendorAccountSharingKey] != true {
		t.Fatalf("PUT response %s = %v, want true", vendorAccountSharingKey, putBody[vendorAccountSharingKey])
	}
	if got := get()[vendorAccountSharingKey]; got != true {
		t.Fatalf("GET after PUT %s = %v, want true", vendorAccountSharingKey, got)
	}
	if values, err := settings.SystemSettings(ctx); err != nil || values[vendorAccountSharingKey] != "true" {
		t.Fatalf("stored = %v, %v, want %s=true", values, err, vendorAccountSharingKey)
	}

	// A PUT of an unrelated vendor field leaves the flag alone ...
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT other field = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[vendorAccountSharingKey]; got != true {
		t.Fatalf("GET after an unrelated PUT %s = %v, want the kept true", vendorAccountSharingKey, got)
	}

	// ... and only a system principal can write it.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaOwnerSecret, `{"`+vendorAccountSharingKey+`":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as a plain user = %d, want 403", rec.Code)
	}
	if got := get()[vendorAccountSharingKey]; got != true {
		t.Fatalf("a forbidden PUT changed %s to %v", vendorAccountSharingKey, got)
	}

	// Switching it back off applies on the next GET too.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorAccountSharingKey+`":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT off = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[vendorAccountSharingKey]; got != false {
		t.Fatalf("GET after PUT off %s = %v, want false", vendorAccountSharingKey, got)
	}
}
