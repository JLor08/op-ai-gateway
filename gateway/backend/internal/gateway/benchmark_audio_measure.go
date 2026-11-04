// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// This file holds the audio benchmark's single measurements: one synthesis or one
// transcription sent to one target, timed, and turned into the figures a mapping's audio
// metrics are made of (audioMeasurement). It decides nothing about which mapping is
// measured, how often, or where the figures are stored; the orchestration does that and
// hands each call the exact routing.Target to measure -- an instance address (m#k) in
// ProviderModel included -- together with a context that carries its deadline and the
// upstream credential (upstreamAuthCtx).
//
// Every call goes straight to that target through the provider's native relay, as the
// speech benchmark's does (benchmark_speech.go explains why a benchmark does not loop
// back through the gateway's own endpoints): a buffered request through
// provider.NativeProxyClient.ProxyNative, the live transcription through
// provider.NativeDuplexProxyClient.ProxyNativeDuplex on the same /live path and query the
// live relay uses (liveUpstreamPath). The streams are read with the relay's own parsers
// (speechStreamScan for a synthesis, liveDownMsgFromSSELine for a live transcript), so a
// frame the relay would count is the frame a measurement counts.
//
// A measurement either yields every figure or is an error with none: a figure is only
// ever measured, and a time window or an amount of audio that is zero would make a rate
// infinite or zero, which a later routing decision would read as a real one. Every error
// is an *audioMeasureError, whose kind says what the failure tells about the target.

// audioBenchmarkPassage is the text the TTS measurement synthesizes: plain English prose
// of about 55 words, roughly 20 seconds of speech. It is long enough that a streamed
// synthesis arrives in many chunks, so its rate after the first one is measured over a
// span the time to first audio does not dominate, and fixed so successive runs of one
// model are comparable. It holds no digits, abbreviations or names, which models read out
// in different ways and would make the length of audio differ between models.
const audioBenchmarkPassage = "Every morning the harbor town wakes slowly. Fishing boats return with the tide, " +
	"gulls circle above the market, and the baker opens his shutters to the smell of warm bread. " +
	"Children walk along the sea wall on their way to school, while the ferry waits at the pier, " +
	"ready to carry travelers across the bay."

// audioBenchmarkMaxAnswerBytes bounds how much of a one-shot answer (a synthesized WAV, a
// transcription document) is read. The passage's WAV is a few megabytes at most; the
// bound only stops a runaway upstream from filling memory before the deadline ends the
// call.
const audioBenchmarkMaxAnswerBytes = speechBenchmarkMaxAudioBytes

// audioStreamReadBytes is the buffer a streamed synthesis is first read into. A delta frame
// may be larger: the line reader grows the buffer up to speechStreamMaxLine, the relay's
// own bound on one frame.
const audioStreamReadBytes = 32 << 10

// liveSampleFormatS16 is the sample format the live measurement states: the bundled
// clip's (benchaudio.PCM).
const liveSampleFormatS16 = "s16le"

// audioMeasurement is what one audio measurement established. Every field is positive
// when a measurement returns without an error, and FirstByteMS <= TTFTMS <= WallMS.
type audioMeasurement struct {
	// FirstByteMS is the time from sending the request to the start of the answer: its
	// status line and headers, before any of its body is read. For a buffered synthesis it
	// is the time to the first response byte, which leaves out the transfer of the WAV that
	// TTFTMS includes; for a stream it is the answer's start, before its first output.
	FirstByteMS int64
	// TTFTMS is the time from sending the request to the first output a client could use:
	// the first audio of a streamed synthesis, the first transcript text of a live
	// transcription, and the whole answer of a buffered one (its output is usable only once
	// it is whole).
	TTFTMS int64
	// WallMS is the time from sending the request to the end of the output.
	WallMS int64
	// AudioSeconds is the length of the audio synthesized or transcribed.
	AudioSeconds float64
	// RateX is how many times faster than real time the audio was produced or transcribed.
	RateX float64
	// RTF is the real-time factor, WallMS over AudioSeconds in seconds.
	RTF float64
}

