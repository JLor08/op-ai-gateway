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
// infinite or zero, which a later routing decision would read as a real one.

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
// when a measurement returns without an error.
type audioMeasurement struct {
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
// measurement. voice is a clone model's preset voice, "" for a model that needs none.
func (m audioMeasurer) measureTTSStream(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string, byteRate int) (audioMeasurement, error) {
	if byteRate <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: no output byte rate to count the streamed audio with")
	}
	body, err := json.Marshal(audioStreamSpeechRequest{
		speechBenchmarkRequest: speechBenchmarkRequest{Model: target.ProviderModel, Input: text, Voice: voice},
		Stream:                 true,
		StreamFormat:           speechStreamFormatSSE,
	})
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: build the synthesis request: %w", err)
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointSpeech), body, jsonContentType)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: synthesis request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	if err := checkAudioEventStream(resp); err != nil {
		return audioMeasurement{}, err
	}

	scan := &speechStreamScan{maxLine: speechStreamMaxLine}
	var first, done time.Time
	ended := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, audioStreamReadBytes), speechStreamMaxLine)
	for sc.Scan() {
		if sseLineIsDone(sc.Bytes()) {
			ended = true
			break
		}
		if !done.IsZero() {
			// The synthesis ended at its done event; what follows up to [DONE] is not part
			// of what was timed.
			continue
		}
		scan.scanLine(sc.Bytes())
		if scan.lost {
			break
		}
		at := m.now()
		if first.IsZero() && scan.pcmBytes() > 0 {
			first = at
		}
		if scan.sawDone {
			done = at
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// A frame past the relay's own bound is one the relay could not count either.
			scan.lost = true
		} else {
			return audioMeasurement{}, fmt.Errorf("audio benchmark: the synthesis stream broke off: %w", err)
		}
	}
	switch {
	case scan.lost:
		return audioMeasurement{}, errors.New("audio benchmark: the streamed audio could not be counted (a frame or a delta was unreadable)")
	case done.IsZero():
		return audioMeasurement{}, errors.New("audio benchmark: the synthesis stream ended before speech.audio.done")
	case !ended:
		return audioMeasurement{}, errors.New("audio benchmark: the synthesis stream ended without [DONE] after speech.audio.done")
	case scan.pcmBytes() == 0:
		return audioMeasurement{}, errors.New("audio benchmark: the synthesis stream carried no audio")
	}
	audioSeconds := float64(scan.pcmBytes()) / float64(byteRate)
	production := done.Sub(first)
	if production <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: no measurable time between the first audio and speech.audio.done")
	}
	wall := done.Sub(sent)
	return audioMeasurement{
		TTFTMS:       millisAtLeastOne(first.Sub(sent).Seconds()),
		WallMS:       millisAtLeastOne(wall.Seconds()),
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / production.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// measureTTSBuffered synthesizes text as one WAV on target and times it. The answer is
// usable only once it is whole, so TTFTMS and WallMS are both the time from the send to
// the end of the answer, RateX the audio over that time and RTF its inverse. The length of
// the audio is audio.cpp's own X-AudioCPP-Audio-Duration-Ms when usable, else the WAV's
// (speechBenchmarkAudioSeconds, the speech benchmark's rule). voice is a clone model's
// preset voice, "" for a model that needs none.
func (m audioMeasurer) measureTTSBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, text, voice string) (audioMeasurement, error) {
	body, err := json.Marshal(speechBenchmarkRequest{Model: target.ProviderModel, Input: text, Voice: voice})
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: build the synthesis request: %w", err)
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointSpeech), body, jsonContentType)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: synthesis request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	if contentType := resp.Header.Get(contentTypeHeader); !speechAudioMediaTypeOf(contentType) {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: the synthesis answer is not labelled as WAV (Content-Type %q)", contentType)
	}
	audio, end, err := readBenchmarkAnswer(resp.Body, audioBenchmarkMaxAnswerBytes, m.now)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: %w", err)
	}
	audioSeconds, ok := speechBenchmarkAudioSeconds(resp.Header, audio)
	if !ok {
		return audioMeasurement{}, errors.New("audio benchmark: the synthesis answer states no audio length (no usable duration header and no readable WAV header)")
	}
	return bufferedAudioMeasurement(sent, end, audioSeconds)
}

