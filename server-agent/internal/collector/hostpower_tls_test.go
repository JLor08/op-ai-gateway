// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// colonHex is an independent reference formatter: uppercase hex, ':' between bytes.
func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	parts := make([]string, 0, len(b))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func TestFormatHostpowerFingerprint(t *testing.T) {
	der := []byte{0x30, 0x82, 0x01, 0x0a, 0x02, 0x82, 0x01, 0x01, 0x00, 0xc3}
	sum := sha256.Sum256(der)
	got := FormatHostpowerFingerprint(der)
	if want := colonHex(sum[:]); got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
	if len(got) != 95 {
		t.Fatalf("len = %d, want 95", len(got))
	}
	// Published SHA-256 test vector of the empty input.
	const empty = "E3:B0:C4:42:98:FC:1C:14:9A:FB:F4:C8:99:6F:B9:24:27:AE:41:E4:64:9B:93:4C:A4:95:99:1B:78:52:B8:55"
	if got := FormatHostpowerFingerprint(nil); got != empty {
		t.Fatalf("Format(nil) = %q, want %q", got, empty)
	}
}

func TestParseHostpowerFingerprint(t *testing.T) {
	sum := sha256.Sum256([]byte("hostpower test certificate"))
	colonUpper := colonHex(sum[:])
	plainLower := hex.EncodeToString(sum[:])
	mixed := []rune(colonUpper)
	for i := range mixed {
		if i%3 == 0 {
			mixed[i] = []rune(strings.ToLower(string(mixed[i])))[0]
		}
	}
	for name, in := range map[string]string{
		"colon upper":          colonUpper,
		"plain lower":          plainLower,
		"mixed case, trimmed":  " \t" + string(mixed) + " \n",
		"plain upper, trimmed": "  " + strings.ToUpper(plainLower) + "  ",
	} {
		got, err := ParseHostpowerFingerprint(in)
		if err != nil {
			t.Errorf("%s: Parse(%q) error: %v", name, in, err)
			continue
		}
		if !bytes.Equal(got, sum[:]) {
			t.Errorf("%s: Parse(%q) = %x, want %x", name, in, got, sum[:])
		}
	}
	for name, in := range map[string]string{
		"63 hex digits":  plainLower[:63],
		"65 hex digits":  plainLower + "a",
		"non-hex":        strings.Repeat("zz", 32),
		"empty":          "",
		"blank":          "   ",
		"sha256: prefix": "sha256:" + plainLower,
		"62 + colons":    colonUpper[:len(colonUpper)-3],
	} {
		if got, err := ParseHostpowerFingerprint(in); err == nil {
			t.Errorf("%s: Parse(%q) = %x, want an error", name, in, got)
		}
	}
	der := []byte("round trip")
	rt := sha256.Sum256(der)
	got, err := ParseHostpowerFingerprint(FormatHostpowerFingerprint(der))
	if err != nil || !bytes.Equal(got, rt[:]) {
		t.Fatalf("Parse(Format(der)) = %x, %v; want %x", got, err, rt[:])
	}
}

func TestHostpowerPinErrorMessage(t *testing.T) {
	mismatch := (&HostpowerPinError{Want: "AA:BB", Got: "CC:DD"}).Error()
	for _, want := range []string{"AA:BB", "CC:DD"} {
		if !strings.Contains(mismatch, want) {
			t.Errorf("mismatch message %q lacks %q", mismatch, want)
		}
	}
	none := (&HostpowerPinError{Want: "AA:BB"}).Error()
	if !strings.Contains(none, "AA:BB") || !strings.Contains(none, "no certificate") {
		t.Errorf("no-certificate message = %q", none)
	}
}
