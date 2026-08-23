// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/totp"
	"strings"
	"testing"
	"time"
)

// TestPortalTOTPEnrollConfirmDisable confirms the full self-service TOTP
// lifecycle under totp_mode=optional: enroll returns a secret, confirm with a
// valid code enables it, and disable with a fresh code clears it again.
func TestPortalTOTPEnrollConfirmDisable(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode: %v", err)
	}
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("enroll should be 200, got %d body=%s", enrollRec.Code, enrollRec.Body.String())
	}
	if !strings.Contains(enrollRec.Body.String(), `"secret_base32"`) {
		t.Fatalf("enroll response missing secret_base32: %s", enrollRec.Body.String())
	}
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	if enrollBody.SecretBase32 == "" {
		t.Fatalf("expected non-empty secret_base32, body=%s", enrollRec.Body.String())
	}

	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm should be 200, got %d body=%s", confirmRec.Code, confirmRec.Body.String())
	}
	if !strings.Contains(confirmRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("expected totp_enabled:true after confirm, got %s", confirmRec.Body.String())
	}

	code2, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate second code: %v", err)
	}
	disableReq, err := http.NewRequest(http.MethodDelete, "/api/portal/totp", strings.NewReader(`{"code":"`+code2+`"}`))
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	disableReq.Header.Set(csrfHeaderName, "1")
	disableReq.AddCookie(cookie)
	disableRec := httptest.NewRecorder()
	srv.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable should be 200, got %d body=%s", disableRec.Code, disableRec.Body.String())
	}

	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled=false after disable, got true")
	}
	if stored.TOTPSecret != "" {
		t.Fatalf("expected TOTPSecret cleared after disable, got %q", stored.TOTPSecret)
	}
	if stored.TOTPConfirmedAt != nil {
		t.Fatalf("expected TOTPConfirmedAt cleared after disable, got %v", stored.TOTPConfirmedAt)
	}
}

// TestPortalTOTPEnrollForbiddenWhenOff confirms that with the default
// totp_mode=off, self-service enrollment is rejected.
func TestPortalTOTPEnrollForbiddenWhenOff(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusConflict {
		t.Fatalf("enroll under totp_mode=off should be 409, got %d body=%s", enrollRec.Code, enrollRec.Body.String())
	}
}

