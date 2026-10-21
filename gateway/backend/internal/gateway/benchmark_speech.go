// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// This file is the speech benchmark (#263): runBenchmark's "speech" mode, the one
// benchmark that runs on the audio mappings every chat-prompt mode refuses.
//
// It exists for one measurement. A streamed speech response is raw PCM with no
// duration and no format on the wire, so it is billed in audio seconds only when the
// mapping's output byte rate (bytes of PCM per second of audio) is known, and
// otherwise in the honest audio_bytes fallback (ADR-041, ADR-068). The rate is a
// property of the model, and the only place the API states it is the `fmt` chunk of a
// NON-stream WAV answer -- so the benchmark synthesizes one short clip, reads the rate
// from that header (wavByteRateFromReader) and caches it under the mapping
// (routing.ModelMappingCacheOutputByteRate), where the streamed relay finds it
// (cachedOutputByteRate). It never guesses: no readable WAV, no rate.
//
// The same call yields the audio perf metrics -- the time to the first byte, the wall
// time and the real-time factor -- recorded as a kind=="speech" history row
// (routing.BenchmarkRun, migration 95).
//
// The synthesis is sent straight to the mapping's upstream through the provider's
// native relay (provider.NativeProxyClient.ProxyNative), the primitive the gateway's
// own speech relay is built on, and not as a request to the gateway's own
// /v1/audio/speech over the internal loopback pair that the portal's speech run
// (chat_runs_audio.go) uses. Every benchmark addresses its mapping's application
// directly (benchmarkTargetReq), and the loopback hop does not fit a benchmark:
//
//   - the run holds the server's reservation (BenchmarkRegistry.TryStart), which the
//     resolver honours by excluding the server from routing, so a request routed
//     through the gateway would not find the very server under test (the portal run
//     pins a server with X-OP-Server-Override, which needs AuthorizeServerManage for
//     a user);
//   - the loopback pair authenticates as a real user (X-OP-Internal-User, looked up
//     by UserByID), and the runs that start without one -- the scheduled run and the
//     health loop's automatic trigger -- have no user to speak as, and no system
//     principal exists to stand in;
//   - the relay answers its client the body, Content-Type and Content-Length only:
//     audio.cpp's X-AudioCPP-* metering headers are not forwarded (they reach only
//     the payload capture), and it records a usage row for the call, which a
//     benchmark is not.
//
// A direct call reaches a direct audio_cpp application and an agent-launched child
// alike: both are addressed by routing.ApplicationEndpoint, and the agent's router
// starts a child that is not running, as it does for the chat benchmark.

// benchmarkModeSpeech is the ?mode= value that starts the speech benchmark.
const benchmarkModeSpeech = "speech"

// benchmarkKindSpeech is the Kind of the speech benchmark's history rows.
const benchmarkKindSpeech = "speech"

// speechBenchmarkInput is the text the benchmark synthesizes: a short, ordinary
// sentence, long enough to be a clip of a second or two and short enough that the
// run is quick. It is fixed so successive runs of one model are comparable.
const speechBenchmarkInput = "The quick brown fox jumps over the lazy dog."

// speechBenchmarkMaxAudioBytes bounds how much of the synthesized answer is read. A
// clip of a sentence is well under a megabyte; the cap only stops a runaway upstream
// from filling memory before the run's time budget ends it.
const speechBenchmarkMaxAudioBytes = 64 << 20

// speechBenchmarkErrorBytes bounds how much of a non-2xx answer is read to say what
// went wrong, and speechBenchmarkErrorRunes how much of it a result quotes.
const (
	speechBenchmarkErrorBytes = 4 << 10
	speechBenchmarkErrorRunes = 200
)

// The metering headers audio.cpp sets on a non-stream synthesis, beside the one
// speechDurationHeader names: its own wall time for the call (milliseconds) and its
// real-time factor (wall time over the length of audio produced).
const (
	speechWallHeader = "X-AudioCPP-Wall-Ms"
	speechRTFHeader  = "X-AudioCPP-RTF"
)

// errBenchmarkNoNativeProxy is the speech benchmark's twin of errBenchmarkNoStreaming:
// the provider cannot forward a raw request body to a mapping's upstream.
var errBenchmarkNoNativeProxy = errors.New("benchmark: provider does not support native passthrough")

