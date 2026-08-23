// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// defaultLimiterCacheTTL is how long a UsageAggregateSince read is cached per
// (principal, period) before being reloaded from the store — see §6.2 of the
// design spec. 10s keeps the DB read rate low under a burst of requests from
// the same principal while staying close enough to real-time that a quota/
// budget trips promptly.
const defaultLimiterCacheTTL = 10 * time.Second

// Principal identifies who a request's rate/quota/budget limits are checked
// against: either a Service (routing.PrincipalTypeService, service id) or a
// User (routing.PrincipalTypeUser, user id) — see design spec §3. It is a
// plain, comparable struct so it can be used directly as a map key (every
// per-principal state below — rate buckets, the aggregate cache, the
// last-seen config — is keyed on it, which is what gives two principals their
// isolation for free).
type Principal struct {
	Type string
	ID   string
}

// PrincipalLimitStore is the narrow slice of routing.Store the limiter needs.
// routing.Store already implements it structurally (Go interfaces are
// satisfied implicitly), so a caller wires the limiter with the same store the
// rest of the gateway uses; tests inject a lightweight fake instead of a real
// DB-backed Store.
type PrincipalLimitStore interface {
	// PrincipalLimits reads a principal's config; ok is false when no row
	// exists (no limits configured — mirrors routing.Store.PrincipalLimits).
	PrincipalLimits(ctx context.Context, principalType, principalID string) (routing.LimitConfig, bool, error)
	// UsageAggregateSince sums a principal's request count, token count, and
	// price-weighted cost since the given time (mirrors
	// routing.Store.UsageAggregateSince).
	UsageAggregateSince(ctx context.Context, principalType, principalID string, since time.Time) (requests int64, tokens int64, cost float64, err error)
}

// PrincipalLimiterOptions configures a PrincipalLimiter. Every field is
// optional; the zero value produces sane defaults.
type PrincipalLimiterOptions struct {
	// CacheTTL is how long an aggregate-cache entry stays fresh before the next
	// Admit reloads it from the store. <=0 uses defaultLimiterCacheTTL (10s).
	CacheTTL time.Duration
	// Now returns the current time. Injectable so tests are deterministic
	// (advance a window boundary, a calendar period, ... without sleeping);
	// nil uses time.Now().UTC(). The limiter NEVER calls time.Now() directly —
	// every time-dependent decision goes through l.now().
	Now func() time.Time
}

// aggregateCacheKey identifies one cached UsageAggregateSince read: a
// principal's aggregate for one specific quota/budget PERIOD (a principal can
// have its request-quota, token-quota, and cost-budget on three different
// periods, each tracked independently).
type aggregateCacheKey struct {
	Principal
	period string
}

// aggregateCacheEntry is one cached calendar-period aggregate. periodStart is
// recorded alongside the sum so a period ROLLOVER (e.g. crossing midnight UTC
// on a "day" quota) is detected even within the TTL window — a stale entry
// from the just-ended period is never mistaken for the new one's (empty) start.
type aggregateCacheEntry struct {
	requests    int64
	tokens      int64
	cost        float64
	periodStart time.Time
	loadedAt    time.Time
}

// principalConfigCacheEntry is one cached PrincipalLimits(p) read, mirroring
// aggregateCacheEntry's freshness model: loadedAt lets Admit decide, purely by
// elapsed wall-clock time against CacheTTL, whether to trust the cached value
// or re-read the store. found distinguishes "the store returned a present
// row" from "no row exists" for documentation purposes only — cfg is already
// the zero value in the latter case, so nothing downstream branches on found;
// it exists so a reader of the cache doesn't have to infer "not configured"
// from "happens to be the zero value" (the exact confusion the "found bool"
// design in the underlying store method itself avoids).
type principalConfigCacheEntry struct {
	cfg      routing.LimitConfig
	found    bool
	loadedAt time.Time
}

// rateBucketState is one principal's tumbling-window rate-limit counter: the
// bucket index it belongs to (floor(now.Unix()/window)) and how many requests
// have been admitted in that bucket so far. Only the CURRENT bucket is ever
// retained per principal — the moment a new bucket starts, the old count is
// simply overwritten (see PrincipalLimiter.checkRate), which is what keeps
// this map bounded by "one entry per principal ever seen" rather than growing
// with the number of elapsed windows.
type rateBucketState struct {
	bucket int64
	count  int
}