// The kinds of audioMeasureError: what a measurement that gave no figures says about its
// target.
const (
	// audioMeasureUpstreamRefused: the upstream answered and refused the work -- a non-2xx
	// answer (the agent's 409 runtime.instance_no_capacity or runtime.instance_demand_unknown
	// among them) or an error frame inside a stream: audio.cpp's own, or the agent's terminal
	// frame with its code (sseStreamError). The target did not do the work, so the refusal
	// says nothing about its speed.
	audioMeasureUpstreamRefused = "upstream_refused"
	// audioMeasureTransport: no answer came, or its body broke off -- the connection failed,
	// or the deadline ended the call.
	audioMeasureTransport = "transport"
	// audioMeasureProtocol: the upstream answered, but not with something figures can be read
	// from -- the wrong media type, an empty or oversized answer, a stream cut short or out
	// of order, a frame that cannot be read, no audio or no text, or no measurable time.
	audioMeasureProtocol = "protocol"
	// audioMeasureInvalidInput: the measurement refused what it was given before sending
	// anything -- no byte rate, a clip whose length cannot be read, a PCM shape or a language
	// the live endpoint does not take.
	audioMeasureInvalidInput = "invalid_input"
)

// audioMeasureError is why an audio measurement gave no figures; every error a measurement
// returns is one (errors.As). Its kind lets a caller -- the capacity ramp above all -- tell a
// target that refused the work (an instance the agent has no room for at that moment) from
// one that failed or answered nonsense, and both from a slow target, which is no error but
// figures. Unwrap gives the cause underneath, so errors.Is still finds a transport failure's:
// provider.ErrTimeout when the deadline ended the request, provider.ErrUnavailable when it
// could not be made, and the read's own error when the body broke off.
type audioMeasureError struct {
	// Kind is one of the audioMeasure* kinds above.
	Kind string
	// Status is the HTTP status of a refusal the upstream answered as one; 0 for an error
	// frame inside a stream and for every other kind.
	Status int
	// Code is the upstream's stable error code when its refusal names one
	// (runtime.instance_no_capacity, say); "" otherwise.
	Code string
	// Message is what the upstream said, for a refusal; the measurement's own account of
	// what went wrong, for every other kind.
	Message string
	// Err is the cause underneath, when there is one.
	Err error
}

func (e *audioMeasureError) Error() string {
	var b strings.Builder
	b.WriteString("audio benchmark: ")
	switch {
	case e.Kind == audioMeasureUpstreamRefused && e.Status != 0:
		fmt.Fprintf(&b, "the upstream answered HTTP %d: %s", e.Status, e.Message)
	case e.Kind == audioMeasureUpstreamRefused:
		b.WriteString("the upstream reported an error in its stream: ")
		b.WriteString(e.Message)
	default:
		b.WriteString(e.Message)
	}
	if e.Code != "" && !strings.Contains(e.Message, e.Code) {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *audioMeasureError) Unwrap() error { return e.Err }

// audioInputError is a measurement refused before it sent anything.
func audioInputError(message string) error {
	return &audioMeasureError{Kind: audioMeasureInvalidInput, Message: message}
}

// audioProtocolError is an answer figures cannot be read from.
func audioProtocolError(message string) error {
	return &audioMeasureError{Kind: audioMeasureProtocol, Message: message}
}

// audioTransportError is an exchange that failed underneath: what failed, and why.
func audioTransportError(what string, cause error) error {
	return &audioMeasureError{Kind: audioMeasureTransport, Message: what, Err: cause}
}

// audioRefusalError is a non-2xx answer: its status, its code and what it said
// (readBenchmarkRefusal).
func audioRefusalError(r benchmarkRefusal) error {
	return &audioMeasureError{Kind: audioMeasureUpstreamRefused, Status: r.status, Code: r.code, Message: r.text}
}

// audioAnswerReadError is the error of a whole answer readBenchmarkAnswer could not give: an
// empty or oversized answer is no answer, a body that broke off a failed exchange.
func audioAnswerReadError(err error) error {
	if errors.Is(err, errBenchmarkAnswerEmpty) || errors.Is(err, errBenchmarkAnswerTooLarge) {
		return audioProtocolError(err.Error())
	}
	return audioTransportError("the answer could not be read to its end", errors.Unwrap(err))
}

// audioStreamReadError is the error of a stream whose reading failed (bufio.Scanner.Err): a
// line past the bound is a frame no reader takes, anything else a body that broke off.
func audioStreamReadError(what string, err error, maxLine int) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return audioProtocolError(fmt.Sprintf("%s has a frame longer than %d bytes", what, maxLine))
	}
	return audioTransportError(what+" broke off", err)
}

