// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"os"
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
	// The postgres leg, added with the capability table (#49-2 follow-up).
	// Until then this harness ran memory + sqlite only, so every routing-store
	// conformance assertion in this file -- including the SQL that dialects
	// genuinely disagree about, `on conflict (...) do update`, `where ... in
	// (...)` with rebound placeholders, and narrow-vs-wide column types
	// (ADR-005) -- was unverified on the driver operators actually deploy.
	// forEachDialect (conformance_test.go) already had this leg; this one is
	// the same block, and adding it turned out to cost about five seconds and
	// to pass unchanged, which is the argument for having it.
	//
	// It SKIPS silently without the DSN, exactly like its sibling: that is the
	// documented convention (persistence.md), and AGENTS.md is the standing
	// instruction to set it -- "verify store changes with
	// OP_AI_GATEWAY_TEST_POSTGRES_DSN set". CI sets it.
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OP_AI_GATEWAY_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run postgres conformance tests")
		}
		ctx := context.Background()
		pgStore, err := OpenPostgres(ctx, dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		defer pgStore.Close()
		// Clean slate so the suite is deterministic against a reused database
		// -- the same guard forEachDialect's postgres leg applies.
		if err := dropAllTables(ctx, pgStore); err != nil {
			t.Fatalf("drop tables: %v", err)
		}
		if err := pgStore.Migrate(ctx); err != nil {
			t.Fatalf("migrate postgres: %v", err)
		}
		if seedSQL != nil {
			seedSQL(t, pgStore)
		}
		run(t, pgStore)
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

// TestRoutingStoreActiveMappingsForModelReadsCapabilityVerdicts (Task 4)
// proves ActiveMappingsForModel's filtered join (SQL) and MemoryStore's
// mirror apply the IDENTICAL three-state-to-string boundary conversion on
// every backend: LiveProgressSupport is "supported"/"unsupported"/"" for a
// "live_progress" yes/no/absent row. Because forEachRoutingStore runs the
// SAME assertions against MemoryStore, sqlite and postgres, a divergence in
// either driver's join/mirror or in the shared
// LiveProgressSupportFromVerdict conversion surfaces here, not in
// production.
//
// It pins a SECOND property that is easy to lose by accident: that the SQL
// join is FILTERED, not merely present. The map_yes fixture deliberately
// holds TWO capability rows so the cardinality assertion below
// (len(got) != 1) fails the moment `lp.capability = ?` is dropped -- see
// that fixture's own note for why the row must stay.
func TestRoutingStoreActiveMappingsForModelReadsCapabilityVerdicts(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

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
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}

		newMapping := func(id string) routing.ModelMapping {
			return routing.ModelMapping{
				ID: id, ApplicationID: "app1", GatewayModelName: "model-" + id, AppModelName: "up-" + id,
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}
		}
		for _, id := range []string{"map_yes", "map_no", "map_absent"} {
			if err := s.CreateMapping(ctx, newMapping(id)); err != nil {
				t.Fatalf("create mapping %s: %v", id, err)
			}
		}

		// The "vision" row is LOAD-BEARING, and not for anything this test
		// asserts about it -- nothing here reads a vision verdict at all. It
		// is what makes the len(got) != 1 assertion below able to FAIL.
		// ActiveMappingsForModel's LEFT JOIN is filtered (`lp.capability =
		// ?`, sqlite_applications.go), so a mapping holding two capability
		// rows still yields exactly ONE candidate; remove the predicate and
		// the same mapping yields one candidate PER ROW. With at most one row
		// per mapping in the fixture, an unfiltered join is indistinguishable
		// from a filtered one and the assertion cannot bite -- so a fixture
		// narrowing that drops this row silently retires the only check in
		// the repository on the predicate that
		// sqlite_applications.go's cost comment and ADR-040's roughly
		// 6-microsecond saving both rest on. Verified by mutation, in both
		// directions:
		// with the predicate removed this test fails "got 2 candidates,
		// want 1", and with it in place it passes. DO NOT remove this row.
		if err := s.UpsertMappingCapabilities(ctx, "map_yes", []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
			{Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}); err != nil {
			t.Fatalf("upsert map_yes capabilities: %v", err)
		}
		if err := s.UpsertMappingCapabilities(ctx, "map_no", []routing.CapabilityRow{
			{Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}); err != nil {
			t.Fatalf("upsert map_no capabilities: %v", err)
		}
		// map_absent gets no capability rows at all -- "never determined".

		for _, tc := range []struct {
			model            string
			wantLiveProgress string
		}{
			{"model-map_yes", "supported"},
			{"model-map_no", "unsupported"},
			{"model-map_absent", ""},
		} {
			got, err := s.ActiveMappingsForModel(ctx, tc.model, routing.APIFlavorOpenAI)
			if err != nil {
				t.Fatalf("active mappings for %s: %v", tc.model, err)
			}
			if len(got) != 1 {
				t.Fatalf("active mappings for %s: got %d candidates, want 1: %+v", tc.model, len(got), got)
			}
			if got[0].LiveProgressSupport != tc.wantLiveProgress {
				t.Fatalf("model %s: LiveProgressSupport = %q, want %q", tc.model, got[0].LiveProgressSupport, tc.wantLiveProgress)
			}
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

// --- Capability rows and metrics_locked (#49-3) -----------------------------

// TestUpsertMappingCapabilitiesIgnoresMetricsLock is the load-bearing test for
// the deliberate deviation in UpsertMappingCapabilities: UNLIKE every targeted
// mapping METRIC writer (UpdateMappingContextProbe,
// UpdateMappingBenchmarkMetrics, UpdateMappingOpportunisticMetrics,
// UpdateMappingCapacityMetrics, UpdateMappingEnergyEWMA -- five for five),
// this one must NOT be blocked by MetricsLocked, and must NOT restamp
// MetricsSource / MetricsUpdatedAt. It writes a capability row on a mapping
// that is LOCKED, with a known MetricsSource, a known MetricsUpdatedAt and a
// known GenTokensPerSecond, and asserts the row lands while all four metrics
// fields stay exactly as seeded. If a future change "fixes" this writer to
// look like its metric siblings (adds a MetricsLocked guard, or starts
// stamping the metrics provenance columns), this test must fail on both
// backends.
//
// It is this file's converted form of the same test that pinned the property
// for the pre-78 live_progress_support column: the argument
// (routing.MappingStore.UpsertMappingCapabilities) survived the columns'
// writers, so the test that pins it moved onto the table rather than being
// deleted with them.
func TestUpsertMappingCapabilitiesIgnoresMetricsLock(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

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
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// Lock the mapping with a known throughput + provenance, exactly as an
		// operator pinning numbers they answer for would (via UpdateMapping).
		metricsAt := now.Add(time.Hour)
		const seededThroughput = 42.5
		mapping.GenTokensPerSecond = seededThroughput
		mapping.MetricsLocked = true
		mapping.MetricsSource = "benchmark"
		mapping.MetricsUpdatedAt = &metricsAt
		mapping.UpdatedAt = now.Add(2 * time.Hour)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("lock mapping: %v", err)
		}

		// Write a capability row on the LOCKED mapping.
		verdictAt := now.Add(3 * time.Hour)
		if err := s.UpsertMappingCapabilities(ctx, "m1", []routing.CapabilityRow{{
			Capability: routing.CapabilityLiveProgress, Verdict: routing.CapabilityYes,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: verdictAt,
		}}); err != nil {
			t.Fatalf("upsert mapping capabilities: %v", err)
		}

		// The verdict landed -- the lock did NOT block it.
		caps, err := s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping capabilities: %v", err)
		}
		if len(caps) != 1 {
			t.Fatalf("capability rows = %+v, want exactly 1 (MetricsLocked must not block this writer)", caps)
		}
		if caps[0].Capability != routing.CapabilityLiveProgress || caps[0].Verdict != routing.CapabilityYes {
			t.Fatalf("capability row = %+v, want live_progress/yes", caps[0])
		}
		if caps[0].Source != routing.CapabilitySourceLlamaCppProps {
			t.Fatalf("capability row Source = %q, want %q", caps[0].Source, routing.CapabilitySourceLlamaCppProps)
		}
		if !caps[0].CheckedAt.Equal(verdictAt) {
			t.Fatalf("capability row CheckedAt = %v, want %v", caps[0].CheckedAt, verdictAt)
		}

		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}

		// All four metrics fields are untouched.
		if got.MetricsSource != "benchmark" {
			t.Fatalf("MetricsSource = %q, want %q (must be untouched by a capability write)", got.MetricsSource, "benchmark")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(metricsAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v (must be untouched by a capability write)", got.MetricsUpdatedAt, metricsAt)
		}
		if got.GenTokensPerSecond != seededThroughput {
			t.Fatalf("GenTokensPerSecond = %v, want %v (must be untouched by a capability write)", got.GenTokensPerSecond, seededThroughput)
		}
		if !got.MetricsLocked {
			t.Fatalf("MetricsLocked = %v, want true (must be untouched by a capability write)", got.MetricsLocked)
		}
	})
}

