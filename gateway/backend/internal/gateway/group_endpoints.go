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

const (
	codeGroupNotFound = "group.not_found"
	msgGroupNotFound  = "group not found"
)

// createGroupRequest is the POST /api/portal/groups body.
type createGroupRequest struct {
	Tier          string `json:"tier"`
	Name          string `json:"name"`
	ParentGroupID string `json:"parent_group_id"`
	// OwnerUserID is admin-tier-only: a system_admin caller sets it to create
	// the group FOR another admin (see portal.CreateGroupInput.OwnerUserID).
	OwnerUserID string `json:"owner_user_id"`
}

// patchGroupRequest is the PATCH /api/portal/groups/{id} body. It carries
// EITHER a rename (name) OR an ownership transfer (owner_user_id) -- pointers
// so "field present but empty" (e.g. a client bug sending name:"") is
// distinguishable from "field absent" (nothing to do for that field).
type patchGroupRequest struct {
	Name        *string `json:"name"`
	OwnerUserID *string `json:"owner_user_id"`
}

// groupMembersRequest is the POST /api/portal/groups/{id}/members body.
type groupMembersRequest struct {
	UserIDs []string `json:"user_ids"`
}

// groupManagerRequest is the POST /api/portal/groups/{id}/managers body
// (promote user_id to co-manager, with the given per-permission flags) AND
// the PATCH /api/portal/groups/{id}/managers/{userId} body (narrow/widen an
// EXISTING co-manager's flags -- user_id is ignored there, the path segment
// is authoritative). Per-Admin-Group co-manager permissions, spec 2026-08-10
// + Phase B 2026-08-10 (CanManageServers) + Phase C 2026-08-10
// (CanManageServices) + Resource Groups Phase 1 2026-08-11
// (CanManageResources).
type groupManagerRequest struct {
	UserID             string `json:"user_id"`
	CanManageUsers     bool   `json:"can_manage_users"`
	CanManageGroup     bool   `json:"can_manage_group"`
	CanManageServers   bool   `json:"can_manage_servers"`
	CanManageServices  bool   `json:"can_manage_services"`
	CanManageResources bool   `json:"can_manage_resources"`
}

