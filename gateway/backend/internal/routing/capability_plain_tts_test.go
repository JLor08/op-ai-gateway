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

// CapabilityRowsSpeech is "speech yes", clone or not: the producer eligibility for
// a model whose voices are worth listing (#301). An absent row is unknown, so an
// unstated speech verdict is not speech.
func TestCapabilityRowsSpeech(t *testing.T) {
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
		{"speech yes, clone yes", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityYes)}, true},
		{"speech no", []CapabilityRow{row(CapabilitySpeech, CapabilityNo)}, false},
		{"another capability only", []CapabilityRow{row(CapabilityTranscription, CapabilityYes)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapabilityRowsSpeech(tc.rows); got != tc.want {
				t.Fatalf("CapabilityRowsSpeech = %v, want %v", got, tc.want)
			}
		})
	}
}

// CapabilityRowsOfferableTTS is "speech yes and (not clone yes, or it has named
// voices to offer)": a plain tts model is always offerable, a clone one only once
// it has a non-empty voice list (#301).
func TestCapabilityRowsOfferableTTS(t *testing.T) {
	row := func(capability, verdict string) CapabilityRow {
		return CapabilityRow{Capability: capability, Verdict: verdict, Source: CapabilitySourceAudioCppModels}
	}
	cases := []struct {
		name              string
		rows              []CapabilityRow
		hasNonEmptyVoices bool
		want              bool
	}{
		{"plain tts, no voices", []CapabilityRow{row(CapabilitySpeech, CapabilityYes)}, false, true},
		{"clone, with voices", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityYes)}, true, true},
		{"clone, no voices", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityYes)}, false, false},
		{"clone, with voices, an unrelated row", []CapabilityRow{row(CapabilitySpeech, CapabilityYes), row(CapabilitySpeechClone, CapabilityYes), row(CapabilityTranscription, CapabilityYes)}, true, true},
		{"speech no, with voices", []CapabilityRow{row(CapabilitySpeech, CapabilityNo)}, true, false},
		{"no rows at all, with voices", nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapabilityRowsOfferableTTS(tc.rows, tc.hasNonEmptyVoices); got != tc.want {
				t.Fatalf("CapabilityRowsOfferableTTS = %v, want %v", got, tc.want)
			}
		})
	}
}
