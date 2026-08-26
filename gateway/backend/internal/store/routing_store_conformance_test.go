// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// This file is the MEMORY-vs-SQL routing.Store conformance suite (RT-2). The
// existing conformance suite above (forEachDialect) proves sqlite and postgres
// behave identically, but never exercises routing.MemoryStore — even though
// MemoryStore is the production Playwright/dev driver. A behavior that only
// the SQL store implements correctly (or vice versa) is invisible to
// forEachDialect and ships straight to the memory-mode e2e suite. Every
// subtest below runs against BOTH backends, through the routing.Store
// interface only, with identical inputs and identical hardcoded-expected
// assertions — so a divergence in either implementation fails its own
// t.Run() subtest.

// forEachRoutingStore runs run against both production routing.Store
// backends: routing.NewMemoryStore() and a freshly migrated sqlite
// *SQLStore. Use forEachRoutingStoreSeeded instead when the subtest needs to
// seed a row that routing.Store itself has no method for (e.g. an
// api_tokens/users row that route_affinity's FK requires on the SQL side but
// MemoryStore does not enforce at all).
func forEachRoutingStore(t *testing.T, run func(t *testing.T, s routing.Store)) {
	forEachRoutingStoreSeeded(t, nil, run)
}

// forEachRoutingStoreSeeded is forEachRoutingStore with an extra hook that
// runs against the concrete *SQLStore, BEFORE it is handed to run as a plain
// routing.Store, to satisfy an FK routing.Store has no method to populate.
// seedSQL is skipped for the memory backend: MemoryStore has no FK
// enforcement, so it needs no such seed to accept the same input data.
func forEachRoutingStoreSeeded(t *testing.T, seedSQL func(t *testing.T, s *SQLStore), run func(t *testing.T, s routing.Store)) {
	t.Run("memory", func(t *testing.T) {
		run(t, routing.NewMemoryStore())
	})
	t.Run("sqlite", func(t *testing.T) {
		sqlStore, err := OpenSQLite(filepath.Join(t.TempDir(), "rt2-routing-conformance.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		defer sqlStore.Close()
		if err := sqlStore.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate sqlite: %v", err)
		}
		if seedSQL != nil {
			seedSQL(t, sqlStore)
		}
		run(t, sqlStore)
	})
}

// --- ActiveMappingsForModel filtering + ordering ----------------------------

// TestRoutingStoreActiveMappingsForModel proves both backends apply the same
// three filters (mapping status, mapping gateway model name, application
// status + served API flavor) and the same tie-break ordering (ascending by
// mapping id), on identical seed data.
func TestRoutingStoreActiveMappingsForModel(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "S1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		newApp := func(id string, port int, flavor, status string) routing.Application {
			return routing.Application{
				ID: id, ServerID: "srv1", Type: "ollama", Port: port, Scheme: "http",
				APIFlavors: []string{flavor}, Priority: 1, Weight: 1,
				TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: status,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
				CreatedAt:       now, UpdatedAt: now,
			}
		}
		apps := []routing.Application{
			newApp("app1", 11434, routing.APIFlavorOpenAI, routing.ServerStatusActive),    // matches
			newApp("app2", 11435, routing.APIFlavorAnthropic, routing.ServerStatusActive), // wrong flavor
			newApp("app3", 11436, routing.APIFlavorOpenAI, routing.ServerStatusDisabled),  // disabled app
		}
		for _, app := range apps {
			if err := s.CreateApplication(ctx, app); err != nil {
				t.Fatalf("create application %s: %v", app.ID, err)
			}
		}

		newMapping := func(id, appID, model, status string) routing.ModelMapping {
			return routing.ModelMapping{
				ID: id, ApplicationID: appID, GatewayModelName: model, AppModelName: "up-" + id,
				Status: status, CreatedAt: now, UpdatedAt: now,
			}
		}
		mappings := []routing.ModelMapping{
			newMapping("map_b", "app1", "gpt-4o-mini", routing.ServerStatusActive), // matches
			newMapping("map_a", "app1", "gpt-4o-mini", routing.ServerStatusActive), // matches, sorts before map_b
			newMapping("map_c1", "app1", "other-model", routing.ServerStatusActive),
			newMapping("map_c2", "app1", "gpt-4o-mini", routing.ServerStatusDisabled),
			newMapping("map_c3", "app2", "gpt-4o-mini", routing.ServerStatusActive),
			newMapping("map_c4", "app3", "gpt-4o-mini", routing.ServerStatusActive),
		}
		for _, mapping := range mappings {
			if err := s.CreateMapping(ctx, mapping); err != nil {
				t.Fatalf("create mapping %s: %v", mapping.ID, err)
			}
		}

		got, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("active mappings: %v", err)
		}
		var ids []string
		for _, c := range got {
			ids = append(ids, c.Mapping.ID)
			if c.Server.ID != "srv1" || c.Application.ID != "app1" {
				t.Fatalf("candidate %s joined the wrong server/application: %+v", c.Mapping.ID, c)
			}
		}
		want := []string{"map_a", "map_b"}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("ActiveMappingsForModel ids = %v, want %v (filter+order mismatch)", ids, want)
		}

		// The SAME gateway model queried under the Anthropic flavor routes to the
		// OTHER application (app2, which serves Anthropic, not OpenAI): the flavor
		// filter must swap which mapping matches, not just narrow the OpenAI set.
		anthropic, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorAnthropic)
		if err != nil {
			t.Fatalf("active mappings (anthropic flavor): %v", err)
		}
		if len(anthropic) != 1 || anthropic[0].Mapping.ID != "map_c3" || anthropic[0].Application.ID != "app2" {
			t.Fatalf("active mappings (anthropic flavor) = %+v, want exactly map_c3/app2", anthropic)
		}

		// A gateway model name that matches no mapping at all yields no candidates.
		none, err := s.ActiveMappingsForModel(ctx, "no-such-model", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("active mappings (unknown model): %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("expected 0 candidates for an unknown model, got %d: %+v", len(none), none)
		}
	})
}

