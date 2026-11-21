// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// codexLimitBlockedBody is the 429 body the Codex CLI parses: error.type
// "usage_limit_reached" plus resets_at selects its usage-limit message.
type codexLimitBlockedBody struct {
	Error codexLimitBlockedError `json:"error"`
}

type codexLimitBlockedError struct {
	Type     string `json:"type"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	ResetsAt int64  `json:"resets_at"`
}

// codexWindowMinutes maps a limit period onto the window length the Codex CLI
// renders. An empty result means the header is omitted.
func codexWindowMinutes(period string) string {
	switch period {
	case "hour":
		return "60"
	case "day":
		return "1440"
	case "week":
		return "10080"
	case "month":
		return "43200"
	default:
		return ""
	}
}

// codexLimitName is the limit name the Codex CLI shows for the gateway's own limit
// on a success response.
func codexLimitName(kind LimitKind) string {
	return "OP AI Gateway " + limitBlockedLabel(kind)
}

// codexPromoMessage is the ASCII-only sentence the Codex CLI shows on a block.
func codexPromoMessage(kind LimitKind) string {
	return "OP AI Gateway " + limitBlockedLabel(kind) + " reached; ask your gateway administrator to raise it"
}

// writeCodexPrimaryWindow writes the primary window (usedPercent, window length,
// reset) and the two credits flags. has-credits true suppresses the CLI's
// gpt-5.4-mini switch-model popup; unlimited false with no balance avoids showing a
// fabricated credit figure.
func writeCodexPrimaryWindow(h http.Header, r LimitReading, usedPercent int) {
	h.Set("X-Codex-Primary-Used-Percent", strconv.Itoa(usedPercent))
	if minutes := codexWindowMinutes(r.Period); minutes != "" {
		h.Set("X-Codex-Primary-Window-Minutes", minutes)
	}
	h.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(r.Reset.Unix(), 10))
	h.Set("X-Codex-Credits-Has-Credits", "true")
	h.Set("X-Codex-Credits-Unlimited", "false")
}

// writeCodexLimitStanding writes the success-path headers describing r in the Codex
// CLI's default x-codex family, with the limit name so the CLI labels the bar.
func writeCodexLimitStanding(dst http.Header, r LimitReading) {
	writeCodexPrimaryWindow(dst, r, overageUtilizationCents(r.Utilization()))
	dst.Set("X-Codex-Limit-Name", codexLimitName(r.Kind))
}

// attachCodexPrincipalLimitStanding lays the principal's fullest limit onto a
// response header set dst. It does nothing when dst already carries any Codex
// rate-limit header (the subscription's figures win), when ctx has no describable
// standing, or when the reset is not after now.
func attachCodexPrincipalLimitStanding(ctx context.Context, dst http.Header, now time.Time) {
	if hasCodexRateLimitHeader(dst) {
		return
	}
	r, ok := limitStandingFrom(ctx).Fullest()
	if !ok || !r.Reset.After(now) {
		return
	}
	writeCodexLimitStanding(dst, r)
}

// writeCodexLimitBlocked answers a quota or budget denial on the Codex flavor: 429,
// usage_limit_reached body, primary window at 100. x-codex-limit-name stays absent
// on purpose: with a limit name the CLI composes its own line and drops the promo
// sentence. X-Should-Retry false stops the retry loop.
func writeCodexLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time) {
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(limitRetryAfterSeconds(r.Reset, now)))
	h.Set("X-Should-Retry", "false")
	h.Set("X-Codex-Promo-Message", codexPromoMessage(r.Kind))
	writeCodexPrimaryWindow(h, r, 100)
	writeJSON(w, http.StatusTooManyRequests, codexLimitBlockedBody{
		Error: codexLimitBlockedError{
			Type:     "usage_limit_reached",
			Code:     limitBlockedCode(r.Kind),
			Message:  limitBlockedMessage(r),
			ResetsAt: r.Reset.Unix(),
		},
	})
}
