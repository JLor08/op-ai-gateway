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
	"strconv"
	"strings"
)

const (
	msgVendorAccountNotFound = "vendor account not found"

	// The codes and messages of the gate rows that both vendor-account mappers
	// (portalVendorAccountErrRows, and portalResourceGroupErrRows for the
	// resource-group manager side of a shared account) answer with, so the two cannot
	// drift apart.
	codeVendorAccountsModuleDisabled  = "vendor_accounts.module_disabled"
	msgVendorAccountsModuleDisabled   = "vendor accounts are not enabled"
	codeVendorAccountsSharingDisabled = "vendor_accounts.sharing_disabled"
	msgVendorAccountsSharingDisabled  = "vendor account sharing is not enabled"
	codeVendorAccountForbidden        = "vendor_account.forbidden"
	codeVendorAccountNotShared        = "vendor_account.not_shared"

	codeVendorAccountListFailed    = "vendor_account.list_failed"
	codeVendorAccountCreateFailed  = "vendor_account.create_failed"
	codeVendorAccountGetFailed     = "vendor_account.get_failed"
	codeVendorAccountUpdateFailed  = "vendor_account.update_failed"
	codeVendorAccountDeleteFailed  = "vendor_account.delete_failed"
	codeVendorAccountConnectFailed = "vendor_account.connect_failed"
	codeVendorAccountCheckFailed   = "vendor_account.check_failed"
	codeVendorAccountRefreshFailed = "vendor_account.refresh_failed"

	// codeVendorAccountShareLevelFailed is the uncategorized fallback of the
	// share-level endpoint (PUT .../share-level): a store failure.
	codeVendorAccountShareLevelFailed = "vendor_account.share_level_failed"

	// codeVendorAccountAdminGroupsFailed is the uncategorized fallback of the
	// management-group assignment (PUT .../admin-groups) and
	// codeVendorAccountAdminGroupCandidatesFailed that of its candidate list: a store
	// failure.
	codeVendorAccountAdminGroupsFailed          = "vendor_account.admin_groups_failed"
	codeVendorAccountAdminGroupCandidatesFailed = "vendor_account.admin_group_candidates_failed"

	// codeVendorAccountResourceGroupsFailed is the uncategorized fallback of the owner's
	// resource-group self-service (GET/PUT/DELETE .../resource-groups[/{rgId}]): a store
	// failure.
	codeVendorAccountResourceGroupsFailed = "vendor_account.resource_groups_failed"

	// codeVendorAccountUsageRefreshFailed is the uncategorized fallback of the
	// dedicated usage refresh (POST .../usage/refresh): a store failure.
	codeVendorAccountUsageRefreshFailed = "vendor_account.usage_refresh_failed"
)

// handlePortalVendorAccountsEnabled reports two booleans for any portal user
// (gateway:use, GET-only): module_enabled, the vendor-accounts master flag (system
// setting vendor_accounts_enabled), and subscriptions_enabled, the
// vendor_subscriptions_enabled switch (default on) that gates only subscription
// accounts. It returns ONLY those two -- no routing mode or other setting -- so
// any user's shell can show or hide the "Anbieter" nav item, and the Anbieter page
// can explain why subscription accounts are inert, without being granted the
// system-scoped settings read. It answers while the module is off (that is its
// purpose), and the exact-path route registered for it wins over the
// "/api/portal/vendor-accounts/{id}" subtree (account ids are "va_"-prefixed, so
// none can be called "enabled").
func (s *Server) handlePortalVendorAccountsEnabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"module_enabled":        s.Portal.VendorAccountsEnabled(r.Context()),
		"subscriptions_enabled": s.Portal.VendorSubscriptionsEnabled(r.Context()),
	})
}

