// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/totp"
	"strings"
	"time"
)

const (
	sessionCookieName = "op_ai_gateway_session"
	csrfHeaderName    = "X-OP-CSRF"
	runAsHeaderName   = "X-OP-Run-As-Token"

	internalAuthHeaderName = "X-OP-Internal-Auth"
	internalUserHeaderName = "X-OP-Internal-User"

	// serverOverrideHeaderName / serverOverrideForceHeaderName carry a per-request
	// server-override (Feature: server_override). They are set ONLY by the
	// gateway's own background chat-run executor calling itself over the internal
	// trusted-loopback path (see authenticateWeb) — an external client can never
	// inject them because nginx blanks both at the public edge (deploy/nginx/*.conf,
	// deploy/k8s/nginx-configmap.yaml), mirroring internalAuthHeaderName/
	// internalUserHeaderName. See applyServerOverride (server.go) for the actual
	// security boundary (AuthorizeServerManage re-authorization), which does not
	// rely on the header's provenance alone.
	serverOverrideHeaderName      = "X-OP-Server-Override"
	serverOverrideForceHeaderName = "X-OP-Server-Override-Force"
)

// Auth error codes/messages reused by more than one handler below.
const (
	codeAuthSessionInvalid = "auth.session_invalid"
	msgAuthSessionInvalid  = "session is invalid"

	codeAuthInvalidCredentials = "auth.invalid_credentials"

	codeAuthTOTPInvalid = "auth.totp_invalid"
	msgAuthTOTPInvalid  = "invalid authentication code"
)

// Scope strings checked by requireWebScope/requireScope/requireAnyScope/
// requireWebAnyScope across every Portal/Admin/System/inference handler in
// this package.
const (
	scopeGatewayUse = "gateway:use"
	scopeLLMInvoke  = "llm:invoke"
)

// Generic request-shape/method error code + message, shared by every handler
// below that decodes a JSON body or enforces its own method allowlist.
const (
	codeRequestInvalidJSON = "request.invalid_json"

	codeRequestMethodNotAllowed = "request.method_not_allowed"
	msgMethodNotAllowed         = "method not allowed"
)

// authenticateWeb resolves a principal. It first checks the internal trusted-
// loopback header (before cookie/bearer): only the gateway calling its own
// endpoints sets it, guarded by a per-process secret. Otherwise it resolves the
// principal from the session cookie when present, else from a bearer token.
// Cookie auth on state-changing methods requires the CSRF header.
func (s *Server) authenticateWeb(w http.ResponseWriter, r *http.Request) (auth.Token, bool) {
	// Internal trusted-loopback principal injection: only the gateway calling
	// its own endpoints sets these headers, guarded by a per-process secret.
	// Returns a bare session principal (ID==""), exactly as the browser cookie
	// path does; the handler applies any X-OP-Run-As-Token and fails closed on
	// error. Fail-closed: absent/incorrect secret, no lookup, or any lookup
	// error (including not-found) falls through to normal auth without writing
	// a response.
	if s.internalAuthSecret != "" && s.users != nil {
		if presented := r.Header.Get(internalAuthHeaderName); presented != "" &&
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.internalAuthSecret)) == 1 {
			if user, err := s.users.UserByID(r.Context(), r.Header.Get(internalUserHeaderName)); err == nil {
				// Background/internal runs are never elevated: they are not an
				// interactive session that went through the System-Admin
				// step-up, and the request never carried a session cookie to
				// resolve elevation from.
				return sessionPrincipal(user, false), true
			}
		}
	}
	if s.Account != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
			user, session, err := s.Account.ResolveSessionDetail(r.Context(), cookie.Value)
			if err != nil {
				s.clearSessionCookie(w)
				writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthSessionInvalid, msgAuthSessionInvalid, ""))
				return auth.Token{}, false
			}
			if !isSafeMethod(r.Method) && !csrfOK(r) {
				writeJSON(w, http.StatusForbidden, apierror.Response("auth.csrf_required", "csrf header required", ""))
				return auth.Token{}, false
			}
			return sessionPrincipal(user, sessionElevated(session)), true
		}
	}
	return s.authenticate(w, r)
}

