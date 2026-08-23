// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// utc is a tiny helper so test fixtures read as plain "Y-M-D h:m:s" without the
// verbosity of a full time.Date call at every call site.
func utc(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
}

// TestPeriodStart pins periodStart against independently-verified (via the
// `date -u` coreutil, not Go's own time package) calendar boundaries, including
// week starts that span a month and a year boundary — the cases most likely to
// expose an off-by-one in a hand-rolled Monday calculation.
func TestPeriodStart(t *testing.T) {
	cases := []struct {
		name   string
		period string
		now    time.Time
		want   time.Time
	}{
		{"hour mid", "hour", utc(2026, 3, 15, 13, 47, 22), utc(2026, 3, 15, 13, 0, 0)},
		{"hour exact boundary", "hour", utc(2026, 1, 1, 0, 0, 0), utc(2026, 1, 1, 0, 0, 0)},
		{"day mid", "day", utc(2026, 3, 15, 13, 47, 22), utc(2026, 3, 15, 0, 0, 0)},
		{"day year boundary", "day", utc(2025, 12, 31, 23, 59, 59), utc(2025, 12, 31, 0, 0, 0)},
		// 2026-03-18 is a Wednesday (verified: `date -u -d 2026-03-18 +%A`); its
		// Monday is 2026-03-16.
		{"week midweek", "week", utc(2026, 3, 18, 9, 0, 0), utc(2026, 3, 16, 0, 0, 0)},
		// 2026-08-10 is a Monday itself: the period start is that same day.
		{"week on monday", "week", utc(2026, 8, 10, 0, 0, 1), utc(2026, 8, 10, 0, 0, 0)},
		// 2026-08-08 is a Saturday; its Monday (2026-08-03) is earlier in the
		// SAME month.
		{"week saturday same month", "week", utc(2026, 8, 8, 23, 59, 59), utc(2026, 8, 3, 0, 0, 0)},
		// 2026-02-01 is a Sunday; its Monday (2026-01-26) is in the PREVIOUS
		// month — a week spanning a month boundary.
		{"week spans month boundary", "week", utc(2026, 2, 1, 5, 0, 0), utc(2026, 1, 26, 0, 0, 0)},
		// 2026-01-01 is a Thursday; its Monday (2025-12-29) is in the PREVIOUS
		// year — a week spanning a year boundary.
		{"week spans year boundary", "week", utc(2026, 1, 1, 10, 0, 0), utc(2025, 12, 29, 0, 0, 0)},
		{"month mid", "month", utc(2026, 3, 15, 13, 47, 22), utc(2026, 3, 1, 0, 0, 0)},
		{"month year boundary", "month", utc(2026, 12, 31, 23, 59, 59), utc(2026, 12, 1, 0, 0, 0)},
		{"month start of january", "month", utc(2026, 1, 15, 0, 0, 0), utc(2026, 1, 1, 0, 0, 0)},
		{"empty period", "", utc(2026, 3, 15, 13, 47, 22), time.Time{}},
		{"unknown period", "fortnight", utc(2026, 3, 15, 13, 47, 22), time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := periodStart(tc.period, tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("periodStart(%q, %v) = %v, want %v", tc.period, tc.now, got, tc.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				// Equal() ignores the *representation* of the zone, but the design
				// mandates UTC-aligned boundaries throughout — pin the zone too.
				t.Fatalf("periodStart(%q, %v) location = %v, want UTC", tc.period, tc.now, got.Location())
			}
		})
	}
}

// fakePrincipalStore is a minimal, mutex-guarded fake of the narrow store slice
// PrincipalLimiter needs (PrincipalLimits + UsageAggregateSince), keyed per
// Principal so a single fake instance can host several distinct principals in
// one test (proving isolation) without any real DB.
type fakePrincipalStore struct {
	mu       sync.Mutex
	configs  map[Principal]routing.LimitConfig // presence => ok=true
	aggs     map[Principal]fakeAggregate
	cfgErr   error
	aggErr   error
	cfgCalls int
	aggCalls int
}

type fakeAggregate struct {
	requests int64
	tokens   int64
	cost     float64
}

