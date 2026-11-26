// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A request routed through a vendor account GROUP (a target with a VendorGroupID)
// can find its member out of a limit: the vendor answers 429 or 402
// (provider.IsLimitStatus) before any byte of the response reached the client. The
// gateway then moves the request to the group's next eligible member, and the
// client never sees the refusal:
//
//  1. the refusal's headers are scraped into the member's usage snapshot, as for any
//     served response;
//  2. the member is cooled down for the requested upstream model
//     (vendorLimitCooldownUntil), so the requests that follow skip it too. The
//     registry also keeps the cooldown's public end (vendorLimitPublicCooldownUntil:
//     Retry-After or the default, never a scraped window reset), which is all a
//     group owner who may not see that account's usage is shown;
//  3. the request is resolved again with the member excluded
//     (withExcludedVendorAccount), by the resolve step of the path in hand. Only a
//     target of the SAME group that is not excluded -- another member -- is taken:
//     the resolver's own-account step does not honour the exclusion, so where a
//     group's prefix shadows an own account's, a re-resolve can return the very
//     account that refused, outside the group. That, and a re-resolve that finds no
//     target, end the failover;
//  4. the request runs on that member, as a fresh attempt. A limit answer there fails
//     over again with the exclusion grown, so every member is tried at most once.
//     When no member is left, the client gets the last vendor answer: verbatim on
//     passthrough, the translate path's 502 provider.unavailable on translate.
//
// Only the attempt that answers records the request's usage row. A target that did
// not come from a group, and an answer that comes after the first byte of the
// response reached the client, never fail over.

// vendorTargetResolver is the resolve step of the path that fails over:
// resolveTranslateTarget on the translate path, resolveTarget on passthrough.
type vendorTargetResolver func(token *auth.Token, req inference.Request) (routing.Target, error)

// translateTargetResolver is the translate path's resolve step for r.
func (s *Server) translateTargetResolver(r *http.Request) vendorTargetResolver {
	return func(token *auth.Token, req inference.Request) (routing.Target, error) {
		return s.resolveTranslateTarget(r, token, req)
	}
}

// vendorLimitCooldownUntil is when a member that answered a limit with headers h
// may serve the model again: the earliest of now+retryAfter (when positive), the
// Anthropic unified reset, and the reset of every Codex window reported at 100 %
// used or more -- each only when it lies after now. With none of them known it is
// now+routing.DefaultVendorLimitCooldown.
func vendorLimitCooldownUntil(h http.Header, retryAfter time.Duration, now time.Time) time.Time {
	var until time.Time
	consider := func(t time.Time) {
		if t.After(now) && (until.IsZero() || t.Before(until)) {
			until = t
		}
	}
	if retryAfter > 0 {
		consider(now.Add(retryAfter))
	}
	hdr := lowerHeaderValues(h)
	if t, ok := parseEpochSeconds(hdr[hdrAnthropicReset]); ok {
		consider(t)
	}
	for _, w := range [...]struct{ pct, reset string }{
		{hdrCodexPrimaryPct, hdrCodexPrimaryReset},
		{hdrCodexSecondaryPct, hdrCodexSecondaryReset},
	} {
		pct, err := strconv.ParseFloat(strings.TrimSpace(hdr[w.pct]), 64)
		if err != nil || pct < 100 {
			continue
		}
		if t, ok := parseEpochSeconds(hdr[w.reset]); ok {
			consider(t)
		}
	}
	if until.IsZero() {
		return now.Add(routing.DefaultVendorLimitCooldown)
	}
	return until
}

// vendorLimitPublicCooldownUntil is the public end of the cooldown of a member that
// answered a limit with Retry-After retryAfter: now+retryAfter when positive, else
// now+routing.DefaultVendorLimitCooldown. Unlike vendorLimitCooldownUntil it never
// reads a scraped window reset, so it reveals nothing of the account's usage; the
// portal shows it for a member whose usage the viewer is not entitled to.
func vendorLimitPublicCooldownUntil(retryAfter time.Duration, now time.Time) time.Time {
	if retryAfter > 0 {
		return now.Add(retryAfter)
	}
	return now.Add(routing.DefaultVendorLimitCooldown)
}

// withExcludedVendorAccount returns req with accountID added to the vendor accounts
// its group selection must skip. The slice is copied, so the caller's request is
// untouched (as withLostInstance does for instances).
func withExcludedVendorAccount(req inference.Request, accountID string) inference.Request {
	req.ExcludeVendorAccountIDs = append(slices.Clone(req.ExcludeVendorAccountIDs), accountID)
	return req
}

// vendorGroupFailover runs steps 1-3 above for lost, a group member whose vendor
// answered req with a limit (h its headers, retryAfter its parsed Retry-After), and
// reports whether the request moves on: to the returned target, with the returned
// request (lost excluded). resolve is the path's resolve step; it may refresh *token
// as resolveTarget does. A target without a group returns false at once, with no
// scrape and no cooldown: a single account behaves as it always has. On false the
// caller answers with the vendor's own answer.
func (s *Server) vendorGroupFailover(token *auth.Token, req inference.Request, lost routing.Target, h http.Header, retryAfter time.Duration, resolve vendorTargetResolver) (inference.Request, routing.Target, bool) {
	if lost.VendorGroupID == "" {
		return req, routing.Target{}, false
	}
	s.scrapeVendorAccountUsage(lost, h)
	now := time.Now()
	until := vendorLimitCooldownUntil(h, retryAfter, now)
	s.vendorCooldowns.Set(lost.VendorAccountID, lost.ProviderModel, until, vendorLimitPublicCooldownUntil(retryAfter, now))
	next := withExcludedVendorAccount(req, lost.VendorAccountID)
	target, err := resolve(token, next)
	if err != nil || target.VendorGroupID != lost.VendorGroupID || slices.Contains(next.ExcludeVendorAccountIDs, target.VendorAccountID) {
		slog.Info("vendor group: member answered a limit and no other member can take the request", "group", lost.VendorGroupID, "account", lost.VendorAccountID, "model", req.Model, "cooldown_until", until, "err", err)
		return req, routing.Target{}, false
	}
	slog.Info("vendor group: member answered a limit; failing over", "group", lost.VendorGroupID, "lost", lost.VendorAccountID, "account", target.VendorAccountID, "model", req.Model, "cooldown_until", until)
	return next, target, true
}
