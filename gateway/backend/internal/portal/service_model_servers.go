// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/auth"
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

	GenTokensPerSecond           float64    `json:"gen_tokens_per_second"`
	PromptTokensPerSecond        float64    `json:"prompt_tokens_per_second"`
	LoadTimeMS                   int        `json:"load_time_ms"`
	ContextSize                  int        `json:"context_size"`
	MaxConcurrency               int        `json:"max_concurrency"`
	RecommendedConcurrency       int        `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64    `json:"gen_tokens_per_second_at_capacity"`
	IsMtp                        bool       `json:"is_mtp"`
	VisionCapable                bool       `json:"vision_capable"`
	MetricsSource                string     `json:"metrics_source"`
	MetricsUpdatedAt             *time.Time `json:"metrics_updated_at,omitempty"`

	// LiveProgressSupport is the mapping's PERSISTED verdict on whether this
	// upstream tolerates the live-progress two-parameter request (#51):
	// "supported" / "unsupported" / "" (never determined) -- routing.
	// ModelMapping.LiveProgressSupport, read straight off view.mapping exactly
	// like ContextSize above. Unlike State/ActiveRequests/QueueDepth/
	// MetricsProbe/ContextProbe (which Service.ModelServers leaves zero/empty
	// for the gateway layer to inject from the runtime-status registry), this
	// value is written directly to the mapping by the background detectors
	// (until #49-3 moved them onto model_mapping_capabilities rows -- see
	// routing.MappingStore.UpsertMappingCapabilities; this column is read-only
	// legacy until the readers move), so it needs no gateway-injection seam --
	// Service.ModelServers fills it itself. No
	// `omitempty`: "never determined" must be an explicit "" on the wire, not a
	// missing key -- same rule the wire encoding of State already follows.
	LiveProgressSupport string `json:"live_progress_support"`
	// LiveProgressCheckedAt is when that verdict was last determined; nil when
	// never determined. Diagnostic/tooltip only, mirroring ModelMapping.
	// LiveProgressCheckedAt's own doc-comment -- no decision logic may read it.
	LiveProgressCheckedAt *time.Time `json:"live_progress_checked_at,omitempty"`

	// CapVision/CapVideo/CapAudio/CapTools are the auto-detected capability
	// verdicts (#49 sub-project 2), each "" (never determined) | "yes" | "no",
	// read straight off ModelMapping.CapVision/CapVideo/CapAudio/CapTools
	// exactly like LiveProgressSupport above. Same gateway-injection-seam
	// story as LiveProgressSupport: a background detector wrote these to the
	// mapping directly (until #49-3 moved every probe onto
	// model_mapping_capabilities rows -- see
	// routing.MappingStore.UpsertMappingCapabilities, which is also where the
	// no-metrics_locked-guard argument these columns cited now lives), so
	// there is nothing for the gateway layer to inject after the fact --
	// Service.ModelServers fills them itself. No `omitempty` on any of the
	// four: "never determined" must be an explicit "" on the wire, not a
	// missing key -- the same rule LiveProgressSupport's own doc-comment
	// explains.
	CapVision string `json:"cap_vision"`
	CapVideo  string `json:"cap_video"`
	CapAudio  string `json:"cap_audio"`
	CapTools  string `json:"cap_tools"`
	// CapExtra is ModelMapping.CapExtra's stored JSON array decoded into a
	// []string for the wire, rather than passing the raw JSON-encoded string
	// through -- the portal renders one chip per entry, not a JSON blob. Nil
	// (omitted from the wire) when empty. Decoding is best-effort: the stored
	// string is operator-invisible JSON a background detector wrote, so a
	// decode failure degrades to nil/omitted rather than failing the whole
	// row -- one malformed mapping must not blank a server's entire listing.
	CapExtra []string `json:"cap_extra,omitempty"`
	// CapabilitiesSource is ModelMapping.CapabilitiesSource read straight
	// through: which probe produced the current verdicts ("llama_cpp_props" |
	// "ollama_show" | ""). No `omitempty`, same reasoning as the four verdicts
	// above.
	CapabilitiesSource string `json:"capabilities_source"`
	// CapabilitiesCheckedAt is ModelMapping.CapabilitiesCheckedAt read
	// straight through; nil when never determined. Diagnostic/tooltip only,
	// mirroring LiveProgressCheckedAt's own doc-comment -- no decision logic
	// may read it.
	CapabilitiesCheckedAt *time.Time `json:"capabilities_checked_at,omitempty"`
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

	rows := make([]ModelServerDTO, 0)
	for _, view := range views {
		if view.mapping.GatewayModelName != gatewayModelName {
			continue
		}
		loaded := false
		if s.loadedModels != nil && view.mapping.AppModelName != "" {
			for _, m := range s.loadedModels.LoadedAppModels(view.app.ID, view.server.ID) {
				if m == view.mapping.AppModelName {
					loaded = true
					break
				}
			}
		}
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
			IsMtp:                        view.mapping.IsMTP,
			VisionCapable:                view.mapping.VisionCapable,
			MetricsSource:                view.mapping.MetricsSource,
			MetricsUpdatedAt:             view.mapping.MetricsUpdatedAt,
			LiveProgressSupport:          view.mapping.LiveProgressSupport,
			LiveProgressCheckedAt:        view.mapping.LiveProgressCheckedAt,
			CapVision:                    view.mapping.CapVision,
			CapVideo:                     view.mapping.CapVideo,
			CapAudio:                     view.mapping.CapAudio,
			CapTools:                     view.mapping.CapTools,
			CapExtra:                     decodeCapExtra(view.mapping.CapExtra),
			CapabilitiesSource:           view.mapping.CapabilitiesSource,
			CapabilitiesCheckedAt:        view.mapping.CapabilitiesCheckedAt,
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

// decodeCapExtra decodes a mapping's stored CapExtra JSON-array string into a
// []string for the wire (see ModelServerDTO.CapExtra). "" (nothing extra
// reported) decodes to nil, same as a decode failure: the stored string is
// operator-invisible JSON a background detector wrote, so a malformed value
// must degrade this one field to empty rather than error the whole row.
func decodeCapExtra(raw string) []string {
	if raw == "" {
		return nil
	}
	var extra []string
	if err := json.Unmarshal([]byte(raw), &extra); err != nil {
		return nil
	}
	return extra
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
