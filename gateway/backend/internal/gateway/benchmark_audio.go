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
	"slices"
	"strconv"
	"strings"
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
//     OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR times faster than real time, counted from the
//     send, so a stream its process holds back behind the others is that much slower. The
//     last good k and why the ramp stopped are one result (routing.AudioMetrics).
//   - both: speed, then capacity.
//
// Every measurement goes straight to its target through the provider's native relay, like
// the speech benchmark's synthesis, never through the gateway's own routing: the run holds the
// server's reservation, which keeps routed requests away from it.

// The Kind of the audio benchmark's history rows: a speed run's and a ramp's. A row of either
// kind carries its own figures as JSON in the history's capacity_curve column
// (routing.AudioSpeedHistory, routing.AudioCapacityHistory), which the portal decodes by kind
// with the same types.
const (
	benchmarkKindAudio         = routing.BenchmarkKindAudio
	benchmarkKindAudioCapacity = routing.BenchmarkKindAudioCapacity
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

// audioRampNotManagedCode is the agent router's answer to an instance address m#k its spec
// does not allow: the spec is not replicable in the document the agent holds. A ramp by
// instance meets it only while the restore it wrote (audioRampReplicableAgain) is still on
// its way to the agent, and waits it out (ensureAudioInstance).
const audioRampNotManagedCode = "runtime.model_not_managed"

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
	// timesOnly takes a streamed synthesis without counting its audio (measureTTSStreamTimes):
	// the cold pass of one whose byte rate is not cached yet, which lends the load time only its
	// time to the first audio. Never set on a pass whose rate or real-time factor is read.
	timesOnly bool
	warmUp    bool
	level     int
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
		if call.timesOnly {
			return audioMeasurer{now: time.Now}.measureTTSStreamTimes(ctx, proxy, call.target, call.text, call.voice)
		}
		return measureTTSStream(ctx, proxy, call.target, call.text, call.voice, call.byteRate)
	case audioKindTTSBuffered:
		return measureTTSBuffered(ctx, proxy, call.target, call.text, call.voice)
	default:
		return measureASRBuffered(ctx, proxy, call.target, benchaudio.WAV(), benchaudio.Language)
	}
}

// audioCallFor decides the one measurement an audio target gets, and is the measurement every
// pass of its speed run and every stream of its ramp takes. measureAudioTarget asks it once per
// target and run (audioPrepared), so its one request of its own -- the speech benchmark for a
// missing byte rate -- is sent at most once per run, and its outcome, a failure included, is
// every part's:
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
	call := s.audioCallPlan(ctx, tgt)
	if !call.needsByteRate() {
		return call, nil
	}
	speech := s.runSpeechTarget(ctx, tgt)
	if speech.Error != "" {
		return audioMeasureCall{}, fmt.Errorf("audio benchmark: the output byte rate a streamed synthesis is counted with could not be measured: %s", speech.Error)
	}
	call.byteRate = int(speech.OutputByteRate)
	return call, nil
}

// audioCallPlan is audioCallFor without its one request: the measurement decided from what is
// known, a streamed synthesis carrying the cached byte rate or none (needsByteRate) when no rate
// is cached. It sends nothing, so it may be asked before a cold start is judged.
func (s *Server) audioCallPlan(ctx context.Context, tgt benchmarkTarget) audioMeasureCall {
	target, _ := benchmarkTargetReq(tgt)
	call := audioMeasureCall{target: target}
	if tgt.audio.role == routing.RoleASR {
		call.kind = audioKindASRBuffered
		if tgt.audio.streaming && s.audioLiveServed(tgt) {
			call.kind = audioKindASRLive
		}
		return call
	}
	call.text, call.voice, call.kind = audioBenchmarkPassage, tgt.voice, audioKindTTSBuffered
	if !tgt.audio.streaming {
		return call
	}
	call.kind = audioKindTTSStream
	if rate, ok := s.cachedOutputByteRate(ctx, tgt.mapping.ID); ok {
		call.byteRate = int(rate)
	}
	return call
}

