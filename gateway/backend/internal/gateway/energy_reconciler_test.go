// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePortalEnergySettings supplies a fixed SystemSettingsDTO (only the two
// energy-default fields the reconciler reads matter) via the same
// nil-embedded-interface trick as fakePortalAgentPresence in
// agent_reactivation_test.go: only the overridden method is ever called.
type fakePortalEnergySettings struct {
	portal.API
	mu       sync.Mutex
	calls    int
	settings portal.SystemSettingsDTO
}

func (f *fakePortalEnergySettings) SystemSettingsView(context.Context) portal.SystemSettingsDTO {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.settings
}

// energyFixture bundles everything one reconciler test needs: a real
// usage.Recorder + routing.MemoryStore (both satisfy their respective Store
// interfaces fully), a fake Portal returning the given system energy
// defaults, and a *Server wired directly (bypassing gateway.New) with a fresh
// idle tracker -- exercising reconcileEnergyOnce's own defensive defaulting of
// energySettleDelay/energyBackfillWindow (both left at the zero value here).
type energyFixture struct {
	t       *testing.T
	usage   *usage.Recorder
	routes  *routing.MemoryStore
	portalF *fakePortalEnergySettings
	srv     *Server
}

func newEnergyFixture(t *testing.T, sysDefaultPue, sysDefaultWhPerToken float64) *energyFixture {
	t.Helper()
	rec := usage.NewRecorder()
	mem := routing.NewMemoryStore()
	fp := &fakePortalEnergySettings{settings: portal.SystemSettingsDTO{
		EnergyDefaultPue:        sysDefaultPue,
		EnergyDefaultWhPerToken: sysDefaultWhPerToken,
	}}
	srv := &Server{
		Usage:      rec,
		Routes:     mem,
		Portal:     fp,
		EnergyIdle: newIdleTracker(time.Hour),
	}
	return &energyFixture{t: t, usage: rec, routes: mem, portalF: fp, srv: srv}
}

// createServer creates an AIServer with the given id + energy config.
func (f *energyFixture) createServer(id string, cfg routing.AIServer) {
	f.t.Helper()
	cfg.ID = id
	if err := f.routes.CreateAIServer(context.Background(), cfg); err != nil {
		f.t.Fatalf("createServer(%s): %v", id, err)
	}
}

// createMapping creates an application + a mapping on it in one call (the
// memory store's CreateMapping requires the application to already exist).
func (f *energyFixture) createMapping(mappingID, appID, serverID string, port int, mapping routing.ModelMapping) {
	f.t.Helper()
	ctx := context.Background()
	app := routing.Application{
		ID:       appID,
		ServerID: serverID,
		Type:     "mock",
		Port:     port,
		Status:   routing.ServerStatusActive,
	}
	if err := f.routes.CreateApplication(ctx, app); err != nil {
		f.t.Fatalf("createApplication(%s): %v", appID, err)
	}
	mapping.ID = mappingID
	mapping.ApplicationID = appID
	if mapping.Status == "" {
		mapping.Status = routing.ServerStatusActive
	}
	if err := f.routes.CreateMapping(ctx, mapping); err != nil {
		f.t.Fatalf("createMapping(%s): %v", mappingID, err)
	}
}

// insertSample inserts one telemetry sample for serverID (constPowerSamples/
// sampleAt from energy_engine_test.go don't set ServerID -- this fixture does).
func (f *energyFixture) insertSample(serverID string, sample routing.TelemetrySample) {
	f.t.Helper()
	sample.ServerID = serverID
	if err := f.routes.InsertTelemetrySample(context.Background(), sample); err != nil {
		f.t.Fatalf("insertSample: %v", err)
	}
}

// event returns the current stored copy of the event with id (fails the test
// if it no longer exists).
func (f *energyFixture) event(id string) usage.Event {
	f.t.Helper()
	for _, e := range f.usage.All() {
		if e.ID == id {
			return e
		}
	}
	f.t.Fatalf("event %s not found", id)
	return usage.Event{}
}

