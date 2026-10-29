// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
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

// --- the relay's gap fill ----------------------------------------------------------------------
//
// relayVoiceSections is driven here directly, with hand-built legs: a test plays the part of each
// leg's speech goroutine (sending real chunks and closing audioCh when it likes), and a recording
// subscriber on a plain run sees exactly what the relay published. A silence frame is an audio frame
// whose bytes are all zero; the "real" chunks the tests send are never all zero.

const (
	relayTestRate     = 24000
	relayTestFallback = 16000
	// relayTestGap is a quiet stretch long enough for the relay's cadence timer to fire at least
	// once (twice, usually) before the test ends it.
	relayTestGap = 3 * voiceSilenceCadence
	// relayTestWait bounds how long a test waits for the relay to return: a relay that does not is
	// the deadlock the full-drain invariant exists to prevent.
	relayTestWait = 10 * time.Second
)

// relayRecorder is a run with one subscriber that records every audio frame published to it.
type relayRecorder struct {
	run       *ChatRun
	returned  chan struct{}
	collected chan struct{}
	frames    []runAudio
}

func newRelayRecorder() *relayRecorder {
	run := newChatRun("run-relay", "chat-relay", "user-relay", func() { /* the relay never cancels the run */ })
	_, ch, _ := run.subscribe()
	rec := &relayRecorder{run: run, returned: make(chan struct{}), collected: make(chan struct{})}
	go func() {
		defer close(rec.collected)
		for ev := range ch {
			if ev.Event == "audio" && ev.Audio != nil {
				rec.frames = append(rec.frames, *ev.Audio)
			}
		}
	}()
	return rec
}

// start runs the relay in its own goroutine, as executeVoiceRun does.
func (r *relayRecorder) start(legsCh <-chan *voiceSpeechLeg, fallbackRate int, silenceEnabled bool) {
	go func() {
		defer close(r.returned)
		relayVoiceSections(r.run, legsCh, fallbackRate, silenceEnabled)
	}()
}

// wait blocks until the relay has returned (failing the test if it never does) and hands back every
// frame it published, in order.
func (r *relayRecorder) wait(t *testing.T) []runAudio {
	t.Helper()
	select {
	case <-r.returned:
	case <-time.After(relayTestWait):
		t.Fatal("relayVoiceSections did not return after every leg and the leg feed closed: a leg goroutine would be left blocked and joinVoiceLegs would deadlock")
	}
	r.run.finish("completed", "", nil)
	<-r.collected
	return r.frames
}

// relayLegs builds n section legs queued on a leg feed that is closed once they are queued.
func relayLegs(n int) ([]*voiceSpeechLeg, chan *voiceSpeechLeg) {
	legs := make([]*voiceSpeechLeg, n)
	legsCh := make(chan *voiceSpeechLeg, n)
	for i := range legs {
		legs[i] = newVoiceSpeechLeg(i, "", true, 0, 0, nil)
		legsCh <- legs[i]
	}
	close(legsCh)
	return legs, legsCh
}

// realChunk is one chunk of real (non-zero) audio at rate, as a leg's sink hands it to the relay.
func realChunk(rate int) runAudio {
	return runAudio{Rate: rate, Data: base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})}
}

// isSilenceFrame reports whether a is a frame of zeroed PCM.
func isSilenceFrame(t *testing.T, a runAudio) bool {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		t.Fatalf("frame %d: data is not standard base64: %v", a.Seq, err)
	}
	if len(raw) == 0 {
		return false
	}
	for _, b := range raw {
		if b != 0 {
			return false
		}
	}
	return true
}

// requireSeqUnbroken fails unless frames carry seq 0, 1, 2, ...: silence is numbered like any
// audio_delta, so the client sees one unbroken stream.
func requireSeqUnbroken(t *testing.T, frames []runAudio) {
	t.Helper()
	for i, f := range frames {
		if f.Seq != i {
			t.Fatalf("frame %d has seq %d, want %d (one monotonic seq over real audio and silence)", i, f.Seq, i)
		}
	}
}

