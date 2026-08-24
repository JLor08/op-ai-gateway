// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
)

// redirectUnknownModel returns the model to use instead of `requested`, or ""
// when the request keeps its model. It runs AFTER resolveModelOverride, so an
// exact override row and the catch-all have already had their say — the
// redirect only ever sees what neither of them claimed.
//
// The chain: the token's last successfully routed model, but only while it is
// still offered for this flavor; then the configured fallback, under the same
// condition; then nothing, which leaves today's error untouched.
//
// "Does not apply" is deliberately narrow by default: only a name that does not
// exist at all. A model that exists but this token may not use is a refusal,
// and a refusal is a signal about a misconfiguration — silently routing around
// it costs whoever debugs it later. UnknownModelRedirectBlocked widens this to
// cover those too. Either way the caller keeps applying every admission gate to
// the RESULT, so the redirect can never reach further than the token may.
//
// A failed offering lookup needs no special case here. ModelOfferingFor is
// all-or-nothing and hands back two WHOLLY EMPTY sets on any store error, and
// against empty sets every candidate reads as unoffered, so the chain runs out
// and declines. A store hiccup therefore surfaces as today's ordinary error
// rather than silently rerouting the request somewhere arbitrary.
func redirectUnknownModel(token auth.Token, requested string, off portal.ModelOffering) string {
	if !token.UnknownModelRedirect {
		return ""
	}
	if _, offered := off.Offered[requested]; offered {
		return ""
	}
	if _, exists := off.Existing[requested]; exists && !token.UnknownModelRedirectBlocked {
		return ""
	}
	for _, candidate := range []string{token.LastUsedModel, token.UnknownModelFallback} {
		if candidate == "" {
			continue
		}
		if _, ok := off.Offered[candidate]; ok {
			return candidate
		}
	}
	return ""
}
