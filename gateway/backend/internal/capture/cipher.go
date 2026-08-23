// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package capture provides at-rest encryption for opt-in payload captures
// (SP-C). Blobs are AES-256-GCM sealed as nonce ‖ ciphertext. A nil *Cipher
// means capture is disabled (fail-closed); every caller must nil-guard before
// use — the encryption key is the global on/off switch.
package capture

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeyVersion is the current capture encryption key version, persisted with every
// blob so a future rotation workflow can select the matching key. v1 wires a
// single key at this fixed version.
const KeyVersion = 1

// ErrKeyLength is returned by New when the decoded key is not exactly 32 bytes.
var ErrKeyLength = errors.New("capture: encryption key must decode to 32 bytes")

// ErrBlobTooShort is returned by Open when a blob is shorter than the GCM nonce.
var ErrBlobTooShort = errors.New("capture: blob shorter than nonce")

// Cipher seals and opens capture payloads with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New parses a hex-encoded 32-byte key and returns an AES-256-GCM Cipher. A
// malformed or wrong-length key is a fatal misconfiguration surfaced to the
// server-build path (never in config.Load).
func New(hexKey string) (*Cipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("capture: decode encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, ErrKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("capture: new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("capture: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plain and returns nonce ‖ ciphertext. The nonce is a fresh
// crypto/rand read per call. crypto/rand.Read is documented never to fail on
// supported platforms; a failure here is unrecoverable and panics (Seal has no
// error return by contract, and callers run it fire-and-forget).
func (c *Cipher) Seal(plain []byte) []byte {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(fmt.Sprintf("capture: read nonce: %v", err))
	}
	return c.aead.Seal(nonce, nonce, plain, nil)
}

// Open reverses Seal: it splits the nonce prefix, then authenticates and
// decrypts. A tampered or truncated blob returns an error and never leaks
// plaintext.
func (c *Cipher) Open(blob []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(blob) < nonceSize {
		return nil, ErrBlobTooShort
	}
	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("capture: open: %w", err)
	}
	return plain, nil
}
