// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/benchaudio"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"sync"
	"time"
)

// This file is the audio benchmark's orchestration (#340): runBenchmark's speed, capacity and
// both modes for a mapping with the speech (TTS) or transcription (ASR) capability. It decides
// which single measurement a mapping gets (benchmark_audio_measure.go takes it), how often,
// against which target, and where the figures are stored: on the mapping
// (UpdateMappingAudioMetrics, under the metrics_locked guard, like the text figures) and in
// the mapping's benchmark history.
//
//   - speed: the text benchmark's cold-start technique -- ensureColdLoad, a cold pass, then a
//     warm pass of the same request -- with the warm pass as the measurement and the cold pass
//     as its warm-up. The load time is the cold pass's time to its first output minus the warm
//     pass's, recorded only for a confirmed cold start, and stored in the mapping's
//     load_time_ms like a text one.
//   - capacity: the concurrency ramp. Level k runs k measurements at once; it is good while its
//     slowest stream still produces (or transcribes) audio at least
//     OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR times faster than real time. The last good k and
//     why the ramp stopped are one result (routing.AudioMetrics).
//   - both: speed, then capacity.
//
// Every measurement goes straight to its target through the provider's native relay, like
// the speech benchmark's synthesis, never through the gateway's own routing: the run holds the
// server's reservation, which keeps routed requests away from it.

// The Kind of the audio benchmark's history rows: a speed run's and a ramp's. A row of either
// kind carries its own figures as JSON in the history's capacity_curve column
// (audioSpeedHistory, audioCapacityHistory), which the portal decodes by kind.
const (
	benchmarkKindAudio         = "audio"
	benchmarkKindAudioCapacity = "audio_capacity"
)

// The metrics_source an audio run stamps on the mapping: a speed run the text speed run's, a
// ramp the text capacity run's, so a both run ends "capacity" like a text both run.
const (
	audioMetricsSourceSpeed    = "benchmark"
	audioMetricsSourceCapacity = "capacity"
)

// The defaults of the two audio ramp settings, for a Server built without them
// (OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR and OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX).
const (
	defaultAudioRealtimeMinFactor = 1.2
	defaultAudioConcurrencyMax    = 8
)

// The agent router's codes for an instance it has no room for at that moment
// (runtime.instance_no_capacity) and for a burst instance whose demand is not measured yet
// (runtime.instance_demand_unknown). A ramp refused with either stops with the limit
// resources: the count is then a lower bound, not a ceiling. They are decided on by code, not
// by status: a 409 or a stream frame with any other code is a failure.
const (
	audioRampNoCapacityCode    = "runtime.instance_no_capacity"
	audioRampDemandUnknownCode = "runtime.instance_demand_unknown"
)

// audioTarget is what makes a benchmarkTarget an audio target: the role its capability rows
// name and whether it streams. The zero value is a target that is not one.
type audioTarget struct {
	// role is routing.RoleTTS or routing.RoleASR; "" for a target that is no audio target.
	role routing.ModelRole
	// streaming is the role's streaming verdict: speech_streaming for a TTS model,
	// transcription_streaming for an ASR model.
	streaming bool
}

// audioTargetOf derives a mapping's audio target from its stored capability rows and its
// effective runtime type: the role routing.DeriveRole names, so a mapping with both the
// transcription and the speech capability is measured as the transcriber it is grouped as,
// and the streaming verdict of that role. A mapping whose role is neither is no audio target.
func audioTargetOf(rows []routing.CapabilityRow, specType routing.RuntimeSpecType) audioTarget {
	byName := routing.CapabilityRowsByName(rows)
	switch role := routing.DeriveRole(specType, routing.YesCapabilities(rows)); role {
	case routing.RoleTTS:
		return audioTarget{role: role, streaming: byName[routing.CapabilitySpeechStreaming].Verdict == routing.CapabilityYes}
	case routing.RoleASR:
		return audioTarget{role: role, streaming: byName[routing.CapabilityTranscriptionStreaming].Verdict == routing.CapabilityYes}
	}
	return audioTarget{}
}

// audioMeasureKind is which of the four single measurements an audio target gets.
type audioMeasureKind int

const (
	audioKindTTSStream audioMeasureKind = iota + 1
	audioKindTTSBuffered
	audioKindASRLive
	audioKindASRBuffered
)

