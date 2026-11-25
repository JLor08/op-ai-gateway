// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

const anthropicUsageBody = `{
  "five_hour":  {"utilization": 37.5, "resets_at": "2026-11-24T15:00:00.123456+00:00"},
  "seven_day":  {"utilization": 81,   "resets_at": "2026-11-28T09:30:00Z"},
  "seven_day_opus": null,
  "extra_usage": {"is_enabled": false}
}`

func TestFetchAnthropicSubscriptionUsageParsesBothWindows(t *testing.T) {
	client, rec := newProbeClient(t, respondWith(http.StatusOK, anthropicUsageBody))

	got, err := FetchAnthropicSubscriptionUsage(context.Background(), client, probeSecret)
	if err != nil {
		t.Fatalf("FetchAnthropicSubscriptionUsage: %v", err)
	}
	if got.FiveHourPct != 37.5 || got.WeeklyPct != 81 {
		t.Fatalf("pcts = %v / %v, want 37.5 / 81", got.FiveHourPct, got.WeeklyPct)
	}
	wantFive := time.Date(2026, 11, 24, 15, 0, 0, 123456000, time.UTC)
	if got.FiveHourResetAt == nil || !got.FiveHourResetAt.Equal(wantFive) {
		t.Fatalf("five-hour reset = %v, want %v", got.FiveHourResetAt, wantFive)
	}
	wantWeek := time.Date(2026, 11, 28, 9, 30, 0, 0, time.UTC)
	if got.WeeklyResetAt == nil || !got.WeeklyResetAt.Equal(wantWeek) {
		t.Fatalf("weekly reset = %v, want %v", got.WeeklyResetAt, wantWeek)
	}
	if got.SpendUsedPct != -1 {
		t.Fatalf("SpendUsedPct = %v, want unknown", got.SpendUsedPct)
	}

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodGet || r.Host != "api.anthropic.com" || r.Path != "/api/oauth/usage" || len(r.Query) != 0 {
		t.Fatalf("request = %s %s%s ?%v, want GET api.anthropic.com/api/oauth/usage", r.Method, r.Host, r.Path, r.Query)
	}
	if h := r.Header.Get("Authorization"); h != "Bearer "+probeSecret {
		t.Fatalf("Authorization = %q", h)
	}
	if h := r.Header.Get("anthropic-beta"); h != AnthropicBeta {
		t.Fatalf("anthropic-beta = %q, want %q", h, AnthropicBeta)
	}
	if h := r.Header.Get("User-Agent"); h != AnthropicUsageUserAgent {
		t.Fatalf("User-Agent = %q, want %q", h, AnthropicUsageUserAgent)
	}
}

func TestFetchAnthropicSubscriptionUsageNullFiveHourStaysUnknown(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusOK, `{"five_hour": null, "seven_day": {"utilization": 12, "resets_at": "2026-11-28T09:30:00Z"}}`))

	got, err := FetchAnthropicSubscriptionUsage(context.Background(), client, probeSecret)
	if err != nil {
		t.Fatalf("FetchAnthropicSubscriptionUsage: %v", err)
	}
	if got.FiveHourPct != -1 || got.FiveHourResetAt != nil {
		t.Fatalf("five-hour = %v / %v, want unknown (-1, nil)", got.FiveHourPct, got.FiveHourResetAt)
	}
	if got.WeeklyPct != 12 || got.WeeklyResetAt == nil {
		t.Fatalf("weekly = %v / %v, want 12 and a reset", got.WeeklyPct, got.WeeklyResetAt)
	}
}

func TestFetchAnthropicSubscriptionUsageRateLimited(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error"}}`))

	_, err := FetchAnthropicSubscriptionUsage(context.Background(), client, probeSecret)
	if !errors.Is(err, ErrUsageRateLimited) {
		t.Fatalf("err = %v, want ErrUsageRateLimited", err)
	}
}

func TestFetchAnthropicSubscriptionUsageUnusableAnswers(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"401":            respondWith(http.StatusUnauthorized, `{}`),
		"500":            respondWith(http.StatusInternalServerError, `{}`),
		"not json":       respondWith(http.StatusOK, `<html>`),
		"no windows":     respondWith(http.StatusOK, `{"five_hour": null, "seven_day": null}`),
		"wrong typed":    respondWith(http.StatusOK, `{"five_hour": {"utilization": "high", "resets_at": 5}}`),
		"bad reset time": respondWith(http.StatusOK, `{"five_hour": {"resets_at": "tomorrow"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := newProbeClient(t, h)
			_, err := FetchAnthropicSubscriptionUsage(context.Background(), client, probeSecret)
			if err == nil || errors.Is(err, ErrUsageRateLimited) {
				t.Fatalf("err = %v, want a non-rate-limit error", err)
			}
		})
	}
}
