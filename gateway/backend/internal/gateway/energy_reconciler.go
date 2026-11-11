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
// ComputeEnergy, ComputeEnergyPart, maxSampleGap) directly -- those are only reachable from
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
// event: the event's own host as before (ComputeEnergy over the inputs
// hostEnergyInput reads, honoring the engine's contracts), plus, for a
// llama.cpp RPC cluster request, one part per member host
// (clusterEnergyParts).
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
			// Parts from an earlier attempt go first: this figure has none.
			if !s.clearEnergyParts(ctx, ev) {
				return
			}
			s.finalizeEnergyEvent(ctx, ev, modeledEnergy(ev, mappingCoeff, sysDefaultWhPerToken))
			return
		}
		slog.Debug("energy reconcile: server lookup failed", "event_id", ev.ID, "server_id", ev.Host, "err", err)
		return
	}

	head, err := s.hostEnergyInput(ctx, ev, server, sysDefaultPue)
	if err != nil {
		slog.Debug("energy reconcile: host input lookup failed", "event_id", ev.ID, "server_id", ev.Host, "err", err)
		return
	}
	res := ComputeEnergy(ev, head.samples, head.siblings, head.cfg, head.idleW, mappingCoeff, sysDefaultWhPerToken)

	// A llama.cpp RPC cluster request lists the members its head held when
	// it completed; each of their hosts adds its own part.
	hosts, err := s.Usage.EventHosts(ctx, ev.ID)
	if err != nil {
		slog.Debug("energy reconcile: event hosts lookup failed", "event_id", ev.ID, "err", err)
		return
	}
	parts, res, ok := s.clusterEnergyParts(ctx, ev, hosts, res, sysDefaultPue)
	if !ok {
		return
	}
	// The parts are written before the stamp: a stamped event is never priced
	// again, so a stamp without its parts would stay without them. An event
	// listing other hosts that ends up without parts (its head priced per
	// token) has any parts an earlier, unstamped attempt wrote removed, since
	// every per-host sum reads parts in place of energy_wh.
	if len(hosts) > 1 || len(parts) > 0 {
		if err := s.Usage.SetEnergyParts(ctx, ev.ID, parts); err != nil {
			slog.Debug("energy reconcile: energy parts write failed", "event_id", ev.ID, "err", err)
			return
		}
	}
	s.finalizeEnergyEvent(ctx, ev, res)

	// Calibration: only on a genuine per-request power MEASUREMENT, only on a
	// TOKEN-METERED row, only with a resolvable mapping to calibrate, and only
	// with real output tokens to divide by. For a cluster request "measured"
	// means every part was measured (the weakest part sets the source), so the
	// coefficient learns the whole cluster's energy per token -- the figure
	// Tier 3 then prices a whole request with -- and never a share of it.
	//
	// The billing-unit conjunct is explicit rather than implied. The divide at
	// the next line is guarded today only by OutputTokens > 0, which holds only
	// because a producer in another package honours an unenforced convention --
	// and dividing WhMarginal by a billing_quantity here is one of the two
	// surfaces that would tempt a unit-to-token conversion, writing a per-image
	// number into energy_wh_per_token. See ADR-041.
	//
	// UpdateMappingEnergyEWMA is itself metrics_locked-guarded at the store
	// layer (a locked mapping's write is a benign no-op), so no separate lock
	// check is needed here.
	//
	// A request on a cluster head's mapping without parts measured the head's
	// share only (its member list could not be recorded, or no member was in
	// session), so it never calibrates the whole-cluster coefficient.
	if res.Source == "measured" && ev.BillingUnit == usage.BillingUnitTokens && ev.OutputTokens > 0 && mappingID != "" &&
		(len(parts) > 0 || !s.mappingIsClusterHead(ctx, mappingID)) {
		sample := res.WhMarginal / float64(ev.OutputTokens)
		if err := s.Routes.UpdateMappingEnergyEWMA(ctx, mappingID, sample, energyCalibrationAlpha, now); err != nil {
			slog.Debug("energy reconcile: calibration write failed", "mapping_id", mappingID, "err", err)
		}
	}
}

