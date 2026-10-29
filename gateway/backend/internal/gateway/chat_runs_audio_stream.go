// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// This file is the streaming half of the speech kind's run executor (ADR-068, the portal's
// TTS composer). The speech run of ADR-063 (chat_runs_audio.go) asks the gateway's own
// /v1/audio/speech for one complete WAV and commits it; this branch asks for a STREAM of
// the same synthesis instead, so the composer can play the audio while it is still being
// generated, and commits the same thing at the end -- the assembled audio, as WAV audio_url
// parts, which chat_blobs stores as it always has: one part for a clip that fits the
// per-blob cap and, for a longer one, several in order, each cut at speechPartBytes and a
// whole file of its own (chat_runs_audio_parts.go). It is the speech twin of the
// streaming transcription run (chat_runs_transcription.go) and reuses that run's plumbing:
// the reservation, the first-delta deadline (armRunDeadline / liftDeadline), the TTFT
// metric and the single terminal commit. It is not a second audio path: the request is the
// run's own /v1/audio/speech call, and the relay and its metering are the ones the public
// endpoint has.
//
// What differs from the transcription run is what a delta IS. A transcript delta is text
// that fills the turn in, so it is published into the run's text buffer; a speech delta is
// headerless PCM, which is no text and which a late subscriber could not use, so it goes to
// the run's subscribers as an `audio` event and into a local buffer from which the WAV
// parts are built, and never into the text buffer (and so never into a checkpoint -- a
// clip's prefix is not a turn worth persisting; a stream that does not complete commits
// nothing, as below).

// runAudio is one chunk of a streaming speech run's audio, the payload of an `audio` run
// event: the PCM of one speech.audio.delta, exactly as the stream carried it (base64, 16-bit
// little-endian mono), numbered so the client can tell a chunk its connection dropped from
// the end of the audio, and the sample rate to play it at -- a stream carries no format, and
// the client has none to read it from.
//
// A subscriber's channel drops events when it is full (fanoutLocked): harmless for text,
// which the next snapshot repairs, and an audible gap for audio. So the client plays the
// chunks only while their seq is unbroken and, at the first gap, stops live playback -- the
// terminal event carries the assembled WAV, which is complete whatever a subscriber missed.
type runAudio struct {
	Seq int `json:"seq"`
	// Section is the index of the spoken <speak> section this chunk belongs to, used ONLY to
	// attribute live audio to the right transcript block (NOT for ordering -- Seq is the one
	// monotonic order). A voice run with several sections tags each chunk with its section; a
	// standalone speech run leaves it 0 (and omits it on the wire). See the voice run executor.
	Section int    `json:"section,omitempty"`
	Rate    int    `json:"rate"`
	Data    string `json:"data"`
}

// publishAudio fans one audio chunk out to the run's subscribers. It buffers nothing: the
// run's own copy of the audio is the executor's, and a subscriber that joins late is served
// the terminal turn, not a replay.
func (r *ChatRun) publishAudio(a runAudio) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return
	}
	r.fanoutLocked(runEvent{Event: "audio", Audio: &a})
}

// speechStreamSampleRate bounds, in Hz, the sample rate the run will play a stream at. A
// rate outside it is not a speech model's, it is an arithmetic accident (a byte rate of
// another format, a corrupted cache row), and playing PCM at it would be noise.
const (
	speechStreamMinSampleRate = 8000
	speechStreamMaxSampleRate = 192000
)

// speechStreamBytesPerFrame is the size of one sample of the PCM audio.cpp streams: a
// streamed response is `pcm`, which is signed 16-bit little-endian MONO (OpenAI's
// definition of the format, and what the live contract measured: 44.1 kHz, mono, 16-bit
// for a model whose WAV byte rate is 88200). Only the sample rate varies from model to
// model; the cached output byte rate (sample rate x channels x bytes per sample) is
// therefore twice it.
const speechStreamBytesPerFrame = 2

// speechStreamSampleRate turns a cached output byte rate into the sample rate a stream is
// played at, and false when it cannot be one: a byte rate that is not a whole number of
// 16-bit mono frames a second, or one outside speechStreamMin/MaxSampleRate. The caller
// then does not stream -- it never guesses a rate, for the reason the billing path does not
// (ADR-041): a wrong one is played, and stored, as audio at the wrong speed.
func speechStreamSampleRate(byteRate uint32) (int, bool) {
	if byteRate == 0 || byteRate%speechStreamBytesPerFrame != 0 {
		return 0, false
	}
	rate := int(byteRate / speechStreamBytesPerFrame)
	if rate < speechStreamMinSampleRate || rate > speechStreamMaxSampleRate {
		return 0, false
	}
	return rate, true
}