// speechBenchmarkRequest is the plain non-stream synthesis the benchmark sends: the
// model (the application's own id for it, which is what the gateway's relay would
// rewrite the client's name to) and the text, and nothing else. A voice, a speed and
// every other field stay on the model's own defaults, and response_format and stream
// are absent so audio.cpp answers its one-shot WAV.
type speechBenchmarkRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// speechMeasurement is what one successful benchmark synthesis measured.
type speechMeasurement struct {
	// byteRate is the output byte rate read from the answer's WAV `fmt` chunk.
	byteRate uint32
	// ttftMS, wallMS and rtf are the audio perf metrics (routing.BenchmarkRun.TTFTMS,
	// WallMS and RTF); each is positive when set, and rtf is 0 when the length of the
	// audio could not be established.
	ttftMS, wallMS int64
	rtf            float64
}

// speechBenchmarkSkip is the speech benchmark's per-mapping decision from the
// mapping's stored capability rows: "" when it is a target, else why it is skipped.
//
// A target is a plain text-to-speech model -- (speech, yes) and not (speech_clone,
// yes), routing.CapabilityRowsPlainTTS, the one definition of that rule the voices
// cache uses too. An absent speech row is unknown, which is not a speech model; an
// absent or negative clone row does not exclude, because "unknown" is never "needs a
// voice" (a clone model that slips through fails its synthesis, writes no rate and
// bills audio_bytes, which is the same outcome as the skip). The other two outcomes
// are the closed values of BenchmarkResult.Skipped: benchmarkSkippedSpeechClone for a
// clone model, whose synthesis needs a server-side reference WAV and its text that a
// plain model-and-input request cannot supply (it answers HTTP 500), and
// benchmarkSkippedNotSpeech for everything else.
func speechBenchmarkSkip(rows []routing.CapabilityRow) string {
	if routing.CapabilityRowsPlainTTS(rows) {
		return ""
	}
	if routing.CapabilityRowsByName(rows)[routing.CapabilitySpeech].Verdict != routing.CapabilityYes {
		return benchmarkSkippedNotSpeech
	}
	return benchmarkSkippedSpeechClone
}

// partitionSpeechBenchmarkViews is partitionBenchmarkViews for the speech
// benchmark, where the audio-only split is inverted: a view that serves only audio is
// exactly what runs here, and a view that does not synthesize speech is what is
// skipped. The chat modes keep partitionBenchmarkViews, which still refuses audio.
//
// Each view's runtime spec is read once (mappingRuntimeSpec) and its capability rows
// once (MappingCapabilities). A view is runnable when speechBenchmarkSkip says so and
// the mapping is one the gateway would route a speech request to
// (routing.MappingServesAPIFlavor for openai_audio, on the effective flavors); it is
// skipped, with the reason as its recorded result and no history row, when it is not;
// and it is unreadable when either read failed, which is recorded as an error and
// logged -- whether it is a target is then unknown, and is not guessed at. A runnable
// target is built from the spec that was read.
func (s *Server) partitionSpeechBenchmarkViews(ctx context.Context, views []portal.BenchmarkTargetView) benchmarkViewPartition {
	var p benchmarkViewPartition
	unreadable := func(v portal.BenchmarkTargetView, what string, err error) {
		slog.Warn("speech benchmark: "+what+" read failed; mapping not benchmarked", "mapping_id", v.Mapping.ID, "err", err)
		p.unreadable++
		p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Error: "speech benchmark: " + what + " unreadable; not benchmarked: " + err.Error()})
	}
	skipped := func(v portal.BenchmarkTargetView, why string) {
		p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: why})
	}
	for _, v := range views {
		spec, hasSpec, err := s.mappingRuntimeSpec(ctx, v.App, v.Mapping.ID)
		if err != nil {
			unreadable(v, "runtime spec", err)
			continue
		}
		rows, err := s.Routes.MappingCapabilities(ctx, v.Mapping.ID)
		if err != nil {
			unreadable(v, "capability", err)
			continue
		}
		switch why := speechBenchmarkSkip(rows); {
		case why != "":
			skipped(v, why)
		case !routing.MappingServesAPIFlavor(v.App, routing.EffectiveFields(v.App, spec, hasSpec), routing.APIFlavorOpenAIAudio):
			skipped(v, benchmarkSkippedNotSpeech)
		default:
			p.runnable = append(p.runnable, s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec))
		}
	}
	return p
}

// refuseNothingRunnableSpeech answers a speech partition with no runnable view, and
// reports whether it did: 500 benchmark.request_failed when a read failed (either
// refusal would be a guess), else 409 benchmark.speech_unavailable.
func (p benchmarkViewPartition) refuseNothingRunnableSpeech(w http.ResponseWriter) bool {
	switch {
	case len(p.runnable) > 0:
		return false
	case p.unreadable > 0:
		writeBenchmarkRequestFailed(w)
	default:
		writeBenchmarkError(w, errBenchmarkSpeechUnavailable)
	}
	return true
}

