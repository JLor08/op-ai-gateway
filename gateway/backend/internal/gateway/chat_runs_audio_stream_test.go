// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// The portal speech-streaming run (ADR-068, the TTS composer): a streaming branch of the
// ADR-063 speech run that asks the gateway's own /v1/audio/speech for a stream, relays the
// PCM to the client as `audio` events and commits the assembled WAV as the turn.

const (
	// 88200 bytes a second is 44.1 kHz, 16-bit, mono: the live contract (spec section 2).
	speechRunTestByteRate = "88200"
	speechRunTestRate     = 44100
)

// seedStreamableSpeech makes the mapping a portal speech run may stream: a
// (speech_streaming, yes) verdict and a cached output byte rate.
func seedStreamableSpeech(t *testing.T, srv *Server, mappingID, byteRate string) {
	t.Helper()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.UpsertMappingCapabilities(context.Background(), mappingID, []routing.CapabilityRow{{
		Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes,
		Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
	}
	if byteRate != "" {
		seedSpeechOutputByteRate(t, srv, mappingID, byteRate)
	}
}

// subscribedSpeechRun starts a speech run with its subscription already open (a run
// started the plain way may publish before a test could subscribe) and returns the run
// and everything it publishes until it ends.
func subscribedSpeechRun(t *testing.T, srv *Server, owner auth.Token, chatID string, prep PrepareRunResult) (*ChatRun, func() []runEvent) {
	t.Helper()
	run, ctx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	srv.launchRun(ctx, owner, run, prep)
	return run, func() []runEvent {
		defer unsub()
		var events []runEvent
		timeout := time.After(10 * time.Second)
		for {
			select {
			case ev, open := <-ch:
				if !open {
					return events
				}
				events = append(events, ev)
			case <-timeout:
				t.Fatal("the run published no terminal event within 10s")
			}
		}
	}
}

// audioEventsOf returns the audio events among events, in order.
func audioEventsOf(events []runEvent) []runAudio {
	var out []runAudio
	for _, ev := range events {
		if ev.Event == "audio" && ev.Audio != nil {
			out = append(out, *ev.Audio)
		}
	}
	return out
}

// committedWAV returns the bytes of the one audio part of a terminal event.
func committedWAV(t *testing.T, parts json.RawMessage) []byte {
	t.Helper()
	urls := audioPartURLs(t, parts)
	if len(urls) != 1 {
		t.Fatalf("want one audio part, got %s", parts)
	}
	payload, ok := strings.CutPrefix(urls[0], "data:audio/wav;base64,")
	if !ok {
		t.Fatalf("audio url %.60q is not a data:audio/wav;base64 url", urls[0])
	}
	wav, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the audio payload is not standard padded base64: %v", err)
	}
	return wav
}

// --- the gate --------------------------------------------------------------

// A speech run streams when the mapped model is (speech_streaming, yes) AND its output
// byte rate is cached: the run asks the gateway for a stream, every PCM chunk reaches the
// client as an `audio` event (in order, the sample rate on each so the client can play
// headerless PCM), and the turn is the assembled WAV -- the 44-byte header the stream
// never carried, then the PCM exactly as it came -- committed as the one audio part. No
// text is ever written into the run's buffer: the PCM is not text.
func TestSpeechRunStreamsWhenCapableAndRateKnown(t *testing.T) {
	first, second := speechPCM(8000, 1), speechPCM(6000, 2)
	up, calls := newSSESpeechUpstream(t, speechFrameDelta(first), speechFrameDelta(second), speechSSEDone, speechSSEEnd)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, up.URL)
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)

	run, collect := subscribedSpeechRun(t, srv, owner, chatID, speechRunPrep())
	events := collect()

	done := events[len(events)-1]
	if done.Event != "done" || done.Status != "completed" {
		t.Fatalf("terminal event = %s %q (err %q), want done completed", done.Event, done.Status, done.Err)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want none", got)
	}

	// The gateway was asked for a stream, by the run's own request.
	sent := drainSpeechUpstreamCalls(calls)
	if len(sent) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(sent))
	}
	fields := speechUpstreamFields(t, sent[0])
	if fields["stream"] != true || fields["stream_format"] != "sse" {
		t.Fatalf("upstream body = %s, want stream=true and stream_format=sse", sent[0].body)
	}

	audio := audioEventsOf(events)
	if len(audio) != 2 {
		t.Fatalf("audio events = %d, want 2 (one per delta)", len(audio))
	}
	for i, want := range [][]byte{first, second} {
		if audio[i].Seq != i || audio[i].Rate != speechRunTestRate {
			t.Fatalf("audio event %d = seq %d rate %d, want seq %d rate %d", i, audio[i].Seq, audio[i].Rate, i, speechRunTestRate)
		}
		if audio[i].Data != base64.StdEncoding.EncodeToString(want) {
			t.Fatalf("audio event %d carries other bytes than the delta it relays", i)
		}
	}
	if done.Content != "" {
		t.Fatalf("a speech stream wrote %q into the text buffer, want nothing", done.Content)
	}

	// The assembled WAV: header the stream never had, PCM as it came.
	wav := committedWAV(t, done.ContentParts)
	pcm := append(append([]byte(nil), first...), second...)
	if len(wav) != 44+len(pcm) {
		t.Fatalf("WAV is %d bytes, want %d (44-byte header + %d PCM)", len(wav), 44+len(pcm), len(pcm))
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatalf("WAV header is malformed: % x", wav[:44])
	}
	le := binary.LittleEndian
	if le.Uint16(wav[22:]) != 1 || le.Uint32(wav[24:]) != speechRunTestRate || le.Uint32(wav[28:]) != 88200 || le.Uint16(wav[32:]) != 2 || le.Uint16(wav[34:]) != 16 {
		t.Fatalf("WAV fmt = channels %d rate %d byterate %d align %d bits %d, want mono 44100 Hz 88200 B/s 16-bit",
			le.Uint16(wav[22:]), le.Uint32(wav[24:]), le.Uint32(wav[28:]), le.Uint16(wav[32:]), le.Uint16(wav[34:]))
	}
	if le.Uint32(wav[40:]) != uint32(len(pcm)) || le.Uint32(wav[4:]) != uint32(36+len(pcm)) {
		t.Fatalf("WAV sizes = riff %d data %d, want %d and %d", le.Uint32(wav[4:]), le.Uint32(wav[40:]), 36+len(pcm), len(pcm))
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Fatal("the WAV's PCM differs from the PCM the stream carried")
	}
}