// portalSpeechStreamRate is the gate of the portal's speech streaming: the sample rate the
// composer may play gatewayModel's stream at, and true, only when EVERY mapping a speech
// request for the model could be routed to (the portal-visible, OFFERABLE ones:
// portalOfferableSpeechMappings, the set the voice picker and the language control read, so
// since #301 a configured clone model with named presets is one of them) is
//
//   - (speech_streaming, yes): the gateway streams only a route its verdict says does, and
//     answers any other buffered; and
//   - has its output byte rate cached, and a usable one (speechStreamSampleRate); and
//   - agrees with the others on it.
//
// The rate is needed for two things the stream cannot supply: telling the client how to play
// headerless PCM, and the `fmt` chunk of the WAV the turn is committed as (the same rate the
// billing reads, ADR-068's rate-equality assumption). A route is picked per request, after
// this is asked, so a model with two mappings can be streamed only if it makes no difference
// which serves it -- hence the fold over all of them, the AND the portal's other capability
// flags use (fail closed), and the equality of their rates.
//
// The candidate set is the OFFERABLE one on purpose, and not plain tts alone: every mapping a
// speech request could be routed to is folded, so the gate opens only when they all agree on a
// rate. Since #306 the benchmark learns a byte rate for a preset clone too (it synthesizes with
// a configured preset voice), so a preset clone that streams and has an agreed rate STREAMS; a
// preset clone not yet benchmarked has no cached rate and closes the gate (buffered) until it
// is. This fold keeps a name that mixes a streaming plain tts mapping with a preset-clone
// sibling honest: it streams only if their rates agree, and otherwise buffers -- never a request
// routed to one sibling played, committed as a WAV and billed at the other's rate. A name that
// is only plain tts streams as before; a reference-only clone has no preset, so it is not
// offerable here at all and never reaches this gate. An empty candidate set is a closed gate. A
// store failure is an error and is never read as an open gate.
func (s *Server) portalSpeechStreamRate(ctx context.Context, principal auth.Token, gatewayModel string) (int, bool, error) {
	candidates, verdicts, err := s.portalOfferableSpeechMappings(ctx, principal, gatewayModel)
	if err != nil || len(candidates) == 0 {
		return 0, false, err
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, candidates, routing.ModelMappingCacheOutputByteRate)
	if err != nil {
		return 0, false, err
	}
	rate, ok := agreedSpeechStreamRate(candidates, verdicts, cached)
	return rate, ok, nil
}

// portalOfferableSpeechMappings resolves the mappings the portal may OFFER a speech request
// for gatewayModel on, the ones a speech request for it could be routed to: the ids
// Portal.ModelVoiceMappingIDs offers principal (the visibility the voice picker applies:
// hidden/locked suppression, group expansion, provisioning, active mappings of openai_audio
// applications), deduped and in the portal's sorted order, together with the capability rows
// it read, so a caller that needs a verdict does not read them again. A mapping stays when its
// CURRENT verdicts say routing.CapabilityRowsOfferableTTS given whether its voices cache lists
// a named voice: a plain tts model always, a clone model (#301) only with a named preset to
// pick (a reference-only clone synthesises only in a reference voice it is handed, so there is
// nothing to offer). It is the candidate set of the speech language control
// (portalSpeechLanguage) and of the streaming gate (portalSpeechStreamRate), the counterpart
// of what the voice picker offers (cachedVoicesForMappings).
//
// The streaming gate shares it ON PURPOSE: a preset clone it contains is a routable speech
// candidate, so folding it in makes the gate open only when every candidate agrees on a rate.
// Since #306 a preset clone has its own benchmarked stream rate, so it streams when the rates
// agree and buffers (fail closed) when they do not or when it is not yet benchmarked -- never a
// streamed request routed to a sibling played, stored and billed at another's rate.
//
// The voices cache is read, in one batch, for the speech mappings only (the others are
// offerable by no row of it), and only the scope-"" row decides (routing.VoiceNamesFromCacheRows,
// the one reader of its encoding). A row that does not decode counts as no named voice: it
// keeps a clone model out, as it does from the picker (which logs it), and costs a plain model
// nothing, since the voices decide a clone model alone.
//
// No candidate -- no routing store or portal, a blank or unknown name, a name the portal
// hides, a non-speech model, a clone model with nothing to pick -- is an empty slice and no
// error. A store failure is an error and is never read as that.
func (s *Server) portalOfferableSpeechMappings(ctx context.Context, principal auth.Token, gatewayModel string) ([]string, map[string][]routing.CapabilityRow, error) {
	if s.Routes == nil || s.Portal == nil || strings.TrimSpace(gatewayModel) == "" {
		return nil, nil, nil
	}
	ids, err := s.Portal.ModelVoiceMappingIDs(ctx, principal, gatewayModel)
	if err != nil {
		return nil, nil, err
	}
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	verdicts, err := s.Routes.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	speech := ids[:0:0]
	for _, id := range ids {
		if routing.CapabilityRowsSpeech(verdicts[id]) {
			speech = append(speech, id)
		}
	}
	if len(speech) == 0 {
		return nil, verdicts, nil
	}
	voices, err := s.Routes.ModelMappingCacheForMappings(ctx, speech, routing.ModelMappingCacheVoices)
	if err != nil {
		return nil, nil, err
	}
	candidates := speech[:0:0]
	for _, id := range speech {
		names, err := routing.VoiceNamesFromCacheRows(voices[id])
		if routing.CapabilityRowsOfferableTTS(verdicts[id], err == nil && len(names) > 0) {
			candidates = append(candidates, id)
		}
	}
	return candidates, verdicts, nil
}