// TestPortalTOTPReEnrollRequiresCurrentCode is the FIX for a CONFIRMED MAJOR
// bug: re-enrolling an already-enabled user via POST /api/portal/totp/enroll
// used to overwrite the live secret and flip TOTPEnabled=false with no proof
// of the current factor, so a hijacked session cookie (without the physical
// authenticator) could rebind 2FA to an attacker's device, or a benign
// abandoned re-enroll would silently downgrade the account to
// password-only. Enrolling while already enabled must now require a valid
// CURRENT code as a step-up check: (a) without a code, 401; (b) with the
// valid current code, 200 + a fresh pending secret; (c) confirming that
// pending secret with its own code promotes it, and the OLD secret's codes
// stop authenticating while the NEW secret's do.
func TestPortalTOTPReEnrollRequiresCurrentCode(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode: %v", err)
	}
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	// Initial enrollment (no prior factor, so no code required).
	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("initial enroll = %d, want 200 (body=%s)", enrollRec.Code, enrollRec.Body.String())
	}
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	originalSecret := enrollBody.SecretBase32

	code, err := totp.Code(originalSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("initial confirm = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}

	// (a) Re-enroll WITHOUT a code -> 401, and the live secret is untouched.
	reenrollNoCode := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	reenrollNoCode.Header.Set(csrfHeaderName, "1")
	reenrollNoCode.AddCookie(cookie)
	reenrollNoCodeRec := httptest.NewRecorder()
	srv.ServeHTTP(reenrollNoCodeRec, reenrollNoCode)
	if reenrollNoCodeRec.Code != http.StatusUnauthorized {
		t.Fatalf("re-enroll without code = %d, want 401 (body=%s)", reenrollNoCodeRec.Code, reenrollNoCodeRec.Body.String())
	}
	if got := decodeErrorCode(t, reenrollNoCodeRec.Body.Bytes()); got != "auth.totp_invalid" {
		t.Fatalf("re-enroll without code error = %q, want auth.totp_invalid", got)
	}
	stillOriginal, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !stillOriginal.TOTPEnabled {
		t.Fatalf("TOTPEnabled = false after a rejected re-enroll attempt, want still true")
	}

	// (b) Re-enroll WITH the valid current code -> 200 + a fresh pending secret.
	currentCode, err := totp.Code(originalSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate current code: %v", err)
	}
	reenroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", strings.NewReader(`{"code":"`+currentCode+`"}`))
	reenroll.Header.Set(csrfHeaderName, "1")
	reenroll.AddCookie(cookie)
	reenrollRec := httptest.NewRecorder()
	srv.ServeHTTP(reenrollRec, reenroll)
	if reenrollRec.Code != http.StatusOK {
		t.Fatalf("re-enroll with valid code = %d, want 200 (body=%s)", reenrollRec.Code, reenrollRec.Body.String())
	}
	var reenrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(reenrollRec.Body.Bytes(), &reenrollBody); err != nil {
		t.Fatalf("unmarshal re-enroll response: %v", err)
	}
	newSecret := reenrollBody.SecretBase32
	if newSecret == "" || newSecret == originalSecret {
		t.Fatalf("new pending secret = %q, want non-empty and different from %q", newSecret, originalSecret)
	}

	// The live factor is STILL the original until the new one is confirmed.
	stillLive, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !stillLive.TOTPEnabled {
		t.Fatalf("TOTPEnabled = false after re-enroll (pre-confirm), want still true")
	}

	// (c) Confirming the NEW secret's code promotes it.
	newCode, err := totp.Code(newSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate new code: %v", err)
	}
	confirmNew := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+newCode+`"}`))
	confirmNew.Header.Set(csrfHeaderName, "1")
	confirmNew.AddCookie(cookie)
	confirmNewRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmNewRec, confirmNew)
	if confirmNewRec.Code != http.StatusOK {
		t.Fatalf("confirm new secret = %d, want 200 (body=%s)", confirmNewRec.Code, confirmNewRec.Body.String())
	}

	// Login now requires the NEW secret's code; the OLD secret's code fails.
	oldLoginCode, err := totp.Code(originalSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate old code: %v", err)
	}
	oldLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"`+oldLoginCode+`"}`))
	oldLogin.Header.Set(csrfHeaderName, "1")
	oldLoginRec := httptest.NewRecorder()
	srv.ServeHTTP(oldLoginRec, oldLogin)
	if oldLoginRec.Code != http.StatusUnauthorized {
		t.Fatalf("login with OLD secret's code after rebind = %d, want 401 (body=%s)", oldLoginRec.Code, oldLoginRec.Body.String())
	}

	newLoginCode, err := totp.Code(newSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate new login code: %v", err)
	}
	newLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"`+newLoginCode+`"}`))
	newLogin.Header.Set(csrfHeaderName, "1")
	newLoginRec := httptest.NewRecorder()
	srv.ServeHTTP(newLoginRec, newLogin)
	if newLoginRec.Code != http.StatusOK {
		t.Fatalf("login with NEW secret's code after rebind = %d, want 200 (body=%s)", newLoginRec.Code, newLoginRec.Body.String())
	}
}

