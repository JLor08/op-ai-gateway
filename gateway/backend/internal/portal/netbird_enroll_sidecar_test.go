// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newEnrollSidecarService builds a portal.Service with the NetBird module either
// enabled (pointing at url) or disabled, and netbirdKeyFile set to keyFile (may be
// empty). It reuses the netbird_server_test fake + enableNetbird helper.
func newEnrollSidecarService(t *testing.T, url string, enabled bool, keyFile string) *Service {
	t.Helper()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc := NewService(ServiceDeps{
		SystemSettings: NewMemorySystemSettings(),
		Cipher:         newTestCipher(t),
		NetbirdKeyFile: keyFile,
		Clock:          func() time.Time { return now },
	})
	enableNetbird(t, svc, url, enabled)
	return svc
}

// TestEnrollGatewaySidecarWritesKeyFile: with the module enabled + a key-file path
// configured, EnrollGatewaySidecar mints a one-off setup key AND writes it to the
// file atomically with 0600 perms, returning the key + the `netbird up` command.
func TestEnrollGatewaySidecarWritesKeyFile(t *testing.T) {
	fake := newFakeNetbird(t)
	keyFile := filepath.Join(t.TempDir(), "netbird-setup-key")
	svc := newEnrollSidecarService(t, fake.srv.URL, true, keyFile)

	key, command, err := svc.EnrollGatewaySidecar(context.Background(), systemToken())
	if err != nil {
		t.Fatalf("EnrollGatewaySidecar: %v", err)
	}
	if key != "nbkey-secret-value" {
		t.Fatalf("key = %q, want the minted key", key)
	}
	if want := "netbird up --management-url " + fake.srv.URL + " --setup-key nbkey-secret-value"; command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}

	// The file exists and its content is EXACTLY the key.
	data, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if string(data) != key {
		t.Fatalf("key file content = %q, want %q", string(data), key)
	}

	// Its mode is 0600 (not group/world readable).
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 0600", perm)
	}
}

// TestEnrollGatewaySidecarNotConfigured: with no key-file path configured, the
// action returns ErrNetbirdKeyFileNotConfigured and makes ZERO NetBird calls (the
// gate is checked before any mint).
func TestEnrollGatewaySidecarNotConfigured(t *testing.T) {
	fake := newFakeNetbird(t)
	svc := newEnrollSidecarService(t, fake.srv.URL, true, "")
	before := fake.count()

	key, command, err := svc.EnrollGatewaySidecar(context.Background(), systemToken())
	if !errors.Is(err, ErrNetbirdKeyFileNotConfigured) {
		t.Fatalf("err = %v, want ErrNetbirdKeyFileNotConfigured", err)
	}
	if key != "" || command != "" {
		t.Fatalf("key/command = %q/%q, want empty", key, command)
	}
	if got := fake.count() - before; got != 0 {
		t.Fatalf("NetBird calls = %d, want 0 (gate before mint)", got)
	}
}

// TestEnrollGatewaySidecarModuleDisabled: with a key-file configured but the module
// off, the action returns ErrNetbirdModuleDisabled and never writes the file.
func TestEnrollGatewaySidecarModuleDisabled(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "netbird-setup-key")
	svc := newEnrollSidecarService(t, "http://netbird.invalid", false, keyFile)

	_, _, err := svc.EnrollGatewaySidecar(context.Background(), systemToken())
	if !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("err = %v, want ErrNetbirdModuleDisabled", err)
	}
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Fatalf("key file should not exist, stat err = %v", statErr)
	}
}

// TestEnrollGatewaySidecarForbidsNonSystem proves the PT-2 Part 2b internal
// authz guard: a principal without the "system" scope is rejected with
// ErrPrincipalForbidden, makes ZERO NetBird calls, and never writes the key
// file -- the HTTP-level gate (requireWebScope("system") in
// handleSystemNetbirdEnrollSidecar) is defense-in-depth on TOP of this, not
// instead of it.
func TestEnrollGatewaySidecarForbidsNonSystem(t *testing.T) {
	fake := newFakeNetbird(t)
	keyFile := filepath.Join(t.TempDir(), "netbird-setup-key")
	svc := newEnrollSidecarService(t, fake.srv.URL, true, keyFile)
	before := fake.count()

	for _, tc := range []struct {
		name string
		tok  auth.Token
	}{
		{"plain admin (no system scope)", adminToken()},
		{"owner", ownerToken()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, command, err := svc.EnrollGatewaySidecar(context.Background(), tc.tok)
			if !errors.Is(err, ErrPrincipalForbidden) {
				t.Fatalf("EnrollGatewaySidecar(%s) err = %v, want ErrPrincipalForbidden", tc.name, err)
			}
			if key != "" || command != "" {
				t.Fatalf("key/command = %q/%q, want empty", key, command)
			}
		})
	}
	if got := fake.count() - before; got != 0 {
		t.Fatalf("NetBird calls = %d, want 0 (guard before mint)", got)
	}
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Fatalf("key file should not exist, stat err = %v", statErr)
	}

	// The flip side: a system-scoped principal succeeds exactly as before the
	// guard was added.
	key, _, err := svc.EnrollGatewaySidecar(context.Background(), systemToken())
	if err != nil {
		t.Fatalf("EnrollGatewaySidecar(system): %v", err)
	}
	if key == "" {
		t.Fatal("system principal must still be able to enroll")
	}
	if _, statErr := os.Stat(keyFile); statErr != nil {
		t.Fatalf("key file should now exist: %v", statErr)
	}
}
