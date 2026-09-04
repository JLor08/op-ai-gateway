// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// runLoadModel warm-loads tgt's model on tgt's specific server (forcing a load if not resident),
// then best-effort updates the loaded registry so the model-servers SSE reflects it immediately.
// Reuses the benchmark server reservation (mutually exclusive with benchmarks + live traffic). Does
// NOT persist and does NOT call Release: like runContextProbe, the terminal status must LINGER so
// the frontend's benchmarkStatus poll can read it. The deferred finish() frees the server on EVERY
// exit (including panic).
func (s *Server) runLoadModel(ctx context.Context, run *benchmarkRun, serverID string, tgt benchmarkTarget) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	defer func() {
		run.addResult(res)
		run.finish(res.Error)
		s.Benchmarks.publish(serverID, run.snapshot())
	}()

	if _, _, err := s.ensureResidentForRun(ctx, tgt); err != nil {
		res.Error = err.Error()
		return
	}
	res.Loaded = true
}

// ensureResidentForRun is the LOAD CORE, shared by the load run and the VRAM
// benchmark: make tgt's model resident on tgt's server, and report whether it
// was ALREADY resident before we touched it.
//
// IT LOADS BY GENERATING, and that is load-bearing rather than incidental:
// there is no non-generating load path anywhere in this code, so by the time
// this returns the model has both loaded AND served a complete one-token
// generation. A backend that allocates its KV cache lazily on first use has
// therefore necessarily already done so -- which is why the VRAM run has no
// second "send one tiny generation" step. Two windows for one observation
// would double the exposure to a drifting neighbour and to the reservation
// being held open, in exchange for a number that cannot differ.
//
// THE alreadyResident RETURN IS A CONTAMINATION SIGNAL, not a convenience.
// The core short-circuits on a resident model, so a caller that has just
// confirmed the model STOPPED and still gets true is being told that
// something it could not stop is serving that model. The load run ignores the
// value (it only wants the model up); the VRAM run reports inconclusive on
// it, because a delta measured against a baseline that already contains the
// model is a definitive ~0.
//
// residencyProbed IS THE OTHER HALF OF THAT SIGNAL, and it exists because
// "not resident" and "could not tell" are not the same answer. The probe
// needs an application-level loaded_models_path (operator-entered, no
// default) and a mapping-level app model name, and it can fail outright -- and
// in each of those cases alreadyResident is false for a model that may well
// be resident. A caller that treats the signal as load-bearing has to know
// which of the two it got: the VRAM run reports the unavailability as a
// caveat, because otherwise the contamination surfaces as a sub-floor delta
// whose stated next action can never work.
func (s *Server) ensureResidentForRun(ctx context.Context, tgt benchmarkTarget) (alreadyResident, residencyProbed bool, err error) {
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return false, false, errBenchmarkNoStreaming
	}
	target, req := benchmarkTargetReq(tgt)
	req.MaxTokens = 1 // minimal — we only want the model loaded

	resident, probed := s.modelResident(ctx, target, tgt)
	if resident {
		return true, probed, nil
	}
	// The run just cleared the target's stop, so this is a genuine cold load. A
	// server that answers 503 WHILE it is still loading -- the behaviour of
	// llama-swap and other single-slot swappers, which this benchmark provokes by
	// design -- would otherwise fail the whole run on the first probe. Retry the
	// load until it becomes servable, the cold-load budget elapses, or the run is
	// cancelled.
	//
	// The predicate is DELIBERATELY narrow: ONLY provider.ErrUpstreamStarting (a
	// 503) is retried. Every other failure returns at once -- a 4xx (bad model
	// name / auth), a refused connection or a mid-load crash (a VRAM benchmark
	// pushing the ceiling is exactly where an OOM crash happens; re-driving the
	// load would loop the crash), and streamOnce's own idle-watchdog cancellation
	// of a genuinely hung stream. Only a live server explicitly reporting "still
	// loading" waits.
	gap := coldLoadPollGap
	if gap <= 0 {
		gap = 100 * time.Millisecond // never busy-spin
	}
	deadline := time.Now().Add(coldLoadResidentMaxWait)
	for {
		if _, _, streamErr := s.streamOnce(ctx, streamer, target, req); streamErr != nil {
			if !errors.Is(streamErr, provider.ErrUpstreamStarting) {
				return false, probed, streamErr
			}
			if ctx.Err() != nil {
				return false, probed, ctx.Err()
			}
			if !time.Now().Before(deadline) {
				return false, probed, streamErr
			}
			select {
			case <-ctx.Done():
				return false, probed, ctx.Err()
			case <-time.After(gap):
			}
			continue
		}
		break
	}
	s.reflectLoadedAfterLoad(ctx, target, tgt)
	return false, probed, nil
}

// modelResident best-effort reports whether tgt's upstream model is already
// loaded on tgt's server, and whether the question was ANSWERED at all.
//
// probed is false when there is nothing to ask (no LoadedModelLister, no
// application loaded_models_path, no mapping app model name) or when the ask
// failed. resident is then false as well, but it is false the way an
// unanswered question is false -- see ensureResidentForRun's residencyProbed.
func (s *Server) modelResident(ctx context.Context, target routing.Target, tgt benchmarkTarget) (resident, probed bool) {
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok || strings.TrimSpace(tgt.app.LoadedModelsPath) == "" || strings.TrimSpace(tgt.mapping.AppModelName) == "" {
		return false, false
	}
	probeCtx := s.upstreamAuthCtx(ctx, target)
	loaded, err := modelLoaded(probeCtx, lister, target, tgt.app, tgt.mapping.AppModelName)
	if err != nil {
		return false, false
	}
	return loaded, true
}

// reflectLoadedAfterLoad best-effort re-probes the app's loaded set and writes it to the gateway-poll
// registry, so the model-servers SSE flips the row to loaded immediately instead of waiting for the
// next health-poll pass. No-op when the app has no loaded-models endpoint / no registry.
func (s *Server) reflectLoadedAfterLoad(ctx context.Context, target routing.Target, tgt benchmarkTarget) {
	if s.LoadedModels == nil {
		return
	}
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok || strings.TrimSpace(tgt.app.LoadedModelsPath) == "" {
		return
	}
	probeCtx := s.upstreamAuthCtx(ctx, target)
	names, err := lister.LoadedModels(probeCtx, target, tgt.app.LoadedModelsPath, tgt.app.LoadedModelsFormat)
	if err != nil {
		return
	}
	s.LoadedModels.SetGatewayProbe(tgt.app.ID, names) // publishes to the model-servers SSE
}