// sseStreamError reads one SSE line as an error frame an upstream sends inside its stream
// and reports its code and message, or false for any other line. Two shapes are error
// frames:
//
//   - an event of type "error", audio.cpp's ({"type":"error","message":...});
//   - an event with no type that carries an error member: the envelope
//     {"error":{"code":...,"message":...}} the agent's runtime router ends a streamed
//     request with when it fails after it already answered 200 -- it commits a stream
//     with keepalive comments while it waits for the instance, so a cold start that timed
//     out, an instance address that moved meanwhile (runtime.instance_no_capacity) or a
//     child that died mid-answer can only be this frame by then.
//
// An event of another type is what its type says, and an error member that is null is
// none. The code is the error object's (liveErrorCode), else the event's own "code". The
// message is what messageOf reads from the frame, bounded to speechBenchmarkErrorRunes:
// each stream reads it the way its own relay does (speechStreamErrorMessage for a
// synthesis, liveErrorMessage for a live transcription), so a measurement says what a
// client of that relay would have been told.
func sseStreamError(line []byte, messageOf func(payload []byte) string) (code, message string, ok bool) {
	payload, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	payload = bytes.TrimSpace(payload)
	if !found || len(payload) == 0 || payload[0] != '{' || !json.Valid(payload) {
		return "", "", false
	}
	var frame struct {
		Type  string          `json:"type"`
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	// A member of another type is a member not stated; json decodes the others regardless.
	_ = json.Unmarshal(payload, &frame)
	hasError := len(frame.Error) > 0 && string(frame.Error) != "null"
	if frame.Type != liveEventError && (frame.Type != "" || !hasError) {
		return "", "", false
	}
	code = liveErrorCode(payload)
	if own := strings.TrimSpace(frame.Code); code == "" && len(own) <= maxLiveRefusalCodeBytes {
		code = own
	}
	return code, truncateRunes(messageOf(payload), speechBenchmarkErrorRunes), true
}

// speechStreamErrorMessage is what an error frame of a streamed synthesis says: the event's
// own "message" when it states one, else the error itself when it is a string, else what
// an error object says or, failing that, the frame quoted (speechBenchmarkErrorText, the
// speech benchmark's reading of an answer it cannot read a message from).
func speechStreamErrorMessage(payload []byte) string {
	var frame struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(payload, &frame)
	if message := strings.TrimSpace(frame.Message); message != "" {
		return message
	}
	var text string
	if json.Unmarshal(frame.Error, &text) == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return speechBenchmarkErrorText(payload)
}

// audioMeasurer runs the measurements on a clock. Production uses time.Now (the
// package-level functions below); a test scripts the clock, so every time a measurement
// reads is one the test chose and the math is checked exactly.
type audioMeasurer struct {
	now func() time.Time
}

// measureTTSStream measures a streamed synthesis of text on target (see
// audioMeasurer.measureTTSStream).
func measureTTSStream(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string, byteRate int) (audioMeasurement, error) {
	return audioMeasurer{now: time.Now}.measureTTSStream(ctx, client, target, text, voice, byteRate)
}

// measureTTSBuffered measures a one-shot synthesis of text on target (see
// audioMeasurer.measureTTSBuffered).
func measureTTSBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string) (audioMeasurement, error) {
	return audioMeasurer{now: time.Now}.measureTTSBuffered(ctx, client, target, text, voice)
}

// measureASRBuffered measures a one-shot transcription of wav on target (see
// audioMeasurer.measureASRBuffered).
func measureASRBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, wav []byte, lang string) (audioMeasurement, error) {
	return audioMeasurer{now: time.Now}.measureASRBuffered(ctx, client, target, wav, lang)
}

// measureASRLive measures a live transcription of pcm on target (see
// audioMeasurer.measureASRLive).
func measureASRLive(ctx context.Context, client provider.NativeDuplexProxyClient, target routing.Target, pcm []byte, sampleRate int, lang string) (audioMeasurement, error) {
	return audioMeasurer{now: time.Now}.measureASRLive(ctx, client, target, pcm, sampleRate, lang)
}