// agreedSpeechStreamRate folds the candidate mappings into the one sample rate a stream
// may be played at, and true, only when every one of them has a speech_streaming verdict
// of yes, a usable cached output byte rate (scopeLessByteRate, speechStreamSampleRate),
// and the same rate as the others. Any mapping that fails any of the three closes the
// gate: (0, false). An empty candidate set is a closed gate too.
func agreedSpeechStreamRate(candidates []string, verdicts map[string][]routing.CapabilityRow, cached map[string][]routing.ModelMappingCacheRow) (int, bool) {
	sampleRate := 0
	for _, id := range candidates {
		if routing.CapabilityRowsByName(verdicts[id])[routing.CapabilitySpeechStreaming].Verdict != routing.CapabilityYes {
			return 0, false
		}
		rate, ok := scopeLessByteRate(cached[id])
		if !ok {
			return 0, false
		}
		hz, ok := speechStreamSampleRate(rate)
		if !ok || (sampleRate != 0 && hz != sampleRate) {
			return 0, false
		}
		sampleRate = hz
	}
	return sampleRate, sampleRate != 0
}

// scopeLessByteRate reads the usable output byte rate out of one mapping's cache rows: the
// scope-"" row, the one the speech benchmark writes (cachedOutputByteRate's rule over rows
// already in hand).
func scopeLessByteRate(rows []routing.ModelMappingCacheRow) (uint32, bool) {
	for _, row := range rows {
		if row.ScopeKey == "" {
			return routing.ParseOutputByteRate(row.Payload)
		}
	}
	return 0, false
}

// speechStreamOptedOut reports whether a thread's ChatAudioSettings.Stream says NOT to stream: an
// explicit false. Unset (nil) is not an opt-out -- the model's own capability decides then. It is
// the one place that reads the toggle, so the run's streaming gate (speechRunStreamRate) and the
// voice relay's gap silence (relayVoiceSections, a streaming-only feature) cannot disagree.
func speechStreamOptedOut(stream *bool) bool {
	return stream != nil && !*stream
}

// speechRunStreamRate decides whether this run streams, and at what sample rate. It is the
// run-time half of the gate (the composer's is GET /api/portal/model-speech-streaming): a
// thread that opted out (ChatAudioSettings.Stream false) is buffered, and so is one whose
// model fails portalSpeechStreamRate -- evaluated here again, never taken from the client,
// because the client's toggle is a choice and not an authority, and the cache can change
// between the composer's question and the send.
//
// The model asked about is the one the request will ROUTE for, which is not always the
// thread's: the run-as token's model override rewrites the requested name (resolveModelOverride,
// as inferencePreflight applies it to the loopback request). A gate that read the thread's
// name would judge a different model than the one that synthesizes.
//
// Every failure to ask -- an unauthorized run-as token, a store error -- closes the gate and
// is logged: the run is then the buffered one, which the loopback request will refuse for
// the same reasons if it must, in its own code.
func (s *Server) speechRunStreamRate(ctx context.Context, owner auth.Token, prep PrepareRunResult) (int, bool) {
	if audio := prep.Settings.Audio; audio != nil && speechStreamOptedOut(audio.Stream) {
		return 0, false
	}
	model := prep.Settings.Model
	if id := prep.Settings.RunAsTokenID; id != "" {
		if s.Portal == nil {
			return 0, false
		}
		runAs, err := s.Portal.AuthorizeRunAsToken(ctx, owner, id)
		if err != nil {
			slog.Debug("speech run: the run-as token is not usable; the run is buffered", "err", err)
			return 0, false
		}
		model = resolveModelOverride(ctx, runAs, model, s.Portal)
	}
	rate, ok, err := s.portalSpeechStreamRate(ctx, owner, model)
	if err != nil {
		slog.Warn("speech run: the streaming gate could not be read; the run is buffered", "model", model, "err", err)
		return 0, false
	}
	return rate, ok
}

