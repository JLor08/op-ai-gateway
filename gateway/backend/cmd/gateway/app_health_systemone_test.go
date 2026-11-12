// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/routing"
	"strconv"
	"sync"
	"testing"
	"time"
)

// systemOneRequest is one request the fake System One upstream saw.
type systemOneRequest struct {
	method, path, model, authorization string
}

// systemOneUpstream is an httptest stand-in for a System One server -- an
// external application, or the agent's router in front of its children. It
// records EVERY request it receives (whatever the path), so a test can assert
// that none reached it at all, and answers POST /v1/systemone with status and
// body.
type systemOneUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	seen   []systemOneRequest
	status int
	body   string
}

func newSystemOneUpstream(t *testing.T) *systemOneUpstream {
	t.Helper()
	u := &systemOneUpstream{status: http.StatusOK, body: `{"answers":{"probe":{"type":"noul","noul":0.9}}}`}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &req)
		u.mu.Lock()
		u.seen = append(u.seen, systemOneRequest{method: r.Method, path: r.URL.Path, model: req.Model, authorization: r.Header.Get("Authorization")})
		status, body := u.status, u.body
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *systemOneUpstream) answer(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.body = status, body
}

func (u *systemOneUpstream) requests() []systemOneRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]systemOneRequest(nil), u.seen...)
}

// hostPort splits the upstream's URL into the server domain and the
// application port the runner builds ApplicationEndpoint from.
func (u *systemOneUpstream) hostPort(t *testing.T) (string, int) {
	t.Helper()
	parsed, err := url.Parse(u.srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	return parsed.Hostname(), port
}

// systemOneApp is an application on the fake upstream declaring decisions.
// always_reachable, so the health probe sends nothing: the only requests the
// upstream can see are the System One probe's.
func systemOneApp(appType string, port int, flavors ...string) routing.Application {
	return routing.Application{
		ID: "a1", ServerID: "s1", Type: appType, Port: port, Scheme: "http",
		Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
		APIFlavors: flavors,
	}
}

func systemOneMapping(id, appModel string) routing.ModelMapping {
	return routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: appModel, Status: routing.ServerStatusActive}
}

// systemOneRunner builds a runner over the REAL provider chain
// (providerClients' Multiplexer and its OpenAI-compatible client) against the
// fake upstream, so a test also fails when the Multiplexer stops routing the
// probe or the pass's type assertion stops matching it.
func systemOneRunner(t *testing.T, up *systemOneUpstream, app routing.Application, mappings ...routing.ModelMapping) (*appHealthRunner, *fakeHealthStore, *gateway.LoadedModelRegistry) {
	t.Helper()
	shrinkRetryGap(t)
	host, _ := up.hostPort(t)
	st := &fakeHealthStore{
		servers:  []routing.AIServer{{ID: "s1", Domain: host, Provider: app.Type, Status: routing.ServerStatusActive}},
		apps:     map[string][]routing.Application{"s1": {app}},
		mappings: map[string][]routing.ModelMapping{"a1": mappings},
	}
	loaded := gateway.NewLoadedModelRegistry()
	runner := &appHealthRunner{
		store: st, prober: providerClients(0, false, nil), registry: gateway.NewAppHealthRegistry(nil),
		loaded: loaded, settings: st, probeTimeout: time.Second, now: time.Now,
	}
	return runner, st, loaded
}

// TestSystemOnePassWritesYesForAnExternalApp is the pass's whole job for an
// external application: one tiny POST /v1/systemone naming the mapping's own
// model, with the application's credential, and the answer becomes the
// mapping's decision verdict under source systemone_probe.
func TestSystemOnePassWritesYesForAnExternalApp(t *testing.T) {
	up := newSystemOneUpstream(t)
	_, port := up.hostPort(t)
	app := systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions)
	app.APIToken = "plain:app-tok"
	runner, st, _ := systemOneRunner(t, up, app, systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	want := systemOneRequest{method: http.MethodPost, path: "/v1/systemone", model: "decider", authorization: "Bearer app-tok"}
	if reqs[0] != want {
		t.Fatalf("request = %+v, want %+v", reqs[0], want)
	}
}