// --- Affinity upsert/lookup/delete round-trip -------------------------------

// TestRoutingStoreAffinityRoundTrip proves UpsertAffinity/Affinity/DeleteAffinity
// agree on both backends: miss before upsert, insert, re-pin (update
// resolved_model in place), then delete.
func TestRoutingStoreAffinityRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// route_affinity.api_token_id/user_id carry FKs the SQL store enforces but
	// routing.Store has no method to satisfy (api_tokens/users are auth
	// concepts, not routing ones). Seed them directly against the concrete
	// *SQLStore before it is downcast to routing.Store. MemoryStore does not
	// check these at all, so the same literal ids ("tok1"/"u1") work there
	// with no seed.
	seedSQL := func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateUser(ctx, newTestUser("u1", "rt2-affinity@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok1", UserID: "u1", Name: "rt2", CreatedAt: now, UpdatedAt: now}, "rt2-affinity-secret"); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}

	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "S1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}

		key := routing.AffinityKey{APITokenID: "tok1", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess1"}

		if _, ok, err := s.Affinity(ctx, key); err != nil || ok {
			t.Fatalf("expected no affinity before upsert, ok=%v err=%v", ok, err)
		}

		aff := routing.RouteAffinity{
			ID: "aff1", APITokenID: "tok1", UserID: "u1",
			Model: "fast-coder", ResolvedModel: "model-b", APIFlavor: routing.APIFlavorOpenAI,
			SessionID: "sess1", ApplicationID: "app1", ServerID: "srv1",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertAffinity(ctx, aff); err != nil {
			t.Fatalf("upsert affinity: %v", err)
		}
		got, ok, err := s.Affinity(ctx, key)
		if err != nil || !ok {
			t.Fatalf("read affinity: ok=%v err=%v", ok, err)
		}
		if got.ResolvedModel != "model-b" || got.ApplicationID != "app1" || got.ServerID != "srv1" {
			t.Fatalf("affinity after insert = %+v, want ResolvedModel=model-b ApplicationID=app1 ServerID=srv1", got)
		}

		// Re-pin to a different resolved model; the upsert must update in place
		// (same unique key: api_token_id, model, api_flavor, session_id).
		aff.ResolvedModel = "model-a"
		aff.UpdatedAt = now.Add(time.Minute)
		if err := s.UpsertAffinity(ctx, aff); err != nil {
			t.Fatalf("re-upsert affinity: %v", err)
		}
		got2, ok, err := s.Affinity(ctx, key)
		if err != nil || !ok || got2.ResolvedModel != "model-a" {
			t.Fatalf("affinity after re-pin = %+v ok=%v err=%v, want ResolvedModel=model-a", got2, ok, err)
		}

		if err := s.DeleteAffinity(ctx, key); err != nil {
			t.Fatalf("delete affinity: %v", err)
		}
		if _, ok, err := s.Affinity(ctx, key); err != nil || ok {
			t.Fatalf("expected no affinity after delete, ok=%v err=%v", ok, err)
		}
	})
}