// handlePortalModelSpeechStreaming answers whether the portal's speech composer may offer
// streaming for a gateway model (?name=<model>, the model the run will route for): {"streaming":
// true,"sample_rate":<Hz>} when portalSpeechStreamRate's gate is open, {"streaming":false}
// otherwise. It is the composer's gate (the run decides again, speechRunStreamRate), the
// twin of GET /api/portal/model-voices in everything but its answer: the same session-cookie
// auth (requireWebScope, gateway:use), the same name-as-a-query-parameter, the same blank-name
// 400 (model.name_required, for the reason an empty name would otherwise read as "cannot
// stream"), and the same no-leak rule -- every "no" (an unknown, hidden, locked or restricted
// model, a non-speech or clone one, one not yet benchmarked) is the same 200, so the answer
// cannot probe which models exist. A store failure is the only 500, never read as a "no".
// Nothing here loads a model or contacts an upstream.
func (s *Server) handlePortalModelSpeechStreaming(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, "model name is required", ""))
		return
	}
	rate, streaming, err := s.portalSpeechStreamRate(r.Context(), token, name)
	if err != nil {
		slog.Error("portal speech streaming lookup failed", "model", name, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response(portalSpeechStreamingFailed, "speech streaming lookup failed", ""))
		return
	}
	if !streaming {
		writeJSON(w, http.StatusOK, portalSpeechStreamingResponse{})
		return
	}
	writeJSON(w, http.StatusOK, portalSpeechStreamingResponse{Streaming: true, SampleRate: rate})
}

// portalSpeechStreamingFailed is the code of a failed read of the gate (API surface).
const portalSpeechStreamingFailed = "model.speech_streaming_failed"

// portalSpeechStreamingResponse is the body of GET /api/portal/model-speech-streaming.
// sample_rate is present only with streaming: a "no" carries nothing to learn from.
type portalSpeechStreamingResponse struct {
	Streaming  bool `json:"streaming"`
	SampleRate int  `json:"sample_rate,omitempty"`
}

// portalTranscriptionStreams is the gate of the portal's live transcription: whether the
// record mode may be offered for gatewayModel, which is the case only when EVERY mapping a
// transcription request for the model could be routed to is known to stream. Those are the
// portal-visible mappings (Portal.ModelTranscriptionMappingIDs, the visibility the voice
// picker and the speech gate apply too) that carry a (transcription, yes) verdict -- the
// capability gate every transcription request passes (transcriptionInferenceShape), so a
// speech mapping of the same name is no candidate -- and each of them must say
// (transcription_streaming, yes): the verdict the ticket endpoint requires of the one
// mapping it resolves (mappingStreamsTranscription). A route is picked per request, after
// this is asked, so a model with two mappings streams only if it makes no difference which
// serves it: the AND the portal's other capability flags use (fail closed). A no and an
// absent row, which is "unknown", are the same closed gate, and so is an empty candidate set.
//
// The verdicts are read with the batch reader and not mappingStreamsTranscription's keyed
// one, which answers a failed read as a "no": a store failure here is an error, so the
// handler can tell it from a closed gate and is never read as one.
//
// This is the composer's question and not the session's authority: the ticket endpoint
// asks again, with the admission and the routing a live session needs, when one is opened.
func (s *Server) portalTranscriptionStreams(ctx context.Context, principal auth.Token, gatewayModel string) (bool, error) {
	if s.Routes == nil || s.Portal == nil || strings.TrimSpace(gatewayModel) == "" {
		return false, nil
	}
	ids, err := s.Portal.ModelTranscriptionMappingIDs(ctx, principal, gatewayModel)
	if err != nil {
		return false, err
	}
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return false, nil
	}
	verdicts, err := s.Routes.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return false, err
	}
	candidates := 0
	for _, id := range ids {
		byName := routing.CapabilityRowsByName(verdicts[id])
		if byName[routing.CapabilityTranscription].Verdict != routing.CapabilityYes {
			continue
		}
		if byName[routing.CapabilityTranscriptionStreaming].Verdict != routing.CapabilityYes {
			return false, nil
		}
		candidates++
	}
	return candidates > 0, nil
}

