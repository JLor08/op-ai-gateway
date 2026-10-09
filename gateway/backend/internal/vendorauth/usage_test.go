// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const usageAccountID = "acct-usage-7f3a"

// usageBody mirrors the reverse-engineered GET wham/usage answer: a singular
// rate_limit with primary_window (five-hour) and secondary_window (weekly), each
// {used_percent, reset_at as absolute unix seconds}, and a credits object whose
// balance is a string. The keys we do not map (plan_type, limit_window_seconds,
// reset_after_seconds, allowed, ...) ride along and must be ignored.
const usageBody = `{
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window":   {"used_percent": 23, "limit_window_seconds": 18000,  "reset_after_seconds": 12000,  "reset_at": 1760000000},
    "secondary_window": {"used_percent": 61, "limit_window_seconds": 604800, "reset_after_seconds": 300000, "reset_at": 1760500000}
  },
  "credits": {"has_credits": true, "unlimited": false, "balance": "12.34"},
  "account_id": "acc_x",
  "user_id": "usr_x"
}`

// businessUsageBody mirrors the redacted GET wham/usage answer of a Business plan:
// no rate-limit windows and no credit balance (both null), the quota under
// spend_control.individual_limit (limit/used/remaining are JSON strings,
// used_percent/remaining_percent/reset_after_seconds/reset_at are JSON numbers) and
// the credit state in the credits flags. Identifiers are placeholders and the
// figures are illustrative, not a real account's.
const businessUsageBody = `{
  "user_id": "usr_x", "account_id": "acc_x", "email": "user@example.test",
  "plan_type": "business",
  "rate_limit": null,
  "code_review_rate_limit": null,
  "additional_rate_limits": null,
  "model_usage": {"model-x": {"available": true, "available_at": null, "credits_would_enable": false}},
  "credits": {"has_credits": true, "unlimited": false, "overage_limit_reached": false, "balance": null, "approx_local_messages": null, "approx_cloud_messages": null},
  "spend_control": {
    "reached": false,
    "individual_limit": {
      "source": "group_based_spend_controls", "unit": "credit",
      "limit": "6000", "used": "42.5", "remaining": "5957.5",
      "used_percent": 1, "remaining_percent": 99,
      "reset_after_seconds": 1938953, "reset_at": 1793491200
    }
  },
  "rate_limit_reached_type": null, "promo": null,
  "rate_limit_reset_credits": {"available_count": 0, "applicable_available_count": 0}
}`

func usageTime(sec int64) *time.Time {
	t := time.Unix(sec, 0).UTC()
	return &t
}

// wantUnknownUsage is the all-unknown snapshot every Unverifiable answer carries.
func wantUnknownUsage() OpenAISubscriptionUsage {
	return OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1}
}

func fetchUsage(t *testing.T, h http.HandlerFunc) (OpenAISubscriptionUsage, DiscoveryStatus, *probeRecorder) {
	t.Helper()
	client, rec := newProbeClient(t, h)
	got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
	return got, status, rec
}

func TestOpenAIUsageConstants(t *testing.T) {
	if OpenAIUsageURL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Errorf("OpenAIUsageURL = %q", OpenAIUsageURL)
	}
	if OpenAIUsageUserAgent != "codex-cli" {
		t.Errorf("OpenAIUsageUserAgent = %q", OpenAIUsageUserAgent)
	}
}

