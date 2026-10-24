// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"os"
	"slices"
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

// addPresetCloneMapping adds a configured clone mapping of the gateway model on the fixture's
// audio application: a speech model that is a clone one and ships a named preset, which the
// portal now OFFERS (the voice picker, the language control, #301), so a speech request for
// the name can be routed to it. The output byte rate and the speech_streaming yes verdict
// are seeded when streams is true / byteRate is not empty, so a case can give it the facts
// a stream candidate needs (the benchmark never learns them for a clone model today, so
// they are the exception, not the rule).
func addPresetCloneMapping(t *testing.T, srv *Server, id, gatewayModel string, streams bool, byteRate string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app-audio", GatewayModelName: gatewayModel, AppModelName: "clone_" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", id, err)
	}
	rows := []routing.CapabilityRow{
		{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
		{Capability: routing.CapabilitySpeechClone, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
	}
	if streams {
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now})
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, id, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
	}
	if err := srv.Routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: id, CacheType: routing.ModelMappingCacheVoices, ScopeKey: "", Payload: `["Thorsten"]`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelMappingCache %s: %v", id, err)
	}
	if byteRate != "" {
		seedSpeechOutputByteRate(t, srv, id, byteRate)
	}
}

// The portal's STREAM gate counts every mapping a speech request could be ROUTED to, which
// since #301 includes a configured preset clone (the same offerable set the voice picker and
// the language control read). This test pins the state BEFORE the rate benchmark has run for a
// preset clone: with no cached stream rate yet it closes the gate, so a name that mixes a
// streaming plain tts mapping with a not-yet-benchmarked preset-clone sibling is BUFFERED, the
// known-good path -- the pick is made per request after the gate is asked, so a streamed request
// routed to the clone sibling must not be played, committed as a WAV and billed at the PLAIN
// mapping's rate. (Since #306 the benchmark learns a preset clone's rate, and it then streams
// when the candidates agree on a rate -- see the rate-benchmark tests.) A name that is only
// plain tts still streams; a name that is only a not-yet-benchmarked preset clone is closed too.
func TestPortalSpeechStreamRateClosesForAReachablePresetClone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		arrange  func(t *testing.T, srv *Server)
		model    string
		wantRate int // 0: the gate is closed
	}{
		{"the plain mapping alone", func(t *testing.T, srv *Server) {}, speechGatewayModel, speechRunTestRate},
		{"a preset clone sibling with no stream facts", func(t *testing.T, srv *Server) {
			addPresetCloneMapping(t, srv, "route-clone", speechGatewayModel, false, "")
		}, speechGatewayModel, 0},
		{"a preset clone sibling with a streaming verdict but no cached rate", func(t *testing.T, srv *Server) {
			addPresetCloneMapping(t, srv, "route-clone", speechGatewayModel, true, "")
		}, speechGatewayModel, 0},
		{"a preset clone sibling that would stream at another rate", func(t *testing.T, srv *Server) {
			addPresetCloneMapping(t, srv, "route-clone", speechGatewayModel, true, "48000")
		}, speechGatewayModel, 0},
		{"a name that is only a preset clone", func(t *testing.T, srv *Server) {
			addPresetCloneMapping(t, srv, "route-clone", "tts-clone-only", false, "")
		}, "tts-clone-only", 0},
		{"a name that is only a preset clone with a streaming verdict but no cached rate", func(t *testing.T, srv *Server) {
			addPresetCloneMapping(t, srv, "route-clone", "tts-clone-only", true, "")
		}, "tts-clone-only", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
			srv, owner, _, _ := newSpeechRunTestServer(t, up.URL)
			seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate)
			tc.arrange(t, srv)

			rate, ok, err := srv.portalSpeechStreamRate(context.Background(), owner, tc.model)
			if err != nil {
				t.Fatalf("portalSpeechStreamRate: %v", err)
			}
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

// --- the portal's live-transcription gate -------------------------------------
//
// GET /api/portal/model-transcription-streaming is the speech gate's twin for the live
// record mode: the portal offers it only for a model the ticket endpoint would also let
// stream. Its fixture is the speech run's server (a Portal over one audio.cpp application),
// to which a test adds the transcription mappings it needs.

// addTranscriptionMapping adds a mapping of gatewayModel on the fixture's audio.cpp
// application with a (transcription, yes) verdict and, when streams is given, a
// transcription_streaming verdict of yes or no (nil: no row at all, a model no probe has
// stated the mode of).
func addTranscriptionMapping(t *testing.T, srv *Server, id, gatewayModel string, streams *bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app-audio", GatewayModelName: gatewayModel, AppModelName: "asr_" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", id, err)
	}
	rows := []routing.CapabilityRow{{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}
	if streams != nil {
		verdict := routing.CapabilityNo
		if *streams {
			verdict = routing.CapabilityYes
		}
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilityTranscriptionStreaming, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now})
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, id, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
	}
}

