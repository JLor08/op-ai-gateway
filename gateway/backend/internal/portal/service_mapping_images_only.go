// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/routing"
)

// mappingsImagesOnly returns ModelMappingDTO.ImagesOnly for each of app's
// mappings, keyed by mapping id. A server_agent application costs one
// RuntimeSpecsByApplication read for the whole listing; any other application
// needs none, because its mappings' flavors are its own.
//
// A failed read reports false for every mapping of the application and is
// logged. The marker is advisory; the gateway's own check fails closed.
func (s *Service) mappingsImagesOnly(ctx context.Context, app routing.Application, mappings []routing.ModelMapping) map[string]bool {
	specs := map[string]routing.RuntimeSpec{}
	if app.Type == routing.ProviderServerAgent {
		rows, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
		if err != nil {
			slog.Warn("portal: runtime spec read failed; images_only is reported false for the application's mappings",
				"app_id", app.ID, "err", err)
			return map[string]bool{}
		}
		for _, spec := range rows {
			specs[spec.MappingID] = spec
		}
	}
	out := make(map[string]bool, len(mappings))
	for _, mapping := range mappings {
		spec, hasSpec := specs[mapping.ID]
		out[mapping.ID] = effectiveFlavorsAreImagesOnly(app, spec, hasSpec)
	}
	return out
}

// mappingImagesOnly is mappingsImagesOnly for one mapping, with one
// RuntimeSpecByMapping read for a server_agent application and none for any
// other. A failed read reports false and is logged.
func (s *Service) mappingImagesOnly(ctx context.Context, app routing.Application, mappingID string) bool {
	if app.Type != routing.ProviderServerAgent {
		return effectiveFlavorsAreImagesOnly(app, routing.RuntimeSpec{}, false)
	}
	spec, hasSpec, err := s.routes.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil {
		slog.Warn("portal: runtime spec read failed; images_only is reported false for the mapping",
			"mapping_id", mappingID, "err", err)
		return false
	}
	return effectiveFlavorsAreImagesOnly(app, spec, hasSpec)
}

// effectiveFlavorsAreImagesOnly is the images-only rule on a mapping's
// effective flavors (routing.EffectiveFields): the spec's for a server_agent
// mapping that has one, the application's otherwise. It is the rule the
// gateway refuses a chat prompt by.
func effectiveFlavorsAreImagesOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreImagesOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}
