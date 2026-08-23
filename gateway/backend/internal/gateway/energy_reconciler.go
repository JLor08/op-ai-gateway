// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"time"
)

const (
	// energyReconcileBatchLimit caps how many un-priced usage events one
	// reconcileEnergyOnce pass processes, mirroring UnpricedUsageEvents' own
	// documented default-limit convention: a large backlog is drained over
	// several passes instead of one huge query.
	energyReconcileBatchLimit = 500
	// energyCalibrationAlpha is the EWMA blend weight fed to
	// UpdateMappingEnergyEWMA when a measured event calibrates its mapping's
	// energy_wh_per_token coefficient.
	energyCalibrationAlpha = 0.2
	// defaultEnergySettleDelay/defaultEnergyBackfillWindow are the fallbacks
	// reconcileEnergyOnce applies when Server.energySettleDelay/
	// energyBackfillWindow are unset (<=0) -- which is always true for a
	// *Server built directly (bypassing gateway.New's ServerDeps-driven
	// defaulting), e.g. in a test.
	defaultEnergySettleDelay    = 10 * time.Second
	defaultEnergyBackfillWindow = 168 * time.Hour
	// energyPueDefaultTTL bounds how long systemEnergyDefaultPue memoizes the
	// system-wide energy_default_pue setting before re-reading it.
	energyPueDefaultTTL = 30 * time.Second
	// defaultEnergyReconcileInterval is StartEnergyReconciler's fallback tick
	// period when called with a non-positive interval.
	defaultEnergyReconcileInterval = 15 * time.Second
)

// StartEnergyReconciler runs reconcileEnergyOnce on a fixed-interval ticker
// until ctx is cancelled, mirroring the fixed-interval-retry style of
// runTelemetryPruneLoop/runCapturePruneLoop (cmd/gateway/prune.go). It is
// implemented as a *Server method rather than a free function taking a narrow
// dependency interface because reconcileEnergyEvent calls this package's own
// unexported energy-engine helpers (effectivePue, serverPowerW, modeledEnergy,
// ComputeEnergy, maxSampleGap) directly -- those are only reachable from
// within package gateway. The caller is expected to run this in its own
// goroutine (`go srv.StartEnergyReconciler(ctx, interval)`) and cancel ctx on
// shutdown; it blocks until then.
func (s *Server) StartEnergyReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultEnergyReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileEnergyOnce(ctx, time.Now().UTC())
		}
	}
}

// reconcileEnergyOnce attributes energy to every currently un-priced usage
// event (energy_source=="") whose request window has "settled" -- finished at
// least s.energySettleDelay ago, so its server's telemetry samples and
// concurrent sibling events have had time to land -- and is still within the
// backfill horizon (s.energyBackfillWindow), so a persistently-unpriceable
// event is not retried forever.
//
// Idempotent: UnpricedUsageEvents only ever selects energy_source=="" rows,
// and every event this pass actually looks at is unconditionally stamped via
// finalizeEnergyEvent (even a zero-Wh "modeled" fallback) before the next
// tick -- so a re-run of this exact pass reprocesses nothing. Best-effort per
// event: a lookup/store error on ONE event is Debug-logged and that event is
// skipped (left unpriced, retried on the reconciler's next pass) -- it never
// aborts the rest of the batch.
func (s *Server) reconcileEnergyOnce(ctx context.Context, now time.Time) {
	if s.Usage == nil || s.Routes == nil {
		return
	}
	settle := s.energySettleDelay
	if settle <= 0 {
		settle = defaultEnergySettleDelay
	}
	backfill := s.energyBackfillWindow
	if backfill <= 0 {
		backfill = defaultEnergyBackfillWindow
	}
	notAfter := now.Add(-settle)
	notBefore := now.Add(-backfill)

	events, err := s.Usage.UnpricedUsageEvents(ctx, notBefore, notAfter, energyReconcileBatchLimit)
	if err != nil {
		slog.Debug("energy reconcile: list unpriced events failed", "err", err)
		return
	}
	if len(events) == 0 {
		return
	}

	// Resolve the system-wide energy defaults ONCE per pass (not per event):
	// EnergyDefaultPue folds into contract 2 (ComputeEnergy cannot see it
	// itself); EnergyDefaultWhPerToken is Tier 3's sysDefaultWhPerToken input.
	// EnergyDefaultPricePerKwh is intentionally not read here -- costing is a
	// later phase.
	var sysDefaultPue, sysDefaultWhPerToken float64
	if s.Portal != nil {
		settings := s.Portal.SystemSettingsView(ctx)
		sysDefaultPue = settings.EnergyDefaultPue
		sysDefaultWhPerToken = settings.EnergyDefaultWhPerToken
	}

	for _, ev := range events {
		s.reconcileEnergyEvent(ctx, ev, now, sysDefaultPue, sysDefaultWhPerToken)
	}
}