// Without the rate the run cannot tell the client how to play the PCM (and cannot write
// the WAV's header), so it never asks for a stream: it is today's buffered run of ADR-063,
// one request without any stream field, the upstream's WAV committed byte for byte.
func TestSpeechRunFallsBackToBufferedWithoutRate(t *testing.T) {
	wav := speechTestWAV()
	up, calls := newSpeechUpstream(t, wav)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, up.URL)
	seedStreamableSpeech(t, srv, "route-tts", "") // the verdict, but no rate

	_, collect := subscribedSpeechRun(t, srv, owner, chatID, speechRunPrep())
	events := collect()

	done := events[len(events)-1]
	if done.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", done.Status, done.Err)
	}
	if got := audioEventsOf(events); len(got) != 0 {
		t.Fatalf("a buffered run published %d audio events, want none", len(got))
	}
	sent := drainSpeechUpstreamCalls(calls)
	if len(sent) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(sent))
	}
	for _, key := range []string{"stream", "stream_format", "response_format"} {
		if _, ok := speechUpstreamFields(t, sent[0])[key]; ok {
			t.Fatalf("a buffered run sent %q: %s", key, sent[0].body)
		}
	}
	if !bytes.Equal(committedWAV(t, done.ContentParts), wav) {
		t.Fatal("the committed audio differs from the WAV audio.cpp produced")
	}
}

