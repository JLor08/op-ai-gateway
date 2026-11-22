// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"op-ai-gateway/internal/auth"
	"strconv"
	"strings"
	"time"
)

// Unified-header suffixes appended to unifiedRateLimitPrefix.
const (
	unifiedStatus                    = "status"
	unifiedReset                     = "reset"
	unifiedOverageStatus             = "overage-status"
	unifiedOverageReset              = "overage-reset"
	unifiedOverageUtilization        = "overage-utilization"
	unifiedOverageSurpassedThreshold = "overage-surpassed-threshold"
)

// limitBlockedBody is the one gateway error body that carries a top-level "type":
// Claude Code reads error.type and error.message of an Anthropic-shaped error, and
// only a "billing_error" with this envelope shows our sentence. Every other gateway
// error keeps the apierror shape (design spec §6), except the Codex
// usage_limit_reached body (codexLimitBlockedBody in codex_principal_limit_headers.go).
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

// limitRetryAfterSeconds is the Retry-After value for a block: the seconds until
// reset, rounded up, at least 1.
func limitRetryAfterSeconds(reset, now time.Time) int {
	return max(int(math.Ceil(reset.Sub(now).Seconds())), 1)
}

// writeLimitBlocked answers a quota or budget denial on the Anthropic Messages
// flavor (design spec §6): 429 with a billing_error body. With the unified headers
// (sendUsage, the token's send_usage_limits opt-in, #420) Claude Code prints the
// body's sentence verbatim (ADR-107); without them it shows its generic form,
// "API Error: Request rejected (429) · <message>". Either way the retries stop,
// through X-Should-Retry false; Retry-After is the seconds until the reset, rounded
// up, at least 1. Those two headers and the body are unconditional. The unified
// headers carry status rejected and the reset; representative-claim and
// overage-status stay absent on purpose: with them the client composes its own line
// and drops ours.
func writeLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time, sendUsage bool) {
	reset := strconv.FormatInt(r.Reset.Unix(), 10)
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(limitRetryAfterSeconds(r.Reset, now)))
	h.Set("X-Should-Retry", "false")
	if sendUsage {
		h.Set(unifiedRateLimitPrefix+unifiedStatus, "rejected")
		h.Set(unifiedRateLimitPrefix+unifiedReset, reset)
		h.Set(unifiedRateLimitPrefix+unifiedOverageReset, reset)
		h.Set(unifiedRateLimitPrefix+unifiedOverageUtilization, "1")
		h.Set(unifiedRateLimitPrefix+unifiedOverageSurpassedThreshold, "1")
		if period := limitBlockedPeriod(r.Period); period != "" {
			h.Set(unifiedRateLimitPrefix+"overage-period", period)
		}
		h.Set(unifiedRateLimitPrefix+"overage-disabled-reason", "org_spend_cap_reached")
	}
	writeJSON(w, http.StatusTooManyRequests, limitBlockedBody{
		Type: "error",
		Error: limitBlockedError{
			Type:    "billing_error",
			Code:    limitBlockedCode(r.Kind),
			Message: limitBlockedMessage(r),
		},
	})
}

// overageUtilizationCents is the success-header utilization in whole percent:
// floor(u * 100), clamped to 0..99. The small epsilon keeps a binary fraction such
// as 0.29 (28.999999999999996 after the multiplication) from dropping a cent. The
// cap at 99 keeps an admitted request below 1: concurrent Record bumps can push the
// cached figure to 1 or above while requests are still allowed.
func overageUtilizationCents(u float64) int {
	cents := int(math.Floor(u*100 + 1e-9))
	return min(max(cents, 0), 99)
}

// overageUtilizationValue formats a utilization as the overage-utilization header
// value: two decimals, floored, at most 0.99 (design spec §5).
func overageUtilizationValue(u float64) string {
	return fmt.Sprintf("0.%02d", overageUtilizationCents(u))
}

// overageSurpassedThreshold is the threshold the written utilization has reached:
// 0.95 from 95 %, 0.75 from 75 %, otherwise none. It is derived from the formatted
// value, so a written 0.75 always carries threshold 0.75.
func overageSurpassedThreshold(cents int) string {
	switch {
	case cents >= 95:
		return "0.95"
	case cents >= 75:
		return "0.75"
	default:
		return ""
	}
}

