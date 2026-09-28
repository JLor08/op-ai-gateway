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
			ResponsesLiveTimingsEnabled: spec.ResponsesLiveTimingsEnabled,
		}
	}
	return EffectiveSpecFields{
		APIFlavors:                  app.APIFlavors,
		ResponsesMode:               app.ResponsesMode,
		MessagesMode:                app.MessagesMode,
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
// flavors, and the effective half of the openai row of MappingHasAPIFlavor and
// MappingServesAPIFlavor.
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

// MappingHasAPIFlavor is the FLAVOR HALF of the rule the model listings apply
// to one mapping: whether a request of the coarse flavor gets past both flavor
// checks on its way to the mapping, before any endpoint mode is read. app is
// the mapping's application and eff its EffectiveFields.
//
//   - openai: the application declares it (applicationHasAPIFlavor, which text
//     candidacy admits on), and eff's flavors are not images-only, the one
//     spec-flavor refusal of the text translate dispatch. So a spec narrowed
//     to [anthropic], or stored as [], keeps openai: chat completions serve it.
//   - anthropic, openai_images: the application declares it, and eff's flavors
//     list it, the check /v1/messages and the images relay make after resolve
//     (the gateway's targetServesFlavor).
//
// Any other value, a fine flavor included, is false. For an ordinary
// application eff's flavors are the application's own, so the rule is exactly
// applicationHasAPIFlavor for the three coarse flavors.
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
// For an ordinary application whose messages endpoint is not disabled the rule
// is exactly the application's flavors among the three coarse flavors.
func MappingServesAPIFlavor(app Application, eff EffectiveSpecFields, flavor string) bool {
	endpoint, known := listedEndpoint(flavor)
	if !known || !applicationServesEndpoint(app, endpoint) || !effectiveHasAPIFlavor(eff, flavor) {
		return false
	}
	return flavor != APIFlavorAnthropic || eff.MessagesMode != EndpointModeDisabled
}

// listedEndpoint is the fine flavor a listed coarse flavor stands for, the one
// MappingServesAPIFlavor asks applicationServesEndpoint about: chat
// completions for openai (never openai_responses, whose mode no listing
// reflects), the Messages endpoint for anthropic, and openai_images itself.
// known is false for any other value.
func listedEndpoint(flavor string) (endpoint string, known bool) {
	switch flavor {
	case APIFlavorOpenAI:
		return "openai_chat_completions", true
	case APIFlavorAnthropic:
		return "anthropic_messages", true
	case APIFlavorOpenAIImages:
		return APIFlavorOpenAIImages, true
	default:
		return "", false
	}
}

// effectiveHasAPIFlavor is the half of both mapping rules that eff answers:
// for openai that its flavors are not images-only, for any other flavor that
// they list it.
func effectiveHasAPIFlavor(eff EffectiveSpecFields, flavor string) bool {
	if flavor == APIFlavorOpenAI {
		return !FlavorsAreImagesOnly(eff.APIFlavors)
	}
	return slices.Contains(eff.APIFlavors, flavor)
}
