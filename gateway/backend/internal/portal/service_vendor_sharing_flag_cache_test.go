// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"sync/atomic"
	"testing"
	"time"
)

// This file pins the HOT-PATH READ of the two sharing switches (#183, spec 2026-10-15
// section 13: "Read on the resolver hot path through the 5s invalidatable
// settingCache[bool], like the other vendor flags; fail-closed"). The switches live in
// system_settings, whose store read is an uncached full-table SELECT, and the routing
// enumerator plus every effective-level decision read them, so reading them per call
// would add that SELECT to every routed vendor request, sharing on or not.

// countingSettings is a settings store that counts SystemSettings reads and can be
// made to fail them, over a real in-memory store.
type countingSettings struct {
	*MemorySystemSettings
	reads   atomic.Int64
	failing atomic.Bool
}

func (c *countingSettings) SystemSettings(ctx context.Context) (map[string]string, error) {
	c.reads.Add(1)
	if c.failing.Load() {
		return nil, errors.New("settings store down")
	}
	return c.MemorySystemSettings.SystemSettings(ctx)
}

// withCountingSettings swaps e's settings store for a counting one over the same data
// and returns it, with the read counter at zero.
func (e *vendorGrantEnv) withCountingSettings() *countingSettings {
	e.t.Helper()
	mem, ok := e.svc.settings.(*MemorySystemSettings)
	if !ok {
		e.t.Fatalf("fixture settings store is %T, want *MemorySystemSettings", e.svc.settings)
	}
	counting := &countingSettings{MemorySystemSettings: mem}
	e.svc.settings = counting
	return counting
}

// TestRoutingEnumeratorReadsTheSharingFlagsOncePerWindow is the headline: the shared
// arm of VendorAccountsForRouting reads the master flag (the shortcut), the
// subscription flag, and the EFFECTIVE level of every candidate account, which is the
// master flag again. Twenty routed requests must cost ONE settings read, not a handful
// each.
func TestRoutingEnumeratorReadsTheSharingFlagsOncePerWindow(t *testing.T) {
	e := newVendorGrantEnv(t)
	counting := e.withCountingSettings()
	member := userPrincipal(grantDirect)

	var first []string
	for i := 0; i < 20; i++ {
		got := e.routable(e.svc, member)
		if i == 0 {
			first = got
		}
		if len(got) != len(first) {
			t.Fatalf("call %d routable = %v, want the stable answer %v", i, got, first)
		}
	}
	if len(first) < 2 {
		t.Fatalf("fixture: member routes to %v, want its own account plus shared ones, or the test proves nothing", first)
	}
	if got := counting.reads.Load(); got != 1 {
		t.Fatalf("settings reads for 20 routed requests = %d, want exactly 1 (both sharing flags from one cached read)", got)
	}
}

// TestEffectiveShareLevelReadsTheMasterFlagOncePerWindow is the same for the gate side:
// every write gate, the member/manager reads and the listings decide through
// effectiveVendorAccountShareLevel, so a page that walks many accounts must not read
// the settings table once per account.
func TestEffectiveShareLevelReadsTheMasterFlagOncePerWindow(t *testing.T) {
	e := newVendorGrantEnv(t)
	counting := e.withCountingSettings()
	acc := e.accountRow(grantShared)

	for i := 0; i < 20; i++ {
		if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelShared {
			t.Fatalf("call %d: effective level = %q, want the stored %q while sharing is on", i, got, routing.VendorShareLevelShared)
		}
	}
	if got := counting.reads.Load(); got != 1 {
		t.Fatalf("settings reads for 20 effective-level decisions = %d, want exactly 1", got)
	}
}

// TestSharingFlagFlipThroughTheSettingsWriteIsImmediate: the cache is not allowed to
// make an operator wait out its TTL. UpdateSystemSettings is the one write path of the
// two switches, and it must drop the cached read, in BOTH directions (turning sharing
// off is the one an operator is watching for), for the master flag and for the
// subscription one.
func TestSharingFlagFlipThroughTheSettingsWriteIsImmediate(t *testing.T) {
	e := newVendorGrantEnv(t)
	admin := token("usr_s", "system", "admin")
	member := userPrincipal(grantAdminMember)
	acc := e.accountRow(grantShared)

	put := func(req UpdateSystemSettingsRequest) {
		t.Helper()
		if _, err := e.svc.UpdateSystemSettings(e.ctx, admin, req); err != nil {
			t.Fatalf("UpdateSystemSettings: %v", err)
		}
	}
	on, off := true, false

	// Prime the cache with sharing ON, subscription sharing OFF (the fixture's state).
	e.requireRoutable("primed", e.svc, member, grantedShared...)

	put(UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &off})
	if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelNone {
		t.Fatalf("effective level right after switching the master OFF = %q, want none", got)
	}
	e.requireRoutable("master just switched off", e.svc, member)

	put(UpdateSystemSettingsRequest{VendorAccountSharingEnabled: &on})
	if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelShared {
		t.Fatalf("effective level right after switching the master ON = %q, want shared", got)
	}
	e.requireRoutable("master just switched back on", e.svc, member, grantedShared...)

	put(UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &on})
	e.requireRoutable("subscription sharing just switched on", e.svc, member, grantShared, grantFull, grantSub, grantDup)
	put(UpdateSystemSettingsRequest{VendorSubscriptionSharingEnabled: &off})
	e.requireRoutable("subscription sharing just switched off", e.svc, member, grantedShared...)
}