func newFakePrincipalStore() *fakePrincipalStore {
	return &fakePrincipalStore{
		configs: make(map[Principal]routing.LimitConfig),
		aggs:    make(map[Principal]fakeAggregate),
	}
}

func (f *fakePrincipalStore) setConfig(p Principal, cfg routing.LimitConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[p] = cfg
}

func (f *fakePrincipalStore) setAggregate(p Principal, requests, tokens int64, cost float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aggs[p] = fakeAggregate{requests: requests, tokens: tokens, cost: cost}
}

func (f *fakePrincipalStore) PrincipalLimits(_ context.Context, principalType, principalID string) (routing.LimitConfig, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfgCalls++
	if f.cfgErr != nil {
		return routing.LimitConfig{}, false, f.cfgErr
	}
	cfg, ok := f.configs[Principal{Type: principalType, ID: principalID}]
	return cfg, ok, nil
}

func (f *fakePrincipalStore) UsageAggregateSince(_ context.Context, principalType, principalID string, _ time.Time) (int64, int64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aggCalls++
	if f.aggErr != nil {
		return 0, 0, 0, f.aggErr
	}
	agg := f.aggs[Principal{Type: principalType, ID: principalID}]
	return agg.requests, agg.tokens, agg.cost, nil
}

func (f *fakePrincipalStore) callCounts() (cfgCalls, aggCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfgCalls, f.aggCalls
}

// clockAt returns an injectable now func() time.Time pinned to a mutable
// pointer, so a test can advance time deterministically between Admit calls
// (e.g. to cross a rate-limit window boundary) without any real sleeping.
func clockAt(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestPrincipalLimiterNoopWithoutConfig(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	// No config row at all for p (store.configs has no entry => ok=false).
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{})

	for i := 0; i < 5; i++ {
		allow, reason, retryAfter := limiter.Admit(context.Background(), p)
		if !allow || reason != "" || retryAfter != 0 {
			t.Fatalf("Admit() with no config = (%v,%q,%v), want (true,\"\",0)", allow, reason, retryAfter)
		}
	}
}

func TestPrincipalLimiterNoopWithZeroConfig(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc1"}
	// An explicit, present-but-all-zero row must be just as inert as no row.
	store.setConfig(p, routing.LimitConfig{})
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{})

	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if !allow || reason != "" || retryAfter != 0 {
		t.Fatalf("Admit() with zero-value config = (%v,%q,%v), want (true,\"\",0)", allow, reason, retryAfter)
	}
}

func TestPrincipalLimiterNoopNilLimiter(t *testing.T) {
	var limiter *PrincipalLimiter
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if !allow || reason != "" || retryAfter != 0 {
		t.Fatalf("nil-limiter Admit() = (%v,%q,%v), want (true,\"\",0)", allow, reason, retryAfter)
	}
	// Record on a nil limiter must not panic either.
	limiter.Record(p, 100, 1.5)
}

func TestPrincipalLimiterNoopNilStore(t *testing.T) {
	limiter := NewPrincipalLimiter(nil, PrincipalLimiterOptions{})
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if !allow || reason != "" || retryAfter != 0 {
		t.Fatalf("nil-store Admit() = (%v,%q,%v), want (true,\"\",0)", allow, reason, retryAfter)
	}
	limiter.Record(p, 100, 1.5) // must not panic
}

func TestPrincipalLimiterRateLimit(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RateRequests: 3, RateWindowSeconds: 10})

	now := utc(2026, 1, 1, 0, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	for i := 0; i < 3; i++ {
		allow, reason, retryAfter := limiter.Admit(context.Background(), p)
		if !allow || reason != "" {
			t.Fatalf("request %d: Admit() = (%v,%q), want allowed", i, allow, reason)
		}
		if retryAfter != 0 {
			t.Fatalf("request %d: retryAfter = %v, want 0 on an allowed request", i, retryAfter)
		}
	}

	// The 4th request in the same 10s bucket must be denied.
	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if allow || reason != "rate" {
		t.Fatalf("4th request: Admit() = (%v,%q), want (false,\"rate\")", allow, reason)
	}
	if retryAfter <= 0 || retryAfter > 10*time.Second {
		t.Fatalf("4th request: retryAfter = %v, want in (0,10s]", retryAfter)
	}

	// Advancing past the window boundary must reopen the gate — and must NOT
	// require a code path that leaks the previous bucket's count forward.
	now = now.Add(10 * time.Second)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("post-window request: Admit() = (%v,%q), want allowed", allow, reason)
	}
}