// Each way the gate can be closed keeps the run buffered, and none of them is an error:
// the user gets today's complete WAV.
func TestSpeechRunStreamGateStaysClosedUnlessEverythingHolds(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		arrange  func(t *testing.T, srv *Server)
		prep     func(p *PrepareRunResult)
		wantRate int // 0: the gate is closed
	}{
		{"every condition holds", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate) }, nil, speechRunTestRate},
		{"no verdict, a rate", func(t *testing.T, srv *Server) { seedSpeechOutputByteRate(t, srv, "route-tts", speechRunTestByteRate) }, nil, 0},
		{"a no verdict", func(t *testing.T, srv *Server) {
			seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
			if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-tts", []routing.CapabilityRow{{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
				t.Fatal(err)
			}
		}, nil, 0},
		{"a rate that is no integer", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", "fast") }, nil, 0},
		{"a rate of zero", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", "0") }, nil, 0},
		// 16-bit mono PCM has an even byte rate; an odd one is not that format, and a
		// guessed sample rate would play the audio at the wrong speed.
		{"an odd byte rate", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", "88201") }, nil, 0},
		{"an implausibly low sample rate", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", "2000") }, nil, 0},
		{"an implausibly high sample rate", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", "4000000") }, nil, 0},
		{"the thread turned streaming off", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate) }, func(p *PrepareRunResult) {
			off := false
			p.Settings.Audio = &portal.ChatAudioSettings{Stream: &off}
		}, 0},
		{"the thread left streaming at its default", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate) }, func(p *PrepareRunResult) {
			p.Settings.Audio = &portal.ChatAudioSettings{Voice: "alloy"}
		}, speechRunTestRate},
		{"a second mapping of the model that cannot stream", func(t *testing.T, srv *Server) {
			seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
			addSpeechMapping(t, srv, "route-tts-b", speechGatewayModel, nil, "")
		}, nil, 0},
		{"a second mapping with another rate", func(t *testing.T, srv *Server) {
			seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
			addSpeechMapping(t, srv, "route-tts-b", speechGatewayModel, ptr(true), "48000")
		}, nil, 0},
		{"a second mapping with the same rate", func(t *testing.T, srv *Server) {
			seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
			addSpeechMapping(t, srv, "route-tts-b", speechGatewayModel, ptr(true), speechRunTestByteRate)
		}, nil, speechRunTestRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
			srv, owner, _, _ := newSpeechRunTestServer(t, up.URL)
			tc.arrange(t, srv)
			prep := speechRunPrep()
			if tc.prep != nil {
				tc.prep(&prep)
			}
			rate, ok := srv.speechRunStreamRate(context.Background(), owner, prep)
			if tc.wantRate == 0 {
				if ok {
					t.Fatalf("the gate is open at %d Hz, want it closed", rate)
				}
				return
			}
			if !ok || rate != tc.wantRate {
				t.Fatalf("gate = (%d, %v), want (%d, true)", rate, ok, tc.wantRate)
			}
		})
	}
}

// The gate judges the model the request will ROUTE for, not the thread's: a run-as token's
// model override rewrites the requested name before the gateway resolves anything
// (inferencePreflight), so reading the thread's own would judge a different model than the
// one that synthesizes. Here the thread names a model that cannot stream and the override
// sends it to one that can -- and the other way round.
func TestSpeechRunStreamGateFollowsTheRunAsTokensModelOverride(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, dir, owner, _, _ := newSpeechRunTestServerWithDirectory(t, up.URL)
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)  // tts-voice streams
	addSpeechMapping(t, srv, "route-tts-plain", "tts-plain", nil, "") // tts-plain does not
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for id, override := range map[string]string{"tok_to_stream": speechGatewayModel, "tok_to_plain": "tts-plain"} {
		if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{
			ID: id, UserID: "usr_dev", Name: id, Status: store.TokenStatusActive,
			Scopes: `["gateway:use"]`, ModelOverride: override, CreatedAt: now, UpdatedAt: now,
		}, id+"-secret"); err != nil {
			t.Fatalf("seed token %s: %v", id, err)
		}
	}

	prep := speechRunPrep()
	prep.Settings.Model = "tts-plain"
	prep.Settings.RunAsTokenID = "tok_to_stream"
	if rate, ok := srv.speechRunStreamRate(context.Background(), owner, prep); !ok || rate != speechRunTestRate {
		t.Fatalf("a plain model overridden to a streaming one: gate = (%d, %v), want (%d, true)", rate, ok, speechRunTestRate)
	}
	prep.Settings.Model = speechGatewayModel
	prep.Settings.RunAsTokenID = "tok_to_plain"
	if rate, ok := srv.speechRunStreamRate(context.Background(), owner, prep); ok {
		t.Fatalf("a streaming model overridden to a plain one: gate open at %d Hz, want closed", rate)
	}
	prep.Settings.RunAsTokenID = "tok_nobody" // not authorized: the run is buffered, never failed here
	if _, ok := srv.speechRunStreamRate(context.Background(), owner, prep); ok {
		t.Fatal("an unauthorized run-as token must close the gate")
	}
}