// reconcileEnergyEvent attributes + persists energy for exactly one usage
// event. It honors the three engine contracts the pure ComputeEnergy engine
// does NOT resolve on its own:
//
//  1. UsageEventsForServerWindow returns the target event itself alongside its
//     siblings -- it is filtered out by id before being passed to
//     ComputeEnergy, which always counts the target as +1 on its own.
//  2. The system-wide PUE default is folded into cfg.Pue via effectivePue
//     BEFORE calling ComputeEnergy (which internally calls
//     effectivePue(cfg, 0) and so has no visibility into a system default).
//  3. The effective idle wattage (idleW) is resolved here -- the server's own
//     IdleWatts override wins, else the emergent per-server rolling-minimum
//     from s.EnergyIdle -- and passed as ComputeEnergy's explicit idleW
//     parameter (the engine ignores cfg.IdleWatts).
//
// Any lookup/store error that is NOT "the referenced row no longer exists" is
// treated as transient: the event is left unpriced (skipped, no stamp) and is
// retried on the reconciler's next pass.
func (s *Server) reconcileEnergyEvent(ctx context.Context, ev usage.Event, now time.Time, sysDefaultPue, sysDefaultWhPerToken float64) {
	mappingID, mappingCoeff := s.energyMappingCoeff(ctx, ev.RouteID)

	server, err := s.Routes.AIServerByID(ctx, ev.Host)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The server no longer exists (deleted) and can never regain
			// telemetry -- finalize now via Tier 3 (modeled), which always
			// succeeds (even at coeff==0 -> Wh==0), rather than retrying this
			// event forever against a server that will never come back.
			s.finalizeEnergyEvent(ctx, ev, modeledEnergy(ev, mappingCoeff, sysDefaultWhPerToken))
			return
		}
		slog.Debug("energy reconcile: server lookup failed", "event_id", ev.ID, "server_id", ev.Host, "err", err)
		return
	}

	// Contract 2: fold the system PUE default into cfg.Pue up front.
	cfg := ServerEnergyConfig{
		EstimatedWatts: server.EstimatedWatts,
		Pue:            effectivePue(ServerEnergyConfig{Pue: server.Pue}, sysDefaultPue),
	}

	// Contract 3: resolve the effective idle wattage.
	idleW := server.IdleWatts
	if idleW <= 0 {
		idleW = s.EnergyIdle.Idle(server.ID)
	}

	start := ev.CreatedAt.Add(-time.Duration(ev.LatencyMS) * time.Millisecond)
	end := ev.CreatedAt

	// Telemetry samples are only useful when Tier 1 could possibly apply
	// (end > start); ComputeEnergy itself gates Tier 1 on the same condition,
	// so skipping the store round-trip on a degenerate (zero/negative-latency)
	// window changes nothing about the result.
	var samples []routing.TelemetrySample
	if end.After(start) {
		samples, err = s.Routes.TelemetrySamples(ctx, ev.Host, start.Add(-maxSampleGap), end.Add(maxSampleGap), 0)
		if err != nil {
			slog.Debug("energy reconcile: telemetry samples lookup failed", "event_id", ev.ID, "server_id", ev.Host, "err", err)
			return
		}
	}

	rawSiblings, err := s.Usage.UsageEventsForServerWindow(ctx, ev.Host, start, end)
	if err != nil {
		slog.Debug("energy reconcile: sibling events lookup failed", "event_id", ev.ID, "server_id", ev.Host, "err", err)
		return
	}
	// Contract 1: UsageEventsForServerWindow includes the target event itself.
	siblings := make([]usage.Event, 0, len(rawSiblings))
	for _, sib := range rawSiblings {
		if sib.ID == ev.ID {
			continue
		}
		siblings = append(siblings, sib)
	}

	res := ComputeEnergy(ev, samples, siblings, cfg, idleW, mappingCoeff, sysDefaultWhPerToken)
	s.finalizeEnergyEvent(ctx, ev, res)

	// Calibration: only on a genuine per-request power MEASUREMENT, only with a
	// resolvable mapping to calibrate, and only with real output tokens to
	// divide by (a zero-token event carries no per-token signal).
	// UpdateMappingEnergyEWMA is itself metrics_locked-guarded at the store
	// layer (a locked mapping's write is a benign no-op), so no separate lock
	// check is needed here.
	if res.Source == "measured" && ev.OutputTokens > 0 && mappingID != "" {
		sample := res.WhMarginal / float64(ev.OutputTokens)
		if err := s.Routes.UpdateMappingEnergyEWMA(ctx, mappingID, sample, energyCalibrationAlpha, now); err != nil {
			slog.Debug("energy reconcile: calibration write failed", "mapping_id", mappingID, "err", err)
		}
	}
}