// requireOnlyRealAudio fails if any of frames is silence, and returns the frames for chaining.
func requireOnlyRealAudio(t *testing.T, frames []runAudio) {
	t.Helper()
	for _, f := range frames {
		if isSilenceFrame(t, f) {
			t.Fatalf("seq %d is a silence frame, want none: %+v", f.Seq, frames)
		}
	}
}

// requireSilenceBetweenRealChunks fails unless frames are one real chunk, at least one silence frame
// and one more real chunk, in that order, every silence frame being voiceSilenceFrameMs of zeroed
// PCM at silenceRate.
func requireSilenceBetweenRealChunks(t *testing.T, frames []runAudio, silenceRate int) {
	t.Helper()
	requireSeqUnbroken(t, frames)
	if len(frames) < 3 {
		t.Fatalf("got %d frames, want a real chunk, silence over the gap, then a real chunk", len(frames))
	}
	if isSilenceFrame(t, frames[0]) || isSilenceFrame(t, frames[len(frames)-1]) {
		t.Fatalf("the stream must start and end with real audio, silence only between: %+v", frames)
	}
	requireSilenceFrames(t, frames[1:len(frames)-1], silenceRate)
}

// requireSilenceFrames fails unless every one of frames is voiceSilenceFrameMs of zeroed PCM at
// silenceRate.
func requireSilenceFrames(t *testing.T, frames []runAudio, silenceRate int) {
	t.Helper()
	wantBytes := silenceRate * voiceSilenceFrameMs / 1000 * speechStreamBytesPerFrame
	for _, f := range frames {
		if !isSilenceFrame(t, f) {
			t.Fatalf("seq %d is not silence: %+v", f.Seq, f)
		}
		if f.Rate != silenceRate {
			t.Fatalf("silence seq %d at %d Hz, want %d", f.Seq, f.Rate, silenceRate)
		}
		if raw, _ := base64.StdEncoding.DecodeString(f.Data); len(raw) != wantBytes {
			t.Fatalf("silence seq %d is %d bytes, want %d (%d ms at %d Hz)", f.Seq, len(raw), wantBytes, voiceSilenceFrameMs, silenceRate)
		}
	}
}

// requireSectionsAcrossGap fails unless the silence between the first and last of frames is tagged
// with the section of the audio that came BEFORE the gap (gapSection) and the last frame, the real
// chunk after it, with its own (nextSection). The client attributes what it hears by the section
// tag, so silence must not claim the next section before any of its audio exists -- that leg may yet
// fail, and its block would then have been given to silence.
func requireSectionsAcrossGap(t *testing.T, frames []runAudio, gapSection, nextSection int) {
	t.Helper()
	for _, f := range frames[1 : len(frames)-1] {
		if f.Section != gapSection {
			t.Fatalf("silence seq %d is tagged section %d, want %d (the section last heard)", f.Seq, f.Section, gapSection)
		}
	}
	if last := frames[len(frames)-1]; last.Section != nextSection {
		t.Fatalf("the real chunk after the gap is tagged section %d, want %d", last.Section, nextSection)
	}
}

// requireSilenceThenRealChunk fails unless frames are at least one silence frame and then one real
// chunk at realRate, in that order -- a gap BEFORE the stream's first real chunk -- every silence
// frame being voiceSilenceFrameMs of zeroed PCM at silenceRate.
func requireSilenceThenRealChunk(t *testing.T, frames []runAudio, silenceRate, realRate int) {
	t.Helper()
	requireSeqUnbroken(t, frames)
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want silence over the gap and then the real chunk", len(frames))
	}
	last := frames[len(frames)-1]
	if isSilenceFrame(t, last) || last.Rate != realRate {
		t.Fatalf("the stream must end with the real chunk at %d Hz: %+v", realRate, last)
	}
	requireSilenceFrames(t, frames[:len(frames)-1], silenceRate)
}

// A leg that is still waiting for its first chunk -- queued behind the TTS cap, synthesizing, or
// backing off to retry -- is a gap in the stream: the relay fills it with silence at the run's
// output rate (the fallback, nothing real has played yet), and the leg's real audio then follows in
// the same unbroken seq.
func TestRelaySilenceDuringInnerGap(t *testing.T) {
	rec := newRelayRecorder()
	legs, legsCh := relayLegs(1)
	rec.start(legsCh, relayTestFallback, true)

	time.Sleep(relayTestGap)
	legs[0].audioCh <- realChunk(relayTestRate)
	close(legs[0].audioCh)

	frames := rec.wait(t)
	// The gap's silence is at the FALLBACK rate (nothing real has played to say otherwise); the
	// real chunk keeps its own.
	requireSilenceThenRealChunk(t, frames, relayTestFallback, relayTestRate)
}