// audioStreamSpeechRequest is the streamed synthesis the TTS measurement sends: the speech
// benchmark's request (the target's own model name, the text, and a clone model's preset
// voice) asking for an event stream. stream_format is said outright although "sse" is
// audio.cpp's default, because the measurement reads frames and would not parse the raw
// PCM the other format answers; response_format stays unset, a stream is pcm.
type audioStreamSpeechRequest struct {
	speechBenchmarkRequest
	Stream       bool   `json:"stream"`
	StreamFormat string `json:"stream_format"`
}

// measureTTSStream synthesizes text as an event stream on target and times it:
//
//   - TTFTMS from the send to the first speech.audio.delta that carries audio;
//   - AudioSeconds the PCM the deltas carried over byteRate, the mapping's output byte rate
//     (a stream states no duration, so the rate is what turns bytes into seconds, and
//     without one nothing is sent);
//   - RateX that audio over the time from the first delta to speech.audio.done -- the
//     production speed once audio flows, which is what decides whether playback stutters;
//   - WallMS from the send to speech.audio.done, and RTF that time over the audio.
//
// The stream is read line by line, each line by the relay's own parser (speechStreamScan),
// and a moment is the arrival of the read that completed its line. The synthesis ends at
// speech.audio.done: a delta after it is not part of what was timed and is not counted.
// The stream must end the way a whole synthesis does, speech.audio.done and then [DONE],
// with every delta before done counted. Reading stops at any [DONE], so an upstream that
// keeps the connection open after it cannot hold the measurement until its deadline; one
// that comes before speech.audio.done ends a stream that has no done, and so no
// measurement. An error frame anywhere before [DONE] -- audio.cpp's, or the terminal frame
// of the agent's router after its keepalives (sseStreamError) -- is the upstream's refusal
// (audioMeasureUpstreamRefused, with its code and message), and nothing after it is read.
// voice is a clone model's preset voice, "" for a model that needs none.
func (m audioMeasurer) measureTTSStream(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string, byteRate int) (audioMeasurement, error) {
	if byteRate <= 0 {
		return audioMeasurement{}, audioInputError("no output byte rate to count the streamed audio with")
	}
	st, err := m.streamSynthesis(ctx, client, target, text, voice)
	if err != nil {
		return audioMeasurement{}, err
	}
	audioSeconds := float64(st.pcmBytes) / float64(byteRate)
	production := st.done.Sub(st.first)
	if production <= 0 {
		return audioMeasurement{}, audioProtocolError("no measurable time between the first audio and speech.audio.done")
	}
	wall := st.done.Sub(st.sent)
	return audioMeasurement{
		FirstByteMS:  millisAtLeastOne(st.answered.Sub(st.sent).Seconds()),
		TTFTMS:       millisAtLeastOne(st.first.Sub(st.sent).Seconds()),
		WallMS:       millisAtLeastOne(wall.Seconds()),
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / production.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// measureTTSStreamTimes is measureTTSStream for a pass whose audio is not counted: the same
// streamed synthesis, held to the same rules, timed to its first byte (FirstByteMS), its first
// audio delta (TTFTMS) and its speech.audio.done (WallMS), with no byte rate and so no
// AudioSeconds, RateX or RTF. It is the cold pass of a streamed synthesis whose byte rate is
// not cached yet: that pass only lends its time to the first audio to the load time, and the
// speech benchmark that learns the rate is a request to the target, so it may only run once
// the cold pass has taken the cold start (measureAudioSpeed).
func (m audioMeasurer) measureTTSStreamTimes(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string) (audioMeasurement, error) {
	st, err := m.streamSynthesis(ctx, client, target, text, voice)
	if err != nil {
		return audioMeasurement{}, err
	}
	return audioMeasurement{
		FirstByteMS: millisAtLeastOne(st.answered.Sub(st.sent).Seconds()),
		TTFTMS:      millisAtLeastOne(st.first.Sub(st.sent).Seconds()),
		WallMS:      millisAtLeastOne(st.done.Sub(st.sent).Seconds()),
	}, nil
}

// ttsStreamTiming is what one streamed synthesis observed (streamSynthesis): when it was
// sent, answered, its first audio delta and its speech.audio.done arrived, and the PCM bytes
// the deltas before done carried.
type ttsStreamTiming struct {
	sent, answered, first, done time.Time
	pcmBytes                    int64
}

// streamSynthesis sends the streamed synthesis of text on target and reads it to its end by
// measureTTSStream's rules, returning what it observed; every rule a stream breaks is the
// error measureTTSStream documents.
func (m audioMeasurer) streamSynthesis(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string) (ttsStreamTiming, error) {
	body, err := json.Marshal(audioStreamSpeechRequest{
		speechBenchmarkRequest: speechBenchmarkRequest{Model: target.ProviderModel, Input: text, Voice: voice},
		Stream:                 true,
		StreamFormat:           speechStreamFormatSSE,
	})
	if err != nil {
		return ttsStreamTiming{}, &audioMeasureError{Kind: audioMeasureInvalidInput, Message: "build the synthesis request", Err: err}
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointSpeech), body, jsonContentType)
	if err != nil {
		return ttsStreamTiming{}, audioTransportError("synthesis request failed", err)
	}
	answered := m.now()
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return ttsStreamTiming{}, err
	}
	if err := checkAudioEventStream(resp); err != nil {
		return ttsStreamTiming{}, err
	}

	read, err := m.readSynthesisStream(resp.Body)
	if err != nil {
		return ttsStreamTiming{}, err
	}
	if err := read.complete(); err != nil {
		return ttsStreamTiming{}, err
	}
	return ttsStreamTiming{sent: sent, answered: answered, first: read.first, done: read.done, pcmBytes: read.scan.pcmBytes()}, nil
}