func TestFetchOpenAISubscriptionUsageRequestShape(t *testing.T) {
	_, _, rec := fetchUsage(t, respondWith(http.StatusOK, usageBody))

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want exactly 1", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.Method)
	}
	if got.Host != "chatgpt.com" {
		t.Errorf("host = %q, want chatgpt.com", got.Host)
	}
	if got.Path != "/backend-api/wham/usage" {
		t.Errorf("path = %q, want /backend-api/wham/usage", got.Path)
	}
	if !reflect.DeepEqual(got.Query, url.Values{}) {
		t.Errorf("query = %v, want none", got.Query)
	}
	if got.Body != "" {
		t.Errorf("body = %q, want none", got.Body)
	}
	for name, want := range map[string]string{
		"Authorization":      "Bearer " + probeSecret,
		"Chatgpt-Account-Id": usageAccountID,
		"User-Agent":         "codex-cli",
		"Accept":             "application/json",
	} {
		if v := got.Header.Get(name); v != want {
			t.Errorf("header %s = %q, want %q", name, v, want)
		}
	}
	// The usage endpoint takes no originator and no version header.
	for _, name := range []string{"Originator", "X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "Version", "Openai-Beta"} {
		if v := got.Header.Get(name); v != "" {
			t.Errorf("header %s = %q, want it absent", name, v)
		}
	}
}

func TestFetchOpenAISubscriptionUsageWithoutAccountIDOmitsTheHeader(t *testing.T) {
	client, rec := newProbeClient(t, respondWith(http.StatusOK, usageBody))
	got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, "")
	if status != DiscoveryOK || got.FiveHourPct != 23 {
		t.Fatalf("got %+v / %v, want the parsed usage and ok", got, status)
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if _, present := reqs[0].Header["Chatgpt-Account-Id"]; present {
		t.Errorf("ChatGPT-Account-Id = %q, want the header omitted", reqs[0].Header.Get("Chatgpt-Account-Id"))
	}
	if v := reqs[0].Header.Get("Authorization"); v != "Bearer "+probeSecret {
		t.Errorf("Authorization = %q", v)
	}
}