func (k audioMeasureKind) String() string {
	switch k {
	case audioKindTTSStream:
		return "tts-stream"
	case audioKindTTSBuffered:
		return "tts-buffered"
	case audioKindASRLive:
		return "asr-live"
	case audioKindASRBuffered:
		return "asr-buffered"
	}
	return "unknown"
}

// firstOutputMS is the time to the first output a client of this measurement could use, the
// figure a cold start is the difference of: the first speech.audio.delta of a streamed
// synthesis, the first transcript text of a live transcription, the first response byte of a
// one-shot synthesis (its WAV is read after it, and its length would only add the transfer),
// and the whole answer of a one-shot transcription, whose text is usable only once it is
// whole.
func (k audioMeasureKind) firstOutputMS(m audioMeasurement) int64 {
	switch k {
	case audioKindTTSBuffered:
		return m.FirstByteMS
	case audioKindASRBuffered:
		return m.WallMS
	default:
		return m.TTFTMS
	}
}

// audioMeasureCall is one measurement the orchestration takes: which kind, against exactly
// which target (an instance address m#k in ProviderModel included), and what it sends. warmUp
// and level say what the measurement is for -- the warm-up of a process the ramp is about to
// measure, and the ramp level it belongs to (0 outside the ramp) -- and change nothing the
// measurement sends but the passage a synthesis warm-up speaks.
type audioMeasureCall struct {
	kind     audioMeasureKind
	target   routing.Target
	text     string // the synthesis passage (TTS)
	voice    string // a clone model's preset voice (TTS)
	byteRate int    // the output byte rate a streamed synthesis counts its audio with
	warmUp   bool
	level    int
}

// audioMeasureBudget is the deadline of one audio measurement: the stream's first-data budget
// (coldStartBudget, long enough for an agent to start a cold child) plus the idle budget for
// the work itself, since a measurement is bounded as a whole and not only to its first data.
func (s *Server) audioMeasureBudget(target routing.Target) time.Duration {
	idle := s.benchmarkStreamIdle()
	return coldStartBudget(target, idle) + idle
}

// takeAudioMeasurement takes one measurement on its own deadline (audioMeasureBudget), with
// the target's upstream credential, through the primitive its kind names. A provider that
// cannot relay the request is an error. audioMeasureSeam, a test seam, takes it instead when
// set.
func (s *Server) takeAudioMeasurement(ctx context.Context, call audioMeasureCall) (audioMeasurement, error) {
	ctx, cancel := context.WithTimeout(ctx, s.audioMeasureBudget(call.target))
	defer cancel()
	ctx = s.upstreamAuthCtx(ctx, call.target)
	if s.audioMeasureSeam != nil {
		return s.audioMeasureSeam(ctx, call)
	}
	if call.kind == audioKindASRLive {
		duplex, ok := s.Provider.(provider.NativeDuplexProxyClient)
		if !ok {
			return audioMeasurement{}, errBenchmarkNoNativeProxy
		}
		pcm, sampleRate, _ := benchaudio.PCM()
		return measureASRLive(ctx, duplex, call.target, pcm, sampleRate, benchaudio.Language)
	}
	proxy, ok := s.Provider.(provider.NativeProxyClient)
	if !ok {
		return audioMeasurement{}, errBenchmarkNoNativeProxy
	}
	switch call.kind {
	case audioKindTTSStream:
		return measureTTSStream(ctx, proxy, call.target, call.text, call.voice, call.byteRate)
	case audioKindTTSBuffered:
		return measureTTSBuffered(ctx, proxy, call.target, call.text, call.voice)
	default:
		return measureASRBuffered(ctx, proxy, call.target, benchaudio.WAV(), benchaudio.Language)
	}
}

