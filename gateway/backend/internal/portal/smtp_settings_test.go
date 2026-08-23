// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/capture"
	"strings"
	"testing"
)

// newTestCipher builds a real AES-256-GCM cipher from a fixed 32-byte key
// (64 hex chars) for the seal/open round-trip tests.
func newTestCipher(t *testing.T) *capture.Cipher {
	t.Helper()
	c, err := capture.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	return c
}

func TestSealSecretWithCipherRoundTrips(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: fixedClock()})
	sealed, err := svc.sealSecret("hunter2")
	if err != nil {
		t.Fatalf("sealSecret: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("sealed = %q, want enc: prefix", sealed)
	}
	got, err := svc.openSecret(sealed)
	if err != nil {
		t.Fatalf("openSecret: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("openSecret = %q, want %q", got, "hunter2")
	}
}

func TestSealSecretVolatilePlaintext(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), SettingsVolatile: true, Clock: fixedClock()})
	sealed, err := svc.sealSecret("hunter2")
	if err != nil {
		t.Fatalf("sealSecret: %v", err)
	}
	if sealed != "plain:hunter2" {
		t.Fatalf("sealed = %q, want %q", sealed, "plain:hunter2")
	}
	got, err := svc.openSecret(sealed)
	if err != nil || got != "hunter2" {
		t.Fatalf("openSecret = %q, %v; want hunter2, nil", got, err)
	}
}

func TestSealSecretDiskWithoutKeyRejected(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	if _, err := svc.sealSecret("hunter2"); !errors.Is(err, ErrSMTPKeyRequired) {
		t.Fatalf("sealSecret on disk-without-key error = %v, want ErrSMTPKeyRequired", err)
	}
}

func TestOpenSecretEmptyIsEmpty(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	got, err := svc.openSecret("")
	if err != nil || got != "" {
		t.Fatalf("openSecret(\"\") = %q, %v; want \"\", nil", got, err)
	}
}

func TestSMTPEnabledHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"smtp_enabled": ""}, false},
		{map[string]string{"smtp_enabled": "nope"}, false},
		{map[string]string{"smtp_enabled": "true"}, true},
		{map[string]string{"smtp_enabled": "false"}, false},
	}
	for _, tc := range cases {
		if got := SMTPEnabled(tc.in); got != tc.want {
			t.Fatalf("SMTPEnabled(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSMTPPortHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want int
	}{
		{map[string]string{}, 587},
		{map[string]string{"smtp_port": ""}, 587},
		{map[string]string{"smtp_port": "abc"}, 587},
		{map[string]string{"smtp_port": "0"}, 587},
		{map[string]string{"smtp_port": "70000"}, 587},
		{map[string]string{"smtp_port": "25"}, 25},
		{map[string]string{"smtp_port": "465"}, 465},
	}
	for _, tc := range cases {
		if got := SMTPPort(tc.in); got != tc.want {
			t.Fatalf("SMTPPort(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSMTPTLSModeHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want string
	}{
		{map[string]string{}, "starttls"},
		{map[string]string{"smtp_tls_mode": ""}, "starttls"},
		{map[string]string{"smtp_tls_mode": "wat"}, "starttls"},
		{map[string]string{"smtp_tls_mode": "ssl"}, "ssl"},
		{map[string]string{"smtp_tls_mode": "none"}, "none"},
		{map[string]string{"smtp_tls_mode": "starttls"}, "starttls"},
	}
	for _, tc := range cases {
		if got := SMTPTLSMode(tc.in); got != tc.want {
			t.Fatalf("SMTPTLSMode(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSMTPRuntimeConfigDefaults(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	cfg, err := svc.SMTPRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("SMTPRuntimeConfig: %v", err)
	}
	if cfg.Enabled || cfg.Port != 587 || cfg.TLSMode != "starttls" || cfg.Password != "" {
		t.Fatalf("defaults = %+v, want disabled/587/starttls/empty", cfg)
	}
}

func TestSMTPRuntimeConfigReadsSealedPassword(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})
	// Seal the password DIRECTLY into the settings store (not via the Task-3
	// write path / SMTPPassword request field) so this task stands alone and
	// compiles/passes in isolation. sealSecret + openSecret land in Task 1.
	sealed, err := svc.sealSecret("hunter2")
	if err != nil {
		t.Fatalf("sealSecret: %v", err)
	}
	if err := settings.SetSystemSetting(context.Background(), smtpPasswordKey, sealed, fixedClock()()); err != nil {
		t.Fatalf("SetSystemSetting(password): %v", err)
	}
	cfg, err := svc.SMTPRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("SMTPRuntimeConfig: %v", err)
	}
	if cfg.Password != "hunter2" {
		t.Fatalf("Password = %q, want %q", cfg.Password, "hunter2")
	}
}

func TestUpdateSystemSettingsPersistsSMTPBasics(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		SMTPHost:    strPtr("smtp.example.test"),
		SMTPPort:    intPtr(465),
		SMTPFrom:    strPtr("noreply@example.test"),
		SMTPTLSMode: strPtr("ssl"),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if got.SMTPHost != "smtp.example.test" || got.SMTPPort != 465 || got.SMTPFrom != "noreply@example.test" || got.SMTPTLSMode != "ssl" {
		t.Fatalf("DTO = %+v, want host/465/from/ssl", got)
	}
	values, _ := settings.SystemSettings(context.Background())
	if values["smtp_host"] != "smtp.example.test" || values["smtp_port"] != "465" || values["smtp_tls_mode"] != "ssl" {
		t.Fatalf("stored = %v", values)
	}
}

func TestUpdateSMTPPasswordKeepClearReplace(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})

	// replace: non-empty -> stored + smtp_password_set true
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPPassword: strPtr("secret1")})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !got.SMTPPasswordSet {
		t.Fatalf("after replace SMTPPasswordSet = false, want true")
	}
	values, _ := settings.SystemSettings(context.Background())
	if !strings.HasPrefix(values["smtp_password"], "enc:") {
		t.Fatalf("stored password = %q, want enc: prefix", values["smtp_password"])
	}

	// keep: nil -> unchanged
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPHost: strPtr("h")}); err != nil {
		t.Fatalf("keep: %v", err)
	}
	after, _ := settings.SystemSettings(context.Background())
	if after["smtp_password"] != values["smtp_password"] {
		t.Fatalf("keep changed the password: %q -> %q", values["smtp_password"], after["smtp_password"])
	}

	// clear: "" -> stored "" + smtp_password_set false
	got, err = svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPPassword: strPtr("")})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got.SMTPPasswordSet {
		t.Fatalf("after clear SMTPPasswordSet = true, want false")
	}
	cleared, _ := settings.SystemSettings(context.Background())
	if cleared["smtp_password"] != "" {
		t.Fatalf("stored password after clear = %q, want empty", cleared["smtp_password"])
	}
}

func TestUpdateSMTPPasswordDiskWithoutKeyRejected(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()}) // no cipher, not volatile
	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPPassword: strPtr("secret")})
	if !errors.Is(err, ErrSMTPKeyRequired) {
		t.Fatalf("error = %v, want ErrSMTPKeyRequired", err)
	}
}

