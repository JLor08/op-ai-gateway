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
// The gate is visibleAudioMappingIDs', the one the transcription twin
// (ModelTranscriptionMappingIDs) asks as well. It is the portal's, not routing's
// (that is the public GET /v1/audio/voices, which asks the resolver what a speech
// dispatch could reach).
func (s *Service) ModelVoiceMappingIDs(ctx context.Context, principal auth.Token, gatewayModelName string) ([]string, error) {
	return s.visibleAudioMappingIDs(ctx, principal, gatewayModelName)
}

// ModelTranscriptionMappingIDs returns the ids of the mappings the portal may offer
// principal for a TRANSCRIPTION of gatewayModelName: the visibility gate of GET
// /api/portal/model-transcription-streaming, which runs before anything is read of a
// mapping's verdicts, so a name the portal hides (or a server it keeps from the caller)
// answers exactly as an unknown one does. It is the twin of ModelVoiceMappingIDs and the
// SAME gate (visibleAudioMappingIDs): a transcription request shares the openai_audio
// flavor with speech (viewServesAudio), and the portal's hidden/locked, group and
// resource-group rules do not depend on what the model does with the audio.
//
// Like its twin it decides WHICH mappings may be looked at and nothing else: the verdicts
// (which of them transcribe, and stream) are the gateway's to read, as the voice picker's
// narrowing to plain tts is. The ids are deduped and sorted; an unknown or blank name, or
// one with no visible audio mapping, is an empty non-nil slice and not an error.
func (s *Service) ModelTranscriptionMappingIDs(ctx context.Context, principal auth.Token, gatewayModelName string) ([]string, error) {
	return s.visibleAudioMappingIDs(ctx, principal, gatewayModelName)
}

// visibleAudioMappingIDs is the portal's visibility gate for the audio endpoints'
// pickers and gates, shared by ModelVoiceMappingIDs and ModelTranscriptionMappingIDs:
//
//   - a non-admin gets nothing for a name whose model_settings.visibility is
//     "hidden" or "locked" (modelServersSuppressed, fail-CLOSED on a settings read
//     error, so a store blip is an error and never a leaked mapping). An admin
//     bypasses it, as everywhere else in the portal;
//   - only mappings on servers the principal may USE under resource-group
//     provisioning survive (filterVisibleMappingViews, failOpen=false: an error
//     propagates);
//   - only mappings a speech or a transcription request could be routed to count
//     (viewServesAudio, the listings' own openai_audio rule, which both share,
//     fail-closed on an unreadable runtime spec), so an application that does not
//     declare openai_audio offers neither voices nor a transcription;
//   - only ACTIVE mappings of active applications on selectable servers count
//     (activeMappingViews).
//
// A group name expands to its leaf members, nested subgroups flattened by their
// own traversal (routing.FlattenGroup), and the ids are those of ALL the members'
// mappings that survive provisioning: what a group can offer is the UNION of what
// its members offer. The hidden/locked gate applies to the name that was
// ASKED ABOUT only, the group's own name included (the first bullet above): a
// member's own visibility does NOT narrow a visible group, because a hidden or
// locked model is still a full member of it (modelGroupOverlay), and "locked"
// means reachable through a group but not directly. Leaving a locked member out
// would empty the answer for the main locked-via-group setup, and no access
// boundary is crossed by listing it: a group dispatch may route to the member,
// and provisioning still filters its servers. An inactive group is not
// resolvable and offers nothing.
//
// The ids are deduped and sorted. An unknown or blank name, or one with no
// visible audio mapping, is an empty non-nil slice and not an error.
func (s *Service) visibleAudioMappingIDs(ctx context.Context, principal auth.Token, gatewayModelName string) ([]string, error) {
	ids := []string{}
	suppressed, err := s.modelServersSuppressed(ctx, principal, gatewayModelName)
	if err != nil {
		return nil, err
	}
	if suppressed {
		return ids, nil
	}
	names, err := s.voiceModelNames(ctx, gatewayModelName)
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
// names a model group, ALL of that group's leaf members, in flatten order. No
// member is filtered by its own hidden/locked setting (a hidden or locked model is
// still a full member of a group, see ModelVoiceMappingIDs).
//
// Visibility is NOT checked here at all: the caller has already checked the name
// it was asked about, group or model, and provisioning is applied to the mappings
// afterwards.
func (s *Service) voiceModelNames(ctx context.Context, gatewayModelName string) ([]string, error) {
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
	return routing.FlattenGroup(gatewayModelName, graph), nil
}