// audioCallFor decides the one measurement an audio target gets, and is the measurement every
// pass of its speed run and every stream of its ramp takes:
//   - a TTS model synthesizes the passage, as a stream when it streams (speech_streaming yes)
//     and in one piece otherwise, with a clone model's preset voice. A stream counts its audio
//     with the mapping's output byte rate, the one the speech benchmark caches; when none is
//     cached, the speech benchmark's non-streamed synthesis runs first and caches it
//     (runSpeechTarget, which records its own kind=speech history row);
//   - an ASR model transcribes the bundled clip live when it streams (transcription_streaming
//     yes) and its target can be sent a live session -- an agent target only when its agent
//     declared the live route (liveTranscriptionTargetServed's rule), and only through a
//     provider that relays a full-duplex body -- and in one piece otherwise.
func (s *Server) audioCallFor(ctx context.Context, tgt benchmarkTarget) (audioMeasureCall, error) {
	target, _ := benchmarkTargetReq(tgt)
	call := audioMeasureCall{target: target}
	if tgt.audio.role == routing.RoleASR {
		call.kind = audioKindASRBuffered
		if tgt.audio.streaming && s.audioLiveServed(tgt) {
			call.kind = audioKindASRLive
		}
		return call, nil
	}
	call.text, call.voice, call.kind = audioBenchmarkPassage, tgt.voice, audioKindTTSBuffered
	if !tgt.audio.streaming {
		return call, nil
	}
	call.kind = audioKindTTSStream
	if rate, ok := s.cachedOutputByteRate(ctx, tgt.mapping.ID); ok {
		call.byteRate = int(rate)
		return call, nil
	}
	speech := s.runSpeechTarget(ctx, tgt)
	if speech.Error != "" {
		return audioMeasureCall{}, fmt.Errorf("audio benchmark: the output byte rate a streamed synthesis is counted with could not be measured: %s", speech.Error)
	}
	call.byteRate = int(speech.OutputByteRate)
	return call, nil
}

// audioLiveServed reports whether tgt can be measured with a live transcription: through a
// provider that relays a full-duplex body, on a target that is not an agent's or whose agent
// declared fullduplex_proxy, the route the live relay itself requires.
func (s *Server) audioLiveServed(tgt benchmarkTarget) bool {
	if _, ok := s.Provider.(provider.NativeDuplexProxyClient); !ok {
		return false
	}
	return tgt.app.Type != routing.ProviderServerAgent || s.AgentFeatures.Has(tgt.server.ID, fullduplexProxyFeature)
}

// measureAudioTarget is runBenchmark's speed, capacity and both modes for an audio target:
// speed (runAudioSpeed), capacity (runAudioCapacity), or speed then capacity, so the ramp's
// write comes last and metrics_source ends "capacity", as a text both run's does. Each part
// stores what it measured and records its own history row. The result carries both parts'
// figures; its error is the first part's that failed, and the error returned with it as well.
func (s *Server) measureAudioTarget(ctx context.Context, run *benchmarkRun, tgt benchmarkTarget, mode string) (BenchmarkResult, error) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	var firstErr error
	if mode != "capacity" {
		speed, loadMS, err := s.runAudioSpeed(ctx, tgt)
		res.AudioRateX, res.AudioTTFTMS, res.AudioLoadTimeMS = speed.RateX, speed.TTFTMS, loadMS
		firstErr = err
	}
	if mode == "capacity" || mode == "both" {
		ramp, err := s.runAudioCapacity(ctx, run, tgt)
		if ramp.Limit != "" {
			res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit = ramp.Concurrency, ramp.Limit
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		res.Error = firstErr.Error()
	}
	return res, firstErr
}

// audioSpeedHistory is a kind=audio history row's JSON payload: the single-stream rate, the
// one speed figure the row has no column for (its time to first output, wall time, real-time
// factor and load time have theirs).
type audioSpeedHistory struct {
	RateX float64 `json:"audio_rate_x"`
}