// TestUpsertMappingCapabilitiesUnknownMappingFails pins the ONE way
// UpsertMappingCapabilities is NOT "the benign no-op every other targeted
// mapping writer promises for a missing id" (see case (e) in
// TestUpdateMappingMetrics* above, and DeleteMappingCapability's own doc):
// model_mapping_capabilities.mapping_id carries a real FK
// (`references model_mappings(id) on delete cascade`, migration 78), so
// writing against a mapping id that does not exist fails on every driver
// instead of silently creating an orphan row.
//
// This is a fix, not a pre-existing property: MemoryStore originally had no
// existence check here at all (unlike its sibling mapping-child writers
// UpsertRuntimeSpec/InsertBenchmarkRun) and would fabricate a capabilities
// entry for a mapping that was never created -- invisible on the SQL drivers,
// which have always rejected it via the real FK, so the divergence would
// only ever have surfaced through a memory-mode-only bug report. This test
// is what makes it visible instead: both backends must not only ERROR, they
// must leave NO row behind.
func TestUpsertMappingCapabilitiesUnknownMappingFails(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

		err := s.UpsertMappingCapabilities(ctx, "does-not-exist", []routing.CapabilityRow{{
			Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
		}})
		if err == nil {
			t.Fatalf("UpsertMappingCapabilities(missing mapping) = nil error, want a failure (FK violation on SQL, ErrNotFound on memory)")
		}

		caps, err := s.MappingCapabilities(ctx, "does-not-exist")
		if err != nil {
			t.Fatalf("MappingCapabilities: %v", err)
		}
		if len(caps) != 0 {
			t.Fatalf("capability rows = %+v, want none -- the rejected write must not leave an orphan row behind", caps)
		}
	})
}

// --- Capability rows (model_mapping_capabilities) ---------------------------

