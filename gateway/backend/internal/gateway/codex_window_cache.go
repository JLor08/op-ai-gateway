// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Codex CLI clears its usage windows on any /v1/responses response without
// x-codex-* rate-limit headers. The Codex counterpart of the Anthropic window cache
// stores a principal's last observed family windows on a second rateLimitWindowCache
// instance, so the two clients never evict each other.

const codexUsedPercentSuffix = "-used-percent"

// codexWindowFields are the per-window header suffixes (after "<family>-<window>-").
var codexWindowFields = [...]string{"used-percent", "window-minutes", "reset-at"}

// codexWindowKinds are the windows of a family.
var codexWindowKinds = [...]string{"primary", "secondary"}

// codexDefaultFamily is the family of the unsuffixed x-codex-primary-* headers.
const codexDefaultFamily = "x-codex"

// Bounds on what one observation may store: at most codexMaxFamilies families, and no
// header value longer than codexMaxValueLen bytes (such a value is treated as absent).
const (
	codexMaxFamilies = 8
	codexMaxValueLen = 256
)

// codexFamilyOf returns the family ("x-codex" or "x-codex-<id>") of a lower-case
// "…-primary-used-percent" / "…-secondary-used-percent" header name.
func codexFamilyOf(name string) (string, bool) {
	if !strings.HasPrefix(name, codexRateLimitPrefix) {
		return "", false
	}
	for _, kind := range codexWindowKinds {
		if family, found := strings.CutSuffix(name, "-"+kind+codexUsedPercentSuffix); found {
			return family, true
		}
	}
	return "", false
}

// collectCodexHeaders indexes the first value of each header by lower-case name and
// returns the families that carry a used-percent header. A value longer than
// codexMaxValueLen is treated as absent.
func collectCodexHeaders(h http.Header) (lower map[string]string, families map[string]struct{}) {
	lower = make(map[string]string, len(h))
	families = map[string]struct{}{}
	for name, vals := range h {
		if len(vals) == 0 || len(vals[0]) > codexMaxValueLen {
			continue
		}
		n := strings.ToLower(name)
		if _, dup := lower[n]; !dup {
			lower[n] = vals[0]
		}
		if fam, ok := codexFamilyOf(n); ok {
			families[fam] = struct{}{}
		}
	}
	return lower, families
}

// selectCodexFamilies picks at most codexMaxFamilies families deterministically: the
// default family first, then the others in sorted order.
func selectCodexFamilies(families map[string]struct{}) []string {
	out := make([]string, 0, len(families))
	for fam := range families {
		out = append(out, fam)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == codexDefaultFamily) != (out[j] == codexDefaultFamily) {
			return out[i] == codexDefaultFamily
		}
		return out[i] < out[j]
	})
	if len(out) > codexMaxFamilies {
		out = out[:codexMaxFamilies]
	}
	return out
}

// parseCodexWindow reads one window (base is "<family>-<kind>"). A window without a
// positive integer reset-at is not returned: it could never expire.
func parseCodexWindow(lower map[string]string, base string) (cachedRateLimitWindow, bool) {
	win := cachedRateLimitWindow{fields: make(map[string]string, len(codexWindowFields))}
	for _, f := range codexWindowFields {
		if v := lower[base+"-"+f]; v != "" {
			win.fields[f] = v
		}
	}
	secs, err := strconv.ParseInt(win.fields["reset-at"], 10, 64)
	if err != nil || secs <= 0 {
		return win, false
	}
	win.reset = time.Unix(secs, 0)
	return win, true
}

// parseCodexWindows reads the Codex family windows of h (first value of each header,
// case-insensitive), bounded as described at codexMaxFamilies. A window without a
// positive integer reset-at is skipped: it could never expire. fresh reports whether h
// carries any family used-percent header.
func parseCodexWindows(h http.Header) (windows map[string]cachedRateLimitWindow, names map[string]string, fresh bool) {
	lower, families := collectCodexHeaders(h)
	windows = make(map[string]cachedRateLimitWindow)
	names = make(map[string]string)
	for _, fam := range selectCodexFamilies(families) {
		if v := lower[fam+"-limit-name"]; v != "" {
			names[fam] = v
		}
		for _, kind := range codexWindowKinds {
			base := fam + "-" + kind
			if win, ok := parseCodexWindow(lower, base); ok {
				windows[base] = win
			}
		}
	}
	return windows, names, len(families) > 0
}