// runAudioSpeed is the speed part of an audio run for one target: the measurement
// (measureAudioSpeed), its write on the mapping (UpdateMappingAudioMetrics: the rate, the time
// to first output and a confirmed load time, source benchmark; a locked mapping keeps its
// figures), and the target's kind=audio history row, appended on success and failure alike.
// A load time written for a replicable spec pushes the server's runtime config
// (pushBenchmarkLoadTime).
func (s *Server) runAudioSpeed(ctx context.Context, tgt benchmarkTarget) (audioMeasurement, int64, error) {
	m, loadMS, err := s.measureAudioSpeed(ctx, tgt)
	now := time.Now().UTC()
	row := routing.BenchmarkRun{MappingID: tgt.mapping.ID, ServerID: tgt.server.ID, CreatedAt: now, Kind: benchmarkKindAudio}
	if err != nil {
		m, loadMS = audioMeasurement{}, 0
		row.Error = err.Error()
	} else {
		payload, _ := json.Marshal(audioSpeedHistory{RateX: m.RateX})
		row.TTFTMS, row.WallMS, row.RTF, row.LoadTimeMS, row.CapacityCurve = m.TTFTMS, m.WallMS, m.RTF, int(loadMS), string(payload)
		metrics := routing.AudioMetrics{RateX: m.RateX, TTFAMS: m.TTFTMS, LoadTimeMS: loadMS}
		if werr := s.Routes.UpdateMappingAudioMetrics(ctx, tgt.mapping.ID, metrics, audioMetricsSourceSpeed, now); werr != nil {
			slog.Warn("audio benchmark: could not store the speed figures on the mapping", "mapping_id", tgt.mapping.ID, "err", werr)
		} else {
			s.pushBenchmarkLoadTime(tgt, int(loadMS))
		}
	}
	if herr := s.Routes.InsertBenchmarkRun(ctx, row); herr != nil {
		slog.Warn("audio benchmark: history row write failed", "mapping_id", tgt.mapping.ID, "err", herr)
	}
	return m, loadMS, err
}

// measureAudioSpeed measures one audio target's speed with the text benchmark's technique:
// ensureColdLoad, a cold pass, then a warm pass of the same measurement (audioCallFor). The
// warm pass is the measurement; the cold pass, which a load may lengthen, is its warm-up. The
// load time is the cold pass's time to its first output minus the warm pass's
// (audioMeasureKind.firstOutputMS), and it is recorded only when ensureColdLoad confirmed a
// cold start and the cold pass was the slower: unknown, never bogus, like a text load time.
// When the stop-all wrote, or may have written, the target's own force_stopped, the cold pass
// rides the router's 503 until the clear reaches the agent (audioColdPassAfterStop). An error
// from ensureColdLoad, a stop-all's failed clear, ends the measurement before any pass.
func (s *Server) measureAudioSpeed(ctx context.Context, tgt benchmarkTarget) (audioMeasurement, int64, error) {
	call, err := s.audioCallFor(ctx, tgt)
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	cold, err := s.ensureColdLoad(ctx, tgt)
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	var coldPass audioMeasurement
	if cold.rideOutStop {
		coldPass, err = s.audioColdPassAfterStop(ctx, call)
	} else {
		coldPass, err = s.takeAudioMeasurement(ctx, call)
	}
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	warm, err := s.takeAudioMeasurement(ctx, call)
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	var loadMS int64
	if coldFirst, warmFirst := call.kind.firstOutputMS(coldPass), call.kind.firstOutputMS(warm); cold.confirmed && warmFirst > 0 && coldFirst > warmFirst {
		loadMS = coldFirst - warmFirst
	}
	return warm, loadMS, nil
}

// audioColdPassAfterStop is coldPassAfterStop for an audio measurement: the router answers
// 503 runtime.admission_blocked until the clear of the target's own force_stopped reaches the
// agent, and the load loop (loadUntilServable) retries that refusal -- an audio refusal with
// status 503 is the one it waits out -- and keeps the attempt that was served, whose time to
// first output includes the whole start.
func (s *Server) audioColdPassAfterStop(ctx context.Context, call audioMeasureCall) (audioMeasurement, error) {
	var served audioMeasurement
	err := s.loadUntilServable(ctx, call.target, func(ctx context.Context, _ loadBound) error {
		m, err := s.takeAudioMeasurement(ctx, call)
		var merr *audioMeasureError
		if errors.As(err, &merr) && merr.Kind == audioMeasureUpstreamRefused && merr.Status == http.StatusServiceUnavailable {
			return fmt.Errorf("%w: %w", provider.ErrUpstreamStarting, err)
		}
		if err == nil {
			served = m
		}
		return err
	})
	if err != nil {
		return audioMeasurement{}, fmt.Errorf("cold pass after the benchmark's stop: %w", err)
	}
	return served, nil
}

// pushBenchmarkLoadTime pushes tgt's server its runtime config after a benchmark wrote a load
// time for tgt's mapping. A replicable spec's pushed idle timeout is derived from that
// load_time_ms, and the write is outside the portal service, whose write paths push a changed
// document themselves. A spec that is not replicable is pushed its stored idle timeout, which
// a load time does not change, so nothing is pushed for it.
func (s *Server) pushBenchmarkLoadTime(tgt benchmarkTarget, loadMS int) {
	if loadMS > 0 && tgt.spec.Replicable {
		s.PushRuntimeConfig(tgt.server.ID)
	}
}

