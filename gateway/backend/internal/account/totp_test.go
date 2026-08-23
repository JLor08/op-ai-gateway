// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account

import (
	"context"
	"errors"
	"op-ai-gateway/internal/totp"
	"strings"
	"testing"
	"time"
)

func TestSetPendingConfirmTOTP(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	secret, uri, err := svc.SetPendingTOTP(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("SetPendingTOTP: %v", err)
	}
	if secret == "" {
		t.Fatalf("secret = %q, want non-empty", secret)
	}
	if !strings.Contains(uri, "OP%20AI%20Gateway") && !strings.Contains(uri, "otpauth://totp/") {
		t.Fatalf("uri = %q, want issuer or otpauth prefix", uri)
	}

	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled {
		t.Fatalf("TOTPEnabled = true after SetPendingTOTP, want false")
	}
	if stored.TOTPSecret != "" {
		t.Fatalf("TOTPSecret = %q after SetPendingTOTP, want empty (secret must stage in TOTPPendingSecret only)", stored.TOTPSecret)
	}
	if stored.TOTPPendingSecret == "" {
		t.Fatalf("TOTPPendingSecret = empty after SetPendingTOTP, want the sealed pending secret")
	}

	code, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}

	u, err := svc.ConfirmTOTP(context.Background(), "usr_1", code)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if !u.TOTPEnabled {
		t.Fatalf("TOTPEnabled = false after ConfirmTOTP, want true")
	}
	if u.TOTPConfirmedAt == nil {
		t.Fatalf("TOTPConfirmedAt = nil after ConfirmTOTP, want set")
	}
	if u.TOTPSecret == "" {
		t.Fatalf("TOTPSecret = empty after ConfirmTOTP, want the promoted secret")
	}
	if u.TOTPPendingSecret != "" {
		t.Fatalf("TOTPPendingSecret = %q after ConfirmTOTP, want cleared", u.TOTPPendingSecret)
	}
	// The promoted live secret actually verifies (the pending blob moved, not a copy).
	liveCode, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if !svc.VerifyTOTP(u, liveCode) {
		t.Fatalf("VerifyTOTP after ConfirmTOTP promotion = false, want true")
	}
}

