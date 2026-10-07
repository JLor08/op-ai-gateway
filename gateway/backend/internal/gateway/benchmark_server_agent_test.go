// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// portalAgentSpecID is the launch spec of the portal-shaped agent fixture's
// one mapping.
const portalAgentSpecID = "rs_map1"

// portalAgentAllFeatures is what a current agent declares: the runtime
// manager, the router's /upstream/{model}/props passthrough and the agent's
// own per-spec context probe.
var portalAgentAllFeatures = []string{"runtime_manager", RuntimeUpstreamPropsFeature, runtimeModelProbeFeature}

// portalAgentOpts shapes one portal-created server_agent application behind
// the fake agent router.
type portalAgentOpts struct {
	// features is the agent's declared feature set.
	features []string
	// running is what the router's /running lists at the start.
	running []string
	// loadedModelsPath is an application value set through the API; "" is
	// the portal form's body.
	loadedModelsPath string
	// statuses is the agent's last runtime-status snapshot for the server.
	statuses []RuntimeStatusDTO
}

// portalAgentFixture is a server_agent application created through the
// portal service with the form's body, one mapping (gw-qwen -> qwen) with an
// enabled llama_cpp spec that carries its own API token, and the production
// provider shape pointed at the fake agent router.
type portalAgentFixture struct {
	srv    *Server
	mem    *routing.MemoryStore
	router *fakeAgentRouter
	target benchmarkTarget
}

func newPortalAgentFixture(t *testing.T, opts portalAgentOpts) *portalAgentFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	router := newFakeAgentRouter(t, fakeAgentRouterOpts{
		models: []string{"qwen"}, running: opts.running, coldDelay: 400 * time.Millisecond, nCtx: 32768,
	})
	mem := routing.NewMemoryStore()
	server := routing.AIServer{ID: "srv1", Name: "Host", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	var endpoint routing.Application
	router.point(&server, &endpoint)
	must("CreateAIServer", mem.CreateAIServer(ctx, server))

	svc := portal.NewService(portal.ServiceDeps{Routes: mem, Clock: func() time.Time { return now }})
	// The body ApplicationSection's buildBody sends for server_agent: the
	// three gateway-side probe fields go out empty.
	dto, err := svc.CreateApplication(ctx, auth.Token{Scopes: []string{"system"}}, server.ID, portal.CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: endpoint.Port, Scheme: endpoint.Scheme,
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, TimeoutMS: 600000,
		LoadedModelsPath: opts.loadedModelsPath, LoadedModelsFormat: "", ContextProbePath: "",
	})
	must("CreateApplication", err)
	app, err := mem.ApplicationByID(ctx, dto.ID)
	must("ApplicationByID", err)

	mapping := routing.ModelMapping{ID: "map1", ApplicationID: app.ID, GatewayModelName: "gw-qwen", AppModelName: "qwen", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must("CreateMapping", mem.CreateMapping(ctx, mapping))
	must("UpsertRuntimeSpec", mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: portalAgentSpecID, MappingID: mapping.ID, Enabled: true, Type: string(routing.RuntimeSpecTypeLlamaCpp),
		Binary: "/usr/local/bin/llama-server", Args: "[]", Env: "{}", HealthPath: "/health",
		HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
		APITokenMode: string(routing.RuntimeAPITokenModeSet), APIToken: "plain:spec-token",
		CreatedAt: now, UpdatedAt: now,
	}))
	spec, hasSpec, err := mem.RuntimeSpecByMapping(ctx, mapping.ID)
	must("RuntimeSpecByMapping", err)

	srv := &Server{
		Provider:      newFakeAgentProvider(),
		Routes:        mem,
		Portal:        svc,
		Benchmarks:    NewBenchmarkRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
		AgentFeatures: NewAgentFeaturesRegistry(),
	}
	srv.AgentFeatures.Set(server.ID, opts.features)
	if opts.statuses != nil {
		srv.RuntimeStatus.publish(server.ID, opts.statuses)
	}
	return &portalAgentFixture{
		srv: srv, mem: mem, router: router,
		target: srv.benchmarkTargetFor(ctx, server, app, mapping, spec, hasSpec),
	}
}