// askTranscriptionStreaming GETs the gate as the fixture's dev bearer and returns the
// recorder, for a test that reads the status, the code or the raw body.
func askTranscriptionStreaming(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/portal/model-transcription-streaming?"+query, nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// requireTranscriptionGate fails unless the gate answers 200 with exactly
// {"streaming": want}: one key, so the "no" carries nothing to learn from and a "yes" no
// sample rate (the client fixes its own format and the ticket endpoint re-gates).
func requireTranscriptionGate(t *testing.T, srv *Server, model string, want bool) {
	t.Helper()
	rec := askTranscriptionStreaming(srv, "name="+model)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%q: the answer is not JSON: %v (body = %s)", model, err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || len(body) != 1 || body["streaming"] != want {
		t.Fatalf("%q: %d %s, want 200 {\"streaming\":%v}", model, rec.Code, rec.Body.String(), want)
	}
}

// The gate is open only for a model every transcription mapping of which has a
// transcription_streaming verdict of yes: the verdict the buffered endpoint relays a stream
// on, and the one the ticket endpoint (mappingStreamsTranscription) requires.
func TestPortalModelTranscriptionStreamingAnswersTheGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, srv *Server)
		model   string
		want    bool
	}{
		{"a streaming asr model", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-live", "asr-live", ptr(true))
		}, "asr-live", true},
		{"an offline asr model", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-off", "asr-off", ptr(false))
		}, "asr-off", false},
		{"an asr model whose mode no probe has stated", func(t *testing.T, srv *Server) { addTranscriptionMapping(t, srv, "route-asr-new", "asr-new", nil) }, "asr-new", false},
		{"a speech-only model", func(*testing.T, *Server) {}, speechGatewayModel, false},
		{"a speech model that streams its synthesis", func(t *testing.T, srv *Server) { seedStreamableSpeech(t, srv, "route-tts", speechRunTestByteRate) }, speechGatewayModel, false},
		{"an unknown model", func(*testing.T, *Server) {}, "no-such-asr", false},
		{"two mappings that both stream", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-a", "asr-two", ptr(true))
			addTranscriptionMapping(t, srv, "route-asr-b", "asr-two", ptr(true))
		}, "asr-two", true},
		// A transcription request may be routed to either mapping, so the model streams only
		// if it makes no difference which one serves it.
		{"two mappings, one offline", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-a", "asr-two", ptr(true))
			addTranscriptionMapping(t, srv, "route-asr-b", "asr-two", ptr(false))
		}, "asr-two", false},
		{"two mappings, one unprobed", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-a", "asr-two", ptr(true))
			addTranscriptionMapping(t, srv, "route-asr-b", "asr-two", nil)
		}, "asr-two", false},
		// A mapping that is no transcription route (no (transcription, yes) verdict) is never
		// a candidate of a transcription request, whatever else it says: a speech mapping of
		// the name does not close the gate, and a streaming verdict on a non-transcriber does
		// not open it.
		{"a speech mapping beside the streaming asr one", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-a", speechGatewayModel, ptr(true))
		}, speechGatewayModel, true},
		{"a streaming verdict on a mapping that does not transcribe", func(t *testing.T, srv *Server) {
			addTranscriptionMapping(t, srv, "route-asr-a", "asr-odd", ptr(true))
			if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-asr-a", []routing.CapabilityRow{{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: time.Date(2026, 10, 23, 12, 0, 1, 0, time.UTC)}}); err != nil {
				t.Fatal(err)
			}
		}, "asr-odd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
			srv, _, _, _ := newSpeechRunTestServer(t, up.URL)
			tc.arrange(t, srv)
			requireTranscriptionGate(t, srv, tc.model, tc.want)
		})
	}
}

