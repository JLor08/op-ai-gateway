// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sort"
)

// ImageOptionMapping is one sd-server offering mapping of a gateway model that the
// principal may see: the identity a caller outside package portal needs to aim an
// image-options probe at it, or to look its live status up, and to tell an external
// sd-server from an agent-launched one. It is the exported projection of the internal
// mappingView (whose fields are unexported) plus the runtime-spec identity of an
// agent-launched mapping; it is a handoff value, not a wire DTO, so it carries no
// json tags.
type ImageOptionMapping struct {
	ServerID      string
	ServerName    string
	ApplicationID string
	// AppType is the application's type: routing.ProviderStableDiffusionCpp for an
	// external sd-server, routing.ProviderServerAgent for an agent-launched one.
	AppType   string
	MappingID string
	// SpecID and SpecType are the mapping's runtime spec id and EFFECTIVE type
	// (routing.EffectiveRuntimeSpecType: always "stable_diffusion_cpp" here) for an
	// agent-launched mapping, and "" for an external stable_diffusion_cpp application,
	// which has no spec.
	SpecID   string
	SpecType string
}

// ModelImageOptionViews returns the sd-server offering mappings of gatewayModelName
// that principal may see: the visibility gate that runs BEFORE anything probes a
// server for its image options, so a probe is never aimed at (and its answer never
// reveals) a mapping the caller cannot route to.
//
// Visibility is exactly ModelServers': a non-admin gets an empty slice for a model
// whose model_settings.visibility is "hidden" or "locked" (modelServersSuppressed,
// fail-CLOSED on a settings read error), and only the mappings on servers the
// principal may USE under resource-group provisioning survive
// (filterVisibleMappingViews, the same AllowedServerIDs filter, failOpen=false: an
// error propagates). An admin bypasses the suppression. Both gates run before the
// sd-server classification, so the runtime-spec reads below touch only mappings the
// caller is already allowed to see.
//
// Of those, only an sd-server mapping is kept: an external application of type
// stable_diffusion_cpp, or a server_agent mapping whose runtime spec's EFFECTIVE type
// (routing.EffectiveRuntimeSpecType: the explicit type, else detected from the
// launched binary) is stable_diffusion_cpp. Every other mapping offering the same
// name (a plain openai application, an agent launching another server kind) is
// dropped. A server_agent mapping without a spec, or whose application's spec read
// failed (logged by runtimeSpecIndexForViews), cannot be shown to be an sd-server
// and is dropped too.
//
// Each survivor is projected to an ImageOptionMapping (the internal mappingView is
// not usable outside this package), ordered by server name, then mapping id, as
// ModelServers orders its rows. An unknown name, or a name with no visible sd-server
// mapping, is an empty slice and not an error.
func (s *Service) ModelImageOptionViews(ctx context.Context, principal auth.Token, gatewayModelName string) ([]ImageOptionMapping, error) {
	suppressed, err := s.modelServersSuppressed(ctx, principal, gatewayModelName)
	if err != nil {
		return nil, err
	}
	if suppressed {
		return []ImageOptionMapping{}, nil
	}
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return nil, err
	}
	offering, _ := offeringMappingViews(views, gatewayModelName)
	visible, err := s.filterVisibleMappingViews(ctx, principal, offering)
	if err != nil {
		return nil, err
	}

	idx := s.runtimeSpecIndexForViews(ctx, visible)
	out := make([]ImageOptionMapping, 0, len(visible))
	for _, view := range visible {
		if m, ok := imageOptionMappingFor(view, idx); ok {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ServerName != out[j].ServerName {
			return out[i].ServerName < out[j].ServerName
		}
		return out[i].MappingID < out[j].MappingID
	})
	return out, nil
}

// imageOptionMappingFor projects view to an ImageOptionMapping when its mapping is
// served by a stable-diffusion.cpp sd-server, and reports false otherwise: an external
// application of that type (no spec identity), or a server_agent mapping whose
// effective runtime-spec type is stable_diffusion_cpp (carrying that spec's id and
// type). idx must cover view. A server_agent mapping with no spec row, including every
// mapping of an application whose spec read failed (idx holds no spec for it), has no
// effective type and is not an sd-server here.
func imageOptionMappingFor(view mappingView, idx runtimeSpecIndex) (ImageOptionMapping, bool) {
	m := ImageOptionMapping{
		ServerID:      view.server.ID,
		ServerName:    view.server.Name,
		ApplicationID: view.app.ID,
		AppType:       view.app.Type,
		MappingID:     view.mapping.ID,
	}
	switch view.app.Type {
	case routing.ProviderStableDiffusionCpp:
		return m, true
	case routing.ProviderServerAgent:
		spec, hasSpec := idx.specs[view.mapping.ID]
		if !hasSpec {
			return ImageOptionMapping{}, false
		}
		specType := routing.EffectiveRuntimeSpecType(spec)
		if specType != routing.RuntimeSpecTypeStableDiffusionCpp {
			return ImageOptionMapping{}, false
		}
		m.SpecID = spec.ID
		m.SpecType = string(specType)
		return m, true
	default:
		return ImageOptionMapping{}, false
	}
}
