// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package capture

import (
	"errors"
	"strings"
	"testing"
)

// secret_test reuses validHexKey (defined in cipher_test.go, same package) to build a real Cipher.

func TestSealSecretEncRoundTrip(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	sealed, err := SealSecret(c, false, "sk-super-secret")
	if err != nil {
		t.Fatalf("SealSecret returned %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("expected enc: prefix, got %q", sealed)
	}
	if strings.Contains(sealed, "sk-super-secret") {
		t.Fatalf("sealed value leaks plaintext: %q", sealed)
	}
	opened, err := OpenSecret(c, sealed)
	if err != nil {
		t.Fatalf("OpenSecret returned %v", err)
	}
	if opened != "sk-super-secret" {
		t.Fatalf("round-trip mismatch: got %q want %q", opened, "sk-super-secret")
	}
}

func TestSealSecretPlainFallback(t *testing.T) {
	// Volatile store, no cipher → plain: prefix, openable without a cipher.
	sealed, err := SealSecret(nil, true, "sk-plain")
	if err != nil {
		t.Fatalf("SealSecret returned %v", err)
	}
	if sealed != "plain:sk-plain" {
		t.Fatalf("expected plain:sk-plain, got %q", sealed)
	}
	opened, err := OpenSecret(nil, sealed)
	if err != nil {
		t.Fatalf("OpenSecret returned %v", err)
	}
	if opened != "sk-plain" {
		t.Fatalf("plain round-trip mismatch: got %q want %q", opened, "sk-plain")
	}
}

func TestSealSecretKeylessDiskRejected(t *testing.T) {
	// Disk store (not volatile) with no cipher → refuse to persist plaintext.
	if _, err := SealSecret(nil, false, "sk-secret"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("expected ErrKeyRequired, got %v", err)
	}
}

func TestOpenSecretEncWithoutCipher(t *testing.T) {
	// A stored enc: value cannot be opened without a cipher.
	if _, err := OpenSecret(nil, "enc:abc123"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("expected ErrKeyRequired, got %v", err)
	}
}

func TestOpenSecretMalformedRejected(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	// A value with neither prefix is corrupt → never leaked.
	if _, err := OpenSecret(c, "bare-value-no-prefix"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("expected ErrKeyRequired for unprefixed value, got %v", err)
	}
	// A malformed base64 enc: payload is a decode error (not silently leaked).
	if _, err := OpenSecret(c, "enc:!!!not-base64!!!"); err == nil {
		t.Fatalf("expected an error for malformed base64, got nil")
	}
}

func TestSealOpenSecretEmpty(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	sealed, err := SealSecret(c, false, "")
	if err != nil {
		t.Fatalf("SealSecret(empty) returned %v", err)
	}
	if sealed != "" {
		t.Fatalf("empty plain should seal to empty, got %q", sealed)
	}
	opened, err := OpenSecret(c, "")
	if err != nil {
		t.Fatalf("OpenSecret(empty) returned %v", err)
	}
	if opened != "" {
		t.Fatalf("empty stored should open to empty, got %q", opened)
	}
}