// --- Opportunistic-metrics EWMA: parity + locked/missing no-op -------------

// TestRoutingStoreOpportunisticMetricsEWMA is the guard that replaces the
// un-extractable EWMA duplication (MemoryStore's ewma() helper vs the 3-branch
// SQL CASE in UpdateMappingOpportunisticMetrics, deliberately left in place
// per RT-2 — the SQL CASE lives inside an atomic `metrics_locked = 0` guarded
// UPDATE, so a shared Go implementation would lose that atomicity). It runs
// the exact same sample sequence against both backends and asserts the exact
// same blended values, so a change to either ewma formula that breaks parity
// fails this test. It also proves both backends apply the same locked/missing
// no-op guard.
func TestRoutingStoreOpportunisticMetricsEWMA(t *testing.T) {
	const tol = 1e-9
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "S1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		mapping := routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			MetricsLocked: false, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// (a) First positive sample against a stored 0 seeds the value directly.
		at1 := now.Add(time.Hour)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 100, 0, 0.2, at1); err != nil {
			t.Fatalf("update opportunistic metrics (seed): %v", err)
		}
		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 100; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 100 (seed on first positive)", got.GenTokensPerSecond)
		}
		if got.PromptTokensPerSecond != 0 {
			t.Fatalf("PromptTokensPerSecond = %v, want 0 (non-positive sample must not seed it)", got.PromptTokensPerSecond)
		}
		if got.MetricsSource != "opportunistic" {
			t.Fatalf("MetricsSource = %q, want opportunistic", got.MetricsSource)
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(at1) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, at1)
		}

		// (b) A second positive sample blends: 0.2*200 + 0.8*100 = 120. This is
		// the EWMA-parity assertion: MemoryStore's ewma() and the SQL CASE must
		// compute the identical blended value.
		at2 := now.Add(2 * time.Hour)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 200, 0, 0.2, at2); err != nil {
			t.Fatalf("update opportunistic metrics (blend): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 120 (EWMA blend)", got.GenTokensPerSecond)
		}

		// (c) A non-positive gen sample leaves gen unchanged; an independent
		// positive prompt sample seeds prompt in the same call.
		at3 := now.Add(3 * time.Hour)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 0, 300, 0.2, at3); err != nil {
			t.Fatalf("update opportunistic metrics (gen-skip): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 120 (non-positive sample must not change it)", got.GenTokensPerSecond)
		}
		if diff := got.PromptTokensPerSecond - 300; diff < -tol || diff > tol {
			t.Fatalf("PromptTokensPerSecond = %v, want 300 (seed on first positive)", got.PromptTokensPerSecond)
		}

		// (d) Lock the mapping; a subsequent opportunistic write is a benign no-op.
		mapping.GenTokensPerSecond = got.GenTokensPerSecond
		mapping.PromptTokensPerSecond = got.PromptTokensPerSecond
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(4 * time.Hour)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("lock mapping: %v", err)
		}
		at4 := now.Add(5 * time.Hour)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 9999, 9999, 0.2, at4); err != nil {
			t.Fatalf("update opportunistic metrics (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if diff := locked.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("locked GenTokensPerSecond = %v, want 120 (opportunistic write must not overwrite a lock)", locked.GenTokensPerSecond)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want manual (opportunistic write must not overwrite a lock)", locked.MetricsSource)
		}

		// (e) A write against a MISSING mapping id is a benign no-op (no error).
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "does-not-exist", 42, 42, 0.2, at4); err != nil {
			t.Fatalf("update opportunistic metrics (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// --- Sample reduction: availability + telemetry -----------------------------

// TestRoutingStoreAvailabilitySampleReduction exercises
// routing.ReduceAvailabilitySamples (RT-2's extracted shared function) through
// both backends' real InsertServerAvailabilitySample/ServerAvailabilitySamples
// methods: a redundant contiguous heartbeat must drop, a state transition must
// be kept, and a >gap-floor boundary must be kept AND carry GapBefore=true.
func TestRoutingStoreAvailabilitySampleReduction(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "host1", Name: "Host 1", Provider: routing.ProviderMock, Endpoint: "mock://host1",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: base, UpdatedAt: base,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		insert := func(offset time.Duration, health string) {
			if err := s.InsertServerAvailabilitySample(ctx, routing.ServerAvailabilitySample{
				ServerID: "host1", ReportedAt: base.Add(offset), Health: health,
				ReachableCount: 1, ActiveCount: 1, AgentReporting: true,
			}); err != nil {
				t.Fatalf("insert availability sample @%v: %v", offset, err)
			}
		}
		insert(0, routing.HealthHealthy)
		insert(time.Minute, routing.HealthHealthy)      // redundant heartbeat -> dropped
		insert(2*time.Minute, routing.HealthHealthy)    // pre-transition endpoint -> kept
		insert(3*time.Minute, routing.HealthUnhealthy)  // transition -> kept
		insert(20*time.Minute, routing.HealthUnhealthy) // >10m gap boundary -> kept, GapBefore

		got, err := s.ServerAvailabilitySamples(ctx, "host1", base.Add(-time.Minute), base.Add(30*time.Minute), 0)
		if err != nil {
			t.Fatalf("server availability samples: %v", err)
		}
		var offsets []time.Duration
		for _, g := range got {
			offsets = append(offsets, g.ReportedAt.Sub(base))
		}
		want := []time.Duration{0, 2 * time.Minute, 3 * time.Minute, 20 * time.Minute}
		if !reflect.DeepEqual(offsets, want) {
			t.Fatalf("reduced offsets = %v, want %v", offsets, want)
		}
		if got[len(got)-1].GapBefore != true {
			t.Fatalf("expected GapBefore=true on the post-gap (@+20m) sample: %+v", got[len(got)-1])
		}
		for _, g := range got[:len(got)-1] {
			if g.GapBefore {
				t.Fatalf("only the post-gap sample may carry GapBefore=true: %+v", g)
			}
		}
	})
}