func TestUpdateSMTPEnableValidation(t *testing.T) {
	newSvc := func() *Service {
		return NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	}
	// enable with no host/from -> incomplete
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPEnabled: boolPtr(true)}); !errors.Is(err, ErrSMTPConfigIncomplete) {
		t.Fatalf("incomplete error = %v, want ErrSMTPConfigIncomplete", err)
	}
	// enable with a malformed from -> from invalid
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		SMTPEnabled: boolPtr(true), SMTPHost: strPtr("h"), SMTPFrom: strPtr("not-an-email"),
	}); !errors.Is(err, ErrSMTPFromInvalid) {
		t.Fatalf("from error = %v, want ErrSMTPFromInvalid", err)
	}
	// out-of-range port -> port invalid (regardless of enabled)
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPPort: intPtr(70000)}); !errors.Is(err, ErrSMTPPortInvalid) {
		t.Fatalf("port error = %v, want ErrSMTPPortInvalid", err)
	}
	// bad tls mode -> tls mode invalid
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPTLSMode: strPtr("wat")}); !errors.Is(err, ErrSMTPTLSModeInvalid) {
		t.Fatalf("tls error = %v, want ErrSMTPTLSModeInvalid", err)
	}
	// valid enable succeeds
	if _, err := newSvc().UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		SMTPEnabled: boolPtr(true), SMTPHost: strPtr("smtp.example.test"), SMTPFrom: strPtr("noreply@example.test"),
	}); err != nil {
		t.Fatalf("valid enable: %v", err)
	}
}

// TestUpdateSystemSettingsSMTPFromStoredBare guards against storing the
// whole "Name <addr>" string as the envelope smtp_from: net/mail.ParseAddress
// is used to validate the address, but only the bare .Address must be
// persisted (the display name belongs in smtp_from_name) — otherwise the
// envelope MAIL FROM sent to the SMTP relay is malformed and every send fails.
func TestUpdateSystemSettingsSMTPFromStoredBare(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		SMTPFrom: strPtr("OP Gateway <noreply@example.test>"),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if got.SMTPFrom != "noreply@example.test" {
		t.Fatalf("DTO SMTPFrom = %q, want bare %q", got.SMTPFrom, "noreply@example.test")
	}
	values, _ := settings.SystemSettings(context.Background())
	if values[smtpFromKey] != "noreply@example.test" {
		t.Fatalf("stored smtp_from = %q, want bare %q", values[smtpFromKey], "noreply@example.test")
	}
	cfg, err := svc.SMTPRuntimeConfig(context.Background())
	if err != nil {
		t.Fatalf("SMTPRuntimeConfig: %v", err)
	}
	if cfg.From != "noreply@example.test" {
		t.Fatalf("SMTPRuntimeConfig().From = %q, want bare %q", cfg.From, "noreply@example.test")
	}
}

// TestUpdateSystemSettingsSMTPFailureLeavesThemeUnchanged guards against a
// partial write: previously, non-SMTP fields (like theme) were persisted
// immediately, before the SMTP block validated. A subsequent SMTP validation
// failure then still returned an error, but the earlier field had already
// been committed to the store. UpdateSystemSettings must validate everything
// — including the SMTP block — before writing anything.
func TestUpdateSystemSettingsSMTPFailureLeavesThemeUnchanged(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{Theme: strPtr("skynet")}); err != nil {
		t.Fatalf("seed theme: %v", err)
	}

	_, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		Theme:       strPtr("matrix"),
		SMTPEnabled: boolPtr(true), // no host/from -> ErrSMTPConfigIncomplete
	})
	if !errors.Is(err, ErrSMTPConfigIncomplete) {
		t.Fatalf("error = %v, want ErrSMTPConfigIncomplete", err)
	}

	values, _ := settings.SystemSettings(context.Background())
	if values["theme"] != "skynet" {
		t.Fatalf("theme = %q, want unchanged %q (no partial write on SMTP validation failure)", values["theme"], "skynet")
	}
}

func TestSystemSettingsDTONeverExposesPassword(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCipher(t), Clock: fixedClock()})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{SMTPPassword: strPtr("topsecret")}); err != nil {
		t.Fatalf("set password: %v", err)
	}
	blob, err := json.Marshal(svc.SystemSettingsView(context.Background()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "topsecret") || strings.Contains(string(blob), "smtp_password\"") {
		t.Fatalf("DTO JSON leaks the password: %s", blob)
	}
	if !strings.Contains(string(blob), "\"smtp_password_set\":true") {
		t.Fatalf("DTO JSON missing smtp_password_set: %s", blob)
	}
}
