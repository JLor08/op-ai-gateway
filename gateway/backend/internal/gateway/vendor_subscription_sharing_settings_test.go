// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const vendorSubscriptionSharingKey = "vendor_subscription_sharing_enabled"

// vendor_subscription_sharing_enabled round-trips through the system settings
// endpoint: the default is OFF, a PUT takes effect on the very next GET, a PUT
// that omits it leaves it alone, and only a system principal can write it.
func TestSystemSettingsVendorSubscriptionSharingEnabled(t *testing.T) {
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

	if got := get()[vendorSubscriptionSharingKey]; got != false {
		t.Fatalf("default %s = %v, want false (opt-in)", vendorSubscriptionSharingKey, got)
	}

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorSubscriptionSharingKey+`":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT on = %d, body = %s", rec.Code, rec.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if putBody[vendorSubscriptionSharingKey] != true {
		t.Fatalf("PUT response %s = %v, want true", vendorSubscriptionSharingKey, putBody[vendorSubscriptionSharingKey])
	}
	if got := get()[vendorSubscriptionSharingKey]; got != true {
		t.Fatalf("GET after PUT %s = %v, want true", vendorSubscriptionSharingKey, got)
	}
	if values, err := settings.SystemSettings(ctx); err != nil || values[vendorSubscriptionSharingKey] != "true" {
		t.Fatalf("stored = %v, %v, want %s=true", values, err, vendorSubscriptionSharingKey)
	}

	// A PUT of an unrelated vendor field leaves the flag alone ...
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT other field = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[vendorSubscriptionSharingKey]; got != true {
		t.Fatalf("GET after an unrelated PUT %s = %v, want the kept true", vendorSubscriptionSharingKey, got)
	}

	// ... and only a system principal can write it.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaOwnerSecret, `{"`+vendorSubscriptionSharingKey+`":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as a plain user = %d, want 403", rec.Code)
	}
	if got := get()[vendorSubscriptionSharingKey]; got != true {
		t.Fatalf("a forbidden PUT changed %s to %v", vendorSubscriptionSharingKey, got)
	}

	// Switching it back off applies on the next GET too.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorSubscriptionSharingKey+`":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT off = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get()[vendorSubscriptionSharingKey]; got != false {
		t.Fatalf("GET after PUT off %s = %v, want false", vendorSubscriptionSharingKey, got)
	}
}

// The two sharing switches are independent end to end: a PUT that switches the
// master vendor_account_sharing_enabled on leaves the subscription switch OFF, so
// enabling sharing never opens the ToS-restricted subscription path by itself.
func TestSystemSettingsMasterSharingPutLeavesSubscriptionSharingOff(t *testing.T) {
	srv, _, _ := newVendorAccountSettingsTestServer(t, true)

	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_account_sharing_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT master on = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal PUT: %v", err)
	}
	if body["vendor_account_sharing_enabled"] != true {
		t.Fatalf("PUT response master = %v, want true", body["vendor_account_sharing_enabled"])
	}
	if body[vendorSubscriptionSharingKey] != false {
		t.Fatalf("PUT response %s = %v, want false", vendorSubscriptionSharingKey, body[vendorSubscriptionSharingKey])
	}

	// The stored state agrees on the next GET: master on, subscription sharing off.
	rec = vaDo(t, srv, http.MethodGet, "/api/system/settings", vaSystemSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d, body = %s", rec.Code, rec.Body.String())
	}
	body = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if body["vendor_account_sharing_enabled"] != true || body[vendorSubscriptionSharingKey] != false {
		t.Fatalf("GET master = %v, %s = %v, want true / false", body["vendor_account_sharing_enabled"], vendorSubscriptionSharingKey, body[vendorSubscriptionSharingKey])
	}
}
