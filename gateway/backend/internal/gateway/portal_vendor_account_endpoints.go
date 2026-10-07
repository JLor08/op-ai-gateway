// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

const (
	msgVendorAccountNotFound = "vendor account not found"

	codeVendorAccountListFailed    = "vendor_account.list_failed"
	codeVendorAccountCreateFailed  = "vendor_account.create_failed"
	codeVendorAccountGetFailed     = "vendor_account.get_failed"
	codeVendorAccountUpdateFailed  = "vendor_account.update_failed"
	codeVendorAccountDeleteFailed  = "vendor_account.delete_failed"
	codeVendorAccountConnectFailed = "vendor_account.connect_failed"
)

// handlePortalVendorAccountsEnabled reports whether the vendor-accounts master
// flag (system setting vendor_accounts_enabled) is on, for any portal user
// (gateway:use, GET-only). It returns ONLY that boolean -- no routing mode or
// other setting -- so any user's shell can show or hide the "Anbieter" nav item
// without being granted the system-scoped settings read. It answers while the
// module is off (that is its purpose), and the exact-path route registered for
// it wins over the "/api/portal/vendor-accounts/{id}" subtree (account ids are
// "va_"-prefixed, so none can be called "enabled").
func (s *Server) handlePortalVendorAccountsEnabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"module_enabled": s.Portal.VendorAccountsEnabled(r.Context())})
}

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
// (GET / PATCH / DELETE), the subscription-connect sub-resources
// "/api/portal/vendor-accounts/{id}/connect/{import|begin|complete}" (POST), and
// the OPTIONAL device-code connect sub-resources
// "/api/portal/vendor-accounts/{id}/connect/device/{begin|poll}" (POST). Any
// other deeper path is answered with the same 404 as an unknown id.
func (s *Server) handlePortalVendorAccountItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/vendor-accounts/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 4 && parts[0] != "" && parts[1] == "connect" && parts[2] == "device" {
		switch parts[3] {
		case "begin":
			s.handlePortalVendorAccountConnectDeviceBegin(w, r, token, parts[0])
			return
		case "poll":
			s.handlePortalVendorAccountConnectDevicePoll(w, r, token, parts[0])
			return
		}
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] == "connect" {
		switch parts[2] {
		case "import":
			s.handlePortalVendorAccountConnectImport(w, r, token, parts[0])
			return
		case "begin":
			s.handlePortalVendorAccountConnectBegin(w, r, token, parts[0])
			return
		case "complete":
			s.handlePortalVendorAccountConnectComplete(w, r, token, parts[0])
			return
		}
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

// vendorAccountConnectCompleteRequest is the body of POST
// /api/portal/vendor-accounts/{id}/connect/complete: whatever the user pasted
// back from the vendor -- a code, "code#state", or a whole callback URL.
type vendorAccountConnectCompleteRequest struct {
	Code string `json:"code"`
}

// handlePortalVendorAccountConnectImport (POST .../connect/import) connects a
// subscription account from tokens the user already holds. Owner-only and gated
// by the vendor_accounts_enabled master flag (both in portal.Service).
func (s *Server) handlePortalVendorAccountConnectImport(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	// The tokens are write-only: no response carries them. expires_at is an RFC
	// 3339 time; absent means unknown.
	var req portal.ConnectVendorAccountImportRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.ConnectVendorAccountImport(r.Context(), token, id, req)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountConnectFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalVendorAccountConnectBegin (POST .../connect/begin) starts the OAuth
// code-paste flow and returns the vendor URL the portal opens; the request has
// no body.
func (s *Server) handlePortalVendorAccountConnectBegin(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	authorizeURL, err := s.Portal.BeginVendorAccountConnect(r.Context(), token, id)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountConnectFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authorizeURL})
}

// handlePortalVendorAccountConnectComplete (POST .../connect/complete) exchanges
// the pasted code for tokens and connects the account.
func (s *Server) handlePortalVendorAccountConnectComplete(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req vendorAccountConnectCompleteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.CompleteVendorAccountConnect(r.Context(), token, id, req.Code)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountConnectFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalVendorAccountConnectDeviceBegin (POST .../connect/device/begin)
// starts the OPTIONAL Codex device-code login and returns the user_code plus the
// verification page URL for the UI to display; the request has no body.
func (s *Server) handlePortalVendorAccountConnectDeviceBegin(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	userCode, verificationURL, err := s.Portal.BeginVendorAccountDeviceConnect(r.Context(), token, id)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountConnectFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user_code": userCode, "verification_url": verificationURL})
}

// handlePortalVendorAccountConnectDevicePoll (POST .../connect/device/poll) is
// one backend poll of a begun device login; the frontend calls it on the interval.
// It answers {"connected": bool}: false while the user has not approved yet, true
// once the account is connected. The request has no body and no token is ever
// returned.
func (s *Server) handlePortalVendorAccountConnectDevicePoll(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	connected, err := s.Portal.PollVendorAccountDeviceConnect(r.Context(), token, id)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountConnectFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"connected": connected})
}

