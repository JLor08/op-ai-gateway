// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// errBenchmarkNoRuntimeEnsure is the load core's error for a target that loads
// without generating on a provider that cannot start a model through the agent
// router's ensure route (provider.RuntimeEnsurer). The production Multiplexer
// always can.
var errBenchmarkNoRuntimeEnsure = errors.New("benchmark: provider cannot start a model without generating")

// errLoadLoopBound is the cause the load loop's own deadline ends an ensure
// attempt with, so that attempt's failure can be told apart from a deadline or
// cancellation of the run itself.
var errLoadLoopBound = errors.New("load loop: bound elapsed")

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
// IT LOADS BY GENERATING, except for an images-only agent child (ADR-046),
// and that is load-bearing rather than incidental: a text model has, by the
// time this returns, both loaded AND served a complete one-token generation. A
// backend that allocates its KV cache lazily on first use has therefore
// necessarily already done so -- which is why the VRAM run has no second
// "send one tiny generation" step. Two windows for one observation would
// double the exposure to a drifting neighbour and to the reservation being
// held open, in exchange for a number that cannot differ.
//
// An images-only agent child (tgt.loadWithoutGenerating) has no chat endpoint
// to generate on, so the core starts it through the agent router's ensure
// route instead (provider.RuntimeEnsurer): it returns once the agent reports
// the child running, and nothing has been generated. The VRAM run reports that
// as vramWarningFirstGenerationNotMeasured.
//
// THE alreadyResident RETURN IS A CONTAMINATION SIGNAL, not a convenience.
// The core short-circuits on a resident model: it returns without loading or
// generating. The load run ignores the value (it only wants the model up).
// The VRAM run gets true only after a confirmed drain, when the probe still
// lists the target after the run cleared its override and before it loaded
// anything. With the default probe, the agent router's /running, which lists
// only the agent's own running children, the target's own child is up again:
// a request reached the router, or a pinned target restarted at the clear. An
// API-set loaded_models_path replaces /running and answers whatever that path
// lists. The run then has no load of its own to measure, so it reports
// inconclusive and no delta.
//
// residencyProbed IS THE OTHER HALF OF THAT SIGNAL, and it exists because
// "not resident" and "could not tell" are not the same answer. The probe
// needs a loaded-models path (routing.EffectiveLoadedModelsProbe: a
// server_agent application always has the agent router's /running, any other
// application only an operator-entered loaded_models_path) and a
// mapping-level app model name, and it can fail outright -- and in each of
// those cases alreadyResident is false for a model that may well be
// resident. A caller that treats the signal as load-bearing has to know
// which of the two it got: the VRAM run reports the unavailability as a
// caveat, because otherwise "not resident" would stand in for an answer it
// never got.
func (s *Server) ensureResidentForRun(ctx context.Context, tgt benchmarkTarget) (alreadyResident, residencyProbed bool, err error) {
	target, req := benchmarkTargetReq(tgt)
	req.MaxTokens = 1 // minimal — we only want the model loaded
	attempt, err := s.loadAttemptFor(tgt, target, req)
	if err != nil {
		return false, false, err
	}

	resident, probed := s.modelResident(ctx, target, tgt)
	if resident {
		return true, probed, nil
	}
	if err := s.loadUntilServable(ctx, target, attempt); err != nil {
		return false, probed, err
	}
	s.reflectLoadedAfterLoad(ctx, target, tgt)
	return false, probed, nil
}

// loadAttempt is one attempt of the load loop (loadUntilServable): it returns
// nil once the model is servable, and fits itself into the loop's bound.
type loadAttempt func(ctx context.Context, bound loadBound) error

// loadBound is the load loop's one bound: when it runs out, its length, and
// the two stream budgets it was computed from, which a text attempt's
// first-data budget is cut from (loadAttemptBudget).
type loadBound struct {
	deadline time.Time
	length   time.Duration
	budget   time.Duration
	idle     time.Duration
}

// newLoadBound starts the load loop's bound for target: the larger of
// coldLoadResidentMaxWait and the stream's first-data budget
// (coldStartBudget), from now.
func (s *Server) newLoadBound(target routing.Target) loadBound {
	idle := s.benchmarkStreamIdle()
	budget := coldStartBudget(target, idle)
	length := max(coldLoadResidentMaxWait, budget)
	return loadBound{deadline: time.Now().Add(length), length: length, budget: budget, idle: idle}
}

