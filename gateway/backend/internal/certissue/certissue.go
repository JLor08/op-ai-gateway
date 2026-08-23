// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// Result is one issued certificate: the PEM chain (leaf first), the freshly
// generated private key in PEM, the parsed leaf, and the leaf's SHA-256
// fingerprint (hex) used as the distribution/ETag key. Both issuers (the
// ACMEClient and the SelfSignedCA) return the same shape, so the caller (the
// portal reconcile) does not need to branch on which issuer produced it.
type Result struct {
	FullchainPEM string
	KeyPEM       string
	Fingerprint  string
	Leaf         *x509.Certificate
}

// GenerateAccountKey creates the ACME account key (ECDSA P-256, like autocert).
func GenerateAccountKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("acme generate account key: %w", err)
	}
	return key, nil
}

// MarshalECKeyPEM encodes an EC private key as PEM ("EC PRIVATE KEY"). Shared by
// the ACME certificate key and the internal CA's root/leaf keys, so every
// private key this package produces round-trips through the same format.
func MarshalECKeyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("acme marshal ec key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// ParseECKeyPEM reverses MarshalECKeyPEM.
func ParseECKeyPEM(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("acme: no PEM block in key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("acme parse ec key: %w", err)
	}
	return key, nil
}
