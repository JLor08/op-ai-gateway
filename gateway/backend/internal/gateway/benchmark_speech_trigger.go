// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// benchmarkScopeSpeechRate is the Scope of a run the app-health loop started for a
// mapping's output byte rate (TriggerSpeechRateBenchmark), beside the "scheduled" of the
// scheduler's runs and the "mapping" / "application" / "server" of an operator's.
const benchmarkScopeSpeechRate = "speech-rate"

// SpeechRateTrigger is the signature of Server.TriggerSpeechRateBenchmark, the one thing
// the app-health loop needs of the gateway Server: the loop is built before the Server
// exists, so it holds a trigger bound late (ServerDeps.SetSpeechRateTrigger) and calls it
// through this type.
type SpeechRateTrigger func(ctx context.Context, server routing.AIServer, app routing.Application, mapping routing.ModelMapping, identity string) bool

// TriggerSpeechRateBenchmark starts the speech benchmark for ONE mapping on behalf of the
// app-health loop (#263), so a streaming speech mapping has its output byte rate cached
// before any client streams and the streamed call bills in audio seconds. It is the
// health-loop twin of POST .../benchmark?mode=speech: the same targets, the same
// measurement and the same writes (runSpeechTarget), with no principal -- the run needs
// none (see the file comment of benchmark_speech.go) -- and no HTTP response.
//
// It never measures inline. It decides, reserves and launches; the synthesis runs on a
// background context in its own goroutine, so a health tick is held up by two store reads
// and a registry call, never by a model. It answers true only when a run was launched, and
// false for everything else, each of which is the health loop's cue to ask again on a later
// tick and none of which writes anything:
//
//   - the mapping is not a target. The decision is the manual benchmark's own
//     (partitionSpeechBenchmarkViews): a text-to-speech mapping the gateway routes speech to,
//     from its stored capability rows -- a plain one, or a clone model with a cached preset
//     voice, which is then synthesized with that preset (#306). A reference-only clone model
//     (no preset), a transcriber and a mapping with no speech verdict are refused, so the
//     trigger is never wider than the manual run;
//   - another run holds the server's one slot (BenchmarkRegistry.TryStart), a manual run, a
//     scheduled one or an earlier trigger still in flight;
//   - the server has in-flight requests. Like the scheduled benchmark it never competes with
//     live traffic: it takes the reservation (which keeps new requests off the server), sees
//     the traffic, and gives the reservation back.
//
// identity is the loaded-model identity the caller observed (the model file a static server's
// listing states, a fingerprint of the launch spec for an agent-launched child), or "" when it
// observed none. It rides on the target, and runSpeechTarget records it beside the rate once
// the rate is written; a failed run records neither, so a rate already cached and the identity
// it was measured under stay a matching pair.
//
// Whether a rate is already cached, and for which model, is the caller's decision: this method
// runs the benchmark it is asked to run.
func (s *Server) TriggerSpeechRateBenchmark(ctx context.Context, server routing.AIServer, app routing.Application, mapping routing.ModelMapping, identity string) bool {
	if s == nil || s.Routes == nil {
		return false
	}
	part := s.partitionSpeechBenchmarkViews(ctx, []portal.BenchmarkTargetView{{Server: server, App: app, Mapping: mapping}})
	if len(part.runnable) == 0 {
		slog.Debug("speech rate benchmark not started: the mapping is not a speech benchmark target", "mapping_id", mapping.ID, "recorded", part.recorded)
		return false
	}
	tgt := part.runnable[0]
	tgt.rateIdentity = identity
	// The run outlives this call, so it runs on a background context that TryStart's
	// cancel tears down when the run finishes or is released -- the manual starter's and
	// the scheduler's arrangement.
	runCtx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, benchmarkScopeSpeechRate, benchmarkModeSpeech, 1, time.Now().UTC(), cancel)
	if !ok {
		cancel()
		return false
	}
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		slog.Debug("speech rate benchmark deferred: the server has in-flight requests", "mapping_id", mapping.ID, "server_id", server.ID)
		return false
	}
	slog.Info("speech rate benchmark started by the health loop", "mapping_id", mapping.ID, "server_id", server.ID)
	go func() {
		defer cancel()
		s.runBenchmark(runCtx, run, server.ID, []benchmarkTarget{tgt}, benchmarkModeSpeech)
	}()
	return true
}

