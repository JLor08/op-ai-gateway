// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

var (
	codexTestNow   = time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	codexTestReset = time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
)

func codexTokenReading(used, limitCap float64) LimitReading {
	return LimitReading{Kind: LimitKindTokenQuota, Used: used, Cap: limitCap, Period: "day", Reset: codexTestReset}
}

func TestCodexLimitStandingHeaders(t *testing.T) {
	h := http.Header{}
	writeCodexLimitStanding(h, codexTokenReading(620, 1000))
	want := map[string]string{
		"X-Codex-Primary-Used-Percent":   "62",
		"X-Codex-Primary-Window-Minutes": "1440",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(codexTestReset.Unix(), 10),
		"X-Codex-Limit-Name":             "OP AI Gateway token quota",
		"X-Codex-Credits-Has-Credits":    "true",
		"X-Codex-Credits-Unlimited":      "false",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-Codex-Secondary-Used-Percent", "X-Codex-Credits-Balance", "X-Codex-Active-Limit", "X-Codex-Promo-Message"} {
		if _, ok := h[k]; ok {
			t.Errorf("%s must be absent", k)
		}
	}
}

func TestCodexWindowMinutesPerPeriod(t *testing.T) {
	for period, want := range map[string]string{"hour": "60", "day": "1440", "week": "10080", "month": "43200", "": "", "year": ""} {
		if got := codexWindowMinutes(period); got != want {
			t.Errorf("codexWindowMinutes(%q) = %q, want %q", period, got, want)
		}
	}
	h := http.Header{}
	r := codexTokenReading(1, 10)
	r.Period = "year"
	writeCodexLimitStanding(h, r)
	if _, ok := h["X-Codex-Primary-Window-Minutes"]; ok {
		t.Error("unknown period must write no window-minutes header")
	}
}

func TestCodexLimitStandingClampsBelow100(t *testing.T) {
	for _, c := range []struct {
		used, limitCap float64
		want           string
	}{{1000, 1000, "99"}, {1500, 1000, "99"}, {0, 1000, "0"}, {29, 100, "29"}} {
		h := http.Header{}
		writeCodexLimitStanding(h, codexTokenReading(c.used, c.limitCap))
		if got := h.Get("X-Codex-Primary-Used-Percent"); got != c.want {
			t.Errorf("%v/%v: used percent = %q, want %q", c.used, c.limitCap, got, c.want)
		}
	}
}

func TestCodexLimitNamesAndPromo(t *testing.T) {
	cases := []struct {
		kind        LimitKind
		name, promo string
	}{
		{LimitKindRequestQuota, "OP AI Gateway request quota", "OP AI Gateway request quota reached; ask your gateway administrator to raise it"},
		{LimitKindTokenQuota, "OP AI Gateway token quota", "OP AI Gateway token quota reached; ask your gateway administrator to raise it"},
		{LimitKindCostBudget, "OP AI Gateway cost budget", "OP AI Gateway cost budget reached; ask your gateway administrator to raise it"},
	}
	for _, c := range cases {
		if got := codexLimitName(c.kind); got != c.name {
			t.Errorf("name = %q, want %q", got, c.name)
		}
		got := codexPromoMessage(c.kind)
		if got != c.promo {
			t.Errorf("promo = %q, want %q", got, c.promo)
		}
		for i := 0; i < len(got); i++ {
			if got[i] >= 0x80 {
				t.Errorf("promo %q has a non-ASCII byte at %d", got, i)
			}
		}
	}
}

