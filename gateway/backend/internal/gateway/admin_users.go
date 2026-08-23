// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
	"time"
)

const (
	codeAdminUserListFailed = "admin.user_list_failed"
	msgAdminUserListFailed  = "user list failed"
)

type adminUserDTO struct {
	ID                string    `json:"id"`
	Email             string    `json:"email"`
	DisplayName       string    `json:"display_name"`
	Role              string    `json:"role"`
	Status            string    `json:"status"`
	PreferredLanguage string    `json:"preferred_language"`
	CreatedAt         time.Time `json:"created_at"`
	TOTPEnabled       bool      `json:"totp_enabled"`
}

func adminUserFromStore(user store.User) adminUserDTO {
	return adminUserDTO{
		ID:                user.ID,
		Email:             user.Email,
		DisplayName:       user.DisplayName,
		Role:              user.Role,
		Status:            user.Status,
		PreferredLanguage: user.PreferredLanguage,
		CreatedAt:         user.CreatedAt,
		TOTPEnabled:       user.TOTPEnabled,
	}
}

type createUserRequest struct {
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	Role              string `json:"role"`
	PreferredLanguage string `json:"preferred_language"`
	AdminGroupID      string `json:"admin_group_id"`
}

type updateUserRequest struct {
	DisplayName       *string `json:"display_name"`
	Role              *string `json:"role"`
	Status            *string `json:"status"`
	PreferredLanguage *string `json:"preferred_language"`
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := s.Account.ListUsers(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response(codeAdminUserListFailed, msgAdminUserListFailed, ""))
			return
		}
		if !token.HasScope("system") {
			manageable, err := s.Portal.ManageableUserIDs(r.Context(), token)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, apierror.Response(codeAdminUserListFailed, msgAdminUserListFailed, ""))
				return
			}
			filtered := users[:0]
			for _, u := range users {
				if manageable[u.ID] {
					filtered = append(filtered, u)
				}
			}
			users = filtered
		}
		out := make([]adminUserDTO, 0, len(users))
		for _, user := range users {
			out = append(out, adminUserFromStore(user))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req createUserRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		adminGroupID, parentSystemGroupID, err := s.Portal.ResolveInviteAdminGroup(r.Context(), token, req.AdminGroupID)
		if err != nil {
			switch {
			case errors.Is(err, portal.ErrInviteAdminGroupRequired):
				writeJSON(w, http.StatusBadRequest, apierror.Response("user.admin_group_required", "you must assign the new user to an admin group", ""))
			case errors.Is(err, portal.ErrInviteAdminGroupInvalid):
				writeJSON(w, http.StatusBadRequest, apierror.Response("user.admin_group_invalid", "the requested admin group does not exist or is not yours to assign", ""))
			default:
				writeJSON(w, http.StatusInternalServerError, apierror.Response("admin.invite_failed", "invite failed", ""))
			}
			return
		}
		user, secret, err := s.Account.InviteUser(r.Context(), account.InviteInput{
			Email:             req.Email,
			DisplayName:       req.DisplayName,
			Role:              req.Role,
			PreferredLanguage: req.PreferredLanguage,
		}, token.HasScope("system"))
		if err != nil {
			writeAdminUserError(w, err)
			return
		}
		if err := s.Portal.AddUserToAdminGroup(r.Context(), token, user.ID, adminGroupID, parentSystemGroupID); err != nil {
			slog.Error("admin invite: failed to add new user to admin group", "user_id", user.ID, "admin_group", adminGroupID, "err", err)
		}
		inviteURL := s.inviteURL(secret)
		emailSent, emailErr := s.sendInviteEmail(r.Context(), user, inviteURL)
		writeJSON(w, http.StatusCreated, map[string]any{
			"user":        adminUserFromStore(user),
			"invite_url":  inviteURL,
			"email_sent":  emailSent,
			"email_error": emailErr,
		})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

func (s *Server) handleAdminUserItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/users/"), "/")
	parts := strings.Split(rest, "/")

	// Non-system callers are scoped to ManageableUserIDs -- narrower than the
	// list endpoint's VisibleUserIDs-turned-ManageableUserIDs filter, per the
	// per-Admin-Group co-manager permissions model (spec 2026-08-10). Checked
	// ONCE here, before dispatching to any sub-route, so update/invite-reissue
	// (password-reset)/TOTP-reset/token-list all uniformly 404-no-leak on a
	// target outside the caller's manageable set -- identical to a
	// nonexistent user, never a distinguishable forbidden. A system caller
	// (who manages every user) skips this entirely.
	if parts[0] != "" && !token.HasScope("system") {
		manageable, err := s.Portal.ManageableUserIDs(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response(codeAdminUserListFailed, msgAdminUserListFailed, ""))
			return
		}
		if !manageable[parts[0]] {
			writeJSON(w, http.StatusNotFound, apierror.Response("admin.user_not_found", "user not found", ""))
			return
		}
	}

	if len(parts) == 2 && parts[1] == "invite" && parts[0] != "" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		user, secret, err := s.Account.ReissueInvite(r.Context(), parts[0])
		if err != nil {
			writeAdminUserError(w, err)
			return
		}
		inviteURL := s.inviteURL(secret)
		emailSent, emailErr := s.sendInviteEmail(r.Context(), user, inviteURL)
		writeJSON(w, http.StatusOK, map[string]any{
			"user":        adminUserFromStore(user),
			"invite_url":  inviteURL,
			"email_sent":  emailSent,
			"email_error": emailErr,
		})
		return
	}

	if len(parts) == 2 && parts[1] == "tokens" && parts[0] != "" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		resp, err := s.Portal.UserTokens(r.Context(), token, parts[0])
		if err != nil {
			writeMappedError(w, err, nil, http.StatusInternalServerError, "admin.user_tokens_failed", "user tokens failed")
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if len(parts) == 3 && parts[1] == "totp" && parts[2] == "reset" && parts[0] != "" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if err := s.Account.ResetTOTP(r.Context(), parts[0]); err != nil {
			writeTOTPError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if len(parts) != 1 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("admin.user_not_found", "user not found", ""))
		return
	}
	if !requireMethod(w, r, http.MethodPatch) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	user, err := s.Account.UpdateUser(r.Context(), parts[0], account.UserUpdate{
		DisplayName:       req.DisplayName,
		Role:              req.Role,
		Status:            req.Status,
		PreferredLanguage: req.PreferredLanguage,
	}, token.HasScope("system"))
	if err != nil {
		writeAdminUserError(w, err)
		return
	}
	if req.Status != nil && *req.Status == store.UserStatusDisabled {
		// Best-effort: the user is already disabled at this point, so a
		// succession failure must not fail the PATCH -- it would just leave
		// their groups with a stale (now-inactive) owner until the next
		// disable/retry, which is recoverable and not worth 500ing on.
		if err := s.Portal.ReassignGroupsOwnedBy(r.Context(), token, user.ID); err != nil {
			slog.Error("admin disable user: owner succession failed", "user_id", user.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, adminUserFromStore(user))
}

func (s *Server) inviteURL(secret string) string {
	base := strings.TrimRight(s.PublicURL, "/")
	return base + "/set-password?token=" + secret
}

// adminUserErrRows are writeAdminUserError's mapper-specific rows;
// account.ErrUserNotFound maps identically in writeTOTPError and lives in
// sharedErrorMap instead.
var adminUserErrRows = []errRow{
	{err: account.ErrEmailRequired, status: http.StatusBadRequest, code: "admin.email_required", msg: "email is required"},
	{err: account.ErrInvalidRole, status: http.StatusBadRequest, code: "admin.invalid_role", msg: "role is invalid"},
	{err: account.ErrInvalidStatus, status: http.StatusBadRequest, code: "admin.invalid_status", msg: "status is invalid"},
	{err: account.ErrUserConflict, status: http.StatusConflict, code: "admin.user_conflict", msg: "user already exists"},
	{err: account.ErrLastAdmin, status: http.StatusConflict, code: "admin.cannot_disable_last_admin", msg: "cannot remove the last active admin"},
	{err: account.ErrForbiddenRole, status: http.StatusForbidden, code: "admin.system_admin_forbidden", msg: "only a system admin can manage system admins"},
	{err: account.ErrLastSystemAdmin, status: http.StatusConflict, code: "admin.cannot_disable_last_system_admin", msg: "cannot remove the last active system admin"},
}

func writeAdminUserError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, adminUserErrRows, http.StatusInternalServerError, "admin.user_update_failed", "user update failed")
}
