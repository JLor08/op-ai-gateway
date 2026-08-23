// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package totp

import (
	"bytes"
	"encoding/base32"
	"image"
	_ "image/png"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCodeRFC6238SHA1Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		got, err := Code(secret, time.Unix(tc.unix, 0).UTC())
		if err != nil {
			t.Fatalf("Code(%d): %v", tc.unix, err)
		}
		if got != tc.want {
			t.Fatalf("Code(%d) = %q, want %q", tc.unix, got, tc.want)
		}
	}
}

func TestVerifySkewAccept(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	base := time.Unix(1700000000, 0).UTC()
	c, err := Code(secret, base)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	if !Verify(secret, c, base.Add(-30*time.Second)) {
		t.Fatalf("Verify: expected accept one step early")
	}
	if !Verify(secret, c, base.Add(30*time.Second)) {
		t.Fatalf("Verify: expected accept one step late")
	}
}

func TestVerifySkewReject(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	base := time.Unix(1700000000, 0).UTC()
	c, err := Code(secret, base)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	if Verify(secret, c, base.Add(60*time.Second)) {
		t.Fatalf("Verify: expected reject two steps late")
	}
	if Verify(secret, c, base.Add(-60*time.Second)) {
		t.Fatalf("Verify: expected reject two steps early")
	}
}

func TestVerifyWrongCode(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	base := time.Unix(1700000000, 0).UTC()
	c, err := Code(secret, base)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	wrong := "000000"
	if wrong == c {
		wrong = "111111"
	}
	if Verify(secret, wrong, base) {
		t.Fatalf("Verify: expected reject for wrong code %q (correct %q)", wrong, c)
	}
}

func TestCodeMalformedSecret(t *testing.T) {
	if _, err := Code("not-valid-base32!!", time.Now()); err == nil {
		t.Fatalf("Code: expected error for malformed base32 secret")
	}
}

func TestVerifyMalformedInputs(t *testing.T) {
	now := time.Now()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	c, err := Code(secret, now)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}

	if Verify("not-valid-base32!!", c, now) {
		t.Fatalf("Verify: expected false for malformed base32 secret, not panic")
	}
	if Verify(secret, "12ab56", now) {
		t.Fatalf("Verify: expected false for non-digit code")
	}
	if Verify(secret, "12345", now) {
		t.Fatalf("Verify: expected false for wrong-length code")
	}
	if Verify("", "123456", now) {
		t.Fatalf("Verify: expected false for empty secret")
	}
}

// TestVerifyEmptySecretRejected proves the fix for a CONFIRMED MAJOR bug: an
// empty secret used to base32-decode to a zero-length HMAC key, and Verify
// would accept a code computed from that zero-length key. Verify must reject
// an empty secret outright, before any decode, regardless of the code.
func TestVerifyEmptySecretRejected(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// The exact code an empty secret's own (zero-length-key) HMAC would
	// produce -- the strongest possible input, since a naive "code != valid"
	// check would still let this one through.
	code, err := Code("", now)
	if err != nil {
		t.Fatalf("Code(\"\"): %v", err)
	}
	if Verify("", code, now) {
		t.Fatalf("Verify(\"\", %q, now) = true, want false: empty secret must never verify", code)
	}
}

func TestOtpauthURIShape(t *testing.T) {
	uri := OtpauthURI("OP AI Gateway", "alice@example.com", "JBSWY3DPEHPK3PXP")
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("scheme/host = %q/%q, want otpauth/totp", u.Scheme, u.Host)
	}
	if u.Path != "/OP AI Gateway:alice@example.com" {
		t.Fatalf("label path = %q", u.Path)
	}
	// literal encoded form: space -> %20, colon kept as label separator
	if !strings.HasPrefix(uri, "otpauth://totp/OP%20AI%20Gateway:alice@example.com?") {
		t.Fatalf("encoded label prefix wrong: %q", uri)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"secret": "JBSWY3DPEHPK3PXP", "issuer": "OP AI Gateway",
		"algorithm": "SHA1", "digits": "6", "period": "30",
	} {
		if got := q.Get(k); got != want {
			t.Fatalf("query %s = %q, want %q", k, got, want)
		}
	}
}

func TestQRCodePNGDecodes(t *testing.T) {
	uri := OtpauthURI("OP AI Gateway", "alice@example.com", "JBSWY3DPEHPK3PXP")
	png, err := QRCodePNG(uri)
	if err != nil {
		t.Fatalf("QRCodePNG: %v", err)
	}
	if len(png) == 0 {
		t.Fatal("QRCodePNG returned empty bytes")
	}
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
		t.Fatalf("decoded image has zero dimensions: %v", b)
	}
}

func TestGenerateSecretShape(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s1)
	if err != nil {
		t.Fatalf("GenerateSecret: not valid no-padding base32: %v", err)
	}
	if len(raw) != 20 {
		t.Fatalf("GenerateSecret: decoded length = %d, want 20", len(raw))
	}

	s2, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if s1 == s2 {
		t.Fatalf("GenerateSecret: two calls returned identical secrets\ngot: %q\nwant: %q (different)", s2, s1)
	}
}
