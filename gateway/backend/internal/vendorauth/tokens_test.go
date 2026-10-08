// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/capture"
	"strings"
	"testing"
	"time"
)

// testHexKey is a throwaway 32-byte AES key (hex) for building a real cipher.
const testHexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func testCipher(t *testing.T) *capture.Cipher {
	t.Helper()
	c, err := capture.New(testHexKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	return c
}

func sampleTokenSet() TokenSet {
	return TokenSet{
		AccessToken:  "sk-ant-oat01-access-secret",
		RefreshToken: "sk-ant-ort01-refresh-secret",
		ExpiresAt:    time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		AccountID:    "acct-123",
		PlanType:     "plus",
		Scope:        "user:inference user:profile",
	}
}

func TestTokenSetSealOpenRoundTripWithCipher(t *testing.T) {
	c := testCipher(t)
	want := sampleTokenSet()
	sealed, err := SealTokenSet(c, false, want)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("sealed = %q, want enc: prefix", sealed)
	}
	for _, secret := range []string{want.AccessToken, want.RefreshToken} {
		if strings.Contains(sealed, secret) {
			t.Fatalf("sealed blob leaks %q", secret)
		}
	}
	got, err := OpenTokenSet(c, sealed)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestSealTokenSetKeylessDiskRefusesPlaintext(t *testing.T) {
	sealed, err := SealTokenSet(nil, false, sampleTokenSet())
	if !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want capture.ErrKeyRequired", err)
	}
	if sealed != "" {
		t.Fatalf("a keyless disk store must never return a blob, got %q", sealed)
	}
}

func TestTokenSetVolatilePlainPath(t *testing.T) {
	want := sampleTokenSet()
	sealed, err := SealTokenSet(nil, true, want)
	if err != nil {
		t.Fatalf("SealTokenSet(volatile): %v", err)
	}
	if !strings.HasPrefix(sealed, "plain:") {
		t.Fatalf("sealed = %q, want plain: prefix", sealed)
	}
	got, err := OpenTokenSet(nil, sealed)
	if err != nil {
		t.Fatalf("OpenTokenSet(plain): %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestOpenTokenSetEmptyIsZeroValue(t *testing.T) {
	got, err := OpenTokenSet(nil, "")
	if err != nil {
		t.Fatalf("OpenTokenSet(\"\"): %v", err)
	}
	if got != (TokenSet{}) {
		t.Fatalf("got %+v, want the zero TokenSet", got)
	}
}

func TestOpenTokenSetErrors(t *testing.T) {
	c := testCipher(t)
	cases := []struct {
		name    string
		cipher  *capture.Cipher
		stored  string
		wantKey bool
	}{
		{"enc without cipher", nil, "enc:AAAA", true},
		{"unprefixed value", c, "not-a-sealed-value", true},
		{"plain but not json", nil, "plain:{not json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OpenTokenSet(tc.cipher, tc.stored)
			if err == nil {
				t.Fatalf("OpenTokenSet = %+v, want an error", got)
			}
			if got != (TokenSet{}) {
				t.Fatalf("an error must return the zero TokenSet, got %+v", got)
			}
			if tc.wantKey && !errors.Is(err, capture.ErrKeyRequired) {
				t.Fatalf("err = %v, want capture.ErrKeyRequired", err)
			}
		})
	}
}

func TestTokenSetNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	buffer := 2 * time.Minute
	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero expiry means unknown, never refresh", time.Time{}, false},
		{"far in the future", now.Add(time.Hour), false},
		{"just outside the buffer", now.Add(buffer + time.Second), false},
		{"exactly at the buffer boundary", now.Add(buffer), false},
		{"just inside the buffer", now.Add(buffer - time.Second), true},
		{"already expired", now.Add(-time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := TokenSet{ExpiresAt: tc.expiresAt}
			if got := ts.NeedsRefresh(now, buffer); got != tc.want {
				t.Fatalf("NeedsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTokenSetNeverFormatsSecrets guards the log/print path: a stray %v / %+v /
// %#v of a TokenSet must not spill the bearer or refresh token.
func TestTokenSetNeverFormatsSecrets(t *testing.T) {
	ts := sampleTokenSet()
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, ts)
		for _, secret := range []string{ts.AccessToken, ts.RefreshToken} {
			if strings.Contains(out, secret) {
				t.Fatalf("fmt %s leaks %q: %s", format, secret, out)
			}
		}
	}
}

// TestTokenSetLogValueNeverEmitsSecrets guards structured logging: slog resolves
// a LogValuer before handing the value to a handler, so neither the JSON nor the
// text handler may see (or marshal) the real token fields.
func TestTokenSetLogValueNeverEmitsSecrets(t *testing.T) {
	ts := sampleTokenSet()
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	}
	for name, newHandler := range handlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(newHandler(&buf))
			logger.Info("connected", "tokens", ts, slog.Any("again", ts))
			out := buf.String()
			for _, secret := range []string{ts.AccessToken, ts.RefreshToken} {
				if strings.Contains(out, secret) {
					t.Fatalf("slog %s output leaks %q: %s", name, secret, out)
				}
			}
			if !strings.Contains(out, "redacted") || !strings.Contains(out, ts.AccountID) {
				t.Fatalf("slog %s output should carry the redacted summary, got: %s", name, out)
			}
		})
	}
	if got := ts.LogValue().String(); got != ts.String() {
		t.Fatalf("LogValue = %q, want the same redacted text as String() = %q", got, ts.String())
	}
}
