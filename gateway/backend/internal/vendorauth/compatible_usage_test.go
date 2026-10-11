// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	openRouterKeyURL    = "https://openrouter.example.test/api/v1/key"
	kiloBalanceURL      = "https://kilo.example.test/api/profile/balance"
	openRouterCappedRaw = `{"data":{"label":"k","limit":100,"limit_remaining":74.5,"limit_reset":"monthly","usage":25.5,"is_free_tier":false}}`
)

func fetchOpenRouter(t *testing.T, h http.HandlerFunc) (OpenAISubscriptionUsage, DiscoveryStatus, *probeRecorder) {
	t.Helper()
	client, rec := newProbeClient(t, h)
	got, status := FetchOpenRouterKeyUsage(context.Background(), client, openRouterKeyURL, probeSecret)
	return got, status, rec
}

func fetchKilo(t *testing.T, h http.HandlerFunc) (OpenAISubscriptionUsage, DiscoveryStatus, *probeRecorder) {
	t.Helper()
	client, rec := newProbeClient(t, h)
	got, status := FetchKiloBalanceUsage(context.Background(), client, kiloBalanceURL, probeSecret)
	return got, status, rec
}

func assertOpenRouterFields(t *testing.T, got OpenAISubscriptionUsage, want OpenAISubscriptionUsage) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestFetchOpenRouterKeyUsageRequestShape(t *testing.T) {
	_, _, rec := fetchOpenRouter(t, respondWith(http.StatusOK, openRouterCappedRaw))

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want exactly 1", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.Method)
	}
	if got.Host != "openrouter.example.test" || got.Path != "/api/v1/key" {
		t.Errorf("target = %s%s, want the composed key URL", got.Host, got.Path)
	}
	if v := got.Header.Get("Authorization"); v != "Bearer "+probeSecret {
		t.Errorf("Authorization = %q, want the bearer key", v)
	}
	if v := got.Header.Get("Accept"); v != "application/json" {
		t.Errorf("Accept = %q", v)
	}
	if got.Body != "" {
		t.Errorf("body = %q, want none", got.Body)
	}
}

func TestFetchOpenRouterKeyUsageCappedKey(t *testing.T) {
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, openRouterCappedRaw))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := wantUnknownUsage()
	want.SpendUnit = "USD"
	want.SpendLimit = "100"
	want.SpendRemaining = "74.5"
	want.SpendUsed = "25.5"
	want.SpendUsedPct = 25.5
	want.CreditStatus = creditStatusHasCredits
	assertOpenRouterFields(t, got, want)
	if got.CreditBalance != "" {
		t.Errorf("CreditBalance = %q, want empty (per-key, not an account balance)", got.CreditBalance)
	}
	if got.FiveHourPct != -1 || got.WeeklyPct != -1 {
		t.Errorf("windows = %v/%v, want unknown", got.FiveHourPct, got.WeeklyPct)
	}
}

func TestFetchOpenRouterKeyUsageUnlimitedKey(t *testing.T) {
	// An explicit null limit is "no per-key cap". usage is all-time, not a period
	// figure, so it must not surface as a spend amount.
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":null,"usage":12.3}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := wantUnknownUsage()
	want.CreditStatus = creditStatusUnlimited
	assertOpenRouterFields(t, got, want)
}

func TestFetchOpenRouterKeyUsageRemainingZeroIsNone(t *testing.T) {
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":10,"limit_remaining":0}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := wantUnknownUsage()
	want.SpendUnit = "USD"
	want.SpendLimit = "10"
	want.SpendRemaining = "0"
	want.SpendUsed = "10"
	want.SpendUsedPct = 100
	want.CreditStatus = creditStatusNone
	assertOpenRouterFields(t, got, want)
}

func TestFetchOpenRouterKeyUsageOverLimitIsClamped(t *testing.T) {
	// A negative remaining (spent past the cap) reads as exhausted, with the used
	// share clamped to 100.
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":10,"limit_remaining":-5}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendUsedPct != 100 || got.CreditStatus != creditStatusNone {
		t.Errorf("usage = %+v, want pct 100 and status none", got)
	}
}

func TestFetchOpenRouterKeyUsageRemainingAboveLimitFloorsUsedAtZero(t *testing.T) {
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":10,"limit_remaining":12}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendUsed != "0" || got.SpendUsedPct != 0 || got.CreditStatus != creditStatusHasCredits {
		t.Errorf("usage = %+v, want used 0, pct 0, has_credits", got)
	}
}

func TestFetchOpenRouterKeyUsageZeroLimitLeavesThePercentUnknown(t *testing.T) {
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":0,"limit_remaining":0}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendUsedPct != -1 || got.SpendLimit != "0" || got.CreditStatus != creditStatusNone {
		t.Errorf("usage = %+v, want limit 0, pct unknown, none", got)
	}
}

func TestFetchOpenRouterKeyUsageCappedWithoutRemainingKeepsTheLimit(t *testing.T) {
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":50}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := wantUnknownUsage()
	want.SpendUnit = "USD"
	want.SpendLimit = "50"
	assertOpenRouterFields(t, got, want)
}

