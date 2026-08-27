// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestRoutingStoreDeleteAIServerCascadesEverything closes a PRE-EXISTING,
// SYSTEMIC memory-vs-SQL divergence: the SQL store cascades a server delete
// through every `references ai_servers(id) on delete cascade` FK (and
// transitively through applications -> model_mappings ->
// agent_runtime_specs), while routing.MemoryStore only cleared servers,
// owners, agent tokens, admin-group links, resource-group links,
// certificates, applications and mappings — leaving every other per-server
// map populated. In memory mode (the dev and Playwright driver) those rows
// stayed readable after the server was gone.
//
// The FK graph, from `grep 'references ai_servers(id)' internal/store/migrate.go`
// plus the two transitive hops, is the checklist this test walks. Every read
// below is exercised BEFORE the delete (so a read that returns empty for an
// unrelated reason cannot make the test vacuous) and again after.
//
// Deliberately NOT covered, because the SQL side does not cascade them
// either: principal_limits (no FK — principal_type/principal_id is an opaque
// pair), resource_group_provisions (target_kind/target_id, no FK on the
// target), model_settings (keyed by gateway model name), and
// model_group_members (member_gateway_name, no FK to model_mappings).
func TestRoutingStoreDeleteAIServerCascadesEverything(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// route_affinity.api_token_id/user_id carry FKs only the SQL store
	// enforces; MemoryStore accepts the same literal ids with no seed.
	seedSQL := func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateUser(ctx, newTestUser("u_casc", "cascade@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_casc", UserID: "u_casc", Name: "cascade", CreatedAt: now, UpdatedAt: now}, "cascade-secret"); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}

	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_casc", Name: "Cascade", Domain: "casc.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://casc", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_casc", ServerID: "srv_casc", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		for _, mid := range []string{"map_casc", "map_casc2"} {
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: mid, ApplicationID: "app_casc", GatewayModelName: mid + "-model",
				AppModelName: mid + "-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %s: %v", mid, err)
			}
		}

		// --- populate every per-server / per-application / per-mapping table
		if err := s.SetServerOwners(ctx, "srv_casc", []string{"u_casc"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}
		if err := s.UpsertAgentToken(ctx, routing.AgentToken{
			ID: "agt_casc", ServerID: "srv_casc", SecretPrefix: "casc", CreatedAt: now, UpdatedAt: now,
		}, "cascade-agent-hash"); err != nil {
			t.Fatalf("upsert agent token: %v", err)
		}
		if err := s.UpsertTelemetry(ctx, routing.ServerTelemetry{
			ServerID: "srv_casc", ReportedAt: now, AgentVersion: "1.0.0", OS: "linux", Arch: "amd64",
			CPULoad: 0.5, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert telemetry: %v", err)
		}
		if err := s.UpsertServerHardware(ctx, routing.ServerHardware{
			ServerID: "srv_casc", CollectedAt: now, ReportJSON: `{"cpu":"x"}`, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert hardware: %v", err)
		}
		if err := s.InsertTelemetrySample(ctx, routing.TelemetrySample{
			ServerID: "srv_casc", ReportedAt: now, CPUUtilPct: 42,
		}); err != nil {
			t.Fatalf("insert telemetry sample: %v", err)
		}
		if err := s.InsertServerAvailabilitySample(ctx, routing.ServerAvailabilitySample{
			ServerID: "srv_casc", ReportedAt: now, Health: routing.HealthHealthy,
			ReachableCount: 1, ActiveCount: 1, AgentReporting: true,
		}); err != nil {
			t.Fatalf("insert availability sample: %v", err)
		}
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			ID: "bench_casc", MappingID: "map_casc", ServerID: "srv_casc", CreatedAt: now,
			GenTokensPerSecond: 30, Kind: "speed",
		}); err != nil {
			t.Fatalf("insert benchmark run: %v", err)
		}
		affinityKey := routing.AffinityKey{APITokenID: "tok_casc", Model: "map_casc-model", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_casc"}
		if err := s.UpsertAffinity(ctx, routing.RouteAffinity{
			ID: "aff_casc", APITokenID: "tok_casc", UserID: "u_casc", Model: "map_casc-model",
			APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_casc",
			ApplicationID: "app_casc", ServerID: "srv_casc",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert affinity: %v", err)
		}
		if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: "rspec_casc", MappingID: "map_casc", Enabled: true, Binary: "/usr/bin/llama-server",
			Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert runtime spec: %v", err)
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_casc", []routing.RuntimeSpecGPU{
			{SpecID: "rspec_casc", GPUIndex: 0, VRAMEstimateMB: 22000},
		}); err != nil {
			t.Fatalf("set runtime spec gpus: %v", err)
		}
		if err := s.SetCoResidencyRules(ctx, "app_casc", []routing.CoResidencyRule{
			{ApplicationID: "app_casc", MappingAID: "map_casc", MappingBID: "map_casc2", CreatedAt: now},
		}); err != nil {
			t.Fatalf("set coresidency rules: %v", err)
		}
		if err := s.SetServerGPUBudgets(ctx, "srv_casc", []routing.ServerGPUBudget{
			{ServerID: "srv_casc", GPUIndex: 0, BudgetMB: 24000, CreatedAt: now, UpdatedAt: now},
		}); err != nil {
			t.Fatalf("set gpu budgets: %v", err)
		}
		if err := s.UpsertServerRuntimeReport(ctx, routing.ServerRuntimeReport{
			ServerID: "srv_casc", CollectedAt: now, ReportJSON: `{"mode":"file"}`, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert runtime report: %v", err)
		}

		// --- every read must see the seeded row BEFORE the delete, so a
		// post-delete "empty" assertion is never vacuous.
		assertCascadeState(t, ctx, s, affinityKey, true)

		if err := s.DeleteAIServer(ctx, "srv_casc"); err != nil {
			t.Fatalf("delete server: %v", err)
		}

		// --- and nothing after it.
		assertCascadeState(t, ctx, s, affinityKey, false)
	})
}

