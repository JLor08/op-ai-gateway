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
// generated, and commits the same thing at the end -- the assembled WAV, as the same one
// audio_url part, which chat_blobs stores as it always has. It is the speech twin of the
// streaming transcription run (chat_runs_transcription.go) and reuses that run's plumbing:
// the reservation, the first-delta deadline (armRunDeadline / liftDeadline), the TTFT
// metric and the single terminal commit. It is not a second audio path: the request is the
// run's own /v1/audio/speech call, and the relay and its metering are the ones the public
// endpoint has.
//
// What differs from the transcription run is what a delta IS. A transcript delta is text
// that fills the turn in, so it is published into the run's text buffer; a speech delta is
// headerless PCM, which is no text and which a late subscriber could not use, so it goes to
// the run's subscribers as an `audio` event and into a local buffer from which the WAV is
// built, and never into the text buffer (and so never into a checkpoint -- a clip's prefix is
// not a turn worth persisting; a stream that does not complete commits nothing, as below).

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
	Seq  int    `json:"seq"`
	Rate int    `json:"rate"`
	Data string `json:"data"`
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
// request for the model could be routed to (the portal-visible, plain-tts ones, which is
// what the voice picker asks too: Portal.ModelVoiceMappingIDs) is
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
// A mapping that is not plain tts (not a speech model at all, or a clone one) is not a
// candidate: a speech request is never routed to it. An empty candidate set is a closed gate.
// A store failure is an error and is never read as an open gate.
func (s *Server) portalSpeechStreamRate(ctx context.Context, principal auth.Token, gatewayModel string) (int, bool, error) {
	if s.Routes == nil || s.Portal == nil || strings.TrimSpace(gatewayModel) == "" {
		return 0, false, nil
	}
	ids, err := s.Portal.ModelVoiceMappingIDs(ctx, principal, gatewayModel)
	if err != nil {
		return 0, false, err
	}
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return 0, false, nil
	}
	verdicts, err := s.Routes.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return 0, false, err
	}
	candidates := plainTTSMappingIDs(ids, verdicts)
	if len(candidates) == 0 {
		return 0, false, nil
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, candidates, routing.ModelMappingCacheOutputByteRate)
	if err != nil {
		return 0, false, err
	}
	rate, ok := agreedSpeechStreamRate(candidates, verdicts, cached)
	return rate, ok, nil
}

