// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
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

func TestAccountSealSecretRoundTrips(t *testing.T) {
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	svc := NewService(Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir, Cipher: newTestCipher(t)}, Config{})
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

func TestAccountSealSecretVolatilePlaintext(t *testing.T) {
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	svc := NewService(Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir, SettingsVolatile: true}, Config{})
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

func TestAccountSealSecretDiskWithoutKeyRejected(t *testing.T) {
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	svc := NewService(Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir}, Config{})
	if _, err := svc.sealSecret("hunter2"); !errors.Is(err, ErrTOTPKeyRequired) {
		t.Fatalf("sealSecret on disk-without-key error = %v, want ErrTOTPKeyRequired", err)
	}
}
