// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

func TestVendorAccountsEnabledHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"nil -> off", nil, false},
		{"absent -> off", map[string]string{}, false},
		{"blank -> off", map[string]string{vendorAccountsEnabledKey: ""}, false},
		{"unparseable -> off", map[string]string{vendorAccountsEnabledKey: "maybe"}, false},
		{"explicit false", map[string]string{vendorAccountsEnabledKey: "false"}, false},
		{"explicit true", map[string]string{vendorAccountsEnabledKey: "true"}, true},
		{"padded true", map[string]string{vendorAccountsEnabledKey: " true "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorAccountsEnabled(tc.in); got != tc.want {
				t.Fatalf("VendorAccountsEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestVendorAccountRoutingModeHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"nil -> default", nil, VendorRoutingModeVendorFirst},
		{"absent -> default", map[string]string{}, VendorRoutingModeVendorFirst},
		{"blank -> default", map[string]string{vendorAccountRoutingModeKey: ""}, VendorRoutingModeVendorFirst},
		{"unknown -> default", map[string]string{vendorAccountRoutingModeKey: "bogus"}, VendorRoutingModeVendorFirst},
		{"explicit vendor_first", map[string]string{vendorAccountRoutingModeKey: "vendor_first"}, VendorRoutingModeVendorFirst},
		{"explicit fallback_only", map[string]string{vendorAccountRoutingModeKey: "fallback_only"}, VendorRoutingModeFallbackOnly},
		{"padded fallback_only", map[string]string{vendorAccountRoutingModeKey: " fallback_only "}, VendorRoutingModeFallbackOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorAccountRoutingMode(tc.in); got != tc.want {
				t.Fatalf("VendorAccountRoutingMode(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The master flag is opt-in and the precedence defaults to vendor_first, both
// on the accessors the gateway reads and on the settings view the portal shows.
func TestVendorSettingsDefaults(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	if svc.VendorAccountsEnabled(ctx) {
		t.Fatal("VendorAccountsEnabled default = true, want false (opt-in)")
	}
	if got := svc.VendorAccountRoutingMode(ctx); got != VendorRoutingModeVendorFirst {
		t.Fatalf("VendorAccountRoutingMode default = %q, want %q", got, VendorRoutingModeVendorFirst)
	}
	view := svc.SystemSettingsView(ctx)
	if view.VendorAccountsEnabled || view.VendorAccountRoutingMode != VendorRoutingModeVendorFirst {
		t.Fatalf("view defaults = %v/%q, want false/%q", view.VendorAccountsEnabled, view.VendorAccountRoutingMode, VendorRoutingModeVendorFirst)
	}
}

// A service with no settings store (or one that cannot be read) behaves like
// the default: the area stays OFF and the precedence is vendor_first.
func TestVendorSettingsNilStoreAndReadFailure(t *testing.T) {
	ctx := context.Background()
	nilStore := NewService(ServiceDeps{Clock: fixedClock()})
	if nilStore.VendorAccountsEnabled(ctx) {
		t.Fatal("nil store: VendorAccountsEnabled = true, want false")
	}
	if got := nilStore.VendorAccountRoutingMode(ctx); got != VendorRoutingModeVendorFirst {
		t.Fatalf("nil store: VendorAccountRoutingMode = %q, want %q", got, VendorRoutingModeVendorFirst)
	}
	if view := nilStore.SystemSettingsView(ctx); view.VendorAccountsEnabled || view.VendorAccountRoutingMode != VendorRoutingModeVendorFirst {
		t.Fatalf("nil store view = %v/%q, want false/%q", view.VendorAccountsEnabled, view.VendorAccountRoutingMode, VendorRoutingModeVendorFirst)
	}

	failing := NewService(ServiceDeps{SystemSettings: failingSettingsReadStore{NewMemorySystemSettings()}, Clock: fixedClock()})
	if failing.VendorAccountsEnabled(ctx) {
		t.Fatal("read failure: VendorAccountsEnabled = true, want false (fail closed)")
	}
	if got := failing.VendorAccountRoutingMode(ctx); got != VendorRoutingModeVendorFirst {
		t.Fatalf("read failure: VendorAccountRoutingMode = %q, want %q", got, VendorRoutingModeVendorFirst)
	}
}

// failingSettingsReadStore is a settings store whose reads always fail.
type failingSettingsReadStore struct{ *MemorySystemSettings }

func (failingSettingsReadStore) SystemSettings(context.Context) (map[string]string, error) {
	return nil, errors.New("settings read failed")
}

func TestUpdateSystemSettingsPersistsVendorSettings(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	enabled := true
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		VendorAccountsEnabled:    &enabled,
		VendorAccountRoutingMode: strPtr("  fallback_only  "),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if !got.VendorAccountsEnabled || got.VendorAccountRoutingMode != VendorRoutingModeFallbackOnly {
		t.Fatalf("returned DTO = %v/%q, want true/%q", got.VendorAccountsEnabled, got.VendorAccountRoutingMode, VendorRoutingModeFallbackOnly)
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[vendorAccountsEnabledKey] != "true" {
		t.Fatalf("stored vendor_accounts_enabled = %q, want %q", values[vendorAccountsEnabledKey], "true")
	}
	if values[vendorAccountRoutingModeKey] != VendorRoutingModeFallbackOnly {
		t.Fatalf("stored vendor_account_routing_mode = %q, want %q (trimmed)", values[vendorAccountRoutingModeKey], VendorRoutingModeFallbackOnly)
	}
	if !svc.VendorAccountsEnabled(ctx) || svc.VendorAccountRoutingMode(ctx) != VendorRoutingModeFallbackOnly {
		t.Fatalf("accessors = %v/%q, want true/%q", svc.VendorAccountsEnabled(ctx), svc.VendorAccountRoutingMode(ctx), VendorRoutingModeFallbackOnly)
	}
	if view := svc.SystemSettingsView(ctx); !view.VendorAccountsEnabled || view.VendorAccountRoutingMode != VendorRoutingModeFallbackOnly {
		t.Fatalf("view = %v/%q, want true/%q", view.VendorAccountsEnabled, view.VendorAccountRoutingMode, VendorRoutingModeFallbackOnly)
	}

	// Turning the flag back off persists an explicit "false"; a request that omits
	// both fields keeps the stored routing mode.
	disabled := false
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &disabled}); err != nil {
		t.Fatalf("UpdateSystemSettings(off): %v", err)
	}
	if svc.VendorAccountsEnabled(ctx) {
		t.Fatal("VendorAccountsEnabled = true after turning it off")
	}
	if got := svc.VendorAccountRoutingMode(ctx); got != VendorRoutingModeFallbackOnly {
		t.Fatalf("routing mode = %q after an omitted field, want the stored %q", got, VendorRoutingModeFallbackOnly)
	}
}

func TestUpdateSystemSettingsRejectsUnknownVendorAccountRoutingMode(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"bogus", "", "  ", "VENDOR_FIRST"} {
		settings := NewMemorySystemSettings()
		svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
		enabled := true
		_, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
			VendorAccountsEnabled:    &enabled,
			VendorAccountRoutingMode: strPtr(bad),
		})
		if !errors.Is(err, ErrVendorAccountRoutingModeInvalid) {
			t.Fatalf("UpdateSystemSettings(%q) error = %v, want ErrVendorAccountRoutingModeInvalid", bad, err)
		}
		// The rejection is atomic: the valid flag in the same request is not applied.
		values, err := settings.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		if len(values) != 0 {
			t.Fatalf("a rejected update (%q) persisted %v, want nothing", bad, values)
		}
	}
}

func TestUpdateSystemSettingsVendorSettingsRequireSystemScope(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	enabled := true
	_, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &enabled})
	if !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("plain admin err = %v, want ErrPrincipalForbidden", err)
	}
	if svc.VendorAccountsEnabled(context.Background()) {
		t.Fatal("a forbidden update enabled the flag")
	}
}
