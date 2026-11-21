// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
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

// codexFamilyOf returns the family ("x-codex" or "x-codex-<id>") of a lower-case
// "…-primary-used-percent" / "…-secondary-used-percent" header name.
func codexFamilyOf(name string) (string, bool) {
	if !strings.HasPrefix(name, codexRateLimitPrefix) || !strings.HasSuffix(name, codexUsedPercentSuffix) {
		return "", false
	}
	for _, kind := range codexWindowKinds {
		suffix := "-" + kind + codexUsedPercentSuffix
		if strings.HasSuffix(name, suffix) {
			family := strings.TrimSuffix(name, suffix)
			if family == strings.TrimSuffix(codexRateLimitPrefix, "-") || strings.HasPrefix(family, codexRateLimitPrefix) {
				return family, true
			}
		}
	}
	return "", false
}

// parseCodexWindows reads the Codex family windows of h (first value of each header,
// case-insensitive). A window without a positive integer reset-at is skipped: it could
// never expire. fresh reports whether h carries any family used-percent header.
func parseCodexWindows(h http.Header) (windows map[string]cachedRateLimitWindow, names map[string]string, fresh bool) {
	lower := make(map[string]string, len(h))
	families := map[string]struct{}{}
	for name, vals := range h {
		n := strings.ToLower(name)
		if len(vals) > 0 {
			if _, dup := lower[n]; !dup {
				lower[n] = vals[0]
			}
		}
		if fam, ok := codexFamilyOf(n); ok {
			families[fam] = struct{}{}
			fresh = true
		}
	}
	windows = make(map[string]cachedRateLimitWindow)
	names = make(map[string]string)
	for fam := range families {
		if v := lower[fam+"-limit-name"]; v != "" {
			names[fam] = v
		}
		for _, kind := range codexWindowKinds {
			base := fam + "-" + kind
			win := cachedRateLimitWindow{fields: make(map[string]string, len(codexWindowFields))}
			for _, f := range codexWindowFields {
				if v := lower[base+"-"+f]; v != "" {
					win.fields[f] = v
				}
			}
			secs, err := strconv.ParseInt(win.fields["reset-at"], 10, 64)
			if err != nil || secs <= 0 {
				continue
			}
			win.reset = time.Unix(secs, 0)
			windows[base] = win
		}
	}
	return windows, names, fresh
}

// ObserveCodex stores the Codex windows of h as the principal's latest observation.
// A response without any family used-percent header is ignored, so a credits-only or
// headerless response never overwrites the cache.
func (c *rateLimitWindowCache) ObserveCodex(key, accountID string, h http.Header, now time.Time) {
	windows, names, fresh := parseCodexWindows(h)
	if !fresh {
		return
	}
	c.put(key, cachedRateLimitEntry{accountID: accountID, observed: now, windows: windows, names: names}, now)
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
		for _, f := range codexWindowFields {
			if v, set := win.fields[f]; set {
				dst.Set(base+"-"+f, v)
				wrote = true
			}
		}
		for _, kind := range codexWindowKinds {
			if fam, found := strings.CutSuffix(base, "-"+kind); found {
				families[fam] = struct{}{}
			}
		}
	}
	for fam := range families {
		if name, set := entry.names[fam]; set {
			dst.Set(fam+"-limit-name", name)
			wrote = true
		}
	}
	return wrote
}

// observeCodexRateLimitWindows records the Codex windows of a vendor-OpenAI response
// for an entitled caller; an unentitled caller records nothing and loses any entry
// cached from that account, as on Messages.
func (s *Server) observeCodexRateLimitWindows(token auth.Token, target routing.Target, h http.Header) {
	if s.codexRateLimitWindows == nil || !routing.IsOpenAIVendorProvider(target.Provider) {
		return
	}
	if _, _, fresh := parseCodexWindows(h); !fresh {
		return
	}
	key, ok := principalKey(token)
	if !ok {
		return
	}
	if !s.rateLimitFiguresEntitled(context.Background(), token, target) {
		s.codexRateLimitWindows.Forget(key, target.VendorAccountID)
		return
	}
	s.codexRateLimitWindows.ObserveCodex(key, target.VendorAccountID, h, time.Now())
}

// attachCodexRateLimitWindows writes the caller's last observed Codex windows into
// dst, the header set of a /v1/responses success that carries none of its own.
func (s *Server) attachCodexRateLimitWindows(token auth.Token, dst http.Header) {
	if s.codexRateLimitWindows == nil {
		return
	}
	if key, ok := principalKey(token); ok {
		s.codexRateLimitWindows.AttachCodex(key, dst, time.Now())
	}
}
