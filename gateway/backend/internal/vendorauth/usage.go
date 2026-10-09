// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
)

// This file holds the active usage fetch for a ChatGPT (Codex) subscription: one
// GET OpenAIUsageURL that asks the vendor how much of the account's five-hour and
// weekly rate-limit windows is used and what credit balance is left. It is the
// pull counterpart of the passive x-codex-* header scrape the gateway runs on
// every served request, and it answers the same five facts. A Business plan
// reports neither windows nor a balance (both null); its quota lives in
// spend_control.individual_limit and its credit state in the credits flags, so
// the fetch reads those too.
//
// It follows the discovery rules (see discover.go): the fetch is advisory and
// must never block a caller, so it has no error return. It answers a snapshot plus
// a DiscoveryStatus: DiscoveryOK means the vendor served a body from which at
// least one field could be read, DiscoveryUnverifiable means nothing usable could
// be had (any non-2xx answer including 401, a redirect, a timeout, a transport
// failure, a body that is not JSON, or a JSON body that carries none of the
// fields) and the snapshot is then the all-unknown one. The caller merges the
// snapshot over what it already stores, so a field this fetch does not know never
// blanks a known one.
//
// Parsing is tolerant: every field is optional and nullable, a field of the wrong
// type is skipped without losing the well-formed ones around it, and nothing
// panics. The token and the account id are never logged or returned, and no
// vendor text other than the credit-balance string and the bounded spend-control
// strings reaches the caller; transport error text is dropped.

// OpenAISubscriptionUsage is the usage snapshot a ChatGPT subscription reports.
// An unknown field uses the same sentinel the stored usage snapshot does: -1 for a
// percent, nil for a reset time, "" for the credit balance and the other strings.
// A real 0 percent is a known value, never a stand-in for "unknown".
type OpenAISubscriptionUsage struct {
	// FiveHourPct is the used share of the five-hour ("primary") window, 0..100, or
	// -1 when unknown.
	FiveHourPct float64
	// FiveHourResetAt is when the five-hour window resets, or nil when unknown.
	FiveHourResetAt *time.Time
	// WeeklyPct is the used share of the weekly ("secondary") window, 0..100, or -1
	// when unknown.
	WeeklyPct float64
	// WeeklyResetAt is when the weekly window resets, or nil when unknown.
	WeeklyResetAt *time.Time
	// CreditBalance is the vendor's raw credit-balance string (for example
	// "12.34"), or "" when unknown or absent.
	CreditBalance string

	// SpendUnit is the unit the spend-control figures are in (for example
	// "credit"), or "" when unknown.
	SpendUnit string
	// SpendLimit, SpendUsed and SpendRemaining are the vendor's raw spend-control
	// strings (for example "6000", "42.5"), kept verbatim and never parsed to a
	// float, or "" when unknown or absent.
	SpendLimit     string
	SpendUsed      string
	SpendRemaining string
	// SpendUsedPct is the used share of the spend-control limit, 0..100, or -1 when
	// unknown.
	SpendUsedPct float64
	// SpendResetAt is when the spend-control period resets, or nil when unknown.
	SpendResetAt *time.Time
	// CreditStatus is the credit state the credits flags report: "unlimited",
	// "has_credits" or "none", or "" when the vendor did not say.
	CreditStatus string
}

const (
	// unknownUsagePct is the percent sentinel for "the vendor did not say".
	unknownUsagePct = -1

	// maxUsagePct is the largest percent reported; a vendor value above it (an
	// over-limit account) is clamped, like the passive header scrape does.
	maxUsagePct = 100

	// maxEpochSeconds bounds a reset time to year 9999 so an absurd value cannot
	// overflow the conversion to a time.
	maxEpochSeconds = 253402300799

	// maxCreditBalanceLen bounds the credit-balance string kept: a real balance is a
	// short decimal, so anything longer is not one and is treated as unknown rather
	// than passed to the store.
	maxCreditBalanceLen = 64

	// maxSpendValueLen bounds each spend-control string kept (the unit, the limit,
	// the used and the remaining amount): a real one is a short unit name or a short
	// decimal, so anything longer is not one and is treated as unknown.
	maxSpendValueLen = 64
)

// The CreditStatus values.
const (
	creditStatusUnlimited  = "unlimited"
	creditStatusHasCredits = "has_credits"
	creditStatusNone       = "none"
)

