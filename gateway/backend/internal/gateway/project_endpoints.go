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

// createProjectRequest is the POST /api/portal/projects body. CoupledGroupID
// and CreateCoupledGroup are mutually exclusive (spec 2026-08-09): couple to
// an existing user-tier group the caller owns, or create one then couple.
// Both nil/absent -> a normal project (unchanged behavior).
type createProjectRequest struct {
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	CoupledGroupID     *string `json:"coupled_group_id"`
	CreateCoupledGroup *struct {
		Name          string `json:"name"`
		ParentGroupID string `json:"parent_group_id"`
	} `json:"create_coupled_group"`
}

// patchProjectRequest is the PATCH /api/portal/projects/{id} body. It
// carries EITHER a rename/description update (name and/or description) OR
// an ownership transfer (owner_user_id) -- pointers so "field present but
// empty" is distinguishable from "field absent", mirroring
// patchGroupRequest.
type patchProjectRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	OwnerUserID *string `json:"owner_user_id"`
}

// projectMembersRequest is the POST /api/portal/projects/{id}/members body.
type projectMembersRequest struct {
	UserIDs []string `json:"user_ids"`
}

// projectGroupsRequest is the POST /api/portal/projects/{id}/groups body.
type projectGroupsRequest struct {
	GroupIDs []string `json:"group_ids"`
}