// loadAttemptFor picks how the load loop starts tgt's model: by generating
// req, or, for an images-only agent child, through the agent router's ensure
// route. Each branch asserts only the provider capability it uses.
func (s *Server) loadAttemptFor(tgt benchmarkTarget, target routing.Target, req inference.Request) (loadAttempt, error) {
	if tgt.loadWithoutGenerating {
		ensurer, ok := s.Provider.(provider.RuntimeEnsurer)
		if !ok {
			return nil, errBenchmarkNoRuntimeEnsure
		}
		return s.ensureLoadAttempt(ensurer, target, tgt.spec), nil
	}
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return nil, errBenchmarkNoStreaming
	}
	return func(ctx context.Context, bound loadBound) error {
		_, _, err := s.streamOnceWithin(ctx, streamer, target, req, loadAttemptBudget(bound.budget, bound.idle, time.Until(bound.deadline)))
		return err
	}, nil
}

// ensureLoadAttempt is one call of the agent router's ensure route, under the
// load loop's deadline with errLoadLoopBound as its cause. The route holds the
// call until the child is running or has failed, so that deadline is the only
// bound an attempt has. A failure comes back as a loadEnsureError, worded from
// spec, and names the loop's bound when the loop's own deadline ended it.
//
// loopBound is set only when the PROVIDER also reports a timeout: a 503 or
// 502 that happens to land exactly at the deadline must still be reported
// with its own router code, not the loop-deadline sentence, so the last
// concrete failure is never replaced by a coincidence of timing.
func (s *Server) ensureLoadAttempt(ensurer provider.RuntimeEnsurer, target routing.Target, spec routing.RuntimeSpec) loadAttempt {
	return func(ctx context.Context, bound loadBound) error {
		attemptCtx, cancel := context.WithDeadlineCause(s.upstreamAuthCtx(ctx, target), bound.deadline, errLoadLoopBound)
		defer cancel()
		err := ensurer.EnsureRuntimeModel(attemptCtx, target)
		if err == nil {
			return nil
		}
		ensureErr := &loadEnsureError{err: err, spec: spec}
		if errors.Is(err, provider.ErrTimeout) && errors.Is(context.Cause(attemptCtx), errLoadLoopBound) {
			ensureErr.loopBound = bound.length
		}
		return ensureErr
	}
}

