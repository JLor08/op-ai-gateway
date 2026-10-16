// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

// vendor_account_sharing_enabled is the master flag for sharing a vendor account
// with a resource group. Unlike vendor_subscriptions_enabled it is default OFF
// (opt-in, fail-closed): an absent, blank or unparseable value must read as
// false, so only an explicit true switches sharing on.
func TestVendorAccountSharingEnabledHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"nil -> off", nil, false},
		{"absent -> off", map[string]string{}, false},
		{"blank -> off", map[string]string{vendorAccountSharingEnabledKey: ""}, false},
		{"unparseable -> off", map[string]string{vendorAccountSharingEnabledKey: "maybe"}, false},
		{"explicit true", map[string]string{vendorAccountSharingEnabledKey: "true"}, true},
		{"padded true", map[string]string{vendorAccountSharingEnabledKey: " true "}, true},
		{"explicit false", map[string]string{vendorAccountSharingEnabledKey: "false"}, false},
		{"padded false", map[string]string{vendorAccountSharingEnabledKey: " false "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorAccountSharingEnabled(tc.in); got != tc.want {
				t.Fatalf("VendorAccountSharingEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The setting key is part of the persisted contract (a row in system_settings),
// so it is pinned by name.
func TestVendorAccountSharingEnabledKeyName(t *testing.T) {
	if vendorAccountSharingEnabledKey != "vendor_account_sharing_enabled" {
		t.Fatalf("key = %q, want vendor_account_sharing_enabled", vendorAccountSharingEnabledKey)
	}
}

// The service accessor, the settings view and a missing/unreadable store all agree
// on OFF: sharing is a new capability, so an unreadable flag fails closed.
func TestVendorAccountSharingEnabledDefaultsToOff(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*Service{
		"empty store":  NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()}),
		"nil store":    NewService(ServiceDeps{Clock: fixedClock()}),
		"read failure": NewService(ServiceDeps{SystemSettings: failingSettingsReadStore{NewMemorySystemSettings()}, Clock: fixedClock()}),
	}
	for name, svc := range stores {
		t.Run(name, func(t *testing.T) {
			if svc.VendorAccountSharingEnabled(ctx) {
				t.Fatal("VendorAccountSharingEnabled = true, want false (default off, fails closed)")
			}
			if svc.SystemSettingsView(ctx).VendorAccountSharingEnabled {
				t.Fatal("view.VendorAccountSharingEnabled = true, want false (default off)")
			}
		})
	}
}

func TestUpdateSystemSettingsPersistsVendorAccountSharingEnabled(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	on := true
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &on})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(on): %v", err)
	}
	if !got.VendorAccountSharingEnabled {
		t.Fatal("returned DTO VendorAccountSharingEnabled = false after turning it on")
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[vendorAccountSharingEnabledKey] != "true" {
		t.Fatalf("stored vendor_account_sharing_enabled = %q, want %q", values[vendorAccountSharingEnabledKey], "true")
	}
	if !svc.VendorAccountSharingEnabled(ctx) || !svc.SystemSettingsView(ctx).VendorAccountSharingEnabled {
		t.Fatal("accessor/view report off after turning it on")
	}

	// A request that omits the field keeps the stored value (nil = keep).
	enabled := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &enabled}); err != nil {
		t.Fatalf("UpdateSystemSettings(other field): %v", err)
	}
	if !svc.VendorAccountSharingEnabled(ctx) {
		t.Fatal("an update that omitted vendor_account_sharing_enabled changed it")
	}

	off := false
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &off}); err != nil {
		t.Fatalf("UpdateSystemSettings(off): %v", err)
	}
	if values, _ = settings.SystemSettings(ctx); values[vendorAccountSharingEnabledKey] != "false" {
		t.Fatalf("stored vendor_account_sharing_enabled = %q, want %q", values[vendorAccountSharingEnabledKey], "false")
	}
	if svc.VendorAccountSharingEnabled(ctx) {
		t.Fatal("accessor reports on after turning it back off")
	}
}

// The flag is independent of the other two vendor switches: writing it must not
// touch vendor_accounts_enabled or vendor_subscriptions_enabled.
func TestUpdateSystemSettingsVendorAccountSharingEnabledLeavesOtherVendorFlagsAlone(t *testing.T) {
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
	for _, key := range []string{vendorAccountsEnabledKey, vendorSubscriptionsEnabledKey} {
		if v, ok := values[key]; ok {
			t.Fatalf("writing the sharing flag also wrote %s = %q", key, v)
		}
	}
	if svc.VendorAccountsEnabled(ctx) {
		t.Fatal("the master vendor flag switched on as a side effect")
	}
	if !svc.VendorSubscriptionsEnabled(ctx) {
		t.Fatal("the subscriptions switch left its default as a side effect")
	}
}

func TestUpdateSystemSettingsVendorAccountSharingEnabledRequiresSystemScope(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	on := true
	if _, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &on}); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("plain admin err = %v, want ErrPrincipalForbidden", err)
	}
	if svc.VendorAccountSharingEnabled(context.Background()) {
		t.Fatal("a forbidden update switched sharing on")
	}
}
