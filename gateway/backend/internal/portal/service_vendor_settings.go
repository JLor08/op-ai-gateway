// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/vendorauth"
	"strconv"
	"strings"
)

// Vendor accounts ("Anbieter") system settings. Three keys, all ordinary
// system-settings rows read through the settings store at request time:
//
//   - vendor_accounts_enabled is the MASTER flag, a module-enable switch like
//     netbird_enabled / cert_enabled. It is OFF by default (opt-in): while off,
//     the portal hides the "Anbieter" menu item and every vendor-account
//     service method refuses with ErrVendorAccountsDisabled.
//   - vendor_account_routing_mode is the routing precedence between a caller's
//     own vendor accounts and the self-hosted/shared routes. The resolver
//     consults it; this file only persists, validates and exposes it.
//   - vendor_openai_codex_client_version is the Codex client_version the OpenAI
//     subscription model-discovery call sends (see below).
const (
	vendorAccountsEnabledKey             = "vendor_accounts_enabled"
	vendorAccountRoutingModeKey          = "vendor_account_routing_mode"
	vendorOpenAICodexClientVersionKey    = "vendor_openai_codex_client_version"
	maxVendorOpenAICodexClientVersionLen = 64
)

// DefaultVendorOpenAICodexClientVersion is the vendor_openai_codex_client_version
// used when the setting is unset or blank: the Codex app version the live
// codex/models catalog request was confirmed with. It IS the vendorauth fallback
// constant (not a copy), so the two cannot drift apart.
//
// VERIFY-LIVE / RAISE WHEN OPENAI SHIPS A NEWER CODEX APP. The ChatGPT backend's
// /backend-api/codex/models?client_version=<V> HIDES every model whose
// minimal_client_version exceeds the V sent, so a stale value silently hides new
// models. The operator raises the setting (System settings, no redeploy) when
// OpenAI releases a newer Codex app and a new model is missing from discovery.
const DefaultVendorOpenAICodexClientVersion = vendorauth.CodexModelsClientVersionDefault

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

// ErrVendorOpenAICodexClientVersionInvalid rejects a clearly malformed
// vendor_openai_codex_client_version on write: after trimming it must be empty
// (reset to the default) or a version-shaped token (see
// isValidVendorOpenAICodexClientVersion). The read side is lenient and falls back
// to the default.
var ErrVendorOpenAICodexClientVersionInvalid = errors.New("system.vendor_openai_codex_client_version_invalid")

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

// isValidVendorOpenAICodexClientVersion reports whether v (already trimmed) is a
// plausible Codex client_version: 1 to maxVendorOpenAICodexClientVersionLen
// characters from [0-9A-Za-z._+-], starting with a digit ("26.930.61225",
// "0.46.0-alpha.1"). It deliberately checks only the SHAPE, not that the version
// exists: the value is sent as a URL-encoded query parameter, so this rejects the
// obviously wrong (a leading "v", spaces, separators, control or non-ASCII
// characters, an unbounded string) rather than guarding against injection.
func isValidVendorOpenAICodexClientVersion(v string) bool {
	if v == "" || len(v) > maxVendorOpenAICodexClientVersionLen || v[0] < '0' || v[0] > '9' {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c == '.', c == '_', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// VendorOpenAICodexClientVersion interprets the persisted
// vendor_openai_codex_client_version system setting, defaulting to
// DefaultVendorOpenAICodexClientVersion when absent, blank, or not a plausible
// version (lenient read; a malformed value can only get in out of band, the
// write path rejects it).
func VendorOpenAICodexClientVersion(values map[string]string) string {
	raw := strings.TrimSpace(values[vendorOpenAICodexClientVersionKey])
	if isValidVendorOpenAICodexClientVersion(raw) {
		return raw
	}
	return DefaultVendorOpenAICodexClientVersion
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

// VendorOpenAICodexClientVersion is the effective Codex client_version the OpenAI
// subscription model discovery sends. Read from the settings store on every call
// like the two getters above (discovery is a rare, user-initiated call, not a hot
// path, so nothing needs invalidating: a settings PUT is visible on the very next
// call). Nil-safe: a missing store or a read error yields the built-in default,
// never a blank version.
func (s *Service) VendorOpenAICodexClientVersion(ctx context.Context) string {
	if s.settings == nil {
		return DefaultVendorOpenAICodexClientVersion
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultVendorOpenAICodexClientVersion
	}
	return VendorOpenAICodexClientVersion(values)
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