// FetchOpenAISubscriptionUsage reads the usage of a ChatGPT (Codex) subscription
// with GET OpenAIUsageURL, the bearer accessToken and the ChatGPT-Account-Id
// accountID (omitted when empty). The request has no query string and no
// originator or version header: the usage endpoint takes none.
//
// The body is mapped as: rate_limit.primary_window -> the five-hour window and
// rate_limit.secondary_window -> the weekly window, each by used_percent and
// reset_at (an absolute unix time in seconds; 0 or absent means unknown), and
// credits.balance -> CreditBalance. The spend control of a Business plan maps as
// spend_control.individual_limit.{unit, limit, used, remaining} -> SpendUnit,
// SpendLimit, SpendUsed and SpendRemaining (strings kept verbatim),
// used_percent -> SpendUsedPct and reset_at (unix seconds) -> SpendResetAt, and the
// credits flags collapse into CreditStatus: "unlimited" when credits.unlimited is
// true, else "has_credits" when credits.has_credits is true, else "none" when it is
// false. Anything the body does not carry stays at the unknown sentinel (see
// OpenAISubscriptionUsage).
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func FetchOpenAISubscriptionUsage(ctx context.Context, httpClient *http.Client, accessToken, accountID string) (OpenAISubscriptionUsage, DiscoveryStatus) {
	headers := map[string]string{
		"Authorization": authBearerPrefix + accessToken,
		"User-Agent":    OpenAIUsageUserAgent,
	}
	if accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{url: OpenAIUsageURL, headers: headers})
	if !ok {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	usage, known := parseOpenAISubscriptionUsage(body)
	if !known {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	return usage, DiscoveryOK
}

// unknownOpenAISubscriptionUsage is the snapshot with every field unknown.
func unknownOpenAISubscriptionUsage() OpenAISubscriptionUsage {
	return OpenAISubscriptionUsage{FiveHourPct: unknownUsagePct, WeeklyPct: unknownUsagePct, SpendUsedPct: unknownUsagePct}
}

// usageWindow is one rate-limit window of the /wham/usage body. The fields stay
// raw so a value of the wrong type can be told from a real zero (decoding "high"
// into a *float64 would leave a pointer to 0, a fabricated percent);
// limit_window_seconds and reset_after_seconds are not read.
type usageWindow struct {
	UsedPercent json.RawMessage `json:"used_percent"`
	ResetAt     json.RawMessage `json:"reset_at"`
}

// usageSpendLimit is spend_control.individual_limit of the /wham/usage body, the
// quota a Business plan reports in place of the rate-limit windows. The fields stay
// raw for the same reason as in usageWindow: limit, used and remaining are JSON
// strings and used_percent and reset_at JSON numbers, and a value of the wrong type
// must read as unknown, not as a fabricated zero. source, remaining_percent and
// reset_after_seconds are not read.
type usageSpendLimit struct {
	Unit        json.RawMessage `json:"unit"`
	Limit       json.RawMessage `json:"limit"`
	Used        json.RawMessage `json:"used"`
	Remaining   json.RawMessage `json:"remaining"`
	UsedPercent json.RawMessage `json:"used_percent"`
	ResetAt     json.RawMessage `json:"reset_at"`
}

// parseOpenAISubscriptionUsage reads the /wham/usage body and reports whether it
// yielded at least one field. A decode error (not JSON, a field of the wrong type)
// is deliberately ignored here and per section: json.Unmarshal keeps every field
// it could decode, and anything it could not stays zero and reads as unknown, so
// one malformed section never sinks the others. A body that is not JSON at all
// decodes nothing.
func parseOpenAISubscriptionUsage(body []byte) (OpenAISubscriptionUsage, bool) {
	var env struct {
		RateLimit *struct {
			Primary   *usageWindow `json:"primary_window"`
			Secondary *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits *struct {
			Balance    json.RawMessage `json:"balance"`
			HasCredits json.RawMessage `json:"has_credits"`
			Unlimited  json.RawMessage `json:"unlimited"`
		} `json:"credits"`
		SpendControl *struct {
			IndividualLimit *usageSpendLimit `json:"individual_limit"`
		} `json:"spend_control"`
	}
	_ = json.Unmarshal(body, &env)

	usage := unknownOpenAISubscriptionUsage()
	if env.RateLimit != nil {
		usage.FiveHourPct, usage.FiveHourResetAt = readUsageWindow(env.RateLimit.Primary)
		usage.WeeklyPct, usage.WeeklyResetAt = readUsageWindow(env.RateLimit.Secondary)
	}
	if env.Credits != nil {
		usage.CreditBalance = readCreditBalance(env.Credits.Balance)
		usage.CreditStatus = readCreditStatus(env.Credits.HasCredits, env.Credits.Unlimited)
	}
	if env.SpendControl != nil {
		readSpendLimit(&usage, env.SpendControl.IndividualLimit)
	}
	known := usage.FiveHourPct != unknownUsagePct || usage.FiveHourResetAt != nil ||
		usage.WeeklyPct != unknownUsagePct || usage.WeeklyResetAt != nil ||
		usage.CreditBalance != "" ||
		usage.SpendUsedPct != unknownUsagePct || usage.SpendResetAt != nil ||
		usage.SpendLimit != "" || usage.SpendUsed != "" ||
		usage.CreditStatus != ""
	return usage, known
}

// readUsageWindow maps one window to a percent (-1 when unknown) and a reset time
// (nil when unknown). A missing window is unknown in both.
func readUsageWindow(w *usageWindow) (float64, *time.Time) {
	if w == nil {
		return unknownUsagePct, nil
	}
	return readUsagePct(w.UsedPercent), readResetTime(w.ResetAt)
}

// readSpendLimit copies one spend-control limit into usage; a missing limit leaves
// every spend field unknown.
func readSpendLimit(usage *OpenAISubscriptionUsage, l *usageSpendLimit) {
	if l == nil {
		return
	}
	usage.SpendUnit = readSpendString(l.Unit)
	usage.SpendLimit = readSpendString(l.Limit)
	usage.SpendUsed = readSpendString(l.Used)
	usage.SpendRemaining = readSpendString(l.Remaining)
	usage.SpendUsedPct = readUsagePct(l.UsedPercent)
	usage.SpendResetAt = readResetTime(l.ResetAt)
}

// readUsagePct reads a used-share percent: -1 (unknown) for an absent, null,
// wrong-typed or negative value, and 100 at most. A real percent is never negative
// (-1 is the unknown sentinel) and the JSON number syntax cannot carry a NaN, so
// only the range needs a check.
func readUsagePct(raw json.RawMessage) float64 {
	if v, ok := readJSONNumber(raw); ok && v >= 0 {
		return math.Min(v, maxUsagePct)
	}
	return unknownUsagePct
}

// readResetTime reads an absolute reset time in unix seconds: nil (unknown) for an
// absent, null, wrong-typed, zero, negative or out-of-range value.
func readResetTime(raw json.RawMessage) *time.Time {
	if v, ok := readJSONNumber(raw); ok && v > 0 && v <= maxEpochSeconds {
		t := time.Unix(int64(v), 0).UTC()
		return &t
	}
	return nil
}

// readJSONNumber reads a JSON number (integer or float). ok is false for an absent
// or null value, for any other type, and for a number a float64 cannot hold.
func readJSONNumber(raw json.RawMessage) (float64, bool) {
	var n *float64
	if json.Unmarshal(raw, &n) != nil || n == nil {
		return 0, false
	}
	return *n, true
}

// readCreditBalance returns the credit balance as a string: the vendor sends a
// string ("12.34"), and a JSON number is kept as the literal the vendor wrote
// (12.34 -> "12.34") should it ever send one. Null, absent, blank, over-long or
// any other type is "" (unknown).
func readCreditBalance(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var balance string
	if json.Unmarshal(raw, &balance) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		balance = n.String()
	}
	balance = strings.TrimSpace(balance)
	if len(balance) > maxCreditBalanceLen {
		return ""
	}
	return balance
}

// readCreditStatus collapses the credits flags into one display state: "unlimited"
// when unlimited is true, else "has_credits" when has_credits is true, else "none"
// when has_credits is false, else "" (unknown: both flags absent, null or of the
// wrong type). Each flag is read on its own, so a malformed one never hides the
// other.
func readCreditStatus(hasCredits, unlimited json.RawMessage) string {
	unlimitedVal, unlimitedOK := readJSONBool(unlimited)
	if unlimitedOK && unlimitedVal {
		return creditStatusUnlimited
	}
	hasVal, hasOK := readJSONBool(hasCredits)
	switch {
	case !hasOK:
		return ""
	case hasVal:
		return creditStatusHasCredits
	default:
		return creditStatusNone
	}
}

// readJSONBool reads a JSON boolean. ok is false for an absent or null value and
// for any other type.
func readJSONBool(raw json.RawMessage) (value, ok bool) {
	var b *bool
	if json.Unmarshal(raw, &b) != nil || b == nil {
		return false, false
	}
	return *b, true
}

// readSpendString returns a spend-control value as the string the vendor sent,
// trimmed and kept verbatim (never parsed to a number). Null, absent, blank,
// over-long or any other type, a JSON number included, is "" (unknown).
func readSpendString(raw json.RawMessage) string {
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	v = strings.TrimSpace(v)
	if len(v) > maxSpendValueLen {
		return ""
	}
	return v
}