// handlePortalModelTranscriptionStreaming answers whether the portal may offer the live
// record mode for a gateway model (?name=<model>): {"streaming":true} when
// portalTranscriptionStreams' gate is open, {"streaming":false} otherwise. It is
// handlePortalModelSpeechStreaming's twin in everything but its answer, which carries no
// sample rate: the live client fixes its own audio format and the ticket endpoint validates
// it. The same session-cookie auth (requireWebScope, gateway:use), the same name-as-a-query-
// parameter, the same blank-name 400 (model.name_required: an empty name would otherwise
// read as "cannot stream") and the same no-leak rule -- every "no" (an unknown, hidden,
// locked or restricted model, a non-transcription one, an offline one, one whose mode no
// probe has stated) is the same 200, so the answer cannot probe which models exist. A store
// failure is the only 500, never read as a "no". Nothing here loads a model, runs an
// admission or contacts an upstream.
func (s *Server) handlePortalModelTranscriptionStreaming(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, "model name is required", ""))
		return
	}
	streaming, err := s.portalTranscriptionStreams(r.Context(), token, name)
	if err != nil {
		slog.Error("portal transcription streaming lookup failed", "model", name, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response(portalTranscriptionStreamingFailed, "transcription streaming lookup failed", ""))
		return
	}
	writeJSON(w, http.StatusOK, portalTranscriptionStreamingResponse{Streaming: streaming})
}

// portalTranscriptionStreamingFailed is the code of a failed read of the live gate (API
// surface).
const portalTranscriptionStreamingFailed = "model.transcription_streaming_failed"

// portalTranscriptionStreamingResponse is the body of GET
// /api/portal/model-transcription-streaming. It carries the verdict alone: unlike the
// speech gate's there is no rate to learn.
type portalTranscriptionStreamingResponse struct {
	Streaming bool `json:"streaming"`
}

// buildSpeechStreamBody is buildSpeechBody for a run that streams: the same body, plus the
// two fields only a stream has a meaning for. stream is the literal true; stream_format is
// "sse", audio.cpp's own default, said outright because the run reads an event stream and
// would not parse the raw PCM the other format answers with. response_format is NOT stated:
// a stream is pcm, which is also what the endpoint takes when it is absent
// (validateSpeechRequest), so nothing is left to choose.
func buildSpeechStreamBody(prep PrepareRunResult) ([]byte, error) {
	fields, err := speechBodyFields(prep)
	if err != nil {
		return nil, err
	}
	fields["stream"] = true
	fields["stream_format"] = speechStreamFormatSSE
	return json.Marshal(fields)
}

// speechStreamEvent is the part of a speech.audio.* frame the run reads.
type speechStreamEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// speechStreamRead carries consumeSpeechStream's per-call reading inputs -- everything
// beyond the context, the run, and the response: the sample rate, the dispatch instant and
// the deadline lift that sit behind the TTFT, whether this leg stamps the TTFT, and the
// optional per-chunk audio sink.
type speechStreamRead struct {
	sampleRate   int
	dispatchedAt time.Time
	liftDeadline func()
	stampTTFT    bool
	audioSink    func(runAudio)
}

