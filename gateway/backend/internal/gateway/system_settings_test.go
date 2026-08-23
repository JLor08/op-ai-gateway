// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("Unmarshal error body: %v (body=%s)", err, body)
	}
	return parsed.Error.Code
}

// TestSystemThemePublic confirms the public theme endpoint needs no auth and
// returns the active theme -- a built-in active theme reports source
// "builtin" with a null data payload (the frontend uses its own compiled
// copy for built-ins).
func TestSystemThemePublic(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/theme", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/system/theme = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body portal.ThemePublicView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Theme != "default" {
		t.Fatalf("theme = %q, want %q", body.Theme, "default")
	}
	if body.Source != "builtin" {
		t.Fatalf("source = %q, want %q", body.Source, "builtin")
	}
	if body.Data != nil {
		t.Fatalf("data = %+v, want nil for a builtin active theme", body.Data)
	}
	// The raw body must carry an explicit "data": null, not an omitted key --
	// the frontend contract distinguishes "present and null" from "absent".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("Unmarshal raw returned %v", err)
	}
	if dataRaw, ok := raw["data"]; !ok || string(dataRaw) != "null" {
		t.Fatalf(`raw "data" = %v (present=%v), want explicit null`, string(dataRaw), ok)
	}
}

// TestSystemSettingsRequiresSystemScope confirms an admin session (no system
// scope) is forbidden while a system_admin session succeeds.
func TestSystemSettingsRequiresSystemScope(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")

	adminCookie := loginAs(t, srv, "admin@example.test", "password-1")
	adminReq := httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
	adminReq.AddCookie(adminCookie)
	adminRec := httptest.NewRecorder()
	srv.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusForbidden {
		t.Fatalf("admin GET /api/system/settings = %d, want 403 (body=%s)", adminRec.Code, adminRec.Body.String())
	}

	sysCookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")
	sysReq := httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
	sysReq.AddCookie(sysCookie)
	sysRec := httptest.NewRecorder()
	srv.ServeHTTP(sysRec, sysReq)
	if sysRec.Code != http.StatusOK {
		t.Fatalf("system_admin GET /api/system/settings = %d, want 200 (body=%s)", sysRec.Code, sysRec.Body.String())
	}
	var view struct {
		Theme           string               `json:"theme"`
		AvailableThemes []portal.ThemeOption `json:"available_themes"`
	}
	if err := json.Unmarshal(sysRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.Theme != "default" {
		t.Fatalf("theme = %q, want %q", view.Theme, "default")
	}
	want := []portal.ThemeOption{{ID: "default", Name: "Default"}, {ID: "matrix", Name: "Matrix"}, {ID: "skynet", Name: "Skynet"}}
	if len(view.AvailableThemes) != len(want) {
		t.Fatalf("available_themes = %v, want %v", view.AvailableThemes, want)
	}
	for i, opt := range want {
		if view.AvailableThemes[i] != opt {
			t.Fatalf("available_themes = %v, want %v", view.AvailableThemes, want)
		}
	}
}

// TestSystemSettingsUpdate confirms PUT validates the theme and persists a
// known one for a system_admin session.
func TestSystemSettingsUpdate(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	// Unknown theme → 400 system.theme_invalid.
	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"theme":"nope"}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT bad theme = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.theme_invalid" {
		t.Fatalf("error code = %q, want system.theme_invalid", code)
	}

	// Known theme → 200 with the persisted view.
	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"theme":"default"}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT good theme = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.Theme != "default" {
		t.Fatalf("theme = %q, want %q", view.Theme, "default")
	}
}

// TestSystemSettingsUpdateRejectsRetention confirms an out-of-range
// capture_retention_days is a 400 system.retention_invalid (not a 500), and a
// valid value persists and round-trips in the view.
func TestSystemSettingsUpdateRejectsRetention(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"capture_retention_days":0}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT retention=0 = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.retention_invalid" {
		t.Fatalf("error code = %q, want system.retention_invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"capture_retention_days":14}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT retention=14 = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		CaptureRetentionDays int `json:"capture_retention_days"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.CaptureRetentionDays != 14 {
		t.Fatalf("capture_retention_days = %d, want 14", view.CaptureRetentionDays)
	}
}

// TestSystemSettingsUpdateRejectsHealthCheckInterval confirms an out-of-range
// health_check_interval_seconds is a 400 system.health_check_interval_invalid,
// and a valid value persists and round-trips in the view.
func TestSystemSettingsUpdateRejectsHealthCheckInterval(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"health_check_interval_seconds":4}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT interval=4 = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.health_check_interval_invalid" {
		t.Fatalf("error code = %q, want system.health_check_interval_invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"health_check_interval_seconds":90}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT interval=90 = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.HealthCheckIntervalSeconds != 90 {
		t.Fatalf("health_check_interval_seconds = %d, want 90", view.HealthCheckIntervalSeconds)
	}
}

