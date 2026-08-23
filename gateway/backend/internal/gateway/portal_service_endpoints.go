// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"strings"
)

const msgServiceNotFound = "service not found"

// handlePortalServices is the Service Account collection endpoint (spec §7):
// GET lists (admin -> every service; a delegate -> only the services they
// delegate at either tier, via portal.Service.ListServices) and POST creates
// (admin-only -- portal.Service.CreateService itself enforces the admin gate,
// so a non-admin caller gets ErrServiceForbidden -> 403 here, not a 401/404).
// Both are gated only by the session-scope check (gateway:use); the *Read*/
// *Settings*/*Tokens* OBJECT-level gates (spec §6.1) live in portal.Service.
func (s *Server) handlePortalServices(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		services, err := s.Portal.ListServices(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("service.list_failed", "service list failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": services})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateServiceRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateService(r.Context(), token, req)
		if err != nil {
			writePortalServiceError(w, err, "service.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalServiceItem dispatches every "/api/portal/services/{id}[/...]"
// route (spec §7) after a single session-scope check: "/{id}" (single
// service), "/{id}/tokens" (token collection), "/{id}/tokens/{tid}" (a single
// token, delete-only), and "/{id}/tokens/{tid}/rotate" (rotate, post-only).
// Mirrors handlePortalServerItem/handlePortalTokenItem's path-parsing shape.
func (s *Server) handlePortalServiceItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/services/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServiceNotFound, msgServiceNotFound, ""))
		return
	}
	switch {
	case len(parts) == 1:
		s.handlePortalServiceSingle(w, r, token, id)
	case len(parts) == 2 && parts[1] == "tokens":
		s.handlePortalServiceTokens(w, r, token, id)
	case len(parts) == 3 && parts[1] == "tokens" && parts[2] != "":
		s.handlePortalServiceTokenSingle(w, r, token, id, parts[2])
	case len(parts) == 4 && parts[1] == "tokens" && parts[2] != "" && parts[3] == "rotate":
		s.handlePortalServiceTokenRotate(w, r, token, id, parts[2])
	case len(parts) == 2 && parts[1] == "admin-groups":
		s.handlePortalServiceAdminGroups(w, r, token, id)
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServiceNotFound, msgServiceNotFound, ""))
	}
}