// mapping returns the current stored copy of the mapping with id.
func (f *energyFixture) mapping(id string) routing.ModelMapping {
	f.t.Helper()
	m, err := f.routes.MappingByID(context.Background(), id)
	if err != nil {
		f.t.Fatalf("mapping(%s): %v", id, err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Contract 1: target-exclusion from siblings (proven indirectly via the
// concurrency-halving already covered in energy_engine_test.go; here proven
// end-to-end through the reconciler: a solo event with no OTHER events on the
// server must NOT see itself double-counted as n=2).
// ---------------------------------------------------------------------------

func TestReconcileEnergyEventExcludesSelfFromSiblings(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{
		GatewayModelName: "gw", AppModelName: "upstream",
	})
	ev := usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000}
	f.usage.Record(ev)

	now := end.Add(20 * time.Second)
	f.srv.reconcileEnergyOnce(context.Background(), now)

	got := f.event("evt1")
	if got.EnergySource != "measured" {
		t.Fatalf("EnergySource = %q, want %q (got=%+v)", got.EnergySource, "measured", got)
	}
	// If UsageEventsForServerWindow's inclusion of the target itself were not
	// filtered out, ComputeEnergy would see n=2 (the target's own window
	// matching itself as a "sibling") and halve WhTotal to 0.5 instead of 1.0.
	requireCloseWh(t, "WhTotal (self must not double as its own sibling)", got.EnergyWh, 1.0)
}

// ---------------------------------------------------------------------------
// Contract 2: the system-wide PUE default is folded in BEFORE ComputeEnergy is
// called -- proven by comparing two otherwise-identical reconciler runs whose
// only difference is the system default PUE (1.0 vs 2.0), with the server's
// own Pue left at 0 (unset) in both.
// ---------------------------------------------------------------------------

func runMeasuredScenario(t *testing.T, sysDefaultPue float64) usage.Event {
	t.Helper()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, sysDefaultPue, 0)
	f.createServer("srv1", routing.AIServer{}) // Pue left at 0 (unset) -> must fall back to sysDefaultPue
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{
		GatewayModelName: "gw", AppModelName: "upstream",
	})
	ev := usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000}
	f.usage.Record(ev)

	now := end.Add(20 * time.Second)
	f.srv.reconcileEnergyOnce(context.Background(), now)
	return f.event("evt1")
}

func TestReconcileEnergyFoldsSystemPueDefault(t *testing.T) {
	base := runMeasuredScenario(t, 1.0)
	doubled := runMeasuredScenario(t, 2.0)

	if base.EnergySource != "measured" || doubled.EnergySource != "measured" {
		t.Fatalf("both runs must be measured, got base=%q doubled=%q", base.EnergySource, doubled.EnergySource)
	}
	if base.EnergyWh <= 0 {
		t.Fatalf("baseline (sysDefaultPue=1.0) EnergyWh must be > 0, got %v", base.EnergyWh)
	}
	// A wrong implementation that silently used effectivePue(cfg,0)=1.0
	// (ignoring the system default entirely) would make BOTH runs equal --
	// this assertion specifically catches that.
	requireCloseWh(t, "doubled EnergyWh (sysDefaultPue=2.0 must be exactly 2x the 1.0 baseline)", doubled.EnergyWh, base.EnergyWh*2)
	requireCloseWh(t, "doubled EnergyMarginalWh", doubled.EnergyMarginalWh, base.EnergyMarginalWh*2)
}

// A server's OWN Pue must still win over the system default (unchanged
// behavior, sanity-checked here at the reconciler level too).
func TestReconcileEnergyServerOwnPueWinsOverSystemDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 9.0, 0) // a system default that would obviously change the result if wrongly applied
	f.createServer("srv1", routing.AIServer{Pue: 1.0})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	requireCloseWh(t, "WhTotal (server Pue=1.0 must win over system default 9.0)", got.EnergyWh, 1.0)
}

// ---------------------------------------------------------------------------
// Contract 3: idleW resolution (server override wins, else the idle tracker).
// ---------------------------------------------------------------------------

func TestReconcileEnergyIdleWFromTracker(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{}) // IdleWatts unset -> must fall back to the tracker
	f.srv.EnergyIdle.Observe("srv1", 40, start.Add(-time.Minute))
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	// 100W over 36s -> 1.0 Wh total; idle=40W -> marginal integrand is 60W ->
	// 0.6 Wh (mirrors TestEnergyComputeTier1ConstantSolo's idleW=40 case).
	requireCloseWh(t, "WhTotal", got.EnergyWh, 1.0)
	requireCloseWh(t, "WhMarginal (idleW=40 from the tracker)", got.EnergyMarginalWh, 0.6)
}