func TestFetchOpenRouterKeyUsageMoneyFormatting(t *testing.T) {
	// Float noise and exponent notation never reach the stored strings.
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":1e2,"limit_remaining":0.30000000000000004}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendLimit != "100" || got.SpendRemaining != "0.3" || got.SpendUsed != "99.7" {
		t.Errorf("usage = %+v, want limit 100, remaining 0.3, used 99.7", got)
	}
}

func TestFetchOpenRouterKeyUsageUnusableBodiesAreUnverifiable(t *testing.T) {
	junk := map[string]string{
		"empty":                ``,
		"html":                 `<html><body>Just a moment...</body></html>`,
		"truncated json":       `{"data":{"limit":10,"limit_rem`,
		"null":                 `null`,
		"array at the top":     `[{"data":{"limit":10}}]`,
		"empty object":         `{}`,
		"empty data":           `{"data":{}}`,
		"null data":            `{"data":null}`,
		"label only":           `{"data":{"label":"k"}}`,
		"usage only":           `{"data":{"usage":12.3}}`,
		"wrong-typed limit":    `{"data":{"limit":"oops"}}`,
		"boolean limit":        `{"data":{"limit":true}}`,
		"object limit":         `{"data":{"limit":{"v":1}}}`,
		"wrong-typed all":      `{"data":{"limit":"a","limit_remaining":"b","usage":"c"}}`,
		"absent limit":         `{"data":{"limit_remaining":5,"usage":1}}`,
		"error envelope":       `{"error":{"code":401,"message":"No auth credentials found"}}`,
		"data is a string":     `{"data":"x"}`,
		"overflowing limit":    `{"data":{"limit":1e999}}`,
		"astronomical limit":   `{"data":{"limit":1.7e308}}`,
		"flat (unwrapped) key": `{"limit":10,"limit_remaining":5}`,
	}
	for name, body := range junk {
		t.Run(name, func(t *testing.T) {
			got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, body))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchOpenRouterKeyUsageWrongTypedRemainingKeepsTheLimit(t *testing.T) {
	// A field of the wrong type is skipped without losing the well-formed ones.
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, `{"data":{"limit":100,"limit_remaining":"x"}}`))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if got.SpendLimit != "100" || got.SpendRemaining != "" || got.SpendUsed != "" || got.SpendUsedPct != -1 || got.CreditStatus != "" {
		t.Errorf("usage = %+v, want only the limit known", got)
	}
}

func TestFetchOpenRouterKeyUsageNon2xxIsUnverifiable(t *testing.T) {
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
	}
	for _, code := range statuses {
		t.Run(http.StatusText(code), func(t *testing.T) {
			// The body is a perfectly good snapshot: the status alone decides.
			got, status, _ := fetchOpenRouter(t, respondWith(code, openRouterCappedRaw))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchOpenRouterKeyUsageDoesNotFollowRedirects(t *testing.T) {
	got, status, rec := fetchOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
	})
	if n := len(rec.all()); n != 1 {
		t.Errorf("requests = %d, want 1 (the redirect must not be followed)", n)
	}
	assertUnknownUsage(t, got, status)
}

func TestFetchOpenRouterKeyUsageTransportErrorIsUnverifiableAndLeakFree(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + probeSecret)
	})}
	got, status := FetchOpenRouterKeyUsage(context.Background(), client, openRouterKeyURL, probeSecret)
	assertUnknownUsage(t, got, status)
}

func TestFetchOpenRouterKeyUsageNeverReturnsTheCredential(t *testing.T) {
	// A body that echoes the credential in fields we do not map must not surface it.
	body := `{"data":{"label":"` + probeSecret + `","key":"` + probeSecret + `","limit":100,"limit_remaining":40,"echo":"` + probeSecret + `"}}`
	got, status, _ := fetchOpenRouter(t, respondWith(http.StatusOK, body))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	assertNoCredential(t, got)
}

func TestFetchOpenRouterKeyUsageNilClientDoesNotPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, status := FetchOpenRouterKeyUsage(ctx, nil, openRouterKeyURL, probeSecret)
	assertUnknownUsage(t, got, status)
}

func TestFetchKiloBalanceUsageRequestShape(t *testing.T) {
	_, _, rec := fetchKilo(t, respondWith(http.StatusOK, `{"balance":12.34}`))

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want exactly 1", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.Method)
	}
	if got.Host != "kilo.example.test" || got.Path != "/api/profile/balance" {
		t.Errorf("target = %s%s, want the composed balance URL", got.Host, got.Path)
	}
	if v := got.Header.Get("Authorization"); v != "Bearer "+probeSecret {
		t.Errorf("Authorization = %q, want the bearer key", v)
	}
	if got.Body != "" {
		t.Errorf("body = %q, want none", got.Body)
	}
}

