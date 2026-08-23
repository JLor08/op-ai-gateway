// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/config"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeCapturePruner is an in-memory capturePruner: PruneCaptures drops rows whose
// created_at is before olderThan, mirroring the real store's age filter.
type fakeCapturePruner struct {
	mu       sync.Mutex
	settings map[string]string
	rows     []time.Time
	calls    int
}

func (f *fakeCapturePruner) SystemSettings(_ context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.settings))
	for k, v := range f.settings {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCapturePruner) PruneCaptures(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	kept := f.rows[:0]
	var deleted int64
	for _, ts := range f.rows {
		if ts.Before(olderThan) {
			deleted++
			continue
		}
		kept = append(kept, ts)
	}
	f.rows = kept
	return deleted, nil
}

// Calls returns how many times PruneCaptures has run (thread-safe); used by the
// Task 5.4 goroutine-lifecycle test to observe that ticks actually fire.
func (f *fakeCapturePruner) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestPruneCapturesOnceDeletesOnlyOlder(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)   // 40 days old, beyond the 30-day default
	recent := now.Add(-5 * 24 * time.Hour) // 5 days old, within the window
	p := &fakeCapturePruner{settings: map[string]string{}, rows: []time.Time{old, recent}}

	pruneCapturesOnce(context.Background(), p, now)

	if len(p.rows) != 1 {
		t.Fatalf("rows after prune = %d, want 1", len(p.rows))
	}
	if !p.rows[0].Equal(recent) {
		t.Fatalf("surviving row = %v, want recent %v", p.rows[0], recent)
	}
}

func TestPruneCapturesOnceHonorsConfiguredRetention(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	tenDays := now.Add(-10 * 24 * time.Hour) // survives 30-day default, pruned at 7 days
	p := &fakeCapturePruner{settings: map[string]string{"capture_retention_days": "7"}, rows: []time.Time{tenDays}}

	pruneCapturesOnce(context.Background(), p, now)

	if len(p.rows) != 0 {
		t.Fatalf("rows after prune = %d, want 0 (10d old, 7d retention)", len(p.rows))
	}
}

func TestRunCapturePruneLoopStopsOnCancel(t *testing.T) {
	p := &fakeCapturePruner{settings: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runCapturePruneLoop(ctx, p, time.Hour, func() time.Time { return time.Now().UTC() })
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCapturePruneLoop did not return after context cancel")
	}
}

// fakeTelemetryPruner is an in-memory telemetryPruner: it records the cutoff of
// the most recent prune call for each table and counts invocations, thread-safe.
type fakeTelemetryPruner struct {
	mu                sync.Mutex
	before            time.Time
	calls             int
	benchmarkCalls    int
	benchmarkBefore   time.Time
	availabilityCalls int
	availabilityBefr  time.Time
}

func (f *fakeTelemetryPruner) PruneTelemetrySamples(_ context.Context, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.before = before
	return nil
}

func (f *fakeTelemetryPruner) PruneBenchmarkRuns(_ context.Context, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.benchmarkCalls++
	f.benchmarkBefore = before
	return nil
}

func (f *fakeTelemetryPruner) PruneServerAvailabilitySamples(_ context.Context, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.availabilityCalls++
	f.availabilityBefr = before
	return nil
}

func (f *fakeTelemetryPruner) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTelemetryPruner) BenchmarkCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.benchmarkCalls
}

func (f *fakeTelemetryPruner) AvailabilityCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availabilityCalls
}

func (f *fakeTelemetryPruner) Before() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.before
}

func (f *fakeTelemetryPruner) BenchmarkBefore() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.benchmarkBefore
}

func (f *fakeTelemetryPruner) AvailabilityBefore() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availabilityBefr
}

