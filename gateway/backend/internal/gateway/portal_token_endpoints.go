// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

const msgTokenNotFound = "token not found"

func (s *Server) handlePortalTokens(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListTokens(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.token_list_failed", "token list failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateTokenRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		resp, err := s.Portal.CreateToken(r.Context(), token, req)
		if err != nil {
			if errors.Is(err, portal.ErrTokenNameRequired) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("portal.token_name_required", "token name is required", ""))
				return
			}
			if errors.Is(err, portal.ErrTokenScopeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("portal.token_scope_invalid", "token scope is invalid", ""))
				return
			}
			if errors.Is(err, portal.ErrTokenScopeForbidden) {
				writeJSON(w, http.StatusForbidden, apierror.Response("portal.token_scope_forbidden", "token scope is not allowed", ""))
				return
			}
			if errors.Is(err, portal.ErrTokenNameConflict) {
				writeJSON(w, http.StatusConflict, apierror.Response("portal.token_name_conflict", "token name already exists", ""))
				return
			}
			// Every model-valued token setting (the catch-all override, each
			// override rule's target, the unknown-model redirect's fallback)
			// rejects an unroutable name with this one error. The PATCH path
			// below maps it through writePortalTokenError; this handler's
			// hand-inlined list had no row for it, so on CREATE the same
			// mistake came back as a 500 "the gateway broke" instead of a 400
			// "that name is wrong".
			if errors.Is(err, portal.ErrTokenModelOverrideInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("portal.token_model_override_invalid", "token model override is invalid", ""))
				return
			}
			// Project attribution (spec: 2026-08-08-projects-design.md §6/§9):
			// CreateToken's assignTokenProject enforces membership; this handler
			// predates that (its error mapping was hand-inlined rather than routed
			// through writePortalTokenError, which the PATCH/rotate handlers below
			// already use and which already carries these two cases) -- without
			// them, a non-member's attempt to attribute a token to a project they
			// can't see fell through to the generic 500 default, masking the
			// intended 403/404.
			if errors.Is(err, portal.ErrProjectNotMember) {
				writeJSON(w, http.StatusForbidden, apierror.Response("token.project_not_member", "you are not a member of that project", ""))
				return
			}
			if errors.Is(err, portal.ErrProjectNotFound) {
				writeJSON(w, http.StatusNotFound, apierror.Response("project.not_found", "project not found", ""))
				return
			}
			if errors.Is(err, store.ErrConflict) {
				writeJSON(w, http.StatusConflict, apierror.Response("portal.token_conflict", "token could not be created", ""))
				return
			}
			writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.token_create_failed", "token could not be created", ""))
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalTokenItem dispatches everything under /api/portal/tokens/: the
// single-token CRUD (PATCH/DELETE on /{id}) plus /{id}/rotate. It mirrors
// handlePortalChatItem's strings.Split sub-dispatch.
func (s *Server) handlePortalTokenItem(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/portal/tokens/")
	parts := strings.Split(suffix, "/")
	id := parts[0]
	switch {
	case len(parts) == 1: // /{id}
		s.handlePortalTokenSingle(w, r, id)
	case len(parts) == 2 && parts[1] == "rotate": // /{id}/rotate
		s.handlePortalTokenRotate(w, r, id)
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeTokenNotFound, msgTokenNotFound, ""))
	}
}

func (s *Server) handlePortalTokenSingle(w http.ResponseWriter, r *http.Request, id string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeTokenNotFound, msgTokenNotFound, ""))
		return
	}
	switch r.Method {
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateTokenRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateToken(r.Context(), token, id, req)
		if err != nil {
			writePortalTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteToken(r.Context(), token, id); err != nil {
			writePortalTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

func (s *Server) handlePortalTokenRotate(w http.ResponseWriter, r *http.Request, id string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeTokenNotFound, msgTokenNotFound, ""))
		return
	}
	resp, err := s.Portal.RotateToken(r.Context(), token, id)
	if err != nil {
		writePortalTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// portalTokenErrRows are writePortalTokenError's mapper-specific rows
// (checked before sharedErrorMap); portal.ErrProjectNotFound maps
// identically in writeProjectError and lives in sharedErrorMap instead.
// portal.ErrTokenNotFound maps to a different code in writePortalServiceError
// (service.token_not_found) and writeProjectError (token.not_found), and
// store.ErrNotFound maps to a different code in other mappers -- both must
// stay here.
var portalTokenErrRows = []errRow{
	{err: portal.ErrTokenNameRequired, status: http.StatusBadRequest, code: "portal.token_name_required", msg: "token name is required"},
	{err: portal.ErrTokenNameConflict, status: http.StatusConflict, code: "portal.token_name_conflict", msg: "token name already exists"},
	{err: portal.ErrTokenScopeInvalid, status: http.StatusBadRequest, code: "portal.token_scope_invalid", msg: "token scope is invalid"},
	{err: portal.ErrTokenScopeForbidden, status: http.StatusForbidden, code: "portal.token_scope_forbidden", msg: "token scope is not allowed"},
	{err: portal.ErrTokenStatusInvalid, status: http.StatusBadRequest, code: "portal.token_status_invalid", msg: "token status is invalid"},
	{err: portal.ErrTokenModelOverrideInvalid, status: http.StatusBadRequest, code: "portal.token_model_override_invalid", msg: "token model override is invalid"},
	{err: portal.ErrTokenNotDeletable, status: http.StatusBadRequest, code: "token.not_deletable", msg: "token cannot be modified or deleted"},
	{err: portal.ErrTokenNotFound, status: http.StatusNotFound, code: portal.CodeTokenNotFound, msg: msgTokenNotFound},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeTokenNotFound, msg: msgTokenNotFound},
	{err: portal.ErrTokenRequired, status: http.StatusBadRequest, code: "portal.token_required", msg: "token id is required"},
	{err: portal.ErrTokenForbidden, status: http.StatusForbidden, code: "portal.token_forbidden", msg: "token is not usable"},
	{err: portal.ErrProjectNotMember, status: http.StatusForbidden, code: "token.project_not_member", msg: "you are not a member of that project"},
}

func writePortalTokenError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalTokenErrRows, http.StatusInternalServerError, "portal.token_update_failed", "token update failed")
}
