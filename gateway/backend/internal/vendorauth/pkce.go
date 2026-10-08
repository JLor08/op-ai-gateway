// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// randomBytes is the entropy behind a PKCE verifier and an OAuth state: 32
// bytes encode to 43 base64url characters, the minimum RFC 7636 §4.1 allows.
const randomBytes = 32

// GeneratePKCE returns a fresh PKCE (RFC 7636) code_verifier and its S256
// code_challenge. The verifier is the base64url (no padding) encoding of 32
// crypto/rand bytes; the challenge is the base64url (no padding) SHA-256 of
// the verifier. The verifier is a secret: keep it server-side until the code
// exchange.
func GeneratePKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLString()
	if err != nil {
		return "", "", fmt.Errorf("vendorauth: generate pkce verifier: %w", err)
	}
	return verifier, PKCEChallenge(verifier), nil
}

// PKCEChallenge returns the S256 code_challenge for a verifier:
// BASE64URL-NOPAD(SHA256(ASCII(verifier))).
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomState returns a fresh unguessable OAuth state value (base64url, no
// padding, 32 crypto/rand bytes) for CSRF protection of the authorize round
// trip.
func RandomState() (string, error) {
	state, err := randomURLString()
	if err != nil {
		return "", fmt.Errorf("vendorauth: generate state: %w", err)
	}
	return state, nil
}

func randomURLString() (string, error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