// consumeSpeechStream reads the gateway's event stream of a streamed synthesis and returns
// the turn's outcome, the triple generateSpeechTurn returns. It is
// consumeTranscriptionStream's twin (see there for the shape of the reading): the first
// delta is the moment the run has something to show, so the deadline is lifted and the TTFT
// stamped, from the dispatch of the request (a cold start is in it); every delta's PCM is
// relayed to the run's subscribers and cut into the WAV parts of the turn as it arrives; and
// the stream is read on to its end after speech.audio.done, because the gateway's relay
// bills the call on having delivered the stream whole, and a client that hangs up at the
// done frame would make it a client that disconnected.
//
// The turn is committed only for a stream that ENDED the way a whole synthesis does: its
// speech.audio.done and then [DONE] (the terminal frames the relay's own billing requires
// too) with audio before them. A stream that ends without them is a prefix of the clip -- cut
// by the upstream's death or the gateway's idle timeout -- and committing it would present a
// cut clip as the reply: an ended context is reported as itself (a deadline is not a cancel),
// anything else is the unreadable-response error, and nothing is committed. An empty
// synthesis is the no-audio error, not a blank bubble.
//
// A clip longer than the run's total audio cap (Server.maxChatAudioBytes) is the
// audio-too-large error, and it is decided only here, once the stream is whole: the reader
// stops KEEPING audio at the cap but not READING it, so the listener hears the whole clip
// and the call is billed as delivered. The parts are cut at speechPartBytes of the per-blob
// cap (Server.maxChatBlobBytes) so that each passes the chat store's per-blob check alone.
func (s *Server) consumeSpeechStream(ctx context.Context, run *ChatRun, resp *http.Response, r speechStreamRead) (json.RawMessage, string, string) {
	audioCap, blobCap := s.chatAudioCaps()
	parts, overCap, complete, readErr := readSpeechEvents(run, resp.Body, speechEventReaderConfig{
		sampleRate:    r.sampleRate,
		dispatchedAt:  r.dispatchedAt,
		liftDeadline:  r.liftDeadline,
		partBytes:     speechPartBytes(blobCap),
		maxAudioBytes: audioCap,
		stampTTFT:     r.stampTTFT,
		audioSink:     r.audioSink,
	})
	if !complete {
		// A deadline or a cancel lands here as a read error on the cancelled request, and
		// reporting either as a broken stream -- or a deadline as a cancel -- would tell the
		// user the upstream failed when it was they, or the clock, that ended it.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		// The frames are not logged: they are the audio.
		log.Printf("chat run %s: the speech stream ended before its terminal events (read error: %t, over the audio cap: %t)", run.ID, readErr != nil, overCap)
		return nil, "error", speechRunResponseUnreadableMessage
	}
	if overCap {
		log.Printf("chat run %s: the speech stream is longer than the %d byte audio cap; nothing is committed", run.ID, audioCap)
		return nil, "error", speechRunAudioTooLargeMessage
	}
	// No parts is no audio -- also for a stream whose only PCM was less than one sample --
	// and is refused BEFORE the commit: an empty content array would be a blank bubble.
	if len(parts) == 0 {
		return nil, "error", speechRunNoAudioMessage
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		log.Printf("chat run %s: marshal speech parts failed: %v", run.ID, err)
		return nil, "error", speechRunDispatchFailedMessage
	}
	return raw, "completed", ""
}

// readSpeechEvents reads a streamed synthesis' event stream to its end. Every
// speech.audio.delta's PCM is relayed, still as the base64 the stream carried it in, to the
// run's subscribers (see runAudio) and cut into WAV parts of partBytes of PCM (a part is
// built the moment its PCM is complete, and its PCM released, so the clip is never held
// whole -- only its finished parts are); the first delta lifts the deadline and stamps the
// TTFT. It returns the parts, whether the clip went over maxAudioBytes of PCM, whether the
// stream completed (speech.audio.done, then [DONE], neither before the other) and the
// scanner's read error.
//
// Going over the cap stops the KEEPING of audio and nothing else: the parts held so far are
// dropped (nothing is committed for such a clip), but every later line is still read and
// every later delta still relayed, so the stream is drained to [DONE] and the relay bills it
// as the delivered call it was. A partBytes that is not a whole number of samples is cut at
// the sample below it, and one below a sample is a sample.
//
// A frame that is JSON in shape and cannot be read, and a delta whose audio is not base64,
// each make the audio incomplete: the parts would then hold less than the client was
// relayed, so the stream is NOT complete however it ends. The line bound is the relay's own
// (speechStreamMaxLine): a line past it ends the read (bufio.ErrTooLong), which is the same
// incomplete stream.
// speechEventReaderConfig groups readSpeechEvents' reader inputs beyond the stream itself
// (run and body): the sample rate, the dispatch instant the TTFT is measured from, the
// deadline lift, the per-part and total PCM caps, and whether this reader stamps the TTFT.
// These are the fields the speechEventReader is built from.
type speechEventReaderConfig struct {
	sampleRate    int
	dispatchedAt  time.Time
	liftDeadline  func()
	partBytes     int
	maxAudioBytes int64
	stampTTFT     bool
	// audioSink relays one chunk of synthesized audio. A standalone speech run leaves it nil and
	// the audio is published straight onto the run (run.publishAudio); a VOICE run's section leg
	// supplies a sink that routes the chunk through its ordered, section-serialized relay so the
	// client sees one monotonic stream across all sections (see the voice run executor).
	audioSink func(runAudio)
}

