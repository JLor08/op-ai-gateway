// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"testing"
	"time"
)

// The silence a voice turn plays over a gap in its speech (#339) is plain zeroed PCM at the run's
// own output rate: the player is fed the same format it is already playing, so nothing about it
// needs a new wire shape. The helpers below are what the relay builds that silence from.

// A silence frame is mono signed 16-bit little-endian PCM (speechStreamBytesPerFrame bytes a
// sample), all zero: 200 ms at 24 kHz is 4800 samples, 9600 bytes.
func TestVoiceSilencePCMSizeAndZeroed(t *testing.T) {
	pcm := voiceSilencePCM(24000, 200)

	if want := 24000 * 200 / 1000 * 2; len(pcm) != want {
		t.Fatalf("len = %d, want %d (200 ms of 24 kHz mono 16-bit)", len(pcm), want)
	}
	for i, b := range pcm {
		if b != 0 {
			t.Fatalf("byte %d = %#x, want 0: silence is zeroed PCM", i, b)
		}
	}
}

// Without a usable rate or duration there is no silence to make: nil, never an empty slice the
// caller would have to tell apart from "none".
func TestVoiceSilencePCMNonPositive(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sampleRate, ms int
	}{
		{"zero rate", 0, 200},
		{"zero duration", 24000, 0},
		{"negative rate", -1, 200},
		{"negative duration", 24000, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := voiceSilencePCM(tc.sampleRate, tc.ms); got != nil {
				t.Fatalf("voiceSilencePCM(%d, %d) = %d bytes, want nil", tc.sampleRate, tc.ms, len(got))
			}
		})
	}
}

// The silence frame is longer than the cadence it is sent at, so each frame is still playing when
// the next arrives and the player never runs dry; both stay well inside the client's playing-gap
// tolerance (PLAYING_GAP_MS, 1200 ms) so a gap is never read as the end of the turn.
func TestVoiceSilenceFrameOutlastsItsCadence(t *testing.T) {
	const clientPlayingGap = 1200 * time.Millisecond
	frame := time.Duration(voiceSilenceFrameMs) * time.Millisecond

	if frame <= voiceSilenceCadence {
		t.Fatalf("frame %v <= cadence %v: the player would run dry between frames", frame, voiceSilenceCadence)
	}
	if frame >= clientPlayingGap || voiceSilenceCadence >= clientPlayingGap {
		t.Fatalf("frame %v / cadence %v must stay under the client's %v playing gap", frame, voiceSilenceCadence, clientPlayingGap)
	}
}

// The run's fallback silence is played at the rate its speech model streams at: the cached output
// byte rate of the model's mapping, halved to a sample rate (88200 B/s is 44.1 kHz). A model that
// cannot be asked -- unknown, not yet benchmarked, no rate cached -- is 0 (no silence is better than
// silence at a guessed rate), and so is a store that fails: the keepalive is a nicety and must never
// fail the turn. The positive conversion itself is portalSpeechStreamRate's (and its own tests'); the
// case here is that this resolver returns it unchanged.
func TestVoiceRunOutputRateFromCache(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, owner, _, _ := newSpeechRunTestServer(t, up.URL)
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
	addSpeechMapping(t, srv, "route-tts-unbenchmarked", "tts-unbenchmarked", nil, "")
	ctx := context.Background()

	if got := srv.voiceRunOutputRate(ctx, owner, speechGatewayModel); got != speechRunTestRate {
		t.Fatalf("benchmarked model: rate = %d Hz, want %d (byte rate %s / 2)", got, speechRunTestRate, speechRunTestByteRate)
	}
	if got := srv.voiceRunOutputRate(ctx, owner, "tts-unbenchmarked"); got != 0 {
		t.Fatalf("model with no cached rate: rate = %d Hz, want 0", got)
	}
	if got := srv.voiceRunOutputRate(ctx, owner, "no-such-model"); got != 0 {
		t.Fatalf("unknown model: rate = %d Hz, want 0", got)
	}

	srv.Routes = capabilityBatchFailingStore{Store: srv.Routes}
	// The fixture really does make the gate ERROR (not merely close), so the 0 below is the swallowed
	// error and not a second "no rate" case.
	if _, _, err := srv.portalSpeechStreamRate(ctx, owner, speechGatewayModel); err == nil {
		t.Fatal("the failing store did not make portalSpeechStreamRate fail; the fail-open case below proves nothing")
	}
	if got := srv.voiceRunOutputRate(ctx, owner, speechGatewayModel); got != 0 {
		t.Fatalf("failing store: rate = %d Hz, want 0 (fail open, error swallowed)", got)
	}
}