// The gate follows the portal's visibility, not routing's: a hidden or locked model is the
// same 200 {"streaming":false} as an unknown one (a "no" that cannot probe which models
// exist), while a model reachable through a model group is judged over ALL the group's
// members, as the voice picker does.
func TestPortalModelTranscriptionStreamingFollowsThePortalsVisibility(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, _, _, _ := newSpeechRunTestServer(t, up.URL)
	addTranscriptionMapping(t, srv, "route-asr-live", "asr-live", ptr(true))
	addTranscriptionMapping(t, srv, "route-asr-lock", "asr-lock", ptr(true))
	addTranscriptionMapping(t, srv, "route-asr-off", "asr-off", ptr(false))
	requireTranscriptionGate(t, srv, "asr-live", true)
	requireTranscriptionGate(t, srv, "asr-lock", true)

	ctx := context.Background()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for name, visibility := range map[string]string{"asr-live": "hidden", "asr-lock": "locked"} {
		if err := srv.Routes.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: name, Visibility: visibility, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertModelSetting %s: %v", name, err)
		}
	}
	requireTranscriptionGate(t, srv, "asr-live", false)
	requireTranscriptionGate(t, srv, "asr-lock", false)

	// A group streams only when every member does; a locked member is still a full member of
	// a visible group.
	for _, group := range []struct {
		name    string
		members []string
		want    bool
	}{
		{"asr-group", []string{"asr-live", "asr-lock"}, true},
		{"asr-group-mixed", []string{"asr-live", "asr-off"}, false},
	} {
		members := make([]routing.GroupMember, len(group.members))
		for i, m := range group.members {
			members[i] = routing.GroupMember{MemberGatewayName: m, Priority: i}
		}
		if err := srv.Routes.CreateModelGroup(ctx, routing.ModelGroup{ID: "grp-" + group.name, GatewayModelName: group.name, DisplayName: group.name, Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", group.name, err)
		}
		if err := srv.Routes.SetGroupMembers(ctx, "grp-"+group.name, members); err != nil {
			t.Fatalf("SetGroupMembers %s: %v", group.name, err)
		}
		requireTranscriptionGate(t, srv, group.name, group.want)
	}
}

// A blank name is a 400 (an empty name would otherwise read as "cannot stream"), the method
// is GET only and the portal's session auth applies.
func TestPortalModelTranscriptionStreamingRequestShape(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, _, _, _ := newSpeechRunTestServer(t, up.URL)
	addTranscriptionMapping(t, srv, "route-asr-live", "asr-live", ptr(true))

	for _, query := range []string{"name=", "name=%20%20", ""} {
		rec := askTranscriptionStreaming(srv, query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: %d, want 400", query, rec.Code)
		}
		requireErrorCode(t, rec.Body.String(), "model.name_required")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/portal/model-transcription-streaming?name=asr-live", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/portal/model-transcription-streaming?name=asr-live", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", rec.Code)
	}
}

// capabilityBatchFailingStore is the server's route store with the batch capability reader
// broken, as a store that has gone away would.
type capabilityBatchFailingStore struct{ routing.Store }

func (capabilityBatchFailingStore) MappingCapabilitiesForMappings(context.Context, []string) (map[string][]routing.CapabilityRow, error) {
	return nil, errors.New("capability store unavailable")
}

