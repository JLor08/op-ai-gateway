// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestBenchmarkDue(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	app := func(enabled bool, interval int) routing.Application {
		return routing.Application{ID: "app1", BenchmarkScheduleEnabled: enabled, BenchmarkScheduleIntervalSeconds: interval}
	}
	tests := []struct {
		name           string
		app            routing.Application
		lastRun        map[string]time.Time
		defaultSeconds int
		want           bool
	}{
		{"not enabled never due", app(false, 3600), map[string]time.Time{}, 86400, false},
		{"enabled no last run is due", app(true, 3600), map[string]time.Time{}, 86400, true},
		{"within own interval not due", app(true, 3600), map[string]time.Time{"app1": now.Add(-30 * time.Minute)}, 86400, false},
		{"beyond own interval is due", app(true, 3600), map[string]time.Time{"app1": now.Add(-2 * time.Hour)}, 86400, true},
		{"interval 0 uses default (beyond)", app(true, 0), map[string]time.Time{"app1": now.Add(-2 * time.Hour)}, 3600, true},
		{"interval 0 uses default (within)", app(true, 0), map[string]time.Time{"app1": now.Add(-30 * time.Minute)}, 3600, false},
		{"tiny interval floored to min (within 60s)", app(true, 5), map[string]time.Time{"app1": now.Add(-30 * time.Second)}, 86400, false},
		{"tiny interval floored to min (beyond 60s)", app(true, 5), map[string]time.Time{"app1": now.Add(-90 * time.Second)}, 86400, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkDue(tc.app, tc.lastRun, now, tc.defaultSeconds); got != tc.want {
				t.Fatalf("benchmarkDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// schedTestSeed creates a server + application in mem and returns the structs the
// scheduler operates on directly (Trigger takes them as args). mapStatus/locked
// configure the single mapping.
func schedTestSeed(t *testing.T, mem *routing.MemoryStore, mapStatus string, locked bool) (routing.AIServer, routing.Application) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, BenchmarkScheduleEnabled: true, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: mapStatus, MetricsLocked: locked, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return server, app
}

func TestTriggerScheduledBenchmarkReservesAndRuns(t *testing.T) {
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	server, app := schedTestSeed(t, mem, routing.ServerStatusActive, false)

	// A hanging provider keeps the launched run in-flight so ServerBusy stays true
	// for the assertion; the idle watchdog self-terminates the leaked run shortly.
	srv := &Server{Provider: benchHangingProvider{}, Routes: mem, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}
	srv.streamIdleTimeout = time.Second

	if got := srv.TriggerScheduledBenchmark(ctx, server, app); !got {
		t.Fatalf("TriggerScheduledBenchmark = false, want true (a run should be launched)")
	}
	if !srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("ServerBusy = false after launch, want true (run reserved)")
	}
}

func TestTriggerScheduledBenchmarkNoTargetsIsNoOp(t *testing.T) {
	ctx := context.Background()

	// A locked mapping yields no targets.
	memLocked := routing.NewMemoryStore()
	serverL, appL := schedTestSeed(t, memLocked, routing.ServerStatusActive, true)
	srvL := &Server{Provider: benchHangingProvider{}, Routes: memLocked, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}
	if got := srvL.TriggerScheduledBenchmark(ctx, serverL, appL); !got {
		t.Fatalf("locked-only: TriggerScheduledBenchmark = false, want true (no-op)")
	}
	if srvL.Benchmarks.ServerBusy(serverL.ID) {
		t.Fatalf("locked-only: ServerBusy = true, want false (no run reserved)")
	}

	// An inactive mapping also yields no targets.
	memInactive := routing.NewMemoryStore()
	serverI, appI := schedTestSeed(t, memInactive, "inactive", false)
	srvI := &Server{Provider: benchHangingProvider{}, Routes: memInactive, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}
	if got := srvI.TriggerScheduledBenchmark(ctx, serverI, appI); !got {
		t.Fatalf("inactive-only: TriggerScheduledBenchmark = false, want true (no-op)")
	}
	if srvI.Benchmarks.ServerBusy(serverI.ID) {
		t.Fatalf("inactive-only: ServerBusy = true, want false (no run reserved)")
	}
}

// TestRunScheduledBenchmarkPass exercises runScheduledBenchmarkPass directly: the
// server/app enumeration, the "stamp lastRun only when a run was launched" wiring, and
// the stale-entry prune branch (lastRun entries whose app no longer appears this pass).
func TestRunScheduledBenchmarkPass(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()

	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// seedApp creates an application (with the given status + schedule flag) plus one
	// active, unlocked mapping so an enabled app yields a target. Each app takes a
	// distinct port (the memory store enforces port uniqueness per server).
	seedApp := func(id, status string, port int, scheduleEnabled bool) {
		t.Helper()
		app := routing.Application{ID: id, ServerID: "srv1", Type: routing.ProviderMock, Port: port, Scheme: "http", TimeoutMS: 30000, Status: status, BenchmarkScheduleEnabled: scheduleEnabled, CreatedAt: now, UpdatedAt: now}
		if err := mem.CreateApplication(ctx, app); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map-" + id, ApplicationID: id, GatewayModelName: "gw-" + id, AppModelName: "up-" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	seedApp("app-enabled", routing.ServerStatusActive, 8100, true)   // due -> should launch + stamp
	seedApp("app-disabled", routing.ServerStatusActive, 8101, false) // schedule off -> never triggered
	seedApp("app-inactive", "inactive", 8102, true)                  // inactive -> skipped before seen

	// A hanging provider keeps the launched run in-flight so ServerBusy stays true for the
	// assertion; the idle watchdog self-terminates the leaked run shortly (same as the
	// reserve test, so nothing hangs at teardown).
	srv := &Server{Provider: benchHangingProvider{}, Routes: mem, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}
	srv.streamIdleTimeout = time.Second

	// Pre-seed a stale entry for an app that no longer exists; the prune branch (no
	// coverage before this test) must delete it because it is never "seen" this pass.
	lastRun := map[string]time.Time{"ghost": now.Add(-time.Hour)}

	srv.runScheduledBenchmarkPass(ctx, now, lastRun, 3600)

	// 1. The enabled app was launched -> lastRun stamped == now.
	if got, ok := lastRun["app-enabled"]; !ok || !got.Equal(now) {
		t.Fatalf("lastRun[app-enabled] = %v (present=%v), want %v", got, ok, now)
	}
	// The disabled app was never triggered -> no lastRun entry.
	if _, ok := lastRun["app-disabled"]; ok {
		t.Fatalf("lastRun[app-disabled] present, want absent (BenchmarkScheduleEnabled=false)")
	}
	// The inactive app was skipped -> no lastRun entry.
	if _, ok := lastRun["app-inactive"]; ok {
		t.Fatalf("lastRun[app-inactive] present, want absent (inactive app)")
	}
	// The launched run reserved the server.
	if !srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("ServerBusy = false after the pass, want true (enabled app's run reserved)")
	}
	// 2. Stale-prune branch: the ghost entry was deleted (it was not seen this pass).
	if _, ok := lastRun["ghost"]; ok {
		t.Fatalf("lastRun[ghost] still present, want pruned (no matching app enumerated this pass)")
	}

	// 3. A later pass still within the enabled app's effective interval must NOT re-trigger
	//    (benchmarkDue gates before TryStart, so the stamp is left untouched at its old value).
	srv.runScheduledBenchmarkPass(ctx, now.Add(time.Minute), lastRun, 3600)
	if got := lastRun["app-enabled"]; !got.Equal(now) {
		t.Fatalf("lastRun[app-enabled] = %v after an in-interval pass, want unchanged %v (not due)", got, now)
	}
}

// TestRunScheduledBenchmarkPassColocatedAppsDoNotStarve proves the fix for the
// co-located-apps starvation bug: two schedule-enabled apps on the SAME server share
// the one-run-per-server slot. On a pass where both are due, only the first-enumerated
// (lowest id) app can reserve the server; the second app's TryStart returns !ok and MUST
// defer (return false => lastRun NOT stamped) so it retries once the slot frees. If it
// were stamped in lockstep with the winner it would never be benchmarked.
func TestRunScheduledBenchmarkPassColocatedAppsDoNotStarve(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()

	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// seedApp creates an active, schedule-enabled application on the shared server (a
	// distinct port each — the memory store enforces port uniqueness) plus one active,
	// unlocked mapping so the app yields a benchmark target.
	seedApp := func(id string, port int) {
		t.Helper()
		app := routing.Application{ID: id, ServerID: "srv1", Type: routing.ProviderMock, Port: port, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, BenchmarkScheduleEnabled: true, CreatedAt: now, UpdatedAt: now}
		if err := mem.CreateApplication(ctx, app); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map-" + id, ApplicationID: id, GatewayModelName: "gw-" + id, AppModelName: "up-" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	// Two co-located apps; ApplicationsByServer sorts by id, so app-aaa is enumerated first.
	seedApp("app-aaa", 8100)
	seedApp("app-bbb", 8101)

	// A hanging provider keeps the launched run in-flight so ServerBusy stays true for the
	// assertions; the idle watchdog self-terminates the leaked run shortly.
	srv := &Server{Provider: benchHangingProvider{}, Routes: mem, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}
	srv.streamIdleTimeout = time.Second

	lastRun := map[string]time.Time{}

	// Pass 1: app-aaa reserves the server and launches (stamped); app-bbb's TryStart returns
	// !ok because the slot is busy -> it defers and is NOT stamped.
	srv.runScheduledBenchmarkPass(ctx, now, lastRun, 86400)
	if got, ok := lastRun["app-aaa"]; !ok || !got.Equal(now) {
		t.Fatalf("pass 1: lastRun[app-aaa] = %v (present=%v), want %v (launched + stamped)", got, ok, now)
	}
	if _, ok := lastRun["app-bbb"]; ok {
		t.Fatalf("pass 1: lastRun[app-bbb] present, want absent (TryStart !ok must defer, not stamp)")
	}
	if !srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("pass 1: ServerBusy = false, want true (app-aaa's run reserved the server)")
	}

	// The winner's (quick) run completes, freeing the slot.
	srv.Benchmarks.Release(server.ID)

	// Pass 2 at the SAME now: app-bbb is still due (it was never stamped) and now the slot is
	// free, so it launches and is finally stamped. app-aaa stays stamped at now (not due).
	srv.runScheduledBenchmarkPass(ctx, now, lastRun, 86400)
	if got, ok := lastRun["app-bbb"]; !ok || !got.Equal(now) {
		t.Fatalf("pass 2: lastRun[app-bbb] = %v (present=%v), want %v (launched + stamped once slot freed)", got, ok, now)
	}
	if !srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("pass 2: ServerBusy = false, want true (app-bbb's run reserved the server)")
	}
}

func TestTriggerScheduledBenchmarkIdleGateDefers(t *testing.T) {
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	server, app := schedTestSeed(t, mem, routing.ServerStatusActive, false)

	active := newActiveRegistry(nil)
	active.Add(ActiveRequest{ID: "r1", ServerName: server.Name, ServerID: server.ID})

	srv := &Server{Provider: benchHangingProvider{}, Routes: mem, Benchmarks: NewBenchmarkRegistry(), Active: active}

	if got := srv.TriggerScheduledBenchmark(ctx, server, app); got {
		t.Fatalf("TriggerScheduledBenchmark = true, want false (idle-gate should defer while traffic is in-flight)")
	}
	if srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("ServerBusy = true after idle-gate defer, want false (reservation released)")
	}
}