// plainTTSMappingIDs keeps, in order, the mappings whose capability rows say plain tts
// (routing.CapabilityRowsPlainTTS): the only ones a speech request is ever routed to.
func plainTTSMappingIDs(ids []string, verdicts map[string][]routing.CapabilityRow) []string {
	candidates := ids[:0:0]
	for _, id := range ids {
		if routing.CapabilityRowsPlainTTS(verdicts[id]) {
			candidates = append(candidates, id)
		}
	}
	return candidates
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
	if audio := prep.Settings.Audio; audio != nil && audio.Stream != nil && !*audio.Stream {
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

// consumeSpeechStream reads the gateway's event stream of a streamed synthesis and returns
// the turn's outcome, the triple generateSpeechTurn returns. It is
// consumeTranscriptionStream's twin (see there for the shape of the reading): the first
// delta is the moment the run has something to show, so the deadline is lifted and the TTFT
// stamped, from the dispatch of the request (a cold start is in it); every delta's PCM is
// relayed to the run's subscribers and kept for the WAV; and the stream is read on to its
// end after speech.audio.done, because the gateway's relay bills the call on having
// delivered the stream whole, and a client that hangs up at the done frame would make it a
// client that disconnected.
//
// The turn is committed only for a stream that ENDED the way a whole synthesis does: its
// speech.audio.done and then [DONE] (the terminal frames the relay's own billing requires
// too) with audio before them. A stream that ends without them is a prefix of the clip -- cut
// by the upstream's death or the gateway's idle timeout -- and committing it would present a
// cut clip as the reply: an ended context is reported as itself (a deadline is not a cancel),
// anything else is the unreadable-response error, and nothing is committed. An empty
// synthesis is the no-audio error, not a blank bubble.
func (s *Server) consumeSpeechStream(ctx context.Context, run *ChatRun, resp *http.Response, sampleRate int, dispatchedAt time.Time, liftDeadline func()) (json.RawMessage, string, string) {
	pcm, complete, readErr := readSpeechEvents(run, resp.Body, sampleRate, dispatchedAt, liftDeadline)
	if !complete {
		// A deadline or a cancel lands here as a read error on the cancelled request, and
		// reporting either as a broken stream -- or a deadline as a cancel -- would tell the
		// user the upstream failed when it was they, or the clock, that ended it.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		// The frames are not logged: they are the audio.
		log.Printf("chat run %s: the speech stream ended before its terminal events (read error: %t, %d PCM bytes)", run.ID, readErr != nil, len(pcm))
		return nil, "error", speechRunResponseUnreadableMessage
	}
	if len(pcm) == 0 {
		return nil, "error", speechRunNoAudioMessage
	}
	if len(pcm)%speechStreamBytesPerFrame != 0 {
		log.Printf("chat run %s: the speech stream ended inside a sample (%d PCM bytes); the last byte is dropped", run.ID, len(pcm))
	}
	part, err := speechPartFrom(speechAudioMediaType, wavFromPCM16(pcm, sampleRate))
	if err != nil {
		log.Printf("chat run %s: the assembled speech stream is not usable audio: %v", run.ID, err)
		return nil, "error", err.Error()
	}
	parts, err := json.Marshal([]speechAudioPart{part})
	if err != nil {
		log.Printf("chat run %s: marshal speech parts failed: %v", run.ID, err)
		return nil, "error", speechRunDispatchFailedMessage
	}
	return parts, "completed", ""
}

// readSpeechEvents reads a streamed synthesis' event stream to its end. Every
// speech.audio.delta's PCM is decoded into the returned buffer and relayed, still as the
// base64 the stream carried it in, to the run's subscribers (see runAudio); the first one
// lifts the deadline and stamps the TTFT. It returns the PCM, whether the stream completed
// (speech.audio.done, then [DONE], neither before the other) and the scanner's read error.
//
// A frame that is JSON in shape and cannot be read, and a delta whose audio is not base64,
// each make the audio incomplete: the buffer would then hold less than the client was
// relayed, so the stream is NOT complete however it ends. The line bound is the relay's own
// (speechStreamMaxLine): a line past it ends the read (bufio.ErrTooLong), which is the same
// incomplete stream.
func readSpeechEvents(run *ChatRun, body io.Reader, sampleRate int, dispatchedAt time.Time, liftDeadline func()) (pcm []byte, complete bool, readErr error) {
	r := &speechEventReader{run: run, sampleRate: sampleRate, dispatchedAt: dispatchedAt, liftDeadline: liftDeadline}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), speechStreamMaxLine)
	for scanner.Scan() {
		r.consumeLine(scanner.Text())
	}
	return r.pcm, r.sawDone && r.sawEnd && !r.lost, scanner.Err()
}

// speechEventReader is what reading a streamed synthesis' event stream (readSpeechEvents)
// accumulates, one line at a time: the decoded PCM, the next audio event's sequence
// number, and the facts that decide whether the stream completed -- whether a delta has
// been seen (the first lifts the deadline and stamps the TTFT), a speech.audio.done, a
// [DONE] that came after it, and whether any frame or delta was lost to a read failure.
type speechEventReader struct {
	run          *ChatRun
	sampleRate   int
	dispatchedAt time.Time
	liftDeadline func()

	pcm                             []byte
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
// that is not base64 is lost, and any other is decoded into the PCM buffer and relayed to
// the run's subscribers as it came. The first lifts the deadline and stamps the TTFT.
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
		m := r.run.currentMetrics()
		m.TTFTMs = time.Since(r.dispatchedAt).Milliseconds()
		r.run.setMetrics(m)
	}
	r.pcm = append(r.pcm, chunk...)
	r.run.publishAudio(runAudio{Seq: r.seq, Rate: r.sampleRate, Data: audio})
	r.seq++
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
