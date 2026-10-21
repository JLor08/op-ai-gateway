// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// CapabilityRowsPlainTTS is "speech yes and not clone yes", with an absent row
// read as unknown: unknown speech is not tts, unknown or negative clone does not
// exclude.
func TestCapabilityRowsPlainTTS(t *testing.T) {
	row := func(capability, verdict string) CapabilityRow {
		return CapabilityRow{Capability: capability, Verdict: verdict, Source: CapabilitySourceAudioCppModels}
	}
	cases := []struct {
		name string
		rows []CapabilityRow
		want bool
	}{
		{"no rows at all", nil, false},
		{"speech yes", []CapabilityRow{row(CapabilitySpeech, CapabilityYes)}, true},
		{"speech yes, clone no", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityNo)}, true},
		{"speech yes, clone yes", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityYes)}, false},
		{"speech no", []CapabilityRow{row(CapabilitySpeech, CapabilityNo)}, false},
		{"speech unknown, clone no", []CapabilityRow{row(CapabilitySpeechClone, CapabilityNo)}, false},
		{"another capability only", []CapabilityRow{row(CapabilityTranscription, CapabilityYes)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapabilityRowsPlainTTS(tc.rows); got != tc.want {
				t.Fatalf("CapabilityRowsPlainTTS = %v, want %v", got, tc.want)
			}
		})
	}
}
