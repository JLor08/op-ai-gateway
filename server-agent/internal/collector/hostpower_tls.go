// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"strings"
)

// FormatHostpowerFingerprint returns the SHA-256 of a DER certificate in the
// display form: uppercase hex with ':' between bytes (95 characters). The host
// prints it with this function and the guest parses it with
// ParseHostpowerFingerprint, so both sides share one format.
func FormatHostpowerFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return formatFingerprintSum(sum[:])
}

// formatFingerprintSum renders an already computed digest in the display form.
func formatFingerprintSum(sum []byte) string {
	h := strings.ToUpper(hex.EncodeToString(sum))
	var b strings.Builder
	b.Grow(len(h) + len(h)/2)
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// ParseHostpowerFingerprint decodes a SHA-256 certificate fingerprint: 64 hex
// digits, optional ':' separators, any case, surrounding whitespace trimmed.
// It returns the 32-byte digest.
func ParseHostpowerFingerprint(s string) ([]byte, error) {
	h := strings.ReplaceAll(strings.TrimSpace(s), ":", "")
	if len(h) != 2*sha256.Size {
		return nil, errors.New("fingerprint must be 64 hex digits (optionally ':'-separated)")
	}
	sum, err := hex.DecodeString(h)
	if err != nil {
		return nil, errors.New("fingerprint must be 64 hex digits (optionally ':'-separated)")
	}
	return sum, nil
}

// HostpowerPinError is a TLS handshake that failed the certificate pin. Want
// and Got are display-form fingerprints (public values); Got is empty when the
// server presented no certificate.
type HostpowerPinError struct{ Want, Got string }

func (e *HostpowerPinError) Error() string {
	if e.Got == "" {
		return "hostpower server presented no certificate; want fingerprint " + e.Want
	}
	return "hostpower server certificate fingerprint " + e.Got + " does not match " + e.Want
}

// hostpowerPinnedTLSConfig accepts exactly the server whose leaf certificate
// has the given SHA-256 digest.
func hostpowerPinnedTLSConfig(pin []byte) *tls.Config {
	want := append([]byte(nil), pin...)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// InsecureSkipVerify only disables the chain and host-name check that the
		// pin replaces: VerifyConnection below is the verification and runs in
		// the handshake, so a mismatch fails before any HTTP byte (and so the
		// bearer token) is sent. The hostpower certificate is self-signed.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &HostpowerPinError{Want: formatFingerprintSum(want)}
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return &HostpowerPinError{Want: formatFingerprintSum(want), Got: formatFingerprintSum(got[:])}
			}
			return nil
		},
	}
}