func readSpeechEvents(run *ChatRun, body io.Reader, cfg speechEventReaderConfig) (parts []speechAudioPart, overCap, complete bool, readErr error) {
	sink := cfg.audioSink
	if sink == nil {
		sink = run.publishAudio
	}
	r := &speechEventReader{
		run: run, sampleRate: cfg.sampleRate, dispatchedAt: cfg.dispatchedAt, liftDeadline: cfg.liftDeadline,
		partBytes:     max(cfg.partBytes-cfg.partBytes%speechStreamBytesPerFrame, speechStreamBytesPerFrame),
		maxAudioBytes: cfg.maxAudioBytes,
		stampTTFT:     cfg.stampTTFT,
		audioSink:     sink,
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), speechStreamMaxLine)
	for scanner.Scan() {
		r.consumeLine(scanner.Text())
	}
	r.flushTail()
	return r.parts, r.overCap, r.complete(), scanner.Err()
}

// speechEventReader is what reading a streamed synthesis' event stream (readSpeechEvents)
// accumulates, one line at a time: the clip as the finished WAV parts so far plus the PCM of
// the one being filled, how much PCM the clip has had in all and whether that went over the
// cap, the next audio event's sequence number, and the facts that decide whether the stream
// completed -- whether a delta has been seen (the first lifts the deadline and stamps the
// TTFT), a speech.audio.done, a [DONE] that came after it, and whether any frame or delta
// was lost to a read failure.
type speechEventReader struct {
	run          *ChatRun
	sampleRate   int
	dispatchedAt time.Time
	liftDeadline func()
	// audioSink relays one audio chunk (defaulting to run.publishAudio for a standalone speech
	// run; a voice section leg supplies its ordered sink). Never nil: readSpeechEvents fills it.
	audioSink func(runAudio)
	// stampTTFT is whether the first delta stamps the audio TTFT onto the run's metrics.
	// A standalone speech run (its only metric is the audio first-chunk) sets it; a VOICE
	// run's speech leg does NOT, because its LLM leg is streaming into the same run.metrics
	// concurrently and the two cross-call read-modify-writes would clobber each other's
	// fields (a non-deterministic persisted TTFTMs/tps). For a voice run the LLM leg is thus
	// the sole metrics writer, so the persisted perf metrics stay deterministic. The deadline
	// lift below is unconditional -- it is how the run learns the first chunk arrived.
	stampTTFT bool

	// partBytes is the PCM one part holds, in whole samples (so a cut of it is one too);
	// maxAudioBytes is the most PCM the clip may have in all.
	partBytes     int
	maxAudioBytes int64

	// parts are the finished parts, in order, and segment the PCM of the next one, always
	// under partBytes between deltas. total counts every byte of PCM seen until overCap,
	// from which point nothing is counted or kept (parts and segment are nil).
	parts   []speechAudioPart
	segment []byte
	total   int64
	overCap bool

	sawDelta, sawDone, sawEnd, lost bool
	seq                             int
}

// consumeLine takes one line of the event stream. Only an SSE data line carries
// anything: [DONE] counts as the end only once speech.audio.done has been seen, a
// payload that is not a JSON object is ignored, and one that is but cannot be read
// loses the frame (lost).
func (r *speechEventReader) consumeLine(line string) {
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
	if !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == sseDoneSentinel {
		r.sawEnd = r.sawEnd || r.sawDone
		return
	}
	if !strings.HasPrefix(payload, "{") {
		return
	}
	var ev speechStreamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		r.lost = true
		return
	}
	switch ev.Type {
	case speechEventDelta:
		r.consumeDelta(ev.Audio)
	case speechEventDone:
		r.sawDone = true
	}
}

// consumeDelta takes one speech.audio.delta's base64 audio: an empty one is skipped, one
// that is not base64 is lost, and any other is relayed to the run's subscribers as it came
// and kept (keep) for the WAV parts. The first lifts the deadline and stamps the TTFT. The
// relay does not depend on the keeping: a clip past the cap is still played to its end.
func (r *speechEventReader) consumeDelta(audio string) {
	if audio == "" {
		return
	}
	chunk, err := base64.StdEncoding.DecodeString(audio)
	if err != nil {
		r.lost = true
		return
	}
	if !r.sawDelta {
		r.sawDelta = true
		r.liftDeadline()
		if r.stampTTFT {
			m := r.run.currentMetrics()
			m.TTFTMs = time.Since(r.dispatchedAt).Milliseconds()
			r.run.setMetrics(m)
		}
	}
	r.audioSink(runAudio{Seq: r.seq, Rate: r.sampleRate, Data: audio})
	r.seq++
	r.keep(chunk)
}