func (s *Server) requireWebScope(w http.ResponseWriter, r *http.Request, scope string) (auth.Token, bool) {
	token, ok := s.authenticateWeb(w, r)
	if !ok {
		return auth.Token{}, false
	}
	// Defense-in-depth (service accounts, Phase 1 §5.3): a service token
	// (Kind=="service" — the gateway's "LLM-invoke only" principal, which never
	// carries gateway:use/admin/system) must never reach a Portal/Admin/System
	// route, regardless of scope, in case a future handler mistakenly grants one
	// of those scopes to it. This check is placed HERE — not inside
	// authenticateWeb — because authenticateWeb is also the auth path for
	// requireWebAnyScope, which /v1/chat/completions uses and which DELIBERATELY
	// accepts a service token's llm:invoke scope; rejecting Kind=="service"
	// inside authenticateWeb would 401 chat completions for every service token
	// too. requireWebScope, by contrast, is called ONLY by Portal/Admin/System
	// handlers (grep-verified: every /api/portal/*, /api/admin/*, /api/system/*
	// handler in this package calls requireWebScope, and it is never called by
	// any of the three inference handlers), so gating it here can never block
	// inference.
	if token.IsService() {
		writeJSON(w, http.StatusUnauthorized, apierror.Response("auth.invalid_token", "invalid bearer token", ""))
		return auth.Token{}, false
	}
	if !token.HasScope(scope) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient scope", ""))
		return auth.Token{}, false
	}
	return token, true
}

// requireWebAnyScope resolves a principal via authenticateWeb (internal
// trusted-loopback, session cookie, or bearer token) and requires it to carry AT
// LEAST ONE of the given scopes. Used by /v1/chat/completions, the one inference
// endpoint that is also reachable via the portal session/run-as path, which must
// accept EITHER a normal gateway:use principal OR a service token's sole
// llm:invoke scope (see requireAnyScope for the bearer-only counterpart used by
// /v1/responses and /v1/messages). Unlike requireWebScope, this does NOT reject
// Kind=="service" — that rejection is Portal-route-specific, see requireWebScope.
func (s *Server) requireWebAnyScope(w http.ResponseWriter, r *http.Request, scopes ...string) (auth.Token, bool) {
	token, ok := s.authenticateWeb(w, r)
	if !ok {
		return auth.Token{}, false
	}
	if !hasAnyScope(token, scopes) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient scope", ""))
		return auth.Token{}, false
	}
	return token, true
}

// hasAnyScope reports whether token carries at least one of the given scopes.
func hasAnyScope(token auth.Token, scopes []string) bool {
	for _, scope := range scopes {
		if token.HasScope(scope) {
			return true
		}
	}
	return false
}

// sessionPrincipal builds the auth.Token for a resolved session/loopback
// user. The `system` scope is granted ONLY to a system_admin whose session is
// currently elevated (System-Admin step-up mode) — see sessionElevated. A
// plain admin never carries `system`, elevated or not.
func sessionPrincipal(user store.User, elevated bool) auth.Token {
	scopes := []string{scopeGatewayUse}
	if user.Role == "admin" || user.Role == "system_admin" {
		scopes = append(scopes, "admin")
	}
	if user.Role == "system_admin" && elevated {
		scopes = append(scopes, "system")
	}
	// Token-less session chat (token.ID=="") inherits the two capture flags from
	// the user profile. A real run-as token keeps its own flags via
	// AuthorizeRunAsToken and never passes through here.
	return auth.Token{
		UserID:           user.ID,
		Name:             user.DisplayName,
		Active:           true,
		Scopes:           scopes,
		LogCommunication: user.ChatLogCommunication,
		Secret:           user.ChatSecret,
	}
}