// audioLevel is one level of the ramp: k streams at once, the slowest one's rate and the mean
// of their rates.
type audioLevel struct {
	K         int     `json:"k"`
	MinRateX  float64 `json:"min_rate_x"`
	MeanRateX float64 `json:"mean_rate_x"`
}

// audioCapacityResult is what a ramp established: the last good level (Concurrency, 0 when
// not even one stream was real-time or nothing could be measured) and why the ramp stopped
// there (Limit, one of routing.AudioLimitPerformance, AudioLimitResources, AudioLimitCap), the
// one result routing.AudioMetrics stores as a pair, and the curve of the levels it measured.
// A ramp that failed has no Limit.
type audioCapacityResult struct {
	Concurrency int
	Limit       string
	Curve       []audioLevel
}

// audioCapacityHistory is a kind=audio_capacity history row's JSON payload: the ramp's pair
// and its curve. A failed ramp carries no limit, and the curve of the levels it measured.
type audioCapacityHistory struct {
	RealtimeConcurrency int          `json:"audio_realtime_concurrency"`
	ConcurrencyLimit    string       `json:"audio_concurrency_limit,omitempty"`
	Curve               []audioLevel `json:"audio_curve"`
}

// runAudioCapacity is the capacity part of an audio run for one target: the ramp
// (measureAudioCapacity), with the level it probes published over the run's SSE; its pair on
// the mapping (UpdateMappingAudioMetrics, source capacity, the count and the limit written
// together, a measured 0 included; a locked mapping keeps its figures); and the target's
// kind=audio_capacity history row, appended on success and failure alike. A failed ramp
// stores no pair: it established no reason to stop, and a partial count would read as a
// lower bound the ramp never found.
func (s *Server) runAudioCapacity(ctx context.Context, run *benchmarkRun, tgt benchmarkTarget) (audioCapacityResult, error) {
	onLevel := func(k int) {
		run.setCurrentConcurrency(k)
		s.Benchmarks.publish(tgt.server.ID, run.snapshot())
	}
	ramp, err := s.measureAudioCapacity(ctx, tgt, onLevel)
	now := time.Now().UTC()
	curve := ramp.Curve
	if curve == nil {
		curve = []audioLevel{}
	}
	payload, _ := json.Marshal(audioCapacityHistory{RealtimeConcurrency: ramp.Concurrency, ConcurrencyLimit: ramp.Limit, Curve: curve})
	row := routing.BenchmarkRun{MappingID: tgt.mapping.ID, ServerID: tgt.server.ID, CreatedAt: now, Kind: benchmarkKindAudioCapacity, CapacityCurve: string(payload)}
	if err != nil {
		row.Error = err.Error()
	} else {
		metrics := routing.AudioMetrics{RealtimeConcurrency: ramp.Concurrency, ConcurrencyLimit: ramp.Limit}
		if werr := s.Routes.UpdateMappingAudioMetrics(ctx, tgt.mapping.ID, metrics, audioMetricsSourceCapacity, now); werr != nil {
			slog.Warn("audio benchmark: could not store the ramp's result on the mapping", "mapping_id", tgt.mapping.ID, "err", werr)
		}
	}
	if herr := s.Routes.InsertBenchmarkRun(ctx, row); herr != nil {
		slog.Warn("audio benchmark: history row write failed", "mapping_id", tgt.mapping.ID, "err", herr)
	}
	return ramp, err
}

