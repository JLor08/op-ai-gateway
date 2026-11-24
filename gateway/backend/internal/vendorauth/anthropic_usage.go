// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// This file holds the active usage fetch for a Claude (Pro / Max) subscription:
// one GET AnthropicOAuthUsageURL that asks how much of the account's five-hour and
// weekly windows is used and when each resets. It is the pull counterpart of the
// passive anthropic-ratelimit-* header scrape the gateway runs on every served
// request, and its answer is the same OpenAISubscriptionUsage snapshot the ChatGPT
// pull returns (the type is the shared usage snapshot; the credit and spend fields
// stay unknown here).
//
// Unlike the ChatGPT fetch this one has an error return: the group failover that
// calls it must tell a rate-limited endpoint (back off) from any other miss, so a
// 429 is the typed ErrUsageRateLimited. Every other failure (non-2xx, redirect,
// transport error, a body with no readable window) is a plain error whose text
// carries no token and no vendor text.

// ErrUsageRateLimited is returned by a usage fetch the vendor answered with HTTP
// 429. The usage endpoint is undocumented and throttles hard, so the caller backs
// off exponentially rather than retrying.
var ErrUsageRateLimited = errors.New("vendorauth: usage endpoint rate limited")

// maxUsageResponseBytes bounds the usage body read; a real answer is a few hundred
// bytes.
const maxUsageResponseBytes = 64 << 10

// FetchAnthropicSubscriptionUsage reads the usage of a Claude subscription with
// GET AnthropicOAuthUsageURL and the bearer accessToken. The body is mapped as
// five_hour -> the five-hour window and seven_day -> the weekly window, each by
// utilization (0..100, clamped) and resets_at (RFC 3339). A window that is null or
// absent (a weekly-only plan has no five_hour) stays unknown: -1 and nil. A body
// from which neither window could be read is an error, so the caller never stores
// an empty snapshot as a success.
//
// REVERSE-ENGINEERED / VERIFY-LIVE.
func FetchAnthropicSubscriptionUsage(ctx context.Context, httpClient *http.Client, accessToken string) (OpenAISubscriptionUsage, error) {
	unknown := unknownOpenAISubscriptionUsage()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, AnthropicOAuthUsageURL, nil)
	if err != nil {
		return unknown, errors.New("vendorauth: build usage request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", authBearerPrefix+accessToken)
	req.Header.Set("anthropic-beta", AnthropicBeta)
	req.Header.Set("User-Agent", AnthropicUsageUserAgent)
	status, body, err := sendCapped(withoutRedirects(httpClient), req, maxUsageResponseBytes)
	if err != nil {
		// Transport error text may quote the request; drop it.
		return unknown, errors.New("vendorauth: usage request failed")
	}
	if status == http.StatusTooManyRequests {
		return unknown, ErrUsageRateLimited
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return unknown, fmt.Errorf("vendorauth: usage endpoint answered HTTP %d", status)
	}
	usage, known := parseAnthropicSubscriptionUsage(body)
	if !known {
		return unknown, errors.New("vendorauth: usage answer carried no readable window")
	}
	return usage, nil
}

// anthropicUsageWindow is one window of the usage body. The fields stay raw so a
// value of the wrong type reads as unknown instead of a fabricated zero.
type anthropicUsageWindow struct {
	Utilization json.RawMessage `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

// parseAnthropicSubscriptionUsage reads the usage body and reports whether at least
// one window field could be read. Decode errors are deliberately ignored, as in
// parseOpenAISubscriptionUsage: whatever could not be decoded reads as unknown.
func parseAnthropicSubscriptionUsage(body []byte) (OpenAISubscriptionUsage, bool) {
	var env struct {
		FiveHour *anthropicUsageWindow `json:"five_hour"`
		SevenDay *anthropicUsageWindow `json:"seven_day"`
	}
	_ = json.Unmarshal(body, &env)

	usage := unknownOpenAISubscriptionUsage()
	usage.FiveHourPct, usage.FiveHourResetAt = readAnthropicUsageWindow(env.FiveHour)
	usage.WeeklyPct, usage.WeeklyResetAt = readAnthropicUsageWindow(env.SevenDay)
	known := usage.FiveHourPct != unknownUsagePct || usage.FiveHourResetAt != nil ||
		usage.WeeklyPct != unknownUsagePct || usage.WeeklyResetAt != nil
	return usage, known
}

func readAnthropicUsageWindow(w *anthropicUsageWindow) (float64, *time.Time) {
	if w == nil {
		return unknownUsagePct, nil
	}
	return readUsagePct(w.Utilization), readRFC3339Time(w.ResetsAt)
}

// readRFC3339Time reads an RFC 3339 timestamp (fractional seconds allowed) as UTC:
// nil (unknown) for an absent, null, wrong-typed or unparsable value.
func readRFC3339Time(raw json.RawMessage) *time.Time {
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Unix() <= 0 || t.Unix() > maxEpochSeconds {
		return nil
	}
	t = t.UTC()
	return &t
}
