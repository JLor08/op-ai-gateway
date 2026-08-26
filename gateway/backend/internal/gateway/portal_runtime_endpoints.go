// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
)

const msgRuntimeSpecNotFound = "runtime spec not found"

// handlePortalMappingRuntimeSpec serves GET/PUT/DELETE
// /api/portal/mappings/{mappingID}/runtime-spec, dispatched from
// handlePortalMappingItem's sub-path guard (portal_mapping_endpoints.go)
// BEFORE its own pathID fallthrough. Auth was already checked once by the
// caller (requireWebScope); every further authorization decision (mapping ->
// application -> server ownership/admin-group chain) happens inside
// portal.Service via authorizeMapping.
func (s *Server) handlePortalMappingRuntimeSpec(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetRuntimeSpec(r.Context(), token, mappingID)
		if err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.PutRuntimeSpecRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.PutRuntimeSpec(r.Context(), token, mappingID, req)
		if err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteRuntimeSpec(r.Context(), token, mappingID); err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalRuntimeSpecErrRows are writePortalRuntimeSpecError's mapper-specific
// rows (checked before sharedErrorMap); portal.ErrMappingNotFound (the
// 404-no-leak collapse from authorizeMapping) maps identically elsewhere and
// lives in sharedErrorMap instead. Every sentinel here is 400 except
// ErrRuntimeSpecNotFound (404 — nothing to delete).
var portalRuntimeSpecErrRows = []errRow{
	{err: portal.ErrRuntimeSpecNotFound, status: http.StatusNotFound, code: portal.CodeRuntimeSpecNotFound, msg: msgRuntimeSpecNotFound},
	{err: portal.ErrRuntimeSpecBinaryRequired, status: http.StatusBadRequest, code: "runtime_spec.binary_required", msg: "runtime spec binary is required and must be an absolute path"},
	{err: portal.ErrRuntimeSpecArgsInvalid, status: http.StatusBadRequest, code: "runtime_spec.args_invalid", msg: "runtime spec args are invalid"},
	{err: portal.ErrRuntimeSpecEnvInvalid, status: http.StatusBadRequest, code: "runtime_spec.env_invalid", msg: "runtime spec env is invalid"},
	{err: portal.ErrRuntimeSpecGPUInvalid, status: http.StatusBadRequest, code: "runtime_spec.gpu_invalid", msg: "runtime spec gpu configuration is invalid"},
	{err: portal.ErrRuntimeSpecTuningInvalid, status: http.StatusBadRequest, code: "runtime_spec.tuning_invalid", msg: "runtime spec tuning values must be non-negative"},
	{err: portal.ErrRuntimeSpecAdminStateInvalid, status: http.StatusBadRequest, code: "runtime_spec.admin_state_invalid", msg: "runtime spec admin state is invalid"},
	{err: portal.ErrRuntimeSpecNotServerAgent, status: http.StatusBadRequest, code: "runtime_spec.application_not_server_agent", msg: "runtime spec requires a server_agent application"},
}

func writePortalRuntimeSpecError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalRuntimeSpecErrRows, http.StatusInternalServerError, "runtime_spec.request_failed", "runtime spec request failed")
}