func TestAttachCodexPrincipalLimitStanding(t *testing.T) {
	standing := func(rs ...LimitReading) context.Context {
		return withLimitStanding(context.Background(), LimitStanding(rs))
	}
	rawLower := http.Header{"x-codex-primary-used-percent": []string{"42.5"}}
	balance := http.Header{"X-Codex-Credits-Balance": []string{"7"}}
	past := codexTokenReading(1, 10)
	past.Reset = codexTestNow.Add(-time.Minute)
	atNow := codexTokenReading(1, 10)
	atNow.Reset = codexTestNow
	failed := codexTokenReading(1, 10)
	failed.Failed = true

	t.Run("writes", func(t *testing.T) {
		h := http.Header{}
		attachCodexPrincipalLimitStanding(standing(codexTokenReading(620, 1000)), h, codexTestNow)
		if got := h.Get("X-Codex-Primary-Used-Percent"); got != "62" {
			t.Errorf("used percent = %q, want 62", got)
		}
	})

	noops := []struct {
		name string
		ctx  context.Context
		dst  http.Header
	}{
		{"no standing", context.Background(), http.Header{}},
		{"failed reading", standing(codexTokenReading(620, 1000), failed), http.Header{}},
		{"raw lower-case primary", standing(codexTokenReading(620, 1000)), rawLower},
		{"credits balance only", standing(codexTokenReading(620, 1000)), balance},
		{"reset equals now", standing(atNow), http.Header{}},
		{"reset before now", standing(past), http.Header{}},
	}
	for _, c := range noops {
		t.Run(c.name, func(t *testing.T) {
			before := c.dst.Clone()
			attachCodexPrincipalLimitStanding(c.ctx, c.dst, codexTestNow)
			if !reflect.DeepEqual(before, c.dst) {
				t.Errorf("dst changed: %v -> %v", before, c.dst)
			}
		})
	}
}

func TestWriteCodexLimitBlocked(t *testing.T) {
	r := codexTokenReading(1000, 1000)
	rec := httptest.NewRecorder()
	writeCodexLimitBlocked(rec, r, codexTestNow)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	var body codexLimitBlockedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "usage_limit_reached" || body.Error.Code != "limit.token_quota_exceeded" ||
		body.Error.Message != limitBlockedMessage(r) || body.Error.ResetsAt != codexTestReset.Unix() {
		t.Errorf("body = %+v", body)
	}
	want := map[string]string{
		"Retry-After":                    "21600",
		"X-Should-Retry":                 "false",
		"X-Codex-Promo-Message":          "OP AI Gateway token quota reached; ask your gateway administrator to raise it",
		"X-Codex-Primary-Used-Percent":   "100",
		"X-Codex-Primary-Window-Minutes": "1440",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(codexTestReset.Unix(), 10),
		"X-Codex-Credits-Has-Credits":    "true",
		"X-Codex-Credits-Unlimited":      "false",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-Codex-Limit-Name", "X-Codex-Active-Limit", "X-Codex-Rate-Limit-Reached-Type", "X-Codex-Secondary-Used-Percent"} {
		if _, ok := rec.Header()[k]; ok {
			t.Errorf("%s must be absent", k)
		}
	}

	t.Run("cost budget month", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeCodexLimitBlocked(rec, LimitReading{Kind: LimitKindCostBudget, Used: 5, Cap: 5, Period: "month", Reset: codexTestReset}, codexTestNow)
		var b codexLimitBlockedBody
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if b.Error.Code != "limit.cost_budget_exceeded" {
			t.Errorf("code = %q", b.Error.Code)
		}
		if got := rec.Header().Get("X-Codex-Primary-Window-Minutes"); got != "43200" {
			t.Errorf("window = %q", got)
		}
	})

	t.Run("reset in the past", func(t *testing.T) {
		rec := httptest.NewRecorder()
		past := r
		past.Reset = codexTestNow.Add(-time.Hour)
		writeCodexLimitBlocked(rec, past, codexTestNow)
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1", got)
		}
	})
}

func TestLimitRetryAfterSeconds(t *testing.T) {
	now := codexTestNow
	for _, tc := range []struct {
		name  string
		reset time.Time
		want  int
	}{
		{"ten seconds", now.Add(10 * time.Second), 10},
		{"rounds up", now.Add(9400 * time.Millisecond), 10},
		{"sub-second", now.Add(200 * time.Millisecond), 1},
		{"equal", now, 1},
		{"past", now.Add(-5 * time.Second), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitRetryAfterSeconds(tc.reset, now); got != tc.want {
				t.Fatalf("limitRetryAfterSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}