// clearEnergyParts removes the energy parts of ev when it lists hosts other
// than its own (only such an event can have parts). false means a lookup or
// the write failed: the event stays unpriced for a retry.
func (s *Server) clearEnergyParts(ctx context.Context, ev usage.Event) bool {
	hosts, err := s.Usage.EventHosts(ctx, ev.ID)
	if err != nil {
		slog.Debug("energy reconcile: event hosts lookup failed", "event_id", ev.ID, "err", err)
		return false
	}
	if len(hosts) <= 1 {
		return true
	}
	if err := s.Usage.SetEnergyParts(ctx, ev.ID, nil); err != nil {
		slog.Debug("energy reconcile: energy parts clear failed", "event_id", ev.ID, "err", err)
		return false
	}
	return true
}

// mappingIsClusterHead reports whether mappingID's runtime spec is a
// llama.cpp RPC cluster head, through the instance source's spec cache when
// the server has one (so a run of measured requests on one mapping reads its
// spec once per cache period, not once per request), else by a store read. A
// spec that cannot be read counts as a head: calibration is then skipped,
// which only delays it.
func (s *Server) mappingIsClusterHead(ctx context.Context, mappingID string) bool {
	if s.clusterHeads != nil {
		head, known := s.clusterHeads.specIsClusterHead(ctx, mappingID)
		if !known {
			slog.Debug("energy reconcile: runtime spec unknown; calibration skipped", "mapping_id", mappingID)
			return true
		}
		return head
	}
	spec, found, err := s.Routes.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil {
		slog.Debug("energy reconcile: runtime spec lookup failed; calibration skipped", "mapping_id", mappingID, "err", err)
		return true
	}
	return found && spec.ClusterEnabled
}

// hostEnergyInput is what pricing one host's share of an event needs: the
// host's energy configuration (system PUE default folded in), its effective
// idle draw, its telemetry around the event's window and the other events it
// served in that window.
type hostEnergyInput struct {
	cfg      ServerEnergyConfig
	idleW    float64
	samples  []routing.TelemetrySample
	siblings []usage.Event
}

// hostEnergyInput reads server's inputs for pricing its share of ev. It
// honors the three engine contracts the pure engine does NOT resolve on its
// own:
//
//  1. UsageEventsForServerWindow returns the target event itself alongside its
//     siblings -- it is filtered out by id here, since the engine always
//     counts the target as +1 on its own. The siblings include every cluster
//     event that lists server among its hosts, so a member host's power is
//     split over its own requests and the cluster requests that used it.
//  2. The system-wide PUE default is folded into cfg.Pue via effectivePue
//     (the engine calls effectivePue(cfg, 0) and so has no visibility into a
//     system default).
//  3. The effective idle wattage is resolved here -- the server's own
//     IdleWatts override wins, else the emergent per-server rolling-minimum
//     from s.EnergyIdle.
func (s *Server) hostEnergyInput(ctx context.Context, ev usage.Event, server routing.AIServer, sysDefaultPue float64) (hostEnergyInput, error) {
	in := hostEnergyInput{
		cfg: ServerEnergyConfig{
			EstimatedWatts: server.EstimatedWatts,
			Pue:            effectivePue(ServerEnergyConfig{Pue: server.Pue}, sysDefaultPue),
		},
		idleW: server.IdleWatts,
	}
	if in.idleW <= 0 {
		in.idleW = s.EnergyIdle.Idle(server.ID)
	}

	start := ev.CreatedAt.Add(-time.Duration(ev.LatencyMS) * time.Millisecond)
	end := ev.CreatedAt

	// Telemetry samples are only useful when Tier 1 could possibly apply
	// (end > start); the engine itself gates Tier 1 on the same condition,
	// so skipping the store round-trip on a degenerate (zero/negative-latency)
	// window changes nothing about the result.
	if end.After(start) {
		samples, err := s.Routes.TelemetrySamples(ctx, server.ID, start.Add(-maxSampleGap), end.Add(maxSampleGap), 0)
		if err != nil {
			return hostEnergyInput{}, err
		}
		in.samples = samples
	}

	rawSiblings, err := s.Usage.UsageEventsForServerWindow(ctx, server.ID, start, end)
	if err != nil {
		return hostEnergyInput{}, err
	}
	in.siblings = make([]usage.Event, 0, len(rawSiblings))
	for _, sib := range rawSiblings {
		if sib.ID == ev.ID {
			continue
		}
		in.siblings = append(in.siblings, sib)
	}
	return in, nil
}

