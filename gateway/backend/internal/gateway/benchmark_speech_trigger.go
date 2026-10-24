// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
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