// measureAudioCapacity is the ramp: level k = 1, 2, 3, ... runs k measurements at once
// (audioCallFor's) and records each stream's rate. Level k is good when its slowest stream's
// rate is at least the real-time threshold (audioRealtimeThreshold). The ramp stops:
//   - at the first level that is not good, with the limit performance and the level before as
//     the count -- a measured ceiling, 0 when not even one stream is real-time;
//   - when the agent refuses an instance for lack of room, at its ensure or at a stream
//     (audioRefusedForRoom), with the limit resources and the level before -- a lower bound,
//     the room its neighbours held at that moment;
//   - once level k reaches the ramp's cap (audioRampCap) or, for a ramp by instance, the
//     spec's max_instances, with the limit cap and k -- a lower bound.
//
// A replicable target on an agent known to run instances (audioRampByInstance) is ramped one
// instance per stream: level k first ensures instance k-1 (the address m#(k-1), m itself for
// k = 1) through the agent's ensure route, which starts it only into free room, then sends
// its k streams to instances 0 ... k-1. Any other target gets its k streams on its one process,
// the same concurrency inside one process a text capacity run measures. Every process a level
// adds is sent one warm-up first, so a start is not part of its rate. Any other failure ends
// the ramp as a failed benchmark, with the levels it measured.
//
// The ramp writes no override: the instances it starts are ordinary burst instances
// afterwards, which the agent unloads once they idle.
func (s *Server) measureAudioCapacity(ctx context.Context, tgt benchmarkTarget, onLevel func(int)) (audioCapacityResult, error) {
	var res audioCapacityResult
	call, err := s.audioCallFor(ctx, tgt)
	if err != nil {
		return res, err
	}
	byInstance := s.audioRampByInstance(tgt)
	capK := s.audioRampCap()
	if byInstance && tgt.spec.MaxInstances > 0 {
		capK = min(capK, tgt.spec.MaxInstances)
	}
	threshold := s.audioRealtimeThreshold()
	for k := 1; ; k++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		onLevel(k)
		if k == 1 || byInstance {
			fresh := call
			if byInstance {
				fresh.target.ProviderModel = audioInstanceAddress(tgt.mapping.AppModelName, k-1)
				if err := s.ensureAudioInstance(ctx, fresh.target); err != nil {
					return audioRampStop(res, k, err)
				}
			}
			if _, err := s.takeAudioMeasurement(ctx, audioWarmUp(fresh)); err != nil {
				return audioRampStop(res, k, err)
			}
		}
		rates, err := s.measureAudioLevel(ctx, call, tgt.mapping.AppModelName, k, byInstance)
		if err != nil {
			return audioRampStop(res, k, err)
		}
		level := audioLevelOf(k, rates)
		res.Curve = append(res.Curve, level)
		switch {
		case level.MinRateX < threshold:
			res.Concurrency, res.Limit = k-1, routing.AudioLimitPerformance
			return res, nil
		case k >= capK:
			res.Concurrency, res.Limit = k, routing.AudioLimitCap
			return res, nil
		}
	}
}

// audioRampStop ends a ramp at level k on err: a refusal for lack of room
// (audioRefusedForRoom) is the limit resources with the level before; anything else fails
// the ramp.
func audioRampStop(res audioCapacityResult, k int, err error) (audioCapacityResult, error) {
	if audioRefusedForRoom(err) {
		res.Concurrency, res.Limit = k-1, routing.AudioLimitResources
		return res, nil
	}
	return res, fmt.Errorf("audio benchmark: concurrency level %d: %w", k, err)
}

// audioWarmUp is the warm-up of the process call's target names: the same measurement, with a
// synthesis speaking the speech benchmark's one short sentence instead of the passage. A
// transcription warms up on the bundled clip itself, the only audio the benchmark has.
func audioWarmUp(call audioMeasureCall) audioMeasureCall {
	call.warmUp, call.level = true, 0
	if call.kind == audioKindTTSStream || call.kind == audioKindTTSBuffered {
		call.text = speechBenchmarkInput
	}
	return call
}