// handlePortalProjects backs GET (the caller's project landscape --
// projects they own PLUS projects they are a member of) / POST (create,
// owner = caller) on the exact path /api/portal/projects. Internal
// authorization (owner-or-admin manage gate, per-owner name uniqueness)
// lives in portal.Service -- this handler only translates the wire shape.
func (s *Server) handlePortalProjects(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.Portal.ListProjects(r.Context(), token)
		if err != nil {
			writeProjectError(w, err, "project.list_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req createProjectRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		in := portal.CreateProjectInput{Name: req.Name, Description: req.Description}
		if req.CoupledGroupID != nil {
			in.CoupledGroupID = *req.CoupledGroupID
		}
		if req.CreateCoupledGroup != nil {
			in.CreateCoupledGroup = &portal.NewCoupledGroup{Name: req.CreateCoupledGroup.Name, ParentGroupID: req.CreateCoupledGroup.ParentGroupID}
		}
		dto, err := s.Portal.CreateProject(r.Context(), token, in)
		if err != nil {
			writeProjectError(w, err, "project.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalProjectsMine backs GET /api/portal/projects/mine -- the
// projects the caller is a MEMBER of (owner included), as the slim
// {id,name} list the token-assign picker uses. Registered as its OWN exact
// route in routes() -- Go's ServeMux prefers the longer, exact pattern over
// the "/api/portal/projects/" subtree, so this never falls into
// handlePortalProjectItem and gets misread as a project id of "mine".
func (s *Server) handlePortalProjectsMine(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	list, err := s.Portal.MyProjects(r.Context(), token)
	if err != nil {
		writeProjectError(w, err, "project.mine_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// handlePortalProjectItem is the subtree dispatcher for
// /api/portal/projects/{id} and its sub-resources (members/candidates/
// groups), mirroring handlePortalGroupItem's parts-based split.
func (s *Server) handlePortalProjectItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/projects/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("project.not_found", "project not found", ""))
		return
	}
	switch {
	case len(parts) == 1:
		s.handlePortalProjectSingle(w, r, token, id)
	case len(parts) == 2 && parts[1] == "members":
		s.handlePortalProjectMembers(w, r, token, id)
	case len(parts) == 3 && parts[1] == "members" && parts[2] != "":
		s.handlePortalProjectMemberSingle(w, r, token, id, parts[2])
	case len(parts) == 2 && parts[1] == "candidates":
		s.handlePortalProjectCandidates(w, r, token, id)
	case len(parts) == 2 && parts[1] == "groups":
		s.handlePortalProjectGroups(w, r, token, id)
	case len(parts) == 3 && parts[1] == "groups" && parts[2] != "":
		s.handlePortalProjectGroupSingle(w, r, token, id, parts[2])
	case len(parts) == 2 && parts[1] == "tokens":
		s.handlePortalProjectTokens(w, r, token, id)
	case len(parts) == 3 && parts[1] == "tokens" && parts[2] != "":
		s.handlePortalProjectTokenSingle(w, r, token, id, parts[2])
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response("project.not_found", "project not found", ""))
	}
}

// handlePortalProjectSingle backs PATCH (rename/description OR transfer
// ownership) / DELETE on /api/portal/projects/{id}. There is deliberately
// NO GET here (a single project's detail is read via the ListProjects
// landscape, like the user-groups surface).
//
// PATCH dispatch: a body carrying `name` and/or `description` renames/
// redescribes; a body carrying `owner_user_id` transfers ownership; a body
// carrying BOTH kinds is rejected as ambiguous (400) rather than guessing
// which the caller meant; a body carrying neither is also a 400 ("nothing
// to update").
func (s *Server) handlePortalProjectSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req patchProjectRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		renaming := req.Name != nil || req.Description != nil
		transferring := req.OwnerUserID != nil
		switch {
		case renaming && transferring:
			writeJSON(w, http.StatusBadRequest, apierror.Response("project.patch_ambiguous", "specify either name/description or owner_user_id, not both", ""))
		case transferring:
			if err := s.Portal.TransferProject(r.Context(), token, id, *req.OwnerUserID); err != nil {
				writeProjectError(w, err, "project.transfer_failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case renaming:
			var name, description string
			if req.Name != nil {
				name = *req.Name
			}
			if req.Description != nil {
				description = *req.Description
			}
			dto, err := s.Portal.UpdateProject(r.Context(), token, id, name, description)
			if err != nil {
				writeProjectError(w, err, "project.update_failed")
				return
			}
			writeJSON(w, http.StatusOK, dto)
		default:
			writeJSON(w, http.StatusBadRequest, apierror.Response("project.patch_empty", "nothing to update", ""))
		}
	case http.MethodDelete:
		if err := s.Portal.DeleteProject(r.Context(), token, id); err != nil {
			writeProjectError(w, err, "project.delete_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalProjectMembers backs GET (the resolved roster -- users AND
// groups) / POST ({user_ids}, batch-add) on
// /api/portal/projects/{id}/members. Owner/admin only (authorizeProjectManage,
// 404-no-leak).
func (s *Server) handlePortalProjectMembers(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		view, err := s.Portal.ProjectMembersView(r.Context(), token, id)
		if err != nil {
			writeProjectError(w, err, "project.members_failed")
			return
		}
		writeJSON(w, http.StatusOK, view)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req projectMembersRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		if err := s.Portal.AddProjectMembers(r.Context(), token, id, req.UserIDs); err != nil {
			writeProjectError(w, err, "project.add_members_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalProjectMemberSingle backs DELETE
// /api/portal/projects/{id}/members/{userId} -- removes userId from the
// project (owner/admin only; idempotent on a non-member, mirroring the
// underlying store's RemoveProjectMember).
func (s *Server) handlePortalProjectMemberSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id, userID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.Portal.RemoveProjectMember(r.Context(), token, id, userID); err != nil {
		writeProjectError(w, err, "project.remove_member_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalProjectCandidates backs GET
// /api/portal/projects/{id}/candidates -- the assignable/addable
// users+groups (owner/admin only, per the assigner's own visible set/group
// landscape, minus current members/groups).
func (s *Server) handlePortalProjectCandidates(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	users, groups, err := s.Portal.ProjectCandidates(r.Context(), token, id)
	if err != nil {
		writeProjectError(w, err, "project.candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "groups": groups})
}

// handlePortalProjectGroups backs POST ({group_ids}, batch-add) on
// /api/portal/projects/{id}/groups. Owner/admin only.
func (s *Server) handlePortalProjectGroups(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req projectGroupsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.Portal.AddProjectGroups(r.Context(), token, id, req.GroupIDs); err != nil {
		writeProjectError(w, err, "project.add_groups_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalProjectGroupSingle backs DELETE
// /api/portal/projects/{id}/groups/{groupId} -- removes groupId from the
// project (owner/admin only; idempotent).
func (s *Server) handlePortalProjectGroupSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id, groupID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.Portal.RemoveProjectGroup(r.Context(), token, id, groupID); err != nil {
		writeProjectError(w, err, "project.remove_group_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalProjectTokens backs GET /api/portal/projects/{id}/tokens --
// the API tokens currently attached to the project (each carrying its own
// project-attributed usage) plus the project's true total usage, as
// {tokens:[...], total:{...}} (owner/admin only, authorizeProjectManage's
// 404-no-leak gate; see writeProjectError).
func (s *Server) handlePortalProjectTokens(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	view, err := s.Portal.ProjectTokens(r.Context(), token, id)
	if err != nil {
		writeProjectError(w, err, "project.tokens_failed")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePortalProjectTokenSingle backs DELETE
// /api/portal/projects/{id}/tokens/{tokenId} -- detaches tokenId from the
// project (owner/admin only; a token attached to a DIFFERENT project, or no
// project, or an unknown id, is a no-leak 404 via writeProjectError's
// ErrTokenNotFound arm).
func (s *Server) handlePortalProjectTokenSingle(w http.ResponseWriter, r *http.Request, token auth.Token, id, tokenID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := s.Portal.DetachProjectToken(r.Context(), token, id, tokenID); err != nil {
		writeProjectError(w, err, "project.detach_token_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeProjectError maps portal.Service's projects sentinel errors onto
// HTTP status codes (spec §9): NotFound->404 (also the no-leak result for a
// project that exists but is invisible to the caller, via
// authorizeProjectManage), NameConflict->409,
// TransferTargetNotMember/MemberNotVisible/GroupNotVisible->400,
// Forbidden->403, else defaultCode->500. Every branch uses a STATIC
// message -- never err.Error() -- so no internal detail leaks.
//
// The coupled-create path (spec 2026-08-09) additionally surfaces the two new
// project.couple_* sentinels, plus the group sentinels a CreateCoupledGroup
// can return (ErrGroupNameConflict/ErrGroupNameInvalid/ErrGroupParentInvalid/
// ErrGroupTierInvalid/ErrGroupForbidden, via createUserGroup) so a
// create-group coupling failure isn't a bare 500 -- mapped with the SAME
// codes writeGroupError uses for consistency.
// projectErrRows are writeProjectError's mapper-specific rows (checked
// before sharedErrorMap); portal.ErrProjectNotFound and the 5 portal.ErrGroup*
// sentinels map identically elsewhere (writePortalTokenError /
// writeGroupError respectively) and live in sharedErrorMap instead.
// portal.ErrTokenNotFound maps to a different code in writePortalTokenError
// (portal.token_not_found) and writePortalServiceError
// (service.token_not_found), so it must stay here.
var projectErrRows = []errRow{
	{err: portal.ErrProjectNameConflict, status: http.StatusConflict, code: "project.name_conflict", msg: "a project with this name already exists"},
	{err: portal.ErrProjectTransferTargetNotMember, status: http.StatusBadRequest, code: "project.transfer_not_member", msg: "the new owner must be a current member of the project"},
	{err: portal.ErrProjectMemberNotVisible, status: http.StatusBadRequest, code: "project.member_not_visible", msg: "member is not visible to you"},
	{err: portal.ErrProjectGroupNotVisible, status: http.StatusBadRequest, code: "project.group_not_visible", msg: "group is not visible to you"},
	{err: portal.ErrProjectForbidden, status: http.StatusForbidden, code: "project.forbidden", msg: "not allowed"},
	{err: portal.ErrProjectCoupleGroupInvalid, status: http.StatusBadRequest, code: "project.couple_group_invalid", msg: "the group is not a user group you own"},
	{err: portal.ErrProjectCoupleAmbiguous, status: http.StatusBadRequest, code: "project.couple_ambiguous", msg: "choose EITHER an existing group OR create one, not both"},
	{err: portal.ErrProjectCoupled, status: http.StatusConflict, code: "project.coupled", msg: "this project is coupled to a group; manage membership via the group"},
	{err: portal.ErrTokenNotFound, status: http.StatusNotFound, code: "token.not_found", msg: "token not found"},
}

func writeProjectError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, projectErrRows, http.StatusInternalServerError, defaultCode, "project request failed")
}