// schedFlavorFixture seeds a server with two applications for the images-only
// skip:
//
//   - app-sd, an external stable-diffusion.cpp application whose flavors are
//     [openai_images] alone, with one mapping (sd-ext);
//   - app-agent, a server_agent application declaring both a text flavor and
//     openai_images, with four children: agent-text (spec [openai]),
//     agent-sd (spec [openai_images]), agent-nospec (no spec: the
//     application's flavors stand) and agent-legacy (a spec stored as []).
//
// It returns the server and both applications.
func schedFlavorFixture(t *testing.T, mem *routing.MemoryStore) (routing.AIServer, routing.Application, routing.Application) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	server := routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	sdApp := routing.Application{ID: "app-sd", ServerID: "srv1", Type: routing.ProviderStableDiffusionCpp, Port: 7860, Scheme: "http", TimeoutMS: 600000, APIFlavors: []string{routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, BenchmarkScheduleEnabled: true, CreatedAt: now, UpdatedAt: now}
	agentApp := routing.Application{ID: "app-agent", ServerID: "srv1", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", TimeoutMS: 600000, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, BenchmarkScheduleEnabled: true, CreatedAt: now, UpdatedAt: now}
	for _, app := range []routing.Application{sdApp, agentApp} {
		if err := mem.CreateApplication(ctx, app); err != nil {
			t.Fatalf("CreateApplication %s: %v", app.ID, err)
		}
	}
	mapping := func(appID, name string) {
		t.Helper()
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map-" + name, ApplicationID: appID, GatewayModelName: name, AppModelName: name, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", name, err)
		}
	}
	spec := func(name string, flavors []string) {
		t.Helper()
		if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-" + name, MappingID: "map-" + name, Binary: "/opt/bin/server", Args: "[]", Env: "{}", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", name, err)
		}
	}
	mapping("app-sd", "sd-ext")
	for _, name := range []string{"agent-text", "agent-sd", "agent-nospec", "agent-legacy"} {
		mapping("app-agent", name)
	}
	spec("agent-text", []string{routing.APIFlavorOpenAI})
	spec("agent-sd", []string{routing.APIFlavorOpenAIImages})
	spec("agent-legacy", []string{})
	return server, sdApp, agentApp
}