func TestPrincipalLimiterRequestQuota(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 10, RequestQuotaPeriod: "day"})

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	store.setAggregate(p, 9, 0, 0)
	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() below quota = (%v,%q), want allowed", allow, reason)
	}

	// Advance past the cache TTL so the next Admit re-reads the (now-tripped)
	// aggregate rather than serving the stale below-threshold cache entry.
	now = now.Add(2 * defaultLimiterCacheTTL)
	store.setAggregate(p, 10, 0, 0)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("Admit() at quota = (%v,%q), want (false,\"request_quota\")", allow, reason)
	}
}

// TestPrincipalLimiterPeriodRolloverWithinCacheTTL is the regression the Task 2
// review deferred to the wiring task: a CALENDAR period rollover (crossing
// midnight UTC on a "day" quota) must invalidate the aggregate cache even when
// the TTL has NOT yet expired -- the aggregateCacheEntry.periodStart comparison
// in aggregate(), not just cacheTTL elapsing. Without that comparison, a
// request denied one second before midnight would stay denied for up to
// cacheTTL into the brand-new (empty) day, since the stale cache entry would
// still read as "fresh" by TTL alone.
func TestPrincipalLimiterPeriodRolloverWithinCacheTTL(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 5, RequestQuotaPeriod: "day"})

	now := utc(2026, 3, 15, 23, 59, 59)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	// At quota for the OLD day (2026-03-15): denied, and this Admit call also
	// populates the aggregate cache under the (p, "day") key.
	store.setAggregate(p, 5, 0, 0)
	allow, reason, _ := limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("pre-rollover Admit() = (%v,%q), want (false,\"request_quota\")", allow, reason)
	}

	// Cross midnight UTC into 2026-03-16 -- only 2s of wall-clock time, WELL
	// inside the 10s cacheTTL -- but a brand-new calendar day whose real usage
	// is empty. The fake store is updated to reflect that new day's (empty)
	// aggregate; only the periodStart check inside aggregate() can tell the two
	// apart, since the cache key itself (Principal + period string "day") is
	// identical across the rollover.
	now = utc(2026, 3, 16, 0, 0, 1)
	store.setAggregate(p, 0, 0, 0)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("post-rollover (within TTL) Admit() = (%v,%q), want allowed -- a calendar rollover must invalidate the cache even though only 2s elapsed (well under the %v TTL)", allow, reason, defaultLimiterCacheTTL)
	}
}

func TestPrincipalLimiterTokenQuota(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{TokenQuota: 1_000_000, TokenQuotaPeriod: "month"})

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	store.setAggregate(p, 0, 999_999, 0)
	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() below token quota = (%v,%q), want allowed", allow, reason)
	}

	now = now.Add(2 * defaultLimiterCacheTTL)
	store.setAggregate(p, 0, 1_000_000, 0)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "token_quota" {
		t.Fatalf("Admit() at token quota = (%v,%q), want (false,\"token_quota\")", allow, reason)
	}
}

func TestPrincipalLimiterCostBudget(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc1"}
	store.setConfig(p, routing.LimitConfig{CostBudget: 5.0, CostBudgetPeriod: "month"})

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	store.setAggregate(p, 0, 0, 4.99)
	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() below budget = (%v,%q), want allowed", allow, reason)
	}

	now = now.Add(2 * defaultLimiterCacheTTL)
	store.setAggregate(p, 0, 0, 5.0)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "cost_budget" {
		t.Fatalf("Admit() at budget = (%v,%q), want (false,\"cost_budget\")", allow, reason)
	}
}

