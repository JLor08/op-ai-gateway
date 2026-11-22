// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/auth"
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
// reset) and, unless offerModelSwitch, the two credits flags. has-credits true
// suppresses the CLI's gpt-5.4-mini switch-model popup; unlimited false with no
// balance avoids showing a fabricated credit figure. offerModelSwitch is the token's
// codex_model_switch_prompt opt-in (#420): the popup is then wanted, so the gateway
// writes no credits headers of its own.
func writeCodexPrimaryWindow(h http.Header, r LimitReading, usedPercent int, offerModelSwitch bool) {
	h.Set("X-Codex-Primary-Used-Percent", strconv.Itoa(usedPercent))
	if minutes := codexWindowMinutes(r.Period); minutes != "" {
		h.Set("X-Codex-Primary-Window-Minutes", minutes)
	}
	h.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(r.Reset.Unix(), 10))
	if offerModelSwitch {
		return
	}
	h.Set("X-Codex-Credits-Has-Credits", "true")
	h.Set("X-Codex-Credits-Unlimited", "false")
}

// writeCodexLimitStanding writes the success-path headers describing r in the Codex
// CLI's default x-codex family, with the limit name so the CLI labels the bar.
// offerModelSwitch (the token's codex_model_switch_prompt switch) leaves the credits
// flag out, so Codex may offer its model-switch prompt; otherwise the credits flag
// suppresses it.
func writeCodexLimitStanding(dst http.Header, r LimitReading, offerModelSwitch bool) {
	writeCodexPrimaryWindow(dst, r, overageUtilizationCents(r.Utilization()), offerModelSwitch)
	dst.Set("X-Codex-Limit-Name", codexLimitName(r.Kind))
}

// attachCodexPrincipalLimitStanding lays the principal's fullest limit onto a
// response header set dst. It does nothing when dst already carries any Codex
// rate-limit header (the subscription's figures win), when ctx has no describable
// standing, or when the reset is not after now. offerModelSwitch is passed through
// to writeCodexLimitStanding.
func attachCodexPrincipalLimitStanding(ctx context.Context, dst http.Header, now time.Time, offerModelSwitch bool) {
	if hasCodexRateLimitHeader(dst) {
		return
	}
	r, ok := codexPrincipalLimitReading(ctx, now)
	if !ok {
		return
	}
	writeCodexLimitStanding(dst, r, offerModelSwitch)
}

// codexGatewayTakeoverPercent is the written percent from which the gateway's own
// limit replaces the cached subscription windows on a reply. 75 is the first step
// at which the Codex CLI warns about a rate-limit window, so the gateway's limit is
// visible from the moment Codex would start warning.
const codexGatewayTakeoverPercent = 75

// codexPrincipalLimitReading selects the reading the gateway describes to the Codex
// CLI: the principal's fullest limit, only while its reset still lies after now. ok
// is false when there is no describable standing (none, a failed reading, or a
// reset that has passed). The standing writer and the takeover predicate share it so
// both judge the same reading.
func codexPrincipalLimitReading(ctx context.Context, now time.Time) (LimitReading, bool) {
	r, ok := limitStandingFrom(ctx).Fullest()
	if !ok || !r.Reset.After(now) {
		return LimitReading{}, false
	}
	return r, true
}

// codexGatewayLimitTakesOver decides whether the cached subscription windows yield
// to the gateway's own limit on a reply (ADR-109 amendment): true when the
// principal's describable reading would be written at codexGatewayTakeoverPercent
// or more. It compares the written (floored, clamped) percent, so it agrees with
// what the CLI would show.
func codexGatewayLimitTakesOver(ctx context.Context, now time.Time) bool {
	r, ok := codexPrincipalLimitReading(ctx, now)
	return ok && overageUtilizationCents(r.Utilization()) >= codexGatewayTakeoverPercent
}

// attachTranslatedCodexUsageLimits fills the client header set of a translated
// /v1/responses success: the Codex windows last observed for the caller, then the
// principal's own fullest limit unless those windows already put figures there.
// A token without the send_usage_limits opt-in (#420) gets nothing.
func (s *Server) attachTranslatedCodexUsageLimits(r *http.Request, token auth.Token, dst http.Header) {
	if !token.SendUsageLimits {
		return
	}
	s.attachCodexRateLimitWindows(token, dst)
	attachCodexPrincipalLimitStanding(r.Context(), dst, time.Now(), token.CodexModelSwitchPrompt)
}

// writeCodexLimitBlocked answers a quota or budget denial on the Codex flavor: 429,
// usage_limit_reached body, primary window at 100. x-codex-limit-name stays absent
// on purpose: with a limit name the CLI composes its own line and drops the promo
// sentence. X-Should-Retry false stops the retry loop. Retry-After, X-Should-Retry,
// the promo sentence (the only text the CLI prints for the block) and the body are
// unconditional; the primary window goes out only when sendUsage (the token's
// send_usage_limits opt-in, #420) is set, and its credits flags only without
// offerModelSwitch (codex_model_switch_prompt).
func writeCodexLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time, sendUsage, offerModelSwitch bool) {
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(limitRetryAfterSeconds(r.Reset, now)))
	h.Set("X-Should-Retry", "false")
	h.Set("X-Codex-Promo-Message", codexPromoMessage(r.Kind))
	if sendUsage {
		writeCodexPrimaryWindow(h, r, 100, offerModelSwitch)
	}
	writeJSON(w, http.StatusTooManyRequests, codexLimitBlockedBody{
		Error: codexLimitBlockedError{
			Type:     "usage_limit_reached",
			Code:     limitBlockedCode(r.Kind),
			Message:  limitBlockedMessage(r),
			ResetsAt: r.Reset.Unix(),
		},
	})
}
