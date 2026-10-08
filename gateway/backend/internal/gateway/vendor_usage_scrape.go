// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"time"
)

// Vendor rate-limit response-header names (lowercased), REVERSE-ENGINEERED from
// the Claude subscription and Codex CLI traffic -- VERIFY-LIVE, they may be
// renamed or rotated by the vendor at any time. Matched case-insensitively
// against the upstream response headers (Go canonicalizes header keys, so the
// lookup is tolerant of any casing the vendor sends).
//
// Anthropic reports a 0..1 UTILIZATION FRACTION for a five-hour ("unified-5h")
// and a seven-day ("unified-7d") window plus a single unix-epoch reset; OpenAI/
// Codex reports a 0..100 USED-PERCENT for a "primary" (five-hour) and a
// "secondary" (weekly) window, each with its own unix-epoch reset, plus a raw
// credit-balance string. Both are normalized onto VendorAccountUsage's 0..100
// percent scale (-1 = unknown) and five-hour/weekly slots.
const (
	hdrAnthropicFiveHourUtil = "anthropic-ratelimit-unified-5h-utilization"
	hdrAnthropicWeeklyUtil   = "anthropic-ratelimit-unified-7d-utilization"
	hdrAnthropicReset        = "anthropic-ratelimit-unified-reset"

	hdrCodexPrimaryPct     = "x-codex-primary-used-percent"
	hdrCodexPrimaryReset   = "x-codex-primary-reset-at"
	hdrCodexSecondaryPct   = "x-codex-secondary-used-percent"
	hdrCodexSecondaryReset = "x-codex-secondary-reset-at"
	hdrCodexCredits        = "x-codex-credits-balance"
)

// scrapeVendorAccountUsage parses the vendor rate-limit headers off a served
// vendor-account response and upserts the per-account usage snapshot. Entirely
// BEST-EFFORT: it runs only for a vendor target (VendorAccountID != ""), never
// upserts an all-unknown snapshot (so a response that carries NONE of the
// recognized headers leaves the previous snapshot intact), and swallows every
// parse/store failure with a Debug log -- it must NEVER fault the inference
// request (the client already has its answer by the time recordUsage runs). It
// logs only the account id, never a header value or a token.
func (s *Server) scrapeVendorAccountUsage(target routing.Target, h http.Header) {
	if s.Routes == nil || target.VendorAccountID == "" || len(h) == 0 {
		return
	}
	snapshot, ok := parseVendorAccountUsage(target.Provider, target.VendorAccountID, h, time.Now().UTC())
	if !ok {
		// No recognizable rate-limit headers (e.g. a platform OpenAI api-key
		// response, which carries no x-codex-* headers). Do not overwrite a good
		// snapshot with all-unknowns.
		return
	}
	if err := s.Routes.UpsertVendorAccountUsage(context.Background(), snapshot); err != nil {
		slog.Debug("vendor account usage upsert failed", "account", target.VendorAccountID, "err", err)
	}
}

// parseVendorAccountUsage builds a VendorAccountUsage from the upstream response
// headers for the given provider. It is TOLERANT: unknown/absent/garbage values
// are left at the snapshot's unknown defaults (-1 percent, nil reset, "" credit),
// NEVER a fabricated 0. ok is false when NONE of the provider's recognized
// headers were present (so the caller skips the upsert) or the provider is not a
// vendor one.
func parseVendorAccountUsage(provider, accountID string, h http.Header, now time.Time) (routing.VendorAccountUsage, bool) {
	hdr := lowerHeaderValues(h)
	snapshot := routing.VendorAccountUsage{
		AccountID:   accountID,
		FiveHourPct: -1, // unknown until a header says otherwise
		WeeklyPct:   -1,
		UpdatedAt:   now,
	}
	// Normalize the OpenAI subscription provider to the OpenAI vendor case: both
	// scrape the same Codex rate-limit headers off the ChatGPT backend's response.
	if routing.IsOpenAIVendorProvider(provider) {
		provider = routing.ProviderVendorOpenAI
	}
	var found bool
	switch provider {
	case routing.ProviderVendorAnthropic:
		found = scrapeAnthropicUsage(hdr, &snapshot)
	case routing.ProviderVendorOpenAI:
		found = scrapeCodexUsage(hdr, &snapshot)
	default:
		return routing.VendorAccountUsage{}, false
	}
	return snapshot, found
}

// scrapeAnthropicUsage fills the Anthropic rate-limit fields from the (lower-cased)
// response headers and reports whether any were present. Anthropic utilization is a
// 0..1 fraction, scaled to percent here.
func scrapeAnthropicUsage(hdr map[string]string, snapshot *routing.VendorAccountUsage) bool {
	found := false
	if pct, ok := parseScaledPercent(hdr[hdrAnthropicFiveHourUtil], 100); ok {
		snapshot.FiveHourPct = pct
		found = true
	}
	if pct, ok := parseScaledPercent(hdr[hdrAnthropicWeeklyUtil], 100); ok {
		snapshot.WeeklyPct = pct
		found = true
	}
	// A single unified reset; attribute it to the five-hour window (the weekly
	// reset is not advertised as its own header -- VERIFY-LIVE).
	if ts, ok := parseEpochSeconds(hdr[hdrAnthropicReset]); ok {
		snapshot.FiveHourResetAt = &ts
		found = true
	}
	return found
}

// scrapeCodexUsage fills the Codex rate-limit fields from the (lower-cased) response
// headers and reports whether any were present. Codex used-percent is already a
// 0..100 value.
func scrapeCodexUsage(hdr map[string]string, snapshot *routing.VendorAccountUsage) bool {
	found := false
	if pct, ok := parseScaledPercent(hdr[hdrCodexPrimaryPct], 1); ok {
		snapshot.FiveHourPct = pct
		found = true
	}
	if ts, ok := parseEpochSeconds(hdr[hdrCodexPrimaryReset]); ok {
		snapshot.FiveHourResetAt = &ts
		found = true
	}
	if pct, ok := parseScaledPercent(hdr[hdrCodexSecondaryPct], 1); ok {
		snapshot.WeeklyPct = pct
		found = true
	}
	if ts, ok := parseEpochSeconds(hdr[hdrCodexSecondaryReset]); ok {
		snapshot.WeeklyResetAt = &ts
		found = true
	}
	if cb := strings.TrimSpace(hdr[hdrCodexCredits]); cb != "" {
		snapshot.CreditBalance = cb
		found = true
	}
	return found
}

// lowerHeaderValues flattens h into a lowercased-key -> first-value map, so the
// vendor header lookups are case/canonicalization tolerant.
func lowerHeaderValues(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		out[strings.ToLower(k)] = v[0]
	}
	return out
}

// parseScaledPercent parses a numeric header value and multiplies it by scale
// (100 for an Anthropic 0..1 fraction, 1 for a Codex 0..100 percent). ok is false
// for a missing or non-numeric value. A negative or NaN result is rejected (-1 is
// the reserved unknown sentinel, so a real reading must never be negative, and a
// NaN would make the usage DTO unmarshalable as JSON), and the
// scaled result is clamped to <=100 so a malformed over-range value (e.g. an
// Anthropic fraction >1 scaling past 100) cannot report an impossible percent.
func parseScaledPercent(s string, scale float64) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsNaN(f) {
		return 0, false
	}
	pct := f * scale
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// parseEpochSeconds parses a unix-epoch-seconds header value (integer or
// fractional) into a UTC time. ok is false for a missing, non-numeric or
// non-positive value.
func parseEpochSeconds(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec).UTC(), true
}