// Between two sections the gap is filled at the rate the stream is ALREADY playing at (the last real
// chunk's), not the fallback: the player has one format for the turn, so that is the one it can
// take. It holds whether the next section's leg is already queued behind a slow synthesis, or has
// not even been launched yet (the LLM is still writing it).
func TestRelaySilenceBetweenSections(t *testing.T) {
	t.Run("next leg queued, its first chunk late", func(t *testing.T) {
		rec := newRelayRecorder()
		legs, legsCh := relayLegs(2)
		rec.start(legsCh, relayTestFallback, true)

		legs[0].audioCh <- realChunk(relayTestRate)
		close(legs[0].audioCh)
		time.Sleep(relayTestGap)
		legs[1].audioCh <- realChunk(relayTestRate)
		close(legs[1].audioCh)

		frames := rec.wait(t)
		requireSilenceBetweenRealChunks(t, frames, relayTestRate)
		requireSectionsAcrossGap(t, frames, 0, 1)
	})

	t.Run("next leg not launched yet", func(t *testing.T) {
		rec := newRelayRecorder()
		leg0 := newVoiceSpeechLeg(0, "", true, 0, 0, nil)
		leg1 := newVoiceSpeechLeg(1, "", true, 0, 0, nil)
		legsCh := make(chan *voiceSpeechLeg, 2)
		legsCh <- leg0
		rec.start(legsCh, relayTestFallback, true)

		leg0.audioCh <- realChunk(relayTestRate)
		close(leg0.audioCh)
		time.Sleep(relayTestGap)
		legsCh <- leg1
		leg1.audioCh <- realChunk(relayTestRate)
		close(leg1.audioCh)
		close(legsCh)

		frames := rec.wait(t)
		requireSilenceBetweenRealChunks(t, frames, relayTestRate)
		requireSectionsAcrossGap(t, frames, 0, 1)
	})
}

// With no rate to play silence at -- the model's rate is unknown (fallback 0) and no real audio has
// come yet -- there is no silence: a guessed rate would play as noise. The relay still returns.
func TestRelayNoRateNoSilence(t *testing.T) {
	t.Run("leg closes at once", func(t *testing.T) {
		rec := newRelayRecorder()
		legs, legsCh := relayLegs(1)
		rec.start(legsCh, 0, true)
		close(legs[0].audioCh)

		if frames := rec.wait(t); len(frames) != 0 {
			t.Fatalf("published %d frames, want none: %+v", len(frames), frames)
		}
	})

	t.Run("leg closes after a gap", func(t *testing.T) {
		rec := newRelayRecorder()
		legs, legsCh := relayLegs(1)
		rec.start(legsCh, 0, true)
		time.Sleep(relayTestGap)
		close(legs[0].audioCh)

		if frames := rec.wait(t); len(frames) != 0 {
			t.Fatalf("published %d frames over a gap with no rate, want none: %+v", len(frames), frames)
		}
	})
}

// Silence fills a QUIET gap, never a busy stream: while a leg's chunks keep coming faster than the
// cadence the timer is pushed back by each one, so no silence frame lands between two real chunks.
// The stream here runs for twice the cadence, so a timer that was NOT reset by real audio would
// fire in the middle of it.
func TestRelayNoSilenceWhenAudioSteady(t *testing.T) {
	const chunks = 12
	rec := newRelayRecorder()
	legs, legsCh := relayLegs(1)
	rec.start(legsCh, relayTestFallback, true)

	for range chunks {
		legs[0].audioCh <- realChunk(relayTestRate)
		time.Sleep(voiceSilenceCadence / 6)
	}
	close(legs[0].audioCh)

	frames := rec.wait(t)
	requireOnlyRealAudio(t, frames)
	requireSeqUnbroken(t, frames)
	if len(frames) != chunks {
		t.Fatalf("published %d frames, want the %d real chunks", len(frames), chunks)
	}
}

