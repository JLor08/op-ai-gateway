// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The audio_delta frame carries the `section` index for live attribution: it is additive
// (omitted for section 0, the common case, so an older client is never regressed) and present
// for any later section. seq stays on the wire regardless.
func TestVoiceDownMsg_AudioDeltaCarriesSection(t *testing.T) {
	for _, tc := range []struct {
		msg            voiceDownMsg
		wantHasSection bool
	}{
		{voiceDownMsg{Type: voiceDownAudioDelta, Seq: 0, Section: 0, Rate: 24000, Audio: "AAA="}, false},
		{voiceDownMsg{Type: voiceDownAudioDelta, Seq: 2, Section: 1, Rate: 24000, Audio: "AQI="}, true},
	} {
		b := voiceMarshal(tc.msg)
		if got := strings.Contains(string(b), `"section"`); got != tc.wantHasSection {
			t.Fatalf("marshal %s: section present = %v, want %v", b, got, tc.wantHasSection)
		}
		if tc.wantHasSection && !strings.Contains(string(b), `"section":1`) {
			t.Fatalf("marshal %s: want section:1", b)
		}
		if !strings.Contains(string(b), `"seq":`) {
			t.Fatalf("marshal %s: seq must stay on the wire for an audio_delta", b)
		}
	}
}

// relayAudio sends the chunk's seq and section straight through to the wire (the executor has
// already assigned one monotonic seq across the turn's legs; the machine only attributes and
// counts), and accounts the chunk toward the playout counter.
func TestVoiceRelayAudioCarriesSeqAndSection(t *testing.T) {
	m := &voiceMachine{ctx: context.Background(), writer: newVoiceWriter(&fakeVoiceSocket{}, func() {})}
	m.relayAudio(runAudio{Seq: 5, Section: 1, Rate: 24000, Data: "AAA="})

	var got voiceDownMsg
	select {
	case data := <-m.writer.out:
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("relayed frame is not JSON: %v (%s)", err, data)
		}
	default:
		t.Fatal("relayAudio enqueued no frame")
	}
	if got.Type != voiceDownAudioDelta || got.Seq != 5 || got.Section != 1 {
		t.Fatalf("relayed %+v, want audio_delta seq 5 section 1", got)
	}
	if m.relayedAudioChunks != 1 {
		t.Fatalf("relayedAudioChunks = %d, want 1", m.relayedAudioChunks)
	}
}