// synthesisStreamRead is what reading one synthesis stream (readSynthesisStream) observed:
// its counted audio, when its first audio delta and its speech.audio.done arrived, and
// whether it ended with [DONE].
type synthesisStreamRead struct {
	scan        *speechStreamScan
	first, done time.Time
	ended       bool
}

// readSynthesisStream reads a synthesis event stream up to [DONE], the first line its audio
// can no longer be counted from, or its end. An error frame before [DONE] is the upstream's
// refusal, and a read that fails is the stream's error; whether what was read is a whole
// synthesis is complete's to judge.
func (m audioMeasurer) readSynthesisStream(body io.Reader) (synthesisStreamRead, error) {
	read := synthesisStreamRead{scan: &speechStreamScan{maxLine: speechStreamMaxLine}}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, audioStreamReadBytes), speechStreamMaxLine)
	for sc.Scan() {
		if sseLineIsDone(sc.Bytes()) {
			read.ended = true
			break
		}
		if code, message, isError := sseStreamError(sc.Bytes(), speechStreamErrorMessage); isError {
			return synthesisStreamRead{}, &audioMeasureError{Kind: audioMeasureUpstreamRefused, Code: code, Message: message}
		}
		if !read.done.IsZero() {
			// The synthesis ended at its done event; what follows up to [DONE] is not part
			// of what was timed.
			continue
		}
		read.scan.scanLine(sc.Bytes())
		if read.scan.lost {
			break
		}
		read.mark(m.now())
	}
	if err := sc.Err(); err != nil {
		// A frame past the relay's own bound is one the relay could not count either.
		return synthesisStreamRead{}, audioStreamReadError("the synthesis stream", err, speechStreamMaxLine)
	}
	return read, nil
}

// mark records at as the time of the first audio and of speech.audio.done once the line just
// scanned brought either.
func (read *synthesisStreamRead) mark(at time.Time) {
	if read.first.IsZero() && read.scan.pcmBytes() > 0 {
		read.first = at
	}
	if read.scan.sawDone {
		read.done = at
	}
}

// complete reports whether the stream read was a whole synthesis: every frame counted, ended
// by speech.audio.done and then [DONE], with audio in it.
func (read synthesisStreamRead) complete() error {
	switch {
	case read.scan.lost:
		return audioProtocolError("the streamed audio could not be counted (a frame or a delta was unreadable)")
	case read.done.IsZero():
		return audioProtocolError("the synthesis stream ended before speech.audio.done")
	case !read.ended:
		return audioProtocolError("the synthesis stream ended without [DONE] after speech.audio.done")
	case read.scan.pcmBytes() == 0:
		return audioProtocolError("the synthesis stream carried no audio")
	}
	return nil
}

