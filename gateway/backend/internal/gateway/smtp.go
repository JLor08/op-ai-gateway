// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/mail"
	"op-ai-gateway/internal/store"
	"strings"
)

// Mailer sends one message. Satisfied by *mail.Mailer; a DI seam so tests
// record calls or point a real mailer at a loopback SMTP catcher.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// sendInviteEmail reads the saved SMTP settings and, when SMTP is enabled,
// emails the localized activation link. It NEVER blocks user management: a
// disabled config returns (false, ""); any failure returns (false, err.Error())
// and is logged; success returns (true, ""). The user and invite URL are always
// valid regardless of the outcome.
func (s *Server) sendInviteEmail(ctx context.Context, user store.User, inviteURL string) (sent bool, errMsg string) {
	if s.Portal == nil {
		return false, ""
	}
	cfg, err := s.Portal.SMTPRuntimeConfig(ctx)
	if err != nil {
		log.Printf("smtp: read settings failed: %v", err)
		return false, "smtp settings unavailable"
	}
	if !cfg.Enabled {
		return false, ""
	}
	msg := mail.Invite(user.PreferredLanguage, user.DisplayName, inviteURL)
	if err := s.newMailer(mail.Config{
		Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, Password: cfg.Password,
		From: cfg.From, FromName: cfg.FromName, TLSMode: cfg.TLSMode,
	}).Send(ctx, user.Email, msg.Subject, msg.Body); err != nil {
		log.Printf("smtp: invite email to %s failed: %v", user.Email, err)
		return false, err.Error()
	}
	return true, ""
}

type smtpTestRequest struct {
	To string `json:"to"`
}

// handleSystemSMTPTest sends a fixed test email using the SAVED SMTP settings
// (save first). Recipient defaults to the calling admin's own email. The
// response is {ok, error?} only — it never echoes the SMTP password. Send is
// attempted regardless of the enabled toggle so an admin can verify before
// switching it on.
func (s *Server) handleSystemSMTPTest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req smtpTestRequest
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
	}
	me, err := s.Portal.CurrentUser(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.user_lookup_failed", "user lookup failed", ""))
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		to = me.Email
	}
	cfg, err := s.Portal.SMTPRuntimeConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	msg := mail.Test(me.PreferredLanguage)
	if err := s.newMailer(mail.Config{
		Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, Password: cfg.Password,
		From: cfg.From, FromName: cfg.FromName, TLSMode: cfg.TLSMode,
	}).Send(r.Context(), to, msg.Subject, msg.Body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