// assertCascadeState checks every per-server, per-application and
// per-mapping read for srv_casc/app_casc/map_casc. want=true requires the
// seeded row to be present (the pre-delete sanity pass), want=false requires
// it gone. Both directions go through ONE function so the two passes can
// never drift apart and quietly stop covering a table.
func assertCascadeState(t *testing.T, ctx context.Context, s routing.Store, affinityKey routing.AffinityKey, want bool) {
	t.Helper()
	stage := "after delete"
	if want {
		stage = "before delete"
	}
	// check reports a table whose presence does not match want.
	check := func(table string, present bool) {
		t.Helper()
		if present != want {
			t.Fatalf("%s: %s present=%v, want %v", stage, table, present, want)
		}
	}

	if _, err := s.AIServerByID(ctx, "srv_casc"); (err == nil) != want {
		t.Fatalf("%s: AIServerByID err=%v, want present=%v", stage, err, want)
	}
	owners, err := s.ServerOwners(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ServerOwners: %v", stage, err)
	}
	check("server_owners", len(owners) > 0)

	_, ok, err := s.AgentTokenByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: AgentTokenByServer: %v", stage, err)
	}
	check("agent_tokens", ok)

	_, ok, err = s.TelemetryByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: TelemetryByServer: %v", stage, err)
	}
	check("server_telemetry", ok)

	_, ok, err = s.ServerHardwareByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ServerHardwareByServer: %v", stage, err)
	}
	check("server_hardware", ok)

	from, to := time.Time{}, time.Now().UTC().Add(24*time.Hour)
	samples, err := s.TelemetrySamples(ctx, "srv_casc", from, to, 100)
	if err != nil {
		t.Fatalf("%s: TelemetrySamples: %v", stage, err)
	}
	check("server_telemetry_samples", len(samples) > 0)

	avail, err := s.ServerAvailabilitySamples(ctx, "srv_casc", from, to, 100)
	if err != nil {
		t.Fatalf("%s: ServerAvailabilitySamples: %v", stage, err)
	}
	check("server_availability_samples", len(avail) > 0)

	apps, err := s.ApplicationsByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ApplicationsByServer: %v", stage, err)
	}
	check("applications", len(apps) > 0)

	mappings, err := s.MappingsByApplication(ctx, "app_casc")
	if err != nil {
		t.Fatalf("%s: MappingsByApplication: %v", stage, err)
	}
	check("model_mappings", len(mappings) > 0)

	runs, err := s.BenchmarkRunsByMapping(ctx, "map_casc", 100)
	if err != nil {
		t.Fatalf("%s: BenchmarkRunsByMapping: %v", stage, err)
	}
	check("model_mapping_benchmarks", len(runs) > 0)

	_, ok, err = s.Affinity(ctx, affinityKey)
	if err != nil {
		t.Fatalf("%s: Affinity: %v", stage, err)
	}
	check("route_affinity", ok)

	_, ok, err = s.RuntimeSpecByID(ctx, "rspec_casc")
	if err != nil {
		t.Fatalf("%s: RuntimeSpecByID: %v", stage, err)
	}
	check("agent_runtime_specs", ok)

	gpus, err := s.RuntimeSpecGPUs(ctx, "rspec_casc")
	if err != nil {
		t.Fatalf("%s: RuntimeSpecGPUs: %v", stage, err)
	}
	if gpus == nil {
		t.Fatalf("%s: RuntimeSpecGPUs returned a bare nil, want non-nil (JSON [] not null)", stage)
	}
	check("agent_runtime_spec_gpus", len(gpus) > 0)

	rules, err := s.CoResidencyRulesByApplication(ctx, "app_casc")
	if err != nil {
		t.Fatalf("%s: CoResidencyRulesByApplication: %v", stage, err)
	}
	if rules == nil {
		t.Fatalf("%s: CoResidencyRulesByApplication returned a bare nil, want non-nil", stage)
	}
	check("agent_coresidency_rules", len(rules) > 0)

	budgets, err := s.ServerGPUBudgets(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ServerGPUBudgets: %v", stage, err)
	}
	if budgets == nil {
		t.Fatalf("%s: ServerGPUBudgets returned a bare nil, want non-nil", stage)
	}
	check("ai_server_gpu_budgets", len(budgets) > 0)

	_, ok, err = s.ServerRuntimeReportByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ServerRuntimeReportByServer: %v", stage, err)
	}
	check("server_runtime_reports", ok)
}