func TestFetchOpenAISubscriptionUsageMapsTheRepresentativeBody(t *testing.T) {
	got, status, _ := fetchUsage(t, respondWith(http.StatusOK, usageBody))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := OpenAISubscriptionUsage{
		FiveHourPct:     23,
		FiveHourResetAt: usageTime(1760000000),
		WeeklyPct:       61,
		WeeklyResetAt:   usageTime(1760500000),
		CreditBalance:   "12.34",
		SpendUsedPct:    -1,
		CreditStatus:    "has_credits",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if got.FiveHourResetAt.Location() != time.UTC {
		t.Errorf("five-hour reset location = %v, want UTC", got.FiveHourResetAt.Location())
	}
}

func TestParseOpenAISubscriptionUsageIsTolerant(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   OpenAISubscriptionUsage
		wantOK bool
	}{
		{
			name:   "float used_percent is kept as sent",
			body:   `{"rate_limit":{"primary_window":{"used_percent":73.5,"reset_at":1760000000},"secondary_window":{"used_percent":0.25,"reset_at":1760500000}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 73.5, FiveHourResetAt: usageTime(1760000000), WeeklyPct: 0.25, WeeklyResetAt: usageTime(1760500000), SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a real zero percent is known, not unknown",
			body:   `{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"used_percent":0.0}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 0, WeeklyPct: 0, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "primary window only leaves the weekly window unknown",
			body:   `{"rate_limit":{"primary_window":{"used_percent":40,"reset_at":1760000000}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 40, FiveHourResetAt: usageTime(1760000000), WeeklyPct: -1, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "secondary window only leaves the five-hour window unknown",
			body:   `{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":9,"reset_at":1760500000}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: 9, WeeklyResetAt: usageTime(1760500000), SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a null rate_limit keeps the credit balance",
			body:   `{"rate_limit":null,"credits":{"balance":"5.00"}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "5.00", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "credits only",
			body:   `{"credits":{"has_credits":true,"unlimited":false,"balance":"0"}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "0", SpendUsedPct: -1, CreditStatus: "has_credits"},
			wantOK: true,
		},
		{
			name:   "a zero reset_at is unknown",
			body:   `{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":0},"secondary_window":{"used_percent":20,"reset_at":0}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 10, WeeklyPct: 20, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "an absent or null reset_at is unknown",
			body:   `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20,"reset_at":null}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 10, WeeklyPct: 20, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a negative or absurd reset_at is unknown",
			body:   `{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":-5},"secondary_window":{"used_percent":20,"reset_at":9e18}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 10, WeeklyPct: 20, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a fractional reset_at is truncated to the second",
			body:   `{"rate_limit":{"primary_window":{"reset_at":1760000000.75}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, FiveHourResetAt: usageTime(1760000000), WeeklyPct: -1, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a negative percent is unknown, an over-range one is clamped",
			body:   `{"rate_limit":{"primary_window":{"used_percent":-3},"secondary_window":{"used_percent":140}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: 100, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a numeric balance is coerced to its string form",
			body:   `{"credits":{"balance":12.34}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "12.34", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "an integer balance is coerced to its string form",
			body:   `{"credits":{"balance":7}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "7", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a padded balance is trimmed",
			body:   `{"credits":{"balance":"  3.10 "}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "3.10", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed field is skipped without sinking the others",
			body:   `{"rate_limit":{"primary_window":{"used_percent":"high","reset_at":1760000000},"secondary_window":{"used_percent":61}},"credits":{"balance":true}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, FiveHourResetAt: usageTime(1760000000), WeeklyPct: 61, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed section is skipped without sinking the others",
			body:   `{"rate_limit":"nope","credits":{"balance":"1.50"}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "1.50", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed window leaves the other window",
			body:   `{"rate_limit":{"primary_window":[1,2],"secondary_window":{"used_percent":61}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: 61, SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "the plural rate_limits key is not the wire shape",
			body:   `{"rate_limits":{"primary_window":{"used_percent":50}}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "an over-long balance is unknown",
			body:   `{"credits":{"balance":"` + strings.Repeat("9", maxCreditBalanceLen+1) + `"}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "a blank balance is unknown",
			body:   `{"credits":{"balance":"   "}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "a null balance is unknown",
			body:   `{"credits":{"balance":null}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOpenAISubscriptionUsage([]byte(tc.body))
			if ok != tc.wantOK {
				t.Errorf("known = %v, want %v", ok, tc.wantOK)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFetchOpenAISubscriptionUsageMapsTheBusinessBody(t *testing.T) {
	// On a Business plan the windows and the balance are null, so the quota is only
	// readable from spend_control.individual_limit and the credits flags.
	got, status, _ := fetchUsage(t, respondWith(http.StatusOK, businessUsageBody))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := OpenAISubscriptionUsage{
		FiveHourPct:    -1,
		WeeklyPct:      -1,
		SpendUnit:      "credit",
		SpendLimit:     "6000",
		SpendUsed:      "42.5",
		SpendRemaining: "5957.5",
		SpendUsedPct:   1,
		SpendResetAt:   usageTime(1793491200),
		CreditStatus:   "has_credits",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if got.SpendResetAt.Location() != time.UTC {
		t.Errorf("spend reset location = %v, want UTC", got.SpendResetAt.Location())
	}
}

func TestParseOpenAISubscriptionUsageSpendControl(t *testing.T) {
	const spendReset = 1793491200
	tests := []struct {
		name   string
		body   string
		want   OpenAISubscriptionUsage
		wantOK bool
	}{
		{
			name:   "only spend_control, no rate_limit and no credits",
			body:   `{"spend_control":{"individual_limit":{"unit":"credit","limit":"100","used":"7.25","remaining":"92.75","used_percent":7.25,"reset_at":1793491200}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUnit: "credit", SpendLimit: "100", SpendUsed: "7.25", SpendRemaining: "92.75", SpendUsedPct: 7.25, SpendResetAt: usageTime(spendReset)},
			wantOK: true,
		},
		{
			name:   "the vendor strings are kept verbatim, not float-parsed",
			body:   `{"spend_control":{"individual_limit":{"limit":"6000.000","used":"0.10","remaining":"5999.90"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendLimit: "6000.000", SpendUsed: "0.10", SpendRemaining: "5999.90", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "padded strings are trimmed",
			body:   `{"spend_control":{"individual_limit":{"limit":" 50 ","used":"\t5\n","unit":" credit "}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUnit: "credit", SpendLimit: "50", SpendUsed: "5", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a real zero used_percent is known, not unknown",
			body:   `{"spend_control":{"individual_limit":{"used_percent":0}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: 0},
			wantOK: true,
		},
		{
			name:   "a negative used_percent is unknown",
			body:   `{"spend_control":{"individual_limit":{"used_percent":-3,"used":"1"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "1", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "an over-range used_percent is clamped to 100",
			body:   `{"spend_control":{"individual_limit":{"used_percent":140}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: 100},
			wantOK: true,
		},
		{
			name:   "a reset_at alone makes the snapshot known",
			body:   `{"spend_control":{"individual_limit":{"reset_at":1793491200}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, SpendResetAt: usageTime(spendReset)},
			wantOK: true,
		},
		{
			name:   "a zero reset_at is unknown",
			body:   `{"spend_control":{"individual_limit":{"used":"1","reset_at":0}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "1", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a negative or absurd reset_at is unknown",
			body:   `{"spend_control":{"individual_limit":{"used":"1","reset_at":-5}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "1", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "an absurdly large reset_at is unknown",
			body:   `{"spend_control":{"individual_limit":{"used":"1","reset_at":9e18}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "1", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed limit (number) is dropped, the others stay",
			body:   `{"spend_control":{"individual_limit":{"limit":6000,"used":"42.5","remaining":"5957.5"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "42.5", SpendRemaining: "5957.5", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed limit (bool) is dropped, the others stay",
			body:   `{"spend_control":{"individual_limit":{"limit":true,"used":"42.5"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "42.5", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed limit (object) is dropped, the others stay",
			body:   `{"spend_control":{"individual_limit":{"limit":{"v":"6000"},"used":"42.5","used_percent":1}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: "42.5", SpendUsedPct: 1},
			wantOK: true,
		},
		{
			name:   "an over-long used string is dropped, the others stay",
			body:   `{"spend_control":{"individual_limit":{"limit":"6000","used":"` + strings.Repeat("9", maxSpendValueLen+1) + `"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendLimit: "6000", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a string at the length bound is kept",
			body:   `{"spend_control":{"individual_limit":{"used":"` + strings.Repeat("9", maxSpendValueLen) + `"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsed: strings.Repeat("9", maxSpendValueLen), SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "wrong-typed percent and reset are dropped, the strings stay",
			body:   `{"spend_control":{"individual_limit":{"limit":"6000","used_percent":"high","reset_at":"soon"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendLimit: "6000", SpendUsedPct: -1},
			wantOK: true,
		},
		{
			name:   "a wrong-typed individual_limit leaves the credits and windows",
			body:   `{"rate_limit":{"primary_window":{"used_percent":5}},"credits":{"has_credits":true},"spend_control":{"individual_limit":["x"]}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 5, WeeklyPct: -1, SpendUsedPct: -1, CreditStatus: "has_credits"},
			wantOK: true,
		},
		{
			name:   "a wrong-typed spend_control leaves the credits",
			body:   `{"spend_control":"nope","credits":{"unlimited":true}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, CreditStatus: "unlimited"},
			wantOK: true,
		},
		{
			name:   "spend_control rides alongside the windows and the balance",
			body:   `{"rate_limit":{"primary_window":{"used_percent":23,"reset_at":1760000000}},"credits":{"has_credits":true,"balance":"12.34"},"spend_control":{"individual_limit":{"limit":"10","used":"1","used_percent":10}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: 23, FiveHourResetAt: usageTime(1760000000), WeeklyPct: -1, CreditBalance: "12.34", SpendLimit: "10", SpendUsed: "1", SpendUsedPct: 10, CreditStatus: "has_credits"},
			wantOK: true,
		},
		{
			name:   "an empty or null spend_control reads nothing",
			body:   `{"spend_control":{"reached":false,"individual_limit":null}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "an individual_limit with only unmapped keys reads nothing",
			body:   `{"spend_control":{"individual_limit":{"source":"group_based_spend_controls","remaining_percent":99,"reset_after_seconds":3600}}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "an individual_limit of blank strings reads nothing",
			body:   `{"spend_control":{"individual_limit":{"unit":"  ","limit":"","used":"   ","remaining":null}}}`,
			want:   wantUnknownUsage(),
			wantOK: false,
		},
		{
			name:   "a unit alone is not enough to be known",
			body:   `{"spend_control":{"individual_limit":{"unit":"credit"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUnit: "credit", SpendUsedPct: -1},
			wantOK: false,
		},
		{
			name:   "a remaining string alone is not enough to be known",
			body:   `{"spend_control":{"individual_limit":{"remaining":"5"}}}`,
			want:   OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendRemaining: "5", SpendUsedPct: -1},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOpenAISubscriptionUsage([]byte(tc.body))
			if ok != tc.wantOK {
				t.Errorf("known = %v, want %v", ok, tc.wantOK)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseOpenAISubscriptionUsageCreditStatus(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
		wantOK     bool
	}{
		{"has_credits true", `{"credits":{"has_credits":true,"unlimited":false}}`, "has_credits", true},
		{"unlimited true", `{"credits":{"has_credits":true,"unlimited":true}}`, "unlimited", true},
		{"unlimited wins even without has_credits", `{"credits":{"has_credits":false,"unlimited":true}}`, "unlimited", true},
		{"unlimited alone", `{"credits":{"unlimited":true}}`, "unlimited", true},
		{"has_credits false is none", `{"credits":{"has_credits":false,"unlimited":false}}`, "none", true},
		{"has_credits false with the balance null is none", `{"credits":{"has_credits":false,"balance":null}}`, "none", true},
		{"has_credits false and nothing else is none", `{"credits":{"has_credits":false}}`, "none", true},
		{"has_credits true and nothing else", `{"credits":{"has_credits":true}}`, "has_credits", true},
		{"credits absent", `{"rate_limit":{"primary_window":{"used_percent":5}}}`, "", true},
		{"credits null", `{"credits":null,"rate_limit":{"primary_window":{"used_percent":5}}}`, "", true},
		{"credits an empty object", `{"credits":{},"rate_limit":{"primary_window":{"used_percent":5}}}`, "", true},
		{"credits without the flags", `{"credits":{"balance":"1.00"}}`, "", true},
		{"has_credits null and unlimited false", `{"credits":{"has_credits":null,"unlimited":false,"balance":"1.00"}}`, "", true},
		{"wrong-typed flags are unknown", `{"credits":{"has_credits":"yes","unlimited":1,"balance":"1.00"}}`, "", true},
		{"a wrong-typed unlimited does not hide has_credits", `{"credits":{"has_credits":true,"unlimited":"yes"}}`, "has_credits", true},
		{"a wrong-typed has_credits does not hide unlimited", `{"credits":{"has_credits":"yes","unlimited":true}}`, "unlimited", true},
		{"credits of the wrong type", `{"credits":"nope","rate_limit":{"primary_window":{"used_percent":5}}}`, "", true},
		{"credits an array", `{"credits":[true],"rate_limit":{"primary_window":{"used_percent":5}}}`, "", true},
		{"the flags alone make the snapshot known", `{"credits":{"has_credits":false}}`, "none", true},
		{"no flags and nothing else is not known", `{"credits":{"overage_limit_reached":false}}`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOpenAISubscriptionUsage([]byte(tc.body))
			if ok != tc.wantOK {
				t.Errorf("known = %v, want %v", ok, tc.wantOK)
			}
			if got.CreditStatus != tc.wantStatus {
				t.Errorf("CreditStatus = %q, want %q", got.CreditStatus, tc.wantStatus)
			}
		})
	}
}

func TestFetchOpenAISubscriptionUsageCreditFlagsAloneAreOK(t *testing.T) {
	// A body whose only readable fact is the credit state is a real answer: it must
	// not be dropped as Unverifiable.
	for body, want := range map[string]string{
		`{"credits":{"has_credits":true,"unlimited":true}}`: "unlimited",
		`{"credits":{"has_credits":false}}`:                 "none",
	} {
		got, status, _ := fetchUsage(t, respondWith(http.StatusOK, body))
		if status != DiscoveryOK {
			t.Errorf("%s: status = %v, want ok", body, status)
		}
		if got.CreditStatus != want {
			t.Errorf("%s: CreditStatus = %q, want %q", body, got.CreditStatus, want)
		}
	}
}

func TestFetchOpenAISubscriptionUsageWindowBodyLeavesTheSpendFieldsUnknown(t *testing.T) {
	// The window/balance shape other plans send must not grow spend facts.
	got, status, _ := fetchUsage(t, respondWith(http.StatusOK, usageBody))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendUnit != "" || got.SpendLimit != "" || got.SpendUsed != "" || got.SpendRemaining != "" ||
		got.SpendUsedPct != -1 || got.SpendResetAt != nil {
		t.Errorf("usage = %+v, want every spend field unknown", got)
	}
	if got.FiveHourPct != 23 || got.WeeklyPct != 61 || got.CreditBalance != "12.34" {
		t.Errorf("usage = %+v, want the windows and the balance unchanged", got)
	}
}

func TestFetchOpenAISubscriptionUsageNon2xxIsUnverifiable(t *testing.T) {
	// A 401 is NOT reported as a bad credential here: the fetch only ever says
	// "got a snapshot" or "could not verify", so the caller keeps what it stored.
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
	}
	for _, code := range statuses {
		t.Run(http.StatusText(code), func(t *testing.T) {
			// The body is a perfectly good snapshot: the status alone decides.
			got, status, _ := fetchUsage(t, respondWith(code, usageBody))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchOpenAISubscriptionUsageJunkBodiesAreUnverifiable(t *testing.T) {
	junk := map[string]string{
		"empty":                  ``,
		"whitespace":             "  \n ",
		"html":                   `<html><body>Just a moment...</body></html>`,
		"truncated json":         `{"rate_limit":{"primary_window":{"used_per`,
		"null":                   `null`,
		"number":                 `42`,
		"string":                 `"rate_limit"`,
		"array at the top":       `[{"rate_limit":{"primary_window":{"used_percent":5}}}]`,
		"empty object":           `{}`,
		"foreign object":         `{"object":"error","message":"nope"}`,
		"error envelope":         `{"error":{"type":"authentication_error","message":"invalid token"}}`,
		"empty rate_limit":       `{"rate_limit":{}}`,
		"empty windows":          `{"rate_limit":{"primary_window":{},"secondary_window":{}},"credits":{}}`,
		"null windows":           `{"rate_limit":{"primary_window":null,"secondary_window":null},"credits":null}`,
		"nothing readable":       `{"rate_limit":{"primary_window":{"used_percent":"x","reset_at":"y"}},"credits":{"balance":[1]}}`,
		"only unmapped fields":   `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false},"credits":{"overage_limit_reached":false},"spend_control":{"reached":false}}`,
		"only a zero reset time": `{"rate_limit":{"primary_window":{"reset_at":0}}}`,
	}
	for name, body := range junk {
		t.Run(name, func(t *testing.T) {
			got, status, _ := fetchUsage(t, respondWith(http.StatusOK, body))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchOpenAISubscriptionUsageTransportFailuresAreUnverifiable(t *testing.T) {
	t.Run("client timeout", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		client.Timeout = 50 * time.Millisecond
		got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
		assertUnknownUsage(t, got, status)
	})

	t.Run("context deadline", func(t *testing.T) {
		client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		got, status := FetchOpenAISubscriptionUsage(ctx, client, probeSecret, usageAccountID)
		assertUnknownUsage(t, got, status)
	})

	t.Run("context canceled", func(t *testing.T) {
		client, _ := newProbeClient(t, respondWith(http.StatusOK, usageBody))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, status := FetchOpenAISubscriptionUsage(ctx, client, probeSecret, usageAccountID)
		assertUnknownUsage(t, got, status)
	})

	t.Run("transport error", func(t *testing.T) {
		// The transport error text embeds the credential: nothing the caller gets
		// back may carry it.
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed for " + probeSecret)
		})}
		got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
		assertUnknownUsage(t, got, status)
		if strings.Contains(got.CreditBalance, probeSecret) {
			t.Errorf("usage %+v contains the credential", got)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		target, _ := url.Parse(srv.URL)
		srv.Close() // nothing listens on target any more
		client := &http.Client{Transport: rewriteTransport{target: target}}
		got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
		assertUnknownUsage(t, got, status)
	})
}

func TestFetchOpenAISubscriptionUsageDoesNotFollowRedirects(t *testing.T) {
	// The bearer token must not ride a redirect to another place: a 3xx ends the
	// fetch.
	client, rec := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
	})
	got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
	if n := len(rec.all()); n != 1 {
		t.Errorf("requests = %d, want 1 (the redirect must not be followed)", n)
	}
	assertUnknownUsage(t, got, status)
}

func TestFetchOpenAISubscriptionUsageLeavesCallersClientUntouched(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusOK, usageBody))
	client.Timeout = 7 * time.Second
	FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
	if client.CheckRedirect != nil {
		t.Error("the usage fetch mutated the caller's http.Client.CheckRedirect")
	}
	if client.Timeout != 7*time.Second {
		t.Errorf("client.Timeout = %v, want it untouched", client.Timeout)
	}
}

func TestFetchOpenAISubscriptionUsageNeverReturnsTheCredential(t *testing.T) {
	// A body that echoes the credential and the account id in the fields we do not
	// map must not surface them: only the five mapped facts come back.
	body := `{"rate_limit":{"primary_window":{"used_percent":12,"reset_at":1760000000,"note":"` + probeSecret + `"}},` +
		`"credits":{"balance":"4.20"},"echo":"` + probeSecret + `","account_id":"` + usageAccountID + `"}`
	got, status, _ := fetchUsage(t, respondWith(http.StatusOK, body))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.CreditBalance != "4.20" || got.FiveHourPct != 12 {
		t.Errorf("usage = %+v, want the mapped fields", got)
	}
	if strings.Contains(got.CreditBalance, probeSecret) || strings.Contains(got.CreditBalance, usageAccountID) {
		t.Errorf("usage %+v contains the credential or the account id", got)
	}
}

func TestFetchOpenAISubscriptionUsageNilClientDoesNotPanic(t *testing.T) {
	// A nil client falls back to the package default; with a canceled context the
	// fetch fails fast without reaching the network.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, status := FetchOpenAISubscriptionUsage(ctx, nil, probeSecret, usageAccountID)
	assertUnknownUsage(t, got, status)
}

func TestFetchOpenAISubscriptionUsageBodyBeyondItsCapIsUnverifiable(t *testing.T) {
	// The fetch reuses the discovery body cap: a body past it is cut off, no
	// longer parses, and is Unverifiable.
	client, _ := newProbeClient(t, respondWith(http.StatusOK, padCatalog(usageBody, maxDiscoveryResponseBytes+4096)))
	got, status := FetchOpenAISubscriptionUsage(context.Background(), client, probeSecret, usageAccountID)
	assertUnknownUsage(t, got, status)
}

func assertUnknownUsage(t *testing.T, got OpenAISubscriptionUsage, status DiscoveryStatus) {
	t.Helper()
	if status != DiscoveryUnverifiable {
		t.Errorf("status = %v, want unverifiable", status)
	}
	if !reflect.DeepEqual(got, wantUnknownUsage()) {
		t.Errorf("usage = %+v, want the all-unknown snapshot %+v", got, wantUnknownUsage())
	}
}