// benchmarkScopeAudioAuto is the Scope of the one-time automatic audio benchmark the
// app-health loop starts for a mapping (TriggerAudioBenchmark).
const benchmarkScopeAudioAuto = "audio-auto"

// AudioBenchmarkTrigger is the signature of Server.TriggerAudioBenchmark, which the app-health
// loop holds bound late, like SpeechRateTrigger (ServerDeps.SetAudioBenchmarkTrigger).
type AudioBenchmarkTrigger func(ctx context.Context, server routing.AIServer, app routing.Application, mapping routing.ModelMapping) AudioAutoRunAnswer

// AudioAutoRunAnswer is TriggerAudioBenchmark's answer about one mapping. Beside whether a
// run was started, it tells the app-health loop whether asking about the mapping again could
// start one, so the loop does not ask -- and read the mapping's spec and capability rows --
// on every tick about a mapping the gateway will always refuse.
type AudioAutoRunAnswer int

const (
	// AudioAutoRunDeferred: nothing was started, for a reason that can pass by itself or that
	// the loop's own rule watches: another run holds the server, it has requests in flight, the
	// agent has not declared its features since the gateway started, a read failed, the
	// mapping has no speech or transcription verdict yet, or it is inactive, locked or no
	// longer lacks what the run measures (AudioAutoRunWanted). The loop asks again later.
	AudioAutoRunDeferred AudioAutoRunAnswer = iota
	// AudioAutoRunStarted: the mapping's one run was launched.
	AudioAutoRunStarted
	// AudioAutoRunSettled: nothing was started, and asking again would be refused again unless
	// the mapping itself changes: its one run was started already in this process for its
	// spec's current shape (it is not started twice, however it ended, until replicable
	// changes), or it is refused for what it is -- a chat model or one
	// that serves only images is no audio target, and a clone model with no preset voice
	// cannot synthesize without a reference. Its flavors and model change only by an
	// operator's edit; a clone model's presets reach the voices cache through the loop's own
	// write, which the loop watches. The gateway itself remembers only the started runs.
	AudioAutoRunSettled
)

// String names the answer, for logs and test failures.
func (a AudioAutoRunAnswer) String() string {
	switch a {
	case AudioAutoRunDeferred:
		return "deferred"
	case AudioAutoRunStarted:
		return "started"
	case AudioAutoRunSettled:
		return "settled"
	}
	return "unknown"
}

// audioAutoRunMarker remembers which mappings the one-time automatic audio benchmark was
// started for in this process, each with whether its launch spec was replicable then. A run
// is once per mapping AND shape: an operator who turns replicable on or off has the portal
// clear the ramp result (it counted the other shape), and the run then measures the spec
// again. Its zero value is ready, and it is safe for concurrent use: the health loop asks
// from one goroutine per application.
type audioAutoRunMarker struct {
	mu      sync.Mutex
	started map[string]bool // mapping id -> its spec's replicable when its run started
}

// done reports whether mappingID's run was started already for a spec of this replicable.
func (m *audioAutoRunMarker) done(mappingID string, replicable bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	was, ok := m.started[mappingID]
	return ok && was == replicable
}

// claim marks mappingID's run started for a spec of this replicable and reports whether this
// call did, false when another one had already.
func (m *audioAutoRunMarker) claim(mappingID string, replicable bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if was, ok := m.started[mappingID]; ok && was == replicable {
		return false
	}
	if m.started == nil {
		m.started = map[string]bool{}
	}
	m.started[mappingID] = replicable
	return true
}

// AudioAutoRunWanted is THE rule whether an audio mapping still lacks what the automatic run
// measures: any audio metrics at all (no audio benchmark has stamped it), or, when its launch
// spec is replicable, a ramp result (its concurrency limit is unknown). A ramp's measured 0
// has a limit, so it never asks again: it is a result. A mapping without a spec -- an
// operator-run audio.cpp server's -- is not replicable. TriggerAudioBenchmark decides with it,
// and the app-health loop asks the trigger only about a mapping it holds for, so a mapping
// the run would not measure again is not asked about on every tick.
func AudioAutoRunWanted(mapping routing.ModelMapping, replicable bool) bool {
	return mapping.AudioMetricsUpdatedAt == nil || (replicable && mapping.AudioConcurrencyLimit == "")
}

