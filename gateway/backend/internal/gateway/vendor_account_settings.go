// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/portal"
	"time"
)

// vendorSettingsCacheTTL bounds how long the resolver's vendor-account accessors
// reuse a cached read of vendor_accounts_enabled / vendor_account_routing_mode.
// Both live in system_settings, whose store read is an uncached full-table
// SELECT, and the per-user vendor branch runs on EVERY inference resolve, so
// reading them per request would add a database round-trip to the hot path.
// handleSystemSettings invalidates the caches explicitly after a successful PUT
// that carried either key, so an operator toggling them in the portal sees the
// effect immediately; the TTL only bounds how stale an OUT-OF-BAND change (a
// direct database edit) can be. Matches edgeSchemeSwitchTTL for the same reason.
const vendorSettingsCacheTTL = 5 * time.Second

// vendorAccountsEnabledCached reports the vendor_accounts_enabled master flag
// through the short-TTL, invalidatable cache (vendorEnabledCache). Nil-safe: a
// Server with no portal reports false (the area is opt-in, so an unreadable flag
// fails closed), mirroring portal.Service.VendorAccountsEnabled's own posture.
func (s *Server) vendorAccountsEnabledCached(ctx context.Context) bool {
	if s.Portal == nil {
		return false
	}
	return s.vendorEnabledCache.Get(ctx, vendorSettingsCacheTTL, s.Portal.VendorAccountsEnabled)
}

// vendorAccountRoutingModeCached reports the effective vendor_account_routing_mode
// through the short-TTL, invalidatable cache (vendorRoutingModeCache). Nil-safe:
// a Server with no portal reports the default (vendor_first).
func (s *Server) vendorAccountRoutingModeCached(ctx context.Context) string {
	if s.Portal == nil {
		return portal.DefaultVendorAccountRoutingMode
	}
	return s.vendorRoutingModeCache.Get(ctx, vendorSettingsCacheTTL, s.Portal.VendorAccountRoutingMode)
}

// invalidateVendorSettingsCache drops both cached vendor-settings reads so the
// next resolve re-reads them. Called by handleSystemSettings after a successful
// PUT that carried the master flag or the routing mode, so an operator toggling
// either in the portal does not have to wait out vendorSettingsCacheTTL. The
// generation bump also cancels any store read already in flight (see
// settingCache.Get), so a pre-change read cannot re-arm the cache behind this.
func (s *Server) invalidateVendorSettingsCache() {
	s.vendorEnabledCache.Invalidate()
	s.vendorRoutingModeCache.Invalidate()
}
