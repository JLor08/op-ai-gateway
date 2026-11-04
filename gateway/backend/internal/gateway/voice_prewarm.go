// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"time"
)

// This file is the Voice Chat session-open TTS pre-warm's synthesis (#342). A TTS model pays two
// one-time costs on its first speech -- building its compute graph and encoding a voice's
// reference -- and the first spoken reply of a session then waits for both (seconds, against tens
// of milliseconds warm). The ticket endpoint knows the session's TTS target and chosen voice the
// moment the co-load gate passes, so it fires one short synthesis for that voice right then
// (primeSpeechVoice), overlapping the cost with the user getting ready instead of with the first
// turn.
//
// The synthesis must never be billed and never be stored. It is therefore NOT a request to the
// gateway's own /v1/audio/speech (the loopback the portal's speech run uses, speechTurnAttempt):
// that relay records a usage row for the call. It is sent straight to the target's upstream
// through the provider's native relay (provider.NativeProxyClient.ProxyNative), exactly as the
// speech benchmark (benchmark_speech.go, measureSpeechTarget) sends its clip -- which writes no
// usage row either -- and the answer is read and thrown away: nothing is committed, so nothing is
// stored. The dispatch is mirrored there, not shared: the benchmark's version is entangled with
// its measurement (the time to first byte is taken around the call, its errors are phrased as a
// benchmark result) and a pre-warm is not worth a change to that path. What the two do share is
// the pieces that define the request itself -- the body type (speechBenchmarkRequest), the
// upstream path (upstreamPath for the speech flavor), the content type and the credential
// (upstreamAuthCtx) -- and the reading of the answer (readBenchmarkRefusal, readBenchmarkAnswer).
//
// Unbilled is not invisible: while it runs the synthesis is an in-flight request on the target's
// instance (an ActiveRequest on its InstanceKey), so the per-instance cap counts it and routing does
// not send a session's first turn to an instance busy priming as if it were free.

// voicePrimingText is what the priming synthesis speaks: a few words, enough for the model to run
// its whole synthesis path once and short enough that the clip is quick. It is fixed so every
// session's priming is the same request.
const voicePrimingText = "Hello, I'm ready."

// voicePrimingTimeout bounds one priming synthesis. The caller (the ticket endpoint's detached
// goroutine) puts it on a background context -- never the HTTP request's, which ends when the
// ticket response returns. It covers a cold TTS voice (seconds to roughly eleven) with margin, and
// ends a hung upstream instead of holding a goroutine for ever.
const voicePrimingTimeout = 30 * time.Second

// errVoicePrimingNoNativeProxy is primeSpeechVoice's refusal when the provider cannot forward a raw
// request body to an upstream: there is no direct (unbilled) route to take, and the billing
// loopback is deliberately not a fallback.
var errVoicePrimingNoNativeProxy = errors.New("voice prewarm: provider does not support native passthrough")

// voicePrewarmEnabled reports whether the session-open TTS pre-warm is on (config
// OP_AI_GATEWAY_VOICE_PREWARM, carried through ServerDeps.VoicePrewarmEnabled). A bare or nil
// Server reads as off.
func (s *Server) voicePrewarmEnabled() bool {
	return s != nil && s.voicePrewarm
}

// startVoicePrewarm is the ticket endpoint's one call into the pre-warm: it launches the priming of tts
// and voice (the speech target the co-load gate resolved and the voice the ticket carries) in the
// background and returns at once. It is gated -- nothing starts with the knob off or with no voice
// to warm -- and best-effort in every other respect: the synthesis is never awaited, so the ticket's
// latency and success cannot depend on it, and its failure (no native relay, a transport error, a
// non-2xx answer, the timeout) is logged here and goes no further, so it can never fail the ticket
// or the session that opens on it.
//
// The goroutine runs on its OWN context, derived from context.Background() and bounded by
// voicePrimingTimeout, and never on the request's, for two reasons. The request's context ends the
// moment the ticket response is written, which would cancel the synthesis before the upstream has
// done any of the work being warmed. And it carries the request's state -- among it a
// payload-capture sink, which would record the priming's request and response and so store them,
// where the priming must store nothing. A clean context carries none of that.
func (s *Server) startVoicePrewarm(tts routing.Target, voice string) {
	if !s.voicePrewarmEnabled() || voice == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), voicePrimingTimeout)
		defer cancel()
		if err := s.primeSpeechVoice(ctx, tts, voice); err != nil {
			slog.Warn("voice prewarm: priming synthesis failed", "model", tts.Model, "route", tts.RouteID, "voice", voice, "err", err)
		}
	}()
}

// primeSpeechVoice fires ONE short synthesis for tts and voice straight at the target's upstream, to
// make its first real synthesis warm. It reads the answer to its end and discards it, and returns
// nil on a 2xx answer with audio in it; any failure (no native relay, a transport error, a non-2xx
// answer, an unreadable, empty or oversized body) is returned for the caller to log -- priming is
// best-effort and must never fail the ticket or the session, so there is nothing here to retry or
// recover. From the call to the end of the answer it is an in-flight request on tts's instance.
//
// It runs on ctx as given: the caller bounds it (voicePrimingTimeout) and ends it. It writes no
// usage row and stores nothing (see the file comment): the one request is a plain non-stream
// synthesis naming the application's own model id -- what the speech relay would rewrite the
// client's name to (effectiveProviderModel) -- the fixed priming text and the one voice, and
// nothing else.
func (s *Server) primeSpeechVoice(ctx context.Context, tts routing.Target, voice string) error {
	proxy, ok := s.Provider.(provider.NativeProxyClient)
	if !ok {
		return errVoicePrimingNoNativeProxy
	}
	body, err := json.Marshal(speechBenchmarkRequest{Model: effectiveProviderModel(tts, tts.Model), Input: voicePrimingText, Voice: voice})
	if err != nil {
		return fmt.Errorf("voice prewarm: build request: %w", err)
	}
	ctx = s.upstreamAuthCtx(ctx, tts)
	path := upstreamPath(tts, apiFlavorSpeech, endpointSpeech)

	id := nextRequestID()
	s.Active.Add(ActiveRequest{
		ID: id, ServerName: s.serverName(tts.ServerID), ServerID: tts.ServerID, InstanceKey: tts.InstanceKey,
		Model: tts.Model, APIFlavor: apiFlavorSpeech, BillingUnit: billingUnitFor(apiFlavorSpeech),
		ProviderPath: path, ProviderModel: effectiveProviderModel(tts, tts.Model), StartedAt: time.Now(),
	})
	defer s.Active.Remove(id)

	resp, err := proxy.ProxyNative(ctx, tts, path, body, jsonContentType)
	if err != nil {
		return fmt.Errorf("voice prewarm: synthesis request failed: %w", err)
	}
	defer resp.Body.Close()

	if refusal, refused := readBenchmarkRefusal(resp); refused {
		return fmt.Errorf("voice prewarm: the upstream answered HTTP %d: %s", refusal.status, refusal.text)
	}
	// The clip is read to its end because the upstream's synthesis is the point, then dropped; the
	// benchmark's bound keeps a runaway answer from accumulating, and ctx bounds the time.
	if _, _, err := readBenchmarkAnswer(resp.Body, speechBenchmarkMaxAudioBytes, time.Now); err != nil {
		return fmt.Errorf("voice prewarm: %w", err)
	}
	return nil
}
