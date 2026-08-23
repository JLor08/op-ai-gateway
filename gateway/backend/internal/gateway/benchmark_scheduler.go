// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"time"
)

const (
	// benchmarkSchedulerTick is how often the scheduler wakes to look for a due
	// application. The per-app cadence is enforced separately (benchmarkDue).
	benchmarkSchedulerTick = time.Minute
	// benchmarkScheduleMinSeconds floors the effective per-app interval so a tiny
	// (mis)configured value can't hammer the idle-gated scheduler every tick.
	benchmarkScheduleMinSeconds = 60
)

// effectiveBenchmarkIntervalSeconds resolves an app's cadence: its own value, else the
// system default, floored so a tiny value can't hammer the idle-gated scheduler.
func effectiveBenchmarkIntervalSeconds(app routing.Application, defaultSeconds int) int {
	sec := app.BenchmarkScheduleIntervalSeconds
	if sec <= 0 {
		sec = defaultSeconds
	}
	if sec < benchmarkScheduleMinSeconds {
		sec = benchmarkScheduleMinSeconds
	}
	return sec
}

// benchmarkDue reports whether an application's scheduled benchmark is due now:
// the app must have the schedule enabled, and either never have run or have last
// run at least its effective interval ago.
func benchmarkDue(app routing.Application, lastRun map[string]time.Time, now time.Time, defaultSeconds int) bool {
	if !app.BenchmarkScheduleEnabled {
		return false
	}
	last, ok := lastRun[app.ID]
	if !ok {
		return true
	}
	return now.Sub(last) >= time.Duration(effectiveBenchmarkIntervalSeconds(app, defaultSeconds))*time.Second
}

// TriggerScheduledBenchmark is the scheduled-mode analog of startBenchmark's core: no auth
// token (trusted internal caller), skips inactive + metrics_locked mappings, reuses the P3a
// runner verbatim. Returns true if a run was launched OR one is already in progress OR there
// was nothing to do; false only if the idle-gate deferred (so the caller retries next tick).
func (s *Server) TriggerScheduledBenchmark(ctx context.Context, server routing.AIServer, app routing.Application) bool {
	mappings, err := s.Routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return false
	}
	targets := make([]benchmarkTarget, 0, len(mappings))
	for _, m := range mappings {
		if m.Status != routing.ServerStatusActive || m.MetricsLocked {
			continue
		}
		targets = append(targets, benchmarkTarget{server: server, app: app, mapping: m})
	}
	if len(targets) == 0 {
		return true
	}
	// The run outlives this trigger (it executes on a background context that
	// TryStart's cancel tears down when the run finishes / is released).
	runCtx, cancel := context.WithCancel(context.Background()) // background (not the scheduler ctx) is intentional so a launched run OUTLIVES a scheduler shutdown, self-terminating via the idle watchdog — mirrors the manual startBenchmark path.
	run, ok := s.Benchmarks.TryStart(server.ID, "scheduled", "speed", len(targets), time.Now().UTC(), cancel)
	if !ok {
		// A run (a sibling scheduled run or a manual run) is already in flight on this
		// server's one-run-per-server slot. Defer like the idle-gate: return false so this
		// app is NOT stamped and retries next tick once the slot frees. Stamping here would
		// keep co-located apps' due windows in lockstep with the winner's, starving every
		// app but the first-enumerated one; deferring instead naturally staggers them.
		cancel()
		return false
	}
	// Idle gate: the server is now RESERVED (ServerBusy true → excluded from routing);
	// if it STILL has in-flight traffic, release the reservation and defer so a scheduled
	// benchmark never competes with live requests. The caller retries next tick.
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		return false
	}
	go func() {
		defer cancel()
		s.runBenchmark(runCtx, run, server.ID, targets, "speed") // scheduled runs are speed-only (capacity is manual in CP2a)
	}()
	return true
}

// runScheduledBenchmarkPass enumerates active applications and triggers a due scheduled
// benchmark per app, stamping lastRun only when actually launched (an idle-gate defer
// leaves lastRun untouched so the run is retried next tick). It also prunes lastRun
// entries for applications that no longer appear so the map stays bounded.
func (s *Server) runScheduledBenchmarkPass(ctx context.Context, now time.Time, lastRun map[string]time.Time, defaultSeconds int) {
	servers, err := s.Routes.AIServers(ctx)
	if err != nil {
		return
	}
	seen := make(map[string]bool)
	for _, server := range servers {
		apps, err := s.Routes.ApplicationsByServer(ctx, server.ID)
		if err != nil {
			continue
		}
		for _, app := range apps {
			if app.Status != routing.ServerStatusActive || !app.BenchmarkScheduleEnabled {
				continue
			}
			seen[app.ID] = true
			if !benchmarkDue(app, lastRun, now, defaultSeconds) {
				continue
			}
			if s.TriggerScheduledBenchmark(ctx, server, app) {
				lastRun[app.ID] = now
			}
		}
	}
	for id := range lastRun {
		if !seen[id] {
			delete(lastRun, id)
		}
	}
}

// StartBenchmarkScheduler runs runScheduledBenchmarkPass on a ticker until the returned
// cancel is called. A no-op-safe sibling of the health loop, started after gateway.New.
func (s *Server) StartBenchmarkScheduler(defaultSeconds int) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	lastRun := make(map[string]time.Time)
	go func() {
		ticker := time.NewTicker(benchmarkSchedulerTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runScheduledBenchmarkPass(ctx, time.Now().UTC(), lastRun, defaultSeconds)
			}
		}
	}()
	return cancel
}