// TestRoutingStoreTelemetrySampleDecimation exercises
// routing.DecimateTelemetrySamples through both backends' real
// InsertTelemetrySample/TelemetrySamples methods: with 9 samples and limit=3,
// the even-index mapping i*(n-1)/(limit-1) must keep exactly the 1st, 5th and
// 9th samples on both backends.
func TestRoutingStoreTelemetrySampleDecimation(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "host1", Name: "Host 1", Provider: routing.ProviderMock, Endpoint: "mock://host1",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: base, UpdatedAt: base,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		for i := 0; i < 9; i++ {
			if err := s.InsertTelemetrySample(ctx, routing.TelemetrySample{
				ServerID: "host1", ReportedAt: base.Add(time.Duration(i) * time.Minute), CPUUtilPct: float64(i),
			}); err != nil {
				t.Fatalf("insert telemetry sample %d: %v", i, err)
			}
		}

		got, err := s.TelemetrySamples(ctx, "host1", base.Add(-time.Minute), base.Add(20*time.Minute), 3)
		if err != nil {
			t.Fatalf("telemetry samples: %v", err)
		}
		var offsets []time.Duration
		for _, g := range got {
			offsets = append(offsets, g.ReportedAt.Sub(base))
		}
		want := []time.Duration{0, 4 * time.Minute, 8 * time.Minute}
		if !reflect.DeepEqual(offsets, want) {
			t.Fatalf("decimated offsets = %v, want %v", offsets, want)
		}
	})
}

// --- Agent runtime manager: memory-vs-SQL parity (T1) -----------------------