// PrincipalLimiter enforces the four optional per-principal limits from the
// design spec (§1/§6): a short in-memory rate limit, and persistent-store-
// backed request/token quotas and a cost budget, each on its own calendar
// period. It is nil-safe throughout: a nil *PrincipalLimiter, a nil store, or
// an unconfigured principal all make Admit a pure allow and Record a no-op —
// so a caller can always wire a limiter, even before any principal has any
// limits set, with zero behavioral difference from not having the feature.
//
// Concurrency: two independent mutexes guard two independent pieces of state
// (mu: the aggregate cache + last-seen config; rateMu: the rate-limit
// buckets) — Admit never holds one while acquiring the other, so there is no
// lock-ordering hazard between them.
type PrincipalLimiter struct {
	store    PrincipalLimitStore
	cacheTTL time.Duration
	now      func() time.Time

	mu          sync.Mutex
	aggCache    map[aggregateCacheKey]*aggregateCacheEntry
	configCache map[Principal]*principalConfigCacheEntry // most recent config Admit loaded per principal, cached for CacheTTL — also what Record reads to know which periods to bump (see configFor)

	rateMu      sync.Mutex
	rateBuckets map[Principal]*rateBucketState
}

// NewPrincipalLimiter builds a PrincipalLimiter backed by store. A nil store
// is accepted (the limiter then behaves as a permanent no-op — see the
// nil-safety guarantees on Admit/Record).
func NewPrincipalLimiter(store PrincipalLimitStore, opts PrincipalLimiterOptions) *PrincipalLimiter {
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultLimiterCacheTTL
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &PrincipalLimiter{
		store:       store,
		cacheTTL:    ttl,
		now:         now,
		aggCache:    make(map[aggregateCacheKey]*aggregateCacheEntry),
		configCache: make(map[Principal]*principalConfigCacheEntry),
		rateBuckets: make(map[Principal]*rateBucketState),
	}
}

// periodStart returns the UTC-aligned start of the calendar period containing
// now — see design spec §5. The actual calendar math now lives in
// routing.PeriodStart (promoted there so internal/portal's LimitConfigDTO
// management layer, which cannot import this package, shares the exact same
// logic rather than a hand-duplicated copy); this wrapper exists only so the
// package's own call sites and tests keep their short, unqualified name.
func periodStart(period string, now time.Time) time.Time {
	return routing.PeriodStart(period, now)
}

// Admit decides whether a request from principal p may proceed, checking (in
// order — the cheapest first, and returning on the FIRST breach per design
// spec §6.1): the in-memory rate limit, then the persistent request quota,
// token quota, and cost budget, each only if configured (threshold > 0 AND a
// period is set). reason is one of "" (allowed), "rate", "request_quota",
// "token_quota", "cost_budget". retryAfter is only meaningful for "rate" (how
// long until the current rate window ends); it is always 0 otherwise.
//
// Nil-safe: a nil limiter, a nil store, a principal with no configured limits
// (no row, or a present-but-all-zero row) — all simply allow.
//
// Fail-open: a store error reading either the config or an aggregate is
// Debug-logged and treated as "no limit" for that read (never denies because
// the DB was unavailable — design spec §11).
//
// The config itself is cached per principal for CacheTTL (configFor), the
// same freshness window §6.2 already gives the aggregate reads — including a
// NEGATIVE cache entry for an unconfigured principal, so the hot path for the
// overwhelming common case (a principal with no limits set at all) costs one
// PrincipalLimits store read per CacheTTL window rather than one per request.
// Consequence: a limit change (including a limit newly set on a previously-
// unconfigured principal) takes up to CacheTTL to be observed by Admit —
// accepted, symmetric with the pre-existing aggregate-cache propagation delay.
func (l *PrincipalLimiter) Admit(ctx context.Context, p Principal) (allow bool, reason string, retryAfter time.Duration) {
	if l == nil || l.store == nil {
		return true, "", 0
	}

	now := l.now()
	cfg, ok := l.configFor(ctx, p, now)
	if !ok {
		// A store error on this read: fail open for THIS call only (never cache
		// the failure — a transient blip must not entrench a false "no limits"
		// verdict for the rest of the TTL window, and it must not disturb an
		// already-cached good config either).
		return true, "", 0
	}

	if cfg == (routing.LimitConfig{}) {
		return true, "", 0 // no limits configured at all: skip every check, including the rate bucket
	}

	if deny, wait := l.checkRate(p, cfg, now); deny {
		return false, "rate", wait
	}

	if cfg.RequestQuota > 0 && cfg.RequestQuotaPeriod != "" {
		reqs, _, _ := l.aggregate(ctx, p, cfg.RequestQuotaPeriod, now)
		if reqs >= int64(cfg.RequestQuota) {
			return false, "request_quota", 0
		}
	}
	if cfg.TokenQuota > 0 && cfg.TokenQuotaPeriod != "" {
		_, toks, _ := l.aggregate(ctx, p, cfg.TokenQuotaPeriod, now)
		if toks >= cfg.TokenQuota {
			return false, "token_quota", 0
		}
	}
	if cfg.CostBudget > 0 && cfg.CostBudgetPeriod != "" {
		_, _, cost := l.aggregate(ctx, p, cfg.CostBudgetPeriod, now)
		if cost >= cfg.CostBudget {
			return false, "cost_budget", 0
		}
	}
	return true, "", 0
}