// TriggerAudioBenchmark starts the one-time automatic audio benchmark for ONE mapping on behalf
// of the app-health loop (#340), so a speech or transcription mapping has its audio metrics --
// and a replicable one its real-time concurrency -- normally right after the operator set it
// up, before real use. It is a both run of exactly the manual run's target
// (benchmarkViewTarget), with no principal and no stop-all: the cold start is measured only
// when the server is found quiet (agentColdStart).
//
// Like TriggerSpeechRateBenchmark it decides, reserves and launches, and the run goes on in its
// own goroutine. It answers AudioAutoRunStarted only when a run was launched. It answers
// AudioAutoRunSettled, and the loop does not ask about the mapping again, when:
//   - its run was started already in this process: once per mapping and replicable value of
//     its spec, so a run that fails -- an upstream that cannot synthesize, say -- does not
//     start again on every tick, while a spec the operator made replicable (or not) since,
//     whose ramp result the portal cleared, is measured again;
//   - it is no audio target for what it is: a chat model (it keeps its text benchmark, even
//     with an audio verdict), one that serves only images, or a clone model that lists no
//     preset voice (benchmarkSkippedSpeechClone).
//
// It answers AudioAutoRunDeferred, and the loop asks again on a later tick, for everything
// else, none of which uses up the mapping's one run:
//   - the mapping is not active, its metrics are locked (nothing would be written), or it
//     already has what the run measures (AudioAutoRunWanted) -- what the loop checks itself;
//   - a read failed, or it has no speech or transcription verdict yet (it arrives by itself);
//   - the mapping's agent has not declared its features since the gateway started: whether a
//     replicable spec is ramped one instance per stream depends on them (runtime_instances),
//     and a run against an agent that has not reported would fail;
//   - another run holds the server's slot, or the server has requests in flight.
func (s *Server) TriggerAudioBenchmark(ctx context.Context, server routing.AIServer, app routing.Application, mapping routing.ModelMapping) AudioAutoRunAnswer {
	if s == nil || s.Routes == nil || mapping.Status != routing.ServerStatusActive || mapping.MetricsLocked {
		return AudioAutoRunDeferred
	}
	if app.Type == routing.ProviderServerAgent && !s.AgentFeatures.Known(server.ID) {
		return AudioAutoRunDeferred
	}
	tgt, skipped, err := s.benchmarkViewTarget(ctx, portal.BenchmarkTargetView{Server: server, App: app, Mapping: mapping}, true)
	switch {
	case err != nil:
		return AudioAutoRunDeferred
	case s.audioAutoRuns.done(mapping.ID, tgt.spec.Replicable):
		return AudioAutoRunSettled
	case skipped == benchmarkSkippedImagesOnly, skipped == benchmarkSkippedSpeechClone, skipped == "" && tgt.audio.role == "":
		slog.Debug("audio benchmark not started by the health loop: the mapping is no audio target it can measure", "mapping_id", mapping.ID, "skipped", skipped)
		return AudioAutoRunSettled
	case skipped != "", !AudioAutoRunWanted(mapping, tgt.spec.Replicable):
		return AudioAutoRunDeferred
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, benchmarkScopeAudioAuto, "both", 1, time.Now().UTC(), cancel)
	if !ok {
		cancel()
		return AudioAutoRunDeferred
	}
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		return AudioAutoRunDeferred
	}
	if !s.audioAutoRuns.claim(mapping.ID, tgt.spec.Replicable) {
		s.Benchmarks.Release(server.ID)
		cancel()
		return AudioAutoRunSettled
	}
	slog.Info("audio benchmark started by the health loop", "mapping_id", mapping.ID, "server_id", server.ID)
	go func() {
		defer cancel()
		s.runBenchmark(runCtx, run, server.ID, []benchmarkTarget{tgt}, "both")
	}()
	return AudioAutoRunStarted
}
