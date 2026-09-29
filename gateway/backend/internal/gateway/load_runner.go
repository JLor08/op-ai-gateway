// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
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
	if err := s.loadUntilServable(ctx, streamer, target, req); err != nil {
		return false, probed, err
	}
	s.reflectLoadedAfterLoad(ctx, target, tgt)
	return false, probed, nil
}

// loadUntilServable streams req until the model serves it, for a model that is
// not (known to be) resident: the VRAM run has just cleared the target's stop,
// and a Load found the model not loaded or could not tell. A server that
// answers 503 WHILE it is still loading -- the behaviour of llama-swap and
// other single-slot swappers, which this benchmark provokes by design --
// would otherwise fail the whole run on the first probe. So the load is
// retried until it becomes servable, the loop's bound runs out, or the run is
// cancelled.
//
// The predicate is DELIBERATELY narrow: ONLY provider.ErrUpstreamStarting (a
// 503) is retried. Every other failure returns at once -- a 4xx (bad model
// name / auth), a refused connection or a mid-load crash (a VRAM benchmark
// pushing the ceiling is exactly where an OOM crash happens; re-driving the
// load would loop the crash), and the stream watchdog's provider.ErrTimeout
// for an attempt that stalled or produced no data within its budget. Only a
// live server explicitly reporting "still loading" waits.
//
// ONE bound covers the whole loop: the larger of coldLoadResidentMaxWait and
// the stream's first-data budget (coldStartBudget), so an application whose
// timeout_ms allows a longer cold start than the 503 wait gets the longer of
// the two. An attempt carries no context deadline of its own; its first-data
// budget is cut to what remains of the bound instead (loadAttemptBudget), so
// only the watchdog's two timers can end it and its error text says which.
// When no more than one retry gap of the bound remains, the loop returns the
// last 503 instead of sleeping or starting another attempt.
func (s *Server) loadUntilServable(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) error {
	gap := coldLoadPollGap
	if gap <= 0 {
		gap = 100 * time.Millisecond // never busy-spin
	}
	idle := s.benchmarkStreamIdle()
	budget := coldStartBudget(target, idle)
	deadline := time.Now().Add(max(coldLoadResidentMaxWait, budget))
	for {
		_, _, err := s.streamOnceWithin(ctx, streamer, target, req, loadAttemptBudget(budget, idle, time.Until(deadline)))
		if !errors.Is(err, provider.ErrUpstreamStarting) {
			return err // nil once the model served, or a failure the loop does not wait out
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Until(deadline) <= gap {
			return err
		}
		if !sleepCtx(ctx, gap) {
			return ctx.Err()
		}
		if time.Until(deadline) <= gap {
			return err
		}
	}
}

// loadAttemptBudget is one load attempt's first-data budget: the stream's
// budget, cut to what remains of the load loop's bound, and never below idle.
// A budget at or below idle arms no first-data timer, so an attempt started
// near the bound can overrun it by at most one idle budget.
func loadAttemptBudget(budget, idle, remaining time.Duration) time.Duration {
	return max(idle, min(budget, remaining))
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