// checkRate applies the tumbling-window rate limit (design spec §6.1). It is
// a no-op (never denies) when the config's rate fields are not both positive.
// Admitting increments the current bucket's counter immediately — the
// admission itself counts against the window, independent of whatever the
// subsequent quota/budget checks decide.
func (l *PrincipalLimiter) checkRate(p Principal, cfg routing.LimitConfig, now time.Time) (deny bool, retryAfter time.Duration) {
	if cfg.RateRequests <= 0 || cfg.RateWindowSeconds <= 0 {
		return false, 0
	}
	window := int64(cfg.RateWindowSeconds)
	bucket := now.Unix() / window

	l.rateMu.Lock()
	defer l.rateMu.Unlock()

	st, ok := l.rateBuckets[p]
	if !ok || st.bucket != bucket {
		// New principal, or the window has rolled over: start a fresh bucket.
		// This single overwrite is also the "prune old buckets" step — nothing
		// from a stale bucket is ever retained.
		st = &rateBucketState{bucket: bucket}
		l.rateBuckets[p] = st
	}
	if st.count >= cfg.RateRequests {
		bucketEnd := time.Unix((bucket+1)*window, 0).UTC()
		wait := bucketEnd.Sub(now)
		if wait < 0 {
			wait = 0
		}
		return true, wait
	}
	st.count++
	return false, 0
}

// aggregate returns the current calendar-period sum for principal p on the
// given period, either from the in-memory cache (when fresh — same period AND
// within the TTL, per design spec §6.2) or freshly loaded from the store. A
// store error is Debug-logged and treated as a zero aggregate (fail-open —
// zero is always <= any positive threshold, so this can never wrongly deny).
func (l *PrincipalLimiter) aggregate(ctx context.Context, p Principal, period string, now time.Time) (requests, tokens int64, cost float64) {
	ps := periodStart(period, now)
	key := aggregateCacheKey{Principal: p, period: period}

	l.mu.Lock()
	entry, ok := l.aggCache[key]
	fresh := ok && entry.periodStart.Equal(ps) && now.Sub(entry.loadedAt) < l.cacheTTL
	if fresh {
		requests, tokens, cost = entry.requests, entry.tokens, entry.cost
	}
	l.mu.Unlock()
	if fresh {
		return requests, tokens, cost
	}

	requests, tokens, cost, err := l.store.UsageAggregateSince(ctx, p.Type, p.ID, ps)
	if err != nil {
		slog.Debug("principal limiter: aggregate read failed, failing open",
			"principal_type", p.Type, "principal_id", p.ID, "period", period, "err", err)
		return 0, 0, 0
	}

	l.mu.Lock()
	l.aggCache[key] = &aggregateCacheEntry{
		requests: requests, tokens: tokens, cost: cost,
		periodStart: ps, loadedAt: now,
	}
	l.mu.Unlock()
	return requests, tokens, cost
}