// handlePortalVendorAccounts is the vendor-account ("Anbieter") collection
// endpoint: GET lists the caller's OWN accounts plus, for a non-system caller, the
// shared accounts they manage as a co-manager, POST creates one owned by the caller.
// Both are gated only by the session-scope check (gateway:use); the per-account
// gates (owner, or a CM of a shared account; 404-no-leak) live in portal.Service, and
// the api_key is write-only -- no response ever carries it, to the owner or to a CM.
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
// "/api/portal/vendor-accounts/{id}/connect/device/{begin|poll}" (POST), the
// test-connection action "/api/portal/vendor-accounts/{id}/check" (POST), the
// model-discovery action "/api/portal/vendor-accounts/{id}/models/refresh" (POST),
// the usage-pull action "/api/portal/vendor-accounts/{id}/usage/refresh" (POST) and
// the share-level lifecycle "/api/portal/vendor-accounts/{id}/share-level" (PUT) and
// the management-group assignment "/api/portal/vendor-accounts/{id}/admin-groups" (PUT) and
// the owner's resource-group self-service "/api/portal/vendor-accounts/{id}/resource-groups"
// (GET) and ".../resource-groups/{rgId}" (PUT / DELETE).
// Any other deeper path is answered with the same 404 as an unknown id. The
// "/api/portal/vendor-accounts/" subtree route in server.go already covers every one
// of these sub-resources, so a new sub-route needs no registration of its own.
func (s *Server) handlePortalVendorAccountItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/vendor-accounts/"), "/")
	parts := strings.Split(rest, "/")
	if s.routeVendorAccountSubpath(w, r, token, parts) {
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
		s.handlePortalVendorAccountPatch(w, r, token, id)
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

// routeVendorAccountSubpath dispatches the sub-routes of
// /api/portal/vendor-accounts/{id}/... (the /check, /models/refresh, /usage/refresh,
// /share-level, /admin-groups and /resource-groups[/{rgId}] actions and the /connect/...
// family) and reports whether it handled the request. The item handler falls through to the {id}
// GET/PATCH/DELETE surface when this returns false.
func (s *Server) routeVendorAccountSubpath(w http.ResponseWriter, r *http.Request, token auth.Token, parts []string) bool {
	if len(parts) == 2 && parts[0] != "" && parts[1] == "check" {
		s.handlePortalVendorAccountCheck(w, r, token, parts[0])
		return true
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] == "models" && parts[2] == "refresh" {
		s.handlePortalVendorAccountModelsRefresh(w, r, token, parts[0])
		return true
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] == "usage" && parts[2] == "refresh" {
		s.handlePortalVendorAccountUsageRefresh(w, r, token, parts[0])
		return true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "share-level" {
		s.handlePortalVendorAccountShareLevel(w, r, token, parts[0])
		return true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "admin-groups" {
		s.handlePortalVendorAccountAdminGroups(w, r, token, parts[0])
		return true
	}
	if len(parts) >= 2 && parts[0] != "" && parts[1] == "resource-groups" {
		s.handlePortalVendorAccountResourceGroups(w, r, token, parts)
		return true
	}
	return s.routeVendorAccountConnectSubpath(w, r, token, parts)
}

// routeVendorAccountConnectSubpath dispatches the /connect/... sub-routes of
// /api/portal/vendor-accounts/{id}/... and reports whether it handled the request.
func (s *Server) routeVendorAccountConnectSubpath(w http.ResponseWriter, r *http.Request, token auth.Token, parts []string) bool {
	if len(parts) == 4 && parts[0] != "" && parts[1] == "connect" && parts[2] == "device" {
		switch parts[3] {
		case "begin":
			s.handlePortalVendorAccountConnectDeviceBegin(w, r, token, parts[0])
			return true
		case "poll":
			s.handlePortalVendorAccountConnectDevicePoll(w, r, token, parts[0])
			return true
		}
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] == "connect" {
		switch parts[2] {
		case "import":
			s.handlePortalVendorAccountConnectImport(w, r, token, parts[0])
			return true
		case "begin":
			s.handlePortalVendorAccountConnectBegin(w, r, token, parts[0])
			return true
		case "complete":
			s.handlePortalVendorAccountConnectComplete(w, r, token, parts[0])
			return true
		}
	}
	return false
}

// handlePortalVendorAccountPatch applies a PATCH to the {id} vendor account: it
// decodes the update body and writes the result (or the mapped error). Who may
// send which field is decided in portal.Service, per field and deny by default:
// name, status and model_prefix need the operate gate, every other field (the
// api_key above all) the credential gate, and base_url / auth_type are refused as
// create-only. The handler only decodes; it must never decide authorization.
func (s *Server) handlePortalVendorAccountPatch(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
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
}

// handlePortalVendorAccountShareLevel (PUT .../share-level) moves an account to the
// level in the body ({"level": "none"|"shared"|"full", "admin_group_ids": [...]}):
// the share-level lifecycle of shared vendor accounts. The handler only decodes; the
// service compares the requested level with the stored one and runs the RAISE (owner
// + a current admin role, >= 1 management group in admin_group_ids, a subscription
// account only to `shared`) or the LOWER (the credential gate: the owner always, a CM
// at `full`; to `none` it clears the management groups, the resource-group links and
// the containment root), and decides who may do which. The response is the
// credential-free account. While the vendor_account_sharing_enabled switch is off
// (default) it is a 409 vendor_accounts.sharing_disabled, whoever asks and whatever
// the id (the vendor_accounts_enabled module flag answers first); a subscription
// account raised while vendor_subscription_sharing_enabled is off is a 409
// vendor_accounts.subscription_sharing_disabled, and raised to `full` a 400
// vendor_account.subscription_share_capped; a stranger and an unknown id are the same
// 404, and a caller who can see the account but may not raise or lower it a 403.
func (s *Server) handlePortalVendorAccountShareLevel(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req portal.SetVendorAccountShareLevelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetVendorAccountShareLevel(r.Context(), token, id, req)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountShareLevelFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalVendorAccountAdminGroups (PUT .../admin-groups) replaces the management
// groups of a shared vendor account with the set in the body ({"admin_group_ids":
// [...]}), the vendor-account counterpart of PUT /api/portal/servers/{id}/admin-groups.
// The handler only decodes; the service decides who may (the credential gate's shape,
// the spec's "Management-group assignment" row: the owner always, a CM at `full` only),
// that the account is shared, that the set has >= 1 group the caller may name, and that
// the groups sit under the account's immutable containment root. The response is the
// credential-free account. While the vendor_account_sharing_enabled switch is off
// (default) it is a 409 vendor_accounts.sharing_disabled, whoever asks and whatever the
// id (the vendor_accounts_enabled module flag answers first); a stranger and an unknown
// id are the same 404, a CM of a `shared` account (who can see it) a 403, an account
// that is not shared a 400 vendor_account.not_shared, an empty set a 400
// vendor_account.admin_group_required, a group the caller may not name a 400
// vendor_account.admin_group_invalid, and a set under another root a 400
// vendor_account.root_mismatch.
func (s *Server) handlePortalVendorAccountAdminGroups(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
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
	dto, err := s.Portal.SetVendorAccountAdminGroups(r.Context(), token, id, body.AdminGroupIDs)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountAdminGroupsFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleVendorAccountAdminGroupCandidates backs GET
// /api/portal/vendor-account-admin-group-candidates: the admin-tier groups the caller
// may name as management groups of a vendor account (system scope -> every admin-tier
// group; anyone else -> the groups they manage vendor accounts through, so a plain user
// gets []), each with its parent system group. Drives the share dialog and the
// management-group editor; the mirror of handleServerAdminGroupCandidates. Like every
// vendor-account sharing endpoint it is a 409 while the module or the sharing flag is
// off.
func (s *Server) handleVendorAccountAdminGroupCandidates(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.VendorAccountAdminGroupCandidates(r.Context(), token)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountAdminGroupCandidatesFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handlePortalVendorAccountResourceGroups backs the OWNER's resource-group self-service of
// a shared vendor account (#183, spec 2026-10-15 sections 7 and 9; the vendor-account
// analogue of handlePortalServerResourceGroups):
//
//	GET    /api/portal/vendor-accounts/{id}/resource-groups        -> reachable groups + member flag
//	PUT    /api/portal/vendor-accounts/{id}/resource-groups/{rgId} -> join (idempotent)
//	DELETE /api/portal/vendor-accounts/{id}/resource-groups/{rgId} -> leave (idempotent)
//
// parts[0] = account id, parts[1] = "resource-groups", parts[2] = rgId (item paths); any
// deeper path is the same 404 as an unknown id. The handler only decodes; the service
// decides: the OWNER only (a CM who can see the account is a 403, everyone else the 404 of an
// unknown id), at `shared` or `full` (400 vendor_account.not_shared otherwise), joining only
// a group linked to an admin group the owner is a member of (404 resource_group.not_found
// otherwise) and whose root is the account's (400
// resource_group.vendor_account_system_group_mismatch, on a join). While the vendor-accounts
// module or the sharing switch is off it is a 409, whoever asks and whatever the ids.
func (s *Server) handlePortalVendorAccountResourceGroups(w http.ResponseWriter, r *http.Request, token auth.Token, parts []string) {
	accountID := parts[0]
	switch len(parts) {
	case 2:
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		data, err := s.Portal.VendorAccountResourceGroups(r.Context(), token, accountID)
		if err != nil {
			writePortalVendorAccountError(w, err, codeVendorAccountResourceGroupsFailed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	case 3:
		s.handlePortalVendorAccountResourceGroupItem(w, r, token, accountID, parts[2])
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeVendorAccountNotFound, msgVendorAccountNotFound, ""))
	}
}

// handlePortalVendorAccountResourceGroupItem backs PUT (join) and DELETE (leave) on
// /api/portal/vendor-accounts/{id}/resource-groups/{rgId}; both answer {"ok": true}.
func (s *Server) handlePortalVendorAccountResourceGroupItem(w http.ResponseWriter, r *http.Request, token auth.Token, accountID, rgID string) {
	var err error
	switch r.Method {
	case http.MethodPut:
		err = s.Portal.AddVendorAccountToResourceGroup(r.Context(), token, accountID, rgID)
	case http.MethodDelete:
		err = s.Portal.RemoveVendorAccountFromResourceGroup(r.Context(), token, accountID, rgID)
	default:
		w.Header().Set("Allow", http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		return
	}
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountResourceGroupsFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePortalVendorAccountCheck (POST .../check) is the explicit "test
// connection" action: the gateway asks the vendor whether the account's stored
// credential is accepted and returns the verdict {status, detail, checked_at}
// (status is valid, invalid or unverifiable). The request has no body, the call
// changes nothing, and no credential is ever returned. Gated by the
// vendor_accounts_enabled master flag and the operate gate (both in
// portal.Service): the owner, or a CM of a shared/full account; any other
// principal gets the same 404 as an unknown id. A subscription account is
// owner-only: a CM who can see it gets a 403.
func (s *Server) handlePortalVendorAccountCheck(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	check, err := s.Portal.TestVendorAccountConnection(r.Context(), token, id)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountCheckFailed)
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// vendorAccountModelsRefreshResponse is the response of POST
// /api/portal/vendor-accounts/{id}/models/refresh: the credential-free account as
// it now serves its models, and what the refresh did (status ok or unverifiable,
// how many models were stored, and a short phrase for the portal to show).
type vendorAccountModelsRefreshResponse struct {
	Account portal.VendorAccountDTO `json:"account"`
	Refresh portal.RefreshResult    `json:"refresh"`
}

// handlePortalVendorAccountModelsRefresh (POST .../models/refresh) is the explicit
// "refresh models" action: the gateway asks the vendor which models the account's
// stored credential can use and, when it answers with a usable list, replaces the
// account's models with it (under the account's prefix). It is fail-soft: a vendor
// that cannot be asked leaves the models as they were and is still a 200, with
// refresh.status "unverifiable" and the reason in refresh.detail. The request has no
// body and no credential is ever returned. Gated by the vendor_accounts_enabled
// master flag and the operate gate (both in portal.Service): the owner, or a CM of
// a shared/full account; any other principal gets the same 404 as an unknown id; a
// stored credential that cannot be opened is a 409
// vendor_account.credential_unreadable. A subscription account is owner-only (the
// refresh can rotate its single-use token): a CM who can see it gets a 403.
func (s *Server) handlePortalVendorAccountModelsRefresh(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	dto, result, err := s.Portal.RefreshVendorAccountModels(r.Context(), token, id)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountRefreshFailed)
		return
	}
	writeJSON(w, http.StatusOK, vendorAccountModelsRefreshResponse{Account: dto, Refresh: result})
}

// vendorAccountUsageRefreshResponse is the response of POST
// /api/portal/vendor-accounts/{id}/usage/refresh: the account's stored usage
// snapshot as it is after the refresh (null when none has been stored, which is
// always the case for an account with no usage to show), and what the refresh did
// (status ok, fresh, unverifiable or unsupported, and a short phrase for the portal
// to show). Both keys are always on the wire.
type vendorAccountUsageRefreshResponse struct {
	Usage   *portal.VendorAccountUsageDTO   `json:"usage"`
	Refresh portal.VendorUsageRefreshResult `json:"refresh"`
}

// vendorUsageForce reports whether the request asks for a forced (manual) usage
// refresh: ?force=1 (or any other value strconv.ParseBool reads as true). Anything
// else -- absent, false, empty, unparsable -- is the lazy default, so the vendor
// stays protected by the TTL unless the caller clearly asked to bypass it.
func vendorUsageForce(r *http.Request) bool {
	force, err := strconv.ParseBool(r.URL.Query().Get("force"))
	return err == nil && force
}

// handlePortalVendorAccountUsageRefresh (POST .../usage/refresh) is the dedicated
// usage-pull action of the Usage & Limits panel: the gateway asks the vendor for the
// account's usage snapshot and stores it, WITHOUT the model discovery the models
// refresh runs. It serves both the panel's on-view lazy pull and its refresh button.
// A plain call is the lazy one: when the account's usage was pulled less than the
// lazy TTL ago the vendor is not asked and the stored snapshot is answered with
// refresh.status "fresh". ?force=1 (the button) bypasses the TTL and always asks.
// It is fail-soft: a vendor that cannot be asked, or an account with no usable
// token or API key, leaves the stored snapshot as it was and is still a 200, with
// refresh.status "unverifiable" and the reason in refresh.detail; an account with
// no active pull (anything but an OpenAI subscription or an OpenRouter/Kilo
// api-key account) is a 200 "unsupported" and opens no credential. The request has
// no body and no credential is ever returned.
// Gated by the vendor_accounts_enabled master flag and the operate gate (both in
// portal.Service): the owner, or a CM of a shared/full account; any other principal
// gets the same 404 as an unknown id; a stored credential that cannot be opened is a
// 409 vendor_account.credential_unreadable. A subscription account is owner-only
// (the pull renews its token first), the lazy on-view pull included: a CM who can
// see it gets a 403.
func (s *Server) handlePortalVendorAccountUsageRefresh(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	maxAge := portal.VendorUsageLazyTTL
	if vendorUsageForce(r) {
		maxAge = 0
	}
	usage, result, err := s.Portal.RefreshVendorAccountUsage(r.Context(), token, id, maxAge)
	if err != nil {
		writePortalVendorAccountError(w, err, codeVendorAccountUsageRefreshFailed)
		return
	}
	writeJSON(w, http.StatusOK, vendorAccountUsageRefreshResponse{Usage: usage, Refresh: result})
}

// vendorAccountConnectCompleteRequest is the body of POST
// /api/portal/vendor-accounts/{id}/connect/complete: whatever the user pasted
// back from the vendor -- a code, "code#state", or a whole callback URL.
type vendorAccountConnectCompleteRequest struct {
	Code string `json:"code"`
}

// handlePortalVendorAccountConnectImport (POST .../connect/import) connects a
// subscription account from tokens the user already holds. Gated by the
// vendor_accounts_enabled master flag and the credential gate (both in
// portal.Service): the owner, or a CM of a `full` account (a CM of a `shared` one
// is a 403). The begin/complete and device sub-resources are gated the same way.
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
	{err: portal.ErrVendorAccountsDisabled, status: http.StatusConflict, code: codeVendorAccountsModuleDisabled, msg: msgVendorAccountsModuleDisabled},
	// The vendor_subscriptions_enabled switch is off: creating or connecting a
	// SUBSCRIPTION account is a 409 too (api_key accounts are never refused).
	{err: portal.ErrVendorSubscriptionsDisabled, status: http.StatusConflict, code: "vendor_accounts.subscriptions_disabled", msg: "vendor subscription accounts are not enabled"},
	// The vendor_account_sharing_enabled switch is off: every share / management /
	// resource-group endpoint is a 409 (the account does not even get looked at). The
	// subscription switch narrows it: raising a SUBSCRIPTION account while
	// vendor_subscription_sharing_enabled is off is a 409 too.
	{err: portal.ErrVendorAccountSharingDisabled, status: http.StatusConflict, code: codeVendorAccountsSharingDisabled, msg: msgVendorAccountsSharingDisabled},
	{err: portal.ErrVendorSubscriptionSharingDisabled, status: http.StatusConflict, code: "vendor_accounts.subscription_sharing_disabled", msg: "sharing subscription accounts is not enabled"},
	{err: portal.ErrVendorAccountNotFound, status: http.StatusNotFound, code: portal.CodeVendorAccountNotFound, msg: msgVendorAccountNotFound},
	{err: portal.ErrVendorAccountNameRequired, status: http.StatusBadRequest, code: "vendor_account.name_required", msg: "vendor account name is required"},
	{err: portal.ErrVendorAccountVendorInvalid, status: http.StatusBadRequest, code: "vendor_account.vendor_invalid", msg: "vendor account vendor is invalid"},
	{err: portal.ErrVendorAccountAuthTypeInvalid, status: http.StatusBadRequest, code: "vendor_account.auth_type_invalid", msg: "vendor account auth type is invalid"},
	{err: portal.ErrVendorAccountStatusInvalid, status: http.StatusBadRequest, code: "vendor_account.status_invalid", msg: "vendor account status is invalid"},
	{err: portal.ErrVendorAccountAPIKeyNotAllowed, status: http.StatusBadRequest, code: "vendor_account.api_key_not_allowed", msg: "an api key can only be set on an api_key account"},
	{err: portal.ErrVendorAccountAPIKeyInvalid, status: http.StatusBadRequest, code: "vendor_account.api_key_invalid", msg: "api key must not be blank; send an empty string to clear it"},
	{err: portal.ErrVendorAccountModelPrefixInvalid, status: http.StatusBadRequest, code: "vendor_account.model_prefix_invalid", msg: "model prefix must be at most 64 characters from A-Z a-z 0-9 - _ . ~ : / @ + and must not contain .."},
	{err: portal.ErrVendorAccountBaseURLRequired, status: http.StatusBadRequest, code: "vendor_account.base_url_required", msg: "a base URL is required for a custom OpenAI-compatible account"},
	{err: portal.ErrVendorAccountBaseURLInvalid, status: http.StatusBadRequest, code: "vendor_account.base_url_invalid", msg: "the base URL must be an https URL without credentials, query or fragment"},
	{err: portal.ErrVendorAccountForbidden, status: http.StatusForbidden, code: codeVendorAccountForbidden, msg: notAllowedMsg},
	// base_url and auth_type are create-only: a PATCH that tries to change either is
	// refused for every caller (a 400; the gate has already answered 404/403 for a
	// caller who may not touch the credential).
	{err: portal.ErrVendorAccountFieldImmutable, status: http.StatusBadRequest, code: "vendor_account.field_immutable", msg: "base_url and auth_type are fixed when the account is created and cannot be changed"},
	// Share level (PUT .../share-level). All 400s: the request is wrong for this
	// account, never a 401 (the portal reads that as an expired session). They are
	// answered only AFTER the 404/403 gate, so none of them confirms an account to a
	// caller who may not see it (the level's own shape is account-independent).
	{err: portal.ErrVendorAccountShareLevelInvalid, status: http.StatusBadRequest, code: "vendor_account.share_level_invalid", msg: "the share level must be none, shared or full"},
	{err: portal.ErrVendorAccountShareLevelUnchanged, status: http.StatusBadRequest, code: "vendor_account.share_level_unchanged", msg: "the request does not change the share level in the direction asked for; use a higher level to raise or a lower one to lower"},
	{err: portal.ErrVendorAccountSubscriptionShareCapped, status: http.StatusBadRequest, code: "vendor_account.subscription_share_capped", msg: "a subscription account can be shared at most at the shared level, never full"},
	{err: portal.ErrVendorAccountAdminGroupRequired, status: http.StatusBadRequest, code: "vendor_account.admin_group_required", msg: "at least one management group is required to share an account"},
	{err: portal.ErrVendorAccountAdminGroupInvalid, status: http.StatusBadRequest, code: "vendor_account.admin_group_invalid", msg: "management group is invalid"},
	{err: portal.ErrVendorAccountAdminGroupParentMismatch, status: http.StatusBadRequest, code: "vendor_account.admin_group_parent_mismatch", msg: "management groups must share one parent system group"},
	{err: portal.ErrVendorAccountRootMismatch, status: http.StatusBadRequest, code: "vendor_account.root_mismatch", msg: "management groups must sit under the system group the account is already shared in"},
	// Management groups (PUT .../admin-groups) can only be assigned to an account that
	// is shared; a 400 too, answered only to a caller the gate has let through.
	{err: portal.ErrVendorAccountNotShared, status: http.StatusBadRequest, code: codeVendorAccountNotShared, msg: "management groups can only be assigned to a shared account; raise its share level first"},
	// Resource-group self-service (GET/PUT/DELETE .../resource-groups[/{rgId}]): the
	// reachable-group gate (also what a non-existent group id gets, 404-no-leak) and the
	// containment check of a join, a 400 like the root mismatch of the management groups.
	// Both are answered only after the account gate, so neither confirms an account.
	{err: portal.ErrResourceGroupNotFound, status: http.StatusNotFound, code: portal.CodeResourceGroupNotFound, msg: msgResourceGroupNotFound},
	{err: portal.ErrResourceGroupVendorAccountSystemGroupMismatch, status: http.StatusBadRequest, code: codeResourceGroupVendorAccountSystemGroupMismatch, msg: msgResourceGroupVendorAccountSystemGroupMismatch},
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
	// A token import the vendor definitively rejected (nothing was stored): a 400,
	// never a 401, because the portal treats a 401 from this API as an expired
	// session and would log the user out instead of showing the message.
	{err: portal.ErrVendorAccountConnectInvalidCredentials, status: http.StatusBadRequest, code: "vendor_account.connect_invalid_credentials", msg: "the vendor rejected the access token; check that it is current and was copied completely"},
	{err: portal.ErrVendorAccountConnectUpstream, status: http.StatusBadGateway, code: "vendor_account.connect_upstream_failed", msg: "the vendor could not be reached or answered unexpectedly; try again later"},
	{err: portal.ErrVendorAccountConnectKeyRequired, status: http.StatusBadRequest, code: "vendor_account.connect_key_required", msg: "an encryption key is required to store a vendor subscription on a disk-backed store"},
	// Device-code connect (OpenAI only). Both are 400s, never a 401 (the portal
	// treats a 401 from this API as an expired session).
	{err: portal.ErrVendorAccountDeviceUnsupported, status: http.StatusBadRequest, code: "vendor_account.device_not_supported", msg: "the device connect flow is available for OpenAI accounts only"},
	{err: portal.ErrVendorAccountDeviceConnectState, status: http.StatusBadRequest, code: "vendor_account.device_connect_state", msg: "no device connect is in progress for this account or it has expired; start the device connect again"},
	// The connection test (POST .../check) could not OPEN the stored credential: the
	// encryption key is lost, or the sealed blob no longer decrypts. A 409 -- the
	// account is in a state that cannot be checked -- never a 400 (nothing in the
	// request is wrong), a 401 (the portal reads that as an expired session) or a
	// 500 (this is a known, deterministic state). The row precedes the
	// capture.ErrKeyRequired one because the error wraps it for a lost key, and that
	// row's message talks about STORING an api key.
	{err: portal.ErrVendorAccountCredentialUnreadable, status: http.StatusConflict, code: "vendor_account.credential_unreadable", msg: "the stored credential could not be read; reconnect the account"},
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