// TestPortalTOTPReEnrollWithoutCodeCannotHijackAccount is the attacker-style
// scenario: someone holding only the victim's session cookie (e.g. via XSS
// or a stolen cookie), NOT the victim's physical authenticator, tries to
// rebind 2FA by POSTing enroll then confirm with no current code. Both steps
// must fail, and the victim's original factor must keep authenticating
// afterward -- proving the account cannot be hijacked or downgraded this way.
func TestPortalTOTPReEnrollWithoutCodeCannotHijackAccount(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode: %v", err)
	}
	seedLoginUser(t, dir, "usr_1", "victim@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "victim@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	originalSecret := enrollBody.SecretBase32
	code, err := totp.Code(originalSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("initial confirm = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}

	// Attacker (session cookie only): enroll with no code -> rejected.
	attackerEnroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	attackerEnroll.Header.Set(csrfHeaderName, "1")
	attackerEnroll.AddCookie(cookie)
	attackerEnrollRec := httptest.NewRecorder()
	srv.ServeHTTP(attackerEnrollRec, attackerEnroll)
	if attackerEnrollRec.Code != http.StatusUnauthorized {
		t.Fatalf("attacker enroll without code = %d, want 401 (body=%s)", attackerEnrollRec.Code, attackerEnrollRec.Body.String())
	}

	// Attacker tries to confirm anyway with a made-up code -> no pending
	// secret exists to confirm (the enroll above never got that far).
	attackerConfirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"000000"}`))
	attackerConfirm.Header.Set(csrfHeaderName, "1")
	attackerConfirm.AddCookie(cookie)
	attackerConfirmRec := httptest.NewRecorder()
	srv.ServeHTTP(attackerConfirmRec, attackerConfirm)
	if attackerConfirmRec.Code != http.StatusConflict {
		t.Fatalf("attacker confirm without a pending secret = %d, want 409 (body=%s)", attackerConfirmRec.Code, attackerConfirmRec.Body.String())
	}
	if got := decodeErrorCode(t, attackerConfirmRec.Body.Bytes()); got != "auth.totp_not_enrolled" {
		t.Fatalf("attacker confirm error = %q, want auth.totp_not_enrolled", got)
	}

	// The victim's ORIGINAL factor still authenticates -- the account was
	// neither rebound nor downgraded.
	victimCode, err := totp.Code(originalSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate victim code: %v", err)
	}
	victimLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"victim@example.test","password":"password-1","totp_code":"`+victimCode+`"}`))
	victimLogin.Header.Set(csrfHeaderName, "1")
	victimLoginRec := httptest.NewRecorder()
	srv.ServeHTTP(victimLoginRec, victimLogin)
	if victimLoginRec.Code != http.StatusOK {
		t.Fatalf("victim login with original factor = %d, want 200 (body=%s)", victimLoginRec.Code, victimLoginRec.Body.String())
	}
	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !stored.TOTPEnabled {
		t.Fatalf("TOTPEnabled = false after the attacker's attempt, want still true")
	}
}

// TestPortalTOTPDisableForbiddenWhenRequired confirms that under
// totp_mode=required, an already-enrolled user cannot self-service disable.
func TestPortalTOTPDisableForbiddenWhenRequired(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode optional: %v", err)
	}
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("enroll should be 200, got %d body=%s", enrollRec.Code, enrollRec.Body.String())
	}
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm should be 200, got %d body=%s", confirmRec.Code, confirmRec.Body.String())
	}

	requiredMode := "required"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &requiredMode}); err != nil {
		t.Fatalf("set totp_mode required: %v", err)
	}

	code2, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate second code: %v", err)
	}
	disableReq, err := http.NewRequest(http.MethodDelete, "/api/portal/totp", strings.NewReader(`{"code":"`+code2+`"}`))
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	disableReq.Header.Set(csrfHeaderName, "1")
	disableReq.AddCookie(cookie)
	disableRec := httptest.NewRecorder()
	srv.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusConflict {
		t.Fatalf("disable under totp_mode=required should be 409, got %d body=%s", disableRec.Code, disableRec.Body.String())
	}
	if !strings.Contains(disableRec.Body.String(), "auth.totp_disable_forbidden") {
		t.Fatalf("expected auth.totp_disable_forbidden, got %s", disableRec.Body.String())
	}
}

// TestPortalTOTPDisableAllowedWhenOff confirms the FIX for a real dead-end:
// a user enrolled under totp_mode=optional whose org later turns TOTP off
// entirely must still be able to remove their own stale enrollment via
// self-service disable (blocked only under totp_mode=required).
func TestPortalTOTPDisableAllowedWhenOff(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode optional: %v", err)
	}
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}

	offMode := "off"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &offMode}); err != nil {
		t.Fatalf("set totp_mode off: %v", err)
	}

	code2, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate second code: %v", err)
	}
	disableReq, err := http.NewRequest(http.MethodDelete, "/api/portal/totp", strings.NewReader(`{"code":"`+code2+`"}`))
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	disableReq.Header.Set(csrfHeaderName, "1")
	disableReq.AddCookie(cookie)
	disableRec := httptest.NewRecorder()
	srv.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable under totp_mode=off = %d, want 200 (body=%s)", disableRec.Code, disableRec.Body.String())
	}

	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled=false after disable under off-mode, got true")
	}
}