// A verdict that cannot be read is a 500 with its own stable code, never the "no" it would
// look like: reading a store failure as "cannot stream" would hide the fault behind a normal
// answer.
func TestPortalModelTranscriptionStreamingStoreFailureIsA500(t *testing.T) {
	up, _ := newSSESpeechUpstream(t, speechSSEDone, speechSSEEnd)
	srv, _, _, _ := newSpeechRunTestServer(t, up.URL)
	addTranscriptionMapping(t, srv, "route-asr-live", "asr-live", ptr(true))
	srv.Routes = capabilityBatchFailingStore{Store: srv.Routes}

	rec := askTranscriptionStreaming(srv, "name=asr-live")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "model.transcription_streaming_failed")
	if strings.Contains(rec.Body.String(), `"streaming"`) {
		t.Fatalf("a failed read answered a verdict: %s", rec.Body.String())
	}
}

// --- multi-part commit and the total cap (#279) ------------------------------
//
// A streamed clip is committed as ordered WAV parts, each cut at speechPartBytes of the
// per-blob cap, and a clip past the total-audio cap is the honest "audio too large" error.
// The reader builds the parts as the PCM arrives (it never holds the whole clip), but it
// drains the stream to [DONE] either way: the relay bills a streamed call on delivery whole.

const (
	// streamTestBlobCap makes speechPartBytes 1000: a part of 1000 PCM bytes is the unit
	// the tests count in, so "2.3 parts" is 2300 bytes.
	streamTestBlobCap  = 2000
	streamTestPartSize = 1000
	streamTestAudioCap = 5000
)

// streamTestServer is the Server consumeSpeechStream reads its two caps from. A bare
// &Server{} leaves them 0 (only New defaults them), so a test sets them itself.
func streamTestServer(audioCap, blobCap int) *Server {
	return &Server{maxChatAudioBytes: audioCap, maxChatBlobBytes: blobCap}
}

// drainTrackingReader records whether its source was read to the end, which is what
// "the stream was drained" means to the relay's billing.
type drainTrackingReader struct {
	src io.Reader
	eof bool
}

func (d *drainTrackingReader) Read(p []byte) (int, error) {
	n, err := d.src.Read(p)
	if err == io.EOF {
		d.eof = true
	}
	return n, err
}

// speechStreamOf is an event stream: one speech.audio.delta per chunk, then the tail frames.
func speechStreamOf(chunks [][]byte, tail ...string) string {
	var sb strings.Builder
	for _, chunk := range chunks {
		sb.WriteString(speechFrameDelta(chunk))
	}
	for _, frame := range tail {
		sb.WriteString(frame)
	}
	return sb.String()
}

// cutPCM cuts pcm into consecutive chunks of the given sizes, the rest going into a last
// chunk when the sizes do not cover it. A chunk may split a sample, as a delta can.
func cutPCM(pcm []byte, sizes ...int) [][]byte {
	var chunks [][]byte
	for _, n := range sizes {
		chunks, pcm = append(chunks, pcm[:n]), pcm[n:]
	}
	if len(pcm) > 0 {
		chunks = append(chunks, pcm)
	}
	return chunks
}

// streamRead is what readSpeechEvents made of one stream, with what a test needs to say
// about the reading itself: the audio relayed to the run's subscribers, how often the
// deadline was lifted, and whether the body was read to its end.
type streamRead struct {
	parts    []speechAudioPart
	overCap  bool
	complete bool
	err      error
	relayed  []runAudio
	lifted   int
	drained  bool
}

func readTestSpeechStream(body string, partBytes int, maxAudioBytes int64) streamRead {
	run := newChatRun("run_stream", "chat_stream", "usr_stream", func() {})
	_, events, unsub := run.subscribe()
	defer unsub()
	src := &drainTrackingReader{src: strings.NewReader(body)}
	var got streamRead
	got.parts, got.overCap, got.complete, got.err = readSpeechEvents(run, src, partsTestRate, time.Now(), func() { got.lifted++ }, partBytes, maxAudioBytes)
	got.drained = src.eof
	for len(events) > 0 {
		if ev := <-events; ev.Event == "audio" && ev.Audio != nil {
			got.relayed = append(got.relayed, *ev.Audio)
		}
	}
	return got
}

