// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SelfSignedCA is the gateway's own internal CA: one self-signed root that signs a
// per-name leaf. It exists for deployments that cannot use Let's Encrypt (no
// public DNS, no port 80). Clients need the root ONCE as a trust anchor -- that is
// the whole point of using a CA instead of unrelated self-signed leaves.
type SelfSignedCA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA creates a fresh root valid for the given duration and returns it together
// with its PEM encodings (certPEM is public, keyPEM must be sealed by the caller).
func NewCA(commonName string, validity time.Duration) (*SelfSignedCA, string, string, error) {
	if validity <= 0 {
		return nil, "", "", errors.New("certissue: CA validity must be positive")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", fmt.Errorf("certissue: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, "", "", err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-5 * time.Minute), // small skew tolerance
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", "", fmt.Errorf("certissue: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, "", "", fmt.Errorf("certissue: parse CA certificate: %w", err)
	}
	keyPEM, err := MarshalECKeyPEM(key)
	if err != nil {
		return nil, "", "", err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return &SelfSignedCA{Cert: cert, Key: key}, certPEM, keyPEM, nil
}

// LoadCA reconstructs a CA from its stored PEMs.
func LoadCA(certPEM, keyPEM string) (*SelfSignedCA, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("certissue: no PEM block in CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certissue: parse CA certificate: %w", err)
	}
	key, err := ParseECKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	return &SelfSignedCA{Cert: cert, Key: key}, nil
}

// Fingerprint is the SHA-256 of the root's DER (hex) -- the value stored as a
// leaf's issuer_fingerprint so the reconcile can spot a leaf of an OLD CA.
func (ca *SelfSignedCA) Fingerprint() string {
	sum := sha256.Sum256(ca.Cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Issue signs a leaf for a single domain. Thin wrapper over IssueFor so every
// existing single-name caller keeps working unchanged.
func (ca *SelfSignedCA) Issue(domain string, validity time.Duration, now time.Time) (Result, error) {
	return ca.IssueFor([]string{domain}, validity, now)
}

// IssueFor signs ONE leaf covering every given name (DNS names and IP addresses).
// The requested validity is CLAMPED to the CA's own remaining lifetime -- a leaf
// must never outlive its issuer, or the chain breaks while the leaf still looks
// valid. An already-expired CA is refused outright.
func (ca *SelfSignedCA) IssueFor(names []string, validity time.Duration, now time.Time) (Result, error) {
	dns, ips, err := SplitNames(names)
	if err != nil {
		return Result{}, err
	}
	if !now.Before(ca.Cert.NotAfter) {
		return Result{}, fmt.Errorf("certissue: CA expired at %s", ca.Cert.NotAfter.Format(time.RFC3339))
	}
	if validity <= 0 {
		return Result{}, errors.New("certissue: leaf validity must be positive")
	}
	notBefore := now.Add(-5 * time.Minute)
	notAfter := now.Add(validity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Result{}, fmt.Errorf("certissue: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Result{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: primaryName(dns, ips)},
		DNSNames:     dns,
		IPAddresses:  ips,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return Result{}, fmt.Errorf("certissue: sign leaf: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Result{}, fmt.Errorf("certissue: parse leaf: %w", err)
	}
	keyPEM, err := MarshalECKeyPEM(key)
	if err != nil {
		return Result{}, err
	}
	var sb strings.Builder
	// Leaf first, then the root: a client that trusts only the root still sees a
	// complete chain (and servers that send the file verbatim stay correct).
	if err := pem.Encode(&sb, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return Result{}, err
	}
	if err := pem.Encode(&sb, &pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(leaf.Raw)
	return Result{
		FullchainPEM: sb.String(),
		KeyPEM:       keyPEM,
		Fingerprint:  hex.EncodeToString(sum[:]),
		Leaf:         leaf,
	}, nil
}

// FingerprintPEM is Fingerprint for a PEM-encoded certificate; garbage -> "".
func FingerprintPEM(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func randomSerial() (*big.Int, error) {
	// 128 random bits, the CA/Browser-Forum baseline for serial entropy.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certissue: serial: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil
}