// runSpeechTarget is runBenchmark's "speech" mode for one target, shaped like
// runVisionTarget: the measurement (measureSpeechTarget), the write of what it
// established (the output byte rate, under the MAPPING id -- the id the streamed relay
// reads it by, cachedOutputByteRate), and the target's kind=="speech" history row.
//
// A failed measurement writes no rate and leaves one already cached as it was: a
// failed re-run says nothing about the earlier observation, and the mapping simply
// keeps whatever it had (the audio_bytes fallback when that was nothing). The history
// row is appended on success AND failure, mirroring the speed and vision paths, with
// the metrics zero ("unknown") on a failure. Both writes are best-effort in the sense
// that a failed cache write is the result's error and a failed history write is only
// logged: neither aborts the run's other targets.
func (s *Server) runSpeechTarget(ctx context.Context, tgt benchmarkTarget) BenchmarkResult {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	m, err := s.measureSpeechTarget(ctx, tgt)
	if err == nil {
		err = s.cacheOutputByteRate(ctx, tgt.mapping.ID, m.byteRate)
	}
	row := routing.BenchmarkRun{
		MappingID: tgt.mapping.ID,
		ServerID:  tgt.server.ID,
		CreatedAt: time.Now().UTC(),
		Kind:      benchmarkKindSpeech,
	}
	if err != nil {
		res.Error = err.Error()
		row.Error = res.Error
	} else {
		res.OutputByteRate, res.TTFTMS, res.WallMS, res.RTF = m.byteRate, m.ttftMS, m.wallMS, m.rtf
		row.TTFTMS, row.WallMS, row.RTF = m.ttftMS, m.wallMS, m.rtf
	}
	if herr := s.Routes.InsertBenchmarkRun(ctx, row); herr != nil {
		slog.Warn("speech benchmark: history row write failed", "mapping_id", tgt.mapping.ID, "err", herr)
	}
	return res
}

// cacheOutputByteRate stores a mapping's output byte rate as its
// routing.ModelMappingCacheOutputByteRate row: scope "", the payload the decimal
// bytes per second, the exact form cachedOutputByteRate parses.
func (s *Server) cacheOutputByteRate(ctx context.Context, mappingID string, rate uint32) error {
	err := s.Routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{
		MappingID: mappingID,
		CacheType: routing.ModelMappingCacheOutputByteRate,
		Payload:   strconv.FormatUint(uint64(rate), 10),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("speech benchmark: could not cache the output byte rate: %w", err)
	}
	return nil
}