func TestReconcileEnergyIdleWServerOverrideWinsOverTracker(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{IdleWatts: 10}) // explicit override
	f.srv.EnergyIdle.Observe("srv1", 40, start.Add(-time.Minute))
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	// idleW=10 (the server override, NOT the tracker's 40) -> marginal
	// integrand is 90W -> 0.9 Wh.
	requireCloseWh(t, "WhMarginal (server IdleWatts=10 must win over the tracker's 40)", got.EnergyMarginalWh, 0.9)
}

// ---------------------------------------------------------------------------
// Measured scenario also proves calibration: source=="measured" writes the
// mapping's EWMA-blended energy_wh_per_token coefficient.
// ---------------------------------------------------------------------------

func TestReconcileEnergyMeasuredCalibratesMapping(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	ev := f.event("evt1")
	if ev.EnergySource != "measured" {
		t.Fatalf("EnergySource = %q, want %q", ev.EnergySource, "measured")
	}
	if ev.EnergyWh <= 0 {
		t.Fatalf("EnergyWh must be > 0, got %v", ev.EnergyWh)
	}

	m := f.mapping("map1")
	if m.MetricsSource != "energy" {
		t.Fatalf("mapping MetricsSource = %q, want %q (calibration must have fired)", m.MetricsSource, "energy")
	}
	// A zero-value coefficient is seeded directly to the sample (see
	// UpdateMappingEnergyEWMA's doc): sample = WhMarginal / OutputTokens =
	// 1.0 / 1000 = 0.001 (idleW is 0 here -> marginal == total).
	requireCloseWh(t, "mapping EnergyWhPerToken (seeded from the first measured sample)", m.EnergyWhPerToken, 0.001)
}

func TestReconcileEnergyLockedMappingNotCalibrated(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{
		GatewayModelName: "gw", AppModelName: "upstream",
		EnergyWhPerToken: 0.5, MetricsLocked: true,
	})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	// The usage event itself is still priced (locking a mapping's metrics does
	// not stop its own requests from being attributed energy) ...
	ev := f.event("evt1")
	if ev.EnergySource != "measured" {
		t.Fatalf("EnergySource = %q, want %q", ev.EnergySource, "measured")
	}
	// ... but the LOCKED mapping's coefficient must be left untouched
	// (UpdateMappingEnergyEWMA's own metrics_locked=0 guard is a benign no-op).
	m := f.mapping("map1")
	if m.EnergyWhPerToken != 0.5 {
		t.Fatalf("locked mapping EnergyWhPerToken = %v, want unchanged 0.5", m.EnergyWhPerToken)
	}
}

// ---------------------------------------------------------------------------
// Estimated tier: no telemetry coverage, EstimatedWatts set.
// ---------------------------------------------------------------------------

func TestReconcileEnergyEstimatedTierNoTelemetry(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := base.Add(30 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{EstimatedWatts: 50}) // no telemetry inserted at all
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 30000, OutputTokens: 500})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	if got.EnergySource != "estimated" {
		t.Fatalf("EnergySource = %q, want %q (got=%+v)", got.EnergySource, "estimated", got)
	}
	requireCloseWh(t, "WhTotal (50W flat over 30s, no siblings)", got.EnergyWh, 50.0*30/3600)
}

// ---------------------------------------------------------------------------
// Modeled tier: neither telemetry nor EstimatedWatts.
// ---------------------------------------------------------------------------

func TestReconcileEnergyModeledTierMappingCoeff(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := base.Add(10 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{}) // no EstimatedWatts, no telemetry
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{
		GatewayModelName: "gw", AppModelName: "upstream", EnergyWhPerToken: 0.002,
	})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 10000, OutputTokens: 300})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	if got.EnergySource != "modeled" {
		t.Fatalf("EnergySource = %q, want %q (got=%+v)", got.EnergySource, "modeled", got)
	}
	requireCloseWh(t, "WhTotal (mapping coeff x tokens)", got.EnergyWh, 0.002*300)
	requireCloseWh(t, "WhMarginal (Tier 3 total==marginal)", got.EnergyMarginalWh, 0.002*300)
}