// keep adds one delta's PCM to the clip being cut into parts. Once the clip is over the
// cap it keeps nothing, and the parts held so far -- of a clip that will not be committed --
// are released at the moment it crosses. Otherwise it appends to the part being filled and
// finishes every part that is now full, so no more than one part's PCM (and a delta) is
// held beside the finished parts.
func (r *speechEventReader) keep(chunk []byte) {
	if r.overCap {
		return
	}
	r.total += int64(len(chunk))
	if r.total > r.maxAudioBytes {
		r.overCap = true
		r.parts, r.segment = nil, nil
		return
	}
	r.segment = append(r.segment, chunk...)
	for len(r.segment) >= r.partBytes {
		r.finishPart(r.segment[:r.partBytes])
		r.segment = r.segment[r.partBytes:]
	}
}

// complete reports whether the stream ended the way a whole synthesis does: its
// speech.audio.done and then [DONE], with no frame or delta lost to a read failure.
func (r *speechEventReader) complete() bool {
	return r.sawDone && r.sawEnd && !r.lost
}

// flushTail finishes the part of the PCM left when the stream ends, if there is a whole
// sample in it: a clip that is an exact multiple of the part size has no empty last part,
// and a stray half sample at the very end is dropped (wavFromPCM16 drops it from the part it
// ends) and is never a part of its own, a header over no audio. A stream that is not
// complete, or is over the cap, is not committed, so it builds no tail part and says nothing
// of a half sample.
func (r *speechEventReader) flushTail() {
	if r.overCap || !r.complete() {
		return
	}
	if r.total%speechStreamBytesPerFrame != 0 {
		log.Printf("chat run %s: the speech stream ended inside a sample (%d PCM bytes); the last byte is dropped", r.run.ID, r.total)
	}
	if len(r.segment) >= speechStreamBytesPerFrame {
		r.finishPart(r.segment)
	}
	r.segment = nil
}

// finishPart turns whole samples of PCM into the next part of the clip. A part that cannot
// be built is audio the clip lacks, so it makes the stream incomplete (lost) like any other
// frame that was lost; it cannot happen for a non-empty WAV of the one fixed media type.
func (r *speechEventReader) finishPart(pcm []byte) {
	part, err := speechPartFrom(speechAudioMediaType, wavFromPCM16(pcm, r.sampleRate))
	if err != nil {
		log.Printf("chat run %s: a part of the speech stream is not usable audio: %v", r.run.ID, err)
		r.lost = true
		return
	}
	r.parts = append(r.parts, part)
}

// wavHeaderLen is the size of the canonical 44-byte PCM WAV header: RIFF, `fmt ` (16 bytes
// of body) and the `data` chunk header.
const wavHeaderLen = 44

// wavFromPCM16 wraps headerless 16-bit little-endian mono PCM in the canonical WAV header
// for sampleRate, the container the turn is committed in (and the one the buffered run
// commits audio.cpp's own in, so everything downstream -- the blob store's audio allow-list,
// the player, the download -- sees the same type). The data is written in whole samples: a
// trailing odd byte is half of one and is dropped, so the data chunk's size is even and the
// header never claims a sample the data does not hold.
func wavFromPCM16(pcm []byte, sampleRate int) []byte {
	pcm = pcm[:len(pcm)-len(pcm)%speechStreamBytesPerFrame]
	const channels, bitsPerSample = 1, 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	var buf bytes.Buffer
	buf.Grow(wavHeaderLen + len(pcm))
	le := binary.LittleEndian
	put := func(v any) { _ = binary.Write(&buf, le, v) }
	buf.WriteString("RIFF")
	put(uint32(36 + len(pcm)))
	buf.WriteString("WAVEfmt ")
	put(uint32(16)) // fmt chunk body
	put(uint16(1))  // PCM
	put(uint16(channels))
	put(uint32(sampleRate))
	put(uint32(byteRate))
	put(uint16(channels * bitsPerSample / 8)) // block align
	put(uint16(bitsPerSample))
	buf.WriteString("data")
	put(uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
