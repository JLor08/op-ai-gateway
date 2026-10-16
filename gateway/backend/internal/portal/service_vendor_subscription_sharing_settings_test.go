// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

// vendor_subscription_sharing_enabled gates whether SUBSCRIPTION-type vendor
// accounts may be shared (spec section 11); it is meaningful only with the master
// vendor_account_sharing_enabled on. Unlike vendor_subscriptions_enabled it is
// default OFF (opt-in, fail-closed): an absent, blank or unparseable value must
// read as false, so only an explicit true switches subscription sharing on.
func TestVendorSubscriptionSharingEnabledHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"nil -> off", nil, false},
		{"absent -> off", map[string]string{}, false},
		{"blank -> off", map[string]string{vendorSubscriptionSharingEnabledKey: ""}, false},
		{"unparseable -> off", map[string]string{vendorSubscriptionSharingEnabledKey: "maybe"}, false},
		{"explicit true", map[string]string{vendorSubscriptionSharingEnabledKey: "true"}, true},
		{"padded true", map[string]string{vendorSubscriptionSharingEnabledKey: " true "}, true},
		{"explicit false", map[string]string{vendorSubscriptionSharingEnabledKey: "false"}, false},
		{"padded false", map[string]string{vendorSubscriptionSharingEnabledKey: " false "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorSubscriptionSharingEnabled(tc.in); got != tc.want {
				t.Fatalf("VendorSubscriptionSharingEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The setting key is part of the persisted contract (a row in system_settings),
// so it is pinned by name.
func TestVendorSubscriptionSharingEnabledKeyName(t *testing.T) {
	if vendorSubscriptionSharingEnabledKey != "vendor_subscription_sharing_enabled" {
		t.Fatalf("key = %q, want vendor_subscription_sharing_enabled", vendorSubscriptionSharingEnabledKey)
	}
}

// The service accessor, the settings view and a missing/unreadable store all agree
// on OFF: subscription sharing is opt-in, so an unreadable flag fails closed.
func TestVendorSubscriptionSharingEnabledDefaultsToOff(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*Service{
		"empty store":  NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()}),
		"nil store":    NewService(ServiceDeps{Clock: fixedClock()}),
		"read failure": NewService(ServiceDeps{SystemSettings: failingSettingsReadStore{NewMemorySystemSettings()}, Clock: fixedClock()}),
	}
	for name, svc := range stores {
		t.Run(name, func(t *testing.T) {
			if svc.VendorSubscriptionSharingEnabled(ctx) {
				t.Fatal("VendorSubscriptionSharingEnabled = true, want false (default off, fails closed)")
			}
			if svc.SystemSettingsView(ctx).VendorSubscriptionSharingEnabled {
				t.Fatal("view.VendorSubscriptionSharingEnabled = true, want false (default off)")
			}
		})
	}
}

func TestUpdateSystemSettingsPersistsVendorSubscriptionSharingEnabled(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	on := true
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &on})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(on): %v", err)
	}
	if !got.VendorSubscriptionSharingEnabled {
		t.Fatal("returned DTO VendorSubscriptionSharingEnabled = false after turning it on")
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[vendorSubscriptionSharingEnabledKey] != "true" {
		t.Fatalf("stored vendor_subscription_sharing_enabled = %q, want %q", values[vendorSubscriptionSharingEnabledKey], "true")
	}
	if !svc.VendorSubscriptionSharingEnabled(ctx) || !svc.SystemSettingsView(ctx).VendorSubscriptionSharingEnabled {
		t.Fatal("accessor/view report off after turning it on")
	}

	// A request that omits the field keeps the stored value (nil = keep).
	enabled := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &enabled}); err != nil {
		t.Fatalf("UpdateSystemSettings(other field): %v", err)
	}
	if !svc.VendorSubscriptionSharingEnabled(ctx) {
		t.Fatal("an update that omitted vendor_subscription_sharing_enabled changed it")
	}

	off := false
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &off}); err != nil {
		t.Fatalf("UpdateSystemSettings(off): %v", err)
	}
	if values, _ = settings.SystemSettings(ctx); values[vendorSubscriptionSharingEnabledKey] != "false" {
		t.Fatalf("stored vendor_subscription_sharing_enabled = %q, want %q", values[vendorSubscriptionSharingEnabledKey], "false")
	}
	if svc.VendorSubscriptionSharingEnabled(ctx) {
		t.Fatal("accessor reports on after turning it back off")
	}
}

// The flag is independent of the other vendor switches: writing it must not touch
// vendor_accounts_enabled, vendor_subscriptions_enabled or the master
// vendor_account_sharing_enabled (it only narrows the master, it never implies it).
func TestUpdateSystemSettingsVendorSubscriptionSharingEnabledLeavesOtherVendorFlagsAlone(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &on}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	for _, key := range []string{vendorAccountsEnabledKey, vendorSubscriptionsEnabledKey, vendorAccountSharingEnabledKey} {
		if v, ok := values[key]; ok {
			t.Fatalf("writing the subscription-sharing flag also wrote %s = %q", key, v)
		}
	}
	if svc.VendorAccountsEnabled(ctx) {
		t.Fatal("the master vendor flag switched on as a side effect")
	}
	if !svc.VendorSubscriptionsEnabled(ctx) {
		t.Fatal("the subscriptions switch left its default as a side effect")
	}
	if svc.VendorAccountSharingEnabled(ctx) {
		t.Fatal("the master sharing switch turned on as a side effect")
	}
}

// And the other way round: switching the master sharing flag on does not switch
// subscription sharing on, so enabling sharing never opens the ToS-restricted
// subscription path by itself.
func TestUpdateSystemSettingsVendorAccountSharingEnabledLeavesSubscriptionSharingOff(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &on}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if v, ok := values[vendorSubscriptionSharingEnabledKey]; ok {
		t.Fatalf("writing the master sharing flag also wrote %s = %q", vendorSubscriptionSharingEnabledKey, v)
	}
	if svc.VendorSubscriptionSharingEnabled(ctx) {
		t.Fatal("subscription sharing switched on as a side effect of enabling the master flag")
	}
	if svc.SystemSettingsView(ctx).VendorSubscriptionSharingEnabled {
		t.Fatal("view.VendorSubscriptionSharingEnabled = true after enabling only the master flag")
	}
}

func TestUpdateSystemSettingsVendorSubscriptionSharingEnabledRequiresSystemScope(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	on := true
	if _, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &on}); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("plain admin err = %v, want ErrPrincipalForbidden", err)
	}
	if svc.VendorSubscriptionSharingEnabled(context.Background()) {
		t.Fatal("a forbidden update switched subscription sharing on")
	}
}