// sessionElevated reports whether the session is currently in System-Admin
// mode (elevated_until strictly in the future).
func sessionElevated(session store.Session) bool {
	return !session.ElevatedUntil.IsZero() && session.ElevatedUntil.After(time.Now().UTC())
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func csrfOK(r *http.Request) bool {
	return r.Header.Get(csrfHeaderName) == "1"
}

func requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if !csrfOK(r) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.csrf_required", "csrf header required", ""))
		return false
	}
	return true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.SessionMaxAge / time.Second),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireCSRF(w, r) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req loginRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	user, err := s.Account.AuthenticatePassword(r.Context(), req.Email, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthInvalidCredentials, "invalid email or password", ""))
		return
	}
	mode := s.Portal.TOTPMode(r.Context())
	code := strings.TrimSpace(req.TOTPCode)
	switch {
	case user.TOTPEnabled && mode != "off":
		if code == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"totp_required": true})
			return
		}
		if !s.Account.VerifyTOTP(user, code) {
			writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthTOTPInvalid, msgAuthTOTPInvalid, ""))
			return
		}
		s.issueSessionAndRespond(w, r, user, mode)
	case mode == "required":
		if code == "" {
			secret, uri, err := s.Account.SetPendingTOTP(r.Context(), user.ID)
			if err != nil {
				writeTOTPError(w, err)
				return
			}
			writeTOTPEnrollment(w, "", secret, uri)
			return
		}
		updated, err := s.Account.ConfirmTOTP(r.Context(), user.ID, code)
		if err != nil {
			writeTOTPError(w, err)
			return
		}
		s.issueSessionAndRespond(w, r, updated, mode)
	default:
		s.issueSessionAndRespond(w, r, user, mode)
	}
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireCSRF(w, r) {
		return
	}
	if s.Account != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			_ = s.Account.Logout(r.Context(), cookie.Value)
		}
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type sessionResponse struct {
	Authenticated   bool                `json:"authenticated"`
	User            *portal.CurrentUser `json:"user,omitempty"`
	DefaultLanguage string              `json:"default_language"`
}

// handleAuthSession is a PUBLIC, always-200 session probe used by the SPA on
// mount so the logged-out path makes no 401-producing protected requests. It
// only reports a boolean (+ the caller's own basic user info) derived from the
// server-validated session cookie; it grants no access to protected data.
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.Account != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
			user, session, err := s.Account.ResolveSessionDetail(r.Context(), cookie.Value)
			if err != nil {
				s.clearSessionCookie(w)
				writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false, DefaultLanguage: s.Portal.PublicLanguage(r.Context())})
				return
			}
			dto := currentUserDTO(user, s.Portal.TOTPMode(r.Context()), sessionElevated(session), session.ElevatedUntil, s.Portal.SystemAdminModeRequirePassword(r.Context()))
			writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, User: &dto, DefaultLanguage: s.Portal.PublicLanguage(r.Context())})
			return
		}
	}
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false, DefaultLanguage: s.Portal.PublicLanguage(r.Context())})
}

// currentUserDTO assembles the /me DTO. elevated/expiresAt reflect the
// session's REAL System-Admin-mode state (only handleAuthSession and
// handleSystemAdminMode have a session to read this from); every other call
// site issues or refreshes a session that is never pre-elevated, so it passes
// elevated=false, expiresAt=zero. requirePassword is the current
// system_admin_mode_require_password setting (a UI hint, not an authority).
func currentUserDTO(user store.User, mode string, elevated bool, expiresAt time.Time, requirePassword bool) portal.CurrentUser {
	return portal.NewCurrentUser(user, mode, elevated, expiresAt, requirePassword)
}

func (s *Server) issueSessionAndRespond(w http.ResponseWriter, r *http.Request, u store.User, mode string) {
	secret, err := s.Account.IssueSession(r.Context(), u)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("auth.session_failed", "could not create session", ""))
		return
	}
	s.setSessionCookie(w, secret)
	writeJSON(w, http.StatusOK, currentUserDTO(u, mode, false, time.Time{}, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}

func writeTOTPEnrollment(w http.ResponseWriter, email, secretBase32, otpauthURI string) {
	payload := map[string]any{
		"totp_enrollment_required": true,
		"secret_base32":            secretBase32,
		"otpauth_uri":              otpauthURI,
	}
	if png, err := totp.QRCodePNG(otpauthURI); err == nil {
		payload["qr_png_data_uri"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}
	if email != "" {
		payload["email"] = email
	}
	writeJSON(w, http.StatusOK, payload)
}

// totpErrRows is writeTOTPError's mapper-specific rows; account.ErrUserNotFound
// maps identically in writeAdminUserError and lives in sharedErrorMap instead.
var totpErrRows = []errRow{
	{err: account.ErrTOTPInvalid, status: http.StatusUnauthorized, code: codeAuthTOTPInvalid, msg: msgAuthTOTPInvalid},
	{err: account.ErrTOTPNotEnrolled, status: http.StatusConflict, code: "auth.totp_not_enrolled", msg: "totp is not enrolled"},
}

func writeTOTPError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, totpErrRows, http.StatusInternalServerError, "auth.totp_failed", "totp operation failed")
}

type setPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

