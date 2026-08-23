// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package totp implements RFC 6238 time-based one-time passwords using only
// the Go standard library (crypto/hmac + crypto/sha1 + encoding/base32) for
// the algorithm itself, plus github.com/skip2/go-qrcode (MIT) to render the
// enrollment QR code. It generates 20-byte base32 secrets, derives 6-digit
// codes on a 30s step with SHA-1 HMAC, and verifies a submitted code against
// a ±1 step skew window using a constant-time comparison so timing does not
// leak which step (if any) matched.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	secretBytes = 20
	period      = 30 * time.Second
	digits      = 6
	skewSteps   = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a fresh 20-byte random secret, base32-encoded
// without padding.
func GenerateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("totp: generate secret: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

// Code computes the RFC 6238 6-digit TOTP code for the given base32 secret at
// the given time, using a 30-second step and SHA-1 HMAC.
func Code(secret string, at time.Time) (string, error) {
	key, err := b32.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("totp: decode secret: %w", err)
	}
	counter := uint64(at.Unix()) / uint64(period.Seconds())
	return hotp(key, counter), nil
}

// Verify reports whether code is a valid TOTP for secret at the given time,
// allowing ±1 step (30s) of clock skew. It never panics and never surfaces an
// error: a malformed secret or code simply yields false. The comparison
// across all candidate steps is constant-time so timing does not reveal
// which step (if any) matched.
func Verify(secret, code string, at time.Time) bool {
	if secret == "" {
		return false
	}
	if len(code) != digits {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	key, err := b32.DecodeString(secret)
	if err != nil {
		return false
	}
	base := uint64(at.Unix()) / uint64(period.Seconds())

	matched := 0
	for delta := -skewSteps; delta <= skewSteps; delta++ {
		counter := uint64(int64(base) + int64(delta))
		candidate := hotp(key, counter)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			matched |= 1
		}
	}
	return matched == 1
}

// OtpauthURI builds an otpauth://totp/ provisioning URI for the given
// issuer, account, and base32 secret, suitable for encoding into a QR code
// and scanning with an authenticator app. The label is "issuer:account"; the
// query carries secret, issuer, algorithm=SHA1, digits=6, and period=30 to
// match this package's fixed TOTP parameters.
func OtpauthURI(issuer, account, secret string) string {
	label := issuer + ":" + account
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(digits))
	q.Set("period", strconv.Itoa(int(period.Seconds())))

	u := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + label,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// QRCodePNG renders uri (typically an OtpauthURI) as a 256px PNG QR code with
// medium error correction, using github.com/skip2/go-qrcode (MIT).
func QRCodePNG(uri string) ([]byte, error) {
	png, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("totp: encode qr: %w", err)
	}
	return png, nil
}

// hotp implements RFC 4226 HOTP: HMAC-SHA1(key, counter), dynamic
// truncation, mod 10^6, zero-padded to 6 digits.
func hotp(key []byte, counter uint64) string {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(counterBytes[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, truncated%mod)
}
