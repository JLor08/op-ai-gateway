// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/vendorauth"
	"strings"
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

// The built-in default is the version the live codex/models catalog request was
// confirmed with, and it is the vendorauth fallback constant itself (not a
// copy), so the two cannot drift apart.
func TestVendorOpenAICodexClientVersionDefault(t *testing.T) {
	if DefaultVendorOpenAICodexClientVersion != "26.930.61225" {
		t.Fatalf("DefaultVendorOpenAICodexClientVersion = %q, want %q", DefaultVendorOpenAICodexClientVersion, "26.930.61225")
	}
	if DefaultVendorOpenAICodexClientVersion != vendorauth.CodexModelsClientVersionDefault {
		t.Fatalf("default %q != vendorauth.CodexModelsClientVersionDefault %q", DefaultVendorOpenAICodexClientVersion, vendorauth.CodexModelsClientVersionDefault)
	}
	if vendorOpenAICodexClientVersionKey != "vendor_openai_codex_client_version" {
		t.Fatalf("settings key = %q", vendorOpenAICodexClientVersionKey)
	}
}

func TestVendorOpenAICodexClientVersionHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"nil -> default", nil, DefaultVendorOpenAICodexClientVersion},
		{"absent -> default", map[string]string{}, DefaultVendorOpenAICodexClientVersion},
		{"blank -> default", map[string]string{vendorOpenAICodexClientVersionKey: ""}, DefaultVendorOpenAICodexClientVersion},
		{"whitespace -> default", map[string]string{vendorOpenAICodexClientVersionKey: "   "}, DefaultVendorOpenAICodexClientVersion},
		{"garbage stored out of band -> default", map[string]string{vendorOpenAICodexClientVersionKey: "26.930 61225?x=1"}, DefaultVendorOpenAICodexClientVersion},
		{"explicit", map[string]string{vendorOpenAICodexClientVersionKey: "27.101.40000"}, "27.101.40000"},
		{"padded explicit", map[string]string{vendorOpenAICodexClientVersionKey: " 27.101.40000 "}, "27.101.40000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorOpenAICodexClientVersion(tc.in); got != tc.want {
				t.Fatalf("VendorOpenAICodexClientVersion(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsValidVendorOpenAICodexClientVersion(t *testing.T) {
	for _, ok := range []string{"26.930.61225", "0.46.0", "0.46.0-alpha.1", "1.2.3+build_7", "7"} {
		if !isValidVendorOpenAICodexClientVersion(ok) {
			t.Errorf("isValid(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"", " ", "v26.930.61225", "26.930 61225", "26.930.61225\n", "26.930.61225&x=1", "26.930/61225",
		"26.930.61225?", "26,930", "26.930.61225\u00e9", strings.Repeat("1", 65),
	} {
		if isValidVendorOpenAICodexClientVersion(bad) {
			t.Errorf("isValid(%q) = true, want false", bad)
		}
	}
	if !isValidVendorOpenAICodexClientVersion(strings.Repeat("1", 64)) {
		t.Error("a 64-character version must be accepted (the cap is inclusive)")
	}
}

// Out of the box the setting reads as the built-in default, on the accessor the
// discovery will use and on the settings view the portal shows; a service with
// no store, or one that cannot be read, behaves the same (never blank).
func TestVendorOpenAICodexClientVersionDefaultsNilStoreAndReadFailure(t *testing.T) {
	ctx := context.Background()
	const want = "26.930.61225"

	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	if got := svc.VendorOpenAICodexClientVersion(ctx); got != want {
		t.Fatalf("default accessor = %q, want %q", got, want)
	}
	if got := svc.SystemSettingsView(ctx).VendorOpenAICodexClientVersion; got != want {
		t.Fatalf("default view = %q, want %q", got, want)
	}

	nilStore := NewService(ServiceDeps{Clock: fixedClock()})
	if got := nilStore.VendorOpenAICodexClientVersion(ctx); got != want {
		t.Fatalf("nil store accessor = %q, want %q", got, want)
	}
	if got := nilStore.SystemSettingsView(ctx).VendorOpenAICodexClientVersion; got != want {
		t.Fatalf("nil store view = %q, want %q", got, want)
	}

	failing := NewService(ServiceDeps{SystemSettings: failingSettingsReadStore{NewMemorySystemSettings()}, Clock: fixedClock()})
	if got := failing.VendorOpenAICodexClientVersion(ctx); got != want {
		t.Fatalf("read failure accessor = %q, want %q", got, want)
	}
	if got := failing.SystemSettingsView(ctx).VendorOpenAICodexClientVersion; got != want {
		t.Fatalf("read failure view = %q, want %q", got, want)
	}
}

// A PUT takes effect on the very next read (the portal service reads the store
// per call, so there is nothing to invalidate): the accessor and the view both
// return the new value, trimmed, and an omitted field keeps the stored one.
func TestUpdateSystemSettingsPersistsVendorOpenAICodexClientVersion(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		VendorOpenAICodexClientVersion: strPtr("  27.101.40000  "),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if got.VendorOpenAICodexClientVersion != "27.101.40000" {
		t.Fatalf("returned DTO = %q, want the trimmed %q", got.VendorOpenAICodexClientVersion, "27.101.40000")
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[vendorOpenAICodexClientVersionKey] != "27.101.40000" {
		t.Fatalf("stored = %q, want %q (trimmed)", values[vendorOpenAICodexClientVersionKey], "27.101.40000")
	}
	if v := svc.VendorOpenAICodexClientVersion(ctx); v != "27.101.40000" {
		t.Fatalf("accessor after PUT = %q, want %q", v, "27.101.40000")
	}
	if v := svc.SystemSettingsView(ctx).VendorOpenAICodexClientVersion; v != "27.101.40000" {
		t.Fatalf("view after PUT = %q, want %q", v, "27.101.40000")
	}

	// A second PUT replaces it.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorOpenAICodexClientVersion: strPtr("28.2.1")}); err != nil {
		t.Fatalf("UpdateSystemSettings(second): %v", err)
	}
	if v := svc.VendorOpenAICodexClientVersion(ctx); v != "28.2.1" {
		t.Fatalf("accessor after second PUT = %q, want %q", v, "28.2.1")
	}

	// A request that omits the field keeps the stored value.
	enabled := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &enabled}); err != nil {
		t.Fatalf("UpdateSystemSettings(other field): %v", err)
	}
	if v := svc.VendorOpenAICodexClientVersion(ctx); v != "28.2.1" {
		t.Fatalf("accessor after an omitted field = %q, want the stored %q", v, "28.2.1")
	}
}

// Blank is accepted on write and means "use the built-in default": the stored
// override is cleared and the setting reads as the default again (the same
// outcome as the lenient read, so an operator can reset by emptying the field).
func TestUpdateSystemSettingsBlankVendorOpenAICodexClientVersionResetsToDefault(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorOpenAICodexClientVersion: strPtr("27.101.40000")}); err != nil {
		t.Fatalf("UpdateSystemSettings(set): %v", err)
	}
	for _, blank := range []string{"", "   "} {
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorOpenAICodexClientVersion: strPtr("27.101.40000")}); err != nil {
			t.Fatalf("UpdateSystemSettings(re-set): %v", err)
		}
		got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorOpenAICodexClientVersion: strPtr(blank)})
		if err != nil {
			t.Fatalf("UpdateSystemSettings(%q): %v, want blank accepted", blank, err)
		}
		if got.VendorOpenAICodexClientVersion != DefaultVendorOpenAICodexClientVersion {
			t.Fatalf("returned DTO after blank %q = %q, want the default %q", blank, got.VendorOpenAICodexClientVersion, DefaultVendorOpenAICodexClientVersion)
		}
		if v := svc.VendorOpenAICodexClientVersion(ctx); v != DefaultVendorOpenAICodexClientVersion {
			t.Fatalf("accessor after blank %q = %q, want the default %q", blank, v, DefaultVendorOpenAICodexClientVersion)
		}
		values, err := settings.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		if values[vendorOpenAICodexClientVersionKey] != "" {
			t.Fatalf("stored after blank %q = %q, want the override cleared", blank, values[vendorOpenAICodexClientVersionKey])
		}
	}
}

