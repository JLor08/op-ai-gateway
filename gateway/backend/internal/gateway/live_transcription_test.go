// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

func TestLiveBytesPerSample(t *testing.T) {
	cases := []struct {
		format   string
		wantSize int
		wantOK   bool
	}{
		{"s16le", 2, true},
		{"f32le", 4, true},
		{"s24le", 0, false},
		{"S16LE", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			size, ok := bytesPerSample(tc.format)
			if size != tc.wantSize || ok != tc.wantOK {
				t.Fatalf("bytesPerSample(%q) = (%d, %v), want (%d, %v)",
					tc.format, size, ok, tc.wantSize, tc.wantOK)
			}
		})
	}
}

func TestLiveByteRate(t *testing.T) {
	cases := []struct {
		name       string
		sampleRate int
		channels   int
		format     string
		wantRate   int
		wantOK     bool
	}{
		{"known good s16le mono", 16000, 1, "s16le", 32000, true},
		{"f32le stereo", 48000, 2, "f32le", 384000, true},
		{"unknown format", 16000, 1, "s24le", 0, false},
		{"sample rate 999 rejected", 999, 1, "s16le", 0, false},
		{"sample rate 1000 accepted", 1000, 1, "s16le", 2000, true},
		{"sample rate 384000 accepted", 384000, 1, "s16le", 768000, true},
		{"sample rate 384001 rejected", 384001, 1, "s16le", 0, false},
		{"channels 0 rejected", 16000, 0, "s16le", 0, false},
		{"channels 1 accepted", 16000, 1, "s16le", 32000, true},
		{"channels 16 accepted", 16000, 16, "s16le", 512000, true},
		{"channels 17 rejected", 16000, 17, "s16le", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, ok := liveByteRate(tc.sampleRate, tc.channels, tc.format)
			if rate != tc.wantRate || ok != tc.wantOK {
				t.Fatalf("liveByteRate(%d, %d, %q) = (%d, %v), want (%d, %v)",
					tc.sampleRate, tc.channels, tc.format, rate, ok, tc.wantRate, tc.wantOK)
			}
		})
	}
}
