// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package ttlcache holds Cache, the one TTL memoizer behind every
// periodically-refreshed setting on a request hot path (the gateway's edge/mesh
// plaintext-refusal switches, the vendor-account flags, the agent-presence and
// energy-PUE defaults, ...; the portal's sharing flags). It lives in its own leaf
// package, with no internal imports, so that internal/gateway and internal/portal
// can share ONE implementation of its invalidation-race handling: portal cannot
// import gateway (arch_test.go), and two copies of a generation-counter cache
// are two places to get that subtle part wrong.
package ttlcache

import (
	"context"
	"sync"
	"time"
)

// Cache memoizes one periodically-refreshed value (typically a system_settings
// read) behind a TTL, so a value read on every request in a hot path does not pay
// for a store round trip (or, for the peer IP, a live NetBird call) per request.
// The zero value is an empty, ready-to-use cache; a Cache must not be copied after
// first use.
//
// It supports two usage patterns through the SAME mechanism rather than two
// separate implementations:
//
//   - TTL-only: a cache that is never explicitly invalidated (the agent-presence
//     and energy-PUE defaults, the netbird_only source peer IP). Staleness is
//     bounded purely by the TTL passed to Get/GetTTL.
//   - Invalidatable: a cache with an explicit Invalidate() call site (the edge/mesh
//     plaintext gates and the vendor flags, invalidated after a settings PUT that
//     carried the switch) on top of the same TTL.
//
// The generation counter that makes Invalidate() safe costs nothing extra for
// the TTL-only callers: a cache whose Invalidate is never called never bumps
// gen, so the "was this read raced by an invalidation" check inside Get is
// then unconditionally true and the plain-TTL behaviour falls out as the
// degenerate case.
//
// NOT a fit for every TTL cache: the gateway's cachedGatewayPeerDNS
// (agent_binaries.go) deliberately holds ITS mutex across the load call so
// concurrent misses single-flight into one live NetBird resolution, which is a
// different concurrency contract than the one below (load runs OUTSIDE the
// lock so a slow load never serialises concurrent readers) -- moving it onto
// Cache would let concurrent misses fire one NetBird call each instead
// of one for all of them. It is left as its own dedicated mutex+fields.
type Cache[T any] struct {
	// Now is the clock the TTL is measured against; nil means time.Now. It is a
	// seam for tests that must cross a TTL boundary without sleeping, and is left
	// unset in production. Set it before the first Get.
	Now func() time.Time

	mu  sync.Mutex
	val T
	exp time.Time
	gen uint64
}

func (c *Cache[T]) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
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
func (c *Cache[T]) Get(ctx context.Context, ttl time.Duration, load func(context.Context) T) T {
	return c.get(ctx, func(ctx context.Context) (T, time.Duration) {
		return load(ctx), ttl
	})
}

// GetTTL is Get's variant for a load func that picks its own TTL per call
// depending on the outcome -- e.g. the gateway's cachedGatewayPeerIP
// (agent_netbird_gate.go) caches a resolved (or legitimately empty) peer IP for
// the full TTL but an error-derived miss for a much shorter one, so a transient
// NetBird blip self-heals fast instead of holding the fail-open miss for the whole
// window. A TTL of zero (or less) caches nothing: the next call loads again. Same
// publish-on-load semantics as Get otherwise.
func (c *Cache[T]) GetTTL(ctx context.Context, load func(context.Context) (T, time.Duration)) T {
	return c.get(ctx, load)
}

func (c *Cache[T]) get(ctx context.Context, load func(context.Context) (T, time.Duration)) T {
	now := c.now()
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
		c.exp = c.now().Add(ttl)
	}
	c.mu.Unlock()
	return v
}

// ExpiresAt reports when the cached value stops being served: the zero time while
// nothing is cached or after Invalidate. It exists so a test can assert on the TTL a
// GetTTL load chose (the gateway's source-peer-IP error miss) without reaching into
// the struct.
func (c *Cache[T]) ExpiresAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exp
}

// Invalidate discards the cached value immediately and bumps the generation
// counter, so a load already in flight when Invalidate runs (see Get) cannot
// write its pre-invalidation result back afterwards. A cache that never calls
// Invalidate (the TTL-only callers above) simply never exercises this path.
func (c *Cache[T]) Invalidate() {
	c.mu.Lock()
	c.exp = time.Time{}
	c.gen++
	c.mu.Unlock()
}