// handlePortalServiceSingle backs GET/PUT/DELETE /api/portal/services/{id}
// (spec §7's *Read*/*Settings* routes; GetService is Read, UpdateService and
// DeleteService are Settings -- all enforced inside portal.Service).
func (s *Server) handlePortalServiceSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetService(r.Context(), token, id)
		if err != nil {
			writePortalServiceError(w, err, "service.get_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateServiceRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateService(r.Context(), token, id, req)
		if err != nil {
			writePortalServiceError(w, err, "service.update_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteService(r.Context(), token, id); err != nil {
			writePortalServiceError(w, err, "service.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalServiceTokens backs GET (list)/POST (mint, 201 + one-time
// plaintext secret) /api/portal/services/{id}/tokens (spec §7's *Tokens*
// route -- admin or ANY delegate tier, enforced by
// authorizeServiceTokens inside portal.Service).
func (s *Server) handlePortalServiceTokens(w http.ResponseWriter, r *http.Request, token auth.Token, serviceID string) {
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.Portal.ListServiceTokens(r.Context(), token, serviceID)
		if err != nil {
			writePortalServiceError(w, err, "service.token_list_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": tokens})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateServiceTokenRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		resp, err := s.Portal.CreateServiceToken(r.Context(), token, serviceID, req)
		if err != nil {
			writePortalServiceError(w, err, "service.token_create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalServiceTokenSingle backs DELETE
// /api/portal/services/{id}/tokens/{tid} (spec §7's *Tokens* route; no other
// method is defined on this path). tokenID must belong to serviceID --
// DeleteServiceToken 404s a cross-service id (no leak, see
// portal.Service.serviceTokenByID).
func (s *Server) handlePortalServiceTokenSingle(w http.ResponseWriter, r *http.Request, token auth.Token, serviceID, tokenID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.Portal.DeleteServiceToken(r.Context(), token, serviceID, tokenID); err != nil {
		writePortalServiceError(w, err, "service.token_delete_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalServiceTokenRotate backs POST
// /api/portal/services/{id}/tokens/{tid}/rotate (spec §7's *Tokens* route):
// replaces the token's secret in place, returning the fresh plaintext secret
// once (mirrors handlePortalTokenRotate's user-token analog).
func (s *Server) handlePortalServiceTokenRotate(w http.ResponseWriter, r *http.Request, token auth.Token, serviceID, tokenID string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	resp, err := s.Portal.RotateServiceToken(r.Context(), token, serviceID, tokenID)
	if err != nil {
		writePortalServiceError(w, err, "service.token_rotate_failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePortalServiceAdminGroups backs PUT /api/portal/services/{id}/admin-groups:
// replaces the service's linked admin-group set (Phase C, spec 2026-08-10,
// mirrors handlePortalServerAdminGroups exactly).
func (s *Server) handlePortalServiceAdminGroups(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		AdminGroupIDs []string `json:"admin_group_ids"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetServiceAdminGroups(r.Context(), token, id, body.AdminGroupIDs)
	if err != nil {
		writePortalServiceError(w, err, "service.update_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleServiceAdminGroupCandidates backs GET
// /api/portal/service-admin-group-candidates: the admin-tier groups the
// caller may create/link a service into (system scope -> every admin-tier
// group; anyone else -> the groups they may manage services through).
// Drives the create-service / linkage-editor picker (mirrors
// handleServerAdminGroupCandidates exactly).
func (s *Server) handleServiceAdminGroupCandidates(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ServiceAdminGroupCandidates(r.Context(), token)
	if err != nil {
		writePortalServiceError(w, err, "service.admin_group_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// portalServiceErrRows are writePortalServiceError's mapper-specific rows
// (checked before sharedErrorMap); portal.ErrLimitValidation maps
// identically in writePortalLimitError and lives in sharedErrorMap instead.
// portal.ErrTokenNotFound maps to a different code in writePortalTokenError
// (portal.token_not_found) and writeProjectError (token.not_found), so it
// must stay here.
var portalServiceErrRows = []errRow{
	{err: portal.ErrServiceForbidden, status: http.StatusForbidden, code: "service.forbidden", msg: "not allowed"},
	{err: portal.ErrServiceValidation, status: http.StatusBadRequest, code: "service.validation_failed", msg: "service request is invalid"},
	// Admin-group linkage (service WRITE path, Phase C, spec 2026-08-10) --
	// mirrors the equivalent 3 branches in writePortalServerError exactly.
	{err: portal.ErrServiceAdminGroupRequired, status: http.StatusBadRequest, code: "service.admin_group_required", msg: "at least one admin group is required"},
	{err: portal.ErrServiceAdminGroupInvalid, status: http.StatusBadRequest, code: "service.admin_group_invalid", msg: "admin group is invalid"},
	{err: portal.ErrServiceAdminGroupParentMismatch, status: http.StatusBadRequest, code: "service.admin_group_parent_mismatch", msg: "admin groups must share one parent system group"},
	{err: portal.ErrServiceNotFound, status: http.StatusNotFound, code: portal.CodeServiceNotFound, msg: msgServiceNotFound},
	{err: portal.ErrTokenNotFound, status: http.StatusNotFound, code: "service.token_not_found", msg: "service token not found"},
}

// writePortalServiceError maps portal.Service's Service Account sentinel
// errors onto HTTP status codes (spec §7): Forbidden->403, NotFound (service
// OR token)->404, Validation->400, else defaultCode->500. Every branch uses a
// STATIC message -- never err.Error() -- so no internal detail ever leaks to
// the client (mirrors writePortalServerError/writePortalTokenError).
func writePortalServiceError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, portalServiceErrRows, http.StatusInternalServerError, defaultCode, "service request failed")
}
