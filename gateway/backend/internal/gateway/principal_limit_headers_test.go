// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestWriteLimitBlockedDailyTokenQuota(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	now := utc(2026, 11, 21, 13, 30, 0)
	rec := httptest.NewRecorder()
	writeLimitBlocked(rec, LimitReading{Kind: LimitKindTokenQuota, Used: 1000, Cap: 1000, Period: "day", Reset: reset}, now, true)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	want := `{"type":"error","error":{"type":"billing_error","code":"limit.token_quota_exceeded","message":"token quota reached · resets Nov 22, 00:00 UTC · ask your gateway administrator to raise it"}}`
	if got := rec.Body.String(); got != want+"\n" && got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	unix := strconv.FormatInt(reset.Unix(), 10)
	for name, want := range map[string]string{
		"Retry-After":                                          "37800",
		"X-Should-Retry":                                       "false",
		unifiedRateLimitPrefix + "status":                      "rejected",
		unifiedRateLimitPrefix + "reset":                       unix,
		unifiedRateLimitPrefix + "overage-reset":               unix,
		unifiedRateLimitPrefix + "overage-utilization":         "1",
		unifiedRateLimitPrefix + "overage-surpassed-threshold": "1",
		unifiedRateLimitPrefix + "overage-period":              "daily",
		unifiedRateLimitPrefix + "overage-disabled-reason":     "org_spend_cap_reached",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"representative-claim", "overage-status"} {
		if _, ok := rec.Header()[http.CanonicalHeaderKey(unifiedRateLimitPrefix+name)]; ok {
			t.Errorf("%s must be absent", name)
		}
	}
}

