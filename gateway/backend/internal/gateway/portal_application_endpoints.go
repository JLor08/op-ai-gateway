// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

const msgApplicationNotFound = "application not found"

func (s *Server) handlePortalApplicationItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/applications/"), "/")
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeApplicationNotFound, msgApplicationNotFound, ""))
		return
	}
	appID := parts[0]

	if len(parts) == 2 && parts[1] == "sync-models" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		result, err := s.Portal.SyncApplicationModels(r.Context(), token, appID)
		if err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
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
		s.startBenchmark(w, r, token, "application", appID, mode)
		return
	}

	if len(parts) == 2 && parts[1] == "mappings" {
		switch r.Method {
		case http.MethodGet:
			resp, err := s.Portal.ListMappings(r.Context(), token, appID)
			if err != nil {
				writePortalMappingError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, resp)
		case http.MethodPost:
			raw, ok := readRawJSON(w, r)
			if !ok {
				return
			}
			var req portal.CreateMappingRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
				return
			}
			dto, err := s.Portal.CreateMapping(r.Context(), token, appID, req)
			if err != nil {
				writePortalMappingError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, dto)
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		}
		return
	}

	if len(parts) != 1 {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeApplicationNotFound, msgApplicationNotFound, ""))
		return
	}

	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetApplication(r.Context(), token, appID)
		if err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateApplicationRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateApplication(r.Context(), token, appID, req)
		if err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteApplication(r.Context(), token, appID); err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalApplicationErrRows are writePortalApplicationError's mapper-specific
// rows (checked before sharedErrorMap); portal.ErrApplicationNotFound,
// portal.ErrServerNotFound and portal.ErrPathSuffixInvalid map identically
// elsewhere and live in sharedErrorMap instead. store.ErrNotFound maps to a
// different code in other mappers, so it must stay here.
var portalApplicationErrRows = []errRow{
	{err: portal.ErrApplicationTypeInvalid, status: http.StatusBadRequest, code: "application.type_invalid", msg: "application type is invalid"},
	{err: portal.ErrApplicationSchemeInvalid, status: http.StatusBadRequest, code: "application.scheme_invalid", msg: "application scheme is invalid"},
	{err: portal.ErrApplicationHealthPathInvalid, status: http.StatusBadRequest, code: "application.health_path_invalid", msg: "application health check path is invalid"},
	{err: portal.ErrApplicationHealthModeInvalid, status: http.StatusBadRequest, code: "application.health_mode_invalid", msg: "application health check mode is invalid"},
	{err: portal.ErrApplicationPortInvalid, status: http.StatusBadRequest, code: "application.port_invalid", msg: "application port is invalid"},
	{err: portal.ErrApplicationFlavorInvalid, status: http.StatusBadRequest, code: "application.flavor_invalid", msg: "application api flavor is invalid"},
	{err: portal.ErrApplicationStatusInvalid, status: http.StatusBadRequest, code: "application.status_invalid", msg: "application status is invalid"},
	{err: portal.ErrApplicationConflict, status: http.StatusConflict, code: "application.port_conflict", msg: "application port already in use"},
	{err: portal.ErrApplicationSyncFailed, status: http.StatusBadGateway, code: "application.sync_failed", msg: "model sync failed"},
	{err: portal.ErrApplicationTuningInvalid, status: http.StatusBadRequest, code: "application.tuning_invalid", msg: "application tuning values must be non-negative"},
	{err: portal.ErrApplicationHealthIntervalInvalid, status: http.StatusBadRequest, code: "application.health_interval_invalid", msg: "application health check interval is invalid"},
	{err: portal.ErrApplicationBenchmarkIntervalInvalid, status: http.StatusBadRequest, code: "application.benchmark_interval_invalid", msg: "application benchmark schedule interval is invalid"},
	{err: portal.ErrApplicationTokenHeaderInvalid, status: http.StatusBadRequest, code: "application.token_header_invalid", msg: "token header name is invalid"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeApplicationNotFound, msg: msgApplicationNotFound},
}

func writePortalApplicationError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalApplicationErrRows, http.StatusInternalServerError, "application.request_failed", "application request failed")
}
