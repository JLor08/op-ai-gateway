// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strconv"
	"sync"
	"time"
)

// Claude Code drops its per-window usage display whenever a /v1/messages response
// carries no unified rate-limit headers. rateLimitWindowCache remembers a principal's
// last subscription observation so those windows can be attached to a response from
// another provider and the display survives a mixed session.

// rateLimitWindowNames are the windows whose figures are cached, in attach order.
var rateLimitWindowNames = [...]string{"5h", "7d", "7d_oi"}

// rateLimitWindowFields are the per-window header suffixes (after "-<w>-").
var rateLimitWindowFields = [...]string{"utilization", "reset", "status", "surpassed-threshold"}

// rateLimitClaimWindow maps a representative-claim value to the window it names.
var rateLimitClaimWindow = map[string]string{
	"five_hour":        "5h",
	"seven_day":        "7d",
	"seven_day_opus":   "7d",
	"seven_day_sonnet": "7d",
}

// cachedRateLimitWindow is one window's stored header values; fields absent upstream
// stay empty and are not attached.
type cachedRateLimitWindow struct {
	fields map[string]string
	reset  time.Time
}

type cachedRateLimitEntry struct {
	// accountID is the vendor account the observation came from, so a later
	// revocation seen on that account can drop it (Forget).
	accountID string
	observed  time.Time
	claim     string
	windows   map[string]cachedRateLimitWindow
}

// rateLimitWindowCache is a mutex-guarded, hard-capped map from principal key to its
// last observed subscription windows. Safe for concurrent use.
type rateLimitWindowCache struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]cachedRateLimitEntry
}

func newRateLimitWindowCache(maxEntries int) *rateLimitWindowCache {
	return &rateLimitWindowCache{maxEntries: maxEntries, entries: make(map[string]cachedRateLimitEntry)}
}

// principalKey is the cache key for the caller, "<type>:<id>"; false when the token
// carries no principal.
func principalKey(token auth.Token) (string, bool) {
	p, ok := principalFor(token)
	if !ok {
		return "", false
	}
	return p.Type + ":" + p.ID, true
}

// Observe stores the unified windows of h as the principal's latest observation. A
// response without the top-level status header is ignored, so a non-subscription
// response never overwrites the cache. Expired entries are pruned on write; at the
// cap the entry with the oldest observation is evicted.
func (c *rateLimitWindowCache) Observe(key, accountID string, h http.Header, now time.Time) {
	if h.Get(unifiedRateLimitPrefix+"status") == "" {
		return
	}
	entry := cachedRateLimitEntry{
		accountID: accountID,
		observed:  now,
		claim:     h.Get(unifiedRateLimitPrefix + "representative-claim"),
		windows:   parseRateLimitWindows(h),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneAndMakeRoom(key, now)
	c.entries[key] = entry
}

// parseRateLimitWindows reads the per-window fields of h. A window whose reset is
// missing or not a unix timestamp is skipped: without a reset it could never expire.
func parseRateLimitWindows(h http.Header) map[string]cachedRateLimitWindow {
	windows := make(map[string]cachedRateLimitWindow, len(rateLimitWindowNames))
	for _, w := range rateLimitWindowNames {
		win := cachedRateLimitWindow{fields: make(map[string]string, len(rateLimitWindowFields))}
		for _, f := range rateLimitWindowFields {
			if v := h.Get(unifiedRateLimitPrefix + w + "-" + f); v != "" {
				win.fields[f] = v
			}
		}
		secs, err := strconv.ParseInt(win.fields["reset"], 10, 64)
		if err != nil {
			continue
		}
		win.reset = time.Unix(secs, 0)
		windows[w] = win
	}
	return windows
}

// pruneAndMakeRoom drops every entry whose windows have all reset and, when key is new
// and the cache is at its cap, evicts the oldest observations until one slot is free.
// The caller holds c.mu.
func (c *rateLimitWindowCache) pruneAndMakeRoom(key string, now time.Time) {
	for k, e := range c.entries {
		if !e.hasFutureWindow(now) {
			delete(c.entries, k)
		}
	}
	if _, replacing := c.entries[key]; replacing || c.maxEntries <= 0 {
		return
	}
	for len(c.entries) >= c.maxEntries {
		c.evictOldest()
	}
}

// Forget deletes the principal's entry when, and only when, it was observed from
// accountID. An entry from another account (or none) is left alone, so a revocation on
// one account never discards figures legitimately observed from another.
func (c *rateLimitWindowCache) Forget(key, accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.accountID == accountID {
		delete(c.entries, key)
	}
}

// evictOldest removes the entry with the oldest observation. The caller holds c.mu.
func (c *rateLimitWindowCache) evictOldest() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, e := range c.entries {
		if first || e.observed.Before(oldest) {
			oldestKey, oldest, first = k, e.observed, false
		}
	}
	if !first {
		delete(c.entries, oldestKey)
	}
}

