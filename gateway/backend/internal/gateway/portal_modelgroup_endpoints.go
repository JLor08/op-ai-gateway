// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
	"time"
)

const msgModelGroupNotFound = "model group not found"

// handlePortalModelGroupServers returns every (model, server) a model group can serve with a live
// selection-order rank. gateway:use, GET. name=<group>. An unknown/inactive group (or a nil group
// registry) yields empty data — never an error. The rank = available candidates first, then in
// flattened traversal order (the order the group's own failover walk would try), then by
// descending live score — what the resolver would try right now.
func (s *Server) handlePortalModelGroupServers(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	members, _, ok := s.Groups.Group(name) // flattened leaf model names, in traversal order
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"data": []portal.GroupModelServerDTO{}})
		return
	}
	now := time.Now()
	type keyed struct {
		dto        portal.ModelServerDTO
		model      string
		modelIndex int
		score      float64
		available  bool
	}
	rowsOut := make([]keyed, 0)
	for idx, m := range members {
		modelName := m.MemberGatewayName
		dtos, err := s.Portal.ModelServers(r.Context(), token, modelName)
		if err != nil {
			continue
		}
		scoreByMapping := map[string]struct {
			score     float64
			available bool
		}{}
		if s.Resolver != nil {
			if cs, sErr := s.Resolver.ScoreModelServers(r.Context(), modelName, now); sErr == nil {
				for _, c := range cs {
					scoreByMapping[c.MappingID] = struct {
						score     float64
						available bool
					}{c.Score, c.Available}
				}
			}
		}
		for _, d := range dtos {
			sc := scoreByMapping[d.MappingID]
			rowsOut = append(rowsOut, keyed{dto: d, model: modelName, modelIndex: idx, score: sc.score, available: sc.available})
		}
	}
	sort.SliceStable(rowsOut, func(i, j int) bool {
		if rowsOut[i].available != rowsOut[j].available {
			return rowsOut[i].available // available first
		}
		if rowsOut[i].modelIndex != rowsOut[j].modelIndex {
			return rowsOut[i].modelIndex < rowsOut[j].modelIndex // traversal order
		}
		return rowsOut[i].score > rowsOut[j].score
	})
	out := make([]portal.GroupModelServerDTO, len(rowsOut))
	for i, kr := range rowsOut {
		out[i] = portal.GroupModelServerDTO{ModelServerDTO: kr.dto, Model: kr.model, Priority: i + 1}
	}
	// Defense-in-depth re-filter (Resource Groups Phase 2 T4): every row already
	// came from s.Portal.ModelServers, which itself filters by AllowedServerIDs
	// per member — so this is currently a no-op re-check, kept explicit so a
	// future refactor that stops routing through ModelServers can't silently
	// reopen the leak this handler was flagged for. FilterAllowedGroupModelServerRows
	// (package portal; XC-1) fails open on a store error, matching this
	// handler's original local copy.
	out = portal.FilterAllowedGroupModelServerRows(r.Context(), s.Portal, token, out)
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handlePortalModelGroups handles the model-group collection (GET list + POST
// create). Admin-scoped (role admin OR system_admin) — group management, like
// server/app/mapping management, is a global-admin capability.
func (s *Server) handlePortalModelGroups(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListModelGroups(r.Context(), token)
		if err != nil {
			writePortalModelGroupError(w, err, "model_group.list_failed")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateModelGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateModelGroup(r.Context(), token, req)
		if err != nil {
			writePortalModelGroupError(w, err, "model_group.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalModelGroupItem handles a single model group by id (GET/PUT/DELETE).
func (s *Server) handlePortalModelGroupItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	id := pathID(r.URL.Path, "/api/portal/model-groups/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeModelGroupNotFound, msgModelGroupNotFound, ""))
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetModelGroup(r.Context(), token, id)
		if err != nil {
			writePortalModelGroupError(w, err, "model_group.request_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateModelGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateModelGroup(r.Context(), token, id, req)
		if err != nil {
			writePortalModelGroupError(w, err, "model_group.update_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteModelGroup(r.Context(), token, id); err != nil {
			writePortalModelGroupError(w, err, "model_group.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalModelSettingItem sets a model's visibility (PUT
// /api/portal/model-settings/{name}, body {visibility}). The {name} segment is
// read from the escaped path and unescaped so a gateway model name containing a
// slash (e.g. "openai/gpt-4o", URL-encoded by the client) round-trips intact.
func (s *Server) handlePortalModelSettingItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	rawName := strings.TrimPrefix(r.URL.EscapedPath(), "/api/portal/model-settings/")
	name, uerr := url.PathUnescape(rawName)
	if uerr != nil {
		name = rawName
	}
	name = strings.TrimSpace(name)
	if name == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("model_setting.not_found", "model not found", ""))
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req portal.SetModelVisibilityRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.Portal.SetModelVisibility(r.Context(), token, name, req.Visibility); err != nil {
		writePortalModelGroupError(w, err, "model_setting.update_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// portalModelGroupErrRows are writePortalModelGroupError's mapper-specific
// rows. portal.ErrMappingStatusInvalid maps to a different code in
// writePortalMappingError (mapping.status_invalid), and store.ErrNotFound
// maps to a different code in other mappers, so both must stay here.
var portalModelGroupErrRows = []errRow{
	{err: portal.ErrModelGroupNameConflict, status: http.StatusConflict, code: "model_group.name_conflict", msg: "model group name already in use"},
	{err: portal.ErrModelGroupNameRequired, status: http.StatusBadRequest, code: "model_group.name_required", msg: "model group name is required"},
	{err: portal.ErrModelGroupModeInvalid, status: http.StatusBadRequest, code: "model_group.mode_invalid", msg: "model group failover mode is invalid"},
	{err: portal.ErrModelGroupMemberInvalid, status: http.StatusBadRequest, code: "model_group.member_invalid", msg: "model group member is invalid"},
	{err: portal.ErrModelGroupMemberOrderInvalid, status: http.StatusBadRequest, code: "model_group.member_order_invalid", msg: "model group member order is invalid"},
	{err: portal.ErrModelGroupMinSpeedFallbackInvalid, status: http.StatusBadRequest, code: "model_group.min_speed_fallback_invalid", msg: "model group minimum-speed fallback is invalid"},
	{err: portal.ErrModelGroupMinTokensPerSecondInvalid, status: http.StatusBadRequest, code: "model_group.min_tokens_per_second_invalid", msg: "model group minimum tokens per second must not be negative"},
	{err: portal.ErrModelGroupClimbSpeedMarginInvalid, status: http.StatusBadRequest, code: "model_group.climb_speed_margin_percent_invalid", msg: "model group climb speed margin must not be negative"},
	{err: portal.ErrModelGroupCycle, status: http.StatusBadRequest, code: "model_group.cycle", msg: "model group member set would create a cycle"},
	{err: portal.ErrModelVisibilityInvalid, status: http.StatusBadRequest, code: "model_setting.visibility_invalid", msg: "model visibility is invalid"},
	{err: portal.ErrMappingStatusInvalid, status: http.StatusBadRequest, code: "model_group.status_invalid", msg: "model group status is invalid"},
	{err: portal.ErrModelGroupNotFound, status: http.StatusNotFound, code: portal.CodeModelGroupNotFound, msg: msgModelGroupNotFound},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeModelGroupNotFound, msg: msgModelGroupNotFound},
}

// writePortalModelGroupError maps model-group / model-setting service errors to
// HTTP responses. fallback is the error code for an unclassified (500) error.
func writePortalModelGroupError(w http.ResponseWriter, err error, fallback string) {
	writeMappedError(w, err, portalModelGroupErrRows, http.StatusInternalServerError, fallback, "model group request failed")
}