// portalVendorAccountErrRows are writePortalVendorAccountError's mapper-specific
// rows (checked before sharedErrorMap). store.ErrNotFound maps to a different
// code in other mappers, so its vendor-account row must stay here.
var portalVendorAccountErrRows = []errRow{
	// The vendor_accounts_enabled master flag is off: a 409, like
	// netbird.module_disabled. The portal hides the area in that state, so this
	// is reached by a stale tab or a direct API client.
	{err: portal.ErrVendorAccountsDisabled, status: http.StatusConflict, code: "vendor_accounts.module_disabled", msg: "vendor accounts are not enabled"},
	{err: portal.ErrVendorAccountNotFound, status: http.StatusNotFound, code: portal.CodeVendorAccountNotFound, msg: msgVendorAccountNotFound},
	{err: portal.ErrVendorAccountNameRequired, status: http.StatusBadRequest, code: "vendor_account.name_required", msg: "vendor account name is required"},
	{err: portal.ErrVendorAccountVendorInvalid, status: http.StatusBadRequest, code: "vendor_account.vendor_invalid", msg: "vendor account vendor is invalid"},
	{err: portal.ErrVendorAccountAuthTypeInvalid, status: http.StatusBadRequest, code: "vendor_account.auth_type_invalid", msg: "vendor account auth type is invalid"},
	{err: portal.ErrVendorAccountStatusInvalid, status: http.StatusBadRequest, code: "vendor_account.status_invalid", msg: "vendor account status is invalid"},
	{err: portal.ErrVendorAccountAPIKeyNotAllowed, status: http.StatusBadRequest, code: "vendor_account.api_key_not_allowed", msg: "an api key can only be set on an api_key account"},
	{err: portal.ErrVendorAccountAPIKeyInvalid, status: http.StatusBadRequest, code: "vendor_account.api_key_invalid", msg: "api key must not be blank; send an empty string to clear it"},
	{err: portal.ErrVendorAccountForbidden, status: http.StatusForbidden, code: "vendor_account.forbidden", msg: notAllowedMsg},
	// Subscription connect. A vendor refusal of the pasted code is a 400, never a
	// 401: the portal treats a 401 from this API as an expired session. The vendor
	// being unreachable or answering with something unusable is a 502. The rows
	// precede the capture.ErrKeyRequired one because ErrVendorAccountConnectKeyRequired
	// wraps it, and its message must talk about the subscription, not an api key.
	{err: portal.ErrVendorAccountNotSubscription, status: http.StatusBadRequest, code: "vendor_account.not_subscription", msg: "only a subscription account can be connected to a subscription"},
	{err: portal.ErrVendorAccountConnectTokenRequired, status: http.StatusBadRequest, code: "vendor_account.connect_token_required", msg: "an access token is required"},
	{err: portal.ErrVendorAccountConnectCodeRequired, status: http.StatusBadRequest, code: "vendor_account.connect_code_required", msg: "paste the code the vendor showed after the sign-in"},
	{err: portal.ErrVendorAccountConnectState, status: http.StatusBadRequest, code: "vendor_account.connect_state", msg: "no connect is in progress for this account, it has expired, or the pasted state does not match; start the connect again"},
	{err: portal.ErrVendorAccountConnectRejected, status: http.StatusBadRequest, code: "vendor_account.connect_rejected", msg: "the vendor rejected the code; check it or start the connect again"},
	{err: portal.ErrVendorAccountConnectUpstream, status: http.StatusBadGateway, code: "vendor_account.connect_upstream_failed", msg: "the vendor could not be reached or answered unexpectedly; try again later"},
	{err: portal.ErrVendorAccountConnectKeyRequired, status: http.StatusBadRequest, code: "vendor_account.connect_key_required", msg: "an encryption key is required to store a vendor subscription on a disk-backed store"},
	// Device-code connect (OpenAI only). Both are 400s, never a 401 (the portal
	// treats a 401 from this API as an expired session).
	{err: portal.ErrVendorAccountDeviceUnsupported, status: http.StatusBadRequest, code: "vendor_account.device_not_supported", msg: "the device connect flow is available for OpenAI accounts only"},
	{err: portal.ErrVendorAccountDeviceConnectState, status: http.StatusBadRequest, code: "vendor_account.device_connect_state", msg: "no device connect is in progress for this account or it has expired; start the device connect again"},
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