// partWAVs decodes the data: URL of every part into its WAV.
func partWAVs(t *testing.T, parts []speechAudioPart) [][]byte {
	t.Helper()
	wavs := make([][]byte, 0, len(parts))
	for i, part := range parts {
		if part.Type != speechAudioPartType {
			t.Fatalf("part %d has type %q, want %q", i, part.Type, speechAudioPartType)
		}
		payload, ok := strings.CutPrefix(part.AudioURL.URL, "data:audio/wav;base64,")
		if !ok {
			t.Fatalf("part %d: url %.60q is not a data:audio/wav;base64 url", i, part.AudioURL.URL)
		}
		wav, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("part %d: payload is not standard padded base64: %v", i, err)
		}
		wavs = append(wavs, wav)
	}
	return wavs
}

// consumeTestSpeechStream runs consumeSpeechStream over body on a server with the given caps.
func consumeTestSpeechStream(srv *Server, body io.Reader) (parts json.RawMessage, status, msg string, lifted int) {
	run := newChatRun("run_consume", "chat_consume", "usr_consume", func() {})
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}
	parts, status, msg = srv.consumeSpeechStream(context.Background(), run, resp, partsTestRate, time.Now(), func() { lifted++ })
	return parts, status, msg, lifted
}

// A clip of 2.3 parts is committed as three ordered audio_url parts -- two full ones and the
// remainder -- whose audio put end to end is the clip, however the deltas cut it (one of
// them splits a sample).
func TestStreamCommitsMultipleParts(t *testing.T) {
	pcm := speechPCM(streamTestPartSize*23/10, 3)
	body := speechStreamOf(cutPCM(pcm, 701, 699, 800), speechSSEDone, speechSSEEnd)

	raw, status, msg, lifted := consumeTestSpeechStream(streamTestServer(streamTestAudioCap, streamTestBlobCap), strings.NewReader(body))

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed with no message", status, msg)
	}
	var parts []speechAudioPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("the committed content is not a parts array: %v: %.120s", err, raw)
	}
	sizes, joined := dataSizes(t, partWAVs(t, parts))
	if want := []int{1000, 1000, 300}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the PCM the stream carried")
	}
	if lifted != 1 {
		t.Fatalf("the deadline was lifted %d times, want once (at the first delta)", lifted)
	}
}

// Over the total cap is an error the user can tell from a failure worth retrying, and it is
// reached only AFTER the stream has been read to its end: the relay bills a streamed call on
// delivery whole, and a read abandoned at the cap would make it a disconnect, billed zero.
// The reader says so itself -- it reached [DONE] (complete) and the body's EOF -- and the
// deadline was lifted once, as for any stream.
func TestStreamOverCapIsHonestAndDrained(t *testing.T) {
	pcm := speechPCM(streamTestAudioCap+1000, 5) // 6000 bytes against a cap of 5000
	body := speechStreamOf(cutPCM(pcm, 2000, 2000, 1000), speechSSEDone, speechSSEEnd)

	t.Run("the reader", func(t *testing.T) {
		got := readTestSpeechStream(body, streamTestPartSize, streamTestAudioCap)
		if !got.overCap {
			t.Fatal("a clip over the cap was not flagged")
		}
		if !got.complete || !got.drained {
			t.Fatalf("complete = %v, drained = %v: the reader stopped before the stream's terminal frames", got.complete, got.drained)
		}
		if len(got.parts) != 0 {
			t.Fatalf("an over-cap clip left %d parts held; nothing is committed, so none should be kept", len(got.parts))
		}
		if got.lifted != 1 {
			t.Fatalf("the deadline was lifted %d times, want once", got.lifted)
		}
	})

	t.Run("the outcome", func(t *testing.T) {
		src := &drainTrackingReader{src: strings.NewReader(body)}
		raw, status, msg, lifted := consumeTestSpeechStream(streamTestServer(streamTestAudioCap, streamTestBlobCap), src)
		if status != "error" || msg != speechRunAudioTooLargeMessage {
			t.Fatalf("outcome = %q %q, want error %q", status, msg, speechRunAudioTooLargeMessage)
		}
		if len(raw) != 0 {
			t.Fatalf("an over-cap clip committed parts: %.120s", raw)
		}
		if !src.eof {
			t.Fatal("the stream was not read to its end")
		}
		if lifted != 1 {
			t.Fatalf("the deadline was lifted %d times, want once", lifted)
		}
	})
}