// writeOverageStanding writes the success-path unified headers that describe the
// principal's own fullest limit r as the overage window (design spec §5). The
// top-level status is allowed_warning when the overage window warns, or when a
// subscription window already in dst warns or is rejected; otherwise allowed. No
// -overage-period: the contract does not list it on a success.
func writeOverageStanding(dst http.Header, r LimitReading) {
	cents := overageUtilizationCents(r.Utilization())
	threshold := overageSurpassedThreshold(cents)
	reset := strconv.FormatInt(r.Reset.Unix(), 10)

	status := "allowed"
	if threshold != "" {
		status = "allowed_warning"
	}
	topStatus := status
	if topStatus == "allowed" && subscriptionWindowWarns(dst) {
		topStatus = "allowed_warning"
	}

	dst.Set(unifiedRateLimitPrefix+unifiedOverageUtilization, fmt.Sprintf("0.%02d", cents))
	if threshold != "" {
		dst.Set(unifiedRateLimitPrefix+unifiedOverageSurpassedThreshold, threshold)
	} else {
		dst.Del(unifiedRateLimitPrefix + unifiedOverageSurpassedThreshold)
	}
	dst.Set(unifiedRateLimitPrefix+unifiedOverageStatus, status)
	dst.Set(unifiedRateLimitPrefix+unifiedOverageReset, reset)
	dst.Set(unifiedRateLimitPrefix+unifiedReset, reset)
	dst.Set(unifiedRateLimitPrefix+"representative-claim", "overage")
	dst.Set(unifiedRateLimitPrefix+unifiedStatus, topStatus)
	dst.Del(unifiedRateLimitPrefix + "overage-period")
}

// subscriptionWindowWarns reports whether a subscription window carried in h has a
// status of allowed_warning or rejected.
func subscriptionWindowWarns(h http.Header) bool {
	for _, w := range rateLimitWindowNames {
		switch h.Get(unifiedRateLimitPrefix + w + "-status") {
		case "allowed_warning", "rejected":
			return true
		}
	}
	return false
}

// isSubscriptionWindowHeader reports whether name is one of the per-window fields
// of the subscription windows #407 relays (-5h-, -7d-, -7d_oi-), matched
// case-insensitively like isUnifiedRateLimitHeader.
func isSubscriptionWindowHeader(name string) bool {
	if !isUnifiedRateLimitHeader(name) {
		return false
	}
	rest := strings.ToLower(name[len(unifiedRateLimitPrefix):])
	for _, w := range rateLimitWindowNames {
		for _, f := range rateLimitWindowFields {
			if rest == w+"-"+f {
				return true
			}
		}
	}
	return false
}

// limitStandingKey carries the principal's admission standing on the request
// context, from handleAnthropicMessages to the attach points.
type limitStandingKey struct{}

// withLimitStanding returns ctx carrying the admission standing s.
func withLimitStanding(ctx context.Context, s LimitStanding) context.Context {
	return context.WithValue(ctx, limitStandingKey{}, s)
}

// limitStandingFrom returns the standing withLimitStanding stored, or nil.
func limitStandingFrom(ctx context.Context) LimitStanding {
	s, _ := ctx.Value(limitStandingKey{}).(LimitStanding)
	return s
}

// attachPrincipalLimitStanding lays the principal's own fullest limit over a 2xx
// /v1/messages response's client header set dst (design spec §5 and §8): every
// unified header except the subscription windows' per-window fields is removed, then
// the overage standing is written, so the gateway's cap owns the top-level fields
// and the windows stay beside it. It does nothing unless ctx carries a standing with
// a describable fullest limit, which keeps the #407 headers exactly as they were for
// a principal without limits or with a failed read. dst must be the client header
// set, never an upstream or capture set.
func attachPrincipalLimitStanding(ctx context.Context, dst http.Header) {
	r, ok := limitStandingFrom(ctx).Fullest()
	if !ok {
		return
	}
	for name := range dst {
		if isUnifiedRateLimitHeader(name) && !isSubscriptionWindowHeader(name) {
			delete(dst, name)
		}
	}
	writeOverageStanding(dst, r)
}

// attachTranslatedUsageLimits is the last header step of a translated /v1/messages
// success: #407's cached subscription windows, then the principal's limit standing
// from the request context. It writes nothing for a token without send_usage_limits
// (#420).
func (s *Server) attachTranslatedUsageLimits(r *http.Request, token auth.Token, dst http.Header) {
	if !token.SendUsageLimits {
		return
	}
	s.attachRateLimitWindows(token, dst)
	attachPrincipalLimitStanding(r.Context(), dst)
}