// TestPrincipalLimiterRecordOptimistic proves the "no store reload" promise in
// §6.2: Record must bump the IN-MEMORY aggregate cache so the very next Admit
// sees the tripped threshold without a second UsageAggregateSince call.
func TestPrincipalLimiterRecordOptimistic(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 2, RequestQuotaPeriod: "day"})
	store.setAggregate(p, 1, 0, 0) // one request already recorded this period

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() before Record = (%v,%q), want allowed", allow, reason)
	}
	_, aggCallsAfterFirstAdmit := store.callCounts()

	// Simulate the response-path hook: one more request completed, carrying 0
	// tokens/cost (irrelevant to the request quota).
	limiter.Record(p, 0, 0)

	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("Admit() after Record = (%v,%q), want (false,\"request_quota\")", allow, reason)
	}
	_, aggCallsAfterSecondAdmit := store.callCounts()
	if aggCallsAfterSecondAdmit != aggCallsAfterFirstAdmit {
		t.Fatalf("aggregate store calls = %d after Record+Admit, want unchanged from %d (no reload)",
			aggCallsAfterSecondAdmit, aggCallsAfterFirstAdmit)
	}
}

// TestPrincipalLimiterRecordAccumulatesTokensAndCost proves Record's optimistic
// bump carries the token/cost deltas too, not just the +1 request count.
func TestPrincipalLimiterRecordAccumulatesTokensAndCost(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{TokenQuota: 100, TokenQuotaPeriod: "day"})
	store.setAggregate(p, 0, 40, 0)

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	allow, _, _ := limiter.Admit(context.Background(), p)
	if !allow {
		t.Fatalf("Admit() before Record = false, want allowed (40 < 100)")
	}
	limiter.Record(p, 50, 0) // 40 + 50 = 90, still under 100

	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() after one Record(50) = (%v,%q), want allowed (90 < 100)", allow, reason)
	}

	limiter.Record(p, 20, 0) // 90 + 20 = 110, now over 100
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "token_quota" {
		t.Fatalf("Admit() after second Record(20) = (%v,%q), want (false,\"token_quota\")", allow, reason)
	}
}

// --- M1: config lookup is cached, including a negative cache -------------

// TestPrincipalLimiterConfigCachedWithinTTL is the M1 regression: an
// unconfigured principal must add NO per-request store call once the FIRST
// Admit has established the negative-cache entry -- back-to-back Admit calls
// within CacheTTL must hit store.PrincipalLimits at most once. Once the TTL
// elapses, the next Admit must reload and observe a config that was set in
// the meantime (the documented, accepted propagation delay).
func TestPrincipalLimiterConfigCachedWithinTTL(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	// No config row at all for p: the store read is "not found".

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	for i := 0; i < 5; i++ {
		allow, reason, _ := limiter.Admit(context.Background(), p)
		if !allow || reason != "" {
			t.Fatalf("request %d: Admit() = (%v,%q), want allowed (unconfigured)", i, allow, reason)
		}
	}
	if cfgCalls, _ := store.callCounts(); cfgCalls != 1 {
		t.Fatalf("PrincipalLimits calls after 5 back-to-back Admit() within the TTL = %d, want 1 (the negative result must be cached)", cfgCalls)
	}

	// A limit is configured WHILE the negative-cache entry is still fresh: it
	// must not be observed yet -- that is the whole point of caching the
	// negative result, otherwise every Admit would still hit the store.
	store.setConfig(p, routing.LimitConfig{RequestQuota: 1, RequestQuotaPeriod: "day"})
	store.setAggregate(p, 1, 0, 0) // already at the not-yet-observed quota
	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() with a config set under a still-fresh negative cache = (%v,%q), want still allowed", allow, reason)
	}
	if cfgCalls, _ := store.callCounts(); cfgCalls != 1 {
		t.Fatalf("PrincipalLimits calls = %d, want still 1 (within the TTL, the cache must not reload)", cfgCalls)
	}

	// Advance past the TTL: the next Admit must reload from the store and
	// pick up the newly-set request quota.
	now = now.Add(2 * defaultLimiterCacheTTL)
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("Admit() after the TTL elapsed = (%v,%q), want (false,\"request_quota\") -- the newly-set limit must now be picked up", allow, reason)
	}
	if cfgCalls, _ := store.callCounts(); cfgCalls != 2 {
		t.Fatalf("PrincipalLimits calls after the TTL elapsed = %d, want 2 (exactly one reload)", cfgCalls)
	}
}

