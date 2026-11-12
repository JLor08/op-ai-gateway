// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

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
// ok == false means the mapping has no verdict for the route; the handler
// answers routing.model_not_capable.
func EffectiveDecisionsRoute(mode EndpointMode, decision, decisionOpenAI bool) (route EndpointMode, ok bool) {
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
