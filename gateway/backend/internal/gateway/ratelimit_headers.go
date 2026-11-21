// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"time"
)

// unifiedRateLimitPrefix starts every header of Anthropic's "unified" subscription
// rate-limit family (anthropic-ratelimit-unified-*), the figures Claude Code renders
// as its usage limits.
const unifiedRateLimitPrefix = "anthropic-ratelimit-unified-"

// rateLimitEntitlementErrorTTL is how long a failed or timed-out portal lookup is
// remembered as "not entitled", so a store outage costs one lookup per principal and
// account per window rather than one per request.
const rateLimitEntitlementErrorTTL = 5 * time.Second

// rateLimitEntitlementTimeout bounds one portal lookup, so a slow store cannot hold
// back the response status line.
const rateLimitEntitlementTimeout = 2 * time.Second

// rateLimitEntitlementTTL bounds how long a portal answer for "may this principal see
// this account's rate-limit figures" is reused on the inference hot path.
const rateLimitEntitlementTTL = 60 * time.Second

// isUnifiedRateLimitHeader reports whether name is in the unified rate-limit family
// (case-insensitive prefix test).
func isUnifiedRateLimitHeader(name string) bool {
	return len(name) >= len(unifiedRateLimitPrefix) && strings.EqualFold(name[:len(unifiedRateLimitPrefix)], unifiedRateLimitPrefix)
}

// hasUnifiedRateLimitHeader reports whether h carries any unified rate-limit header,
// i.e. whether there is anything for the entitlement gate to decide.
func hasUnifiedRateLimitHeader(h http.Header) bool {
	for name := range h {
		if isUnifiedRateLimitHeader(name) {
			return true
		}
	}
	return false
}

// redactCapturedAccountHeaders keeps a vendor account's figures out of a payload
// capture the caller's principal is not entitled to see. The translate path stores the
// upstream response headers verbatim, and a capture is readable by its owner and, for
// non-secret captures, by admins, so for a vendor-account target the unified
// rate-limit family and anthropic-organization-id are dropped (from a copy; the
// sink's own header set is shared with the usage scrape) unless the principal is
// entitled to the figures. The entitlement lookup only runs when such a header is
// present; it is cached and fails closed.
func (s *Server) redactCapturedAccountHeaders(ctx context.Context, token auth.Token, target routing.Target, h http.Header) http.Header {
	if target.VendorAccountID == "" || len(h) == 0 {
		return h
	}
	sensitive := false
	for name := range h {
		if isUnifiedRateLimitHeader(name) || strings.EqualFold(name, "anthropic-organization-id") {
			sensitive = true
			break
		}
	}
	if !sensitive || s.rateLimitFiguresEntitled(ctx, token, target) {
		return h
	}
	out := make(http.Header, len(h))
	for name, values := range h {
		if isUnifiedRateLimitHeader(name) || strings.EqualFold(name, "anthropic-organization-id") {
			continue
		}
		out[name] = values
	}
	return out
}

// forwardRateLimitHeaders copies the response headers the Messages passthrough relays
// from src to dst: retry-after and x-should-retry always (they steer the client's retry
// behavior), the unified rate-limit family only when includeUnified is set. Every value
// of a header is copied; nothing else is (request ids, organization ids and the
// per-minute limit headers stay upstream).
func forwardRateLimitHeaders(dst, src http.Header, includeUnified bool) {
	for name, values := range src {
		canon := http.CanonicalHeaderKey(name)
		if canon != "Retry-After" && canon != "X-Should-Retry" && !(includeUnified && isUnifiedRateLimitHeader(canon)) {
			continue
		}
		for _, v := range values {
			dst.Add(canon, v)
		}
	}
}

// rateLimitEntitlementCache is the TTL cache of portal.VendorAccountUsageManager
// answers. Its zero value is ready to use, so a Server built without New works.
type rateLimitEntitlementCache struct {
	mu      sync.Mutex
	entries map[string]rateLimitEntitlement
}

type rateLimitEntitlement struct {
	ok      bool
	expires time.Time
}

// rateLimitFiguresEntitled reports whether the caller may receive the unified
// rate-limit figures of the account target was routed through. Evaluated in order: a
// self-hosted target (no vendor account) is always entitled; so is the account's owner;
// so is anyone when the owner shared the figures with members; otherwise the portal
// decides (system scope or a community manager of the account), cached per principal
// and account. A portal error, a lookup slower than rateLimitEntitlementTimeout, or no
// portal, is not entitled (fail closed; the failure is cached for
// rateLimitEntitlementErrorTTL).
//
// Inference narrows the portal predicate: API tokens never carry system scope (it
// exists only on an elevated session principal, and /v1/messages is bearer-only), so on
// this path the figures reach the owner, a community manager of a shared/full account,
// or anyone when the owner opted in.
func (s *Server) rateLimitFiguresEntitled(ctx context.Context, token auth.Token, target routing.Target) bool {
	if target.VendorAccountID == "" {
		return true
	}
	if token.UserID != "" && token.UserID == target.VendorAccountOwnerUserID {
		return true
	}
	if target.VendorShareUsageWithMembers {
		return true
	}
	if s.Portal == nil {
		return false
	}
	principalType, principalID := "user", token.UserID
	if token.IsService() {
		principalType, principalID = "service", token.ServiceID
	}
	key := principalType + "\x00" + principalID + "\x00" + target.VendorAccountID
	now := time.Now()
	c := &s.rateLimitEntitlements
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.ok
	}
	c.mu.Unlock()

	lookupCtx, cancel := context.WithTimeout(ctx, rateLimitEntitlementTimeout)
	defer cancel()
	ok, err := s.Portal.VendorAccountUsageManager(lookupCtx, token, target.VendorAccountID)
	ttl := rateLimitEntitlementTTL
	if err != nil {
		ok, ttl = false, rateLimitEntitlementErrorTTL
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]rateLimitEntitlement)
	}
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = rateLimitEntitlement{ok: ok, expires: now.Add(ttl)}
	c.mu.Unlock()
	return ok
}
