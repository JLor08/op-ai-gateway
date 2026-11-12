// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "slices"

// EffectiveSpecFields is the part of a mapping's serving contract that a
// server_agent runtime spec takes over from its application: the API flavors,
// the two coding-agent endpoint modes, and the live-timings flag that
// qualifies ResponsesMode. EffectiveFields resolves it, and dispatch
// (Resolver.targetFrom) and the model listings both read it from there.
type EffectiveSpecFields struct {
	APIFlavors                  []string
	ResponsesMode               EndpointMode
	MessagesMode                EndpointMode
	DecisionsMode               EndpointMode
	SystemOneMode               EndpointMode
	ResponsesLiveTimingsEnabled bool
}

// EffectiveFields resolves a mapping's effective flavors, endpoint modes and
// live-timings flag from its application, its runtime spec, and whether a spec
// row exists (hasSpec).
//
// For a server_agent application with a spec row the spec is the sole
// authority for its model, and all four fields come from it: a spec stored
// with no flavors counts as a spec, and spec.Enabled plays no part. In every
// other case -- a server_agent mapping without a spec, or an ordinary
// application, whose spec is never applied even when a caller hands one in --
// all four come from the application. The four always come from one row:
// ResponsesLiveTimingsEnabled qualifies ResponsesMode, so it is read from
// whichever row said what ResponsesMode says.
//
// APIFlavors is the source row's own slice, not a copy.
func EffectiveFields(app Application, spec RuntimeSpec, hasSpec bool) EffectiveSpecFields {
	if app.Type == ProviderServerAgent && hasSpec {
		return EffectiveSpecFields{
			APIFlavors:                  spec.APIFlavors,
			ResponsesMode:               spec.ResponsesMode,
			MessagesMode:                spec.MessagesMode,
			DecisionsMode:               spec.DecisionsMode,
			SystemOneMode:               spec.SystemOneMode,
			ResponsesLiveTimingsEnabled: spec.ResponsesLiveTimingsEnabled,
		}
	}
	return EffectiveSpecFields{
		APIFlavors:                  app.APIFlavors,
		ResponsesMode:               app.ResponsesMode,
		MessagesMode:                app.MessagesMode,
		DecisionsMode:               app.DecisionsMode,
		SystemOneMode:               app.SystemOneMode,
		ResponsesLiveTimingsEnabled: app.ResponsesLiveTimingsEnabled,
	}
}

// FlavorsAreImagesOnly reports whether a flavor list names openai_images and
// neither text flavor (openai, anthropic). Naming openai_images is also what
// makes the list non-empty, so an empty list is never images-only.
//
// It is the text translate dispatch's one spec-flavor refusal (the gateway's
// targetIsImagesOnly, on a resolved Target's flavors), the test the background
// jobs that send a mapping a chat prompt of their own apply to its effective
// flavors, and, with its speech twin FlavorsAreAudioOnly, the effective half of
// the openai row of MappingHasAPIFlavor and MappingServesAPIFlavor.
func FlavorsAreImagesOnly(flavors []string) bool {
	images := false
	for _, f := range flavors {
		switch f {
		case APIFlavorOpenAI, APIFlavorAnthropic:
			return false
		case APIFlavorOpenAIImages:
			images = true
		}
	}
	return images
}

// FlavorsAreAudioOnly is FlavorsAreImagesOnly's speech twin: it reports whether
// a flavor list names openai_audio and neither text flavor (openai, anthropic).
// Naming openai_audio is also what makes the list non-empty, so an empty list
// is never audio-only. It is the test that keeps an audio-only server from
// being offered a chat prompt, as FlavorsAreImagesOnly keeps an images-only one
// from being: the effective half of the openai row of MappingHasAPIFlavor and
// MappingServesAPIFlavor, and the test the background jobs that send a mapping
// a chat prompt of their own (the model warmer, a benchmark's skips and Load)
// apply to its effective flavors. A list naming both openai_images and
// openai_audio is both images-only and audio-only.
func FlavorsAreAudioOnly(flavors []string) bool {
	audio := false
	for _, f := range flavors {
		switch f {
		case APIFlavorOpenAI, APIFlavorAnthropic:
			return false
		case APIFlavorOpenAIAudio:
			audio = true
		}
	}
	return audio
}

// FlavorsAreDecisionsOnly is FlavorsAreAudioOnly's decision twin: it reports
// whether a flavor list names decisions and neither text flavor (openai,
// anthropic). An empty list is never decisions-only. It keeps a decisions-only
// server from being offered a chat prompt: the effective half of the openai row
// of MappingHasAPIFlavor and MappingServesAPIFlavor, and the test the background
// jobs that send a mapping a chat prompt apply to its effective flavors.
func FlavorsAreDecisionsOnly(flavors []string) bool {
	decisions := false
	for _, f := range flavors {
		switch f {
		case APIFlavorOpenAI, APIFlavorAnthropic:
			return false
		case APIFlavorDecisions:
			decisions = true
		}
	}
	return decisions
}

// MappingHasAPIFlavor is the FLAVOR HALF of the rule the model listings apply
// to one mapping: whether a request of the coarse flavor gets past both flavor
// checks on its way to the mapping, before any endpoint mode is read. app is
// the mapping's application and eff its EffectiveFields.
//
//   - openai: the application declares it (applicationHasAPIFlavor, which text
//     candidacy admits on), and eff's flavors are neither images-only nor
//     audio-only: a server that can only generate images or audio does not
//     serve chat completions. So a spec narrowed to [anthropic], or stored as
//     [], keeps openai: chat completions serve it.
//   - anthropic, openai_images, openai_audio: the application declares it, and
//     eff's flavors list it, the check /v1/messages and the images relay make
//     after resolve (the gateway's targetServesFlavor).
//
// Any other value, a fine flavor included, is false. For an ordinary
// application eff's flavors are the application's own, so the rule is exactly
// applicationHasAPIFlavor for the four coarse flavors.
func MappingHasAPIFlavor(app Application, eff EffectiveSpecFields, flavor string) bool {
	if _, known := listedEndpoint(flavor); !known {
		return false
	}
	return applicationHasAPIFlavor(app, flavor) && effectiveHasAPIFlavor(eff, flavor)
}