// TestMappingCapabilityRows proves the per-capability row API on both
// drivers: rows round-trip with their provenance, a second write for the same
// (mapping, capability) REPLACES rather than duplicating, a delete returns a
// capability to unknown, and "unknown" is expressed by the ABSENCE of a row
// rather than by an empty verdict -- including in the bulk reader, which must
// not invent a key for a mapping that has no rows.
func TestMappingCapabilityRows(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

		// server + app + mapping "m1" exactly as
		// TestUpsertMappingCapabilitiesIgnoresMetricsLock builds them.
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
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		rows := []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
			{Capability: routing.CapabilityTools, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}
		if err := s.UpsertMappingCapabilities(ctx, "m1", rows); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2: %+v", len(got), got)
		}
		byName := map[string]routing.CapabilityRow{}
		for _, r := range got {
			byName[r.Capability] = r
		}
		if byName["vision"].Verdict != "yes" || byName["vision"].Source != "llama_cpp_props" {
			t.Fatalf("vision row = %+v", byName["vision"])
		}
		if byName["vision"].CheckedAt.IsZero() {
			t.Fatal("checked_at not stored")
		}
		if _, present := byName["audio"]; present {
			t.Fatal("an unwritten capability must be ABSENT, not a row -- absence is how unknown is expressed")
		}

		// Upsert replaces in place: same capability, new verdict, no duplicate row.
		later := now.Add(time.Hour)
		if err := s.UpsertMappingCapabilities(ctx, "m1", []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: later},
		}); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		got, err = s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("read after re-upsert: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("upsert duplicated a row: %d rows %+v", len(got), got)
		}
		for _, r := range got {
			if r.Capability == "vision" && (r.Verdict != "no" || r.Source != "vision_benchmark") {
				t.Fatalf("upsert did not replace: %+v", r)
			}
		}

		// Delete returns a capability to unknown -- the state the old bool
		// could not express.
		if err := s.DeleteMappingCapability(ctx, "m1", routing.CapabilityVision); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got, err = s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("read after delete: %v", err)
		}
		for _, r := range got {
			if r.Capability == "vision" {
				t.Fatal("delete left the row behind")
			}
		}
		// Deleting an absent row is a benign no-op, not an error.
		if err := s.DeleteMappingCapability(ctx, "m1", routing.CapabilityVision); err != nil {
			t.Fatalf("delete of an absent row must be a no-op: %v", err)
		}
		// Every OTHER failing shape is a benign no-op too, on every driver: an
		// unknown mapping id, a capability name no code knows, and an empty
		// name. This is not a curiosity -- it is the reason the portal's reset
		// path (an empty verdict in portal.Service.UpdateMapping's
		// CapabilityVerdicts) has to carry BOTH its own authorisation
		// (authorizeMapping) and its own empty-name rejection. The store hands
		// that path NO existence signal to lean on: "refused", "the mapping does
		// not exist" and "deleted nothing" are indistinguishable here by design,
		// unlike UpsertMappingCapabilities, whose FK makes an unknown mapping an
		// error (see TestUpsertMappingCapabilitiesUnknownMappingFails).
		for _, absent := range []struct{ mappingID, capability string }{
			{"does-not-exist", routing.CapabilityVision},
			{"m1", "a_capability_no_code_knows"},
			{"m1", ""},
		} {
			if err := s.DeleteMappingCapability(ctx, absent.mappingID, absent.capability); err != nil {
				t.Fatalf("delete(%q, %q) must be a benign no-op, got: %v", absent.mappingID, absent.capability, err)
			}
		}
		// ...and none of those touched the row that IS there.
		if got, err = s.MappingCapabilities(ctx, "m1"); err != nil {
			t.Fatalf("read after the no-op deletes: %v", err)
		}
		if len(got) != 1 || got[0].Capability != routing.CapabilityTools {
			t.Fatalf("rows after the no-op deletes = %+v, want only the untouched %q row", got, routing.CapabilityTools)
		}

		// The batch reader returns exactly the requested parents, and an
		// unknown id contributes no key at all (not an empty slice).
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "m2", ApplicationID: "app1", GatewayModelName: "gpt-4o",
			AppModelName: "up2", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping m2: %v", err)
		}
		if err := s.UpsertMappingCapabilities(ctx, "m2", []routing.CapabilityRow{
			{Capability: routing.CapabilityMTP, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLegacy, CheckedAt: now},
		}); err != nil {
			t.Fatalf("upsert m2: %v", err)
		}
		// m1 must carry at least TWO rows here, not one: a bulk reader whose
		// per-parent accumulation is unpinned (e.g. `out[id] = []Row{r}`
		// instead of `out[id] = append(out[id], r)`) keeps only the last row
		// scanned for a mapping and would still pass a single-row mapping. m1
		// currently has only "tools" left (vision was deleted above), so add a
		// second capability back before reading it through the batch API.
		if err := s.UpsertMappingCapabilities(ctx, "m1", []routing.CapabilityRow{
			{Capability: routing.CapabilityAudio, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}); err != nil {
			t.Fatalf("upsert second capability on m1: %v", err)
		}
		batch, err := s.MappingCapabilitiesForMappings(ctx, []string{"m1", "m2", "nope"})
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if len(batch["m2"]) != 1 || batch["m2"][0].Capability != "mtp" {
			t.Fatalf("batch m2 = %+v", batch["m2"])
		}
		if _, present := batch["nope"]; present {
			t.Fatal("batch reader invented a key for an unknown mapping")
		}
		single, err := s.MappingCapabilities(ctx, "m1")
		if err != nil {
			t.Fatalf("single read for the batch comparison: %v", err)
		}
		if len(single) < 2 {
			t.Fatalf("fixture bug: m1 must carry >=2 rows to exercise the batch reader's accumulation, got %+v", single)
		}
		// Compare element-wise, not just by length: both readers select
		// `order by capability`, so a lockstep comparison is deterministic and
		// catches a reader that drops or overwrites a row a plain length check
		// would miss (e.g. keeping only the mapping's LAST row).
		if len(batch["m1"]) != len(single) {
			t.Fatalf("batch and single reader disagree on count: batch=%+v single=%+v", batch["m1"], single)
		}
		for i := range single {
			if batch["m1"][i] != single[i] {
				t.Fatalf("batch and single reader disagree at row %d: batch=%+v single=%+v", i, batch["m1"], single)
			}
		}
		// An empty id list does no lookup and yields an empty map.
		if empty, err := s.MappingCapabilitiesForMappings(ctx, nil); err != nil || len(empty) != 0 {
			t.Fatalf("empty batch: err=%v map=%+v", err, empty)
		}
	})
}

