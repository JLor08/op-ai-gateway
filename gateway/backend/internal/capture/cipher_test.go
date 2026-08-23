// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package capture

import (
	"bytes"
	"strings"
	"testing"
)

// validHexKey is 64 hex chars = 32 bytes, the AES-256 key size.
const validHexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestCipherRoundTrip(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	plain := []byte(`{"req_body":"hello","truncated":false}`)

	blob := c.Seal(plain)
	if bytes.Equal(blob, plain) {
		t.Fatalf("Seal returned plaintext")
	}
	if len(blob) <= len(plain) {
		t.Fatalf("blob len = %d, want > plaintext len %d (nonce+tag)", len(blob), len(plain))
	}

	got, err := c.Open(blob)
	if err != nil {
		t.Fatalf("Open returned %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, want %q", got, plain)
	}
}

func TestCipherSealUsesRandomNonce(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	a := c.Seal([]byte("same"))
	b := c.Seal([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatalf("Seal produced identical blobs for identical plaintext (nonce not random)")
	}
}

func TestNewRejectsWrongLengthKey(t *testing.T) {
	// 16 bytes (32 hex chars) is valid hex but wrong length for AES-256.
	if _, err := New(strings.Repeat("ab", 16)); err == nil {
		t.Fatalf("New accepted a 16-byte key, want error")
	}
}

func TestNewRejectsNonHexKey(t *testing.T) {
	if _, err := New("zzzz-not-hex-at-all"); err == nil {
		t.Fatalf("New accepted a non-hex key, want error")
	}
}

func TestOpenRejectsTamperedBlob(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	blob := c.Seal([]byte("secret payload"))
	blob[len(blob)-1] ^= 0xff // flip a ciphertext/tag byte

	if _, err := c.Open(blob); err == nil {
		t.Fatalf("Open accepted a tampered blob, want error")
	}
}

func TestOpenRejectsShortBlob(t *testing.T) {
	c, err := New(validHexKey)
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if _, err := c.Open([]byte{0x01, 0x02}); err == nil {
		t.Fatalf("Open accepted a blob shorter than the nonce, want error")
	}
}

func TestKeyVersionIsOne(t *testing.T) {
	if KeyVersion != 1 {
		t.Fatalf("KeyVersion = %d, want 1", KeyVersion)
	}
}
