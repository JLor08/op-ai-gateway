// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sort"
	"strings"
	"time"
)

// ModelServerDTO is one server that offers a gateway model: the server's identity, the mapping's
// distilled benchmark metrics, whether the model is currently loaded there, and whether the caller
// may trigger a load on it (admin or a server owner). Metric fields mirror the mapping DTO.
type ModelServerDTO struct {
	ServerID      string `json:"server_id"`
	ServerName    string `json:"server_name"`
	ApplicationID string `json:"application_id"`
	MappingID     string `json:"mapping_id"`
	Loaded        bool   `json:"loaded"`
	CanLoad       bool   `json:"can_load"`
	// Priority is the live 1-based selection rank among the servers offering this
	// model (1 = would be picked first right now). 0 = unknown/unranked; Service.
	// ModelServers always leaves this 0 — the gateway layer (which holds the
	// resolver/activity/telemetry) computes and injects it after the fact.
	Priority int `json:"priority"`

	// State is the model's live runtime lifecycle ("running"/"starting"/... or "" when
	// no agent-managed runtime status is known); ActiveRequests/QueueDepth are its live
	// per-model load. Like Priority, Service.ModelServers leaves these zero/empty — the
	// gateway layer injects them from the runtime-status registry after the fact.
	State          string `json:"state"`
	ActiveRequests int    `json:"active_requests"`
	QueueDepth     int    `json:"queue_depth"`

	// MetricsProbe/ContextProbe are the agent's last-reported reachability for its
	// /metrics and context probes ("ok"/"unreachable"/"na", or "" when not reported).
	// Like ActiveRequests/QueueDepth, Service.ModelServers leaves these "" — the gateway
	// layer injects them from the runtime-status registry after the fact.
	MetricsProbe string `json:"metrics_probe"`
	ContextProbe string `json:"context_probe"`

	GenTokensPerSecond           float64 `json:"gen_tokens_per_second"`
	PromptTokensPerSecond        float64 `json:"prompt_tokens_per_second"`
	LoadTimeMS                   int     `json:"load_time_ms"`
	ContextSize                  int     `json:"context_size"`
	MaxConcurrency               int     `json:"max_concurrency"`
	RecommendedConcurrency       int     `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64 `json:"gen_tokens_per_second_at_capacity"`
	// IsMtp/VisionCapable are convenience booleans folded from Capabilities
	// below (the "mtp"/"vision" row's Verdict == CapabilityYes; a missing row
	// is NOT capable, same fail-closed rule Capabilities itself carries) --
	// NOT read off ModelMapping.IsMTP/VisionCapable any more, which is why
	// this fill needs the same capability-rows batch Capabilities does. Kept
	// as dedicated booleans (rather than making every caller re-derive them
	// from Capabilities) because GroupServersSection's "mtp"/vision columns
	// mirror this DTO and already key off these two fields.
	IsMtp            bool       `json:"is_mtp"`
	VisionCapable    bool       `json:"vision_capable"`
	MetricsSource    string     `json:"metrics_source"`
	MetricsUpdatedAt *time.Time `json:"metrics_updated_at,omitempty"`

	// LiveProgressSupport is the PERSISTED verdict on whether this upstream
	// tolerates the live-progress two-parameter request (#51): "supported" /
	// "unsupported" / "" (never determined). Folded from the "live_progress"
	// row in the SAME Capabilities batch below (routing.
	// LiveProgressSupportFromVerdict). It is the ONLY source now: #49-3 moved
	// the background detectors onto model_mapping_capabilities rows, and
	// migration 79 then dropped the live_progress_support column they had
	// stopped writing -- which is why a mapping carries no such field to read
	// by mistake. Unlike State/ActiveRequests/QueueDepth/MetricsProbe/
	// ContextProbe (which Service.ModelServers leaves zero/empty for the
	// gateway layer to inject from the runtime-status registry), this one
	// needs no gateway-injection seam -- Service.ModelServers fills it itself
	// from the store. No `omitempty`: "never determined" must be an explicit
	// "" on the wire, not a missing key -- same rule the wire encoding of
	// State already follows.
	LiveProgressSupport string `json:"live_progress_support"`
	// LiveProgressCheckedAt is when that verdict was last determined (the
	// "live_progress" row's own CheckedAt); nil when never determined, and
	// nil rather than a year-0001 timestamp for a row that carries no time.
	// Diagnostic/tooltip only, mirroring ModelMapping.LiveProgressCheckedAt's
	// own doc-comment -- no decision logic may read it.
	LiveProgressCheckedAt *time.Time `json:"live_progress_checked_at,omitempty"`

	// Capabilities is every DETERMINED capability row for this mapping (#49
	// sub-project 3, the capability-table migration), one entry per
	// (mapping, capability) that has ever been established -- "vision",
	// "video", "audio", "tools", "mtp", "live_progress", and any name an
	// upstream reports that this codebase has no constant for (the vocabulary
	// is OPEN -- Ollama passes manifest-declared names straight through). A
	// capability with no row is simply ABSENT from this slice; there is no
	// "" placeholder entry the way the old cap_vision/cap_video/cap_audio/
	// cap_tools columns each needed one for "never determined" -- absence
	// itself is that state now (routing.CapabilityRow's own doc-comment).
	//
	// Filled from ONE routing.MappingCapabilitiesForMappings batch call
	// across every mapping ModelServers is about to return (see the fill
	// below) -- never a per-row MappingCapabilities call: the listing
	// already costs ~31 queries at the documented scale, and two multipliers
	// make a per-row read genuinely expensive (the SSE stream recomputes the
	// whole listing on every loaded-model-registry change, and
	// handlePortalModelGroupServers calls the full listing once per group
	// member).
	//
	// Never nil on the wire: a mapping with no rows carries an empty array,
	// not `null` -- the frontend's capabilityChips renders the shared
	// em-dash for an empty array, but a `null` would need its own
	// nil-vs-empty branch for no reason.
	Capabilities []ModelServerCapabilityDTO `json:"capabilities"`
}

// ModelServerCapabilityDTO is one routing.CapabilityRow on the wire,
// field-for-field: which capability, its verdict ("yes"/"no" -- "unknown" is
// the row's ABSENCE from ModelServerDTO.Capabilities, never a value here),
// who established it (Source: "manual" | "vision_benchmark" |
// "llama_cpp_props" | "legacy" | an unrecognised probe's own name), and when.
// Source and CheckedAt are what the per-capability tooltip renders that the
// old shared, mapping-wide CapabilitiesSource/CapabilitiesCheckedAt pair
// never could: which verdict a given chip actually rests on, not just the
// most recent probe's identity for the whole row.
type ModelServerCapabilityDTO struct {
	Capability string    `json:"capability"`
	Verdict    string    `json:"verdict"`
	Source     string    `json:"source"`
	CheckedAt  time.Time `json:"checked_at"`
}

// GroupModelServerDTO is one (model, server) a model group can serve, with the live
// selection rank across the whole group's candidate list (every flattened member's
// offering servers, ranked together — not per-model). Model is the leaf gateway model
// name this row serves (a flattened group member), distinct from any group name.
type GroupModelServerDTO struct {
	ModelServerDTO
	Model    string `json:"model"`    // the leaf gateway model name this row serves
	Priority int    `json:"priority"` // live rank across the group's candidates
}

// ModelServers returns every reachable server that offers gatewayModelName, one row per
// (server, mapping), with the mapping's distilled benchmark metrics, the live loaded-state, and a
// can_load permission flag for the principal (admin OR a server owner). gateway:use, global — the
// row set is NOT owner-filtered (mirrors Models()), but IS filtered to the servers the principal
// is allowed to USE under resource-group provisioning (Resource Groups Phase 2 — Task 4): a
// non-provisioned principal gets an empty slice for a model exclusively offered by a restricted
// server, closing the detail-view leak a raw GET of a known model name would otherwise expose.
//
// A non-admin principal ALSO gets an empty slice when gatewayModelName's model_settings.visibility
// is "hidden" or "locked" (security fix — mirrors modelsResponse(suppress=true)'s drop from
// Models(); see the VISIBILITY-SURFACE MATRIX doc-comment on visibleMappingViews in service.go). An
// admin bypasses this check entirely (the ModelServersSection management flow, same as
// ManageModels()). An unknown model resolves to an empty slice either way.
func (s *Service) ModelServers(ctx context.Context, principal auth.Token, gatewayModelName string) ([]ModelServerDTO, error) {
	admin := isAdmin(principal)
	if !admin {
		// Fails CLOSED on a ModelSettings store error: a transient read failure
		// must not silently drop the hidden/locked suppression and leak a
		// suppressed model's serving rows to a non-admin. This mirrors
		// modelGroupOverlay (which backs Models()): it propagates the same
		// modelVisibilityByLower error rather than falling back to an empty
		// suppress set, so a blip surfaces as a 500, never as a leak.
		visByLower, sErr := s.modelVisibilityByLower(ctx)
		if sErr != nil {
			return nil, sErr
		}
		if isHiddenOrLocked(visByLower[strings.ToLower(strings.TrimSpace(gatewayModelName))]) {
			return []ModelServerDTO{}, nil
		}
	}
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return nil, err
	}
	ownerCache := make(map[string]bool)
	canManage := func(serverID string) bool {
		if admin {
			return true
		}
		if v, ok := ownerCache[serverID]; ok {
			return v
		}
		v := false
		if owners, oerr := s.routes.ServerOwners(ctx, serverID); oerr == nil {
			for _, o := range owners {
				if o == principal.UserID {
					v = true
					break
				}
			}
		}
		ownerCache[serverID] = v
		return v
	}

	// Filter to the offering views FIRST, so the capability batch call below
	// (the N+1 guard: one query, period) asks for exactly the mapping ids
	// this response needs -- not every active mapping in the system.
	matched := make([]mappingView, 0)
	for _, view := range views {
		if view.mapping.GatewayModelName == gatewayModelName {
			matched = append(matched, view)
		}
	}
	mappingIDs := make([]string, len(matched))
	for i, view := range matched {
		mappingIDs[i] = view.mapping.ID
	}
	// Best-effort, exactly ONE query regardless of how many mappings offer
	// this model: a store error degrades every row's capabilities to empty
	// (fail-closed -- see ModelServerDTO.Capabilities) rather than failing
	// the whole listing, mirroring the ModelSettings best-effort read above.
	capsByMapping, capErr := s.routes.MappingCapabilitiesForMappings(ctx, mappingIDs)
	if capErr != nil {
		// LOUDLY: degrading silently here renders as a perfectly normal page
		// with every capability chip, the MTP/vision flags and the
		// live-progress column all blank -- indistinguishable from "nothing
		// has been determined yet", with no diagnostic trail at all. Every
		// sibling best-effort read in this package logs for exactly that
		// reason.
		slog.Warn("portal: model-servers capability read failed; row capabilities withheld",
			"model", gatewayModelName, "mappings", len(mappingIDs), "err", capErr)
		capsByMapping = nil
	}

	rows := make([]ModelServerDTO, 0, len(matched))
	for _, view := range matched {
		loaded := false
		if s.loadedModels != nil && view.mapping.AppModelName != "" {
			for _, m := range s.loadedModels.LoadedAppModels(view.app.ID, view.server.ID) {
				if m == view.mapping.AppModelName {
					loaded = true
					break
				}
			}
		}
		caps := capsByMapping[view.mapping.ID]
		byName := routing.CapabilityRowsByName(caps)
		liveProgressRow := byName[routing.CapabilityLiveProgress]
		rows = append(rows, ModelServerDTO{
			ServerID:                     view.server.ID,
			ServerName:                   view.server.Name,
			ApplicationID:                view.app.ID,
			MappingID:                    view.mapping.ID,
			Loaded:                       loaded,
			CanLoad:                      canManage(view.server.ID),
			GenTokensPerSecond:           view.mapping.GenTokensPerSecond,
			PromptTokensPerSecond:        view.mapping.PromptTokensPerSecond,
			LoadTimeMS:                   view.mapping.LoadTimeMS,
			ContextSize:                  view.mapping.ContextSize,
			MaxConcurrency:               view.mapping.MaxConcurrency,
			RecommendedConcurrency:       view.mapping.RecommendedConcurrency,
			GenTokensPerSecondAtCapacity: view.mapping.GenTokensPerSecondAtCapacity,
			IsMtp:                        byName[routing.CapabilityMTP].Verdict == routing.CapabilityYes,
			VisionCapable:                byName[routing.CapabilityVision].Verdict == routing.CapabilityYes,
			MetricsSource:                view.mapping.MetricsSource,
			MetricsUpdatedAt:             view.mapping.MetricsUpdatedAt,
			LiveProgressSupport:          routing.LiveProgressSupportFromVerdict(liveProgressRow.Verdict),
			LiveProgressCheckedAt:        capabilityCheckedAt(liveProgressRow),
			Capabilities:                 modelServerCapabilityDTOs(caps),
		})
	}
	rows, err = s.filterAllowedModelServerRows(ctx, principal, rows)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ServerName != rows[j].ServerName {
			return rows[i].ServerName < rows[j].ServerName
		}
		return rows[i].MappingID < rows[j].MappingID
	})
	return rows, nil
}

// capabilityCheckedAt is one capability row's CheckedAt as an optional wire
// timestamp: nil for a row that does not exist (zero Verdict) or that carries
// no time at all. Returning &row.CheckedAt unconditionally would put Go's
// zero time.Time on the wire, which renders as a year-0001 date in the
// portal -- a fake "determined at" for a verdict nobody ever established.
func capabilityCheckedAt(row routing.CapabilityRow) *time.Time {
	if row.Verdict == "" || row.CheckedAt.IsZero() {
		return nil
	}
	at := row.CheckedAt
	return &at
}

// modelServerCapabilityDTOs projects a mapping's stored capability rows onto
// the wire shape (see ModelServerCapabilityDTO), preserving
// MappingCapabilitiesForMappings' order (alphabetical by capability -- the
// frontend imposes its OWN fixed order for the known names and does not rely
// on this one). ALWAYS returns a non-nil slice, even for zero rows: the DTO's
// `capabilities` must be `[]` on the wire, never `null` (see
// ModelServerDTO.Capabilities).
func modelServerCapabilityDTOs(rows []routing.CapabilityRow) []ModelServerCapabilityDTO {
	out := make([]ModelServerCapabilityDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelServerCapabilityDTO{
			Capability: r.Capability,
			Verdict:    r.Verdict,
			Source:     r.Source,
			CheckedAt:  r.CheckedAt,
		})
	}
	return out
}

// filterAllowedModelServerRows drops any row whose ServerID the given principal
// is not allowed to USE under resource-group provisioning (Resource Groups
// Phase 2 — Task 4), via the generic filterByAllowedServers (see
// service_visibility_filter.go), which dedupes into a single AllowedServerIDs
// call and returns immediately on an empty input (no store call). A store
// error propagates (failOpen=false) — see visibleMappingViews's doc-comment
// for the visibility-surface matrix. This filter is layered ADDITIONALLY on
// top of ModelServers' own hidden/locked suppression (checked earlier in
// ModelServers, before any row is even built) — the two filters are
// independent and both apply to a non-admin caller.
func (s *Service) filterAllowedModelServerRows(ctx context.Context, principal auth.Token, rows []ModelServerDTO) ([]ModelServerDTO, error) {
	return filterByAllowedServers(ctx, s.AllowedServerIDs, principal, rows, func(r ModelServerDTO) string { return r.ServerID }, false)
}
