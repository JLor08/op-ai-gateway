// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// The relay's keepalive silence (#339) is, to the machine, one more audio_delta: relaying it numbers
// it into the playout counter, adds its real-time duration to the drain floor, and clears a gap
// playback_done remembered earlier -- exactly what a real chunk does. The counter and the floor stay
// consistent with what the client plays and counts (it counts every audio_delta), and the clear is the
// guard against an old client's stale gap signal ending the drain while more audio is still to play.
// This pins the clear directly, which the WS-level gap test no longer does on its own: over its long
// gap the silence's own floor extension keeps the mic gated whether or not the signal is cleared.
func TestVoiceRelayAudioTreatsSilenceLikeAnyChunk(t *testing.T) {
	const rate = 24000
	silence := base64.StdEncoding.EncodeToString(voiceSilencePCM(rate, voiceSilenceFrameMs))
	// A real chunk of the same 200 ms, so the two differ only in what the machine cannot tell: the bytes.
	speech := base64.StdEncoding.EncodeToString(speechPCM(rate*speechStreamBytesPerFrame*voiceSilenceFrameMs/1000, 1))

	for _, tc := range []struct {
		name string
		data string
	}{
		{"real chunk", speech},
		{"silence frame", silence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &voiceMachine{ctx: context.Background(), writer: newVoiceWriter(&fakeVoiceSocket{}, func() {})}
			m.onPlaybackDone(nil) // an older client's gap signal, remembered while speaking
			if !m.playbackSignaled {
				t.Fatal("precondition: the gap signal was not remembered")
			}

			m.relayAudio(runAudio{Seq: 0, Rate: rate, Data: tc.data})

			if m.playbackSignaled {
				t.Fatal("relaying audio left the remembered gap signal set: it would end the drain while this audio is still to play")
			}
			if m.relayedAudioChunks != 1 {
				t.Fatalf("relayedAudioChunks = %d, want 1: the client counts every audio_delta it receives", m.relayedAudioChunks)
			}
			if want := time.Duration(voiceSilenceFrameMs) * time.Millisecond; m.relayedPlaybackDur != want {
				t.Fatalf("relayedPlaybackDur = %v, want %v: the frame's real-time duration extends the drain floor", m.relayedPlaybackDur, want)
			}
			if m.firstAudioAt.IsZero() {
				t.Fatal("firstAudioAt was not stamped by the first relayed audio")
			}
		})
	}
}
