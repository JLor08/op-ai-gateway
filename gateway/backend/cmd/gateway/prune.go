// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"log"
	"op-ai-gateway/internal/portal"
	"time"
)

// capturePruneInterval is how often the retention job scans for expired captures.
const capturePruneInterval = time.Hour

// capturePruner is the store surface the capture retention loop needs.
// *store.SQLiteStore satisfies it via SystemSettings + PruneCaptures.
type capturePruner interface {
	SystemSettings(ctx context.Context) (map[string]string, error)
	PruneCaptures(ctx context.Context, olderThan time.Time) (int64, error)
}

// runCapturePruneLoop deletes captures past their retention window on a fixed
// interval, re-reading capture_retention_days each tick so a setting change takes
// effect without a restart. It returns when ctx is cancelled. Production has no
// graceful shutdown (main dies via log.Fatal on ListenAndServe, so defer cleanup
// never runs); clean stop-on-cancel is a test/embedded guarantee, and a stateless
// prune loop being killed by process exit is acceptable.
func runCapturePruneLoop(ctx context.Context, pruner capturePruner, interval time.Duration, now func() time.Time) {
	runLoop(ctx, loopOpts{
		Interval: func() time.Duration { return interval }, // fixed cadence: only the retention window is re-read each pass, inside pruneCapturesOnce
		Pass:     func(ctx context.Context) { pruneCapturesOnce(ctx, pruner, now()) },
	})
}

// startCapturePruneLoop launches runCapturePruneLoop in a goroutine and returns the
// cancel func that stops it. sqliteDeps (Task 5.4) calls this with the sqlite store
// and capturePruneInterval, folding the returned cancel into cleanup. It is a package
// var so the Task 5.4 test can substitute a countable fake pruner + a short interval
// and observe that the goroutine actually starts and stops.
var startCapturePruneLoop = func(pruner capturePruner, interval time.Duration) context.CancelFunc {
	return startCancellable(func(ctx context.Context) {
		runCapturePruneLoop(ctx, pruner, interval, func() time.Time { return time.Now().UTC() })
	})
}

// pruneCapturesOnce reads the current retention window and deletes captures older
// than it. Errors are logged and swallowed so the loop keeps running.
func pruneCapturesOnce(ctx context.Context, pruner capturePruner, now time.Time) {
	values, err := pruner.SystemSettings(ctx)
	if err != nil {
		log.Printf("capture prune: read system settings failed: %v", err)
		return
	}
	days := portal.CaptureRetentionDays(values)
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	deleted, err := pruner.PruneCaptures(ctx, cutoff)
	if err != nil {
		log.Printf("capture prune: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("capture prune: removed %d captures older than %s", deleted, cutoff.Format(time.RFC3339))
	}
}

// telemetryPruneInterval is how often the telemetry retention job scans for
// expired rich telemetry samples.
const telemetryPruneInterval = time.Hour

// telemetryPruner is the store surface the telemetry retention loop needs.
// *store.SQLStore satisfies it via PruneTelemetrySamples + PruneBenchmarkRuns
// (both share the telemetry retention window) and PruneServerAvailabilitySamples
// (its own, longer availability retention window).
type telemetryPruner interface {
	PruneTelemetrySamples(ctx context.Context, before time.Time) error
	PruneBenchmarkRuns(ctx context.Context, before time.Time) error
	PruneServerAvailabilitySamples(ctx context.Context, before time.Time) error
}

// runTelemetryPruneLoop deletes telemetry samples past the retention window on a
// fixed interval. Unlike the capture loop, the retention window is a fixed config
// value (OP_AI_GATEWAY_TELEMETRY_RETENTION_HOURS), not a live system setting, so it
// is captured once at startup rather than re-read each tick. It returns when ctx is
// cancelled (see runCapturePruneLoop's note on shutdown semantics).
func runTelemetryPruneLoop(ctx context.Context, pruner telemetryPruner, retention, availabilityRetention, interval time.Duration, now func() time.Time) {
	runLoop(ctx, loopOpts{
		Interval: func() time.Duration { return interval }, // fixed cadence: retention windows are startup-captured, not re-read
		Pass:     func(ctx context.Context) { pruneTelemetryOnce(ctx, pruner, retention, availabilityRetention, now()) },
	})
}

// startTelemetryPruneLoop launches runTelemetryPruneLoop in a goroutine and returns
// the cancel func that stops it. sqliteDeps/postgresDeps call this with the SQL
// store, the configured retention, and telemetryPruneInterval, folding the returned
// cancel into cleanup. It is a package var so a test can substitute a countable fake
// pruner + a short interval.
var startTelemetryPruneLoop = func(pruner telemetryPruner, retention, availabilityRetention, interval time.Duration) context.CancelFunc {
	return startCancellable(func(ctx context.Context) {
		runTelemetryPruneLoop(ctx, pruner, retention, availabilityRetention, interval, func() time.Time { return time.Now().UTC() })
	})
}

// pruneTelemetryOnce deletes telemetry/benchmark samples older than the telemetry
// retention window and availability samples older than their own (typically longer)
// retention window. Each block is independently guarded: a non-positive retention
// disables just that block, so the two windows are decoupled. Errors are logged and
// swallowed so the loop keeps running.
func pruneTelemetryOnce(ctx context.Context, pruner telemetryPruner, retention, availabilityRetention time.Duration, now time.Time) {
	if retention > 0 {
		cutoff := now.Add(-retention)
		if err := pruner.PruneTelemetrySamples(ctx, cutoff); err != nil {
			log.Printf("telemetry prune: %v", err)
		}
		if err := pruner.PruneBenchmarkRuns(ctx, cutoff); err != nil {
			log.Printf("benchmark prune: %v", err)
		}
	}
	if availabilityRetention > 0 {
		if err := pruner.PruneServerAvailabilitySamples(ctx, now.Add(-availabilityRetention)); err != nil {
			log.Printf("availability prune: %v", err)
		}
	}
}
