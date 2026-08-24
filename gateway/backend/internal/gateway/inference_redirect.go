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
// Both questions this asks the offering — "does the request apply" and "is this
// candidate usable" — are asked of Callable, NEVER of a LISTING. A listing set
// is a display: a model_settings "hidden" name drops out of it while the model
// stays fully callable under that same name. Judging by one would make widened
// mode fire on a request the token was entitled to serve and reroute it away
// from a working model — and, worse, defeat a catch-all whose target happens to
// be hidden, which must always win over this redirect. (portal.ModelOffering
// deliberately carries no listing set at all, so there is nothing here to reach
// for by mistake.) Callable
// means "a direct request for this name can succeed", which is the only correct
// answer to either question — including for a "locked" (group-only) name, which
// Callable excludes precisely because a direct request for it cannot route.
// Both questions go through callableFor, which is Callable narrowed by the one
// admission gate the portal cannot see: the service-account model allowlist.
//
// A failed offering lookup needs no special case here. ModelOfferingFor is
// all-or-nothing and hands back WHOLLY EMPTY sets on any store error, and
// against empty sets every candidate reads as uncallable, so the chain runs out
// and declines. A store hiccup therefore surfaces as today's ordinary error
// rather than silently rerouting the request somewhere arbitrary.
func redirectUnknownModel(token auth.Token, requested string, off portal.ModelOffering) string {
	if !token.UnknownModelRedirect {
		return ""
	}
	if callableFor(token, off, requested) {
		return ""
	}
	if _, exists := off.Existing[requested]; exists && !token.UnknownModelRedirectBlocked {
		return ""
	}
	for _, candidate := range []string{token.LastUsedModel, token.UnknownModelFallback} {
		if candidate == "" {
			continue
		}
		if callableFor(token, off, candidate) {
			return candidate
		}
	}
	return ""
}

// callableFor answers "can a direct request from THIS token for `name` succeed"
// — the single question both halves of redirectUnknownModel ask.
//
// portal.ModelOffering.Callable answers it for everything the portal knows
// about: the server-side existence of the name, resource-group provisioning,
// and the "locked" (group-only) access boundary. It cannot answer it alone,
// because one admission gate lives entirely on this side of the boundary and
// the portal never sees it — the service-account model allowlist (modelAllowed,
// inference_handlers.go). A name blocked ONLY by the allowlist is in Callable
// and yet 403s at inference_handlers.go's modelAllowed gate a few lines later.
//
// Leaving it out broke the feature in both directions:
//
//   - The requested-model half: an allowlist-blocked name looked callable, so
//     the redirect declined and the client got the 403 — even under
//     UnknownModelRedirectBlocked, whose entire purpose is "also redirect the
//     models this token may not use". The service allowlist is the canonical
//     instance of that case (it is the only per-token allowlist there is), so
//     widened mode covered every case except the one it was written for.
//   - The candidate half: an allowlist-blocked LastUsedModel or fallback looked
//     usable, so the redirect would swap the client's model for one that then
//     fails the very next gate — turning a legible "unknown model" into a 403
//     naming a model the client never sent.
//
// An empty allowlist on a service token, and any non-service token, mean "every
// model allowed" — modelAllowed is a no-op there, so this stays exactly
// off.Callable for every token that has no allowlist at all.
func callableFor(token auth.Token, off portal.ModelOffering, name string) bool {
	if _, ok := off.Callable[name]; !ok {
		return false
	}
	return modelAllowed(token, name)
}