// TestRoutingStoreRuntimeSpecs proves routing.MemoryStore and the SQL store
// agree on RuntimeStore semantics: absent read, upsert-by-mapping (CreatedAt
// preserved across an overwrite), listing by application, atomic GPU-row
// replace with ordered read, the measured-vs-estimate write isolation, the
// orphan-mapping FK error, and delete + GPU-row cascade.
//
// Unlike route_affinity's api_token_id/user_id (auth concepts routing.Store
// has no method for), agent_runtime_specs.mapping_id references a row
// (model_mappings) that IS fully expressible through routing.Store itself
// (CreateAIServer/CreateApplication/CreateMapping), so both backends seed
// identically through the interface inside run — no forEachRoutingStoreSeeded
// SQL-only seed hook is needed here. That also means MemoryStore's
// hand-rolled FK check (mirroring CreateApplication/CreateMapping) must
// reject an orphan mapping id the same way the SQL FK does, so the orphan
// assertion below runs unmodified on both backends.
func TestRoutingStoreRuntimeSpecs(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_rt2", Name: "RT2", Domain: "rt2.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://rt2", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_rt2", ServerID: "srv_rt2", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_rt2", ApplicationID: "app_rt2", GatewayModelName: "map-rt2-model",
			AppModelName: "map-rt2-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		if _, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt2"); err != nil || ok {
			t.Fatalf("absent spec: ok=%v err=%v", ok, err)
		}

		spec := routing.RuntimeSpec{
			ID: "rspec_rt2", MappingID: "map_rt2", Enabled: true,
			Binary: "/usr/bin/llama-server", Args: `["--port","${PORT}"]`,
			Env: `{"HF_TOKEN":"${AGENT_ENV:HF_TOKEN}"}`, WorkDir: "/srv/models",
			HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			IdleTimeoutSeconds: 900, AdmissionWaitTimeoutSeconds: 30, Pinned: true,
			AdminState: "force_running", VRAMLocked: true, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt2")
		if err != nil || !ok {
			t.Fatalf("read back: ok=%v err=%v", ok, err)
		}
		if got.Binary != spec.Binary || got.Args != spec.Args || got.Env != spec.Env ||
			!got.Enabled || !got.Pinned || !got.VRAMLocked ||
			got.AdminState != "force_running" || !got.CreatedAt.Equal(now) {
			t.Fatalf("round-trip mismatch: %+v", got)
		}

		spec.Binary = "/usr/bin/vllm"
		spec.UpdatedAt = now.Add(time.Minute)
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert overwrite: %v", err)
		}
		got, _, _ = s.RuntimeSpecByMapping(ctx, "map_rt2")
		if got.Binary != "/usr/bin/vllm" || !got.CreatedAt.Equal(now) {
			t.Fatalf("overwrite must keep created_at, got %+v", got)
		}

		specs, err := s.RuntimeSpecsByApplication(ctx, "app_rt2")
		if err != nil || len(specs) != 1 {
			t.Fatalf("by application: %v %d", err, len(specs))
		}

		// A spec that has never had SetRuntimeSpecGPUs called for it must read
		// back a non-nil, empty slice (the documented RuntimeStore contract:
		// "Always non-nil, empty when none" — store.go). A bare nil here would
		// marshal as JSON `null` instead of `[]` in the agent-facing config
		// payload (Task 7), a memory-vs-SQL divergence not caught by a
		// length-only assertion.
		if gpus, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2"); err != nil || gpus == nil || len(gpus) != 0 {
			t.Fatalf("gpus for a spec with no rows yet must be non-nil and empty: err=%v gpus=%#v", err, gpus)
		}

		gpus := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_rt2", GPUIndex: 1, VRAMEstimateMB: 21500},
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 22000},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", gpus); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gotGPUs, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if err != nil || len(gotGPUs) != 2 || gotGPUs[0].GPUIndex != 0 || gotGPUs[1].GPUIndex != 1 {
			t.Fatalf("gpus must read ordered by index: %v %+v", err, gotGPUs)
		}

		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_rt2", 0, 21800); err != nil {
			t.Fatalf("measured: %v", err)
		}
		gotGPUs, _ = s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if gotGPUs[0].VRAMMeasuredMB != 21800 || gotGPUs[0].VRAMEstimateMB != 22000 {
			t.Fatalf("measured must not clobber estimate: %+v", gotGPUs[0])
		}
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_rt2", 7, 1); err != ErrNotFound {
			t.Fatalf("measured on absent gpu row: want ErrNotFound, got %v", err)
		}

		// Explicitly clearing the GPU rows (an empty, non-nil slice in) must
		// still read back non-nil, empty (not a bare nil) — the same contract
		// as the never-set case above, exercised via the other code path
		// (SetRuntimeSpecGPUs's delete-then-insert-nothing) that could
		// plausibly diverge from it.
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", []routing.RuntimeSpecGPU{}); err != nil {
			t.Fatalf("clear gpus: %v", err)
		}
		if gpus, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2"); err != nil || gpus == nil || len(gpus) != 0 {
			t.Fatalf("gpus after clearing via empty slice must be non-nil and empty: err=%v gpus=%#v", err, gpus)
		}

		// Duplicate GPUIndex within one set hits the composite primary key
		// (spec_id, gpu_index) on the SQL side -> ErrConflict; MemoryStore
		// must reject it the same way (an untested memory-vs-SQL divergence
		// otherwise, the same bug class as the nil-vs-empty slice check
		// above). The rejected set must not partially apply either.
		dupGPUs := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 1000},
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 2000},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", dupGPUs); err != ErrConflict {
			t.Fatalf("duplicate gpu_index: want ErrConflict, got %v", err)
		}
		if gpus, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2"); err != nil || len(gpus) != 0 {
			t.Fatalf("failed duplicate-index set must not partially apply: err=%v gpus=%#v", err, gpus)
		}

		orphan := spec
		orphan.ID, orphan.MappingID = "rspec_rt2_orphan", "map_missing"
		if err := s.UpsertRuntimeSpec(ctx, orphan); err != ErrNotFound {
			t.Fatalf("orphan spec: want ErrNotFound, got %v", err)
		}

		if err := s.DeleteRuntimeSpec(ctx, "rspec_rt2"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := s.DeleteRuntimeSpec(ctx, "rspec_rt2"); err != ErrNotFound {
			t.Fatalf("double delete: want ErrNotFound, got %v", err)
		}
		if gotGPUs, err = s.RuntimeSpecGPUs(ctx, "rspec_rt2"); err != nil || len(gotGPUs) != 0 {
			t.Fatalf("gpu rows must cascade: %v %d", err, len(gotGPUs))
		}
	})
}

