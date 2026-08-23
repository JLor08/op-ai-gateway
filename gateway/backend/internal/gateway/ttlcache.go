// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"sync"
	"time"
)

// settingCache memoizes one periodically-refreshed value (typically a
// system_settings read) behind a TTL, so a value read on every request in a
// hot path -- the edge/mesh plaintext-refusal switches, the agent-presence and
// energy-PUE defaults, the netbird_only source-check peer IP -- does not pay
// for a store round trip (or, for the peer IP, a live NetBird call) per
// request.
//
// It supports two usage patterns through the SAME mechanism rather than two
// separate implementations:
//
//   - TTL-only: a cache that is never explicitly invalidated (systemAgentPresenceDefault,
//     systemEnergyDefaultPue, cachedGatewayPeerIP). Staleness is bounded purely by the
//     TTL passed to Get/GetTTL.
//   - Invalidatable: a cache with an explicit Invalidate() call site
//     (edgeRequireHTTPSOn/meshRequireTLSOn, invalidated by handleSystemSettings after a
//     PUT that carried the switch) on top of the same TTL.
//
// The generation counter that makes Invalidate() safe costs nothing extra for
// the TTL-only callers: a cache whose Invalidate is never called never bumps
// gen, so the "was this read raced by an invalidation" check inside Get is
// then unconditionally true and the plain-TTL behaviour falls out as the
// degenerate case.
//
// NOT a fit for every TTL cache on *Server: cachedGatewayPeerDNS
// (agent_binaries.go) deliberately holds ITS mutex across the load call so
// concurrent misses single-flight into one live NetBird resolution, which is a
// different concurrency contract than the one below (load runs OUTSIDE the
// lock so a slow load never serialises concurrent readers) -- moving it onto
// settingCache would let concurrent misses fire one NetBird call each instead
// of one for all of them. It is left as its own dedicated mutex+fields.
type settingCache[T any] struct {
	mu  sync.Mutex
	val T
	exp time.Time
	gen uint64
}

// Get returns the cached value if it has not expired; otherwise it calls load
// OUTSIDE the lock (a store/network read may block, and holding the mutex
// across it would serialise every concurrent caller behind one round trip),
// caches the result for ttl, and returns it.
//
// The result is published back into the cache only if no Invalidate() ran
// while load was in flight (checked via a generation counter captured before
// the unlocked call). Without that check, a load that STARTED before a
// disarming Invalidate could finish AFTER it and overwrite the invalidation
// with its now-stale value under a fresh TTL -- reviving a switch the operator
// just turned off for up to another full ttl, exactly the window they are
// watching to confirm the change took effect. A read racing another READ is
// still harmless: both computed the same answer, and an out-of-band change
// between them is the ordinary TTL-bounded staleness the cache already
// tolerates.
func (c *settingCache[T]) Get(ctx context.Context, ttl time.Duration, load func(context.Context) T) T {
	return c.get(ctx, func(ctx context.Context) (T, time.Duration) {
		return load(ctx), ttl
	})
}

// GetTTL is Get's variant for a load func that picks its own TTL per call
// depending on the outcome -- e.g. cachedGatewayPeerIP (agent_netbird_gate.go)
// caches a resolved (or legitimately empty) peer IP for the full TTL but an
// error-derived miss for a much shorter one, so a transient NetBird blip
// self-heals fast instead of holding the fail-open miss for the whole window.
// Same publish-on-load semantics as Get otherwise.
func (c *settingCache[T]) GetTTL(ctx context.Context, load func(context.Context) (T, time.Duration)) T {
	return c.get(ctx, load)
}

func (c *settingCache[T]) get(ctx context.Context, load func(context.Context) (T, time.Duration)) T {
	now := time.Now()
	c.mu.Lock()
	if now.Before(c.exp) {
		v := c.val
		c.mu.Unlock()
		return v
	}
	gen := c.gen
	c.mu.Unlock()

	v, ttl := load(ctx)

	c.mu.Lock()
	if c.gen == gen {
		c.val = v
		c.exp = time.Now().Add(ttl)
	}
	c.mu.Unlock()
	return v
}

// Invalidate discards the cached value immediately and bumps the generation
// counter, so a load already in flight when Invalidate runs (see Get) cannot
// write its pre-invalidation result back afterwards. A cache that never calls
// Invalidate (the TTL-only callers above) simply never exercises this path.
func (c *settingCache[T]) Invalidate() {
	c.mu.Lock()
	c.exp = time.Time{}
	c.gen++
	c.mu.Unlock()
}

// warnThrottle rate-limits a per-key log line to at most once per interval,
// with the FIRST occurrence of any key always logged immediately. It backs
// the edge/mesh plaintext-gate refusal Warns (shouldLogEdgeGateRefusal /
// shouldLogMeshGateRefusal): without it, a single retrying client hammering a
// refused path would emit one Warn per attempt and evict the log ring's other
// entries -- destroying the very record a locked-out operator needs to read.
//
// The map is bounded by pruning every entry older than interval on each
// ADMITTED (ShouldLog returning true) call, so only keys logged within the
// last interval are retained, and a key is only ever inserted when it is
// actually admitted -- never on a suppressed call -- so a caller cannot grow
// the map by probing keys that never get logged.
type warnThrottle struct {
	mu sync.Mutex
	at map[string]time.Time
}

// ShouldLog reports whether a log line for key at now should be emitted.
func (w *warnThrottle) ShouldLog(key string, now time.Time, interval time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.at[key]; ok && now.Sub(last) < interval {
		return false
	}
	if w.at == nil {
		w.at = make(map[string]time.Time)
	}
	for k, t := range w.at {
		if now.Sub(t) >= interval {
			delete(w.at, k)
		}
	}
	w.at[key] = now
	return true
}