// addSpeechMapping adds a second plain tts mapping of the gateway model on the fixture's
// audio application, with a speech_streaming yes verdict when streams is not nil and the
// output byte rate cached when byteRate is not empty.
func addSpeechMapping(t *testing.T, srv *Server, id, gatewayModel string, streams *bool, byteRate string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app-audio", GatewayModelName: gatewayModel, AppModelName: "other_" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", id, err)
	}
	rows := []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}
	if streams != nil {
		verdict := routing.CapabilityNo
		if *streams {
			verdict = routing.CapabilityYes
		}
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilitySpeechStreaming, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now})
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, id, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
	}
	if byteRate != "" {
		seedSpeechOutputByteRate(t, srv, id, byteRate)
	}
}

// --- the run's request -----------------------------------------------------

// The streaming request carries the run's own stream fields and every setting the buffered
// one does; the buffered request carries none of them.
func TestBuildSpeechStreamBodyAddsTheStreamFieldsToTheSettingsBody(t *testing.T) {
	prep := speechRunPrep()
	speed := 1.5
	prep.Settings.Audio = &portal.ChatAudioSettings{Voice: "alloy", Speed: &speed}

	buffered, err := buildSpeechBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := buildSpeechStreamBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	got, want := decodeSpeechBody(t, streamed), decodeSpeechBody(t, buffered)
	if got["stream"] != true || got["stream_format"] != "sse" {
		t.Fatalf("streaming body = %s, want stream=true and stream_format=sse", streamed)
	}
	delete(got, "stream")
	delete(got, "stream_format")
	if len(got) != len(want) {
		t.Fatalf("streaming body minus its stream fields = %v, want the buffered body %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %q = %v, want %v", k, got[k], v)
		}
	}
	for _, key := range []string{"stream", "stream_format", "response_format"} {
		if _, ok := want[key]; ok {
			t.Fatalf("the buffered body carries %q: %s", key, buffered)
		}
	}
}

// The run owns stream_format now (it asks for the event stream it can read), so a thread's
// passthrough may not set it: raw PCM would arrive in a shape the run cannot parse.
func TestBuildSpeechBodyRejectsAStreamFormatPassthrough(t *testing.T) {
	for _, key := range []string{"stream_format", "Stream_Format"} {
		prep := speechRunPrep()
		prep.Settings.Audio = &portal.ChatAudioSettings{Passthrough: map[string]json.RawMessage{key: json.RawMessage(`"audio"`)}}
		if raw, err := buildSpeechBody(prep); err == nil {
			t.Fatalf("%q was admitted: %s", key, raw)
		}
		if raw, err := buildSpeechStreamBody(prep); err == nil {
			t.Fatalf("%q was admitted on a streaming body: %s", key, raw)
		}
	}
}

// --- the stream's outcomes --------------------------------------------------

