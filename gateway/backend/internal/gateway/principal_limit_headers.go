// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// limitBlockedBody is the one gateway error body that carries a top-level "type":
// Claude Code reads error.type and error.message of an Anthropic-shaped error, and
// only a "billing_error" with this envelope shows our sentence. Every other gateway
// error keeps the apierror shape (design spec §6).
type limitBlockedBody struct {
	Type  string            `json:"type"`
	Error limitBlockedError `json:"error"`
}

type limitBlockedError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// limitBlockedCode maps a limit kind to its stable error code.
func limitBlockedCode(kind LimitKind) string {
	switch kind {
	case LimitKindRequestQuota:
		return codeLimitRequestQuotaExceeded
	case LimitKindTokenQuota:
		return codeLimitTokenQuotaExceeded
	default:
		return codeLimitCostBudgetExceeded
	}
}

// limitBlockedLabel is the human name of a limit kind in the block sentence.
func limitBlockedLabel(kind LimitKind) string {
	switch kind {
	case LimitKindRequestQuota:
		return "request quota"
	case LimitKindTokenQuota:
		return "token quota"
	default:
		return "cost budget"
	}
}

// limitBlockedPeriod maps a limit period onto the unified family's period name. The
// hour has none: the client accepts a missing period.
func limitBlockedPeriod(period string) string {
	switch period {
	case "day":
		return "daily"
	case "week":
		return "weekly"
	case "month":
		return "monthly"
	default:
		return ""
	}
}

// limitBlockedMessage is the fixed English sentence Claude Code shows for a block:
// which limit, when it resets (UTC) and whom to ask.
func limitBlockedMessage(r LimitReading) string {
	return limitBlockedLabel(r.Kind) + " reached · resets " + r.Reset.UTC().Format("Jan 2, 15:04") +
		" UTC · ask your gateway administrator to raise it"
}

// writeLimitBlocked answers a quota or budget denial on the Anthropic Messages
// flavor (design spec §6): 429 with a billing_error body and the unified headers
// that make Claude Code print r's sentence. representative-claim and overage-status
// stay absent on purpose: with them the client composes its own line and drops ours.
// X-Should-Retry false stops the client's retry loop; Retry-After is the seconds
// until the reset, rounded up, at least 1.
func writeLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time) {
	seconds := int(math.Ceil(r.Reset.Sub(now).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	reset := strconv.FormatInt(r.Reset.Unix(), 10)
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(seconds))
	h.Set("X-Should-Retry", "false")
	h.Set(unifiedRateLimitPrefix+"status", "rejected")
	h.Set(unifiedRateLimitPrefix+"reset", reset)
	h.Set(unifiedRateLimitPrefix+"overage-reset", reset)
	h.Set(unifiedRateLimitPrefix+"overage-utilization", "1")
	h.Set(unifiedRateLimitPrefix+"overage-surpassed-threshold", "1")
	if period := limitBlockedPeriod(r.Period); period != "" {
		h.Set(unifiedRateLimitPrefix+"overage-period", period)
	}
	h.Set(unifiedRateLimitPrefix+"overage-disabled-reason", "org_spend_cap_reached")
	writeJSON(w, http.StatusTooManyRequests, limitBlockedBody{
		Type: "error",
		Error: limitBlockedError{
			Type:    "billing_error",
			Code:    limitBlockedCode(r.Kind),
			Message: limitBlockedMessage(r),
		},
	})
}
