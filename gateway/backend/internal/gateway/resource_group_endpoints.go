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
	msgResourceGroupNotFound = "resource group not found"

	codeResourceGroupUpdateFailed = "resource_group.update_failed"
)

// handlePortalResourceGroups is the resource-group collection endpoint
// (Resource Groups Phase 1, Task 4, spec 2026-08-11 -- mirrors
// handlePortalServices exactly, minus the delegate model): GET lists
// (system-scope -> every resource group; anyone else -> only the resource
// groups linked to an admin group they may manage RESOURCES through, via
// portal.Service.ListResourceGroups) and POST creates (the *Create* object
// gate -- system OR can_manage_resources reach into >=1 admin group --
// enforced INSIDE portal.Service.CreateResourceGroup, so a principal with
// neither gets ErrResourceGroupForbidden -> 403 here, not a 401/404). Both
// are gated only by the session-scope check (gateway:use); the *Read*/
// *Write* OBJECT-level gate (authorizeResourceGroup) lives in
// portal.Service.
func (s *Server) handlePortalResourceGroups(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		groups, err := s.Portal.ListResourceGroups(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("resource_group.list_failed", "resource group list failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": groups})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateResourceGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateResourceGroup(r.Context(), token, req)
		if err != nil {
			writePortalResourceGroupError(w, err, "resource_group.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalResourceGroupItem dispatches every
// "/api/portal/resource-groups/{id}[/...]" route (mirrors
// handlePortalServiceItem/handlePortalServerItem's path-parsing shape):
// "/{id}" (single resource group, GET/PATCH/DELETE), "/{id}/admin-groups"
// (admin-group linkage editor, PUT-only), "/{id}/servers" (server
// membership editor, PUT-only -- Task 5) plus the sibling
// "/{id}/server-candidates" (GET-only, the membership-editor picker), and
// "/{id}/provisions" (provisioned-for target editor, GET+PUT -- Resource
// Groups Phase 2, Task 5, spec 2026-08-12-resource-groups-phase-2-provisioning)
// plus its sibling "/{id}/provision-candidates" (GET-only, the
// provisioning-editor's combined users/user_groups/admin_groups/services
// picker).
func (s *Server) handlePortalResourceGroupItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/resource-groups/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeResourceGroupNotFound, msgResourceGroupNotFound, ""))
		return
	}
	switch {
	case len(parts) == 1:
		s.handlePortalResourceGroupSingle(w, r, token, id)
	case len(parts) == 2 && parts[1] == "admin-groups":
		s.handlePortalResourceGroupAdminGroups(w, r, token, id)
	case len(parts) == 2 && parts[1] == "servers":
		s.handlePortalResourceGroupServers(w, r, token, id)
	case len(parts) == 2 && parts[1] == "server-candidates":
		s.handlePortalResourceGroupServerCandidates(w, r, token, id)
	case len(parts) == 2 && parts[1] == "provisions":
		s.handlePortalResourceGroupProvisions(w, r, token, id)
	case len(parts) == 2 && parts[1] == "provision-candidates":
		s.handlePortalResourceGroupProvisionCandidates(w, r, token, id)
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeResourceGroupNotFound, msgResourceGroupNotFound, ""))
	}
}