// Past the cap the listener still hears the clip: every delta, the ones after the cap too,
// is relayed as it arrived, in an unbroken sequence. Only the SAVING stops.
func TestStreamOverCapKeepsRelayingLiveAudio(t *testing.T) {
	pcm := speechPCM(4000, 7)
	chunks := cutPCM(pcm, 1000, 1000, 1000, 1000)
	body := speechStreamOf(chunks, speechSSEDone, speechSSEEnd)

	got := readTestSpeechStream(body, streamTestPartSize, 1500) // crossed by the second delta

	if !got.overCap || !got.complete {
		t.Fatalf("overCap = %v, complete = %v, want both", got.overCap, got.complete)
	}
	if len(got.relayed) != len(chunks) {
		t.Fatalf("relayed %d audio events, want %d (one per delta, past the cap too)", len(got.relayed), len(chunks))
	}
	for i, ev := range got.relayed {
		if ev.Seq != i || ev.Rate != partsTestRate || ev.Data != base64.StdEncoding.EncodeToString(chunks[i]) {
			t.Fatalf("relayed event %d = seq %d rate %d, want seq %d rate %d carrying delta %d as it came", i, ev.Seq, ev.Rate, i, partsTestRate, i)
		}
	}
}

// A clip shorter than one part is one part, exactly the WAV the single-part turn has always
// committed: chunking changes nothing for a short answer.
func TestStreamShortClipOnePart(t *testing.T) {
	pcm := speechPCM(600, 11)
	body := speechStreamOf(cutPCM(pcm, 250, 250), speechSSEDone, speechSSEEnd)

	raw, status, msg, _ := consumeTestSpeechStream(streamTestServer(streamTestAudioCap, streamTestBlobCap), strings.NewReader(body))

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed", status, msg)
	}
	var parts []speechAudioPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("the committed content is not a parts array: %v", err)
	}
	wavs := partWAVs(t, parts)
	if len(wavs) != 1 {
		t.Fatalf("a short clip is %d parts, want 1", len(wavs))
	}
	if !bytes.Equal(wavs[0], wavFromPCM16(pcm, partsTestRate)) {
		t.Fatal("the one part is not wavFromPCM16's WAV of the clip")
	}
}

// Where the cuts fall: a clip that is an exact multiple of the part size has no empty
// trailing part; a stray half sample at the end is dropped and is never a part of its own
// (a header over no audio); and a clip exactly at the total cap is still stored -- the cap
// is crossed by one byte more, not met.
func TestStreamPartBoundariesAndCapEdge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pcmLen    int
		maxAudio  int64
		wantSizes []int
		overCap   bool
	}{
		{"an exact multiple has no empty tail", 2000, streamTestAudioCap, []int{1000, 1000}, false},
		{"a stray byte after full parts is dropped", 2001, streamTestAudioCap, []int{1000, 1000}, false},
		{"a stray byte ends the last part's data", 1501, streamTestAudioCap, []int{1000, 500}, false},
		{"exactly at the cap is stored", 2000, 2000, []int{1000, 1000}, false},
		{"one sample over the cap is not", 2002, 2000, nil, true},
		{"one byte over the cap is not", 2001, 2000, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := speechPCM(tc.pcmLen, 13)
			body := speechStreamOf(cutPCM(pcm, 333, 700), speechSSEDone, speechSSEEnd)

			got := readTestSpeechStream(body, streamTestPartSize, tc.maxAudio)

			if got.overCap != tc.overCap || !got.complete {
				t.Fatalf("overCap = %v, complete = %v, want %v and true", got.overCap, got.complete, tc.overCap)
			}
			if tc.overCap {
				return
			}
			sizes, joined := dataSizes(t, partWAVs(t, got.parts))
			if !slices.Equal(sizes, tc.wantSizes) {
				t.Fatalf("part data sizes = %v, want %v", sizes, tc.wantSizes)
			}
			if !bytes.Equal(joined, pcm[:len(pcm)&^1]) {
				t.Fatal("the parts' data chunks put end to end are not the PCM minus its stray byte")
			}
		})
	}
}