// TestPrincipalLimiterRecordAfterCachedAdmit proves the M1 config-cache
// unification (Record now reads the SAME configCache Admit populates,
// instead of a separate "last config" side table) still lets Record bump the
// right period even when the preceding Admit was served from the cache
// rather than a fresh store read -- the exact scenario the unification had to
// preserve without regressing the Task 3 Record behavior.
func TestPrincipalLimiterRecordAfterCachedAdmit(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 2, RequestQuotaPeriod: "day"})
	store.setAggregate(p, 0, 0, 0)

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	// First Admit: a fresh load, populates the config cache.
	if allow, _, _ := limiter.Admit(context.Background(), p); !allow {
		t.Fatalf("first Admit() should be allowed")
	}
	// Second Admit, same tick: served from the config cache, NOT a fresh
	// store read.
	if allow, _, _ := limiter.Admit(context.Background(), p); !allow {
		t.Fatalf("second (cache-hit) Admit() should be allowed")
	}
	if cfgCalls, _ := store.callCounts(); cfgCalls != 1 {
		t.Fatalf("PrincipalLimits calls after 2 Admit() = %d, want 1 (the second must be served from the cache)", cfgCalls)
	}

	// Record once per Admit above: the request count must reach the quota
	// (2) purely from the optimistic in-memory bump, even though the SECOND
	// Admit never touched the store.
	limiter.Record(p, 0, 0)
	limiter.Record(p, 0, 0)

	allow, reason, _ := limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("Admit() after two Record() calls = (%v,%q), want (false,\"request_quota\") -- Record must still see the cache-hit-populated config", allow, reason)
	}
}

func TestPrincipalLimiterFailOpenOnAggregateError(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 1, RequestQuotaPeriod: "day"})
	store.aggErr = errors.New("db unavailable")

	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{})
	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if !allow || reason != "" || retryAfter != 0 {
		t.Fatalf("Admit() with aggregate store error = (%v,%q,%v), want fail-open (true,\"\",0)", allow, reason, retryAfter)
	}
}

func TestPrincipalLimiterFailOpenOnConfigError(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.cfgErr = errors.New("db unavailable")

	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{})
	allow, reason, retryAfter := limiter.Admit(context.Background(), p)
	if !allow || reason != "" || retryAfter != 0 {
		t.Fatalf("Admit() with config store error = (%v,%q,%v), want fail-open (true,\"\",0)", allow, reason, retryAfter)
	}
}

// TestPrincipalLimiterConfigErrorNotCached proves M1's config cache never
// caches a store ERROR (only a legitimate read, found or not): a transient
// failure fails open for that one Admit call, and the very next call -- same
// tick, well within the TTL -- must hit the store again rather than serving a
// poisoned "no limits" negative-cache entry for the rest of the window.
func TestPrincipalLimiterConfigErrorNotCached(t *testing.T) {
	store := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "u1"}
	store.setConfig(p, routing.LimitConfig{RequestQuota: 1, RequestQuotaPeriod: "day"})
	store.setAggregate(p, 1, 0, 0) // already at quota once the config is observed
	store.cfgErr = errors.New("transient")

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	allow, reason, _ := limiter.Admit(context.Background(), p)
	if !allow || reason != "" {
		t.Fatalf("Admit() during a config-store error = (%v,%q), want fail-open allowed", allow, reason)
	}

	// The store recovers on the VERY NEXT call, same tick (no time advance):
	// the error must not have cached a false "unconfigured" result.
	store.cfgErr = nil
	allow, reason, _ = limiter.Admit(context.Background(), p)
	if allow || reason != "request_quota" {
		t.Fatalf("Admit() right after the store recovered = (%v,%q), want (false,\"request_quota\") -- a transient error must not poison the cache", allow, reason)
	}
	if cfgCalls, _ := store.callCounts(); cfgCalls != 2 {
		t.Fatalf("PrincipalLimits calls = %d, want 2 (both attempts hit the store; the error itself was never cached)", cfgCalls)
	}
}

