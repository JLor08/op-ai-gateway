// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sort"
)

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
// The result is the internal server+app+mapping triples, not a DTO: the caller
// builds probe targets from them and tells an external application from an agent
// one by app.Type. They are ordered by server name, then mapping id, as ModelServers
// orders its rows. An unknown name, or a name with no visible sd-server mapping, is
// an empty slice and not an error.
func (s *Service) ModelImageOptionViews(ctx context.Context, principal auth.Token, gatewayModelName string) ([]mappingView, error) {
	suppressed, err := s.modelServersSuppressed(ctx, principal, gatewayModelName)
	if err != nil {
		return nil, err
	}
	if suppressed {
		return []mappingView{}, nil
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
	out := make([]mappingView, 0, len(visible))
	for _, view := range visible {
		if isSDServerView(view, idx) {
			out = append(out, view)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].server.Name != out[j].server.Name {
			return out[i].server.Name < out[j].server.Name
		}
		return out[i].mapping.ID < out[j].mapping.ID
	})
	return out, nil
}

// isSDServerView reports whether view's mapping is served by a stable-diffusion.cpp
// sd-server: an external application of that type, or a server_agent mapping whose
// effective runtime-spec type is stable_diffusion_cpp. idx must cover view. A
// server_agent mapping with no spec row, including every mapping of an application
// whose spec read failed (idx holds no spec for it), has no effective type and is not
// an sd-server here.
func isSDServerView(view mappingView, idx runtimeSpecIndex) bool {
	switch view.app.Type {
	case routing.ProviderStableDiffusionCpp:
		return true
	case routing.ProviderServerAgent:
		spec, hasSpec := idx.specs[view.mapping.ID]
		return hasSpec && routing.EffectiveRuntimeSpecType(spec) == routing.RuntimeSpecTypeStableDiffusionCpp
	default:
		return false
	}
}
