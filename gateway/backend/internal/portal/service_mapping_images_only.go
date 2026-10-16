// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/routing"
)

// mappingFlavorMarkers is the pair of advisory markers ModelMappingDTO carries
// about a mapping's effective flavors: ImagesOnly (routing.FlavorsAreImagesOnly)
// and AudioOnly (routing.FlavorsAreAudioOnly). They come out of ONE spec read,
// which is why they travel together: reading a server_agent application's specs
// once per marker would double the cost of every mapping listing. The zero value
// is "neither", what a failed read reports.
type mappingFlavorMarkers struct {
	ImagesOnly bool
	AudioOnly  bool
}

// mappingsFlavorMarkers returns ModelMappingDTO.ImagesOnly and AudioOnly for
// each of app's mappings, keyed by mapping id. A server_agent application costs
// one RuntimeSpecsByApplication read for the whole listing; any other
// application needs none, because its mappings' flavors are its own.
//
// A failed read reports false for both markers of every mapping of the
// application and is logged. The markers are advisory; the gateway's own check
// fails closed.
func (s *Service) mappingsFlavorMarkers(ctx context.Context, app routing.Application, mappings []routing.ModelMapping) map[string]mappingFlavorMarkers {
	specs := map[string]routing.RuntimeSpec{}
	if app.Type == routing.ProviderServerAgent {
		rows, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			slog.Warn("portal: runtime spec read failed; images_only and audio_only are reported false for the application's mappings",
				"app_id", app.ID, "err", err)
			return map[string]mappingFlavorMarkers{}
		}
		for _, spec := range rows {
			specs[spec.MappingID] = spec
		}
	}
	out := make(map[string]mappingFlavorMarkers, len(mappings))
	for _, mapping := range mappings {
		spec, hasSpec := specs[mapping.ID]
		out[mapping.ID] = effectiveFlavorMarkers(app, spec, hasSpec)
	}
	return out
}

// mappingFlavorMarkersFor is mappingsFlavorMarkers for one mapping, with one
// RuntimeSpecByMapping read for a server_agent application and none for any
// other. A failed read reports false for both markers and is logged.
func (s *Service) mappingFlavorMarkersFor(ctx context.Context, app routing.Application, mappingID string) mappingFlavorMarkers {
	if app.Type != routing.ProviderServerAgent {
		return effectiveFlavorMarkers(app, routing.RuntimeSpec{}, false)
	}
	spec, hasSpec, err := s.routes.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil {
		slog.Warn("portal: runtime spec read failed; images_only and audio_only are reported false for the mapping",
			"mapping_id", mappingID, "err", err)
		return mappingFlavorMarkers{}
	}
	return effectiveFlavorMarkers(app, spec, hasSpec)
}

// effectiveFlavorMarkers is both markers on a mapping's effective flavors
// (routing.EffectiveFields): the spec's for a server_agent mapping that has
// one, the application's otherwise.
func effectiveFlavorMarkers(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) mappingFlavorMarkers {
	return mappingFlavorMarkers{
		ImagesOnly: effectiveFlavorsAreImagesOnly(app, spec, hasSpec),
		AudioOnly:  effectiveFlavorsAreAudioOnly(app, spec, hasSpec),
	}
}

// effectiveFlavorsAreImagesOnly is the images-only rule on a mapping's
// effective flavors (routing.EffectiveFields): the spec's for a server_agent
// mapping that has one, the application's otherwise. It is the rule the
// gateway refuses a chat prompt by.
func effectiveFlavorsAreImagesOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreImagesOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}

// effectiveFlavorsAreAudioOnly is effectiveFlavorsAreImagesOnly's speech twin:
// the audio-only rule (routing.FlavorsAreAudioOnly) on the same effective
// flavors, and the rule the gateway refuses a chat prompt to an audio-only
// mapping by.
func effectiveFlavorsAreAudioOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreAudioOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}