// Clearly-bad input is rejected, and atomically: a valid field in the same
// request is not applied either.
func TestUpdateSystemSettingsRejectsBadVendorOpenAICodexClientVersion(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"v26.930.61225", "26.930 61225", "26.930.61225&x=1", "a/b", "26.930\n.61225", strings.Repeat("1", 65)} {
		settings := NewMemorySystemSettings()
		svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
		enabled := true
		_, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
			VendorAccountsEnabled:          &enabled,
			VendorOpenAICodexClientVersion: strPtr(bad),
		})
		if !errors.Is(err, ErrVendorOpenAICodexClientVersionInvalid) {
			t.Fatalf("UpdateSystemSettings(%q) error = %v, want ErrVendorOpenAICodexClientVersionInvalid", bad, err)
		}
		values, err := settings.SystemSettings(ctx)
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		if len(values) != 0 {
			t.Fatalf("a rejected update (%q) persisted %v, want nothing", bad, values)
		}
	}
}

func TestUpdateSystemSettingsVendorOpenAICodexClientVersionRequiresSystemScope(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	_, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{VendorOpenAICodexClientVersion: strPtr("27.101.40000")})
	if !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("plain admin err = %v, want ErrPrincipalForbidden", err)
	}
	if got := svc.VendorOpenAICodexClientVersion(context.Background()); got != DefaultVendorOpenAICodexClientVersion {
		t.Fatalf("a forbidden update changed the setting to %q", got)
	}
}
