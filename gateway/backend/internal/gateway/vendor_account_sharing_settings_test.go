// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/portal"
	"sync/atomic"
	"testing"
)

const vendorAccountSharingKey = "vendor_account_sharing_enabled"

// sharingPortalStub reports the vendor_account_sharing_enabled switch and nothing
// else (the nil-embedded-interface trick of subscriptionsPortalStub). reads counts
// the accessor calls so a test can prove the accessor is served from the
// gateway-side cache rather than the store; the value can be flipped between
// reads.
type sharingPortalStub struct {
	portal.API
	on    *atomic.Bool
	reads *atomic.Int32
}

func (f sharingPortalStub) VendorAccountSharingEnabled(context.Context) bool {
	f.reads.Add(1)
	return f.on.Load()
}

// The gateway's sharing read is served from a TTL cache (so a future per-request
// check does not issue a system_settings read), invalidateVendorSettingsCache
// drops it, and a Server with no portal reports OFF: sharing is a new capability,
// so an unreadable state fails closed (the opposite of the default-on
// subscriptions switch).
func TestVendorAccountSharingEnabledIsReadThroughTheCache(t *testing.T) {
	on, reads := &atomic.Bool{}, &atomic.Int32{}
	on.Store(true)
	s := &Server{Portal: sharingPortalStub{on: on, reads: reads}}
	ctx := context.Background()

	for range 5 {
		if !s.vendorAccountSharingEnabledCached(ctx) {
			t.Fatal("setting on: want true")
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("portal reads = %d after 5 cached reads, want 1", got)
	}

	// An out-of-band flip is not visible inside the TTL ...
	on.Store(false)
	if !s.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("flip inside the TTL: want the cached true")
	}
	// ... but the PUT handler's invalidation makes it visible on the very next read.
	s.invalidateVendorSettingsCache()
	if s.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("after invalidate: want the fresh false")
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("portal reads = %d after invalidate, want 2 (one re-read)", got)
	}
}

// With nothing set the accessor reports false, both for a Server with no portal
// and for a portal whose switch is off: fail closed, no sharing until an admin
// enables it.
func TestVendorAccountSharingEnabledCachedDefaultsToOff(t *testing.T) {
	ctx := context.Background()
	if (&Server{}).vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("nil portal must default to off (sharing is opt-in, fails closed)")
	}

	on, reads := &atomic.Bool{}, &atomic.Int32{}
	s := &Server{Portal: sharingPortalStub{on: on, reads: reads}}
	if s.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("portal switch unset: want false")
	}
	on.Store(true)
	s.invalidateVendorSettingsCache()
	if !s.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("after set + invalidate: want the cached true")
	}
}

// vendor_account_sharing_enabled round-trips through the system settings
// endpoint: the default is OFF, a PUT takes effect on the very next GET and on the
// gateway cache (the handler invalidates it), a PUT that omits it leaves it alone,
// and only a system principal can write it.
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
	// Prime the gateway cache with the default so the PUT below must invalidate it.
	if srv.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("default gateway accessor = true, want false")
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
	// The handler dropped the cached read: the toggle applies on the very next call.
	if !srv.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("gateway accessor still false after the PUT: the handler did not invalidate the cache")
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

	// Switching it back off applies on the next call too.
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"`+vendorAccountSharingKey+`":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT off = %d, body = %s", rec.Code, rec.Body.String())
	}
	if srv.vendorAccountSharingEnabledCached(ctx) {
		t.Fatal("gateway accessor still true after switching off: the handler did not invalidate the cache")
	}
	if got := get()[vendorAccountSharingKey]; got != false {
		t.Fatalf("GET after PUT off %s = %v, want false", vendorAccountSharingKey, got)
	}
}