// TestSystemOnePassWritesNoOnA404: the 404 of a server without the route is a
// real no, and only this probe may establish it.
func TestSystemOnePassWritesNoOnA404(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answer(http.StatusNotFound, `{"error":"not found"}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorOpenAI, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityNo, routing.CapabilitySourceSystemOneProbe)
}

// TestSystemOnePassSkipsAMappingWithoutTheFlavor: only a mapping whose
// effective flavors list decisions is probed. An application that does not
// declare decisions is never asked, and a server_agent mapping whose runtime
// spec narrows its flavors to openai is not either, although its application
// declares decisions and the model is loaded.
func TestSystemOnePassSkipsAMappingWithoutTheFlavor(t *testing.T) {
	t.Run("external application without decisions", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		_, port := up.hostPort(t)
		runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorOpenAI), systemOneMapping("m1", "chat"))

		runCapabilityCycle(runner)

		if reqs := up.requests(); len(reqs) != 0 {
			t.Fatalf("upstream saw %+v, want no request", reqs)
		}
		if _, ok := st.capabilityOf("m1", routing.CapabilityDecision); ok {
			t.Fatal("m1 got a decision row without the decisions flavor")
		}
	})
	t.Run("agent spec narrowed away from decisions", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		_, port := up.hostPort(t)
		runner, st, loaded := systemOneRunner(t, up,
			systemOneApp(routing.ProviderServerAgent, port, routing.APIFlavorOpenAI, routing.APIFlavorDecisions),
			systemOneMapping("m1", "chat"), systemOneMapping("m2", "decider"))
		st.runtimeSpecs = []routing.RuntimeSpec{
			{MappingID: "m1", Binary: "llama-server", APIFlavors: []string{routing.APIFlavorOpenAI}},
			{MappingID: "m2", Binary: "llama-server", APIFlavors: []string{routing.APIFlavorDecisions}},
		}
		loaded.SetGatewayProbe("a1", []string{"chat", "decider"})

		runCapabilityCycle(runner)

		reqs := up.requests()
		if len(reqs) != 1 || reqs[0].model != "decider" {
			t.Fatalf("upstream saw %+v, want exactly one probe of decider", reqs)
		}
		if _, ok := st.capabilityOf("m1", routing.CapabilityDecision); ok {
			t.Fatal("m1 (spec flavors [openai]) got a decision row")
		}
		st.assertCapability(t, "m2", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
	})
}

// agentSystemOneRunner is one server_agent application declaring decisions,
// with one decisions mapping (m1, model "decider") and its runtime spec.
func agentSystemOneRunner(t *testing.T, up *systemOneUpstream) (*appHealthRunner, *fakeHealthStore, *gateway.LoadedModelRegistry) {
	t.Helper()
	_, port := up.hostPort(t)
	runner, st, loaded := systemOneRunner(t, up,
		systemOneApp(routing.ProviderServerAgent, port, routing.APIFlavorDecisions),
		systemOneMapping("m1", "decider"))
	st.runtimeSpecs = []routing.RuntimeSpec{{
		MappingID: "m1", Binary: "llama-server", Args: `["-m","decider.gguf"]`,
		APIFlavors: []string{routing.APIFlavorDecisions}, APITokenMode: "set", APIToken: "plain:spec-tok",
	}}
	return runner, st, loaded
}

// TestSystemOnePassNeverProbesAnUnloadedAgentModel: the agent's router starts
// a child for any request that names it, and every request refreshes its idle
// timer. So a model the agent does not report loaded is never asked -- not one
// request reaches the router, neither with nothing loaded nor with only a
// sibling loaded -- and neither is a model that unloaded since an earlier
// tick, whose first probe was inconclusive.
func TestSystemOnePassNeverProbesAnUnloadedAgentModel(t *testing.T) {
	t.Run("nothing loaded", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		runner, st, _ := agentSystemOneRunner(t, up)

		runCapabilityCycle(runner)

		if reqs := up.requests(); len(reqs) != 0 {
			t.Fatalf("the router saw %+v, want no request at all", reqs)
		}
		if n := st.capabilityWriteCount(); n != 0 {
			t.Fatalf("UpsertMappingCapabilities called %d times, want 0", n)
		}
	})
	t.Run("another model loaded", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		runner, _, loaded := agentSystemOneRunner(t, up)
		loaded.SetGatewayProbe("a1", []string{"someone-else"})

		runCapabilityCycle(runner)

		if reqs := up.requests(); len(reqs) != 0 {
			t.Fatalf("the router saw %+v, want no request at all", reqs)
		}
	})
	t.Run("unloaded between ticks", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		up.answer(http.StatusServiceUnavailable, `{"error":"loading"}`)
		runner, st, loaded := agentSystemOneRunner(t, up)
		current := time.Unix(10_000, 0).UTC()
		runner.now = func() time.Time { return current }
		loaded.SetGatewayProbe("a1", []string{"decider"})

		runCapabilityCycle(runner)
		if n := len(up.requests()); n != 1 {
			t.Fatalf("first tick: router saw %d requests, want 1", n)
		}

		// The model unloads; long after any back-off the pass runs again.
		loaded.SetGatewayProbe("a1", nil)
		up.answer(http.StatusOK, `{"answers":{"probe":{"type":"noul","noul":0.9}}}`)
		current = current.Add(24 * time.Hour)
		runCapabilityCycle(runner)

		if n := len(up.requests()); n != 1 {
			t.Fatalf("after the unload: router saw %d requests, want still 1", n)
		}
		if _, ok := st.capabilityOf("m1", routing.CapabilityDecision); ok {
			t.Fatal("an unloaded model got a decision row")
		}
	})
}

// TestSystemOnePassProbesAnUnloadedAgentModelOnceLoaded: the model a tick
// skipped because it was not loaded is probed by the first tick that finds it
// loaded, through the router, naming the mapping's model, with the runtime
// spec's own credential.
func TestSystemOnePassProbesAnUnloadedAgentModelOnceLoaded(t *testing.T) {
	up := newSystemOneUpstream(t)
	runner, st, loaded := agentSystemOneRunner(t, up)

	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 0 {
		t.Fatalf("unloaded: router saw %d requests, want 0", n)
	}

	loaded.SetGatewayProbe("a1", []string{"decider"})
	runCapabilityCycle(runner)

	reqs := up.requests()
	want := systemOneRequest{method: http.MethodPost, path: "/v1/systemone", model: "decider", authorization: "Bearer spec-tok"}
	if len(reqs) != 1 || reqs[0] != want {
		t.Fatalf("router saw %+v, want exactly [%+v]", reqs, want)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
}

// TestSystemOnePassProbesAnIdentityOnce: a settled verdict is not asked again
// -- two passes cost one request -- until what is behind the mapping changes:
// another app model name for an external application, another launch spec for
// an agent-launched one.
func TestSystemOnePassProbesAnIdentityOnce(t *testing.T) {
	t.Run("external application", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		_, port := up.hostPort(t)
		runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

		runCapabilityCycle(runner)
		runCapabilityCycle(runner)
		if n := len(up.requests()); n != 1 {
			t.Fatalf("two passes sent %d requests, want 1", n)
		}

		st.mu.Lock()
		st.mappings["a1"] = []routing.ModelMapping{systemOneMapping("m1", "decider-v2")}
		st.mu.Unlock()
		runCapabilityCycle(runner)
		reqs := up.requests()
		if len(reqs) != 2 || reqs[1].model != "decider-v2" {
			t.Fatalf("after the rename the upstream saw %+v, want a second probe of decider-v2", reqs)
		}
	})
	t.Run("agent-launched application", func(t *testing.T) {
		up := newSystemOneUpstream(t)
		runner, st, loaded := agentSystemOneRunner(t, up)
		loaded.SetGatewayProbe("a1", []string{"decider"})

		runCapabilityCycle(runner)
		runCapabilityCycle(runner)
		if n := len(up.requests()); n != 1 {
			t.Fatalf("two passes sent %d requests, want 1", n)
		}

		st.mu.Lock()
		st.runtimeSpecs[0].Args = `["-m","decider-v2.gguf"]`
		st.mu.Unlock()
		runCapabilityCycle(runner)
		if n := len(up.requests()); n != 2 {
			t.Fatalf("after a spec change the router saw %d requests, want 2", n)
		}
	})
}

// TestSystemOnePassBacksOffOnATransientResult: an answer that states no
// verdict (here a 503 while the model loads) writes nothing and is not asked
// again on every tick, only once the back-off has passed; then the verdict
// lands.
func TestSystemOnePassBacksOffOnATransientResult(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answer(http.StatusServiceUnavailable, `{"error":"loading"}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }

	runCapabilityCycle(runner)
	if _, ok := st.capabilityOf("m1", routing.CapabilityDecision); ok {
		t.Fatal("a 503 wrote a decision row")
	}

	up.answer(http.StatusOK, `{"answers":{"probe":{"type":"noul","noul":0.9}}}`)
	current = current.Add(time.Minute)
	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 1 {
		t.Fatalf("inside the back-off the upstream saw %d requests, want still 1", n)
	}

	current = current.Add(systemOneRetryBackoff)
	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 2 {
		t.Fatalf("after the back-off the upstream saw %d requests, want 2", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
}

// TestSystemOnePassDoesNotOverwriteAManualYes: an operator's yes outranks the
// probe, so even the probe's no leaves it on file, through the shared
// precedence rule.
func TestSystemOnePassDoesNotOverwriteAManualYes(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answer(http.StatusNotFound, `{"error":"not found"}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	st.seedCapability("m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceManual)

	runCapabilityCycle(runner)

	if n := len(up.requests()); n != 1 {
		t.Fatalf("upstream saw %d requests, want 1", n)
	}
	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 0", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceManual)
}

// TestSystemOnePassRetriesAVerdictTheStoreDidNotTake: a verdict is settled only
// once it is on file. A capability read that fails leaves the probe unsettled
// (backed off, not asked again on every tick), and the next probe after the
// back-off lands it.
func TestSystemOnePassRetriesAVerdictTheStoreDidNotTake(t *testing.T) {
	up := newSystemOneUpstream(t)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	st.capsReadErr = true

	runCapabilityCycle(runner)
	current = current.Add(time.Minute)
	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 1 {
		t.Fatalf("inside the back-off the upstream saw %d requests, want 1", n)
	}

	st.mu.Lock()
	st.capsReadErr = false
	st.mu.Unlock()
	current = current.Add(systemOneRetryBackoff)
	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 2 {
		t.Fatalf("after the back-off the upstream saw %d requests, want 2", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
}

// systemOneFakeProber records the target and model of every System One probe
// and answers yes. It embeds fakeProber for the loop's other probes.
type systemOneFakeProber struct {
	*fakeProber
	soMu      sync.Mutex
	soTargets []routing.Target
	soModels  []string
}

func (f *systemOneFakeProber) ProbeSystemOne(_ context.Context, target routing.Target, model string) (string, error) {
	f.soMu.Lock()
	defer f.soMu.Unlock()
	f.soTargets = append(f.soTargets, target)
	f.soModels = append(f.soModels, model)
	return routing.CapabilityYes, nil
}

// TestSystemOnePassUsesItsOwnProbeTimeout: a System One answer is a real
// inference, so the probe target carries systemOneProbeTimeout and not the
// health loop's short probe timeout (which a slow model would never beat,
// leaving the verdict unsettled and an agent child's idle timer refreshed on
// every retry).
func TestSystemOnePassUsesItsOwnProbeTimeout(t *testing.T) {
	shrinkRetryGap(t)
	app := systemOneApp(routing.ProviderLlamaCPP, 8001, routing.APIFlavorDecisions)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{"a1": {systemOneMapping("m1", "decider")}}
	prober := &systemOneFakeProber{fakeProber: newFakeProber()}
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: 3 * time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	prober.soMu.Lock()
	defer prober.soMu.Unlock()
	if len(prober.soTargets) != 1 {
		t.Fatalf("probes = %+v, want exactly one", prober.soTargets)
	}
	got := prober.soTargets[0]
	if got.Provider != routing.ProviderLlamaCPP || got.Endpoint != "http://s1.local:8001" || got.Timeout != systemOneProbeTimeout || prober.soModels[0] != "decider" {
		t.Fatalf("probe = {Provider:%s Endpoint:%s Timeout:%v} for %q, want {llama_cpp http://s1.local:8001 %v} for decider",
			got.Provider, got.Endpoint, got.Timeout, prober.soModels[0], systemOneProbeTimeout)
	}
	if systemOneProbeTimeout != 30*time.Second {
		t.Fatalf("systemOneProbeTimeout = %v, want 30s", systemOneProbeTimeout)
	}
}

// TestSystemOnePassTreatsARouter404AsNoVerdict: a 404 the agent router writes
// itself (runtime.model_not_managed, say: a spec dropped while the loaded
// snapshot still names the model) asked no child, so it writes no sticky
// decision: no; it backs off like any other inconclusive answer.
func TestSystemOnePassTreatsARouter404AsNoVerdict(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answer(http.StatusNotFound, `{"error":{"code":"runtime.model_not_managed","message":"model decider is not managed"}}`)
	runner, st, loaded := agentSystemOneRunner(t, up)
	loaded.SetGatewayProbe("a1", []string{"decider"})

	runCapabilityCycle(runner)
	runCapabilityCycle(runner)

	if n := len(up.requests()); n != 1 {
		t.Fatalf("router saw %d requests, want 1 (the second pass is inside the back-off)", n)
	}
	if row, ok := st.capabilityOf("m1", routing.CapabilityDecision); ok {
		t.Fatalf("a router 404 wrote %+v, want no decision row", row)
	}
}

// blockingSystemOneProber holds every System One probe until release is
// closed (or the probe's context ends), sending on started as each one
// begins. It embeds fakeProber for the loop's other probes.
type blockingSystemOneProber struct {
	*fakeProber
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// unblock releases every probe, held or still to come; it may be called
// more than once.
func (b *blockingSystemOneProber) unblock() { b.once.Do(func() { close(b.release) }) }

func newBlockingSystemOneProber() *blockingSystemOneProber {
	return &blockingSystemOneProber{fakeProber: newFakeProber(), started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingSystemOneProber) ProbeSystemOne(ctx context.Context, _ routing.Target, _ string) (string, error) {
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return routing.CapabilityYes, nil
}

// blockedSystemOneRunner is one external application declaring decisions,
// with one decisions mapping, behind a blocking prober. Its cleanup releases
// the probe and waits for the pass, so no goroutine outlives the test.
func blockedSystemOneRunner(t *testing.T) (*appHealthRunner, *fakeHealthStore, *blockingSystemOneProber) {
	t.Helper()
	shrinkRetryGap(t)
	app := systemOneApp(routing.ProviderLlamaCPP, 8001, routing.APIFlavorDecisions)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{"a1": {systemOneMapping("m1", "decider")}}
	prober := newBlockingSystemOneProber()
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}
	t.Cleanup(func() {
		prober.unblock()
		runner.systemOnePasses.Wait()
	})
	return runner, st, prober
}

// runOnceWithin runs one cycle against state and fails the test unless it
// returns within d.
func runOnceWithin(t *testing.T, runner *appHealthRunner, state *cycleState, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.runOnce(context.Background(), state)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("the health cycle did not return within %v: it waits on the decision probe", d)
	}
}

// awaitProbeStart fails the test unless a System One probe starts within d.
func awaitProbeStart(t *testing.T, prober *blockingSystemOneProber, d time.Duration) {
	t.Helper()
	select {
	case <-prober.started:
	case <-time.After(d):
		t.Fatalf("no decision probe started within %v", d)
	}
}

// TestSystemOnePassDoesNotHoldTheHealthCycle: a decision probe is a real
// inference with its own 30 s timeout, so the pass runs detached from the
// cycle. A probe that hangs leaves the cycle free: it returns at once, well
// within the probe's timeout, and the server's health is written.
func TestSystemOnePassDoesNotHoldTheHealthCycle(t *testing.T) {
	runner, st, prober := blockedSystemOneRunner(t)

	runOnceWithin(t, runner, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: map[string]availWriteState{}}, 5*time.Second)

	awaitProbeStart(t, prober, 5*time.Second)
	st.mu.Lock()
	health := st.health["s1"]
	st.mu.Unlock()
	if health != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q written while the probe hangs", health, routing.HealthHealthy)
	}
}

// TestSystemOnePassRunsOncePerApplication: a tick that finds the
// application's pass still in flight starts no second one, although the
// application's cadence is due again; the tick after the pass ended does.
func TestSystemOnePassRunsOncePerApplication(t *testing.T) {
	runner, st, prober := blockedSystemOneRunner(t)
	var clockMu sync.Mutex
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		current = current.Add(d)
	}
	state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: map[string]availWriteState{}}

	runOnceWithin(t, runner, state, 5*time.Second)
	awaitProbeStart(t, prober, 5*time.Second)

	advance(time.Hour)
	runOnceWithin(t, runner, state, 5*time.Second)
	select {
	case <-prober.started:
		t.Fatal("a second decision pass started while the first was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	prober.unblock()
	runner.systemOnePasses.Wait()
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)

	// Rename the model, so the next pass has a new identity to ask about:
	// the pass ended, so the next due tick starts one.
	st.mu.Lock()
	st.mappings["a1"] = []routing.ModelMapping{systemOneMapping("m1", "decider-v2")}
	st.mu.Unlock()
	advance(time.Hour)
	runOnceWithin(t, runner, state, 5*time.Second)
	awaitProbeStart(t, prober, 5*time.Second)
}

// TestSystemOnePassSkipsAnUnreachableApplication: an application whose last
// observed reachability is false is not asked for a decision at all.
func TestSystemOnePassSkipsAnUnreachableApplication(t *testing.T) {
	shrinkRetryGap(t)
	app := systemOneApp(routing.ProviderLlamaCPP, 8001, routing.APIFlavorDecisions)
	app.HealthCheckMode = routing.HealthCheckModeHealthPath
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{"a1": {systemOneMapping("m1", "decider")}}
	prober := &systemOneFakeProber{fakeProber: newFakeProber()}
	prober.down["http://s1.local:8001"] = true
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	prober.soMu.Lock()
	defer prober.soMu.Unlock()
	if len(prober.soModels) != 0 {
		t.Fatalf("an unreachable application was asked for %v, want no probe", prober.soModels)
	}
}

// TestSystemOnePassReprobesAClearedVerdict: a settled verdict the operator
// clears in the portal (the row is deleted) is asked about again on the next
// pass, not only after a gateway restart, and then settles again.
func TestSystemOnePassReprobesAClearedVerdict(t *testing.T) {
	up := newSystemOneUpstream(t)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)

	st.mu.Lock()
	delete(st.caps["m1"], routing.CapabilityDecision)
	st.mu.Unlock()
	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 2 {
		t.Fatalf("after the clear the upstream saw %d requests, want 2", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)

	runCapabilityCycle(runner)
	if n := len(up.requests()); n != 2 {
		t.Fatalf("with the verdict back on file the upstream saw %d requests, want still 2", n)
	}
}

// TestSystemOnePassEscalatesTheBackoffOnARefusal: an upstream that refuses
// the probe request itself (a 400, 405 or 422) will refuse it again, so each
// refusal in a row doubles the window before the next probe, from
// systemOneRetryBackoff up to systemOneRefusedBackoffCap. A verdict, or a new
// identity behind the mapping, starts again from systemOneRetryBackoff.
func TestSystemOnePassEscalatesTheBackoffOnARefusal(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answer(http.StatusBadRequest, `{"error":"bad request"}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	if systemOneRefusedBackoffCap != time.Hour {
		t.Fatalf("systemOneRefusedBackoffCap = %v, want 1h", systemOneRefusedBackoffCap)
	}

	// expectWindow asserts the next probe comes exactly window after the last.
	expectWindow := func(step string, window time.Duration) {
		t.Helper()
		before := len(up.requests())
		current = current.Add(window - time.Second)
		runCapabilityCycle(runner)
		if n := len(up.requests()); n != before {
			t.Fatalf("%s: %v into a %v window the upstream saw %d requests, want still %d", step, window-time.Second, window, n, before)
		}
		current = current.Add(time.Second)
		runCapabilityCycle(runner)
		if n := len(up.requests()); n != before+1 {
			t.Fatalf("%s: at the end of a %v window the upstream saw %d requests, want %d", step, window, n, before+1)
		}
	}

	runCapabilityCycle(runner)
	for i, window := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		expectWindow("refusal "+strconv.Itoa(i+1), window)
	}

	// A verdict ends the streak: once it is cleared again, a refusal backs
	// off from the start.
	up.answer(http.StatusOK, `{"answers":{"probe":{"type":"noul","noul":0.9}}}`)
	expectWindow("the verdict", time.Hour)
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
	up.answer(http.StatusBadRequest, `{"error":"bad request"}`)
	st.mu.Lock()
	delete(st.caps["m1"], routing.CapabilityDecision)
	st.mu.Unlock()
	runCapabilityCycle(runner)
	expectWindow("after a verdict", systemOneRetryBackoff)
	expectWindow("after a verdict, second refusal", 2*systemOneRetryBackoff)

	// A new identity starts from the start as well.
	st.mu.Lock()
	st.mappings["a1"] = []routing.ModelMapping{systemOneMapping("m1", "decider-v2")}
	st.mu.Unlock()
	runCapabilityCycle(runner)
	expectWindow("a new identity", systemOneRetryBackoff)
}