// measureSpeechTarget synthesizes one clip on the target's upstream and reads what it
// measured from the answer. It returns an error, and no figures at all, for anything
// that is not a 2xx audio/wav answer whose WAV header states a consistent byte rate:
// the rate is only ever read, never assumed.
//
// A benchmark run has no client to end it (it runs on a background context), and the
// server stays reserved until it returns, so the call is bounded as a benchmark stream
// is: by the larger of the application's timeout and the idle budget (coldStartBudget),
// which is long enough for an agent's router to start a cold child.
//
// The figures:
//
//   - the output byte rate is the WAV's own (wavByteRateFromReader);
//   - ttftMS is the time from sending the request to the response's first byte, which
//     for a non-stream synthesis is close to the synthesis time (audio.cpp answers a
//     one-shot call once the clip is done), not the time to the first audio of a
//     streamed call;
//   - wallMS is audio.cpp's own X-AudioCPP-Wall-Ms when it sent a usable one -- the
//     engine's time, without the network -- else the elapsed time to the end of the
//     body;
//   - rtf is audio.cpp's own X-AudioCPP-RTF when usable, else that wall time over the
//     length of audio produced, which is audio.cpp's X-AudioCPP-Audio-Duration-Ms
//     (the header the billing reads, speechDurationSeconds) or else the WAV's own
//     length (wavDurationFromReader); it stays 0 when no length can be established.
//
// A time too short to count is rounded up to one millisecond: a recorded zero means
// "not measured" (routing.BenchmarkRun), and a measurement is never that.
func (s *Server) measureSpeechTarget(ctx context.Context, tgt benchmarkTarget) (speechMeasurement, error) {
	proxy, ok := s.Provider.(provider.NativeProxyClient)
	if !ok {
		return speechMeasurement{}, errBenchmarkNoNativeProxy
	}
	target, _ := benchmarkTargetReq(tgt)
	body, err := json.Marshal(speechBenchmarkRequest{Model: tgt.mapping.AppModelName, Input: speechBenchmarkInput})
	if err != nil {
		return speechMeasurement{}, fmt.Errorf("speech benchmark: build request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, coldStartBudget(target, s.benchmarkStreamIdle()))
	defer cancel()
	ctx = s.upstreamAuthCtx(ctx, target)

	start := time.Now()
	resp, err := proxy.ProxyNative(ctx, target, upstreamPath(target, apiFlavorSpeech, endpointSpeech), body, jsonContentType)
	if err != nil {
		return speechMeasurement{}, fmt.Errorf("speech benchmark: synthesis request failed: %w", err)
	}
	defer resp.Body.Close()
	ttft := time.Since(start)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, speechBenchmarkErrorBytes))
		return speechMeasurement{}, fmt.Errorf("speech benchmark: the upstream answered HTTP %d: %s", resp.StatusCode, speechBenchmarkErrorText(errBody))
	}
	if contentType := resp.Header.Get("Content-Type"); !speechAudioMediaTypeOf(contentType) {
		return speechMeasurement{}, fmt.Errorf("speech benchmark: the answer is not labelled as WAV (Content-Type %q)", contentType)
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, speechBenchmarkMaxAudioBytes+1))
	wall := time.Since(start)
	if err != nil {
		return speechMeasurement{}, fmt.Errorf("speech benchmark: the answer could not be read to its end: %w", err)
	}
	switch {
	case len(audio) == 0:
		return speechMeasurement{}, errors.New("speech benchmark: the answer is empty")
	case len(audio) > speechBenchmarkMaxAudioBytes:
		return speechMeasurement{}, fmt.Errorf("speech benchmark: the answer exceeds %d bytes", speechBenchmarkMaxAudioBytes)
	}
	rate, ok := wavByteRateFromReader(bytes.NewReader(audio))
	if !ok {
		return speechMeasurement{}, errors.New("speech benchmark: the answer is not a WAV whose header states a consistent byte rate")
	}

	audioSeconds, haveAudio := speechDurationSeconds(resp.Header.Get(speechDurationHeader))
	if !haveAudio {
		if ms, ok := wavDurationFromReader(bytes.NewReader(audio)); ok && ms > 0 {
			audioSeconds, haveAudio = float64(ms)/1000, true
		}
	}
	wallSeconds := wall.Seconds()
	if ms, ok := speechPositiveHeader(resp.Header, speechWallHeader); ok {
		wallSeconds = ms / 1000
	}
	m := speechMeasurement{byteRate: rate, ttftMS: millisAtLeastOne(ttft.Seconds()), wallMS: millisAtLeastOne(wallSeconds)}
	if factor, ok := speechPositiveHeader(resp.Header, speechRTFHeader); ok {
		m.rtf = factor
	} else if haveAudio {
		m.rtf = wallSeconds / audioSeconds
	}
	return m, nil
}

// millisAtLeastOne converts a time in seconds to whole milliseconds, never below 1.
func millisAtLeastOne(seconds float64) int64 {
	return max(1, int64(math.Round(seconds*1000)))
}

// speechPositiveHeader parses a numeric response header and reports whether it was a
// usable measurement: a finite number greater than zero (speechDurationSeconds'
// rule). An absent header, a non-number, zero, a negative, NaN and Inf are all
// "not stated".
func speechPositiveHeader(h http.Header, name string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(name)), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, false
	}
	return v, true
}

// speechBenchmarkErrorText says what a non-2xx answer said, for a result's error: the
// message of an OpenAI-shaped error object when it has one (audio.cpp answers
// {"error":{"message":...}}), else the head of the body, trimmed and bounded. An empty
// body says so.
func speechBenchmarkErrorText(body []byte) string {
	var envelope apierror.Body
	if json.Unmarshal(body, &envelope) == nil {
		if msg := strings.TrimSpace(envelope.Error.Message); msg != "" {
			return truncateRunes(msg, speechBenchmarkErrorRunes)
		}
	}
	if text := strings.TrimSpace(string(body)); text != "" && utf8.Valid(body) {
		return truncateRunes(text, speechBenchmarkErrorRunes)
	}
	if len(body) == 0 {
		return "(empty body)"
	}
	return "(unreadable body)"
}

// truncateRunes cuts s to at most limit runes, marking a cut with an ellipsis.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "..."
}