// TestSystemSettingsUpdateRejectsAgentPresenceTimeout confirms an out-of-range
// agent_presence_timeout_seconds is a 400 system.agent_presence_timeout_invalid,
// and a valid value persists and round-trips in the view.
func TestSystemSettingsUpdateRejectsAgentPresenceTimeout(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"agent_presence_timeout_seconds":2}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT agent_presence_timeout_seconds=2 = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.agent_presence_timeout_invalid" {
		t.Fatalf("error code = %q, want system.agent_presence_timeout_invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"agent_presence_timeout_seconds":20}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT agent_presence_timeout_seconds=20 = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		AgentPresenceTimeoutSeconds int `json:"agent_presence_timeout_seconds"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.AgentPresenceTimeoutSeconds != 20 {
		t.Fatalf("agent_presence_timeout_seconds = %d, want 20", view.AgentPresenceTimeoutSeconds)
	}
}

// TestSystemSettingsUpdateRejectsTOTPMode confirms an unknown totp_mode is a
// 400 system.totp_mode_invalid, and a valid value persists and round-trips
// in the view.
func TestSystemSettingsUpdateRejectsTOTPMode(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"nope"}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT totp_mode=nope = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.totp_mode_invalid" {
		t.Fatalf("error code = %q, want system.totp_mode_invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"required"}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT totp_mode=required = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		TOTPMode string `json:"totp_mode"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.TOTPMode != "required" {
		t.Fatalf("totp_mode = %q, want %q", view.TOTPMode, "required")
	}
}

// TestSystemSettingsUpdateRejectsVisionProbeMode confirms an unknown
// vision_probe_mode is a 400 system.vision_probe_mode_invalid (not a 500).
func TestSystemSettingsUpdateRejectsVisionProbeMode(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"vision_probe_mode":"bogus"}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT vision_probe_mode=bogus = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.vision_probe_mode_invalid" {
		t.Fatalf("error code = %q, want system.vision_probe_mode_invalid", code)
	}
}

// TestSystemSettingsUpdateRejectsNegativeEnergyDefault confirms a negative
// energy_default_price_per_kwh is a 400 system.energy_default_invalid (not a
// 500), and that a valid value round-trips through the PUT->GET.
func TestSystemSettingsUpdateRejectsNegativeEnergyDefault(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"energy_default_price_per_kwh":-0.1}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT energy_default_price_per_kwh=-0.1 = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.energy_default_invalid" {
		t.Fatalf("error code = %q, want system.energy_default_invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"energy_default_price_per_kwh":0.32,"energy_default_pue":1.4,"energy_default_wh_per_token":0.0025}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT energy defaults = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		EnergyDefaultPricePerKwh float64 `json:"energy_default_price_per_kwh"`
		EnergyDefaultPue         float64 `json:"energy_default_pue"`
		EnergyDefaultWhPerToken  float64 `json:"energy_default_wh_per_token"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.EnergyDefaultPricePerKwh != 0.32 || view.EnergyDefaultPue != 1.4 || view.EnergyDefaultWhPerToken != 0.0025 {
		t.Fatalf("energy defaults = %+v, want {0.32 1.4 0.0025}", view)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
	get.AddCookie(cookie)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/system/settings = %d, want 200 (body=%s)", getRec.Code, getRec.Body.String())
	}
	var getView struct {
		EnergyDefaultPricePerKwh float64 `json:"energy_default_price_per_kwh"`
		EnergyDefaultPue         float64 `json:"energy_default_pue"`
		EnergyDefaultWhPerToken  float64 `json:"energy_default_wh_per_token"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &getView); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if getView.EnergyDefaultPricePerKwh != 0.32 || getView.EnergyDefaultPue != 1.4 || getView.EnergyDefaultWhPerToken != 0.0025 {
		t.Fatalf("round-trip energy defaults = %+v, want {0.32 1.4 0.0025}", getView)
	}
}

// TestSystemSettingsUpdateRejectsIncompleteSMTP confirms enabling SMTP without
// a host is a 400 system.smtp_config_incomplete (not a 500).
func TestSystemSettingsUpdateRejectsIncompleteSMTP(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"smtp_enabled":true}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT smtp_enabled without host = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "system.smtp_config_incomplete" {
		t.Fatalf("error code = %q, want system.smtp_config_incomplete", code)
	}
}

// TestSystemSettingsUpdateRejectsCertRenewBeforeDays confirms an out-of-range
// cert_renew_before_days is a 400 cert.invalid (not the generic 500
// system.settings_update_failed fallthrough), and a valid value persists and
// round-trips in the view.
func TestSystemSettingsUpdateRejectsCertRenewBeforeDays(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	bad := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"cert_renew_before_days":1}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("PUT cert_renew_before_days=1 = %d, want 400 (body=%s)", badRec.Code, badRec.Body.String())
	}
	if code := decodeErrorCode(t, badRec.Body.Bytes()); code != "cert.invalid" {
		t.Fatalf("error code = %q, want cert.invalid", code)
	}

	ok := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"cert_renew_before_days":14}`))
	ok.Header.Set(csrfHeaderName, "1")
	ok.AddCookie(cookie)
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("PUT cert_renew_before_days=14 = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	var view struct {
		CertRenewBeforeDays int `json:"cert_renew_before_days"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if view.CertRenewBeforeDays != 14 {
		t.Fatalf("cert_renew_before_days = %d, want 14", view.CertRenewBeforeDays)
	}
}