// handlePortalResourceGroupSingle backs GET/PATCH/DELETE
// /api/portal/resource-groups/{id} -- all three enforced inside
// portal.Service via authorizeResourceGroup (404-no-leak: a stranger and an
// unknown id are indistinguishable). PATCH mirrors handlePortalServerItem's
// PATCH (a resource group has no separate Settings/Tokens split, unlike
// Service, so there is no PUT-based full-replace route here).
func (s *Server) handlePortalResourceGroupSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetResourceGroup(r.Context(), token, id)
		if err != nil {
			writePortalResourceGroupError(w, err, "resource_group.get_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateResourceGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateResourceGroup(r.Context(), token, id, req)
		if err != nil {
			writePortalResourceGroupError(w, err, codeResourceGroupUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteResourceGroup(r.Context(), token, id); err != nil {
			writePortalResourceGroupError(w, err, "resource_group.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalResourceGroupAdminGroups backs PUT
// /api/portal/resource-groups/{id}/admin-groups: replaces the resource
// group's linked admin-group set (mirrors handlePortalServerAdminGroups/
// handlePortalServiceAdminGroups exactly).
func (s *Server) handlePortalResourceGroupAdminGroups(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
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
	dto, err := s.Portal.SetResourceGroupAdminGroups(r.Context(), token, id, body.AdminGroupIDs)
	if err != nil {
		writePortalResourceGroupError(w, err, codeResourceGroupUpdateFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalResourceGroupServers backs PUT
// /api/portal/resource-groups/{id}/servers: replaces the resource group's
// member-server set (Task 5, spec 2026-08-11 -- mirrors
// handlePortalResourceGroupAdminGroups exactly, keyed on "servers" instead of
// "admin_group_ids"). The dual-manage + same-system-group-containment gate
// lives inside portal.Service.SetResourceGroupServers; here we only map its
// two new sentinel errors (writePortalResourceGroupError below).
func (s *Server) handlePortalResourceGroupServers(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		ServerIDs []string `json:"server_ids"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetResourceGroupServers(r.Context(), token, id, body.ServerIDs)
	if err != nil {
		writePortalResourceGroupError(w, err, codeResourceGroupUpdateFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalResourceGroupServerCandidates backs GET
// /api/portal/resource-groups/{id}/server-candidates: the AI-servers the
// caller may enter into resource group id (drives the membership-editor
// picker; mirrors handleResourceGroupAdminGroupCandidates below, but scoped
// to ONE resource group -- the containment filter needs its
// SystemGroupID -- so it lives under the item route, not as a top-level
// collection endpoint).
func (s *Server) handlePortalResourceGroupServerCandidates(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ResourceGroupServerCandidates(r.Context(), token, id)
	if err != nil {
		writePortalResourceGroupError(w, err, "resource_group.server_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handlePortalResourceGroupProvisions backs GET/PUT
// /api/portal/resource-groups/{id}/provisions (Resource Groups Phase 2,
// Task 5, spec 2026-08-12-resource-groups-phase-2-provisioning): GET lists
// the resource group's provisioned-for targets (kind + target_id + resolved
// display name); PUT atomically REPLACES the whole set. Both methods are
// gated inside portal.Service via authorizeResourceGroup (404-no-leak,
// mirrors every other resource-group item route); PUT additionally
// validates every target against the caller's OWN visible landscape --
// writePortalResourceGroupError below maps the resulting
// ErrResourceGroupProvisionTargetInvalid sentinel to 400.
func (s *Server) handlePortalResourceGroupProvisions(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.Portal.ResourceGroupProvisionsView(r.Context(), token, id)
		if err != nil {
			writePortalResourceGroupError(w, err, "resource_group.provisions_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var body struct {
			Provisions []struct {
				Kind     string `json:"kind"`
				TargetID string `json:"target_id"`
			} `json:"provisions"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		provisions := make([]routing.ResourceGroupProvision, 0, len(body.Provisions))
		for _, p := range body.Provisions {
			provisions = append(provisions, routing.ResourceGroupProvision{Kind: p.Kind, TargetID: p.TargetID})
		}
		if err := s.Portal.SetResourceGroupProvisions(r.Context(), token, id, provisions); err != nil {
			writePortalResourceGroupError(w, err, codeResourceGroupUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalResourceGroupProvisionCandidates backs GET
// /api/portal/resource-groups/{id}/provision-candidates: the combined
// users/user_groups/admin_groups/services candidate set for resource group
// id's provisioning editor (mirrors handlePortalResourceGroupServerCandidates
// exactly, one level up -- gated per-resource-group via
// authorizeResourceGroup so only a manager of THIS resource group ever sees
// its candidates).
func (s *Server) handlePortalResourceGroupProvisionCandidates(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ResourceGroupProvisionCandidates(r.Context(), token, id)
	if err != nil {
		writePortalResourceGroupError(w, err, "resource_group.provision_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// handleResourceGroupAdminGroupCandidates backs GET
// /api/portal/resource-group-admin-group-candidates: the admin-tier groups
// the caller may create/link a resource group into (system scope -> every
// admin-tier group; anyone else -> the groups they may manage resource
// groups through). Drives the create-resource-group / linkage-editor picker
// (mirrors handleServerAdminGroupCandidates/handleServiceAdminGroupCandidates
// exactly).
func (s *Server) handleResourceGroupAdminGroupCandidates(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ResourceGroupAdminGroupCandidates(r.Context(), token)
	if err != nil {
		writePortalResourceGroupError(w, err, "resource_group.admin_group_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// writePortalResourceGroupError maps portal.Service's resource-group
// sentinel errors onto HTTP status codes: Forbidden->403,
// NotFound->404, everything admin-group-linkage/validation-shaped->400,
// else defaultCode->500. Every branch uses a STATIC message -- never
// err.Error() -- so no internal detail ever leaks to the client (mirrors
// writePortalServerError/writePortalServiceError).
// portalResourceGroupErrRows are writePortalResourceGroupError's
// mapper-specific rows; portal.ErrServerNotFound maps identically elsewhere
// and lives in sharedErrorMap instead.
var portalResourceGroupErrRows = []errRow{
	{err: portal.ErrResourceGroupForbidden, status: http.StatusForbidden, code: "resource_group.forbidden", msg: "not allowed"},
	{err: portal.ErrResourceGroupValidation, status: http.StatusBadRequest, code: "resource_group.validation_failed", msg: "resource group request is invalid"},
	{err: portal.ErrResourceGroupAdminGroupRequired, status: http.StatusBadRequest, code: "resource_group.admin_group_required", msg: "at least one admin group is required"},
	{err: portal.ErrResourceGroupAdminGroupInvalid, status: http.StatusBadRequest, code: "resource_group.admin_group_invalid", msg: "admin group is invalid"},
	{err: portal.ErrResourceGroupAdminGroupParentMismatch, status: http.StatusBadRequest, code: "resource_group.admin_group_parent_mismatch", msg: "admin groups must share one parent system group"},
	{err: portal.ErrResourceGroupServerForbidden, status: http.StatusNotFound, code: "resource_group.server_forbidden", msg: "server not found"},
	{err: portal.ErrResourceGroupServerSystemGroupMismatch, status: http.StatusBadRequest, code: "resource_group.server_system_group_mismatch", msg: "server must belong to the resource group's system group"},
	{err: portal.ErrResourceGroupProvisionTargetInvalid, status: http.StatusBadRequest, code: "resource_group.provision_target_invalid", msg: "provisioning target is invalid or not visible to you"},
	{err: portal.ErrResourceGroupNotFound, status: http.StatusNotFound, code: portal.CodeResourceGroupNotFound, msg: msgResourceGroupNotFound},
}

func writePortalResourceGroupError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, portalResourceGroupErrRows, http.StatusInternalServerError, defaultCode, "resource group request failed")
}
