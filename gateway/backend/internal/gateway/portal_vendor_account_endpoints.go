// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
)

const (
	msgVendorAccountNotFound = "vendor account not found"

	codeVendorAccountListFailed   = "vendor_account.list_failed"
	codeVendorAccountCreateFailed = "vendor_account.create_failed"
	codeVendorAccountGetFailed    = "vendor_account.get_failed"
	codeVendorAccountUpdateFailed = "vendor_account.update_failed"
	codeVendorAccountDeleteFailed = "vendor_account.delete_failed"
)

// handlePortalVendorAccounts is the vendor-account ("Anbieter") collection
// endpoint: GET lists the caller's OWN accounts, POST creates one owned by the
// caller. Both are gated only by the session-scope check (gateway:use); the
// per-account owner gate (404-no-leak) lives in portal.Service, and the api_key
// is write-only -- no response ever carries it.
func (s *Server) handlePortalVendorAccounts(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListVendorAccounts(r.Context(), token)
		if err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountListFailed)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateVendorAccountRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateVendorAccount(r.Context(), token, req)
		if err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountCreateFailed)
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalVendorAccountItem serves "/api/portal/vendor-accounts/{id}"
// (GET / PATCH / DELETE). Any deeper path -- there are none yet -- is answered
// with the same 404 as an unknown id.
func (s *Server) handlePortalVendorAccountItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	id := pathID(r.URL.Path, "/api/portal/vendor-accounts/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeVendorAccountNotFound, msgVendorAccountNotFound, ""))
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetVendorAccount(r.Context(), token, id)
		if err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountGetFailed)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateVendorAccountRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateVendorAccount(r.Context(), token, id, req)
		if err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if _, err := s.Portal.DeleteVendorAccount(r.Context(), token, id); err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountDeleteFailed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalVendorAccountErrRows are writePortalVendorAccountError's mapper-specific
// rows (checked before sharedErrorMap). store.ErrNotFound maps to a different
// code in other mappers, so its vendor-account row must stay here.
var portalVendorAccountErrRows = []errRow{
	{err: portal.ErrVendorAccountNotFound, status: http.StatusNotFound, code: portal.CodeVendorAccountNotFound, msg: msgVendorAccountNotFound},
	{err: portal.ErrVendorAccountNameRequired, status: http.StatusBadRequest, code: "vendor_account.name_required", msg: "vendor account name is required"},
	{err: portal.ErrVendorAccountVendorInvalid, status: http.StatusBadRequest, code: "vendor_account.vendor_invalid", msg: "vendor account vendor is invalid"},
	{err: portal.ErrVendorAccountAuthTypeInvalid, status: http.StatusBadRequest, code: "vendor_account.auth_type_invalid", msg: "vendor account auth type is invalid"},
	{err: portal.ErrVendorAccountStatusInvalid, status: http.StatusBadRequest, code: "vendor_account.status_invalid", msg: "vendor account status is invalid"},
	{err: portal.ErrVendorAccountAPIKeyNotAllowed, status: http.StatusBadRequest, code: "vendor_account.api_key_not_allowed", msg: "an api key can only be set on an api_key account"},
	{err: portal.ErrVendorAccountForbidden, status: http.StatusForbidden, code: "vendor_account.forbidden", msg: notAllowedMsg},
	// capture.SealSecret returns capture.ErrKeyRequired when a non-empty api key
	// is sealed on a disk-backed store with no encryption key: the operator's
	// own keyless misconfiguration, a 400 rather than a 500 (the same class the
	// application and runtime-spec api tokens map).
	{err: capture.ErrKeyRequired, status: http.StatusBadRequest, code: "vendor_account.api_key_key_required", msg: "an encryption key is required to store a vendor account api key on a disk-backed store"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeVendorAccountNotFound, msg: msgVendorAccountNotFound},
}

func writePortalVendorAccountError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, portalVendorAccountErrRows, http.StatusInternalServerError, defaultCode, "vendor account request failed")
}
