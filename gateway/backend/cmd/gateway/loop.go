// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"time"
)

// loopOpts configures runLoop, the shared select-loop scaffold behind this
// package's tick-driven background loops (cert reconcile, netbird peer-sync,
// netbird group/policy reconcile, capture prune, telemetry prune, gateway-peer
// reconcile). Every one of those loops is, underneath its own naming and
// dependencies, the same shape: an optional immediate pass, then react to
// ctx.Done(), an optional trigger channel, and a ticker whose period may be
// re-read from a live source each tick. runAppHealthLoop is the one
// exception — its trigger channel carries a payload (chan string) and
// dispatches a DIFFERENT, narrower pass than its tick pass, which does not
// fit this single-Pass shape, so it stays bespoke.
type loopOpts struct {
	// Immediate, when true, runs Pass once — right after the ctx pre-check,
	// before the ticker is even created — so a loop's state is fresh within
	// one pass of startup rather than only after the first tick. Loops with
	// no meaningful "startup" state (the prune loops, the gateway-peer
	// reconcile loop) leave this false and simply wait for the first tick.
	Immediate bool
	// Interval supplies the ticker's period. It is called once to seed the
	// ticker (before Immediate's pass, if any, has any bearing on it — the
	// two are independent) and again after every TICK-driven pass (never
	// after a Trigger-driven one, matching every hand-rolled loop this
	// replaces) to decide whether to Ticker.Reset. A loop with a fixed
	// cadence returns the same value every time, which makes that check a
	// no-op. Interval must return a positive duration — time.NewTicker
	// panics otherwise — so a caller floors any non-positive configured or
	// live-read value itself before handing Interval to runLoop.
	Interval func() time.Duration
	// Trigger, when non-nil, runs an EXTRA Pass immediately on every
	// receive, independent of the ticker — e.g. reacting to a settings save
	// without waiting out the rest of the current interval. A nil Trigger is
	// safe: a receive on a nil channel never fires, so that select arm is
	// simply never chosen.
	Trigger <-chan struct{}
	// Pass is the one unit of work the loop drives: on the immediate call
	// (if Immediate), on every tick, and on every Trigger receive. Passes
	// never overlap — they all run synchronously on runLoop's own goroutine.
	Pass func(ctx context.Context)
}

// runLoop is the shared scaffold behind this package's tick-driven
// background loops. It first checks ctx (so a loop started against an
// already-cancelled context does nothing at all, not even Immediate's
// pass), optionally runs one immediate pass, then loops: select on
// ctx.Done() (return), opts.Trigger (an extra pass), or the ticker (a pass,
// then an opts.Interval() re-read that Resets the ticker only when the
// period actually changed). It returns once ctx is cancelled.
func runLoop(ctx context.Context, opts loopOpts) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	if opts.Immediate {
		opts.Pass(ctx)
	}
	interval := opts.Interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-opts.Trigger:
			opts.Pass(ctx)
		case <-ticker.C:
			opts.Pass(ctx)
			if next := opts.Interval(); next != interval {
				interval = next
				ticker.Reset(interval)
			}
		}
	}
}

// startLoop runs runLoop in a background goroutine against a fresh
// context.CancelFunc-controlled context and returns that cancel func. Every
// start*Loop package var in this package that fits runLoop's shape is a thin
// wrapper wired through its own run*Loop function (kept as package vars so a
// test can substitute a fake driving loop) rather than calling startLoop
// directly, so run*Loop stays independently callable/testable exactly as
// before.
func startLoop(opts loopOpts) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go runLoop(ctx, opts)
	return cancel
}