// measureTTSBuffered synthesizes text as one WAV on target and times it. The answer is
// usable only once it is whole, so TTFTMS and WallMS are both the time from the send to
// the end of the answer, RateX the audio over that time and RTF its inverse; FirstByteMS is
// the time to the answer's start, before the WAV is read (the first response byte, which a
// cold start is told by for a synthesis that does not stream). The length of
// the audio is audio.cpp's own X-AudioCPP-Audio-Duration-Ms when usable, else the WAV's
// (speechBenchmarkAudioSeconds, the speech benchmark's rule). voice is a clone model's
// preset voice, "" for a model that needs none.
func (m audioMeasurer) measureTTSBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string) (audioMeasurement, error) {
	body, err := json.Marshal(speechBenchmarkRequest{Model: target.ProviderModel, Input: text, Voice: voice})
	if err != nil {
		return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureInvalidInput, Message: "build the synthesis request", Err: err}
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointSpeech), body, jsonContentType)
	if err != nil {
		return audioMeasurement{}, audioTransportError("synthesis request failed", err)
	}
	answered := m.now()
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	if contentType := resp.Header.Get(contentTypeHeader); !speechAudioMediaTypeOf(contentType) {
		return audioMeasurement{}, audioProtocolError(fmt.Sprintf("the synthesis answer is not labelled as WAV (Content-Type %q)", contentType))
	}
	audio, end, err := readBenchmarkAnswer(resp.Body, audioBenchmarkMaxAnswerBytes, m.now)
	if err != nil {
		return audioMeasurement{}, audioAnswerReadError(err)
	}
	audioSeconds, ok := speechBenchmarkAudioSeconds(resp.Header, audio)
	if !ok {
		return audioMeasurement{}, audioProtocolError("the synthesis answer states no audio length (no usable duration header and no readable WAV header)")
	}
	return bufferedAudioMeasurement(sent, answered, end, audioSeconds)
}

// measureASRBuffered transcribes wav on target in one request and times it: the answer is
// usable only once it is whole, so TTFTMS and WallMS are both the time from the send to
// the end of the answer, RateX the clip's length over that time and RTF its inverse, and
// FirstByteMS the time to the answer's start, before its document is read. The
// length is read from the clip that is sent, never from the answer, and a clip whose
// length cannot be read is refused before anything is sent. lang is passed as the form's
// language field; "" states none, and the model detects it.
func (m audioMeasurer) measureASRBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, wav []byte, lang string) (audioMeasurement, error) {
	ms, ok := wavDurationFromReader(bytes.NewReader(wav))
	if !ok || ms <= 0 {
		return audioMeasurement{}, audioInputError("the clip is no WAV whose length can be read")
	}
	body, contentType, err := buildASRBenchmarkBody(target.ProviderModel, lang, wav)
	if err != nil {
		return audioMeasurement{}, &audioMeasureError{Kind: audioMeasureInvalidInput, Message: "build the transcription request", Err: err}
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointTranscription), body, contentType)
	if err != nil {
		return audioMeasurement{}, audioTransportError("transcription request failed", err)
	}
	answered := m.now()
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	answer, end, err := readBenchmarkAnswer(resp.Body, audioBenchmarkMaxAnswerBytes, m.now)
	if err != nil {
		return audioMeasurement{}, audioAnswerReadError(err)
	}
	var transcription struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(answer, &transcription) != nil || transcription.Text == nil {
		return audioMeasurement{}, audioProtocolError("the answer is no transcription (no JSON document with a text field)")
	}
	// A clip of speech transcribed to nothing is a failed transcription, and its time is no
	// rate of transcribing it.
	if strings.TrimSpace(*transcription.Text) == "" {
		return audioMeasurement{}, audioProtocolError("the transcription has no text")
	}
	return bufferedAudioMeasurement(sent, answered, end, float64(ms)/1000)
}