// A scheduled benchmark sends a chat prompt, which a mapping that serves only
// images cannot answer: every scheduled run of it would fail. Such a mapping
// is skipped, judged by its EFFECTIVE flavors with the resolver's precedence
// (a server_agent child's spec wins, even an empty one; without a spec the
// application's flavors stand), and every text-capable mapping beside it is
// still benchmarked. An external stable-diffusion.cpp application has nothing
// to benchmark at all, so no run is reserved for it.
func TestTriggerScheduledBenchmarkSkipsImagesOnlyMappings(t *testing.T) {
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	server, sdApp, agentApp := schedFlavorFixture(t, mem)
	prov := newColdLister(nil)
	srv := &Server{Provider: prov, Routes: mem, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}

	if got := srv.TriggerScheduledBenchmark(ctx, server, sdApp); !got {
		t.Fatalf("images-only application: TriggerScheduledBenchmark = false, want true (nothing to do)")
	}
	if srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("images-only application: ServerBusy = true, want false (no run reserved)")
	}
	if got := prov.streamedModels(); len(got) != 0 {
		t.Fatalf("images-only application: streamed %v, want no chat prompt at all", got)
	}

	if got := srv.TriggerScheduledBenchmark(ctx, server, agentApp); !got {
		t.Fatalf("agent application: TriggerScheduledBenchmark = false, want true (a run should be launched)")
	}
	waitFor(t, func() bool { return !srv.Benchmarks.Status(server.ID).Running })
	status := srv.Benchmarks.Status(server.ID)
	var benchmarked []string
	for _, res := range status.Results {
		benchmarked = append(benchmarked, res.GatewayModelName)
	}
	sort.Strings(benchmarked)
	if want := []string{"agent-legacy", "agent-nospec", "agent-text"}; !reflect.DeepEqual(benchmarked, want) {
		t.Fatalf("benchmarked mappings = %v, want %v (agent-sd serves images only)", benchmarked, want)
	}
	for _, model := range prov.streamedModels() {
		if model == "agent-sd" {
			t.Fatalf("streamed a chat prompt to agent-sd, an images-only child: %v", prov.streamedModels())
		}
	}
}