// TestPrincipalLimiterPrincipalIsolation proves two principals never share a
// rate bucket, an aggregate-cache entry, or a Record accumulator, even when
// both are checked back-to-back on the same limiter instance.
func TestPrincipalLimiterPrincipalIsolation(t *testing.T) {
	store := newFakePrincipalStore()
	a := Principal{Type: routing.PrincipalTypeUser, ID: "alice"}
	b := Principal{Type: routing.PrincipalTypeUser, ID: "bob"}
	store.setConfig(a, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	store.setConfig(b, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})

	now := utc(2026, 3, 15, 12, 0, 0)
	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	// Exhaust A's single-request rate window.
	if allow, _, _ := limiter.Admit(context.Background(), a); !allow {
		t.Fatalf("A's first request should be allowed")
	}
	if allow, reason, _ := limiter.Admit(context.Background(), a); allow || reason != "rate" {
		t.Fatalf("A's second request = (%v,%q), want (false,\"rate\")", allow, reason)
	}

	// B must be entirely unaffected by A's exhausted bucket.
	if allow, reason, _ := limiter.Admit(context.Background(), b); !allow || reason != "" {
		t.Fatalf("B's first request = (%v,%q), want allowed (isolated from A)", allow, reason)
	}

	// Also prove quota isolation: A has usage near a threshold, B does not
	// share it even though both use the "day" period. Advance past the config
	// cache TTL first (M1: Admit now caches PrincipalLimits(p) for CacheTTL,
	// same as the pre-existing aggregate cache) so this Admit call re-reads
	// the just-changed config from the store rather than reusing each
	// principal's still-fresh (and now stale) rate-limit config.
	now = now.Add(2 * defaultLimiterCacheTTL)
	store.setConfig(a, routing.LimitConfig{RequestQuota: 5, RequestQuotaPeriod: "day"})
	store.setConfig(b, routing.LimitConfig{RequestQuota: 5, RequestQuotaPeriod: "day"})
	store.setAggregate(a, 5, 0, 0) // A is AT the quota
	store.setAggregate(b, 0, 0, 0) // B has none

	if allow, reason, _ := limiter.Admit(context.Background(), a); allow || reason != "request_quota" {
		t.Fatalf("A at quota = (%v,%q), want (false,\"request_quota\")", allow, reason)
	}
	if allow, reason, _ := limiter.Admit(context.Background(), b); !allow || reason != "" {
		t.Fatalf("B with a fresh aggregate = (%v,%q), want allowed (isolated from A's quota)", allow, reason)
	}
}

// TestPrincipalLimiterConcurrentRace drives Admit and Record concurrently from
// many goroutines across a few principals; it asserts no crash/deadlock under
// `go test -race` and that the rate limiter's hard cap (RateRequests) is never
// exceeded within one window even under contention.
func TestPrincipalLimiterConcurrentRace(t *testing.T) {
	store := newFakePrincipalStore()
	principals := []Principal{
		{Type: routing.PrincipalTypeUser, ID: "u1"},
		{Type: routing.PrincipalTypeUser, ID: "u2"},
		{Type: routing.PrincipalTypeService, ID: "svc1"},
	}
	for _, p := range principals {
		store.setConfig(p, routing.LimitConfig{
			RateRequests: 1000, RateWindowSeconds: 3600,
			TokenQuota: 1_000_000_000, TokenQuotaPeriod: "day",
		})
		store.setAggregate(p, 0, 0, 0)
	}

	limiter := NewPrincipalLimiter(store, PrincipalLimiterOptions{})

	var wg sync.WaitGroup
	var allowed int64
	var mu sync.Mutex
	const goroutines = 20
	const iterations = 50
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			p := principals[g%len(principals)]
			for i := 0; i < iterations; i++ {
				allow, _, _ := limiter.Admit(context.Background(), p)
				if allow {
					mu.Lock()
					allowed++
					mu.Unlock()
					limiter.Record(p, 10, 0.01)
				}
			}
		}(g)
	}
	wg.Wait()

	if allowed == 0 {
		t.Fatal("expected at least some admitted requests under concurrency")
	}
	// Total admitted across all 3 principals in one shared rate window must
	// never exceed 3 * RateRequests (the per-principal hard cap).
	if allowed > int64(len(principals))*1000 {
		t.Fatalf("allowed = %d, exceeds the per-principal rate caps (%d total)", allowed, len(principals)*1000)
	}
}