func TestPruneTelemetryOnce(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	// The telemetry retention and the availability retention are INDEPENDENT
	// cutoffs: telemetry/benchmark prune at now-retention, availability at
	// now-availabilityRetention. Pick distinct windows so a swap would be caught.
	fake := &fakeTelemetryPruner{}
	pruneTelemetryOnce(context.Background(), fake, 1*time.Hour, 24*time.Hour, now)
	if fake.Calls() != 1 {
		t.Fatalf("PruneTelemetrySamples calls = %d, want 1", fake.Calls())
	}
	if want := now.Add(-1 * time.Hour); !fake.Before().Equal(want) {
		t.Fatalf("telemetry prune cutoff = %v, want %v (now-1h)", fake.Before(), want)
	}
	// The benchmark-run history table is pruned on the SAME telemetry cutoff.
	if fake.BenchmarkCalls() != 1 {
		t.Fatalf("PruneBenchmarkRuns calls = %d, want 1", fake.BenchmarkCalls())
	}
	if want := now.Add(-1 * time.Hour); !fake.BenchmarkBefore().Equal(want) {
		t.Fatalf("benchmark prune cutoff = %v, want %v (now-1h)", fake.BenchmarkBefore(), want)
	}
	// Availability prunes on its OWN, longer cutoff.
	if fake.AvailabilityCalls() != 1 {
		t.Fatalf("PruneServerAvailabilitySamples calls = %d, want 1", fake.AvailabilityCalls())
	}
	if want := now.Add(-24 * time.Hour); !fake.AvailabilityBefore().Equal(want) {
		t.Fatalf("availability prune cutoff = %v, want %v (now-24h)", fake.AvailabilityBefore(), want)
	}

	// A non-positive telemetry retention disables ONLY the telemetry+benchmark
	// blocks; a positive availability retention still prunes availability.
	telemOff := &fakeTelemetryPruner{}
	pruneTelemetryOnce(context.Background(), telemOff, 0, 24*time.Hour, now)
	if telemOff.Calls() != 0 {
		t.Fatalf("PruneTelemetrySamples calls with retention=0 = %d, want 0", telemOff.Calls())
	}
	if telemOff.BenchmarkCalls() != 0 {
		t.Fatalf("PruneBenchmarkRuns calls with retention=0 = %d, want 0", telemOff.BenchmarkCalls())
	}
	if telemOff.AvailabilityCalls() != 1 {
		t.Fatalf("PruneServerAvailabilitySamples calls with availabilityRetention=24h = %d, want 1", telemOff.AvailabilityCalls())
	}
	if want := now.Add(-24 * time.Hour); !telemOff.AvailabilityBefore().Equal(want) {
		t.Fatalf("availability prune cutoff = %v, want %v (now-24h)", telemOff.AvailabilityBefore(), want)
	}

	// A non-positive availability retention disables ONLY the availability block;
	// a positive telemetry retention still prunes telemetry+benchmark.
	availOff := &fakeTelemetryPruner{}
	pruneTelemetryOnce(context.Background(), availOff, 168*time.Hour, 0, now)
	if availOff.AvailabilityCalls() != 0 {
		t.Fatalf("PruneServerAvailabilitySamples calls with availabilityRetention=0 = %d, want 0", availOff.AvailabilityCalls())
	}
	if availOff.Calls() != 1 {
		t.Fatalf("PruneTelemetrySamples calls with retention=168h = %d, want 1", availOff.Calls())
	}
	if availOff.BenchmarkCalls() != 1 {
		t.Fatalf("PruneBenchmarkRuns calls with retention=168h = %d, want 1", availOff.BenchmarkCalls())
	}

	// Both non-positive disables the whole job (store never touched).
	disabled := &fakeTelemetryPruner{}
	pruneTelemetryOnce(context.Background(), disabled, 0, 0, now)
	if disabled.Calls() != 0 || disabled.BenchmarkCalls() != 0 || disabled.AvailabilityCalls() != 0 {
		t.Fatalf("all-disabled prune touched the store: telemetry=%d benchmark=%d availability=%d",
			disabled.Calls(), disabled.BenchmarkCalls(), disabled.AvailabilityCalls())
	}
}

func TestRunTelemetryPruneLoopStops(t *testing.T) {
	fake := &fakeTelemetryPruner{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTelemetryPruneLoop(ctx, fake, 168*time.Hour, 720*time.Hour, 5*time.Millisecond, func() time.Time { return time.Now().UTC() })
		close(done)
	}()

	// Wait for at least one tick so we know the loop is live.
	deadline := time.Now().Add(time.Second)
	for fake.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fake.Calls() == 0 {
		t.Fatal("runTelemetryPruneLoop never ticked")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTelemetryPruneLoop did not return after context cancel")
	}
}

func TestSqliteDepsCleanupStopsPruneJob(t *testing.T) {
	// Substitute the prune-loop seam so we can observe the goroutine sqliteDeps starts:
	// drive the real loop over a countable fake pruner at a fast interval. We capture
	// the interval sqliteDeps passes (must be the production capturePruneInterval) but
	// tick the fake at 5ms so its calls are observable within the test.
	fake := &fakeCapturePruner{settings: map[string]string{}}
	orig := startCapturePruneLoop
	var gotInterval time.Duration
	startCapturePruneLoop = func(pruner capturePruner, interval time.Duration) context.CancelFunc {
		gotInterval = interval
		ctx, cancel := context.WithCancel(context.Background())
		go runCapturePruneLoop(ctx, fake, 5*time.Millisecond, func() time.Time { return time.Now().UTC() })
		return cancel
	}
	t.Cleanup(func() { startCapturePruneLoop = orig })

	dir := t.TempDir()
	cfg := config.Config{
		Addr:                 "127.0.0.1:8080",
		DBDriver:             "sqlite",
		SQLitePath:           filepath.Join(dir, "gateway.db"),
		AutoMigrate:          true,
		CaptureEncryptionKey: "0000000000000000000000000000000000000000000000000000000000000000",
	}

	_, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}

	// sqliteDeps must actually start the goroutine — a trivial pass (no wiring) leaves
	// calls at 0 forever.
	deadline := time.Now().Add(time.Second)
	for fake.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fake.Calls() == 0 {
		t.Fatal("prune loop never ticked: sqliteDeps did not start the goroutine")
	}
	// ...and must pass the production interval, not the test's fast one.
	if gotInterval != capturePruneInterval {
		t.Fatalf("prune interval = %v, want %v (capturePruneInterval)", gotInterval, capturePruneInterval)
	}

	// cleanup must cancel the prune goroutine's context AND close the store, without
	// erroring or hanging.
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup returned %v", err)
	}

	// After cancel, no further ticks may fire. Let the goroutine observe the cancel and
	// return, then confirm two reads across a window are equal.
	time.Sleep(50 * time.Millisecond)
	settled := fake.Calls()
	time.Sleep(50 * time.Millisecond)
	if got := fake.Calls(); got != settled {
		t.Fatalf("prune loop kept ticking after cleanup: calls %d -> %d", settled, got)
	}
}