// TestUpsertMappingCapabilitiesRejectsInvalidRows proves both drivers reject
// (via routing.ValidateCapabilityRow), rather than silently write, a row an
// operator's documented invariant says cannot exist: an empty Capability or
// Source, or a Verdict that is neither "yes" nor "no". A caller passing such
// a row has a bug, and every caller of UpsertMappingCapabilities is
// best-effort (it logs and carries on), so an error here costs nothing.
func TestUpsertMappingCapabilitiesRejectsInvalidRows(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_capval", Name: "S1", Domain: "srv-capval.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv-capval.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_capval", ServerID: "srv_capval", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_capval", ApplicationID: "app_capval", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		cases := []struct {
			name string
			row  routing.CapabilityRow
		}{
			{"empty verdict", routing.CapabilityRow{Capability: routing.CapabilityVision, Verdict: "", Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now}},
			{"verdict neither yes nor no", routing.CapabilityRow{Capability: routing.CapabilityVision, Verdict: "maybe", Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now}},
			{"empty capability", routing.CapabilityRow{Capability: "", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now}},
			{"empty source", routing.CapabilityRow{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: "", CheckedAt: now}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := s.UpsertMappingCapabilities(ctx, "map_capval", []routing.CapabilityRow{tc.row}); err == nil {
					t.Fatalf("UpsertMappingCapabilities(%+v) = nil error, want a rejection", tc.row)
				}
				got, err := s.MappingCapabilities(ctx, "map_capval")
				if err != nil {
					t.Fatalf("read after rejected upsert: %v", err)
				}
				if len(got) != 0 {
					t.Fatalf("a rejected row must not be written: %+v", got)
				}
			})
		}

		// An invalid row anywhere in a batch rejects the WHOLE batch -- no
		// partial write of the valid rows alongside it.
		if err := s.UpsertMappingCapabilities(ctx, "map_capval", []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
			{Capability: routing.CapabilityTools, Verdict: "bogus", Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}); err == nil {
			t.Fatal("a batch with one invalid row must be rejected entirely")
		}
		if got, err := s.MappingCapabilities(ctx, "map_capval"); err != nil || len(got) != 0 {
			t.Fatalf("a rejected batch must leave no rows behind: err=%v got=%+v", err, got)
		}

		// A fully valid row still works after the rejections above.
		if err := s.UpsertMappingCapabilities(ctx, "map_capval", []routing.CapabilityRow{
			{Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now},
		}); err != nil {
			t.Fatalf("a valid row must still be accepted: %v", err)
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

		// The empty case of the list read, asserted for NILNESS and not only
		// for length -- the assertion RuntimeSpecGPUs, CoResidencyRules and
		// GPUBudgets each already carry, and the one this method was missing.
		// The RuntimeStore contract says "Always non-nil, empty when none"
		// (store.go), and both backends reach it differently: the SQL store
		// via `make(..., 0)` before the row loop, MemoryStore via `make` plus
		// a filtering loop. Either is one edit from a bare `var out []T`,
		// which still passes every length-only assertion while marshalling as
		// JSON `null` instead of `[]` in the agent-facing config payload.
		// Both an application with no specs yet and an application id that
		// does not exist at all must answer the same way.
		if specs, err := s.RuntimeSpecsByApplication(ctx, "app_rt2"); err != nil || specs == nil || len(specs) != 0 {
			t.Fatalf("specs for an application with no specs yet must be non-nil and empty: err=%v specs=%#v", err, specs)
		}
		if specs, err := s.RuntimeSpecsByApplication(ctx, "app_rt2_missing"); err != nil || specs == nil || len(specs) != 0 {
			t.Fatalf("specs for an unknown application must be non-nil and empty: err=%v specs=%#v", err, specs)
		}

		spec := routing.RuntimeSpec{
			ID: "rspec_rt2", MappingID: "map_rt2", Enabled: true,
			Binary: "/usr/bin/llama-server", Args: `["--port","${PORT}"]`,
			Env: `{"HF_TOKEN":"${AGENT_ENV:HF_TOKEN}"}`, WorkDir: "/srv/models",
			HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			IdleTimeoutSeconds: 900, AdmissionWaitTimeoutSeconds: 30, Pinned: true,
			AdminState: "force_running", VRAMLocked: true, SetVisibleDevices: true,
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModeTranslate, MessagesMode: routing.EndpointModeDisabled,
			Type:                        string(routing.RuntimeSpecTypeOllama),
			ResponsesLiveTimingsEnabled: true,
			CreatedAt:                   now,
			UpdatedAt:                   now,
		}
		// The one spec in this package that seeds
		// responses_live_timings_enabled = true carries an INCAPABLE effective
		// kind, for the same reason the applications parity fixture does
		// (application_column_parity_test.go): the store is policy-free about
		// this flag -- issue #81 decision (e) puts the "an incapable kind may
		// not store true" refusal in the portal, as a 400 naming the type,
		// never in SQL -- so a store path that "helpfully" cleared it by kind
		// has to fail somewhere, and agent_runtime_specs has no parity fixture
		// of its own to fail in.
		//
		// It did not fail anywhere before this row changed: every spec seeding
		// a true in this package resolved to llama_cpp, so a by-kind clear
		// inside UpsertRuntimeSpec (sqlite_runtime.go) passed the whole suite.
		// The applications-side fixture cannot stand in for this one -- it
		// writes through CreateApplication (sqlite_applications.go), a
		// different statement in a different file, and neither path can see the
		// other.
		//
		// Reaching "incapable" is a deliberate choice rather than an omission,
		// because EffectiveRuntimeSpecType never answers "": with Type left
		// empty, this spec's /usr/bin/llama-server binary DETECTS as llama_cpp,
		// which is capable. Hence the explicit ollama Type -- which also
		// outranks detection, so it survives the binary overwrite further down
		// and every copy derived from this literal keeps the same effective
		// kind. (An explicit Type that disagrees with the binary is exactly
		// what the field is for, and the store must not second-guess it;
		// TestConformanceRuntimeSpecs pairs Type "vllm" with the same
		// llama-server binary for the same reason.)
		if kind := routing.EffectiveRuntimeSpecType(spec); !spec.ResponsesLiveTimingsEnabled || routing.LiveTimingsCapableKind(string(kind)) {
			t.Fatalf("this fixture must seed responses_live_timings_enabled=true on a spec whose EFFECTIVE kind is not live-timings-capable (seeded=%v, effective kind %q): otherwise a store path that clears the flag by kind passes this test unchanged",
				spec.ResponsesLiveTimingsEnabled, kind)
		}
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt2")
		if err != nil || !ok {
			t.Fatalf("read back: ok=%v err=%v", ok, err)
		}
		if got.Binary != spec.Binary || got.Args != spec.Args || got.Env != spec.Env ||
			!got.Enabled || !got.Pinned || !got.VRAMLocked || !got.SetVisibleDevices ||
			got.AdminState != "force_running" || !got.CreatedAt.Equal(now) {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
		if got.ResponsesMode != routing.EndpointModeTranslate || got.MessagesMode != routing.EndpointModeDisabled ||
			!got.ResponsesLiveTimingsEnabled ||
			!reflect.DeepEqual(got.APIFlavors, []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}) {
			t.Fatalf("flavor/mode/live-timings round-trip mismatch: %+v", got)
		}

		// RuntimeSpecByID: the same row, keyed by its own primary key rather
		// than its owning mapping -- proves MemoryStore and the SQL store
		// agree on both the found and absent cases (the telemetry VRAM
		// write-back path's point read).
		gotByID, ok, err := s.RuntimeSpecByID(ctx, "rspec_rt2")
		if err != nil || !ok {
			t.Fatalf("RuntimeSpecByID: ok=%v err=%v", ok, err)
		}
		if gotByID.Binary != spec.Binary || gotByID.MappingID != "map_rt2" {
			t.Fatalf("RuntimeSpecByID mismatch: %+v", gotByID)
		}
		if _, ok, err := s.RuntimeSpecByID(ctx, "rspec_rt2_missing"); err != nil || ok {
			t.Fatalf("RuntimeSpecByID absent: ok=%v err=%v", ok, err)
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

		// The same spec saved again with responses_live_timings_enabled
		// CLEARED. Two independent hazards, one assertion.
		//
		// (1) The SQL upsert's `on conflict (mapping_id) do update set` list is
		// hand-maintained SEPARATELY from its insert column list. A column
		// named in the insert but missing from the do-update list is written
		// on a spec's FIRST save and then silently ignored by every later one
		// -- and every other round-trip assertion in this test writes the
		// same value twice, so none of them can see that.
		//
		// (2) agent_runtime_specs has NO applicationParityBools-style parity
		// fixture (application_column_parity_test.go); this round trip is the
		// whole guard for its five integer-boolean columns. Both column-order
		// consts have fixed arity, so an OMITTED column errors at Scan, but
		// two same-typed columns SWAPPED read each other's values in silence
		// -- and the fixture above seeds enabled, pinned, vram_locked,
		// set_visible_devices AND responses_live_timings_enabled all true, so
		// while that holds a swap among them is invisible. One row in which
		// this column differs from all four of its same-typed neighbours is
		// what makes such a reorder observable.
		flipped := spec
		flipped.ResponsesLiveTimingsEnabled = false
		flipped.UpdatedAt = now.Add(2 * time.Minute)
		if err := s.UpsertRuntimeSpec(ctx, flipped); err != nil {
			t.Fatalf("upsert live-timings off: %v", err)
		}
		got, _, _ = s.RuntimeSpecByMapping(ctx, "map_rt2")
		if got.ResponsesLiveTimingsEnabled {
			t.Fatalf("re-saving a spec with responses_live_timings_enabled cleared read back true: the upsert's do-update list writes the column on the first insert only: %+v", got)
		}
		if !got.Enabled || !got.Pinned || !got.VRAMLocked || !got.SetVisibleDevices {
			t.Fatalf("clearing responses_live_timings_enabled disturbed another integer boolean, so two same-typed columns are transposed in a column list: %+v", got)
		}
		// Back on, so the list read below still describes a true row.
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert live-timings back on: %v", err)
		}

		// A spec id reused for a DIFFERENT mapping is the primary-key
		// collision the SQL insert raises: the upsert's conflict target is
		// mapping_id ALONE, so a colliding id is never absorbed by the
		// do-update branch. MemoryStore's hand-rolled guard must classify it
		// identically -- parity that had been claimed in a report but never
		// machine-checked. map_rt2_b deliberately has NO spec of its own yet,
		// so the id primary key is the only constraint the insert can
		// violate; targeting a mapping that already had a spec would make
		// WHICH constraint fires first ambiguous, and that ambiguity is not
		// what this asserts.
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_rt2_b", ApplicationID: "app_rt2", GatewayModelName: "map-rt2b-model",
			AppModelName: "map-rt2b-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create second mapping: %v", err)
		}
		reusedID := spec
		reusedID.MappingID = "map_rt2_b"
		if err := s.UpsertRuntimeSpec(ctx, reusedID); err != ErrConflict {
			t.Fatalf("spec id reused for another mapping: want ErrConflict, got %v", err)
		}
		if _, ok, err := s.RuntimeSpecByMapping(ctx, "map_rt2_b"); err != nil || ok {
			t.Fatalf("rejected id-reuse upsert must not have written anything: ok=%v err=%v", ok, err)
		}

		// RuntimeSpecsByApplication is documented as ordered by spec id
		// (`order by s.id` on the SQL side, an explicit sort in MemoryStore).
		// Asserting that needs at least TWO rows written in the WRONG order:
		// against a single row a broken ORDER BY or sort comparator still
		// passes. "rspec_aaa" sorts BEFORE the already-written "rspec_rt2"
		// and is written second.
		second := spec
		second.ID, second.MappingID = "rspec_aaa", "map_rt2_b"
		// ... and it deliberately disagrees with rspec_rt2 on
		// responses_live_timings_enabled while agreeing on every OTHER
		// integer boolean, for the list-read assertion below.
		second.ResponsesLiveTimingsEnabled = false
		if err := s.UpsertRuntimeSpec(ctx, second); err != nil {
			t.Fatalf("upsert second spec: %v", err)
		}
		specs, err := s.RuntimeSpecsByApplication(ctx, "app_rt2")
		if err != nil || len(specs) != 2 {
			t.Fatalf("by application: err=%v n=%d, want 2", err, len(specs))
		}
		if specs[0].ID != "rspec_aaa" || specs[1].ID != "rspec_rt2" {
			t.Fatalf("specs must read ordered by id, got [%s %s]", specs[0].ID, specs[1].ID)
		}
		// runtimeSpecCols and runtimeSpecColsPrefixed (sqlite_runtime.go) are
		// two independent, hand-maintained copies of ONE column order, and the
		// `s.`-qualified one serves this list read ALONE -- so both a column
		// MISSING from it and a column at a DIFFERENT OFFSET in it are
		// invisible to the RuntimeSpecByMapping / RuntimeSpecByID assertions
		// above. An omission errors at Scan on the destination count; a SWAP
		// of two same-typed columns does not, which is why rspec_aaa was
		// written with this one integer boolean cleared and the other four
		// set. Reading identical values back for both rows would mean the
		// column is not being read at all.
		if specs[0].ResponsesLiveTimingsEnabled || !specs[1].ResponsesLiveTimingsEnabled {
			t.Fatalf("the s.-qualified column list (runtimeSpecColsPrefixed) disagrees with runtimeSpecCols: rspec_aaa stored responses_live_timings_enabled=false and rspec_rt2 stored true, the list read returned %v/%v",
				specs[0].ResponsesLiveTimingsEnabled, specs[1].ResponsesLiveTimingsEnabled)
		}
		if !specs[0].SetVisibleDevices || !specs[1].SetVisibleDevices {
			t.Fatalf("the list read lost set_visible_devices, which both rows stored as true: it is transposed with responses_live_timings_enabled in runtimeSpecColsPrefixed: %+v", specs)
		}
		// Leave only rspec_rt2 behind, so the GPU-row and delete assertions
		// below still describe a single-spec application.
		if err := s.DeleteRuntimeSpec(ctx, "rspec_aaa"); err != nil {
			t.Fatalf("delete second spec: %v", err)
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

		// GPU rows read back in POSITION order, which is independent of
		// gpu_index. Written with the higher gpu_index (1) at position 0, so a
		// store that still orders by gpu_index reads them in the wrong order.
		gpus := []routing.RuntimeSpecGPU{
			{SpecID: "rspec_rt2", GPUIndex: 1, VRAMEstimateMB: 21500, Position: 0},
			{SpecID: "rspec_rt2", GPUIndex: 0, VRAMEstimateMB: 22000, Position: 1},
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_rt2", gpus); err != nil {
			t.Fatalf("set gpus: %v", err)
		}
		gotGPUs, err := s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if err != nil || len(gotGPUs) != 2 {
			t.Fatalf("set/read gpus: %v %+v", err, gotGPUs)
		}
		if gotGPUs[0].GPUIndex != 1 || gotGPUs[0].Position != 0 ||
			gotGPUs[1].GPUIndex != 0 || gotGPUs[1].Position != 1 {
			t.Fatalf("gpus must read ordered by position, not gpu_index: %+v", gotGPUs)
		}

		// The measured write targets gpu_index 0, which now sits at position 1
		// (read index 1) — proving the write path keys on gpu_index while the
		// read path keys on position.
		if err := s.UpdateRuntimeSpecGPUMeasured(ctx, "rspec_rt2", 0, 21800); err != nil {
			t.Fatalf("measured: %v", err)
		}
		gotGPUs, _ = s.RuntimeSpecGPUs(ctx, "rspec_rt2")
		if gotGPUs[1].GPUIndex != 0 || gotGPUs[1].VRAMMeasuredMB != 21800 || gotGPUs[1].VRAMEstimateMB != 22000 {
			t.Fatalf("measured must not clobber estimate: %+v", gotGPUs[1])
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
		if _, ok, err := s.RuntimeSpecByID(ctx, "rspec_rt2"); err != nil || ok {
			t.Fatalf("RuntimeSpecByID after delete: ok=%v err=%v", ok, err)
		}
	})
}