// speed runs one speed benchmark over the fixture's mapping, the way the
// manual endpoint and the scheduler do, and returns its history row.
func (f *portalAgentFixture) speed(t *testing.T) routing.BenchmarkRun {
	t.Helper()
	ctx := context.Background()
	run, ok := f.srv.Benchmarks.TryStart("srv1", "owner", "speed", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{f.target}, "speed")
	rows, err := f.mem.BenchmarkRunsByMapping(ctx, "map1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("BenchmarkRunsByMapping = %d rows, %v; want one", len(rows), err)
	}
	if rows[0].Error != "" {
		t.Fatalf("history row error = %q, want none", rows[0].Error)
	}
	return rows[0]
}

// requestIndexes lists, in order, the indexes of the recorded requests with
// this method and path.
func requestIndexes(reqs []fakeAgentRequest, method, path string) []int {
	var out []int
	for i, r := range reqs {
		if r.method == method && r.path == path {
			out = append(out, i)
		}
	}
	return out
}

// firstRequest is the index of the first recorded request with this method
// and path, or -1.
func firstRequest(reqs []fakeAgentRequest, method, path string) int {
	if idx := requestIndexes(reqs, method, path); len(idx) > 0 {
		return idx[0]
	}
	return -1
}

// requestsWithPrefix counts the recorded requests whose path starts with prefix.
func requestsWithPrefix(reqs []fakeAgentRequest, prefix string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r.path, prefix) {
			n++
		}
	}
	return n
}