// --- Agent runtime manager: co-residency matrix memory-vs-SQL parity (Task 2) ---

// TestRoutingStoreCoResidencyRules proves routing.MemoryStore and the SQL
// store agree on the co-residency matrix semantics: empty by default, atomic
// full replace (set + ordered read, then clear via an empty/nil set),
// unknown-application ErrNotFound, and the FK error when a rule names a
// mapping id that does not exist — MemoryStore hand-checks application and
// mapping existence to mirror the SQL FKs, so the same assertions run
// unmodified on both backends (mirrors TestRoutingStoreRuntimeSpecs above).
func TestRoutingStoreCoResidencyRules(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_cr", Name: "CR", Domain: "cr.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://cr", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_cr", ServerID: "srv_cr", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		for _, mid := range []string{"map_cr", "map_cr2"} {
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: mid, ApplicationID: "app_cr", GatewayModelName: mid + "-model",
				AppModelName: mid + "-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %s: %v", mid, err)
			}
		}

		// Empty by default
		rules, err := s.CoResidencyRulesByApplication(ctx, "app_cr")
		if err != nil || rules == nil || len(rules) != 0 {
			t.Fatalf("default must be non-nil empty: err=%v rules=%#v", err, rules)
		}

		// Set + ordered read
		want := []routing.CoResidencyRule{
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr2", CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, "app_cr", want); err != nil {
			t.Fatalf("set: %v", err)
		}
		rules, err = s.CoResidencyRulesByApplication(ctx, "app_cr")
		if err != nil || len(rules) != 1 || rules[0].MappingAID != "map_cr" || rules[0].MappingBID != "map_cr2" {
			t.Fatalf("read back: %v %+v", err, rules)
		}

		// Full replace with empty clears, still non-nil
		if err := s.SetCoResidencyRules(ctx, "app_cr", nil); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if rules, err = s.CoResidencyRulesByApplication(ctx, "app_cr"); err != nil || rules == nil || len(rules) != 0 {
			t.Fatalf("clear must leave non-nil empty: err=%v rules=%#v", err, rules)
		}

		// Unknown application -> ErrNotFound
		if err := s.SetCoResidencyRules(ctx, "app_missing", nil); err != ErrNotFound {
			t.Fatalf("unknown app: want ErrNotFound, got %v", err)
		}

		// FK: rule naming a missing mapping -> ErrNotFound
		bad := []routing.CoResidencyRule{{ApplicationID: "app_cr", MappingAID: "map_missing", MappingBID: "map_cr", CreatedAt: now}}
		if err := s.SetCoResidencyRules(ctx, "app_cr", bad); err != ErrNotFound {
			t.Fatalf("missing mapping: want ErrNotFound, got %v", err)
		}
	})
}

// --- Agent runtime manager: per-GPU VRAM budgets memory-vs-SQL parity (Task 3) ---