func TestFetchKiloBalanceUsageMapsTheBalance(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantBalance string
		wantStatus  string
	}{
		{"positive", `{"balance":12.34}`, "12.34", creditStatusHasCredits},
		{"integer", `{"balance":7}`, "7", creditStatusHasCredits},
		{"zero", `{"balance":0}`, "0", creditStatusNone},
		{"negative", `{"balance":-1.5}`, "-1.5", creditStatusNone},
		{"float noise", `{"balance":0.1000000000000001}`, "0.1", creditStatusHasCredits},
		{"extra fields ride along", `{"balance":3.5,"isDepleted":false,"organizationId":"o"}`, "3.5", creditStatusHasCredits},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, status, _ := fetchKilo(t, respondWith(http.StatusOK, tc.body))
			if status != DiscoveryOK {
				t.Fatalf("status = %v, want ok", status)
			}
			want := wantUnknownUsage()
			want.CreditBalance = tc.wantBalance
			want.CreditStatus = tc.wantStatus
			assertOpenRouterFields(t, got, want)
			if got.SpendUnit != "" {
				t.Errorf("SpendUnit = %q, want empty (the unit is undocumented, never asserted)", got.SpendUnit)
			}
		})
	}
}

func TestFetchKiloBalanceUsageUnusableBodiesAreUnverifiable(t *testing.T) {
	junk := map[string]string{
		"empty":               ``,
		"empty object":        `{}`,
		"non-JSON":            `balance: 12`,
		"html":                `<html><body>Sign in</body></html>`,
		"truncated":           `{"balance":12.`,
		"null":                `null`,
		"array at the top":    `[{"balance":1}]`,
		"null balance":        `{"balance":null}`,
		"string balance":      `{"balance":"12.34"}`,
		"boolean balance":     `{"balance":true}`,
		"object balance":      `{"balance":{"v":1}}`,
		"array balance":       `{"balance":[1]}`,
		"overflowing balance": `{"balance":1e999}`,
		"astronomical":        `{"balance":1.7e308}`,
		"nested balance":      `{"data":{"balance":5}}`,
		"error envelope":      `{"error":"Unauthorized"}`,
	}
	for name, body := range junk {
		t.Run(name, func(t *testing.T) {
			got, status, _ := fetchKilo(t, respondWith(http.StatusOK, body))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchKiloBalanceUsageNon2xxIsUnverifiable(t *testing.T) {
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
	}
	for _, code := range statuses {
		t.Run(http.StatusText(code), func(t *testing.T) {
			got, status, _ := fetchKilo(t, respondWith(code, `{"balance":12.34}`))
			assertUnknownUsage(t, got, status)
		})
	}
}

func TestFetchKiloBalanceUsageDoesNotFollowRedirects(t *testing.T) {
	got, status, rec := fetchKilo(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
	})
	if n := len(rec.all()); n != 1 {
		t.Errorf("requests = %d, want 1 (the redirect must not be followed)", n)
	}
	assertUnknownUsage(t, got, status)
}

func TestFetchKiloBalanceUsageTransportErrorIsUnverifiableAndLeakFree(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + probeSecret)
	})}
	got, status := FetchKiloBalanceUsage(context.Background(), client, kiloBalanceURL, probeSecret)
	assertUnknownUsage(t, got, status)
}

func TestFetchKiloBalanceUsageNeverReturnsTheCredential(t *testing.T) {
	body := `{"balance":9.5,"echo":"` + probeSecret + `","token":"` + probeSecret + `"}`
	got, status, _ := fetchKilo(t, respondWith(http.StatusOK, body))
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	assertNoCredential(t, got)
}

func TestFetchKiloBalanceUsageNilClientDoesNotPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, status := FetchKiloBalanceUsage(ctx, nil, kiloBalanceURL, probeSecret)
	assertUnknownUsage(t, got, status)
}

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{100, "100"},
		{74.5, "74.5"},
		{12.34, "12.34"},
		{0.30000000000000004, "0.3"},
		{1e-7, "0"},
		{-1e-9, "0"},
		{1.2345678, "1.234568"},
		{-1.5, "-1.5"},
		{123456789, "123456789"},
		{1e300, ""},       // too long for the stored-string cap
		{math.Inf(1), ""}, // not a number a vendor can send, but never rendered
		{math.NaN(), ""},
	}
	for _, tc := range cases {
		if got := formatMoney(tc.in); got != tc.want {
			t.Errorf("formatMoney(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// assertNoCredential fails when the probe credential appears in any string field.
func assertNoCredential(t *testing.T, got OpenAISubscriptionUsage) {
	t.Helper()
	for name, v := range map[string]string{
		"CreditBalance": got.CreditBalance, "SpendUnit": got.SpendUnit, "SpendLimit": got.SpendLimit,
		"SpendUsed": got.SpendUsed, "SpendRemaining": got.SpendRemaining, "CreditStatus": got.CreditStatus,
	} {
		if strings.Contains(v, probeSecret) {
			t.Errorf("%s = %q carries the credential", name, v)
		}
	}
}