// configFor returns principal p's LimitConfig, preferring a cached value that
// is still within CacheTTL of when it was loaded (mirroring aggregate's own
// freshness check) over a fresh PrincipalLimits store read. ok is false ONLY
// on a store error (the caller must then fail open for that call WITHOUT
// caching the failure — see Admit); a legitimate "no row for this principal"
// result is cached and returned as ok=true, cfg=zero (the negative cache).
//
// This is also where Record's input is populated: the cache this method
// writes IS what Record reads (via the same configCache map) to know which
// periods to optimistically bump, so unifying the two into one cache — rather
// than keeping a separate "last config Admit saw" side table — guarantees
// Record never disagrees with the value Admit itself just used.
func (l *PrincipalLimiter) configFor(ctx context.Context, p Principal, now time.Time) (cfg routing.LimitConfig, ok bool) {
	l.mu.Lock()
	entry, hit := l.configCache[p]
	fresh := hit && now.Sub(entry.loadedAt) < l.cacheTTL
	if fresh {
		cfg = entry.cfg
	}
	l.mu.Unlock()
	if fresh {
		return cfg, true
	}

	loaded, found, err := l.store.PrincipalLimits(ctx, p.Type, p.ID)
	if err != nil {
		slog.Debug("principal limiter: config read failed, failing open",
			"principal_type", p.Type, "principal_id", p.ID, "err", err)
		return routing.LimitConfig{}, false
	}
	if !found {
		loaded = routing.LimitConfig{}
	}

	l.mu.Lock()
	l.configCache[p] = &principalConfigCacheEntry{cfg: loaded, found: found, loadedAt: now}
	l.mu.Unlock()
	return loaded, true
}

// Record is the post-response hook (design spec §6.2/§6.3): it optimistically
// bumps the in-memory aggregate-cache entry for every one of p's CONFIGURED
// quota/budget periods by one request, tokens tokens, and cost cost, so a
// dense run of requests from the same principal sees the update immediately —
// without waiting for the cache TTL to expire and reloading from the store.
// The store itself remains the source of truth (a restart, a period
// rollover, or the next TTL expiry all reconcile back to it).
//
// Record takes no context and does no I/O: it only ever touches in-memory
// state populated by a prior Admit call for the same principal, which is what
// makes it safe to call unconditionally from the response path (it can never
// block or fail). If Admit was never called for p (so its config is unknown),
// or p has no configured quota/budget, Record is a no-op.
//
// Nil-safe: a nil limiter or a nil store make this a no-op.
func (l *PrincipalLimiter) Record(p Principal, tokens int64, cost float64) {
	if l == nil || l.store == nil {
		return
	}

	l.mu.Lock()
	entry, ok := l.configCache[p]
	l.mu.Unlock()
	if !ok || entry.cfg == (routing.LimitConfig{}) {
		return
	}
	cfg := entry.cfg

	// Collect the distinct configured periods (a request-quota and a
	// token-quota, say, may use different periods, and both may coincide with
	// the cost-budget's — de-dup via a set so a shared period is bumped once).
	periods := make(map[string]struct{}, 3)
	if cfg.RequestQuota > 0 && cfg.RequestQuotaPeriod != "" {
		periods[cfg.RequestQuotaPeriod] = struct{}{}
	}
	if cfg.TokenQuota > 0 && cfg.TokenQuotaPeriod != "" {
		periods[cfg.TokenQuotaPeriod] = struct{}{}
	}
	if cfg.CostBudget > 0 && cfg.CostBudgetPeriod != "" {
		periods[cfg.CostBudgetPeriod] = struct{}{}
	}
	if len(periods) == 0 {
		return
	}

	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for period := range periods {
		ps := periodStart(period, now)
		key := aggregateCacheKey{Principal: p, period: period}
		entry, ok := l.aggCache[key]
		if !ok || !entry.periodStart.Equal(ps) {
			// No cache entry yet (Admit never checked this period) or the period
			// has rolled over since it was cached: nothing safe to bump in
			// memory — the next Admit reloads a correct aggregate from the store.
			continue
		}
		entry.requests++
		entry.tokens += tokens
		entry.cost += cost
	}
}
