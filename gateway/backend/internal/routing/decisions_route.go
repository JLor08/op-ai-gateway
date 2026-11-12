// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// DecisionsPassthroughCapableKind reports whether an application type or runtime
// spec type can serve POST /v1/decisions natively. llama-server has no such
// route (ADR-098). The vLLM Semantic Router's /v1/decisions is its alias of
// System One, not the OpenAI shape (ADR-099), so an OpenAI-shaped body must
// never be passed through to it. Every other kind may (the probe decides per
// model). ProviderLlamaCPP == RuntimeSpecTypeLlamaCpp and ProviderVllmSR ==
// RuntimeSpecTypeVllmSR, so one comparison covers both vocabularies.
func DecisionsPassthroughCapableKind(kind string) bool {
	return kind != ProviderLlamaCPP && kind != ProviderVllmSR
}

// EffectiveDecisionsRoute computes the concrete route of an OpenAI Decisions
// request from the effective decisions mode and the mapping's two verdicts
// (ADR-098): decision (System One yes) and decisionOpenAI (native /v1/decisions
// yes). A blank mode behaves as auto.
//
//   - disabled: (disabled, true); the caller answers the endpoint-disabled error.
//   - auto: passthrough when the mapping answers /v1/decisions natively, else
//     translate when it answers System One, else no route.
//   - translate: translate, ok only with the decision verdict.
//   - passthrough: passthrough, ok only with the decision_openai verdict.
//
// kind is the effective upstream kind (the runtime spec type for a server_agent
// mapping with a spec, else the application type). For a kind that is not
// DecisionsPassthroughCapableKind a decisionOpenAI verdict is ignored, so auto
// resolves to translate, and a forced passthrough (a stale row; the save guards
// refuse it) is not ok rather than sent to an upstream that cannot parse it.
//
// ok == false means the mapping has no verdict for the route; the handler
// answers routing.model_not_capable.
func EffectiveDecisionsRoute(kind string, mode EndpointMode, decision, decisionOpenAI bool) (route EndpointMode, ok bool) {
	if !DecisionsPassthroughCapableKind(kind) {
		decisionOpenAI = false
	}
	switch mode {
	case EndpointModeDisabled:
		return EndpointModeDisabled, true
	case EndpointModeTranslate:
		return EndpointModeTranslate, decision
	case EndpointModePassthrough:
		return EndpointModePassthrough, decisionOpenAI
	default: // auto and blank
		switch {
		case decisionOpenAI:
			return EndpointModePassthrough, true
		case decision:
			return EndpointModeTranslate, true
		}
		return "", false
	}
}

// EffectiveSystemOneRoute is EffectiveDecisionsRoute's twin for POST
// /v1/systemone (ADR-100): the concrete route from the effective systemone mode
// and the mapping's two verdicts, decision (System One yes) and decisionOpenAI
// (native /v1/decisions yes). A blank mode behaves as auto.
//
//   - disabled: (disabled, true); the caller answers the endpoint-disabled error.
//   - passthrough: passthrough, ok only with the decision verdict.
//   - translate: translate (System One in, the OpenAI Decisions upstream), ok
//     only with the decisionOpenAI verdict on a DecisionsPassthroughCapableKind.
//   - auto: passthrough when the mapping answers System One, else translate when
//     it answers /v1/decisions on a capable kind, else no route.
//
// kind is the effective upstream kind, as for EffectiveDecisionsRoute. For a
// kind that is not DecisionsPassthroughCapableKind a decisionOpenAI verdict is
// ignored, so a stale translate row there (the save guards refuse it) is not ok
// rather than sent to an upstream that cannot parse it.
func EffectiveSystemOneRoute(kind string, mode EndpointMode, decision, decisionOpenAI bool) (route EndpointMode, ok bool) {
	if !DecisionsPassthroughCapableKind(kind) {
		decisionOpenAI = false
	}
	switch mode {
	case EndpointModeDisabled:
		return EndpointModeDisabled, true
	case EndpointModePassthrough:
		return EndpointModePassthrough, decision
	case EndpointModeTranslate:
		return EndpointModeTranslate, decisionOpenAI
	default: // auto and blank
		switch {
		case decision:
			return EndpointModePassthrough, true
		case decisionOpenAI:
			return EndpointModeTranslate, true
		}
		return "", false
	}
}
