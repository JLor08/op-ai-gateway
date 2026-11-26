// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
)

const (
	pathPortalVendorGroups = "/api/portal/vendor-account-groups"

	// codeVendorGroupFailed is the uncategorized fallback of every group endpoint: a
	// store failure.
	codeVendorGroupFailed = "vendor_account_group.failed"
)

// portalVendorGroupErrRows are the group endpoints' error rows (checked before
// sharedErrorMap): the module flag is a 409, an unknown or foreign group a 404 and
// every validation failure a 400.
var portalVendorGroupErrRows = []errRow{
	{err: portal.ErrVendorAccountsDisabled, status: http.StatusConflict, code: codeVendorAccountsModuleDisabled, msg: msgVendorAccountsModuleDisabled},
	{err: portal.ErrVendorGroupNotFound, status: http.StatusNotFound, code: portal.CodeVendorGroupNotFound, msg: "vendor account group not found"},
	{err: portal.ErrVendorGroupForbidden, status: http.StatusForbidden, code: "vendor_account_group.forbidden", msg: notAllowedMsg},
	{err: portal.ErrVendorGroupNameRequired, status: http.StatusBadRequest, code: "vendor_account_group.name_required", msg: "vendor account group name is required"},
	{err: portal.ErrVendorGroupVendorInvalid, status: http.StatusBadRequest, code: "vendor_account_group.vendor_invalid", msg: "vendor account group vendor is invalid"},
	{err: portal.ErrVendorGroupModelPrefixInvalid, status: http.StatusBadRequest, code: "vendor_account_group.model_prefix_invalid", msg: "model prefix must be at most 64 characters from A-Z a-z 0-9 - _ . ~ : / @ + and must not contain .."},
	{err: portal.ErrVendorGroupStatusInvalid, status: http.StatusBadRequest, code: "vendor_account_group.status_invalid", msg: "vendor account group status must be active or disabled"},
	{err: portal.ErrVendorGroupThresholdInvalid, status: http.StatusBadRequest, code: "vendor_account_group.threshold_invalid", msg: "thresholds must be whole percentages from 1 to 100"},
	{err: portal.ErrVendorGroupMemberVendorMismatch, status: http.StatusBadRequest, code: "vendor_account_group.member_vendor_mismatch", msg: "every member must be an account of the group's vendor"},
	{err: portal.ErrVendorGroupMemberUnreachable, status: http.StatusBadRequest, code: "vendor_account_group.member_unreachable", msg: "a member is not an account you can use"},
	{err: portal.ErrVendorGroupMemberDuplicate, status: http.StatusBadRequest, code: "vendor_account_group.member_duplicate", msg: "an account can be a member of a group only once"},
	{err: portal.ErrVendorGroupPrefixConflict, status: http.StatusBadRequest, code: "vendor_account_group.prefix_conflict", msg: "the group's model names collide with another of your accounts or groups"},
}

func writePortalVendorGroupError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalVendorGroupErrRows, http.StatusInternalServerError, codeVendorGroupFailed, "vendor account group request failed")
}

// handlePortalVendorGroups is the collection endpoint of vendor account groups:
// GET lists the caller's own groups with their member states, POST creates one.
// Same session-scope check as the vendor-account endpoints (gateway:use, CSRF on
// writes); portal.Service owns the module flag and the owner-only gate.
func (s *Server) handlePortalVendorGroups(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListVendorAccountGroups(r.Context(), token)
		if err != nil {
			writePortalVendorGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		var req portal.CreateVendorAccountGroupRequest
		if !decodeVendorGroupBody(w, r, &req) {
			return
		}
		dto, err := s.Portal.CreateVendorAccountGroup(r.Context(), token, req)
		if err != nil {
			writePortalVendorGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		writeVendorGroupMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

// handlePortalVendorGroupItem serves "/api/portal/vendor-account-groups/{id}" (GET /
// PATCH / DELETE) and "/api/portal/vendor-account-groups/{id}/members" (PUT). Any
// other path is a 404.
func (s *Server) handlePortalVendorGroupItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, pathPortalVendorGroups+"/"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		s.handlePortalVendorGroup(w, r, token, parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "members":
		s.handlePortalVendorGroupMembers(w, r, token, parts[0])
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeVendorGroupNotFound, "vendor account group not found", ""))
	}
}

func (s *Server) handlePortalVendorGroup(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetVendorAccountGroup(r.Context(), token, id)
		if err != nil {
			writePortalVendorGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPatch:
		var req portal.UpdateVendorAccountGroupRequest
		if !decodeVendorGroupBody(w, r, &req) {
			return
		}
		dto, err := s.Portal.UpdateVendorAccountGroup(r.Context(), token, id, req)
		if err != nil {
			writePortalVendorGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteVendorAccountGroup(r.Context(), token, id); err != nil {
			writePortalVendorGroupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeVendorGroupMethodNotAllowed(w, http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
	}
}

// handlePortalVendorGroupMembers (PUT .../members) replaces the member set and order
// with {"account_ids": [...]} and answers with the group and its member states.
func (s *Server) handlePortalVendorGroupMembers(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if r.Method != http.MethodPut {
		writeVendorGroupMethodNotAllowed(w, http.MethodPut)
		return
	}
	var req portal.SetVendorAccountGroupMembersRequest
	if !decodeVendorGroupBody(w, r, &req) {
		return
	}
	dto, err := s.Portal.SetVendorAccountGroupMembers(r.Context(), token, id, req.AccountIDs)
	if err != nil {
		writePortalVendorGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func decodeVendorGroupBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, ok := readRawJSON(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return false
	}
	return true
}

func writeVendorGroupMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
}

// VendorLimitCooldowns is the Server's process-local vendor limit-cooldown registry,
// the very one its resolver reads. cmd/gateway hands it to the portal service so the
// group member status shows the cooldowns routing applies.
func (s *Server) VendorLimitCooldowns() *routing.VendorLimitCooldowns {
	return s.vendorCooldowns
}