// specReadFailingStore fails every runtime-spec read by mapping.
type specReadFailingStore struct {
	*routing.MemoryStore
}

func (specReadFailingStore) RuntimeSpecByMapping(context.Context, string) (routing.RuntimeSpec, bool, error) {
	return routing.RuntimeSpec{}, false, errors.New("spec read down")
}

// A server_agent child whose spec cannot be read is skipped for this pass,
// not benchmarked as though the application's flavors stood: the spec may
// narrow it to images only. An ordinary application reads no spec, so its
// mappings are benchmarked as before.
func TestTriggerScheduledBenchmarkSkipsAMappingWhoseSpecReadFails(t *testing.T) {
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	server, _, agentApp := schedFlavorFixture(t, mem)
	srv := &Server{Provider: newColdLister(nil), Routes: specReadFailingStore{mem}, Benchmarks: NewBenchmarkRegistry(), Active: newActiveRegistry(nil)}

	if got := srv.TriggerScheduledBenchmark(ctx, server, agentApp); !got {
		t.Fatalf("TriggerScheduledBenchmark = false, want true (nothing to do)")
	}
	if srv.Benchmarks.ServerBusy(server.ID) {
		t.Fatalf("ServerBusy = true, want false: no child could be classified, so no run is reserved")
	}
}