// energyMappingCoeff resolves a usage event's own per-token energy
// coefficient (ComputeEnergy's mappingCoeff input) plus the mapping id (used
// only to target the calibration write). A missing/unset route_id or an
// unresolvable mapping degrades to ("", 0) rather than failing the event --
// Tier 3 then falls back to the system default, and calibration is skipped
// (mappingID == "").
func (s *Server) energyMappingCoeff(ctx context.Context, routeID string) (mappingID string, coeff float64) {
	if routeID == "" {
		return "", 0
	}
	mapping, err := s.Routes.MappingByID(ctx, routeID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Debug("energy reconcile: mapping lookup failed", "route_id", routeID, "err", err)
		}
		return "", 0
	}
	return mapping.ID, mapping.EnergyWhPerToken
}

// finalizeEnergyEvent stamps ev's computed energy via UpdateUsageEventEnergy.
// This is the ONLY write that clears an event's energy_source=="" (the
// UnpricedUsageEvents selection predicate), so it MUST run for every event
// reconcileEnergyEvent actually attributes -- including a zero-Wh "modeled"
// fallback -- for the reconciler to stay idempotent: a stamped event, of any
// source/value, is never reprocessed.
func (s *Server) finalizeEnergyEvent(ctx context.Context, ev usage.Event, res EnergyResult) {
	if err := s.Usage.UpdateUsageEventEnergy(ctx, ev.ID, res.WhTotal, res.WhMarginal, res.Source); err != nil {
		slog.Debug("energy reconcile: stamp event failed", "event_id", ev.ID, "err", err)
	}
}

// systemEnergyDefaultPue returns the system-wide energy_default_pue setting,
// memoized for energyPueDefaultTTL (via settingCache -- ttlcache.go) so the
// per-telemetry-ingest idle-tracker Observe hook (ingestTelemetrySample,
// server.go) does not read system_settings on every agent report (which
// arrives on a ~1s cadence). reconcileEnergyOnce, by contrast, already reads
// system settings at most once per (much longer) reconcile-interval pass and
// needs no separate cache. Falls back to 0 (unset) when Portal is nil (a bare
// test Server).
func (s *Server) systemEnergyDefaultPue(ctx context.Context) float64 {
	return s.energyPueDefault.Get(ctx, energyPueDefaultTTL, func(ctx context.Context) float64 {
		if s.Portal != nil {
			return s.Portal.SystemSettingsView(ctx).EnergyDefaultPue
		}
		return 0
	})
}