func TestWriteLimitBlockedPeriods(t *testing.T) {
	now := utc(2026, 11, 21, 13, 30, 0)
	reset := utc(2026, 12, 1, 0, 0, 0)
	for _, tc := range []struct {
		kind         LimitKind
		period, want string
		code, label  string
	}{
		{LimitKindTokenQuota, "week", "weekly", "limit.token_quota_exceeded", "token quota"},
		{LimitKindCostBudget, "month", "monthly", "limit.cost_budget_exceeded", "cost budget"},
		{LimitKindRequestQuota, "day", "daily", "limit.request_quota_exceeded", "request quota"},
	} {
		rec := httptest.NewRecorder()
		writeLimitBlocked(rec, LimitReading{Kind: tc.kind, Used: 1, Cap: 1, Period: tc.period, Reset: reset}, now, true)
		if got := rec.Header().Get(unifiedRateLimitPrefix + "overage-period"); got != tc.want {
			t.Errorf("%s period = %q, want %q", tc.period, got, tc.want)
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != tc.code {
			t.Errorf("%s code = %q, want %q", tc.kind, got, tc.code)
		}
		if got, want := limitBlockedMessage(LimitReading{Kind: tc.kind, Reset: reset}), tc.label+" reached · resets Dec 1, 00:00 UTC · ask your gateway administrator to raise it"; got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
	}
}

func TestWriteLimitBlockedHourPeriod(t *testing.T) {
	reset := utc(2026, 11, 21, 14, 0, 0)
	now := utc(2026, 11, 21, 13, 59, 59).Add(400 * time.Millisecond)
	rec := httptest.NewRecorder()
	writeLimitBlocked(rec, LimitReading{Kind: LimitKindRequestQuota, Used: 5, Cap: 5, Period: "hour", Reset: reset}, now, true)
	if _, ok := rec.Header()[http.CanonicalHeaderKey(unifiedRateLimitPrefix+"overage-period")]; ok {
		t.Errorf("overage-period must be absent for the hour")
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}

func TestOverageUtilizationValueFloatEdges(t *testing.T) {
	for _, tc := range []struct {
		u    float64
		want string
	}{
		{82.0 / 100, "0.82"},
		{29.0 / 100, "0.29"},
		{57.0 / 100, "0.57"},
		{0.749, "0.74"},
		{0.75, "0.75"},
		{0.999, "0.99"},
		{1.2, "0.99"},
		{0, "0.00"},
	} {
		if got := overageUtilizationValue(tc.u); got != tc.want {
			t.Errorf("overageUtilizationValue(%v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

func overageReading(used float64, reset time.Time) LimitReading {
	return LimitReading{Kind: LimitKindTokenQuota, Used: used, Cap: 100, Period: "day", Reset: reset}
}

func TestWriteOverageStandingBelowThreshold(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	dst := http.Header{}
	writeOverageStanding(dst, overageReading(50, reset))

	unix := strconv.FormatInt(reset.Unix(), 10)
	for name, want := range map[string]string{
		"overage-utilization":  "0.50",
		"overage-status":       "allowed",
		"status":               "allowed",
		"representative-claim": "overage",
		"reset":                unix,
		"overage-reset":        unix,
	} {
		if got := dst.Get(unifiedRateLimitPrefix + name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"overage-surpassed-threshold", "overage-period"} {
		if _, ok := dst[http.CanonicalHeaderKey(unifiedRateLimitPrefix+name)]; ok {
			t.Errorf("%s must be absent", name)
		}
	}
}

func TestWriteOverageStandingThresholds(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	for _, tc := range []struct {
		used      float64
		threshold string
	}{{80, "0.75"}, {96, "0.95"}} {
		dst := http.Header{}
		writeOverageStanding(dst, overageReading(tc.used, reset))
		if got := dst.Get(unifiedRateLimitPrefix + "overage-surpassed-threshold"); got != tc.threshold {
			t.Errorf("used %v: threshold = %q, want %q", tc.used, got, tc.threshold)
		}
		for _, name := range []string{"overage-status", "status"} {
			if got := dst.Get(unifiedRateLimitPrefix + name); got != "allowed_warning" {
				t.Errorf("used %v: %s = %q, want allowed_warning", tc.used, name, got)
			}
		}
	}
}

func TestWriteOverageStandingThresholdBoundaries(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	for _, tc := range []struct {
		used             float64
		value, threshold string
	}{{75, "0.75", "0.75"}, {74.99, "0.74", ""}, {95, "0.95", "0.95"}} {
		dst := http.Header{}
		writeOverageStanding(dst, overageReading(tc.used, reset))
		if got := dst.Get(unifiedRateLimitPrefix + "overage-utilization"); got != tc.value {
			t.Errorf("used %v: utilization = %q, want %q", tc.used, got, tc.value)
		}
		if got := dst.Get(unifiedRateLimitPrefix + "overage-surpassed-threshold"); got != tc.threshold {
			t.Errorf("used %v: threshold = %q, want %q", tc.used, got, tc.threshold)
		}
	}
}

func TestStandingOverlayReplacesSubscriptionTopLevel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := utc(2026, 11, 22, 0, 0, 0)
	dst := windowHeaders(now, time.Hour, 24*time.Hour)
	dst.Set(rlp+"overage-status", "rejected")
	dst.Set(rlp+"overage-disabled-reason", "org_level_disabled")
	dst.Set(rlp+"fallback-percentage", "0.5")
	dst.Set(rlp+"upgrade-paths", "x")
	before := dst.Clone()

	ctx := withLimitStanding(context.Background(), LimitStanding{overageReading(10, reset)})
	attachPrincipalLimitStanding(ctx, dst)

	if got := dst.Get(rlp + "representative-claim"); got != "overage" {
		t.Errorf("representative-claim = %q, want overage", got)
	}
	if got := dst.Get(rlp + "overage-status"); got != "allowed" {
		t.Errorf("overage-status = %q, want allowed", got)
	}
	if got, want := dst.Get(rlp+"reset"), strconv.FormatInt(reset.Unix(), 10); got != want {
		t.Errorf("reset = %q, want %q", got, want)
	}
	for _, name := range []string{"overage-disabled-reason", "fallback-percentage", "upgrade-paths"} {
		if _, ok := dst[http.CanonicalHeaderKey(rlp+name)]; ok {
			t.Errorf("%s must be removed", name)
		}
	}
	for _, w := range rateLimitWindowNames {
		for _, f := range rateLimitWindowFields {
			name := rlp + w + "-" + f
			if got, want := dst.Values(name), before.Values(name); !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %v, want unchanged %v", name, got, want)
			}
		}
	}
	if dst.Get(rlp+"7d-surpassed-threshold") != "0.1" {
		t.Errorf("7d-surpassed-threshold not kept: %v", dst)
	}
}

func TestStandingStatusMergesSubscriptionWarning(t *testing.T) {
	dst := http.Header{}
	dst.Set(rlp+"5h-status", "allowed_warning")
	writeOverageStanding(dst, overageReading(10, utc(2026, 11, 22, 0, 0, 0)))
	if got := dst.Get(rlp + "status"); got != "allowed_warning" {
		t.Errorf("status = %q, want allowed_warning", got)
	}
	if got := dst.Get(rlp + "overage-status"); got != "allowed" {
		t.Errorf("overage-status = %q, want allowed", got)
	}
}

func TestAttachPrincipalLimitStandingNoop(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	failed := overageReading(10, reset)
	failed.Failed = true
	for name, ctx := range map[string]context.Context{
		"no standing":    context.Background(),
		"empty standing": withLimitStanding(context.Background(), LimitStanding{}),
		"failed reading": withLimitStanding(context.Background(), LimitStanding{overageReading(10, reset), failed}),
	} {
		dst := windowHeaders(time.Unix(1_800_000_000, 0), time.Hour, 24*time.Hour)
		want := dst.Clone()
		attachPrincipalLimitStanding(ctx, dst)
		if !maps.EqualFunc(dst, want, func(a, b []string) bool { return reflect.DeepEqual(a, b) }) {
			t.Errorf("%s: dst changed to %v, want %v", name, dst, want)
		}
	}
}