// The relay ends with the audio: once the last leg's audioCh and the leg feed have both closed,
// nothing more is published -- no trailing silence after the final real chunk.
func TestRelayNoTrailingSilence(t *testing.T) {
	rec := newRelayRecorder()
	legs, legsCh := relayLegs(1)
	rec.start(legsCh, relayTestFallback, true)

	legs[0].audioCh <- realChunk(relayTestRate)
	close(legs[0].audioCh)

	frames := rec.wait(t)
	if len(frames) != 1 || isSilenceFrame(t, frames[0]) || frames[0].Seq != 0 {
		t.Fatalf("published %+v, want exactly the one real chunk", frames)
	}
}

// A leg that fails for good after its retries -- silence played over its backoff, then audioCh
// closed with no real audio -- must not wedge the relay: it advances to the next section, relays its
// audio in the same unbroken seq, and returns once everything has closed.
func TestRelayPermanentFailureAfterSilence(t *testing.T) {
	rec := newRelayRecorder()
	legs, legsCh := relayLegs(2)
	rec.start(legsCh, relayTestFallback, true)

	time.Sleep(relayTestGap)
	close(legs[0].audioCh) // the failed leg: no real audio, ever
	legs[1].audioCh <- realChunk(relayTestRate)
	close(legs[1].audioCh)

	frames := rec.wait(t)
	requireSilenceThenRealChunk(t, frames, relayTestFallback, relayTestRate)
	if frames[len(frames)-1].Section != 1 {
		t.Fatalf("the real chunk is tagged section %d, want 1", frames[len(frames)-1].Section)
	}
}

// Before the first section of audio the user has heard nothing, and the turn is only "thinking": the
// wait for the first leg (the LLM has not closed a region yet) stays quiet, as it always was, even
// with the run's rate known. Silence there would start a stream the client has no reason to play.
func TestRelayNoPreFirstAudioSilenceOnOuterWait(t *testing.T) {
	rec := newRelayRecorder()
	leg := newVoiceSpeechLeg(0, "", true, 0, 0, nil)
	legsCh := make(chan *voiceSpeechLeg, 1)
	rec.start(legsCh, relayTestFallback, true)

	time.Sleep(relayTestGap)
	legsCh <- leg
	close(legsCh)
	leg.audioCh <- realChunk(relayTestRate)
	close(leg.audioCh)

	frames := rec.wait(t)
	if len(frames) != 1 || isSilenceFrame(t, frames[0]) {
		t.Fatalf("published %+v, want only the real chunk: no silence before the first audio", frames)
	}
}

// Gap silence belongs to the streaming regime: a run whose speech streaming is opted out plays no
// live audio at all, so the relay must not start a stream of silence there -- not over a first
// leg's wait, and not between sections once real audio has flowed.
func TestRelayNoSilenceWhenStreamingOptedOut(t *testing.T) {
	rec := newRelayRecorder()
	legs, legsCh := relayLegs(2)
	rec.start(legsCh, relayTestFallback, false)

	time.Sleep(relayTestGap)
	legs[0].audioCh <- realChunk(relayTestRate)
	close(legs[0].audioCh)
	time.Sleep(relayTestGap)
	legs[1].audioCh <- realChunk(relayTestRate)
	close(legs[1].audioCh)

	frames := rec.wait(t)
	requireOnlyRealAudio(t, frames)
	requireSeqUnbroken(t, frames)
	if len(frames) != 2 {
		t.Fatalf("published %d frames, want the 2 real chunks: %+v", len(frames), frames)
	}
}

// The opt-out is the thread's explicit "do not stream": unset (nil) is not one, so the model's own
// capability decides. Both the run's streaming gate and the relay's gap silence read it here.
func TestSpeechStreamOptedOut(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		stream *bool
		want   bool
	}{
		{"unset", nil, false},
		{"explicitly on", &yes, false},
		{"explicitly off", &no, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := speechStreamOptedOut(tc.stream); got != tc.want {
				t.Fatalf("speechStreamOptedOut = %v, want %v", got, tc.want)
			}
		})
	}
}