// ObserveCodex stores the Codex windows of h as the principal's latest observation.
// A response without any family used-percent header is ignored, so a credits-only or
// headerless response never overwrites the cache.
func (c *rateLimitWindowCache) ObserveCodex(key, accountID string, h http.Header, now time.Time) {
	windows, names, fresh := parseCodexWindows(h)
	if fresh {
		c.observeCodexParsed(key, accountID, windows, names, now)
	}
}

// observeCodexParsed stores already parsed windows as the principal's observation.
func (c *rateLimitWindowCache) observeCodexParsed(key, accountID string, windows map[string]cachedRateLimitWindow, names map[string]string, now time.Time) {
	c.put(key, cachedRateLimitEntry{accountID: accountID, observed: now, windows: windows, names: names}, now)
}

// writeCodexWindow writes the fields of one window into dst and reports whether any.
func writeCodexWindow(dst http.Header, base string, win cachedRateLimitWindow) bool {
	wrote := false
	for _, f := range codexWindowFields {
		if v, set := win.fields[f]; set {
			dst.Set(base+"-"+f, v)
			wrote = true
		}
	}
	return wrote
}

// codexFamilyOfBase returns the family of a "<family>-<kind>" window base.
func codexFamilyOfBase(base string) (string, bool) {
	for _, kind := range codexWindowKinds {
		if fam, found := strings.CutSuffix(base, "-"+kind); found {
			return fam, true
		}
	}
	return "", false
}

// writeCodexLimitNames writes the limit name of each family in families into dst.
func writeCodexLimitNames(dst http.Header, names map[string]string, families map[string]struct{}) bool {
	wrote := false
	for fam := range families {
		if name, set := names[fam]; set {
			dst.Set(fam+"-limit-name", name)
			wrote = true
		}
	}
	return wrote
}

// AttachCodex writes the principal's still-open Codex windows, and the limit name of
// each family with at least one of them, into dst. Nothing else is written (no
// credits, 429-only or retry headers). It reports whether anything was written.
func (c *rateLimitWindowCache) AttachCodex(key string, dst http.Header, now time.Time) bool {
	entry, ok := c.liveEntry(key, now)
	if !ok {
		return false
	}
	wrote := false
	families := map[string]struct{}{}
	for base, win := range entry.windows {
		if !win.reset.After(now) {
			continue
		}
		if writeCodexWindow(dst, base, win) {
			wrote = true
		}
		if fam, found := codexFamilyOfBase(base); found {
			families[fam] = struct{}{}
		}
	}
	if writeCodexLimitNames(dst, entry.names, families) {
		wrote = true
	}
	return wrote
}

// observeCodexRateLimitWindows records the Codex windows of a vendor-OpenAI response
// for an entitled caller; an unentitled caller records nothing and loses any entry
// cached from that account, as on Messages. A token without the send_usage_limits
// opt-in (#420) records nothing, yet still purges its principal's entry from that
// account once the principal is no longer entitled; with no such entry it looks
// nothing up.
func (s *Server) observeCodexRateLimitWindows(token auth.Token, target routing.Target, h http.Header) {
	if s.codexRateLimitWindows == nil || !routing.IsOpenAIVendorProvider(target.Provider) {
		return
	}
	windows, names, fresh := parseCodexWindows(h)
	if !fresh {
		return
	}
	key, ok := principalKey(token)
	if !ok {
		return
	}
	if !token.SendUsageLimits {
		if s.codexRateLimitWindows.hasEntryFrom(key, target.VendorAccountID) && !s.rateLimitFiguresEntitled(context.Background(), token, target) {
			s.codexRateLimitWindows.Forget(key, target.VendorAccountID)
		}
		return
	}
	if !s.rateLimitFiguresEntitled(context.Background(), token, target) {
		s.codexRateLimitWindows.Forget(key, target.VendorAccountID)
		return
	}
	s.codexRateLimitWindows.observeCodexParsed(key, target.VendorAccountID, windows, names, time.Now())
}

// attachCodexRateLimitWindows writes the caller's last observed Codex windows into
// dst, the header set of a /v1/responses success that carries none of its own;
// target is the target that served it. As on Messages (attachRateLimitWindows), a
// target from a vendor group gets nothing and drops the principal's entry. It
// writes nothing for a token without send_usage_limits (#420).
func (s *Server) attachCodexRateLimitWindows(token auth.Token, target routing.Target, dst http.Header) {
	if !token.SendUsageLimits || s.codexRateLimitWindows == nil {
		return
	}
	key, ok := principalKey(token)
	if !ok {
		return
	}
	if target.VendorGroupID != "" {
		s.codexRateLimitWindows.Drop(key)
		return
	}
	s.codexRateLimitWindows.AttachCodex(key, dst, time.Now())
}