// clusterEnergyParts splits a cluster request's energy into per-host parts.
// head is the head host's result, computed as for any request. An event
// listing no host besides its own gets no parts and keeps head. ok is false
// when a lookup failed: the event is left unpriced and retried.
//
// Otherwise each member host's part is its own Tier 1 or Tier 2 share of the
// request (ComputeEnergyPart); a member with neither is a 0 Wh part marked
// energySourceMemberUnpriced. energy_wh is the sum of the parts and
// energy_source the weakest of them, member_unpriced weakest of all. A head
// priced by Tier 3 (or unpriceable) gets no parts: a per-token figure is
// calibrated from whole-cluster energy and already prices every host, so
// adding member parts to it would count them twice.
func (s *Server) clusterEnergyParts(ctx context.Context, ev usage.Event, hosts []usage.EventHost, head EnergyResult, sysDefaultPue float64) (parts []usage.EnergyPart, total EnergyResult, ok bool) {
	members := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h.ServerID != ev.Host {
			members = append(members, h.ServerID)
		}
	}
	if len(members) == 0 || (head.Source != "measured" && head.Source != "estimated") {
		return nil, head, true
	}
	parts = append(parts, usage.EnergyPart{
		EventID: ev.ID, ServerID: ev.Host, Role: usage.EventHostRoleHead,
		Wh: head.WhTotal, MarginalWh: head.WhMarginal, Source: head.Source,
	})
	total = head
	for _, member := range members {
		part, err := s.memberEnergyPart(ctx, ev, member, sysDefaultPue)
		if err != nil {
			slog.Debug("energy reconcile: member part lookup failed", "event_id", ev.ID, "server_id", member, "err", err)
			return nil, EnergyResult{}, false
		}
		parts = append(parts, usage.EnergyPart{
			EventID: ev.ID, ServerID: member, Role: usage.EventHostRoleMember,
			Wh: part.WhTotal, MarginalWh: part.WhMarginal, Source: part.Source,
		})
		total.WhTotal += part.WhTotal
		total.WhMarginal += part.WhMarginal
		total.Source = weakerEnergySource(total.Source, part.Source)
	}
	return parts, total, true
}

// memberEnergyPart prices member's share of ev. A member server that no
// longer exists can never report again, so it is unpriced rather than
// retried.
func (s *Server) memberEnergyPart(ctx context.Context, ev usage.Event, member string, sysDefaultPue float64) (EnergyResult, error) {
	server, err := s.Routes.AIServerByID(ctx, member)
	if errors.Is(err, store.ErrNotFound) {
		return EnergyResult{Source: energySourceMemberUnpriced}, nil
	}
	if err != nil {
		return EnergyResult{}, err
	}
	in, err := s.hostEnergyInput(ctx, ev, server, sysDefaultPue)
	if err != nil {
		return EnergyResult{}, err
	}
	if res, ok := ComputeEnergyPart(ev, in.samples, in.siblings, in.cfg, in.idleW); ok {
		return res, nil
	}
	return EnergyResult{Source: energySourceMemberUnpriced}, nil
}

// energySourceRank orders the sources a part can carry from strongest to
// weakest.
var energySourceRank = map[string]int{
	"measured":                 0,
	"estimated":                1,
	energySourceMemberUnpriced: 2,
}

// weakerEnergySource returns the weaker of two part sources.
func weakerEnergySource(a, b string) string {
	if energySourceRank[b] > energySourceRank[a] {
		return b
	}
	return a
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