// handlePortalGroups backs GET (list the caller's group landscape) / POST
// (create a group) on the exact path /api/portal/groups. Internal tier
// authorization (system/admin/user, parent-membership, name uniqueness) lives
// in portal.Service -- this handler only translates the wire shape.
func (s *Server) handlePortalGroups(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.ListGroups(r.Context(), token)
		if err != nil {
			writeGroupError(w, err, "group.list_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req createGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateGroup(r.Context(), token, portal.CreateGroupInput{
			Tier:          req.Tier,
			Name:          req.Name,
			ParentGroupID: req.ParentGroupID,
			OwnerUserID:   req.OwnerUserID,
		})
		if err != nil {
			writeGroupError(w, err, "group.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handleAdminOwnerCandidates backs GET /api/portal/admin-owner-candidates:
// the eligible owners a system-admin may assign a new admin group to.
func (s *Server) handleAdminOwnerCandidates(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		return
	}
	data, err := s.Portal.AdminOwnerCandidates(r.Context(), token)
	if err != nil {
		writeGroupError(w, err, "group.owner_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handlePortalGroupInvitations backs GET /api/portal/groups/invitations (the
// caller's own pending user-tier group invitations). Registered as its OWN
// exact route in routes() -- Go's ServeMux prefers the longer, exact pattern
// over the "/api/portal/groups/" subtree, so this never falls into
// handlePortalGroupItem and gets misread as a group id of "invitations".
func (s *Server) handlePortalGroupInvitations(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	list, err := s.Portal.ListInvitations(r.Context(), token)
	if err != nil {
		writeGroupError(w, err, "group.invitations_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// handlePortalGroupItem is the subtree dispatcher for /api/portal/groups/{id}
// and its sub-resources (candidates/members/managers/accept/decline), mirroring
// handlePortalServiceItem's parts-based split.
func (s *Server) handlePortalGroupItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/groups/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeGroupNotFound, msgGroupNotFound, ""))
		return
	}
	switch {
	case len(parts) == 1:
		s.handlePortalGroupSingle(w, r, token, id)
	case len(parts) == 2 && parts[1] == "candidates":
		s.handlePortalGroupCandidates(w, r, token, id)
	case len(parts) == 2 && parts[1] == "members":
		s.handlePortalGroupMembers(w, r, token, id)
	case len(parts) == 3 && parts[1] == "members" && parts[2] != "":
		s.handlePortalGroupMemberSingle(w, r, token, id, parts[2])
	case len(parts) == 2 && parts[1] == "managers":
		s.handlePortalGroupManagers(w, r, token, id)
	case len(parts) == 3 && parts[1] == "managers" && parts[2] != "":
		s.handlePortalGroupManagerSingle(w, r, token, id, parts[2])
	case len(parts) == 2 && parts[1] == "accept":
		s.handlePortalGroupRespond(w, r, token, id, true)
	case len(parts) == 2 && parts[1] == "decline":
		s.handlePortalGroupRespond(w, r, token, id, false)
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(codeGroupNotFound, msgGroupNotFound, ""))
	}
}

// handlePortalGroupSingle backs PATCH (rename OR transfer ownership) / DELETE
// on /api/portal/groups/{id}. There is deliberately NO GET here (spec §11
// exposes a single group's detail only via the ListGroups landscape).
//
// PATCH dispatch: a body carrying `name` renames; a body carrying
// `owner_user_id` transfers ownership; a body carrying BOTH is rejected as
// ambiguous (400) rather than guessing which the caller meant; a body
// carrying neither is also a 400 ("nothing to update").
func (s *Server) handlePortalGroupSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req patchGroupRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		switch {
		case req.Name != nil && req.OwnerUserID != nil:
			writeJSON(w, http.StatusBadRequest, apierror.Response("group.patch_ambiguous", "specify either name or owner_user_id, not both", ""))
		case req.Name != nil:
			dto, err := s.Portal.RenameGroup(r.Context(), token, id, *req.Name)
			if err != nil {
				writeGroupError(w, err, "group.rename_failed")
				return
			}
			writeJSON(w, http.StatusOK, dto)
		case req.OwnerUserID != nil:
			if err := s.Portal.TransferOwnership(r.Context(), token, id, *req.OwnerUserID); err != nil {
				writeGroupError(w, err, "group.transfer_failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			writeJSON(w, http.StatusBadRequest, apierror.Response("group.patch_empty", "nothing to update", ""))
		}
	case http.MethodDelete:
		if err := s.Portal.DeleteGroup(r.Context(), token, id); err != nil {
			writeGroupError(w, err, "group.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalGroupCandidates backs GET /api/portal/groups/{id}/candidates
// (the member-picker's addable/invitable user list).
func (s *Server) handlePortalGroupCandidates(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	list, err := s.Portal.GroupMemberCandidates(r.Context(), token, id)
	if err != nil {
		writeGroupError(w, err, "group.candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// handlePortalGroupMembers backs GET (list the current roster) / POST
// ({user_ids}, direct-add for system/admin tier groups, invite for user-tier
// groups; portal.Service.AddGroupMembers branches on tier) on
// /api/portal/groups/{id}/members.
func (s *Server) handlePortalGroupMembers(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.Portal.GroupMembers(r.Context(), token, id)
		if err != nil {
			writeGroupError(w, err, "group.members_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req groupMembersRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		if err := s.Portal.AddGroupMembers(r.Context(), token, id, req.UserIDs); err != nil {
			writeGroupError(w, err, "group.add_members_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalGroupMemberSingle backs DELETE
// /api/portal/groups/{id}/members/{userId} -- removes userId from the group
// (with the full descendant cascade), OR lets userId leave on their own.
func (s *Server) handlePortalGroupMemberSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id, userID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.Portal.RemoveGroupMember(r.Context(), token, id, userID); err != nil {
		writeGroupError(w, err, "group.remove_member_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalGroupManagers backs POST /api/portal/groups/{id}/managers
// ({user_id}) -- promotes userID to co-manager (owner/system_admin only).
func (s *Server) handlePortalGroupManagers(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req groupManagerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.Portal.PromoteManager(r.Context(), token, id, req.UserID, req.CanManageUsers, req.CanManageGroup, req.CanManageServers, req.CanManageServices, req.CanManageResources); err != nil {
		writeGroupError(w, err, "group.promote_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalGroupManagerSingle backs DELETE (demotes userId; owner/
// system_admin only, idempotent on a non-manager) AND PATCH (narrows/widens
// an EXISTING co-manager's per-permission flags; owner/system_admin only,
// per-Admin-Group co-manager permissions spec 2026-08-10) on
// /api/portal/groups/{id}/managers/{userId}.
func (s *Server) handlePortalGroupManagerSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id, userID string) {
	switch r.Method {
	case http.MethodDelete:
		if err := s.Portal.DemoteManager(r.Context(), token, id, userID); err != nil {
			writeGroupError(w, err, "group.demote_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req groupManagerRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		if err := s.Portal.SetManagerPermissions(r.Context(), token, id, userID, req.CanManageUsers, req.CanManageGroup, req.CanManageServers, req.CanManageServices, req.CanManageResources); err != nil {
			writeGroupError(w, err, "group.set_manager_permissions_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodDelete+", "+http.MethodPatch)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalGroupRespond backs POST /api/portal/groups/{id}/accept and
// .../decline -- the invitee accepting/declining their OWN pending
// invitation; never an owner/manager action.
func (s *Server) handlePortalGroupRespond(w http.ResponseWriter, r *http.Request, token auth.Token, id string, accept bool) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.Portal.RespondInvitation(r.Context(), token, id, accept); err != nil {
		writeGroupError(w, err, "group.respond_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeGroupError maps portal.Service's user-groups sentinel errors onto HTTP
// status codes (spec §11): NotFound->404 (also the no-leak result for a group
// that exists but is invisible to the caller, per spec §6.3), NameConflict->409,
// NameInvalid/ParentInvalid/TierInvalid/MemberNotVisible/NotParentMember/
// CandidateInvalid/OwnerInvalid->400, Forbidden->403, else defaultCode->500.
// Every branch uses a STATIC message -- never err.Error() -- so no internal
// detail leaks.
// groupErrRows are writeGroupError's mapper-specific rows (checked before
// sharedErrorMap); portal.ErrGroupNameConflict/NameInvalid/ParentInvalid/
// TierInvalid/Forbidden map identically in writeProjectError and live in
// sharedErrorMap instead.
var groupErrRows = []errRow{
	{err: portal.ErrGroupNotFound, status: http.StatusNotFound, code: codeGroupNotFound, msg: msgGroupNotFound},
	{err: portal.ErrGroupOwnerInvalid, status: http.StatusBadRequest, code: "group.owner_invalid", msg: "the chosen owner is not an eligible admin"},
	{err: portal.ErrGroupMemberNotVisible, status: http.StatusBadRequest, code: "group.member_not_visible", msg: "member is not visible to you"},
	{err: portal.ErrGroupNotParentMember, status: http.StatusBadRequest, code: "group.not_parent_member", msg: "invitee is not a member of the parent group"},
	{err: portal.ErrGroupCandidateInvalid, status: http.StatusBadRequest, code: "group.candidate_invalid", msg: "candidate is not eligible"},
}

func writeGroupError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, groupErrRows, http.StatusInternalServerError, defaultCode, "group request failed")
}