// buildASRBenchmarkBody is the buffered transcription's multipart form: the target's model
// name, the language when one is given, and the clip as the WAV `file` part, written the
// way the portal's transcription run writes it (writeTranscriptionFilePart). No stream
// field is sent, so the answer is the one-shot JSON document.
func buildASRBenchmarkBody(model, lang string, wav []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if lang != "" {
		if err := mw.WriteField("language", lang); err != nil {
			return nil, "", err
		}
	}
	if err := writeTranscriptionFilePart(mw, transcriptionFilenames["audio/wav"], "audio/wav", wav); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// measureASRLive sends pcm -- mono s16le at sampleRate Hz -- to target's live
// transcription endpoint unpaced, as fast as the connection takes it, and times the
// transcript:
//
//   - TTFTMS from the send to the first frame before transcript.text.done that carries
//     transcript text, a transcript.text.delta or a transcript.text.partial; for a model
//     that streams no increments, whose text first arrives in transcript.text.done, it is
//     the done event's time, so TTFTMS equals WallMS;
//   - WallMS from the send to transcript.text.done, RateX the clip's length over that time
//     (unpaced, the whole exchange is transcription time) and RTF its inverse.
//
// The transcription ends at transcript.text.done: a frame after it, up to [DONE], is no
// part of it and is never the first text, so TTFTMS never exceeds WallMS.
//
// The PCM's shape and the language are checked as the live relay checks them
// (liveByteRate, validLiveLanguage) before anything is sent: they ride in the request's
// query, which the upstream decodes the samples by. The stream must end with
// transcript.text.done and then [DONE], and reading stops at [DONE]; an error frame
// anywhere before [DONE], in either shape sseStreamError reads, is the upstream's refusal
// (audioMeasureUpstreamRefused, with its code and message), and nothing after it is read.
func (m audioMeasurer) measureASRLive(ctx context.Context, client provider.NativeDuplexProxyClient, target routing.Target, pcm []byte, sampleRate int, lang string) (audioMeasurement, error) {
	byteRate, err := liveMeasureByteRate(pcm, sampleRate, lang)
	if err != nil {
		return audioMeasurement{}, err
	}
	audioSeconds := float64(len(pcm)) / float64(byteRate)
	path := liveUpstreamPath(target, liveParams{SampleRate: sampleRate, Channels: 1, Format: liveSampleFormatS16, Language: lang})

	sent := m.now()
	resp, err := client.ProxyNativeDuplex(ctx, target, path, bytes.NewReader(pcm), liveUpstreamContentType)
	if err != nil {
		return audioMeasurement{}, audioTransportError("live transcription request failed", err)
	}
	answered := m.now()
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	if err := checkAudioEventStream(resp); err != nil {
		return audioMeasurement{}, err
	}

	first, done, err := m.readLiveTranscript(resp.Body)
	if err != nil {
		return audioMeasurement{}, err
	}
	wall := done.Sub(sent)
	if wall <= 0 {
		return audioMeasurement{}, audioProtocolError("no measurable time between the send and transcript.text.done")
	}
	return audioMeasurement{
		FirstByteMS:  millisAtLeastOne(answered.Sub(sent).Seconds()),
		TTFTMS:       millisAtLeastOne(first.Sub(sent).Seconds()),
		WallMS:       millisAtLeastOne(wall.Seconds()),
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / wall.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// liveMeasureByteRate checks a live measurement's input as the live relay checks it --
// mono s16le pcm at a sample rate the endpoint takes, a whole number of samples, a bare
// language tag or none -- and returns the PCM's byte rate.
func liveMeasureByteRate(pcm []byte, sampleRate int, lang string) (int, error) {
	byteRate, ok := liveByteRate(sampleRate, 1, liveSampleFormatS16)
	if !ok {
		return 0, audioInputError(fmt.Sprintf("a sample rate of %d Hz is not one the live endpoint takes", sampleRate))
	}
	if len(pcm) == 0 {
		return 0, audioInputError("no audio to transcribe")
	}
	if len(pcm)%2 != 0 {
		return 0, audioInputError(fmt.Sprintf("the audio (%d bytes) is not a whole number of 2-byte samples", len(pcm)))
	}
	if !validLiveLanguage(lang) {
		return 0, audioInputError(fmt.Sprintf("the language %q is no bare language tag", lang))
	}
	return byteRate, nil
}

// liveTranscriptRead is what reading one live transcript stream (readLiveTranscript)
// observed: when its first text and its transcript.text.done arrived, and whether it ended
// with [DONE].
type liveTranscriptRead struct {
	first, done time.Time
	ended       bool
}

// readLiveTranscript reads a live transcript stream up to [DONE] or its end and returns when
// its first text and its transcript.text.done arrived. An error frame before [DONE] is the
// upstream's refusal; a stream without transcript.text.done, without [DONE] after it, or
// without any text is a protocol error.
func (m audioMeasurer) readLiveTranscript(body io.Reader) (first, done time.Time, err error) {
	var read liveTranscriptRead
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLiveSSELineBytes)
	for sc.Scan() {
		if sseLineIsDone(sc.Bytes()) {
			read.ended = true
			break
		}
		if code, message, isError := sseStreamError(sc.Bytes(), liveErrorMessage); isError {
			return time.Time{}, time.Time{}, &audioMeasureError{Kind: audioMeasureUpstreamRefused, Code: code, Message: message}
		}
		msg, ok := liveDownMsgFromSSELine(sc.Bytes())
		if !ok {
			continue
		}
		if !read.done.IsZero() {
			// The transcription ended at its done event; what follows up to [DONE] is no
			// part of it.
			continue
		}
		read.mark(m.now(), msg)
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, time.Time{}, audioStreamReadError("the live transcript stream", err, maxLiveSSELineBytes)
	}
	if err := read.complete(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return read.first, read.done, nil
}

// mark records at as the time of the first text and of transcript.text.done once msg brought
// either.
func (read *liveTranscriptRead) mark(at time.Time, msg liveDownMsg) {
	if read.first.IsZero() && strings.TrimSpace(msg.Text) != "" {
		read.first = at
	}
	if msg.Type == "done" {
		read.done = at
	}
}

// complete reports whether the stream read was a whole live transcription: ended by
// transcript.text.done and then [DONE], with text in it.
func (read liveTranscriptRead) complete() error {
	switch {
	case read.done.IsZero():
		return audioProtocolError("the live transcript stream ended before transcript.text.done")
	case !read.ended:
		return audioProtocolError("the live transcript stream ended without [DONE] after transcript.text.done")
	case read.first.IsZero():
		return audioProtocolError("the live transcription has no text")
	}
	return nil
}

// bufferedAudioMeasurement is the figures of a one-shot answer that started at answered and
// ended at end: its output is usable only once it is whole, so the time to it is both
// TTFTMS and WallMS, and the time to its start is FirstByteMS. A wall time or a length of
// audio that is not positive gives no rate and is an error.
func bufferedAudioMeasurement(sent, answered, end time.Time, audioSeconds float64) (audioMeasurement, error) {
	wall := end.Sub(sent)
	if wall <= 0 {
		return audioMeasurement{}, audioProtocolError("no measurable time between the send and the end of the answer")
	}
	if audioSeconds <= 0 {
		return audioMeasurement{}, audioProtocolError("the answer holds no audio")
	}
	ms := millisAtLeastOne(wall.Seconds())
	return audioMeasurement{
		FirstByteMS:  millisAtLeastOne(answered.Sub(sent).Seconds()),
		TTFTMS:       ms,
		WallMS:       ms,
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / wall.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// checkAudioAnswerStatus is the refusal of a non-2xx answer, with its status, its code and
// what the upstream said (readBenchmarkRefusal, the speech benchmark's reading of it), or
// nil.
func checkAudioAnswerStatus(resp *provider.ProxyResponse) error {
	if refusal, refused := readBenchmarkRefusal(resp); refused {
		return audioRefusalError(refusal)
	}
	return nil
}

// checkAudioEventStream is the error of an answer that is not an event stream. A streamed
// synthesis may also answer raw PCM (stream_format=audio) and a model that cannot stream
// answers its one-shot document; neither has the frames the measurement times.
func checkAudioEventStream(resp *provider.ProxyResponse) error {
	contentType := resp.Header.Get(contentTypeHeader)
	if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != speechStreamSSEType {
		return audioProtocolError(fmt.Sprintf("the answer is not an event stream (Content-Type %q)", contentType))
	}
	return nil
}

// sseLineIsDone reports whether an SSE line is the [DONE] sentinel's data line.
func sseLineIsDone(line []byte) bool {
	payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	return ok && string(bytes.TrimSpace(payload)) == sseDoneSentinel
}