// measureASRBuffered transcribes wav on target in one request and times it: the answer is
// usable only once it is whole, so TTFTMS and WallMS are both the time from the send to
// the end of the answer, RateX the clip's length over that time and RTF its inverse. The
// length is read from the clip that is sent, never from the answer, and a clip whose
// length cannot be read is refused before anything is sent. lang is passed as the form's
// language field; "" states none, and the model detects it.
func (m audioMeasurer) measureASRBuffered(ctx context.Context, client provider.NativeProxyClient, target routing.Target, wav []byte, lang string) (audioMeasurement, error) {
	ms, ok := wavDurationFromReader(bytes.NewReader(wav))
	if !ok || ms <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: the clip is no WAV whose length can be read")
	}
	body, contentType, err := buildASRBenchmarkBody(target.ProviderModel, lang, wav)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: build the transcription request: %w", err)
	}
	sent := m.now()
	resp, err := client.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointTranscription), body, contentType)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: transcription request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	answer, end, err := readBenchmarkAnswer(resp.Body, audioBenchmarkMaxAnswerBytes, m.now)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: %w", err)
	}
	var transcription struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(answer, &transcription) != nil || transcription.Text == nil {
		return audioMeasurement{}, errors.New("audio benchmark: the answer is no transcription (no JSON document with a text field)")
	}
	// A clip of speech transcribed to nothing is a failed transcription, and its time is no
	// rate of transcribing it.
	if strings.TrimSpace(*transcription.Text) == "" {
		return audioMeasurement{}, errors.New("audio benchmark: the transcription has no text")
	}
	return bufferedAudioMeasurement(sent, end, float64(ms)/1000)
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
//   - TTFTMS from the send to the first frame that carries transcript text (a delta, a
//     partial or, for a model that streams no increments, the done event);
//   - WallMS from the send to transcript.text.done, RateX the clip's length over that time
//     (unpaced, the whole exchange is transcription time) and RTF its inverse.
//
// The PCM's shape and the language are checked as the live relay checks them
// (liveByteRate, validLiveLanguage) before anything is sent: they ride in the request's
// query, which the upstream decodes the samples by. The stream must end with
// transcript.text.done and then [DONE], and reading stops at [DONE]; an error event is
// the measurement's error.
func (m audioMeasurer) measureASRLive(ctx context.Context, client provider.NativeDuplexProxyClient, target routing.Target, pcm []byte, sampleRate int, lang string) (audioMeasurement, error) {
	byteRate, ok := liveByteRate(sampleRate, 1, liveSampleFormatS16)
	if !ok {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: a sample rate of %d Hz is not one the live endpoint takes", sampleRate)
	}
	if len(pcm) == 0 {
		return audioMeasurement{}, errors.New("audio benchmark: no audio to transcribe")
	}
	if len(pcm)%2 != 0 {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: the audio (%d bytes) is not a whole number of 2-byte samples", len(pcm))
	}
	if !validLiveLanguage(lang) {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: the language %q is no bare language tag", lang)
	}
	audioSeconds := float64(len(pcm)) / float64(byteRate)
	path := liveUpstreamPath(target, liveParams{SampleRate: sampleRate, Channels: 1, Format: liveSampleFormatS16, Language: lang})

	sent := m.now()
	resp, err := client.ProxyNativeDuplex(ctx, target, path, bytes.NewReader(pcm), liveUpstreamContentType)
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: live transcription request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkAudioAnswerStatus(resp); err != nil {
		return audioMeasurement{}, err
	}
	if err := checkAudioEventStream(resp); err != nil {
		return audioMeasurement{}, err
	}

	var first, done time.Time
	ended := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLiveSSELineBytes)
	for sc.Scan() {
		if sseLineIsDone(sc.Bytes()) {
			ended = true
			break
		}
		msg, ok := liveDownMsgFromSSELine(sc.Bytes())
		if !ok {
			continue
		}
		at := m.now()
		if msg.Type == "error" {
			return audioMeasurement{}, fmt.Errorf("audio benchmark: the live transcription reported an error: %s", msg.Message)
		}
		if first.IsZero() && strings.TrimSpace(msg.Text) != "" {
			first = at
		}
		if msg.Type == "done" && done.IsZero() {
			done = at
		}
	}
	if err := sc.Err(); err != nil {
		return audioMeasurement{}, fmt.Errorf("audio benchmark: the live transcript stream broke off: %w", err)
	}
	switch {
	case done.IsZero():
		return audioMeasurement{}, errors.New("audio benchmark: the live transcript stream ended before transcript.text.done")
	case !ended:
		return audioMeasurement{}, errors.New("audio benchmark: the live transcript stream ended without [DONE] after transcript.text.done")
	case first.IsZero():
		return audioMeasurement{}, errors.New("audio benchmark: the live transcription has no text")
	}
	wall := done.Sub(sent)
	if wall <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: no measurable time between the send and transcript.text.done")
	}
	return audioMeasurement{
		TTFTMS:       millisAtLeastOne(first.Sub(sent).Seconds()),
		WallMS:       millisAtLeastOne(wall.Seconds()),
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / wall.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// bufferedAudioMeasurement is the figures of a one-shot answer: its output is usable only
// once it is whole, so the time to it is both TTFTMS and WallMS. A wall time or a length of
// audio that is not positive gives no rate and is an error.
func bufferedAudioMeasurement(sent, end time.Time, audioSeconds float64) (audioMeasurement, error) {
	wall := end.Sub(sent)
	if wall <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: no measurable time between the send and the end of the answer")
	}
	if audioSeconds <= 0 {
		return audioMeasurement{}, errors.New("audio benchmark: the answer holds no audio")
	}
	ms := millisAtLeastOne(wall.Seconds())
	return audioMeasurement{
		TTFTMS:       ms,
		WallMS:       ms,
		AudioSeconds: audioSeconds,
		RateX:        audioSeconds / wall.Seconds(),
		RTF:          wall.Seconds() / audioSeconds,
	}, nil
}

// checkAudioAnswerStatus is the error of a non-2xx answer, quoting what the upstream said
// (readBenchmarkRefusal, the speech benchmark's reading of it), or nil.
func checkAudioAnswerStatus(resp *provider.ProxyResponse) error {
	if refusal, refused := readBenchmarkRefusal(resp); refused {
		return fmt.Errorf("audio benchmark: the upstream answered HTTP %d: %s", refusal.status, refusal.text)
	}
	return nil
}

// checkAudioEventStream is the error of an answer that is not an event stream. A streamed
// synthesis may also answer raw PCM (stream_format=audio) and a model that cannot stream
// answers its one-shot document; neither has the frames the measurement times.
func checkAudioEventStream(resp *provider.ProxyResponse) error {
	contentType := resp.Header.Get(contentTypeHeader)
	if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != speechStreamSSEType {
		return fmt.Errorf("audio benchmark: the answer is not an event stream (Content-Type %q)", contentType)
	}
	return nil
}

// sseLineIsDone reports whether an SSE line is the [DONE] sentinel's data line.
func sseLineIsDone(line []byte) bool {
	payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	return ok && string(bytes.TrimSpace(payload)) == sseDoneSentinel
}
