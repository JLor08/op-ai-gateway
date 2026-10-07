// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Vendor accounts ("Anbieter") system settings. Two keys, both ordinary
// system-settings rows read through the settings store at request time:
//
//   - vendor_accounts_enabled is the MASTER flag, a module-enable switch like
//     netbird_enabled / cert_enabled. It is OFF by default (opt-in): while off,
//     the portal hides the "Anbieter" menu item and every vendor-account
//     service method refuses with ErrVendorAccountsDisabled.
//   - vendor_account_routing_mode is the routing precedence between a caller's
//     own vendor accounts and the self-hosted/shared routes. The resolver
//     consults it; this file only persists, validates and exposes it.
const (
	vendorAccountsEnabledKey    = "vendor_accounts_enabled"
	vendorAccountRoutingModeKey = "vendor_account_routing_mode"
)

// The valid vendor_account_routing_mode values. VendorRoutingModeVendorFirst:
// a caller's own account wins whenever it serves the requested model.
// VendorRoutingModeFallbackOnly: an own account is used only when no
// self-hosted/shared route serves the model.
const (
	VendorRoutingModeVendorFirst  = "vendor_first"
	VendorRoutingModeFallbackOnly = "fallback_only"
)

// DefaultVendorAccountRoutingMode is the routing mode used when the setting is
// unset, blank, or not one of the known modes.
const DefaultVendorAccountRoutingMode = VendorRoutingModeVendorFirst

// ErrVendorAccountRoutingModeInvalid rejects an unknown vendor_account_routing_mode
// on write (the read side is lenient and falls back to the default).
var ErrVendorAccountRoutingModeInvalid = errors.New("system.vendor_account_routing_mode_invalid")

// knownVendorAccountRoutingModes is the registry of valid
// vendor_account_routing_mode values.
func knownVendorAccountRoutingModes() []string {
	return []string{VendorRoutingModeVendorFirst, VendorRoutingModeFallbackOnly}
}

func isKnownVendorAccountRoutingMode(mode string) bool {
	for _, m := range knownVendorAccountRoutingModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// VendorAccountsEnabled interprets the vendor_accounts_enabled master switch.
// Defaults to false (opt-in) when absent, blank, or unparseable.
func VendorAccountsEnabled(values map[string]string) bool {
	raw, ok := values[vendorAccountsEnabledKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// VendorAccountRoutingMode interprets the persisted vendor_account_routing_mode
// system setting, defaulting to "vendor_first" when absent, blank, or not one of
// the known modes (lenient read).
func VendorAccountRoutingMode(values map[string]string) string {
	raw := strings.TrimSpace(values[vendorAccountRoutingModeKey])
	if isKnownVendorAccountRoutingMode(raw) {
		return raw
	}
	return DefaultVendorAccountRoutingMode
}

// VendorAccountsEnabled reports whether the vendor-accounts master flag is on.
// It is read from the settings store on every call (the flag is rare to flip
// and cheap to read), so a toggle takes effect on the very next request. A
// service with no settings store, or a store that cannot be read, reports
// false: the area is opt-in, so an unreadable flag fails closed.
func (s *Service) VendorAccountsEnabled(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return VendorAccountsEnabled(values)
}

// VendorAccountRoutingMode is the effective vendor_account_routing_mode, the
// getter the resolver consults. Nil-safe: a missing store or a read error
// yields the default (vendor_first).
func (s *Service) VendorAccountRoutingMode(ctx context.Context) string {
	if s.settings == nil {
		return DefaultVendorAccountRoutingMode
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultVendorAccountRoutingMode
	}
	return VendorAccountRoutingMode(values)
}

// requireVendorAccountsEnabled is the module guard every vendor-account service
// method runs before it does anything else (mirrors the netbird module guard):
// ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) requireVendorAccountsEnabled(ctx context.Context) error {
	if !s.VendorAccountsEnabled(ctx) {
		return ErrVendorAccountsDisabled
	}
	return nil
}