// MappingServesAPIFlavor is the SERVED rule the model listings apply to one
// mapping: MappingHasAPIFlavor with its application half asked of candidacy's
// own endpoint predicate, applicationServesEndpoint, for the endpoint the
// flavor stands for (listedEndpoint), and for anthropic eff's messages mode not
// disabled, the mode /v1/messages reads after resolve. For an ordinary
// application applicationServesEndpoint already applies its messages mode;
// for a server_agent application it checks the coarse flavor only, and eff
// completes the rule.
//
// decisions stands for TWO endpoints, POST /v1/systemone and POST
// /v1/decisions, so a listing's decisions flavor means "at least one of them
// is served": the flavor half, and eff's systemone mode or eff's decisions
// mode not disabled (MappingServesDecisionEndpoint asks each endpoint on its
// own). It reads eff's modes for an ordinary application and a server_agent
// spec alike, so the two cannot disagree.
//
// For an ordinary application whose messages endpoint is not disabled, and
// whose two decision endpoints are not both disabled, the rule is exactly the
// application's flavors among the five coarse flavors.
func MappingServesAPIFlavor(app Application, eff EffectiveSpecFields, flavor string) bool {
	if flavor == APIFlavorDecisions {
		return MappingServesDecisionEndpoint(app, eff, APIFlavorDecisions) ||
			MappingServesDecisionEndpoint(app, eff, APIFlavorOpenAIDecisions)
	}
	endpoint, known := listedEndpoint(flavor)
	if !known || !applicationServesEndpoint(app, endpoint) || !effectiveHasAPIFlavor(eff, flavor) {
		return false
	}
	return flavor != APIFlavorAnthropic || eff.MessagesMode != EndpointModeDisabled
}

// MappingServesDecisionEndpoint is the served rule for ONE decision endpoint,
// named by its request flavor: decisions for POST /v1/systemone, judged by
// eff's systemone mode, and openai_decisions for POST /v1/decisions, judged by
// eff's decisions mode. Both also need the decisions flavor half
// (MappingHasAPIFlavor). eff's modes are the spec's for a server_agent mapping
// with a spec row and the application's otherwise, the modes the two handlers
// read after resolve. Any other flavor is false.
//
// The unknown-model redirect's ModelOffering.Callable and the Decision flag
// GET /v1/systemone/models lists by ask it per endpoint; MappingServesAPIFlavor
// ORs the two for the listings' decisions flavor.
func MappingServesDecisionEndpoint(app Application, eff EffectiveSpecFields, requestFlavor string) bool {
	var mode EndpointMode
	switch requestFlavor {
	case APIFlavorDecisions:
		// Judged by the System One route with both verdicts assumed yes: the
		// listing has no verdicts here, so this is "some route can exist",
		// which excludes disabled and a translate row on a kind that has no
		// /v1/decisions to translate to (the per-model fold judges the verdicts).
		if route, ok := EffectiveSystemOneRoute(app.Type, eff.SystemOneMode, true, true); !ok || route == EndpointModeDisabled {
			return false
		}
		return MappingHasAPIFlavor(app, eff, APIFlavorDecisions)
	case APIFlavorOpenAIDecisions:
		mode = eff.DecisionsMode
	default:
		return false
	}
	return mode != EndpointModeDisabled && MappingHasAPIFlavor(app, eff, APIFlavorDecisions)
}

// listedEndpoint is the fine flavor a listed coarse flavor stands for, the one
// MappingServesAPIFlavor asks applicationServesEndpoint about: chat
// completions for openai (never openai_responses, whose mode no listing
// reflects), the Messages endpoint for anthropic, and openai_images and
// openai_audio themselves. decisions is known too, for MappingHasAPIFlavor,
// but MappingServesAPIFlavor answers it from the two decision endpoints'
// modes instead (MappingServesDecisionEndpoint). known is false for any other
// value.
func listedEndpoint(flavor string) (endpoint string, known bool) {
	switch flavor {
	case APIFlavorOpenAI:
		return "openai_chat_completions", true
	case APIFlavorAnthropic:
		return "anthropic_messages", true
	case APIFlavorOpenAIImages:
		return APIFlavorOpenAIImages, true
	case APIFlavorOpenAIAudio:
		return APIFlavorOpenAIAudio, true
	case APIFlavorDecisions:
		return APIFlavorDecisions, true
	default:
		return "", false
	}
}

// effectiveHasAPIFlavor is the half of both mapping rules that eff answers:
// for openai that its flavors are neither images-only, audio-only nor decisions-only (a server
// that can only generate images, audio or decisions answers no chat prompt), for any
// other flavor that they list it.
func effectiveHasAPIFlavor(eff EffectiveSpecFields, flavor string) bool {
	if flavor == APIFlavorOpenAI {
		return !FlavorsAreImagesOnly(eff.APIFlavors) && !FlavorsAreAudioOnly(eff.APIFlavors) && !FlavorsAreDecisionsOnly(eff.APIFlavors)
	}
	return slices.Contains(eff.APIFlavors, flavor)
}
