// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"time"
)

// The sharing switches on the routing hot path (#183, spec 2026-10-15 section 13).
//
// vendor_account_sharing_enabled and vendor_subscription_sharing_enabled live in
// system_settings, whose store read is an uncached full-table SELECT. Every routed
// vendor request runs the routing enumerator (VendorAccountsForRouting), which asks
// for both switches and, per candidate account, for the EFFECTIVE share level (the
// master switch again), and the gates, listings and member/manager reads decide the
// same way. Read raw, that is a SELECT per decision on the hottest path, and it is paid
// when sharing is OFF too (the default). So those decisions read the switches through
// the 5 s, invalidatable cache below, "like the other vendor flags" (the gateway's
// resolver reads vendor_accounts_enabled and the routing mode the same way, through
// settingCache in vendor_account_settings.go).
//
// Which reads are cached, and which are not, is the one thing to keep straight:
//
//   - CACHED (vendorAccountSharingEnabledCached / vendorSubscriptionSharingEnabledCached):
//     the DECISION reads, the ones that run per request or per account: the routing
//     enumerator, effectiveVendorAccountShareLevel (and through it the write gates, the
//     read gates and the listings), and the member read's subscription check.
//   - RAW (Service.VendorAccountSharingEnabled / VendorSubscriptionSharingEnabled):
//     the public accessors, deliberately left uncached. The vendor-accounts probe reads
//     them fresh, and the require* guards of the write endpoints
//     (requireVendorAccountSharingEnabled and its subscription twin) read them raw as
//     well: they run once per request on a rare write path, and an admin acting right
//     after a flip should be told the truth.
//
// The cache is invalidated after every settings write (persistSystemSettings), so a flip
// through the portal takes effect on the very next decision; the TTL only bounds how
// stale a change made OUT OF BAND (a direct database edit, or another gateway instance
// sharing the database) can be, the same bound the gateway's own flag caches have.
//
// Fail-closed, exactly as the raw accessors: no settings store, or one that cannot be
// read, reads as BOTH switches off (effective level none, the shared arm empty). A
// failed read is not cached: it is answered "off" for that call only, and the next call
// reads again, so a transient outage never pins a denial for a TTL window and a
// sustained one costs what it cost before the cache (one read per call).
const vendorSharingFlagsTTL = 5 * time.Second

// vendorSharingFlagSet is both sharing switches as one cached value, so the pair costs
// ONE settings read per window instead of one each.
type vendorSharingFlagSet struct {
	sharing             bool // vendor_account_sharing_enabled, the master
	subscriptionSharing bool // vendor_subscription_sharing_enabled; narrows the master, does not consult it
}

// vendorAccountSharingEnabledCached is the master sharing switch as the hot path reads
// it: through the cache, fail-closed. It is the value Service.VendorAccountSharingEnabled
// would answer, up to vendorSharingFlagsTTL of staleness for an out-of-band change.
func (s *Service) vendorAccountSharingEnabledCached(ctx context.Context) bool {
	return s.vendorSharingFlagsCached(ctx).sharing
}

// vendorSubscriptionSharingEnabledCached is the subscription sharing switch as the hot
// path reads it (see vendorAccountSharingEnabledCached). Like the raw accessor it only
// narrows the master switch and does not consult it.
func (s *Service) vendorSubscriptionSharingEnabledCached(ctx context.Context) bool {
	return s.vendorSharingFlagsCached(ctx).subscriptionSharing
}

func (s *Service) vendorSharingFlagsCached(ctx context.Context) vendorSharingFlagSet {
	return s.vendorSharingFlags.GetTTL(ctx, s.loadVendorSharingFlags)
}

// loadVendorSharingFlags is the cache's loader: one settings read for both switches,
// interpreted by the same lenient parsers as the raw accessors (so the two can never
// disagree about a blank or malformed value). A missing store or a read error is the
// zero set (both off) with a zero TTL, which the cache does not retain.
func (s *Service) loadVendorSharingFlags(ctx context.Context) (vendorSharingFlagSet, time.Duration) {
	if s.settings == nil {
		return vendorSharingFlagSet{}, 0
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return vendorSharingFlagSet{}, 0
	}
	return vendorSharingFlagSet{
		sharing:             VendorAccountSharingEnabled(values),
		subscriptionSharing: VendorSubscriptionSharingEnabled(values),
	}, vendorSharingFlagsTTL
}

// invalidateVendorSharingFlags drops the cached read so the next decision re-reads the
// store. persistSystemSettings calls it after every settings write; the cache's
// generation counter also cancels a read already in flight, so one that started before
// the write cannot re-arm the cache with its pre-write value (see ttlcache.Cache.Get).
func (s *Service) invalidateVendorSharingFlags() {
	s.vendorSharingFlags.Invalidate()
}