// measureAudioLevel runs level k: k measurements of call at once, each to its own instance
// (instance i for stream i) when byInstance, else all to call's target, and returns their
// rates. Every stream is waited for. A failure that is no refusal for lack of room wins over
// one that is: the level failed, and a lower bound read off it would be wrong.
func (s *Server) measureAudioLevel(ctx context.Context, call audioMeasureCall, model string, k int, byInstance bool) ([]float64, error) {
	rates := make([]float64, k)
	errs := make([]error, k)
	var wg sync.WaitGroup
	for i := range k {
		stream := call
		stream.level = k
		if byInstance {
			stream.target.ProviderModel = audioInstanceAddress(model, i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.takeAudioMeasurement(ctx, stream)
			rates[i], errs[i] = m.RateX, err
		}()
	}
	wg.Wait()
	var refused error
	for _, err := range errs {
		switch {
		case err == nil:
		case audioRefusedForRoom(err):
			refused = err
		default:
			return nil, err
		}
	}
	if refused != nil {
		return nil, refused
	}
	return rates, nil
}

// audioLevelOf is level k's figures from its streams' rates.
func audioLevelOf(k int, rates []float64) audioLevel {
	level := audioLevel{K: k, MinRateX: math.Inf(1)}
	var sum float64
	for _, r := range rates {
		level.MinRateX = min(level.MinRateX, r)
		sum += r
	}
	level.MeanRateX = sum / float64(len(rates))
	return level
}

// ensureAudioInstance starts the agent instance target addresses through the agent router's
// ensure route and returns once it runs. The router starts a burst instance only into free
// room, evicting nothing, and refuses it otherwise (audioRefusedForRoom). The call is bounded
// like a load (newLoadBound) and carries the target's credential.
func (s *Server) ensureAudioInstance(ctx context.Context, target routing.Target) error {
	ensurer, ok := s.Provider.(provider.RuntimeEnsurer)
	if !ok {
		return errBenchmarkNoRuntimeEnsure
	}
	ctx, cancel := context.WithTimeout(ctx, s.newLoadBound(target).length)
	defer cancel()
	if err := ensurer.EnsureRuntimeModel(s.upstreamAuthCtx(ctx, target), target); err != nil {
		return fmt.Errorf("ensure %s: %w", target.ProviderModel, err)
	}
	return nil
}

// audioRefusedForRoom reports whether err is the agent refusing an instance for lack of room:
// an ensure or a measurement refused with runtime.instance_no_capacity or
// runtime.instance_demand_unknown. It decides on the code alone -- a 409 the router answers
// for anything else, and a stream's error frame, carry other codes, and the agent's /live
// route answers these with a 409 before any frame while a lazily committed synthesis stream
// ends with them in a frame.
func audioRefusedForRoom(err error) bool {
	code := ""
	var merr *audioMeasureError
	var rerr *provider.RouterError
	switch {
	case errors.As(err, &merr) && merr.Kind == audioMeasureUpstreamRefused:
		code = merr.Code
	case errors.As(err, &rerr):
		code = rerr.Code
	}
	return code == audioRampNoCapacityCode || code == audioRampDemandUnknownCode
}

// audioRampByInstance reports whether tgt is ramped one instance per stream: a replicable spec
// of a server_agent application whose agent is known to declare runtime_instances. An agent
// without the feature, or not heard from since the gateway started, runs the spec as one
// process whatever the spec says, so its ramp is one process's.
func (s *Server) audioRampByInstance(tgt benchmarkTarget) bool {
	return tgt.app.Type == routing.ProviderServerAgent && tgt.spec.Replicable && s.AgentFeatures.Has(tgt.server.ID, runtimeInstancesFeature)
}

// audioInstanceAddress is the address the agent serves instance k of model under: model
// itself for instance 0, model#k for every other.
func audioInstanceAddress(model string, k int) string {
	if k == 0 {
		return model
	}
	return model + "#" + strconv.Itoa(k)
}

// audioRealtimeThreshold is the rate a ramp level's slowest stream must reach to be good
// (OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR), its default for a Server built without one.
func (s *Server) audioRealtimeThreshold() float64 {
	if s.audioRealtimeMinFactor > 0 {
		return s.audioRealtimeMinFactor
	}
	return defaultAudioRealtimeMinFactor
}

// audioRampCap is the highest level a ramp measures (OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX), its
// default for a Server built without one.
func (s *Server) audioRampCap() int {
	if s.audioConcurrencyMax > 0 {
		return s.audioConcurrencyMax
	}
	return defaultAudioConcurrencyMax
}

// benchmarkModeMeasuresAudio reports whether a run of mode measures an audio target's audio:
// the speed, capacity and both modes (an empty mode is speed). The vision probe and the speech
// rate benchmark have their own targets.
func benchmarkModeMeasuresAudio(mode string) bool {
	switch mode {
	case "", "speed", "capacity", "both":
		return true
	}
	return false
}

// positiveOr is v when it is positive, else def: a setting's default for a value that is
// unset (0), negative or, for a float, not a number.
func positiveOr[T int | float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}
