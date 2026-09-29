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
	"sync/atomic"
	"time"
)

// errBenchmarkStreamIdle and errBenchmarkStreamFirstData are the two causes
// watchBenchmarkStream cancels a stream with. Neither leaves the helper: each
// is mapped onto a provider.ErrTimeout error with its own text, and every
// other cause (a parent context's cancellation or deadline) passes through as
// the provider reported it.
var (
	errBenchmarkStreamIdle      = errors.New("benchmark stream: idle budget elapsed")
	errBenchmarkStreamFirstData = errors.New("benchmark stream: first-data budget elapsed")
)

// benchmarkStreamIdle is the idle budget of every benchmark stream: the
// configured stream idle timeout (OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT), or
// benchmarkDefaultStreamIdle when that is disabled (<= 0).
func (s *Server) benchmarkStreamIdle() time.Duration {
	if s.streamIdleTimeout > 0 {
		return s.streamIdleTimeout
	}
	return benchmarkDefaultStreamIdle
}

// coldStartBudget is how long a benchmark stream may wait for its first data
// event: the larger of the application's timeout_ms (target.Timeout) and the
// idle budget. For a stock application (30 s against 120 s) it equals idle,
// so watchBenchmarkStream arms no first-data timer and SSE comments count for
// nothing. A server_agent or stable_diffusion_cpp application (600 s by
// default) gets that long to start a cold child, as long as the upstream
// keeps the connection alive.
func coldStartBudget(target routing.Target, idle time.Duration) time.Duration {
	return max(target.Timeout, idle)
}

// watchBenchmarkStream runs one benchmark CompleteStream under the benchmark
// stream watchdog and forwards every event to emit. A benchmark stream has no
// client to end it (it runs on context.Background), so the watchdog is always
// on. It has two timers:
//
//   - the idle timer, reset by every event, ends a stream that produced no
//     event for the idle budget (benchmarkStreamIdle);
//   - the first-data timer, armed only when budget exceeds idle, ends a stream
//     whose first event has not arrived within budget. It stops on the first
//     event, so a stream that produces data has no total cap (ADR-010).
//
// While the first-data timer is armed and no event has arrived, an SSE
// comment line (the agent router's `: keepalive`, reported through
// provider.WithStreamActivity) resets the idle timer too. After the first
// event it does not: the router sends heartbeats only until its first body
// byte, and a stream that turned into comments only must still end at idle.
// Crediting a keepalive needs an idle budget above the router's 10 s
// heartbeat interval.
//
// Only the watchdog's own two causes are mapped onto provider.ErrTimeout
// (streamWatchdog.explain); a parent context's cancellation or deadline -- a
// capacity level's deadline, the model warmer's 60 s context -- comes back
// exactly as CompleteStream returned it.
func (s *Server) watchBenchmarkStream(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request, budget time.Duration, emit provider.StreamEmit) error {
	ctx = s.upstreamAuthCtx(ctx, target)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	w := armStreamWatchdog(s.benchmarkStreamIdle(), budget, cancel)
	if w.firstData != nil {
		ctx = provider.WithStreamActivity(ctx, w.keepalive)
	}
	err := streamer.CompleteStream(ctx, target, req, func(ev inference.StreamEvent) error {
		w.progress()
		return emit(ev)
	})
	w.stop()
	if err == nil {
		return nil
	}
	return w.explain(context.Cause(ctx), err)
}

// streamWatchdog is one benchmark stream's pair of timers (watchBenchmarkStream).
// emitted and credited are atomic so that a StreamingClient calling emit or the
// activity hook from a goroutine of its own stays race-free.
type streamWatchdog struct {
	idle      time.Duration
	budget    time.Duration
	idleTimer *time.Timer
	firstData *time.Timer // nil unless budget > idle
	emitted   atomic.Bool // an event has reached emit
	credited  atomic.Bool // a comment line reset the idle timer before the first event
}

// armStreamWatchdog starts the idle timer and, only when budget exceeds idle,
// the first-data timer. When budget is at or below idle the idle timer alone
// bounds the silent phase, so the two timers never race.
func armStreamWatchdog(idle, budget time.Duration, cancel context.CancelCauseFunc) *streamWatchdog {
	w := &streamWatchdog{idle: idle, budget: budget}
	w.idleTimer = time.AfterFunc(idle, func() { cancel(errBenchmarkStreamIdle) })
	if budget > idle {
		w.firstData = time.AfterFunc(budget, func() { cancel(errBenchmarkStreamFirstData) })
	}
	return w
}

// keepalive credits one SSE comment line: before the first event it resets the
// idle timer and records the credit, afterwards it does nothing.
// watchBenchmarkStream installs it only when the first-data timer is armed.
func (w *streamWatchdog) keepalive() {
	if w.emitted.Load() {
		return
	}
	w.credited.Store(true)
	w.idleTimer.Reset(w.idle)
}

// progress records one event: the first one stops the first-data timer, and
// every one resets the idle timer.
func (w *streamWatchdog) progress() {
	if !w.emitted.Swap(true) && w.firstData != nil {
		w.firstData.Stop()
	}
	w.idleTimer.Reset(w.idle)
}

// stop disarms both timers once CompleteStream has returned.
func (w *streamWatchdog) stop() {
	w.idleTimer.Stop()
	if w.firstData != nil {
		w.firstData.Stop()
	}
}

// explain maps the watchdog's own cancellation cause onto a %w-wrapped
// provider.ErrTimeout error that names the budget that ran out, and returns
// err unchanged for any other cause. The first-data text needs a credited
// keepalive: a first-data cause without one reads as the idle text, because
// nothing but silence was observed. The budget is printed to the millisecond,
// since a load attempt's budget is a remaining time (loadAttemptBudget).
func (w *streamWatchdog) explain(cause, err error) error {
	firstData := errors.Is(cause, errBenchmarkStreamFirstData)
	if firstData && w.credited.Load() {
		return fmt.Errorf("%w: benchmark stream: no first data within %s although the upstream kept the connection alive (max of the application's timeout_ms and the idle budget)", provider.ErrTimeout, w.budget.Round(time.Millisecond))
	}
	if firstData || errors.Is(cause, errBenchmarkStreamIdle) {
		return fmt.Errorf("%w: benchmark stream: no data for %s (OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT)", provider.ErrTimeout, w.idle)
	}
	return err
}