// scriptedSpeechRunLoopback is a /v1/audio/speech stand-in the run is pointed at, which
// answers a request through script (so a test can stream, stall or cut the answer) and
// records every request it received.
func scriptedSpeechRunLoopback(t *testing.T, script func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, <-chan speechRunLoopbackCall) {
	t.Helper()
	calls := make(chan speechRunLoopbackCall, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls <- speechRunLoopbackCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: raw}
		script(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, calls
}

// sseFrames writes frames to w, flushing each, as an upstream event stream.
func sseFrames(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	for _, frame := range frames {
		_, _ = io.WriteString(w, frame)
		flusher.Flush()
	}
}

// streamableSpeechRunServer is a run server whose mapping streams at 44.1 kHz, with its own
// loopback pointed at the scripted stand-in.
func streamableSpeechRunServer(t *testing.T, script func(w http.ResponseWriter, r *http.Request)) (*Server, auth.Token, string, <-chan speechRunLoopbackCall) {
	t.Helper()
	loopback, calls := scriptedSpeechRunLoopback(t, script)
	srv, owner, chatID, _ := newSpeechRunTestServer(t, loopback.URL)
	srv.selfBaseURL = loopback.URL
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
	return srv, owner, chatID, calls
}

// A stream that ends before its speech.audio.done and [DONE] is not the synthesis: what
// arrived is a prefix of the audio, and committing it would present a cut clip as the whole
// reply. The run ends in the unreadable-response error and commits no audio part.
func TestSpeechRunStreamCutBeforeItsEndIsNotCommitted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"no terminal events", []string{speechFrameDelta(speechPCM(400, 1))}},
		{"done but no [DONE]", []string{speechFrameDelta(speechPCM(400, 1)), speechSSEDone}},
		{"[DONE] without done", []string{speechFrameDelta(speechPCM(400, 1)), speechSSEEnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) { sseFrames(w, tc.frames...) })
			run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
			if snap.Status != "error" {
				t.Fatalf("status = %q, want error", snap.Status)
			}
			if got := runError(t, run); got != speechRunResponseUnreadableMessage {
				t.Fatalf("error = %q, want %q", got, speechRunResponseUnreadableMessage)
			}
			if len(snap.ContentParts) != 0 {
				t.Fatalf("a cut stream committed parts: %s", snap.ContentParts)
			}
		})
	}
}

// A stream that completes having carried no audio is an error, not a blank bubble.
func TestSpeechRunStreamWithoutAudioIsAnError(t *testing.T) {
	srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) { sseFrames(w, speechSSEDone, speechSSEEnd) })
	run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
	if snap.Status != "error" || runError(t, run) != speechRunNoAudioMessage {
		t.Fatalf("status = %q error = %q, want error %q", snap.Status, runError(t, run), speechRunNoAudioMessage)
	}
}

// The gateway may answer a stream request with the one-shot WAV (a verdict gone stale: it
// retried buffered). The run reads that answer as the buffered run does.
func TestSpeechRunStreamRequestAnsweredWithAWAVIsCommittedBuffered(t *testing.T) {
	wav := speechTestWAV()
	srv, owner, chatID, calls := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	})
	_, collect := subscribedSpeechRun(t, srv, owner, chatID, speechRunPrep())
	events := collect()
	done := events[len(events)-1]
	if done.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", done.Status, done.Err)
	}
	if len(audioEventsOf(events)) != 0 {
		t.Fatal("a one-shot answer published audio events")
	}
	if !bytes.Equal(committedWAV(t, done.ContentParts), wav) {
		t.Fatal("the committed audio differs from the WAV the gateway answered")
	}
	if got := takeSpeechRunCall(t, calls); !bytes.Contains(got.body, []byte(`"stream":true`)) {
		t.Fatalf("the run was meant to ask for a stream: %s", got.body)
	}
}