func (s *Server) handleAuthSetPassword(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireCSRF(w, r) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req setPasswordRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	user, secret, err := s.Account.SetPassword(r.Context(), req.Token, req.Password)
	if err != nil {
		writeSetPasswordError(w, err)
		return
	}
	mode := s.Portal.TOTPMode(r.Context())
	if mode == "required" {
		secretB32, uri, err := s.Account.SetPendingTOTP(r.Context(), user.ID)
		if err != nil {
			writeTOTPError(w, err)
			return
		}
		writeTOTPEnrollment(w, user.Email, secretB32, uri) // includes email; no session cookie
		return
	}
	s.setSessionCookie(w, secret)
	writeJSON(w, http.StatusOK, currentUserDTO(user, mode, false, time.Time{}, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handlePortalPassword(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req changePasswordRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.Account.ChangePassword(r.Context(), token.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrPasswordTooWeak) {
			writeJSON(w, http.StatusBadRequest, apierror.Response("auth.password_too_weak", "password is too weak", ""))
			return
		}
		if errors.Is(err, account.ErrInvalidCredentials) {
			writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthInvalidCredentials, "current password is incorrect", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("auth.password_change_failed", "password change failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePortalLanguage(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if !portal.IsKnownLanguage(req.Language) {
		writeJSON(w, http.StatusBadRequest, apierror.Response("system.language_invalid", "unknown language", ""))
		return
	}
	// Self-service edit of the caller's OWN preferred language: UpdateOwnProfile
	// (not UpdateUser) so a non-elevated system_admin editing their own profile
	// never trips UpdateUser's system-admin-account protection guard (that guard
	// exists to stop OTHER actors from touching a system_admin's role/status, not
	// to block a system_admin's own non-role self-service edits).
	user, err := s.Account.UpdateOwnProfile(r.Context(), token.UserID, account.UserUpdate{PreferredLanguage: &req.Language})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.language_update_failed", "language update failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, currentUserDTO(user, s.Portal.TOTPMode(r.Context()), false, time.Time{}, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}

// handlePortalChatSettings is the self-service endpoint for the two token-less
// session-chat capture flags stored on the user profile (Feature 5). It mirrors
// handlePortalLanguage: session-or-bearer auth, PUT only, and a persistent write
// via account.UpdateUser. The current values are read back through the synthetic
// ChatSession row in GET /api/portal/tokens, so there is no separate GET here.
func (s *Server) handlePortalChatSettings(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		LogCommunication bool `json:"log_communication"`
		Secret           bool `json:"secret"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	// Self-service edit of the caller's OWN chat capture flags: UpdateOwnProfile
	// (see handlePortalLanguage's comment on why this is not UpdateUser).
	user, err := s.Account.UpdateOwnProfile(r.Context(), token.UserID, account.UserUpdate{ChatLogCommunication: &req.LogCommunication, ChatSecret: &req.Secret})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.chat_settings_update_failed", "chat settings update failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, currentUserDTO(user, s.Portal.TOTPMode(r.Context()), false, time.Time{}, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}

// handlePortalTOTP is the self-service TOTP disable endpoint (DELETE only).
// It is permitted when totp_mode is optional or off (so a user can remove a
// stale enrollment once the org turns TOTP off entirely); only totp_mode
// required blocks it (409 auth.totp_disable_forbidden).
func (s *Server) handlePortalTOTP(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if mode := s.Portal.TOTPMode(r.Context()); mode != "optional" && mode != "off" {
		writeJSON(w, http.StatusConflict, apierror.Response("auth.totp_disable_forbidden", "totp cannot be disabled in this mode", ""))
		return
	}
	if err := s.Account.DisableTOTP(r.Context(), token.UserID, strings.TrimSpace(req.Code)); err != nil {
		writeTOTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePortalTOTPItem dispatches everything under /api/portal/totp/: enroll
// and confirm.
func (s *Server) handlePortalTOTPItem(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/api/portal/totp/") {
	case "enroll":
		s.handleTOTPEnroll(w, r)
	case "confirm":
		s.handleTOTPConfirm(w, r)
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response("request.not_found", "not found", ""))
	}
}

// handleTOTPEnroll generates a new pending TOTP secret for the caller, to be
// confirmed via handleTOTPConfirm. Rejected when totp_mode=off. If the caller
// is already enrolled (TOTPEnabled), re-enrollment requires a valid CURRENT
// code as a step-up proof of possession: without it, a hijacked session
// cookie (which carries no proof of holding the physical authenticator)
// could otherwise rebind 2FA to an attacker's device, or a benign abandoned
// re-enroll would silently downgrade the account. A not-yet-enrolled user
// (the login/set-password forced-enrollment paths, which call
// SetPendingTOTP directly rather than through this handler) needs no code.
func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if s.Portal.TOTPMode(r.Context()) == "off" {
		writeJSON(w, http.StatusConflict, apierror.Response("auth.totp_disabled", "totp is disabled", ""))
		return
	}
	user, err := s.Account.UserByID(r.Context(), token.UserID)
	if err != nil {
		writeTOTPError(w, err)
		return
	}
	if user.TOTPEnabled {
		var req struct {
			Code string `json:"code"`
		}
		if r.Body != nil {
			limited := http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
			defer limited.Close()
			// Best-effort decode: a missing/empty/invalid body just leaves
			// Code == "", which VerifyTOTP will correctly reject below.
			_ = json.NewDecoder(limited).Decode(&req)
		}
		if !s.Account.VerifyTOTP(user, strings.TrimSpace(req.Code)) {
			writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthTOTPInvalid, msgAuthTOTPInvalid, ""))
			return
		}
	}
	secret, uri, err := s.Account.SetPendingTOTP(r.Context(), token.UserID)
	if err != nil {
		writeTOTPError(w, err)
		return
	}
	writeTOTPEnrollment(w, "", secret, uri)
}

// handleTOTPConfirm verifies a code against the pending secret set by
// handleTOTPEnroll and, on success, enables TOTP for the caller.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	u, err := s.Account.ConfirmTOTP(r.Context(), token.UserID, strings.TrimSpace(req.Code))
	if err != nil {
		writeTOTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, currentUserDTO(u, s.Portal.TOTPMode(r.Context()), false, time.Time{}, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}

var setPasswordErrRows = []errRow{
	{err: auth.ErrPasswordTooWeak, status: http.StatusBadRequest, code: "auth.password_too_weak", msg: "password is too weak"},
	{err: account.ErrSetPasswordTokenInvalid, status: http.StatusBadRequest, code: "auth.set_password_token_invalid", msg: "invite token is invalid or expired"},
}

func writeSetPasswordError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, setPasswordErrRows, http.StatusInternalServerError, "auth.set_password_failed", "set password failed")
}

// handleSystemAdminMode is the System-Admin step-up mode enter/leave endpoint:
// POST /api/portal/system-admin-mode elevates the caller's session (checked
// against the system_admin_mode_require_password setting), and DELETE drops
// the elevation again. Both respond with the fresh /me DTO so the frontend
// can update immediately without a second round-trip.
func (s *Server) handleSystemAdminMode(w http.ResponseWriter, r *http.Request) {
	// Session principal (cookie), CSRF-enforced by authenticateWeb for the
	// non-safe methods below; role gate lives in the account service.
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthSessionInvalid, msgAuthSessionInvalid, ""))
		return
	}
	switch r.Method {
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		_ = json.Unmarshal(raw, &body) // password optional (ignored when the setting is off)
		requirePw := s.Portal.SystemAdminModeRequirePassword(r.Context())
		err := s.Account.EnterSystemAdminMode(r.Context(), cookie.Value, body.Password, requirePw)
		switch {
		case errors.Is(err, account.ErrNotSystemAdmin):
			writeJSON(w, http.StatusForbidden, apierror.Response("auth.not_system_admin", "only a system admin can enter system-admin mode", ""))
			return
		case errors.Is(err, account.ErrInvalidCredentials):
			writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthInvalidCredentials, "invalid password", ""))
			return
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, apierror.Response("auth.system_admin_mode_failed", "could not enter system-admin mode", ""))
			return
		}
		slog.Info("system-admin mode entered", "user_id", token.UserID)
	case http.MethodDelete:
		if err := s.Account.ExitSystemAdminMode(r.Context(), cookie.Value); err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("auth.system_admin_mode_failed", "could not leave system-admin mode", ""))
			return
		}
		slog.Info("system-admin mode left", "user_id", token.UserID)
	default:
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		return
	}
	// Respond with the fresh /me DTO reflecting the new elevation state.
	user, session, err := s.Account.ResolveSessionDetail(r.Context(), cookie.Value)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, apierror.Response(codeAuthSessionInvalid, msgAuthSessionInvalid, ""))
		return
	}
	writeJSON(w, http.StatusOK, currentUserDTO(user, s.Portal.TOTPMode(r.Context()), sessionElevated(session), session.ElevatedUntil, s.Portal.SystemAdminModeRequirePassword(r.Context())))
}