// TestSharingFlagsAreReReadWhenTheWindowLapses pins the TTL itself: within the window an
// OUT-OF-BAND change (a row edited behind the portal's back, so nothing invalidates) is
// not seen, and one window later it is, so the staleness bound is the TTL and no more.
// The clock is the cache's own seam, so the window is crossed without sleeping.
func TestSharingFlagsAreReReadWhenTheWindowLapses(t *testing.T) {
	e := newVendorGrantEnv(t)
	counting := e.withCountingSettings()
	acc := e.accountRow(grantShared)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	e.svc.vendorSharingFlags.Now = func() time.Time { return now }
	e.svc.invalidateVendorSharingFlags()

	level := func() string { return e.svc.effectiveVendorAccountShareLevel(e.ctx, acc) }
	if got := level(); got != routing.VendorShareLevelShared {
		t.Fatalf("first read = %q, want shared", got)
	}

	// Switch sharing off WITHOUT invalidating, as a direct database edit would.
	if err := counting.MemorySystemSettings.SetSystemSetting(e.ctx, vendorAccountSharingEnabledKey, "false", now); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	now = now.Add(vendorSharingFlagsTTL - time.Second)
	if got := level(); got != routing.VendorShareLevelShared {
		t.Fatalf("read inside the window = %q, want the cached shared (nothing invalidated it)", got)
	}
	if got := counting.reads.Load(); got != 1 {
		t.Fatalf("settings reads inside the window = %d, want 1", got)
	}

	now = now.Add(time.Second)
	if got := level(); got != routing.VendorShareLevelNone {
		t.Fatalf("read once the window lapsed = %q, want none (the out-of-band change)", got)
	}
	if got := counting.reads.Load(); got != 2 {
		t.Fatalf("settings reads after the window = %d, want 2 (one per window)", got)
	}
}

// TestSharingFlagsWithNoSettingsStoreReadAsOff: a service built without a settings store
// is fail-closed (both switches off) and never caches that, so a store wired in later is
// honoured on the next call.
func TestSharingFlagsWithNoSettingsStoreReadAsOff(t *testing.T) {
	e := newVendorGrantEnv(t)
	svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: e.routes, Clock: fixedClock()})
	acc := e.accountRow(grantShared)
	if svc.vendorAccountSharingEnabledCached(e.ctx) || svc.vendorSubscriptionSharingEnabledCached(e.ctx) {
		t.Fatal("no settings store: a sharing switch reads as on, want both off")
	}
	if got := svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelNone {
		t.Fatalf("no settings store: effective level = %q, want none", got)
	}

	svc.settings = e.svc.settings
	if got := svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelShared {
		t.Fatalf("settings store wired in: effective level = %q, want shared", got)
	}
}

// TestCachedSharingFlagsAgreeWithTheRawAccessors: the cache is a memo of the raw
// accessors, not a second interpretation of the setting. Every stored shape of both keys
// (absent, true, false, blank, malformed, padded) reads the same both ways.
func TestCachedSharingFlagsAgreeWithTheRawAccessors(t *testing.T) {
	cases := []struct {
		name  string
		store map[string]string
	}{
		{"absent", map[string]string{}},
		{"true", sharingFlagValues("true")},
		{"false", sharingFlagValues("false")},
		{"blank", sharingFlagValues("")},
		{"malformed", sharingFlagValues("maybe")},
		{"padded", sharingFlagValues(" true ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemorySystemSettings()
			if err := mem.SetSystemSettings(context.Background(), tc.store, time.Time{}); err != nil {
				t.Fatalf("SetSystemSettings: %v", err)
			}
			svc := &Service{settings: mem}
			ctx := context.Background()
			if got, want := svc.vendorAccountSharingEnabledCached(ctx), svc.VendorAccountSharingEnabled(ctx); got != want {
				t.Errorf("master: cached = %v, raw = %v", got, want)
			}
			if got, want := svc.vendorSubscriptionSharingEnabledCached(ctx), svc.VendorSubscriptionSharingEnabled(ctx); got != want {
				t.Errorf("subscription: cached = %v, raw = %v", got, want)
			}
		})
	}
}

// sharingFlagValues is the stored form of both sharing keys set to raw.
func sharingFlagValues(raw string) map[string]string {
	return map[string]string{vendorAccountSharingEnabledKey: raw, vendorSubscriptionSharingEnabledKey: raw}
}

// TestSharingFlagsFailClosedAndAnErrorIsNotPinned: an unreadable settings store reads as
// OFF (effective level none, the shared arm empty, the owner's own accounts kept), and
// the failure is NOT cached: the very next call after the store recovers answers
// truthfully instead of denying for the rest of a TTL window.
func TestSharingFlagsFailClosedAndAnErrorIsNotPinned(t *testing.T) {
	e := newVendorGrantEnv(t)
	counting := e.withCountingSettings()
	member := userPrincipal(grantDirect)
	acc := e.accountRow(grantShared)

	counting.failing.Store(true)
	for i := 0; i < 3; i++ {
		if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelNone {
			t.Fatalf("unreadable store: effective level = %q, want none (fail closed)", got)
		}
	}
	e.requireRoutable("unreadable store", e.svc, member, grantOwnAccount)
	if got := counting.reads.Load(); got < 4 {
		t.Fatalf("settings reads during the outage = %d, want one per call (a failed read must not be cached)", got)
	}

	counting.failing.Store(false)
	if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelShared {
		t.Fatalf("effective level on the first call after recovery = %q, want shared (the outage must not be pinned)", got)
	}
	e.requireRoutable("store recovered", e.svc, member, append([]string{grantOwnAccount}, grantedShared...)...)
}