// The endpoint's own refusal of a stream request reaches the run as its code.
func TestSpeechRunStreamSurfacesTheEndpointsRefusal(t *testing.T) {
	srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"nope","type":"invalid_request_error","code":"speech.stream_unsupported"}}`)
	})
	run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
	if snap.Status != "error" || runError(t, run) != "speech.stream_unsupported" {
		t.Fatalf("status = %q error = %q, want error speech.stream_unsupported", snap.Status, runError(t, run))
	}
}

// The deadline of a streaming run is the budget for the WAIT for the first chunk, as a
// streaming transcription's is: once audio is arriving the stream shows for itself that it
// is alive, so the bound is lifted -- a synthesis longer than the deadline completes -- and
// one that never produces a chunk still ends at the deadline, as a timeout and not a cancel.
func TestSpeechRunStreamDeadlineBoundsTheWaitForTheFirstChunkOnly(t *testing.T) {
	restore := speechRunDeadline
	speechRunDeadline = 150 * time.Millisecond
	t.Cleanup(func() { speechRunDeadline = restore })

	t.Run("a stream that keeps delivering outlives the deadline", func(t *testing.T) {
		pcm := speechPCM(600, 5)
		srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, speechFrameDelta(pcm))
			flusher.Flush()
			time.Sleep(400 * time.Millisecond) // well past the deadline, after the first chunk
			_, _ = io.WriteString(w, speechFrameDelta(pcm)+speechSSEDone+speechSSEEnd)
		})
		run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
		if snap.Status != "completed" {
			t.Fatalf("status = %q (err %q), want completed: the deadline must lift at the first chunk", snap.Status, runError(t, run))
		}
	})

	t.Run("a stream that never delivers times out", func(t *testing.T) {
		srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		run, snap := speechRunOutcome(t, srv, owner, chatID, speechRunPrep())
		if snap.Status != "error" || runError(t, run) != runTimedOutMessage {
			t.Fatalf("status = %q error = %q, want error %q", snap.Status, runError(t, run), runTimedOutMessage)
		}
	})
}

// Stop on a streaming run stays a cancel, with no message.
func TestSpeechRunStreamUserCancelStaysACancel(t *testing.T) {
	srv, owner, chatID, calls := streamableSpeechRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		sseFrames(w, speechFrameDelta(speechPCM(400, 1)))
		<-r.Context().Done()
	})
	run, err := srv.startChatRun(owner, chatID, speechRunPrep())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the loopback")
	}
	time.Sleep(50 * time.Millisecond) // let the first chunk through, so the stream is what is cancelled
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want empty", got)
	}
}

// A chunk may split a 16-bit sample, and a stream may end on half of one: the WAV is
// written in whole samples, and the run says so in its log instead of committing a data
// chunk whose size is odd.
func TestWAVFromPCM16IsWrittenInWholeSamples(t *testing.T) {
	even := wavFromPCM16(speechPCM(10, 1), 24000)
	odd := wavFromPCM16(speechPCM(11, 1), 24000)
	if len(even) != 44+10 || len(odd) != 44+10 {
		t.Fatalf("WAV sizes = %d and %d, want 54 for both (the odd byte is dropped)", len(even), len(odd))
	}
	if got := binary.LittleEndian.Uint32(odd[40:]); got != 10 {
		t.Fatalf("data size = %d, want 10", got)
	}
	if got := binary.LittleEndian.Uint32(odd[24:]); got != 24000 {
		t.Fatalf("sample rate = %d, want 24000", got)
	}
}

// The portal's gate endpoint says whether the composer may offer streaming for a model.
func TestPortalModelSpeechStreamingAnswersTheGate(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, _, _, _ := newSpeechRunTestServer(t, up.URL)
	ask := func(query string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/portal/model-speech-streaming?"+query, nil)
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	if code, body := ask("name=" + speechGatewayModel); code != http.StatusOK || body["streaming"] != false {
		t.Fatalf("before the verdict and the rate: %d %v, want 200 streaming=false", code, body)
	}
	seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
	code, body := ask("name=" + speechGatewayModel)
	if code != http.StatusOK || body["streaming"] != true || body["sample_rate"] != float64(speechRunTestRate) {
		t.Fatalf("with both: %d %v, want 200 streaming=true sample_rate=%d", code, body, speechRunTestRate)
	}
	if code, body := ask("name=no-such-model"); code != http.StatusOK || body["streaming"] != false {
		t.Fatalf("an unknown model: %d %v, want 200 streaming=false", code, body)
	}
	if code, _ := ask("name="); code != http.StatusBadRequest {
		t.Fatalf("a blank name: %d, want 400", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/portal/model-speech-streaming?name=x", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/portal/model-speech-streaming?name=x", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", rec.Code)
	}
}