func TestReconcileEnergyModeledTierZeroCoeffStillModeled(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := base.Add(10 * time.Second)

	f := newEnergyFixture(t, 0, 0) // sysDefaultWhPerToken also 0
	f.createServer("srv1", routing.AIServer{})
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"}) // coeff 0
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 10000, OutputTokens: 300})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	got := f.event("evt1")
	if got.EnergySource != "modeled" {
		t.Fatalf("EnergySource = %q, want %q even at coeff==0 (got=%+v)", got.EnergySource, "modeled", got)
	}
	if got.EnergyWh != 0 {
		t.Fatalf("EnergyWh = %v, want 0", got.EnergyWh)
	}
}

// A deleted (no-longer-existing) server must still get a modeled/finalized
// stamp rather than being retried forever.
func TestReconcileEnergyEventForDeletedServerStillFinalized(t *testing.T) {
	f := newEnergyFixture(t, 0, 0)
	// Deliberately do NOT create the server "srv-gone".
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv-gone", RouteID: "", CreatedAt: time.Now().Add(-time.Minute), LatencyMS: 1000, OutputTokens: 100})

	f.srv.reconcileEnergyOnce(context.Background(), time.Now())

	got := f.event("evt1")
	if got.EnergySource != "modeled" {
		t.Fatalf("EnergySource = %q, want %q (a deleted server must still finalize via Tier 3)", got.EnergySource, "modeled")
	}
}

// ---------------------------------------------------------------------------
// Idempotency: a second pass must not reprocess a stamped event.
// ---------------------------------------------------------------------------

func TestReconcileEnergyOnceIsIdempotent(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)

	f := newEnergyFixture(t, 0, 0)
	f.createServer("srv1", routing.AIServer{})
	for i := 0; i <= 36; i++ {
		f.insertSample("srv1", sampleAt(start, i, 100))
	}
	f.createMapping("map1", "app1", "srv1", 8080, routing.ModelMapping{GatewayModelName: "gw", AppModelName: "upstream"})
	f.usage.Record(usage.Event{ID: "evt1", Host: "srv1", RouteID: "map1", CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000})

	now := end.Add(20 * time.Second)
	f.srv.reconcileEnergyOnce(context.Background(), now)

	first := f.event("evt1")
	firstMapping := f.mapping("map1")
	if first.EnergySource == "" {
		t.Fatalf("first pass did not stamp the event")
	}

	// A direct proof of idempotency at the SELECTION layer: the event no
	// longer matches UnpricedUsageEvents' energy_source=="" predicate.
	unpriced, err := f.usage.UnpricedUsageEvents(context.Background(), now.Add(-time.Hour), now, 0)
	if err != nil {
		t.Fatalf("UnpricedUsageEvents: %v", err)
	}
	for _, e := range unpriced {
		if e.ID == "evt1" {
			t.Fatalf("evt1 is still selected by UnpricedUsageEvents after being stamped")
		}
	}

	// Run a second pass at a later "now" (as the real ticker loop would) and
	// confirm nothing about the event or its mapping's calibration changed.
	f.srv.reconcileEnergyOnce(context.Background(), now.Add(time.Minute))

	second := f.event("evt1")
	if second.EnergyWh != first.EnergyWh || second.EnergyMarginalWh != first.EnergyMarginalWh || second.EnergySource != first.EnergySource {
		t.Fatalf("second pass changed the stamped event: first=%+v second=%+v", first, second)
	}
	secondMapping := f.mapping("map1")
	if secondMapping.EnergyWhPerToken != firstMapping.EnergyWhPerToken {
		t.Fatalf("second pass re-calibrated the mapping: first=%v second=%v", firstMapping.EnergyWhPerToken, secondMapping.EnergyWhPerToken)
	}
}

// ---------------------------------------------------------------------------
// Best-effort: a transient store error on ONE event must not block the rest
// of the batch.
// ---------------------------------------------------------------------------

// flakyRoutes wraps a real *routing.MemoryStore (which satisfies routing.Store
// via promoted methods) and injects a transient (non-NotFound) error from
// AIServerByID for exactly one server id, so every OTHER call behaves
// normally.
type flakyRoutes struct {
	*routing.MemoryStore
	failServerID string
	failCalls    atomic.Int32
}

var errFlakyTransient = errors.New("energy reconcile test: injected transient store error")