// needsByteRate reports whether call is a streamed synthesis with no byte rate to count its
// audio with yet: audioCallFor runs the speech benchmark for one.
func (c audioMeasureCall) needsByteRate() bool {
	return c.kind == audioKindTTSStream && c.byteRate <= 0
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

// audioPrepared is the measurement an audio target's parts take in one run (audioCallFor),
// decided once for the run, or why it could not be: the outcome of the speech benchmark a
// streamed synthesis without a cached byte rate needs first. A both run's capacity part fails
// with the speed part's error then, instead of sending the failing request again.
type audioPrepared struct {
	call audioMeasureCall
	err  error
}

// audioPreparer prepares an audio target's measurement once per run, on its first ask
// (audioCallFor), and hands every later ask the same outcome. The speed part asks only after
// its cold pass, since the preparation may send the target a request of its own.
type audioPreparer struct {
	s    *Server
	tgt  benchmarkTarget
	done bool
	prep audioPrepared
}

func (p *audioPreparer) prepared(ctx context.Context) audioPrepared {
	if !p.done {
		p.prep.call, p.prep.err = p.s.audioCallFor(ctx, p.tgt)
		p.done = true
	}
	return p.prep
}

// measureAudioTarget is runBenchmark's speed, capacity and both modes for an audio target:
// speed (runAudioSpeed), capacity (runAudioCapacity), or speed then capacity, so the ramp's
// write comes last and metrics_source ends "capacity", as a text both run's does. The
// measurement both parts take is prepared once (audioPreparer). Each part stores what it
// measured and records its own history row. The result carries both parts' figures; its
// error is the first part's that failed, and the error returned with it as well.
func (s *Server) measureAudioTarget(ctx context.Context, run *benchmarkRun, tgt benchmarkTarget, mode string) (BenchmarkResult, error) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	prep := &audioPreparer{s: s, tgt: tgt}
	var firstErr error
	if mode != "capacity" {
		speed, loadMS, err := s.runAudioSpeed(ctx, tgt, prep)
		res.AudioRateX, res.AudioTTFTMS, res.AudioLoadTimeMS = speed.RateX, speed.TTFTMS, loadMS
		firstErr = err
	}
	if mode == "capacity" || mode == "both" {
		ramp, err := s.runAudioCapacity(ctx, run, tgt, prep.prepared(ctx))
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

// runAudioSpeed is the speed part of an audio run for one target: the measurement
// (measureAudioSpeed), its write on the mapping (UpdateMappingAudioMetrics: the rate, the time
// to first output and a confirmed load time, source benchmark; a locked mapping keeps its
// figures), and the target's kind=audio history row, appended on success and failure alike.
// A load time written for a replicable spec pushes the server's runtime config
// (pushBenchmarkLoadTime).
func (s *Server) runAudioSpeed(ctx context.Context, tgt benchmarkTarget, prep *audioPreparer) (audioMeasurement, int64, error) {
	m, loadMS, err := s.measureAudioSpeed(ctx, tgt, prep)
	now := time.Now().UTC()
	row := routing.BenchmarkRun{MappingID: tgt.mapping.ID, ServerID: tgt.server.ID, CreatedAt: now, Kind: benchmarkKindAudio}
	if err != nil {
		m, loadMS = audioMeasurement{}, 0
		row.Error = err.Error()
	} else {
		payload, _ := json.Marshal(routing.AudioSpeedHistory{RateX: m.RateX})
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
// ensureColdLoad, a cold pass, then a warm pass of the same measurement (prep's). The
// warm pass is the measurement; the cold pass, which a load may lengthen, is its warm-up. The
// load time is the cold pass's time to its first output minus the warm pass's
// (audioMeasureKind.firstOutputMS), and it is recorded only when ensureColdLoad confirmed a
// cold start and the cold pass was the slower: unknown, never bogus, like a text load time.
// When the stop-all wrote, or may have written, the target's own force_stopped, the cold pass
// rides the router's 503 until the clear reaches the agent (audioColdPassAfterStop). An error
// from ensureColdLoad, a stop-all's failed clear, ends the measurement before any pass.
//
// Nothing is sent to the target before the cold pass: the cold start is judged and taken first,
// and only then is the measurement prepared (prep), which for a streamed synthesis without a
// cached byte rate runs the speech benchmark -- a synthesis on the target, which a run that does
// not stop the server would otherwise find resident and judge warm. That cold pass is timed
// without counting its audio (audioMeasureCall.timesOnly), all its part of the load time needs;
// a measurement that could not be prepared then ends the run before the warm pass.
func (s *Server) measureAudioSpeed(ctx context.Context, tgt benchmarkTarget, prep *audioPreparer) (audioMeasurement, int64, error) {
	coldCall := s.audioCallPlan(ctx, tgt)
	coldCall.timesOnly = coldCall.needsByteRate()
	cold, err := s.ensureColdLoad(ctx, tgt)
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	var coldPass audioMeasurement
	if cold.rideOutStop {
		coldPass, err = s.audioColdPassAfterStop(ctx, coldCall)
	} else {
		coldPass, err = s.takeAudioMeasurement(ctx, coldCall)
	}
	if err != nil {
		return audioMeasurement{}, 0, err
	}
	prepared := prep.prepared(ctx)
	if prepared.err != nil {
		return audioMeasurement{}, 0, prepared.err
	}
	call := prepared.call
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
// of their rates, each the rate the level judges the stream by (audioRampRate). It is the
// history payload's own type (routing.AudioLevel), so the curve is stored as measured.
type audioLevel = routing.AudioLevel

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

// runAudioCapacity is the capacity part of an audio run for one target: the ramp
// (measureAudioCapacity), with the level it probes published over the run's SSE; its pair on
// the mapping (UpdateMappingAudioMetrics, source capacity, the count and the limit written
// together, a measured 0 included; a locked mapping keeps its figures); and the target's
// kind=audio_capacity history row, appended on success and failure alike. A failed ramp
// stores no pair: it established no reason to stop, and a partial count would read as a
// lower bound the ramp never found.
func (s *Server) runAudioCapacity(ctx context.Context, run *benchmarkRun, tgt benchmarkTarget, prep audioPrepared) (audioCapacityResult, error) {
	onLevel := func(k int) {
		run.setCurrentConcurrency(k)
		s.Benchmarks.publish(tgt.server.ID, run.snapshot())
	}
	ramp, err := s.measureAudioCapacity(ctx, tgt, prep, onLevel)
	now := time.Now().UTC()
	curve := ramp.Curve
	if curve == nil {
		curve = []audioLevel{}
	}
	payload, _ := json.Marshal(routing.AudioCapacityHistory{RealtimeConcurrency: ramp.Concurrency, ConcurrencyLimit: ramp.Limit, Curve: curve})
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
// (prep's, audioCallFor) and records each stream's rate -- for a streamed synthesis with its wait
// for the first audio beyond a lone stream's counted in (audioRampRate). Level k is good
// when its slowest stream's rate is at least the real-time threshold
// (audioRealtimeThreshold). The ramp stops:
//   - at the first level that is not good, with the limit performance and the level before as
//     the count -- a measured ceiling, 0 when not even one stream is real-time;
//   - when the agent refuses an instance for lack of room, at its ensure or at a stream
//     (audioRefusedForRoom), with the limit resources and the level before -- a lower bound,
//     the room its neighbours held at that moment;
//   - once level k reaches the ramp's cap (audioRampCap) or, for a ramp by instance, the
//     spec's max_instances or the agent's instance index cap (routing.MaxInstanceIndex + 1
//     instances, m#63 the last), with the limit cap and k -- a lower bound.
//
// A replicable target on an agent known to run instances (audioRampByInstance) is ramped one
// instance per stream: level k first ensures instance k-1 (the address m#(k-1), m itself for
// k = 1) through the agent's ensure route, which starts it only into free room, then sends
// its k streams to instances 0 ... k-1. A replicable target whose agent is not known to run
// instances is refused (audioRampShapeKnown). Any other target gets its k streams on its one
// process, the same concurrency inside one process a text capacity run measures. Every process a level
// adds is sent one warm-up first, so a start is not part of its rate. Any other failure ends
// the ramp as a failed benchmark, with the levels it measured.
//
// The ramp writes no override of its own: the instances it starts are ordinary burst
// instances afterwards, which the agent unloads once they idle. It needs its spec
// replicable, so in a both run that holds the spec as one process for its speed part, the
// ramp by instance first makes it replicable again (audioRampReplicableAgain), waits for the
// protected instances that starts by themselves (ensureAudioFloor), and afterwards holds the
// spec as one process again when a later target of the run may still stop the server's
// models (audioRampOneProcessAgain).
func (s *Server) measureAudioCapacity(ctx context.Context, tgt benchmarkTarget, prep audioPrepared, onLevel func(int)) (audioCapacityResult, error) {
	var res audioCapacityResult
	if prep.err != nil {
		return res, prep.err
	}
	call := prep.call
	byInstance := s.audioRampByInstance(tgt)
	if err := s.audioRampShapeKnown(tgt, byInstance); err != nil {
		return res, err
	}
	var err error
	restored := false
	up := 0 // the instances at addrs[0 ... up-1] are known to run
	var addrs []string
	if byInstance {
		if addrs, err = s.audioRampAddresses(ctx, tgt); err != nil {
			return res, err
		}
		if restored, err = s.audioRampReplicableAgain(ctx, tgt); err != nil {
			return res, err
		}
	}
	if restored {
		defer s.audioRampOneProcessAgain(ctx, tgt)
		if up, err = s.ensureAudioFloor(ctx, tgt, call.target, addrs); err != nil {
			return res, err
		}
	}
	capK := s.audioRampCap()
	if byInstance {
		capK = min(capK, len(addrs))
	}
	threshold := s.audioRealtimeThreshold()
	var loneFirstMS int64 // level 1's stream's time to first output (audioRampRate)
	for k := 1; ; k++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		onLevel(k)
		if k == 1 || byInstance {
			fresh := call
			if byInstance {
				fresh.target.ProviderModel = addrs[k-1]
				if k > up {
					if err := s.ensureAudioInstance(ctx, fresh.target, restored); err != nil {
						return audioRampStop(res, k, err)
					}
				}
			}
			if _, err := s.takeAudioMeasurement(ctx, audioWarmUp(fresh)); err != nil {
				return audioRampStop(res, k, err)
			}
		}
		var levelAddrs []string
		if byInstance {
			levelAddrs = addrs[:k]
		}
		streams, err := s.measureAudioLevel(ctx, call, k, levelAddrs)
		if err != nil {
			return audioRampStop(res, k, err)
		}
		if k == 1 {
			loneFirstMS = streams[0].TTFTMS
		}
		rates := make([]float64, k)
		for i, m := range streams {
			rates[i] = audioRampRate(call.kind, m, loneFirstMS)
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
// (stream i to addrs[i]) when addrs is given, else all to call's target, and returns them.
// Every stream is waited for. A failure that is no refusal for lack of room wins over one
// that is: the level failed, and a lower bound read off it would be wrong.
func (s *Server) measureAudioLevel(ctx context.Context, call audioMeasureCall, k int, addrs []string) ([]audioMeasurement, error) {
	streams := make([]audioMeasurement, k)
	errs := make([]error, k)
	var wg sync.WaitGroup
	for i := range k {
		stream := call
		stream.level = k
		if addrs != nil {
			stream.target.ProviderModel = addrs[i]
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			streams[i], errs[i] = s.takeAudioMeasurement(ctx, stream)
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
	return streams, nil
}

// audioRampRate is the rate a ramp level judges one of its streams by. For a one-shot
// synthesis, a one-shot transcription and a live transcription it is the stream's rate,
// which already runs from the send, so a stream its process held back behind the others
// is slower by exactly that wait. A streamed synthesis's rate runs only from its first
// audio -- the speed once audio flows -- so a process that serves the level's streams one
// after another would read as fast as a lone stream for every one of them. Its wait for
// that first audio beyond the lone stream's (loneFirstMS, level 1's) is therefore counted
// as part of its production: the rate is its audio over the production time plus that
// wait. A stream that waited no longer than the lone one keeps its rate, so level 1 is
// judged by the very rate a speed run stores.
func audioRampRate(kind audioMeasureKind, m audioMeasurement, loneFirstMS int64) float64 {
	waited := m.TTFTMS - loneFirstMS
	if kind != audioKindTTSStream || waited <= 0 || m.RateX <= 0 {
		return m.RateX
	}
	production := m.AudioSeconds / m.RateX
	return m.AudioSeconds / (production + float64(waited)/1000)
}

// audioLevelOf is level k's figures from the rates its streams are judged by
// (audioRampRate).
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
// room, evicting nothing, and refuses it otherwise (audioRefusedForRoom). Each call is bounded
// like a load (newLoadBound) and carries the target's credential.
//
// After the ramp's own restore (restored), the agent may still hold the document that runs
// the spec as one process, and refuses an instance address with 404
// runtime.model_not_managed until the restore reaches it -- over its stream at once, or at
// its next poll. That refusal is retried every coldLoadPollGap for as long as the stop path
// waits for a document to reach an agent (benchmarkStopWaitBound); any other answer, and
// that one once the bound is spent, is the ensure's.
func (s *Server) ensureAudioInstance(ctx context.Context, target routing.Target, restored bool) error {
	ensurer, ok := s.Provider.(provider.RuntimeEnsurer)
	if !ok {
		return errBenchmarkNoRuntimeEnsure
	}
	deadline := time.Now().Add(benchmarkStopWaitBound)
	for {
		err := s.ensureAudioInstanceOnce(ctx, ensurer, target)
		var rerr *provider.RouterError
		if err == nil || !restored || !errors.As(err, &rerr) || rerr.Code != audioRampNotManagedCode {
			return err
		}
		if time.Until(deadline) <= coldLoadPollGap || !sleepCtx(ctx, coldLoadPollGap) {
			return err
		}
	}
}

// ensureAudioInstanceOnce is one ensure of ensureAudioInstance.
func (s *Server) ensureAudioInstanceOnce(ctx context.Context, ensurer provider.RuntimeEnsurer, target routing.Target) error {
	ctx, cancel := context.WithTimeout(ctx, s.newLoadBound(target).length)
	defer cancel()
	if err := ensurer.EnsureRuntimeModel(s.upstreamAuthCtx(ctx, target), target); err != nil {
		return fmt.Errorf("ensure %s: %w", target.ProviderModel, err)
	}
	return nil
}

// audioRampReplicableAgain makes tgt's spec replicable again before its ramp by instance when
// the run holds it as one process -- a manual both run, whose speed part measured it alone
// (beginBenchmarkSingleProcess) -- and reports whether it took the spec out of the run's
// override lease: the agent refuses every instance address of a spec it holds as not
// replicable, and the ramp is the measurement of exactly those instances. The write is the
// restore the run's end would make (restoreBenchmarkReplicable), made now, and the lease is
// rewritten without the spec, so the end does not make it again. A restore that failed leaves
// the spec in the lease for the run's end and fails the ramp: one process cannot be measured
// as instances. A target the run does not hold as one process needs nothing.
//
// A restore that was not written -- a Conflict or a gone spec -- takes the spec out of the
// lease as well, since the run's end owes it nothing, but it may leave the spec not
// replicable: an operator gave it a fixed listen port while the run held it, which a
// replicable spec cannot have, and the operator's setting stands. The spec is then read
// again (audioRampStillReplicable), and one that is not replicable, or gone, fails the ramp
// at once: the agent would refuse every instance address of it for good.
//
// Replicable again, the spec's protected instances start by themselves, as a pinned spec's
// one does: right away, beside the process the ramp is about to measure (ensureAudioFloor
// waits for them), and at every later stop's clear, inside a later target's cold pass. So
// after the ramp, the spec is held as one process again for as long as a later target may
// still stop the server's models (audioRampOneProcessAgain).
func (s *Server) audioRampReplicableAgain(ctx context.Context, tgt benchmarkTarget) (bool, error) {
	ov := tgt.overrides
	if ov == nil || !slices.Contains(ov.singleProcess, tgt.spec.ID) {
		return false, nil
	}
	out := s.restoreBenchmarkReplicableOutcome(ctx, tgt.server.ID, []string{tgt.spec.ID})
	if len(out.Failed) > 0 {
		return false, fmt.Errorf("audio benchmark: launch spec %s could not be made replicable again for its concurrency ramp, which measures its instances", tgt.spec.ID)
	}
	ov.singleProcess = benchmarkWithout(ov.singleProcess, []string{tgt.spec.ID})
	if err := s.writeBenchmarkLease(ctx, tgt.server.ID, ov); err != nil {
		slog.Warn("audio benchmark: could not update the override lease after making the ramp's spec replicable again; it is rewritten when the run ends", "server_id", tgt.server.ID, "spec_id", tgt.spec.ID, "err", err)
	}
	if !slices.Contains(out.Written, tgt.spec.ID) {
		if err := s.audioRampStillReplicable(ctx, tgt.spec.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// audioRampStillReplicable reads launch spec specID again after a restore that was not
// written (audioRampReplicableAgain) and returns nil only when it is stored replicable -- an
// operator turned the flag back on during the run, or the run's own write never stored
// false. A spec that is not replicable is the ramp's error, naming the fixed listen port that
// keeps it one process when it has one, and so is a spec that is gone or could not be read.
func (s *Server) audioRampStillReplicable(ctx context.Context, specID string) error {
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, specID)
	switch {
	case err != nil:
		return fmt.Errorf("audio benchmark: read launch spec %s again after its replicable restore was not written: %w", specID, err)
	case !ok:
		return fmt.Errorf("audio benchmark: launch spec %s is gone, so its concurrency ramp, which measures its instances, has nothing to measure", specID)
	case spec.Replicable:
		return nil
	case spec.ListenPort != 0:
		return fmt.Errorf("audio benchmark: launch spec %s now listens on fixed port %d, which keeps it one process, so its concurrency ramp, which measures its instances, cannot run; remove the fixed port and turn multiple instances on to ramp it", specID, spec.ListenPort)
	}
	return fmt.Errorf("audio benchmark: launch spec %s is not replicable any more, so its concurrency ramp, which measures its instances, cannot run", specID)
}

// ensureAudioFloor waits, before a ramp whose spec it has just made replicable again
// (audioRampReplicableAgain), for the instances the agent then starts by itself with no
// request: the protected ones, 0 ... audioProtectedFloor-1. It ensures them lowest first, the
// order the agent starts them in, and each ensure returns once that instance runs, so no
// level is measured while one of them still loads beside the measured process. A protected
// index whose address another mapping names is not among addrs (audioRampAddresses): the
// agent never runs it. It returns how many of addrs, from the first on, are known to run,
// which the ramp does not ensure again.
// The agent admits a protected instance like a baseline start, not like a burst one: it may
// wait for room or evict an idle neighbour for it, and the ensure answers only once it runs,
// but it never refuses it for room. So an ensure that fails at all -- a start that failed or
// timed out, whatever code it carries -- fails the ramp: the floor the ramp would be measured
// beside never came up. A floor of one instance at most starts nothing beside the process
// the speed part left running.
func (s *Server) ensureAudioFloor(ctx context.Context, tgt benchmarkTarget, target routing.Target, addrs []string) (int, error) {
	floor := audioProtectedFloor(tgt.spec)
	if floor <= 1 {
		return 0, nil
	}
	up := 0
	for _, addr := range addrs {
		if audioAddressIndex(tgt.mapping.AppModelName, addr) >= floor {
			break
		}
		target.ProviderModel = addr
		if err := s.ensureAudioInstance(ctx, target, true); err != nil {
			return up, fmt.Errorf("audio benchmark: the protected instances of launch spec %s, which start once it is replicable again: %w", tgt.spec.ID, err)
		}
		up++
	}
	return up, nil
}

// audioRampAddresses is the addresses a ramp by instance measures, in order: instance 0's,
// then every instance k the agent can run -- below max_instances when the spec sets one, and
// up to its index cap -- whose address is not another mapping's upstream model on the same
// agent. The agent resolves a literal upstream name before it reads an instance address, so
// an ensure of such an address would start the other spec, and the agent never runs that
// instance of this one; the instance scaler skips the same indexes (instanceIndex). The
// agent's specs are the mappings of the server's server_agent application.
func (s *Server) audioRampAddresses(ctx context.Context, tgt benchmarkTarget) ([]string, error) {
	mappings, err := s.Routes.MappingsByApplication(ctx, tgt.app.ID)
	if err != nil {
		return nil, fmt.Errorf("audio benchmark: read the agent's mappings for the instance addresses: %w", err)
	}
	others := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		if m.ID != tgt.mapping.ID {
			others[m.AppModelName] = true
		}
	}
	model := tgt.mapping.AppModelName
	last := routing.MaxInstanceIndex
	if tgt.spec.MaxInstances > 0 {
		last = min(last, tgt.spec.MaxInstances-1)
	}
	addrs := []string{model}
	for k := 1; k <= last; k++ {
		if addr := audioInstanceAddress(model, k); !others[addr] {
			addrs = append(addrs, addr)
		}
	}
	return addrs, nil
}

// audioAddressIndex is the instance index addr names for model (audioInstanceAddress's
// inverse): 0 for model itself, k for model#k.
func audioAddressIndex(model, addr string) int {
	k, err := strconv.Atoi(strings.TrimPrefix(addr, model+"#"))
	if addr == model || err != nil {
		return 0
	}
	return k
}

// audioProtectedFloor is how many instances of a replicable spec the agent keeps running
// during a run with no request at all: its min_instances, bounded as the agent bounds them
// by max_instances and the index cap. A pin, the other way an instance is protected, is
// lifted for a run that holds a spec as one process (beginBenchmarkUnpin), and would protect
// instance 0 only, which runs anyway.
func audioProtectedFloor(spec routing.RuntimeSpec) int {
	floor := min(spec.MinInstances, routing.MaxInstanceIndex+1)
	if spec.MaxInstances > 0 {
		floor = min(floor, spec.MaxInstances)
	}
	return floor
}

// audioRampOneProcessAgain holds tgt's spec as one process again after its ramp made it
// replicable (audioRampReplicableAgain), when a later target of the run may still stop the
// server's running models (benchmarkTarget.stopsFollow) and stops are still allowed.
// Replicable, the spec's protected instances would start by themselves at that target's
// stop clear, inside its cold pass, and the stop path cannot tell their start from the
// target's own. It is beginBenchmarkSingleProcess for the one spec: the spec goes back into
// the override lease before the write, so the run's end -- or a reconcile after a crash --
// makes it replicable again, and a spec the write found already not replicable, or gone,
// is owed nothing. When the spec could not be held -- the lease or the write failed -- it
// may still be replicable, so the run stops nothing more: each later target is measured as
// it finds the server, where a neighbour that runs leaves its load time unconfirmed.
func (s *Server) audioRampOneProcessAgain(ctx context.Context, tgt benchmarkTarget) {
	ov := tgt.overrides
	if !tgt.stopsFollow || !ov.stopsAllowed {
		return
	}
	held := ov.singleProcess
	ov.singleProcess = benchmarkUnion(held, []string{tgt.spec.ID})
	if err := s.writeBenchmarkLease(ctx, tgt.server.ID, ov); err != nil {
		ov.singleProcess, ov.stopsAllowed = held, false
		slog.Warn("audio benchmark: could not record the override lease to run a ramped spec as one process again; running agent models are not stopped for the rest of the run", "server_id", tgt.server.ID, "spec_id", tgt.spec.ID, "err", err)
		return
	}
	out, err := s.Portal.SetBenchmarkRuntimeSpecsReplicable(ctx, []string{tgt.spec.ID}, true, false)
	ov.singleProcess = benchmarkUnion(held, benchmarkUnion(out.Written, out.Failed))
	if err != nil {
		ov.stopsAllowed = false
		slog.Warn("audio benchmark: could not run a ramped spec as one process again; running agent models are not stopped for the rest of the run", "server_id", tgt.server.ID, "spec_id", tgt.spec.ID, "err", err)
	}
	if !slices.Contains(ov.singleProcess, tgt.spec.ID) {
		if lerr := s.writeBenchmarkLease(ctx, tgt.server.ID, ov); lerr != nil {
			slog.Warn("audio benchmark: could not update the override lease after running a ramped spec as one process again; it is rewritten when the run ends", "server_id", tgt.server.ID, "err", lerr)
		}
	}
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
// process whatever the spec says, and the ramp of such a replicable spec is refused
// (audioRampShapeKnown).
func (s *Server) audioRampByInstance(tgt benchmarkTarget) bool {
	return tgt.app.Type == routing.ProviderServerAgent && tgt.spec.Replicable && s.AgentFeatures.Has(tgt.server.ID, runtimeInstancesFeature)
}

// audioRampShapeKnown refuses a ramp of a replicable spec that would not be ramped by
// instance: its agent has not declared its features since the gateway started, or does not
// declare runtime_instances. The ramp's result is stored as the mapping's real-time
// concurrency, which for a replicable spec the scaler reads as a ceiling on the instance
// count, so a ramp that counted streams inside one process -- one, normally, for a server
// that serializes -- would cap the spec at one instance for good: nothing measures a stored
// result again by itself. A spec that is not replicable, and a direct application, are
// ramped inside their one process as before.
func (s *Server) audioRampShapeKnown(tgt benchmarkTarget, byInstance bool) error {
	if byInstance || tgt.app.Type != routing.ProviderServerAgent || !tgt.spec.Replicable {
		return nil
	}
	if !s.AgentFeatures.Known(tgt.server.ID) {
		return fmt.Errorf("audio benchmark: launch spec %s is replicable, so its concurrency ramp counts instances, and the server's agent has not reported whether it runs them since the gateway started; run the ramp again once it has", tgt.spec.ID)
	}
	return fmt.Errorf("audio benchmark: launch spec %s is replicable, so its concurrency ramp counts instances, and the server's agent does not run them (it does not declare %s); update the agent, or turn replicable off to measure one process", tgt.spec.ID, runtimeInstancesFeature)
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
