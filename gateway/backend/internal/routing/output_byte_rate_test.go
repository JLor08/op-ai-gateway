// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// ParseOutputByteRate is the one definition of "a usable cached output byte rate": the
// streamed relay bills with it (an unusable payload bills audio_bytes) and the health
// loop's speech-rate gate asks it whether a rate is cached (an unusable payload is no
// rate, so the benchmark runs again). A payload both sides read differently would bill
// at a rate the gate believes absent, or leave a mapping on audio_bytes for good.
func TestParseOutputByteRate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    uint32
		wantOK  bool
	}{
		{"a decimal integer", "88200", 88200, true},
		{"surrounding whitespace is tolerated", " 48000\n", 48000, true},
		{"the largest uint32", "4294967295", 4294967295, true},
		{"empty is the invalidated state, not a rate", "", 0, false},
		{"zero is no rate: the caller divides by it", "0", 0, false},
		{"negative", "-88200", 0, false},
		{"a fraction", "88200.5", 0, false},
		{"a unit suffix", "88200B/s", 0, false},
		{"overflows uint32", "4294967296", 0, false},
		{"not a number", "fast", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseOutputByteRate(tc.payload)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ParseOutputByteRate(%q) = (%d, %v), want (%d, %v)", tc.payload, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The companion cache type is a fixed string: it is a persisted key, so renaming it
// would orphan every identity row already written. It differs from the rate's own
// type, whose payload (a plain decimal) billing parses and must never share a row with
// anything else.
func TestModelMappingCacheOutputByteRateModelType(t *testing.T) {
	if ModelMappingCacheOutputByteRateModel != "output_byte_rate_model" {
		t.Fatalf("ModelMappingCacheOutputByteRateModel = %q, want %q", ModelMappingCacheOutputByteRateModel, "output_byte_rate_model")
	}
	if ModelMappingCacheOutputByteRateModel == ModelMappingCacheOutputByteRate {
		t.Fatal("the model identity must be its own cache type, apart from the rate billing parses")
	}
}
