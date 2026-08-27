// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

const (
	// cascadeSystemGroupID/cascadeAdminGroupID back the server_admin_groups
	// link the cascade must drop. The admin group itself must SURVIVE the
	// server delete (the FK cascades the join row, not the group).
	cascadeSystemGroupID = "ugrp_casc_sys"
	cascadeAdminGroupID  = "ugrp_casc_admin"
	// cascadeResourceGroupID is the resource group srv_casc is a MEMBER of.
	// resource_group_servers is keyed the reverse way round from
	// server_admin_groups, which is why MemoryStore needs a loop over every
	// group rather than one delete — and why it needs a reader here.
	cascadeResourceGroupID = "rg_casc"
	// cascadeCertDomain is the certificates row pinned to srv_casc
	// (certificates.server_id, migration v57).
	cascadeCertDomain = "cert-casc.example.test"
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
// plus the two transitive hops, is what this test is MEANT to walk — but note
// what the enumeration method can and cannot establish. Running this test
// against the pre-fix store reveals only the maps the test actually READS, so
// it can never prove the cascade list is complete; it proves that every table
// listed below leaks without the fix. Three maps were in the cascade but read
// by nothing (serverAdminGroups, resourceGroupServers, certificates) and so
// could be deleted with both tests still green — they are covered here now.
// Every read below is exercised BEFORE the delete (so a read that returns
// empty for an unrelated reason cannot make the test vacuous) and again
// after.
//
// Deliberately NOT covered, because the SQL side does not cascade them
// either: principal_limits (no FK — principal_type/principal_id is an opaque
// pair), resource_group_provisions (target_kind/target_id, no FK on the
// target), model_settings (keyed by gateway model name), and
// model_group_members (member_gateway_name, no FK to model_mappings).
//
// Deliberately NOT covered even though the FK exists: nothing. If a per-server
// map is added to MemoryStore.DeleteAIServer without a reader here, this
// comment is wrong and the new map is untested — add the reader.
func TestRoutingStoreDeleteAIServerCascadesEverything(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// route_affinity.api_token_id/user_id and server_admin_groups.group_id
	// carry FKs only the SQL store enforces; MemoryStore accepts the same
	// literal ids with no seed.
	seedSQL := func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if err := s.CreateUser(ctx, newTestUser("u_casc", "cascade@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_casc", UserID: "u_casc", Name: "cascade", CreatedAt: now, UpdatedAt: now}, "cascade-secret"); err != nil {
			t.Fatalf("seed token: %v", err)
		}
		sysGroup := UserGroup{ID: cascadeSystemGroupID, Tier: GroupTierSystem, Name: "Cascade System", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sysGroup); err != nil {
			t.Fatalf("seed system group: %v", err)
		}
		if err := s.CreateUserGroup(ctx, UserGroup{
			ID: cascadeAdminGroupID, Tier: GroupTierAdmin, Name: "Cascade Admin",
			ParentGroupID: sysGroup.ID, OwnerUserID: "u_casc", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed admin group: %v", err)
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

		// A SECOND server, with its own application, that must survive the
		// delete untouched. Its application is what the route_affinity row
		// below points at, so that row can ONLY be removed by the server_id
		// sweep (deleteAffinitiesForServerLocked) and never by the
		// application hop — with the affinity pointing at srv_casc's own
		// application, deleting that line left both tests green.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_casc2", Name: "Cascade Bystander", Domain: "casc2.example.test", Provider: routing.ProviderMock,
			Endpoint: "mock://casc2", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create bystander server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_casc2", ServerID: "srv_casc2", Type: routing.ProviderVLLM, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create bystander application: %v", err)
		}

		// --- populate every per-server / per-application / per-mapping table
		if err := s.SetServerOwners(ctx, "srv_casc", []string{"u_casc"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}
		if err := s.SetServerAdminGroup(ctx, "srv_casc", cascadeAdminGroupID); err != nil {
			t.Fatalf("set admin group: %v", err)
		}
		if err := s.CreateResourceGroup(ctx, routing.ResourceGroup{
			ID: cascadeResourceGroupID, Name: "Cascade RG", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create resource group: %v", err)
		}
		if err := s.SetResourceGroupServer(ctx, cascadeResourceGroupID, "srv_casc"); err != nil {
			t.Fatalf("set resource group server: %v", err)
		}
		if err := s.SetResourceGroupServer(ctx, cascadeResourceGroupID, "srv_casc2"); err != nil {
			t.Fatalf("set resource group bystander server: %v", err)
		}
		if err := s.UpsertCertificate(ctx, routing.Certificate{
			Domain: cascadeCertDomain, Kind: "server", ServerID: "srv_casc", Status: "active",
			FullchainPEM: "-----BEGIN CERTIFICATE-----\ncasc\n-----END CERTIFICATE-----\n",
			KeySealed:    "plain:casc-key", NotBefore: now, NotAfter: now.Add(24 * time.Hour),
			IssuedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert certificate: %v", err)
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
		// route_affinity.application_id and .server_id are INDEPENDENT FKs, so
		// an affinity pinned to srv_casc but owned by an application on
		// srv_casc2 is legal on both backends. That is deliberate: it makes
		// the server_id sweep the ONLY thing that can remove this row.
		affinityKey := routing.AffinityKey{APITokenID: "tok_casc", Model: "map_casc-model", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_casc"}
		if err := s.UpsertAffinity(ctx, routing.RouteAffinity{
			ID: "aff_casc", APITokenID: "tok_casc", UserID: "u_casc", Model: "map_casc-model",
			APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_casc",
			ApplicationID: "app_casc2", ServerID: "srv_casc",
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

		// --- the cascade must stop at the FK boundary: the rows the deleted
		// server merely POINTED at, and the bystander server's own state,
		// survive. Without this half, a cascade that deleted the admin group
		// or the whole resource group would also pass the assertions above.
		if _, err := s.AIServerByID(ctx, "srv_casc2"); err != nil {
			t.Fatalf("bystander server must survive: %v", err)
		}
		if apps, err := s.ApplicationsByServer(ctx, "srv_casc2"); err != nil || len(apps) != 1 {
			t.Fatalf("bystander application must survive: err=%v n=%d, want 1", err, len(apps))
		}
		if _, err := s.ResourceGroupByID(ctx, cascadeResourceGroupID); err != nil {
			t.Fatalf("resource group itself must survive (only the membership row cascades): %v", err)
		}
		members, err := s.ResourceGroupServers(ctx, cascadeResourceGroupID)
		if err != nil || len(members) != 1 || members[0] != "srv_casc2" {
			t.Fatalf("resource group must keep its OTHER member: err=%v members=%v, want [srv_casc2]", err, members)
		}
	})
}

// assertCascadeState checks every per-server, per-application and
// per-mapping read for srv_casc/app_casc/map_casc. want=true requires the
// seeded row to be present (the pre-delete sanity pass), want=false requires
// it gone. Both directions go through ONE function so the two passes can
// never drift apart and quietly stop covering a table.
//
// THE COVERAGE RULE: a line in MemoryStore.DeleteAIServer with no reader here
// is a line that can be deleted with every test still green. Three of them
// were (server_admin_groups, resource_group_servers, certificates) until this
// function grew the readers below. Add the reader with the cascade line, not
// after it.
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

	// server_admin_groups. Was in the cascade but read by nothing: deleting
	// `delete(m.serverAdminGroups, id)` left both cascade tests green. In
	// memory mode a leaked grant is not cosmetic — dev/e2e fixtures use FIXED
	// server ids, so re-creating a server under the same id silently inherits
	// the previous server's admin-group grant.
	adminGroups, err := s.ServerAdminGroups(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ServerAdminGroups: %v", stage, err)
	}
	check("server_admin_groups", len(adminGroups) > 0)

	// resource_group_servers, read from BOTH directions: MemoryStore keys it
	// resourceGroupID -> serverID, so the cascade is a loop over every group,
	// and only the by-server reader would notice a loop that skipped one.
	rgMembers, err := s.ResourceGroupServers(ctx, cascadeResourceGroupID)
	if err != nil {
		t.Fatalf("%s: ResourceGroupServers: %v", stage, err)
	}
	memberOfRG := false
	for _, id := range rgMembers {
		if id == "srv_casc" {
			memberOfRG = true
		}
	}
	check("resource_group_servers (by group)", memberOfRG)
	rgsByServer, err := s.ResourceGroupsByServer(ctx, "srv_casc")
	if err != nil {
		t.Fatalf("%s: ResourceGroupsByServer: %v", stage, err)
	}
	check("resource_group_servers (by server)", len(rgsByServer) > 0)

	// certificates.server_id (migration v57). Also read by nothing before:
	// deleting the certificates loop left both cascade tests green.
	if _, err := s.CertificateByServer(ctx, "srv_casc"); (err == nil) != want {
		t.Fatalf("%s: CertificateByServer err=%v, want present=%v", stage, err, want)
	}

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
//
// And it pins the CROSS-APPLICATION co-residency row, without which
// `delete(m.coresidency, applicationID)` is dead code that can be removed
// with every test still green: the mapping hop (which drops every pair naming
// a deleted mapping, on either side) already removes an application's own
// pairs, so only a rule OWNED by application X that names mappings owned by
// application Y survives that hop and needs the whole-set delete.
// agent_coresidency_rules has three independent FKs and SetCoResidencyRules
// checks only that the mappings EXIST, so such a row is legal on both
// backends — and with the line removed, memory leaks it while SQL cascades it
// through application_id.
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

		// --- CROSS-APPLICATION co-residency: a rule OWNED by app_dc2 that
		// names two of app_dc's mappings. app_dc2 has no mappings of its own,
		// so the mapping hop cannot reach this row — only the whole-set
		// `delete(m.coresidency, applicationID)` can.
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_dc2", ServerID: "srv_dc", Type: routing.ProviderVLLM, Port: 8082, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create second application: %v", err)
		}
		if err := s.SetCoResidencyRules(ctx, "app_dc2", []routing.CoResidencyRule{
			{ApplicationID: "app_dc2", MappingAID: "map_dc_b", MappingBID: "map_dc_c", CreatedAt: now},
		}); err != nil {
			t.Fatalf("set cross-application coresidency: %v", err)
		}
		if rules, err := s.CoResidencyRulesByApplication(ctx, "app_dc2"); err != nil || len(rules) != 1 {
			t.Fatalf("cross-application rule must exist before the delete: err=%v rules=%+v", err, rules)
		}
		if err := s.DeleteApplication(ctx, "app_dc2"); err != nil {
			t.Fatalf("delete second application: %v", err)
		}
		if rules, err := s.CoResidencyRulesByApplication(ctx, "app_dc2"); err != nil || len(rules) != 0 {
			t.Fatalf("the application's WHOLE co-residency set must cascade, including pairs naming another application's mappings: err=%v rules=%+v", err, rules)
		}
		// The mappings that rule named belong to app_dc and are untouched, and
		// so is app_dc's own pair over the same two mappings.
		if mappings, err := s.MappingsByApplication(ctx, "app_dc"); err != nil || len(mappings) != 2 {
			t.Fatalf("another application's mappings must survive: err=%v n=%d, want 2", err, len(mappings))
		}
		if rules, err := s.CoResidencyRulesByApplication(ctx, "app_dc"); err != nil || len(rules) != 1 {
			t.Fatalf("app_dc's own pair must survive app_dc2's delete: err=%v rules=%+v", err, rules)
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