func (e cachedRateLimitEntry) hasFutureWindow(now time.Time) bool {
	for _, w := range e.windows {
		if w.reset.After(now) {
			return true
		}
	}
	return false
}

// Attach writes the principal's still-open windows into dst and reports whether it
// did. The top-level status is allowed_warning when any attached window is warning or
// rejected, otherwise allowed; it is never rejected, because a response from another
// provider was not refused. The representative claim and reset are written only when
// the claim maps to an attached window. When no window is in the future the entry is
// deleted and false returned.
func (c *rateLimitWindowCache) Attach(key string, dst http.Header, now time.Time) bool {
	entry, ok := c.liveEntry(key, now)
	if !ok {
		return false
	}
	attached, status := entry.writeOpenWindows(dst, now)
	dst.Set(unifiedRateLimitPrefix+"status", status)
	if win, ok := attached[rateLimitClaimWindow[entry.claim]]; ok {
		dst.Set(unifiedRateLimitPrefix+"representative-claim", entry.claim)
		dst.Set(unifiedRateLimitPrefix+"reset", win.fields["reset"])
	}
	return true
}

// liveEntry returns the principal's entry while at least one of its windows is still
// open; an entry whose windows have all reset is deleted and reported as absent.
func (c *rateLimitWindowCache) liveEntry(key string, now time.Time) (cachedRateLimitEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok && !entry.hasFutureWindow(now) {
		delete(c.entries, key)
		return cachedRateLimitEntry{}, false
	}
	return entry, ok
}

// writeOpenWindows writes the fields of every window still open at now into dst and
// returns those windows plus the top-level status they imply: allowed_warning when any
// of them is warning or rejected, otherwise allowed (never rejected).
func (e cachedRateLimitEntry) writeOpenWindows(dst http.Header, now time.Time) (map[string]cachedRateLimitWindow, string) {
	status := "allowed"
	attached := make(map[string]cachedRateLimitWindow, len(e.windows))
	for _, w := range rateLimitWindowNames {
		win, present := e.windows[w]
		if !present || !win.reset.After(now) {
			continue
		}
		attached[w] = win
		for _, f := range rateLimitWindowFields {
			if v, set := win.fields[f]; set {
				dst.Set(unifiedRateLimitPrefix+w+"-"+f, v)
			}
		}
		if s := win.fields["status"]; s == "allowed_warning" || s == "rejected" {
			status = "allowed_warning"
		}
	}
	return attached, status
}

// observeRateLimitWindows records the unified windows of a vendor-Anthropic response
// for the caller, when the caller is entitled to see that account's figures. Other
// providers and responses without the top-level status are ignored; the entitlement
// lookup only runs when there is something to record. An unentitled caller records
// nothing and, because the response proves the caller now reaches that account
// without entitlement (a revoked opt-in or role), loses any entry cached from it.
func (s *Server) observeRateLimitWindows(token auth.Token, target routing.Target, h http.Header) {
	if s.rateLimitWindows == nil || target.Provider != routing.ProviderVendorAnthropic || h.Get(unifiedRateLimitPrefix+"status") == "" {
		return
	}
	key, ok := principalKey(token)
	if !ok {
		return
	}
	if !s.rateLimitFiguresEntitled(context.Background(), token, target) {
		s.rateLimitWindows.Forget(key, target.VendorAccountID)
		return
	}
	s.rateLimitWindows.Observe(key, target.VendorAccountID, h, time.Now())
}

// attachRateLimitWindows writes the caller's last observed subscription windows into
// dst, the header set of a /v1/messages response answered by another provider. The
// callers decide that the flavor is Anthropic Messages and the response a success.
func (s *Server) attachRateLimitWindows(token auth.Token, dst http.Header) {
	if s.rateLimitWindows == nil {
		return
	}
	if key, ok := principalKey(token); ok {
		s.rateLimitWindows.Attach(key, dst, time.Now())
	}
}