// loadUntilServable runs attempt until the model serves, for a model that is
// not (known to be) resident: the VRAM run has just cleared the target's stop,
// and a Load found the model not loaded or could not tell. A server that
// answers 503 WHILE it is still loading -- the behaviour of llama-swap and
// other single-slot swappers, which this benchmark provokes by design, and of
// an agent that has not yet received the VRAM run's cleared stop -- would
// otherwise fail the whole run on the first probe. So the load is retried
// until it becomes servable, the loop's bound runs out, or the run is
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
// ONE bound covers the whole loop (newLoadBound): the larger of
// coldLoadResidentMaxWait and the stream's first-data budget
// (coldStartBudget), so an application whose timeout_ms allows a longer cold
// start than the 503 wait gets the longer of the two. A text attempt carries
// no context deadline of its own; its first-data budget is cut to what remains
// of the bound instead (loadAttemptBudget), so only the watchdog's two timers
// can end it and its error text says which. An ensure attempt runs under the
// bound's deadline itself (ensureLoadAttempt). When no more than one retry gap
// of the bound remains, the loop returns the last 503 instead of sleeping or
// starting another attempt.
func (s *Server) loadUntilServable(ctx context.Context, target routing.Target, attempt loadAttempt) error {
	gap := coldLoadPollGap
	if gap <= 0 {
		gap = 100 * time.Millisecond // never busy-spin
	}
	bound := s.newLoadBound(target)
	for {
		err := attempt(ctx, bound)
		if !errors.Is(err, provider.ErrUpstreamStarting) {
			return err // nil once the model served, or a failure the loop does not wait out
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Until(bound.deadline) <= gap {
			return err
		}
		if !sleepCtx(ctx, gap) {
			return ctx.Err()
		}
		if time.Until(bound.deadline) <= gap {
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

// loadEnsureError is a failed ensure attempt, worded for the operator:
// "<router code>: <hint> (<provider text>)" for a router code with a hint, a
// sentence of its own for the loop's deadline and for an invalid 2xx, and the
// provider's own text for anything else. It unwraps to the provider error, so
// the load loop's 503 retry and every errors.Is check still see it.
type loadEnsureError struct {
	err error
	// spec is the target's spec, which the start-timeout hint quotes.
	spec routing.RuntimeSpec
	// loopBound is the load loop's bound when the loop's own deadline ended
	// the attempt, and 0 otherwise.
	loopBound time.Duration
}

func (e *loadEnsureError) Error() string {
	if e.loopBound > 0 {
		return "provider.timeout: not running within " + e.loopBound.String() + " (the larger of 5 min and the stream budget); a start already under way may still come up"
	}
	var routerErr *provider.RouterError
	if errors.As(e.err, &routerErr) {
		if hint := loadEnsureHint(routerErr.Code, e.spec); hint != "" {
			return routerErr.Code + ": " + hint + " (" + routerErr.Err.Error() + ")"
		}
	}
	if errors.Is(e.err, provider.ErrInvalidResponse) {
		return `provider.invalid_response: the agent's ensure route answered without "running"`
	}
	return e.err.Error()
}

func (e *loadEnsureError) Unwrap() error { return e.err }

// loadEnsureHint says what an agent router's error code means for a Load of
// spec's child, or "" for a code it has no hint for.
func loadEnsureHint(code string, spec routing.RuntimeSpec) string {
	switch code {
	case "runtime.start_timeout":
		return fmt.Sprintf("not healthy within startup_timeout_seconds (%d s)", spec.StartupTimeoutSeconds)
	case "runtime.start_failed":
		return "the process exited or failed to start; see the agent log"
	case "runtime.not_permitted":
		return "the agent refused the binary or its arguments"
	case "runtime.admission_blocked":
		return "the agent did not admit the start (a force-stopped spec, a pinned sibling or no free VRAM)"
	case "runtime.model_not_managed":
		return "the agent does not manage this model"
	case "runtime.upstream_gone":
		return "the agent stopped while starting the child"
	}
	return ""
}

// modelResident best-effort reports whether tgt's upstream model is already
// loaded on tgt's server, and whether the question was ANSWERED at all.
//
// It asks the loaded-models path routing.EffectiveLoadedModelsProbe resolves:
// a server_agent application always has one, the agent router's /running
// (unless an API-set loaded_models_path overrides it), and any other
// application only its operator-entered loaded_models_path. The Load, the VRAM
// run and the model warmer all ask through here.
//
// probed is false when there is nothing to ask (no LoadedModelLister, no
// loaded-models path, no mapping app model name) or when the ask failed.
// resident is then false as well, but it is false the way an unanswered
// question is false -- see ensureResidentForRun's residencyProbed.
func (s *Server) modelResident(ctx context.Context, target routing.Target, tgt benchmarkTarget) (resident, probed bool) {
	path, format := routing.EffectiveLoadedModelsProbe(tgt.app)
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok || strings.TrimSpace(path) == "" || strings.TrimSpace(tgt.mapping.AppModelName) == "" {
		return false, false
	}
	probeCtx := s.upstreamAuthCtx(ctx, target)
	loaded, err := modelLoaded(probeCtx, lister, target, path, format, tgt.mapping.AppModelName)
	if err != nil {
		return false, false
	}
	return loaded, true
}

// reflectLoadedAfterLoad best-effort re-probes the app's loaded set and writes it to the gateway-poll
// registry, so the model-servers SSE flips the row to loaded immediately instead of waiting for the
// next health-poll pass. No-op when the app has no loaded-models endpoint / no registry.
//
// It reads the application's own loaded_models_path, not routing.EffectiveLoadedModelsProbe, like
// the health loop's loaded pass: both write the loaded-model registry, and for a server_agent
// application the agent's report is the truth there. LoadedAppModels ignores an empty agent report,
// so a one-shot gateway-poll entry for an agent application would stay "loaded" for as long as the
// agent reports nothing loaded.
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