func (f *portalAgentFixture) storedContextSize(t *testing.T) int {
	t.Helper()
	m, err := f.mem.MappingByID(context.Background(), "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	return m.ContextSize
}

func TestSpeedBenchmarkPortalShapedAgentAppRecordsLoadTimeAndContext(t *testing.T) {
	runningRow := func(size int, probe string) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: portalAgentSpecID, Model: "qwen", State: "running", ContextSize: size, ContextProbe: probe}
	}

	t.Run("cold at the start: a load time and the router's context size", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: portalAgentAllFeatures,
			statuses: []RuntimeStatusDTO{{SpecID: portalAgentSpecID, Model: "qwen", State: "stopped"}},
		})
		row := f.speed(t)
		if row.LoadTimeMS < 250 {
			t.Errorf("LoadTimeMS = %d, want >= 250 (the router's cold start is 400 ms)", row.LoadTimeMS)
		}
		if row.ContextSize != 32768 {
			t.Errorf("ContextSize = %d, want 32768 from /upstream/qwen/props", row.ContextSize)
		}
		reqs := f.router.requests()
		running := firstRequest(reqs, "GET", "/running")
		chats := requestIndexes(reqs, "POST", "/v1/chat/completions")
		props := firstRequest(reqs, "GET", "/upstream/qwen/props")
		if len(chats) != 2 {
			t.Fatalf("chat streams = %d, want 2 (cold and warm)", len(chats))
		}
		if running < 0 || running > chats[0] {
			t.Errorf("GET /running at %d, cold pass at %d: the cold check must ask /running first", running, chats[0])
		}
		if props >= 0 && props < chats[1] {
			t.Errorf("GET /upstream/qwen/props at %d, warm pass at %d: the props probe must follow the warm pass", props, chats[1])
		}
		if props >= 0 && reqs[props].auth != "Bearer spec-token" {
			t.Errorf("props Authorization = %q, want the spec's token", reqs[props].auth)
		}
		if got := f.storedContextSize(t); got != 32768 {
			t.Errorf("mapping context_size = %d, want 32768 (a probe answer is written to the mapping)", got)
		}
	})

	t.Run("both sources answer: the router's props come first", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: portalAgentAllFeatures,
			running:  []string{"qwen"},
			statuses: []RuntimeStatusDTO{runningRow(8192, "ok")},
		})
		start := time.Now()
		row := f.speed(t)
		elapsed := time.Since(start)
		if row.ContextSize != 32768 {
			t.Errorf("ContextSize = %d, want 32768 from /upstream/qwen/props, not the agent's 8192", row.ContextSize)
		}
		if got := f.storedContextSize(t); got != 32768 {
			t.Errorf("mapping context_size = %d, want 32768 (a probe answer is written to the mapping)", got)
		}
		if elapsed >= 3*time.Second {
			t.Errorf("speed run took %v, want under 3 s: a probe answer never waits for the agent's telemetry (up to %v)", elapsed, benchmarkTelemetryContextWait)
		}
	})

	t.Run("without runtime_upstream_props: the agent's telemetry", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: []string{"runtime_manager", runtimeModelProbeFeature},
			running:  []string{"qwen"},
			statuses: []RuntimeStatusDTO{runningRow(8192, "ok")},
		})
		row := f.speed(t)
		if row.ContextSize != 8192 {
			t.Errorf("ContextSize = %d, want 8192 from the agent's status row", row.ContextSize)
		}
		if n := requestsWithPrefix(f.router.requests(), "/upstream/"); n != 0 {
			t.Errorf("/upstream requests = %d, want 0 without runtime_upstream_props", n)
		}
		if got := f.storedContextSize(t); got != 0 {
			t.Errorf("mapping context_size = %d, want 0: a telemetry value is never written by the benchmark", got)
		}
	})

	t.Run("without both features: no context size", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: []string{"runtime_manager"},
			running:  []string{"qwen"},
			statuses: []RuntimeStatusDTO{runningRow(8192, "ok")},
		})
		row := f.speed(t)
		if row.ContextSize != 0 {
			t.Errorf("ContextSize = %d, want 0 without runtime_upstream_props and runtime_model_probe", row.ContextSize)
		}
		if n := requestsWithPrefix(f.router.requests(), "/upstream/"); n != 0 {
			t.Errorf("/upstream requests = %d, want 0 without runtime_upstream_props", n)
		}
	})

	t.Run("an API-set loaded_models_path is the path probed", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{features: portalAgentAllFeatures, loadedModelsPath: "/custom-running"})
		f.speed(t)
		reqs := f.router.requests()
		if firstRequest(reqs, "GET", "/custom-running") < 0 {
			t.Error("GET /custom-running was never requested: an application value set through the API must win")
		}
		if firstRequest(reqs, "GET", "/running") >= 0 {
			t.Error("GET /running was requested although the application sets its own loaded-models path")
		}
	})

	t.Run("a starting row the router does not list yet: no load time", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: portalAgentAllFeatures,
			statuses: []RuntimeStatusDTO{{SpecID: portalAgentSpecID, Model: "qwen", State: "starting"}},
		})
		row := f.speed(t)
		if row.LoadTimeMS != 0 {
			t.Errorf("LoadTimeMS = %d, want 0: a starting child is resident, so the cold start is unconfirmed", row.LoadTimeMS)
		}
		if row.GenTokensPerSecond <= 0 {
			t.Errorf("GenTokensPerSecond = %v, want a rate (throughput is measured regardless)", row.GenTokensPerSecond)
		}
	})

	t.Run("a backoff row the router does not list: no load time", func(t *testing.T) {
		f := newPortalAgentFixture(t, portalAgentOpts{
			features: portalAgentAllFeatures,
			statuses: []RuntimeStatusDTO{{SpecID: portalAgentSpecID, Model: "qwen", State: "backoff"}},
		})
		row := f.speed(t)
		if row.LoadTimeMS != 0 {
			t.Errorf("LoadTimeMS = %d, want 0: a request for a spec in backoff waits for its backoff timer, so the cold start is unconfirmed", row.LoadTimeMS)
		}
		if row.GenTokensPerSecond <= 0 {
			t.Errorf("GenTokensPerSecond = %v, want a rate (throughput is measured regardless)", row.GenTokensPerSecond)
		}
	})
}

func TestRunContextProbeServerAgentUsesTheRouterProps(t *testing.T) {
	f := newPortalAgentFixture(t, portalAgentOpts{features: portalAgentAllFeatures})
	run, ok := f.srv.Benchmarks.TryStart("srv1", "context-probe", "context", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runContextProbe(context.Background(), run, "srv1", f.target)

	st := f.srv.Benchmarks.Status("srv1")
	if len(st.Results) != 1 || st.Results[0].Error != "" {
		t.Fatalf("status results = %+v, want one successful result", st.Results)
	}
	if st.Results[0].ContextSize != 32768 {
		t.Errorf("ContextSize = %d, want 32768 from /upstream/qwen/props", st.Results[0].ContextSize)
	}
	reqs := f.router.requests()
	props := firstRequest(reqs, "GET", "/upstream/qwen/props")
	if props < 0 {
		t.Fatal("GET /upstream/qwen/props was never requested")
	}
	if reqs[props].auth != "Bearer spec-token" {
		t.Errorf("props Authorization = %q, want the spec's token", reqs[props].auth)
	}
	if got := f.storedContextSize(t); got != 0 {
		t.Errorf("mapping context_size = %d, want 0: the context probe reports and never writes", got)
	}
}