// What is not audio stays an error: a stream with no delta is the no-audio error, and so is
// one that carried less than a sample (a half sample is dropped, and a WAV over nothing
// would be stored and played as silence).
func TestStreamWithoutWholeSamplesIsNoAudio(t *testing.T) {
	for name, body := range map[string]string{
		"no deltas":         speechSSEDone + speechSSEEnd,
		"half a sample":     speechStreamOf([][]byte{{0x7f}}, speechSSEDone, speechSSEEnd),
		"empty delta audio": "data: {\"type\":\"speech.audio.delta\",\"audio\":\"\"}\n\n" + speechSSEDone + speechSSEEnd,
	} {
		t.Run(name, func(t *testing.T) {
			raw, status, msg, _ := consumeTestSpeechStream(streamTestServer(streamTestAudioCap, streamTestBlobCap), strings.NewReader(body))
			if status != "error" || msg != speechRunNoAudioMessage || len(raw) != 0 {
				t.Fatalf("outcome = %q %q %.60s, want error %q and no parts", status, msg, raw, speechRunNoAudioMessage)
			}
		})
	}
}

// A stream cut before its terminal frames is the unreadable-response error even when what
// arrived is already over the cap: the cap's verdict is about a whole clip, and this is a
// prefix of one.
func TestStreamCutBeforeItsEndIsUnreadableEvenWhenOverCap(t *testing.T) {
	body := speechStreamOf(cutPCM(speechPCM(4000, 17), 2000, 2000)) // no terminal frames

	raw, status, msg, _ := consumeTestSpeechStream(streamTestServer(1000, streamTestBlobCap), strings.NewReader(body))

	if status != "error" || msg != speechRunResponseUnreadableMessage || len(raw) != 0 {
		t.Fatalf("outcome = %q %q %.60s, want error %q and no parts", status, msg, raw, speechRunResponseUnreadableMessage)
	}
}

// The whole path: a long streamed clip reaches the committed turn as several audio parts, in
// order, through the real run, and the live relay carries every chunk.
func TestSpeechRunStreamLongClipIsCommittedAsOrderedParts(t *testing.T) {
	pcm := speechPCM(2300, 19)
	chunks := cutPCM(pcm, 900, 900)
	frames := make([]string, 0, len(chunks)+2)
	for _, chunk := range chunks {
		frames = append(frames, speechFrameDelta(chunk))
	}
	frames = append(frames, speechSSEDone, speechSSEEnd)
	srv, owner, chatID, _ := streamableSpeechRunServer(t, func(w http.ResponseWriter, _ *http.Request) { sseFrames(w, frames...) })
	srv.maxChatBlobBytes = streamTestBlobCap // parts of 1000 PCM bytes

	_, collect := subscribedSpeechRun(t, srv, owner, chatID, speechRunPrep())
	events := collect()

	done := events[len(events)-1]
	if done.Status != "completed" {
		t.Fatalf("status = %q (err %q), want completed", done.Status, done.Err)
	}
	if got := len(audioEventsOf(events)); got != len(chunks) {
		t.Fatalf("audio events = %d, want %d", got, len(chunks))
	}
	var parts []speechAudioPart
	if err := json.Unmarshal(done.ContentParts, &parts); err != nil {
		t.Fatalf("the terminal content is not a parts array: %v", err)
	}
	// The run streams at 44.1 kHz, not partsTestRate, so the parts are read here by their data
	// chunks alone (dataSizes' header check is for partsTestRate WAVs).
	var sizes []int
	var joined []byte
	for i, wav := range partWAVs(t, parts) {
		if string(wav[0:4]) != "RIFF" || binary.LittleEndian.Uint32(wav[24:]) != speechRunTestRate {
			t.Fatalf("part %d is not a WAV at %d Hz", i, speechRunTestRate)
		}
		sizes = append(sizes, len(wav)-wavHeaderLen)
		joined = append(joined, wav[wavHeaderLen:]...)
	}
	if want := []int{1000, 1000, 300}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the PCM the stream carried")
	}
}

