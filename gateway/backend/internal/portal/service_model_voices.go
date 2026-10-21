// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
)

// ModelVoiceMappingIDs returns the ids of the mappings whose cached voices the
// portal's voice picker may show principal for gatewayModelName: the visibility
// gate of GET /api/portal/model-voices, which runs BEFORE anything is read from
// the voices cache, so a name the portal hides (or a server it keeps from the
// caller) never reaches the cache and its answer reveals nothing. The caller (the
// gateway handler) hands exactly these ids to the shared union core, which owns
// everything that does not depend on who is asking: the current tts verdict, the
// cache read, the dedupe and the sort. This method decides WHICH mappings may be
// looked at, never what voices they hold.
//
// The gate is the portal's, not routing's (that is the public GET
// /v1/audio/voices, which asks the resolver what a speech dispatch could reach):
//
//   - a non-admin gets nothing for a name whose model_settings.visibility is
//     "hidden" or "locked" (modelServersSuppressed, fail-CLOSED on a settings read
//     error, so a store blip is an error and never a leaked mapping). An admin
//     bypasses it, as everywhere else in the portal;
//   - only mappings on servers the principal may USE under resource-group
//     provisioning survive (filterVisibleMappingViews, failOpen=false: an error
//     propagates);
//   - only mappings a speech request could be routed to count (viewServesAudio,
//     the listings' own openai_audio rule, fail-closed on an unreadable runtime
//     spec), so an application that does not declare speech offers no voices;
//   - only ACTIVE mappings of active applications on selectable servers count
//     (activeMappingViews).
//
// A group name expands to its leaf members, nested subgroups flattened by their
// own traversal (routing.FlattenGroup), and the ids are those of ALL the visible
// members: the voices a group can offer are the UNION of what its members offer.
// Visibility applies at both levels, as the portal's group server listing
// (handlePortalModelGroupServers, which asks ModelServers per member) applies
// it: the group's own hidden/locked setting suppresses the whole group for a
// non-admin, and a hidden or locked MEMBER is left out of an otherwise visible
// group, since the portal does not show that model's servers either. An inactive
// group is not resolvable and offers nothing.
//
// The ids are deduped and sorted. An unknown or blank name, or one with no
// visible speech mapping, is an empty non-nil slice and not an error.
func (s *Service) ModelVoiceMappingIDs(ctx context.Context, principal auth.Token, gatewayModelName string) ([]string, error) {
	ids := []string{}
	suppressed, err := s.modelServersSuppressed(ctx, principal, gatewayModelName)
	if err != nil {
		return nil, err
	}
	if suppressed {
		return ids, nil
	}
	names, err := s.voiceModelNames(ctx, principal, gatewayModelName)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return ids, nil
	}
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return nil, err
	}
	var offering []mappingView
	for _, name := range names {
		matched, _ := offeringMappingViews(views, name)
		offering = append(offering, matched...)
	}
	visible, err := s.filterVisibleMappingViews(ctx, principal, offering)
	if err != nil {
		return nil, err
	}
	idx := s.runtimeSpecIndexForViews(ctx, visible)
	for _, view := range visible {
		if viewServesAudio(view, idx) {
			ids = append(ids, view.mapping.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// voiceModelNames returns the gateway model names whose mappings may offer
// voices for gatewayModelName: the name itself when it is a model, or, when it
// names a model group, the leaf members of that group that principal may see (a
// hidden or locked member of a visible group is dropped for a non-admin, exactly
// as ModelServers drops it, member by member, in the portal's group listing).
//
// gatewayModelName's own visibility is NOT checked here: the caller has already
// done it for the name it was asked about, group or model.
func (s *Service) voiceModelNames(ctx context.Context, principal auth.Token, gatewayModelName string) ([]string, error) {
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(strings.TrimSpace(gatewayModelName))
	isGroup := slices.ContainsFunc(groups, func(g routing.ModelGroup) bool {
		return strings.ToLower(strings.TrimSpace(g.GatewayModelName)) == key
	})
	if !isGroup {
		return []string{gatewayModelName}, nil
	}
	graph, err := s.groupFlattenGraph(ctx, groups)
	if err != nil {
		return nil, err
	}
	members := routing.FlattenGroup(gatewayModelName, graph)
	names := make([]string, 0, len(members))
	for _, member := range members {
		memberSuppressed, err := s.modelServersSuppressed(ctx, principal, member)
		if err != nil {
			return nil, err
		}
		if !memberSuppressed {
			names = append(names, member)
		}
	}
	return names, nil
}