// TestRoutingStoreApplicationEndpointModes proves all three drivers round-trip
// ResponsesMode / MessagesMode on an application.
func TestRoutingStoreApplicationEndpointModes(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_em", Name: "EM", Domain: "em.local", Provider: routing.ProviderVLLM,
			Endpoint: "http://em:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_em", ServerID: "srv_em", Type: routing.ProviderVLLM, Port: 8100, Scheme: "http",
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModePassthrough,
			Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		got, err := s.ApplicationByID(ctx, "app_em")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.ResponsesMode != routing.EndpointModeDisabled || got.MessagesMode != routing.EndpointModePassthrough {
			t.Fatalf("modes: got responses=%q messages=%q, want disabled/passthrough", got.ResponsesMode, got.MessagesMode)
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
		for _, mid := range []string{"map_cr", "map_cr2", "map_cr3"} {
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

		// Set + ordered read. THREE pairs, submitted in fully reversed order,
		// so both halves of the documented `order by mapping_a_id,
		// mapping_b_id` are actually exercised: the two (map_cr, *) pairs pin
		// the mapping_b_id tie-break, and (map_cr2, map_cr3) pins the primary
		// key. A single-pair assertion here could not fail against a broken
		// ORDER BY or sort comparator at all.
		want := []routing.CoResidencyRule{
			{ApplicationID: "app_cr", MappingAID: "map_cr2", MappingBID: "map_cr3", CreatedAt: now},
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr3", CreatedAt: now},
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr2", CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, "app_cr", want); err != nil {
			t.Fatalf("set: %v", err)
		}
		rules, err = s.CoResidencyRulesByApplication(ctx, "app_cr")
		if err != nil || len(rules) != 3 {
			t.Fatalf("read back: err=%v rules=%+v, want 3", err, rules)
		}
		wantOrder := [][2]string{{"map_cr", "map_cr2"}, {"map_cr", "map_cr3"}, {"map_cr2", "map_cr3"}}
		for i, w := range wantOrder {
			if rules[i].MappingAID != w[0] || rules[i].MappingBID != w[1] {
				t.Fatalf("position %d = (%s, %s), want (%s, %s); full read: %+v",
					i, rules[i].MappingAID, rules[i].MappingBID, w[0], w[1], rules)
			}
		}

		// An EXACT duplicate pair within one set hits the composite primary
		// key (application_id, mapping_a_id, mapping_b_id) on the SQL side ->
		// ErrConflict; MemoryStore must reject it the same way (it did not
		// until this was added -- a real memory-vs-SQL divergence that only
		// the portal's own pair validation was hiding). The rejected set must
		// not partially apply either: the previous three pairs stay.
		dupPairs := []routing.CoResidencyRule{
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr2", CreatedAt: now},
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr2", CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, "app_cr", dupPairs); err != ErrConflict {
			t.Fatalf("exact duplicate pair: want ErrConflict, got %v", err)
		}
		if rules, err = s.CoResidencyRulesByApplication(ctx, "app_cr"); err != nil || len(rules) != 3 {
			t.Fatalf("failed duplicate-pair set must not partially apply: err=%v rules=%+v", err, rules)
		}
		// The reversed pair (b, a) is NOT a duplicate at the store layer --
		// canonical ordering is portal-level validation, and the composite PK
		// treats the two as distinct rows. Pinning this keeps the new
		// duplicate check from being tightened into store-level canonicalization
		// by accident.
		reversedOK := []routing.CoResidencyRule{
			{ApplicationID: "app_cr", MappingAID: "map_cr", MappingBID: "map_cr2", CreatedAt: now},
			{ApplicationID: "app_cr", MappingAID: "map_cr2", MappingBID: "map_cr", CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, "app_cr", reversedOK); err != nil {
			t.Fatalf("reversed pair must not be treated as a duplicate by the store: %v", err)
		}
		if rules, err = s.CoResidencyRulesByApplication(ctx, "app_cr"); err != nil || len(rules) != 2 {
			t.Fatalf("reversed pair set: err=%v rules=%+v, want 2", err, rules)
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

// --- Agent runtime manager: file-mode runtime reports memory-vs-SQL parity (Task 4) ---

// TestRoutingStoreServerRuntimeReports proves routing.MemoryStore and the SQL
// store agree on ServerRuntimeReport semantics: absent read, insert,
// round-trip, upsert-overwrite, and the orphan-server FK error — mirroring
// TestRoutingStoreServerGPUBudgets above, but for the 1:1 upsert-overwrite
// shape server_runtime_reports shares with server_hardware.
func TestRoutingStoreServerRuntimeReports(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_rr", Name: "RR", Domain: "rr.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://rr", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// Absent -> (zero, false, nil).
		if _, ok, err := s.ServerRuntimeReportByServer(ctx, "srv_rr"); err != nil || ok {
			t.Fatalf("absent report: ok=%v err=%v", ok, err)
		}

		// Insert, then round-trip.
		first := routing.ServerRuntimeReport{
			ServerID: "srv_rr", CollectedAt: now, ReportJSON: `{"mode":"file","binary":"/usr/bin/llama-server"}`, UpdatedAt: now,
		}
		if err := s.UpsertServerRuntimeReport(ctx, first); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := s.ServerRuntimeReportByServer(ctx, "srv_rr")
		if err != nil || !ok || got.ReportJSON != first.ReportJSON || !got.CollectedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
			t.Fatalf("read back: ok=%v err=%v got=%#v", ok, err, got)
		}

		// Upsert overwrites the same server row.
		second := routing.ServerRuntimeReport{
			ServerID: "srv_rr", CollectedAt: now.Add(time.Minute), ReportJSON: `{"mode":"file","binary":"/usr/bin/vllm"}`, UpdatedAt: now.Add(time.Minute),
		}
		if err := s.UpsertServerRuntimeReport(ctx, second); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		got, ok, err = s.ServerRuntimeReportByServer(ctx, "srv_rr")
		if err != nil || !ok || got.ReportJSON != second.ReportJSON || !got.CollectedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("after overwrite: ok=%v err=%v got=%#v", ok, err, got)
		}

		// Unknown server -> ErrNotFound.
		orphan := routing.ServerRuntimeReport{ServerID: "srv_missing", CollectedAt: now, ReportJSON: "{}", UpdatedAt: now}
		if err := s.UpsertServerRuntimeReport(ctx, orphan); err != ErrNotFound {
			t.Fatalf("unknown server: want ErrNotFound, got %v", err)
		}
	})
}

// TestRoutingStoreSingleServerAgentApplication is the memory/SQL parity case
// for the "at most one server_agent application per AI server" invariant.
//
// Migration 68 gives the SQL side a partial unique index on
// applications(server_id) where type = 'server_agent'; before this case
// existed, routing.MemoryStore guarded only duplicate id, missing server and
// applicationPortTakenLocked, so it accepted a second server_agent
// application (and a retype into one) with a nil error where every SQL
// dialect returned ErrConflict. Memory is the dev/Playwright driver, so that
// divergence let the e2e suite reach a state the product cannot hold.
//
// Both write paths are covered because retyping an existing application is
// the easy way past a create-only guard, and both the create and the retype
// use a FREE port, so a passing ErrConflict cannot be the port gate firing by
// accident. The negative half (a second NON-server_agent application, and a
// no-op retype of the server's own server_agent application) is asserted too:
// a guard that rejects those would be strictly stronger than the SQL index
// and would make ordinary edits impossible.
func TestRoutingStoreSingleServerAgentApplication(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_sa", Name: "SA", Domain: "sa.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://sa", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		app := func(id, appType string, port int) routing.Application {
			return routing.Application{
				ID: id, ServerID: "srv_sa", Type: appType, Port: port, Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
				TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
			}
		}

		if err := s.CreateApplication(ctx, app("app_sa_first", routing.ProviderServerAgent, 8081)); err != nil {
			t.Fatalf("first server_agent application: %v", err)
		}

		// A SECOND server_agent application on the same server, on a free
		// port -> ErrConflict on both backends.
		if err := s.CreateApplication(ctx, app("app_sa_second", routing.ProviderServerAgent, 8082)); err != ErrConflict {
			t.Fatalf("second server_agent create: want ErrConflict, got %v", err)
		}

		// A non-server_agent application on the same server is unaffected --
		// the index's WHERE clause excludes it.
		plain := app("app_sa_plain", routing.ProviderVLLM, 8083)
		if err := s.CreateApplication(ctx, plain); err != nil {
			t.Fatalf("plain application on the same server: %v", err)
		}

		// RETYPING that one to server_agent is the second mapped path and must
		// be refused identically.
		retyped := plain
		retyped.Type = routing.ProviderServerAgent
		if err := s.UpdateApplication(ctx, retyped); err != ErrConflict {
			t.Fatalf("retype to server_agent: want ErrConflict, got %v", err)
		}

		// An in-place edit of the server's OWN server_agent application is not
		// a self-collision (the row already satisfies the index).
		edited := app("app_sa_first", routing.ProviderServerAgent, 8091)
		if err := s.UpdateApplication(ctx, edited); err != nil {
			t.Fatalf("edit the server's own server_agent application: %v", err)
		}

		// End state on both backends: exactly one server_agent application.
		apps, err := s.ApplicationsByServer(ctx, "srv_sa")
		if err != nil {
			t.Fatalf("ApplicationsByServer: %v", err)
		}
		agents := 0
		for _, got := range apps {
			if got.Type == routing.ProviderServerAgent {
				agents++
			}
		}
		if len(apps) != 2 || agents != 1 {
			t.Fatalf("end state: %d applications (%d server_agent), want 2 (1 server_agent): %+v", len(apps), agents, apps)
		}

		// A second server_agent application on a DIFFERENT server is legal:
		// the constraint is per-server, not global.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_sa2", Name: "SA2", Domain: "sa2.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://sa2", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create second server: %v", err)
		}
		other := app("app_sa_other", routing.ProviderServerAgent, 8081)
		other.ServerID = "srv_sa2"
		if err := s.CreateApplication(ctx, other); err != nil {
			t.Fatalf("server_agent application on a second server: %v", err)
		}
	})
}