// TestRoutingStoreServerGPUBudgets proves routing.MemoryStore and the SQL
// store agree on the per-GPU VRAM budget semantics: empty-and-non-nil by
// default, atomic full replace (two out-of-order rows, ordered read, then a
// replace with a different set), clear via a nil set, and unknown-server
// ErrNotFound (MemoryStore hand-checks server existence to mirror the SQL FK
// on ai_servers, mirroring TestRoutingStoreCoResidencyRules above).
func TestRoutingStoreServerGPUBudgets(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_gb", Name: "GB", Domain: "gb.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://gb", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// Empty by default, and non-nil.
		budgets, err := s.ServerGPUBudgets(ctx, "srv_gb")
		if err != nil || budgets == nil || len(budgets) != 0 {
			t.Fatalf("default must be non-nil empty: err=%v budgets=%#v", err, budgets)
		}

		// Set two budgets, indexes 1 then 0 (deliberately out of order) ->
		// ordered read by GPUIndex.
		want := []routing.ServerGPUBudget{
			{ServerID: "srv_gb", GPUIndex: 1, BudgetMB: 12000, ExpectedUUID: "GPU-1", ExpectedName: "RTX 4090", CreatedAt: now, UpdatedAt: now},
			{ServerID: "srv_gb", GPUIndex: 0, BudgetMB: 24000, ExpectedUUID: "GPU-0", ExpectedName: "RTX 4090", CreatedAt: now, UpdatedAt: now},
		}
		if err := s.SetServerGPUBudgets(ctx, "srv_gb", want); err != nil {
			t.Fatalf("set: %v", err)
		}
		budgets, err = s.ServerGPUBudgets(ctx, "srv_gb")
		if err != nil || len(budgets) != 2 {
			t.Fatalf("read back: %v %+v", err, budgets)
		}
		if budgets[0].GPUIndex != 0 || budgets[0].BudgetMB != 24000 {
			t.Fatalf("position 0 must be gpu_index 0: %+v", budgets[0])
		}
		if budgets[1].GPUIndex != 1 || budgets[1].BudgetMB != 12000 {
			t.Fatalf("position 1 must be gpu_index 1: %+v", budgets[1])
		}

		// Full replace with a different set overwrites the previous one.
		replacement := []routing.ServerGPUBudget{
			{ServerID: "srv_gb", GPUIndex: 0, BudgetMB: 8000, ExpectedUUID: "GPU-0b", ExpectedName: "A100", CreatedAt: now, UpdatedAt: now},
		}
		if err := s.SetServerGPUBudgets(ctx, "srv_gb", replacement); err != nil {
			t.Fatalf("replace: %v", err)
		}
		budgets, err = s.ServerGPUBudgets(ctx, "srv_gb")
		if err != nil || len(budgets) != 1 || budgets[0].BudgetMB != 8000 {
			t.Fatalf("replace must overwrite the previous set: %v %+v", err, budgets)
		}

		// Full replace with a nil set clears, still non-nil on read.
		if err := s.SetServerGPUBudgets(ctx, "srv_gb", nil); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if budgets, err = s.ServerGPUBudgets(ctx, "srv_gb"); err != nil || budgets == nil || len(budgets) != 0 {
			t.Fatalf("clear must leave non-nil empty: err=%v budgets=%#v", err, budgets)
		}

		// Duplicate GPUIndex within one set hits the composite primary key
		// (server_id, gpu_index) on the SQL side -> ErrConflict; MemoryStore
		// must reject it the same way (an untested memory-vs-SQL divergence
		// otherwise). The rejected set must not partially apply either.
		dup := []routing.ServerGPUBudget{
			{ServerID: "srv_gb", GPUIndex: 0, BudgetMB: 1000, CreatedAt: now, UpdatedAt: now},
			{ServerID: "srv_gb", GPUIndex: 0, BudgetMB: 2000, CreatedAt: now, UpdatedAt: now},
		}
		if err := s.SetServerGPUBudgets(ctx, "srv_gb", dup); err != ErrConflict {
			t.Fatalf("duplicate gpu_index: want ErrConflict, got %v", err)
		}
		if budgets, err := s.ServerGPUBudgets(ctx, "srv_gb"); err != nil || len(budgets) != 0 {
			t.Fatalf("failed duplicate-index set must not partially apply: %v %+v", err, budgets)
		}

		// Unknown server -> ErrNotFound.
		if err := s.SetServerGPUBudgets(ctx, "srv_missing", nil); err != ErrNotFound {
			t.Fatalf("unknown server: want ErrNotFound, got %v", err)
		}
	})
}