// A part target that is not a whole number of samples, or is below one, is cut at whole
// samples and still terminates: the reader clamps it as the whole-clip splitter does.
func TestStreamPartTargetIsClampedToWholeSamples(t *testing.T) {
	pcm := speechPCM(6, 23)
	body := speechStreamOf(cutPCM(pcm, 3), speechSSEDone, speechSSEEnd)
	for _, partBytes := range []int{3, 2, 1, 0, -4} {
		got := readTestSpeechStream(body, partBytes, streamTestAudioCap)
		sizes, joined := dataSizes(t, partWAVs(t, got.parts))
		if want := []int{2, 2, 2}; !slices.Equal(sizes, want) || !bytes.Equal(joined, pcm) {
			t.Fatalf("partBytes %d: part data sizes = %v, want %v with the clip's own audio", partBytes, sizes, want)
		}
	}
}

// A Server that was not built by New has both caps at 0, which the executor reads as the
// defaults: an ordinary short clip is one committed part, not "over the cap" (an audio
// budget of 0) and not cut into one part per sample (a blob cap of 0).
func TestStreamOnABareServerUsesTheDefaultCaps(t *testing.T) {
	pcm := speechPCM(3000, 29)
	body := speechStreamOf(cutPCM(pcm, 1000, 1000), speechSSEDone, speechSSEEnd)

	raw, status, msg, _ := consumeTestSpeechStream(&Server{}, strings.NewReader(body))

	if status != "completed" || msg != "" {
		t.Fatalf("outcome = %q %q, want completed (a bare Server's caps are the defaults, not zero)", status, msg)
	}
	var parts []speechAudioPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("the committed content is not a parts array: %v", err)
	}
	wavs := partWAVs(t, parts)
	if len(wavs) != 1 || !bytes.Equal(wavs[0], wavFromPCM16(pcm, partsTestRate)) {
		t.Fatalf("a short clip is %d parts, want the one WAV of the clip", len(wavs))
	}
}

// A stream that will not be committed -- cut before its terminal frames, or one that lost a
// frame -- builds no tail part (it would be base64-encoded only to be discarded) and says
// nothing of a half sample being dropped. A stream that completes does, which the first case
// shows is observable at all.
func TestStreamUncommittedStreamBuildsNoTailPart(t *testing.T) {
	pcm := speechPCM(1501, 31) // one full part of 1000, a tail of 501 bytes
	deltas := speechStreamOf(cutPCM(pcm, 700, 700))
	for _, tc := range []struct {
		name         string
		body         string
		wantComplete bool
		wantParts    int
		wantLog      bool
	}{
		{"complete: the tail is a part and the stray byte is logged", deltas + speechSSEDone + speechSSEEnd, true, 2, true},
		{"cut before the terminal frames", deltas, false, 1, false},
		{"done but no [DONE]", deltas + speechSSEDone, false, 1, false},
		{"a delta that is not base64", deltas + "data: {\"type\":\"speech.audio.delta\",\"audio\":\"***\"}\n\n" + speechSSEDone + speechSSEEnd, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			got := readTestSpeechStream(tc.body, streamTestPartSize, streamTestAudioCap)

			if got.complete != tc.wantComplete || len(got.parts) != tc.wantParts {
				t.Fatalf("complete = %v with %d parts, want %v with %d", got.complete, len(got.parts), tc.wantComplete, tc.wantParts)
			}
			if said := strings.Contains(logged.String(), "ended inside a sample"); said != tc.wantLog {
				t.Fatalf("half-sample log = %v, want %v (log: %q)", said, tc.wantLog, logged.String())
			}
		})
	}
}
