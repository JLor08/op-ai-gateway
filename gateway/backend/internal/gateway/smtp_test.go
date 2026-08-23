// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/mail"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

type recordingMailer struct {
	cfg         mail.Config
	to, subject string
	body        string
	sends       int
	err         error
}

func (m *recordingMailer) Send(ctx context.Context, to, subject, body string) error {
	m.sends++
	m.to, m.subject, m.body = to, subject, body
	return m.err
}

// enableSMTP writes a minimal enabled SMTP config into the server's settings
// store (no password => no cipher/volatile dependency).
func enableSMTP(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		SMTPEnabled: boolPtr(true),
		SMTPHost:    strPtr("smtp.example.test"),
		SMTPPort:    intPtr(2525),
		SMTPFrom:    strPtr("noreply@example.test"),
		SMTPTLSMode: strPtr("none"),
	}); err != nil {
		t.Fatalf("enable smtp: %v", err)
	}
}

func TestSendInviteEmailDisabledNoAttempt(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	rec := &recordingMailer{}
	srv.newMailer = func(mail.Config) Mailer { return rec }
	sent, errMsg := srv.sendInviteEmail(context.Background(),
		store.User{Email: "u@example.test", DisplayName: "U", PreferredLanguage: "de"}, "http://host/set-password?token=x")
	if sent || errMsg != "" || rec.sends != 0 {
		t.Fatalf("disabled: got (sent=%v, err=%q, sends=%d), want (false, \"\", 0)", sent, errMsg, rec.sends)
	}
}

func TestSendInviteEmailEnabledSends(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	enableSMTP(t, srv)
	rec := &recordingMailer{}
	srv.newMailer = func(cfg mail.Config) Mailer { rec.cfg = cfg; return rec }
	sent, errMsg := srv.sendInviteEmail(context.Background(),
		store.User{Email: "u@example.test", DisplayName: "U", PreferredLanguage: "de"}, "http://host/set-password?token=abc")
	if !sent || errMsg != "" || rec.sends != 1 {
		t.Fatalf("enabled: got (sent=%v, err=%q, sends=%d), want (true, \"\", 1)", sent, errMsg, rec.sends)
	}
	if rec.to != "u@example.test" || rec.cfg.Host != "smtp.example.test" {
		t.Fatalf("recorded to=%q host=%q", rec.to, rec.cfg.Host)
	}
	if !strings.Contains(rec.body, "http://host/set-password?token=abc") {
		t.Fatalf("invite body must contain the activation link, got %q", rec.body)
	}
}

func TestSendInviteEmailSendFailureNonBlocking(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	enableSMTP(t, srv)
	srv.newMailer = func(mail.Config) Mailer { return &recordingMailer{err: errors.New("dial tcp: refused")} }
	sent, errMsg := srv.sendInviteEmail(context.Background(),
		store.User{Email: "u@example.test", PreferredLanguage: "de"}, "http://host/set-password?token=x")
	if sent || errMsg == "" {
		t.Fatalf("send failure: got (sent=%v, err=%q), want (false, non-empty)", sent, errMsg)
	}
}

func TestSMTPTestEndpointDefaultsToCaller(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	enableSMTP(t, srv)
	rec := &recordingMailer{}
	srv.newMailer = func(mail.Config) Mailer { return rec }
	cookie := loginAs(t, srv, "sys@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	req := httptest.NewRequest(http.MethodPost, "/api/system/smtp/test", strings.NewReader(`{}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("smtp test = %d body=%s", rr.Code, rr.Body.String())
	}
	if rec.to != "sys@example.test" || rec.sends != 1 {
		t.Fatalf("default recipient=%q sends=%d", rec.to, rec.sends)
	}
}

func TestSMTPTestEndpointExplicitToAndFailure(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	enableSMTP(t, srv)
	srv.newMailer = func(mail.Config) Mailer { return &recordingMailer{err: errors.New("dial tcp: refused")} }
	cookie := loginAs(t, srv, "sys@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	req := httptest.NewRequest(http.MethodPost, "/api/system/smtp/test", strings.NewReader(`{"to":"dest@example.test"}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok":false`) || !strings.Contains(rr.Body.String(), "refused") {
		t.Fatalf("failure test = %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "password") {
		t.Fatalf("test endpoint must never echo the password: %s", rr.Body.String())
	}
}

func TestSMTPTestRequiresSystemScope(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin") // admin, not system_admin
	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	req := httptest.NewRequest(http.MethodPost, "/api/system/smtp/test", strings.NewReader(`{}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-system admin should be 403, got %d", rr.Code)
	}
}