// TestRoutingStoreDeleteApplicationAndMappingCascade covers the same defect
// class one and two levels DOWN from A6's server delete, found while fixing
// it: MemoryStore.DeleteApplication stopped at `mappings` and
// MemoryStore.DeleteMapping deleted only the mapping row, while the SQL side
// cascades applications -> {model_mappings, agent_coresidency_rules,
// route_affinity} and model_mappings -> {model_mapping_benchmarks,
// agent_runtime_specs -> agent_runtime_spec_gpus, agent_coresidency_rules}.
//
// The mapping case also pins the PARTIAL co-residency removal a server or
// application delete cannot distinguish: deleting ONE of an application's
// mappings must drop only the pairs naming it, leaving the application's
// other pairs in place.
func TestRoutingStoreDeleteApplicationAndMappingCascade(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateUser(ctx, newTestUser("u_dc", "delcascade@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_dc", UserID: "u_dc", Name: "dc", CreatedAt: now, UpdatedAt: now}, "delcascade-secret"); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}

	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_dc", Name: "DC", Domain: "dc.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://dc", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_dc", ServerID: "srv_dc", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		for _, mid := range []string{"map_dc_a", "map_dc_b", "map_dc_c"} {
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: mid, ApplicationID: "app_dc", GatewayModelName: mid + "-model",
				AppModelName: mid + "-upstream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %s: %v", mid, err)
			}
		}
		if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: "rspec_dc_a", MappingID: "map_dc_a", Enabled: true, Binary: "/usr/bin/x",
			Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert spec a: %v", err)
		}
		if err := s.SetRuntimeSpecGPUs(ctx, "rspec_dc_a", []routing.RuntimeSpecGPU{
			{SpecID: "rspec_dc_a", GPUIndex: 0, VRAMEstimateMB: 1000},
		}); err != nil {
			t.Fatalf("set gpus a: %v", err)
		}
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			ID: "bench_dc_a", MappingID: "map_dc_a", ServerID: "srv_dc", CreatedAt: now, Kind: "speed",
		}); err != nil {
			t.Fatalf("insert benchmark: %v", err)
		}
		// (a,b) names map_dc_a; (b,c) does not.
		if err := s.SetCoResidencyRules(ctx, "app_dc", []routing.CoResidencyRule{
			{ApplicationID: "app_dc", MappingAID: "map_dc_a", MappingBID: "map_dc_b", CreatedAt: now},
			{ApplicationID: "app_dc", MappingAID: "map_dc_b", MappingBID: "map_dc_c", CreatedAt: now},
		}); err != nil {
			t.Fatalf("set coresidency: %v", err)
		}

		// --- DeleteMapping(map_dc_a): its spec, that spec's GPU rows, its
		// benchmark runs, and the ONE pair naming it must go; the (b,c) pair
		// and the other two mappings must survive.
		if err := s.DeleteMapping(ctx, "map_dc_a"); err != nil {
			t.Fatalf("delete mapping: %v", err)
		}
		if _, ok, err := s.RuntimeSpecByID(ctx, "rspec_dc_a"); err != nil || ok {
			t.Fatalf("spec must cascade with its mapping: ok=%v err=%v", ok, err)
		}
		if gpus, err := s.RuntimeSpecGPUs(ctx, "rspec_dc_a"); err != nil || len(gpus) != 0 {
			t.Fatalf("spec gpu rows must cascade with the mapping: err=%v gpus=%#v", err, gpus)
		}
		if runs, err := s.BenchmarkRunsByMapping(ctx, "map_dc_a", 10); err != nil || len(runs) != 0 {
			t.Fatalf("benchmark runs must cascade with the mapping: err=%v runs=%d", err, len(runs))
		}
		rules, err := s.CoResidencyRulesByApplication(ctx, "app_dc")
		if err != nil {
			t.Fatalf("coresidency after mapping delete: %v", err)
		}
		if len(rules) != 1 || rules[0].MappingAID != "map_dc_b" || rules[0].MappingBID != "map_dc_c" {
			t.Fatalf("only the pair naming the deleted mapping may go, got %+v", rules)
		}
		if mappings, err := s.MappingsByApplication(ctx, "app_dc"); err != nil || len(mappings) != 2 {
			t.Fatalf("sibling mappings must survive: err=%v n=%d, want 2", err, len(mappings))
		}

		// --- DeleteApplication: the remaining mappings AND the application's
		// whole co-residency set AND its route affinities must go.
		affinityKey := routing.AffinityKey{APITokenID: "tok_dc", Model: "map_dc_b-model", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_dc"}
		if err := s.UpsertAffinity(ctx, routing.RouteAffinity{
			ID: "aff_dc", APITokenID: "tok_dc", UserID: "u_dc", Model: "map_dc_b-model",
			APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_dc",
			ApplicationID: "app_dc", ServerID: "srv_dc",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert affinity: %v", err)
		}
		if _, ok, err := s.Affinity(ctx, affinityKey); err != nil || !ok {
			t.Fatalf("affinity must exist before the application delete: ok=%v err=%v", ok, err)
		}
		if err := s.DeleteApplication(ctx, "app_dc"); err != nil {
			t.Fatalf("delete application: %v", err)
		}
		if mappings, err := s.MappingsByApplication(ctx, "app_dc"); err != nil || len(mappings) != 0 {
			t.Fatalf("mappings must cascade with the application: err=%v n=%d", err, len(mappings))
		}
		if rules, err := s.CoResidencyRulesByApplication(ctx, "app_dc"); err != nil || len(rules) != 0 {
			t.Fatalf("coresidency must cascade with the application: err=%v rules=%+v", err, rules)
		}
		if _, ok, err := s.Affinity(ctx, affinityKey); err != nil || ok {
			t.Fatalf("route affinity must cascade with the application: ok=%v err=%v", ok, err)
		}
		// The server itself is untouched by an application delete.
		if _, err := s.AIServerByID(ctx, "srv_dc"); err != nil {
			t.Fatalf("server must survive an application delete: %v", err)
		}
	})
}
