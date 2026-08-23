// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
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
