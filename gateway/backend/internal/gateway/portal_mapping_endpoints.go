// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

const msgMappingNotFound = "mapping not found"

// mappingRunStarters are the mapping sub-actions that are POST-only, take
// nothing but the mapping id, and write their own response — the
// `/api/portal/mappings/{id}/{action}` shape whose whole body is "reject any
// other method, then hand the id to a starter".
//
// A table rather than one `if` clause per action, because the three clauses it
// replaces made no decision of their own: the sub-action's name is the only
// thing that differed between them, so an added action was three lines of
// copied control flow around one changed identifier. Adding an entry here
// cannot get the method guard wrong, and cannot get it right for two actions
// and wrong for the third.
//
// It is deliberately NOT the home for every sub-action of a mapping. The
// remaining clauses in handlePortalMappingItem each decide something else —
// `benchmark` parses a mode off the request before it may start, `benchmarks`
// is a GET that maps a service error, `runtime-spec` dispatches on the method
// itself — and folding those in would hide three different bodies behind one
// uniform-looking lookup.
var mappingRunStarters = map[string]func(*Server, http.ResponseWriter, *http.Request, auth.Token, string){
	"probe-context": (*Server).startContextProbe,
	"load":          (*Server).startLoadModel,
	"probe-vram":    (*Server).startVRAMProbe,
}

func (s *Server) handlePortalMappingItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/mappings/"), "/")
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeMappingNotFound, msgMappingNotFound, ""))
		return
	}
	if len(parts) == 2 && parts[1] == "benchmark" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		mode, ok := parseBenchmarkMode(w, r)
		if !ok {
			return
		}
		s.startBenchmark(w, r, token, "mapping", parts[0], mode)
		return
	}
	if len(parts) == 2 {
		if start, ok := mappingRunStarters[parts[1]]; ok {
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			start(s, w, r, token, parts[0])
			return
		}
	}
	if len(parts) == 2 && parts[1] == "benchmarks" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		runs, err := s.Portal.MappingBenchmarks(r.Context(), token, parts[0], 50)
		if err != nil {
			writePortalMappingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": runs})
		return
	}
	if len(parts) == 2 && parts[1] == "runtime-spec" && parts[0] != "" {
		s.handlePortalMappingRuntimeSpec(w, r, token, parts[0])
		return
	}
	id := pathID(r.URL.Path, "/api/portal/mappings/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeMappingNotFound, msgMappingNotFound, ""))
		return
	}
	switch r.Method {
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateMappingRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateMapping(r.Context(), token, id, req)
		if err != nil {
			writePortalMappingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteMapping(r.Context(), token, id); err != nil {
			writePortalMappingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalMappingErrRows are writePortalMappingError's mapper-specific rows
// (checked before sharedErrorMap); portal.ErrMappingNotFound and
// portal.ErrApplicationNotFound map identically elsewhere and live in
// sharedErrorMap instead. portal.ErrMappingStatusInvalid maps to a different
// code in writePortalModelGroupError, and store.ErrNotFound maps to a
// different code in other mappers, so both must stay here.
var portalMappingErrRows = []errRow{
	{err: portal.ErrMappingGatewayNameRequired, status: http.StatusBadRequest, code: "mapping.gateway_name_required", msg: "mapping gateway model name is required"},
	{err: portal.ErrMappingAppNameRequired, status: http.StatusBadRequest, code: "mapping.app_name_required", msg: "mapping app model name is required"},
	{err: portal.ErrMappingStatusInvalid, status: http.StatusBadRequest, code: "mapping.status_invalid", msg: "mapping status is invalid"},
	{err: portal.ErrMappingMetricInvalid, status: http.StatusBadRequest, code: "mapping.metric_invalid", msg: "mapping metric value is invalid"},
	{err: portal.ErrMappingGatewayNameConflict, status: http.StatusConflict, code: "mapping.gateway_name_conflict", msg: "mapping gateway model name already in use"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeMappingNotFound, msg: msgMappingNotFound},
}

func writePortalMappingError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalMappingErrRows, http.StatusInternalServerError, "mapping.request_failed", "mapping request failed")
}