func (f *flakyRoutes) AIServerByID(ctx context.Context, id string) (routing.AIServer, error) {
	if id == f.failServerID {
		f.failCalls.Add(1)
		return routing.AIServer{}, errFlakyTransient
	}
	return f.MemoryStore.AIServerByID(ctx, id)
}

func TestReconcileEnergyOnceBestEffortOneBadEventDoesNotBlockOthers(t *testing.T) {
	f := newEnergyFixture(t, 0, 0)
	flaky := &flakyRoutes{MemoryStore: f.routes, failServerID: "srv-bad"}
	f.srv.Routes = flaky

	f.createServer("srv-bad", routing.AIServer{EstimatedWatts: 10})
	f.createServer("srv-good", routing.AIServer{EstimatedWatts: 50})
	f.createMapping("map-bad", "app-bad", "srv-bad", 8080, routing.ModelMapping{GatewayModelName: "gw-bad", AppModelName: "up-bad"})
	f.createMapping("map-good", "app-good", "srv-good", 8081, routing.ModelMapping{GatewayModelName: "gw-good", AppModelName: "up-good"})

	end := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	f.usage.Record(usage.Event{ID: "evt-bad", Host: "srv-bad", RouteID: "map-bad", CreatedAt: end, LatencyMS: 30000, OutputTokens: 100})
	f.usage.Record(usage.Event{ID: "evt-good", Host: "srv-good", RouteID: "map-good", CreatedAt: end, LatencyMS: 30000, OutputTokens: 100})

	f.srv.reconcileEnergyOnce(context.Background(), end.Add(20*time.Second))

	bad := f.event("evt-bad")
	if bad.EnergySource != "" {
		t.Fatalf("evt-bad EnergySource = %q, want %q (a transient lookup error must leave it unpriced, not stamp a bogus value)", bad.EnergySource, "")
	}
	if flaky.failCalls.Load() == 0 {
		t.Fatalf("the injected failure was never exercised -- test is not testing what it claims")
	}

	good := f.event("evt-good")
	if good.EnergySource != "estimated" {
		t.Fatalf("evt-good EnergySource = %q, want %q (the batch must continue past the bad event)", good.EnergySource, "estimated")
	}
	requireCloseWh(t, "evt-good WhTotal", good.EnergyWh, 50.0*30/3600)
}

// ---------------------------------------------------------------------------
// The loop wrapper: ticks and stops on cancel.
// ---------------------------------------------------------------------------

// countingUsageStore wraps a real *usage.Recorder (satisfies usage.Store via
// promotion) and counts UnpricedUsageEvents calls, to prove the ticker loop
// actually invokes reconcileEnergyOnce.
type countingUsageStore struct {
	*usage.Recorder
	calls atomic.Int32
}

func (c *countingUsageStore) UnpricedUsageEvents(ctx context.Context, notBefore, notAfter time.Time, limit int) ([]usage.Event, error) {
	c.calls.Add(1)
	return c.Recorder.UnpricedUsageEvents(ctx, notBefore, notAfter, limit)
}

func TestStartEnergyReconcilerTicksAndStopsOnCancel(t *testing.T) {
	cu := &countingUsageStore{Recorder: usage.NewRecorder()}
	srv := &Server{Usage: cu, Routes: routing.NewMemoryStore(), EnergyIdle: newIdleTracker(time.Hour)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.StartEnergyReconciler(ctx, 5*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for cu.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if cu.calls.Load() == 0 {
		t.Fatal("StartEnergyReconciler never ticked")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartEnergyReconciler did not return after context cancel")
	}
}

func TestStartEnergyReconcilerDefaultsNonPositiveInterval(t *testing.T) {
	// Exercises the <=0 -> defaultEnergyReconcileInterval fallback without
	// actually waiting ~15s for a tick: cancel immediately and just confirm it
	// returns promptly (no panic, no hang) rather than busy-looping on a zero
	// ticker (time.NewTicker(0) would panic).
	srv := &Server{Usage: usage.NewRecorder(), Routes: routing.NewMemoryStore(), EnergyIdle: newIdleTracker(time.Hour)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		srv.StartEnergyReconciler(ctx, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartEnergyReconciler(ctx-already-cancelled, 0) did not return")
	}
}
