// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestWriteLimitBlockedDailyTokenQuota(t *testing.T) {
	reset := utc(2026, 11, 22, 0, 0, 0)
	now := utc(2026, 11, 21, 13, 30, 0)
	rec := httptest.NewRecorder()
	writeLimitBlocked(rec, LimitReading{Kind: LimitKindTokenQuota, Used: 1000, Cap: 1000, Period: "day", Reset: reset}, now)

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
		writeLimitBlocked(rec, LimitReading{Kind: tc.kind, Used: 1, Cap: 1, Period: tc.period, Reset: reset}, now)
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
	writeLimitBlocked(rec, LimitReading{Kind: LimitKindRequestQuota, Used: 5, Cap: 5, Period: "hour", Reset: reset}, now)
	if _, ok := rec.Header()[http.CanonicalHeaderKey(unifiedRateLimitPrefix+"overage-period")]; ok {
		t.Errorf("overage-period must be absent for the hour")
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}
