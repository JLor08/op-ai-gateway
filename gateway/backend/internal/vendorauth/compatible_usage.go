// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// This file holds the active usage fetches of the OpenAI-compatible vendors that
// publish a spend or balance figure: OpenRouter (per-key limit) and Kilo (account
// balance). They answer into the same OpenAISubscriptionUsage snapshot the ChatGPT
// fetch (usage.go) does, and follow the same rules: no error return, a snapshot
// plus a DiscoveryStatus, tolerant parsing, and Unverifiable for a non-2xx answer
// (401 included), a redirect, a transport failure or a body with nothing usable.
//
// Unlike FetchOpenAISubscriptionUsage they take the URL: the portal composes it
// from the preset's base URL and usage path, so this package stays free of the
// routing catalogue. The api key is sent as a bearer token and is never logged or
// returned; transport error text is dropped.

// FetchOpenRouterKeyUsage reads OpenRouter's per-key limit and usage with GET
// keyURL (the /api/v1/key endpoint, composed by the portal) and a bearer apiKey.
//
// The body is mapped from data.{limit, limit_remaining}: a numeric limit is a
// capped key and maps to SpendUnit "USD", SpendLimit, SpendRemaining, SpendUsed
// (limit - remaining, never below 0), SpendUsedPct (0..100, only for a positive
// limit) and CreditStatus ("has_credits" while remaining > 0, else "none"; unknown
// when limit_remaining is absent). An explicit null limit is a key without a cap
// and maps to CreditStatus "unlimited" with every spend field left empty: data.usage
// is an all-time total, not a period figure, so showing it next to "Unlimited"
// would mislead. All amounts are per key, so CreditBalance stays empty.
//
// REVERSE-ENGINEERED / VERIFY LIVE: the shape is derived from OpenRouter's docs.
func FetchOpenRouterKeyUsage(ctx context.Context, httpClient *http.Client, keyURL, apiKey string) (OpenAISubscriptionUsage, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     keyURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	usage, known := parseOpenRouterKeyUsage(body)
	if !known {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	return usage, DiscoveryOK
}

// parseOpenRouterKeyUsage reads the /api/v1/key body and reports whether it
// yielded at least one field. The fields stay raw so a null limit (no cap) can be
// told from an absent or wrong-typed one, and a decode error is ignored for the
// same reason as in parseOpenAISubscriptionUsage.
func parseOpenRouterKeyUsage(body []byte) (OpenAISubscriptionUsage, bool) {
	var env struct {
		Data struct {
			Limit          json.RawMessage `json:"limit"`
			LimitRemaining json.RawMessage `json:"limit_remaining"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &env)

	u := unknownOpenAISubscriptionUsage()
	limit, limitOK := readJSONNumber(env.Data.Limit)
	switch {
	case limitOK: // a capped key
		u.SpendUnit = "USD"
		u.SpendLimit = formatMoney(limit)
		if remaining, remOK := readJSONNumber(env.Data.LimitRemaining); remOK {
			u.SpendRemaining = formatMoney(remaining)
			used := math.Max(limit-remaining, 0)
			u.SpendUsed = formatMoney(used)
			if limit > 0 {
				u.SpendUsedPct = math.Min(used/limit*100, maxUsagePct)
			}
			if remaining > 0 {
				u.CreditStatus = creditStatusHasCredits
			} else {
				u.CreditStatus = creditStatusNone
			}
		}
	case bytes.Equal(bytes.TrimSpace(env.Data.Limit), []byte("null")): // no cap
		u.CreditStatus = creditStatusUnlimited
	}
	known := u.SpendLimit != "" || u.CreditStatus != ""
	return u, known
}

// FetchKiloBalanceUsage reads Kilo's credit balance with GET balanceURL (the
// /api/profile/balance endpoint, composed by the portal) and a bearer apiKey. The
// body {"balance": n} maps to CreditBalance and CreditStatus ("has_credits" while
// the balance is above 0, else "none"). SpendUnit stays empty: the unit of the
// balance is undocumented and is not asserted.
//
// BEST-EFFORT / UNDOCUMENTED: the endpoint is not part of Kilo's published API and
// may not accept the stored api-key type, so any failure, a missing or garbled
// balance included, degrades to Unverifiable.
func FetchKiloBalanceUsage(ctx context.Context, httpClient *http.Client, balanceURL, apiKey string) (OpenAISubscriptionUsage, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     balanceURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	var env struct {
		Balance json.RawMessage `json:"balance"`
	}
	_ = json.Unmarshal(body, &env)

	balance, balOK := readJSONNumber(env.Balance)
	if !balOK {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	u := unknownOpenAISubscriptionUsage()
	u.CreditBalance = formatMoney(balance)
	if u.CreditBalance == "" { // a number no real balance is
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	if balance > 0 {
		u.CreditStatus = creditStatusHasCredits
	} else {
		u.CreditStatus = creditStatusNone
	}
	return u, DiscoveryOK
}

// formatMoney renders a monetary amount as a plain decimal without exponent or
// float noise (rounded to six places). It returns "" (unknown) for an amount that
// is not finite or whose rendering is longer than the stored-string cap, so an
// absurd vendor value never reaches the store.
func formatMoney(v float64) string {
	rounded := math.Round(v*1e6) / 1e6
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) {
		return ""
	}
	if rounded == 0 {
		rounded = 0 // normalise a negative zero, which would print as "-0"
	}
	s := strconv.FormatFloat(rounded, 'f', -1, 64)
	if len(s) > maxSpendValueLen {
		return ""
	}
	return s
}