// TestSetPendingTOTPDoesNotDisturbLiveEnrollment proves the FIX for a
// CONFIRMED MAJOR bug: calling SetPendingTOTP on an already-enrolled user (as
// happens when re-enrollment is triggered, e.g. by a hijacked session cookie)
// must NOT touch the live TOTPSecret/TOTPEnabled — the current factor keeps
// authenticating logins until a NEW code is confirmed, so an attacker who can
// only start-but-not-finish enrollment (no access to the victim's
// authenticator) can never rebind or downgrade the account.
func TestSetPendingTOTPDoesNotDisturbLiveEnrollment(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	liveSecret, _, err := svc.SetPendingTOTP(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("SetPendingTOTP (initial): %v", err)
	}
	liveCode, err := totp.Code(liveSecret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	confirmed, err := svc.ConfirmTOTP(context.Background(), "usr_1", liveCode)
	if err != nil {
		t.Fatalf("ConfirmTOTP (initial): %v", err)
	}
	if !confirmed.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled=true after initial enrollment")
	}

	// An attacker (or an abandoned legitimate re-enroll) starts a SECOND
	// enrollment. This alone must not disable TOTP or clear the live secret.
	attackerSecret, _, err := svc.SetPendingTOTP(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("SetPendingTOTP (re-enroll): %v", err)
	}
	if attackerSecret == liveSecret {
		t.Fatalf("re-enroll produced the same secret as the live one")
	}

	stillStored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !stillStored.TOTPEnabled {
		t.Fatalf("TOTPEnabled = false after a bare re-enroll, want still true (live factor must survive until confirmed)")
	}

	// The ORIGINAL device's code still authenticates -- rebind requires
	// confirming the NEW secret, not merely starting enrollment.
	freshLiveCode, err := totp.Code(liveSecret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if !svc.VerifyTOTP(stillStored, freshLiveCode) {
		t.Fatalf("VerifyTOTP with the original live secret = false after a bare re-enroll, want true")
	}

	// The attacker's code (from the pending secret) does NOT verify against
	// the live secret -- it only becomes live once ConfirmTOTP promotes it.
	attackerCode, err := totp.Code(attackerSecret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if svc.VerifyTOTP(stillStored, attackerCode) {
		t.Fatalf("VerifyTOTP with the pending (unconfirmed) secret = true, want false")
	}
}

// TestVerifyTOTPEmptySecretRejected proves a defense-in-depth guard: even
// though totp.Verify itself now rejects an empty secret, VerifyTOTP must
// short-circuit on u.TOTPSecret == "" before ever opening/decoding it, so a
// user with no TOTP secret enrolled can never have a code "verify" against
// their account.
func TestVerifyTOTPEmptySecretRejected(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")
	u, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.TOTPSecret != "" {
		t.Fatalf("fresh user should have an empty TOTPSecret, got %q", u.TOTPSecret)
	}

	code, err := totp.Code("", time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if svc.VerifyTOTP(u, code) {
		t.Fatalf("VerifyTOTP with empty TOTPSecret = true, want false")
	}
}

func TestConfirmTOTPWrongCode(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	if _, _, err := svc.SetPendingTOTP(context.Background(), "usr_1"); err != nil {
		t.Fatalf("SetPendingTOTP: %v", err)
	}

	if _, err := svc.ConfirmTOTP(context.Background(), "usr_1", "000000"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("ConfirmTOTP wrong code err = %v, want ErrTOTPInvalid", err)
	}
}

// TestConfirmTOTPNotEnrolled proves ConfirmTOTP rejects a confirm attempt
// when there is no pending secret staged (nothing was ever enrolled, or a
// prior enrollment was already confirmed/cleared).
func TestConfirmTOTPNotEnrolled(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	if _, err := svc.ConfirmTOTP(context.Background(), "usr_1", "123456"); !errors.Is(err, ErrTOTPNotEnrolled) {
		t.Fatalf("ConfirmTOTP with no pending secret err = %v, want ErrTOTPNotEnrolled", err)
	}
}

func TestDisableTOTP(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	secret, _, err := svc.SetPendingTOTP(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("SetPendingTOTP: %v", err)
	}
	code, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := svc.ConfirmTOTP(context.Background(), "usr_1", code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	disableCode, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if err := svc.DisableTOTP(context.Background(), "usr_1", disableCode); err != nil {
		t.Fatalf("DisableTOTP: %v", err)
	}

	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled || stored.TOTPSecret != "" || stored.TOTPPendingSecret != "" || stored.TOTPConfirmedAt != nil {
		t.Fatalf("stored user not cleared after DisableTOTP: %+v", stored)
	}
}

func TestResetTOTPRevokesSessions(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "totp@example.com", "Passw0rd!", "user")

	secret, _, err := svc.SetPendingTOTP(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("SetPendingTOTP: %v", err)
	}
	code, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := svc.ConfirmTOTP(context.Background(), "usr_1", code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	_, sessionSecret, err := svc.Login(context.Background(), "totp@example.com", "Passw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.ResetTOTP(context.Background(), "usr_1"); err != nil {
		t.Fatalf("ResetTOTP: %v", err)
	}

	if _, err := svc.ResolveSession(context.Background(), sessionSecret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("ResolveSession after ResetTOTP err = %v, want ErrSessionInvalid", err)
	}

	stored, err := dir.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled || stored.TOTPSecret != "" || stored.TOTPPendingSecret != "" || stored.TOTPConfirmedAt != nil {
		t.Fatalf("stored user not cleared after ResetTOTP: %+v", stored)
	}
}