// --- Benchmark history: the kind-specific payload columns -------------------

// TestRoutingStoreBenchmarkRunVRAMJSON is the MEMORY-vs-SQL half of the
// migration-v71 vram_json contract (the sqlite-vs-postgres half is
// TestConformanceBenchmarkRunsVRAMJSON): a kind=="vram" row round-trips its
// opaque per-GPU payload through routing.Store on both production backends,
// and a run of any other kind reads back with the payload empty. The memory
// driver stores routing.BenchmarkRun by value, so a new field reaches it for
// free while the SQL driver needs the column in BOTH its insert and its
// select list -- exactly the asymmetry a memory-only suite cannot see.
func TestRoutingStoreBenchmarkRunVRAMJSON(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_vram", Name: "VRAM", Domain: "vram.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://vram", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_vram", ServerID: "srv_vram", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_vram", ApplicationID: "app_vram", GatewayModelName: "vram-model",
			AppModelName: "vram-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		const payload = `{"isolated":true,"gpus":[{"index":1,"delta_mb":22000,"attributable":true}]}`
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map_vram", ServerID: "srv_vram", CreatedAt: now.Add(time.Minute),
			Kind: "vram", VRAMJSON: payload,
		}); err != nil {
			t.Fatalf("insert vram run: %v", err)
		}
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map_vram", ServerID: "srv_vram", CreatedAt: now, GenTokensPerSecond: 12,
		}); err != nil {
			t.Fatalf("insert speed run: %v", err)
		}

		runs, err := s.BenchmarkRunsByMapping(ctx, "map_vram", 50)
		if err != nil {
			t.Fatalf("BenchmarkRunsByMapping: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("expected 2 runs, got %d (%+v)", len(runs), runs)
		}
		// Newest-first: the +1m VRAM row leads the speed row.
		if runs[0].Kind != "vram" {
			t.Fatalf("newest run kind = %q, want vram", runs[0].Kind)
		}
		if runs[0].VRAMJSON != payload {
			t.Errorf("vram payload round-trip mismatch:\n got %q\nwant %q", runs[0].VRAMJSON, payload)
		}
		if runs[1].Kind != "speed" || runs[1].VRAMJSON != "" {
			t.Errorf("speed run = %+v, want kind=speed with an empty VRAM payload", runs[1])
		}
	})
}
