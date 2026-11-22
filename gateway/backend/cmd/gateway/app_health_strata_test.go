// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

// strataShareModelsBody is the /v1/models body measured on a Strata desktop
// app's LAN share listener.
const strataShareModelsBody = `{"object":"list","data":[{"id":"unsloth-q8_0","owned_by":"strata","status":{"value":"loaded"},"architecture":{"input_modalities":["text","image"]},"context_length":262144}]}`

// strataShareServer mimics the Strata desktop app's LAN share listener: it
// serves GET /v1/models only with "Authorization: Bearer k" (401 without) and
// answers every other path 404 "Not found", the way the share answers /props,
// /metrics, /health and /v1/status. It records every requested path.
func strataShareServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return strataShareServerWithBody(t, strataShareModelsBody)
}

// strataShareServerWithBody is strataShareServer answering /v1/models with body.
func strataShareServerWithBody(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/v1/models" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// strataShareApp is a strata application carrying the portal's form defaults
// for the type (health and loaded models from /v1/models, format openai, no
// context probe path, no capacity probe) and the share's API key.
func strataShareApp(t *testing.T, srv *httptest.Server) (routing.AIServer, routing.Application) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	token, err := capture.SealSecret(nil, true, "k")
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	server := routing.AIServer{ID: "s1", Domain: u.Hostname(), Provider: routing.ProviderStrata, Status: routing.ServerStatusActive}
	app := routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderStrata, Port: port, Scheme: "http",
		Status:             routing.ServerStatusActive,
		HealthCheckMode:    routing.HealthCheckModeHealthPath,
		HealthCheckPath:    "/v1/models",
		LoadedModelsPath:   "/v1/models",
		LoadedModelsFormat: "openai",
		APIToken:           token,
	}
	return server, app
}

// TestStrataShareListenerHealthy pins that a strata application with the
// portal defaults is fully served by the share listener's single readable
// route: the health probe and the loaded-models probe both carry the
// application's API key, the listed model is recorded as loaded, its
// context_length lands on the mapping, and nothing ever asks the share for a
// route it answers 404 (so no optional probe can mark the application
// degraded).
func TestStrataShareListenerHealthy(t *testing.T) {
	shrinkRetryGap(t)
	srv, requested := strataShareServer(t)
	server, app := strataShareApp(t, srv)
	st := &fakeHealthStore{
		servers: []routing.AIServer{server},
		apps:    map[string][]routing.Application{"s1": {app}},
		mappings: map[string][]routing.ModelMapping{
			"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "qwen", AppModelName: "unsloth-q8_0", Status: routing.ServerStatusActive}},
		},
	}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	mux := providerClients(0, false, srv.Client())

	(&appHealthRunner{store: st, prober: mux, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !reg.Reachable("a1") {
		t.Fatalf("strata app not reachable: %+v", reg.Snapshot()["a1"])
	}
	if got := reg.Snapshot()["a1"].LastError; got != "" {
		t.Fatalf("strata app LastError = %q, want none", got)
	}
	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
	if got := loaded.LoadedAppModels("a1", "s1"); !reflect.DeepEqual(got, []string{"unsloth-q8_0"}) {
		t.Fatalf("loaded models = %v, want [unsloth-q8_0]", got)
	}
	mp, ok := st.mappingOf("m1")
	if !ok {
		t.Fatalf("mapping m1 missing")
	}
	if mp.ContextSize != 262144 || mp.MetricsSource != "probe" {
		t.Fatalf("mapping context = %d (source %q), want 262144 (probe)", mp.ContextSize, mp.MetricsSource)
	}
	st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceStrataModels)
	for _, p := range requested() {
		if p != "/v1/models" {
			t.Fatalf("share was asked for %q; only /v1/models exists there (all requests: %v)", p, requested())
		}
	}

	// Model discovery (the portal's model sync and the "discover models"
	// action) lists through the same client with the application's key.
	models, err := mux.ListModels(provider.WithUpstreamAuth(context.Background(), app.APITokenHeader, "k"), routing.Target{
		Provider: app.Type, Endpoint: routing.ApplicationEndpoint(server, app), Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"unsloth-q8_0"}) {
		t.Fatalf("discovered models = %v, want [unsloth-q8_0]", models)
	}
}

// TestStrataShareListenerUnreachableWithoutKey pins the other half: without
// the API key the share answers 401, and the application is unreachable.
func TestStrataShareListenerUnreachableWithoutKey(t *testing.T) {
	shrinkRetryGap(t)
	srv, _ := strataShareServer(t)
	server, app := strataShareApp(t, srv)
	app.APIToken = ""
	st := &fakeHealthStore{servers: []routing.AIServer{server}, apps: map[string][]routing.Application{"s1": {app}}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: providerClients(0, false, srv.Client()), syncer: nil, registry: reg, loaded: gateway.NewLoadedModelRegistry(), agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if reg.Reachable("a1") {
		t.Fatalf("strata app reachable without its API key")
	}
}

// TestListedContextSizeRules pins recordListedContextSizes' guards on the
// share body: a locked mapping keeps its pinned size, and an application with
// its own context probe path takes its size from that probe only (here a
// /props the share answers 404, so nothing is recorded).
func TestListedContextSizeRules(t *testing.T) {
	cases := []struct {
		name     string
		locked   bool
		ctxProbe string
		want     int
	}{
		{"locked mapping keeps its size", true, "", 4096},
		{"own context probe path wins", false, "/props", 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkRetryGap(t)
			srv, _ := strataShareServer(t)
			server, app := strataShareApp(t, srv)
			app.ContextProbePath = tc.ctxProbe
			st := &fakeHealthStore{
				servers: []routing.AIServer{server},
				apps:    map[string][]routing.Application{"s1": {app}},
				mappings: map[string][]routing.ModelMapping{
					"a1": {{ID: "m1", ApplicationID: "a1", AppModelName: "unsloth-q8_0", Status: routing.ServerStatusActive, ContextSize: 4096, MetricsLocked: tc.locked}},
				},
			}
			(&appHealthRunner{store: st, prober: providerClients(0, false, srv.Client()), syncer: nil, registry: gateway.NewAppHealthRegistry(nil), loaded: gateway.NewLoadedModelRegistry(), agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

			if mp, _ := st.mappingOf("m1"); mp.ContextSize != tc.want {
				t.Fatalf("ContextSize = %d, want %d", mp.ContextSize, tc.want)
			}
		})
	}
}

// runStrataShareCycle runs one health cycle for app against srv through the
// real provider clients and returns the store.
func runStrataShareCycle(t *testing.T, srv *httptest.Server, server routing.AIServer, app routing.Application) *fakeHealthStore {
	t.Helper()
	st := &fakeHealthStore{
		servers: []routing.AIServer{server},
		apps:    map[string][]routing.Application{"s1": {app}},
		mappings: map[string][]routing.ModelMapping{
			"a1": {{ID: "m1", ApplicationID: "a1", AppModelName: "unsloth-q8_0", Status: routing.ServerStatusActive, ContextSize: 4096}},
		},
	}
	(&appHealthRunner{store: st, prober: providerClients(0, false, srv.Client()), syncer: nil, registry: gateway.NewAppHealthRegistry(nil), loaded: gateway.NewLoadedModelRegistry(), agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
	return st
}

// TestStrataTextOnlyListingRecordsNoVision pins that a text-only
// input_modalities list is no vision verdict: nothing pins it as exhaustive.
func TestStrataTextOnlyListingRecordsNoVision(t *testing.T) {
	shrinkRetryGap(t)
	srv, _ := strataShareServerWithBody(t, `{"object":"list","data":[{"id":"unsloth-q8_0","architecture":{"input_modalities":["text"]},"context_length":262144}]}`)
	server, app := strataShareApp(t, srv)
	st := runStrataShareCycle(t, srv, server, app)

	if _, ok := st.capabilityOf("m1", routing.CapabilityVision); ok {
		t.Fatalf("text-only listing wrote a vision verdict")
	}
	if mp, _ := st.mappingOf("m1"); mp.ContextSize != 262144 {
		t.Fatalf("ContextSize = %d, want 262144", mp.ContextSize)
	}
}

// TestListedModelInfosRecordedForStrataOnly pins that the listing's context
// size and vision verdict are recorded for a strata application only: an
// existing vllm-typed application whose /v1/models emits context_length and
// input_modalities (another gateway instance does) keeps its mapping as it was.
func TestListedModelInfosRecordedForStrataOnly(t *testing.T) {
	shrinkRetryGap(t)
	srv, _ := strataShareServer(t)
	server, app := strataShareApp(t, srv)
	app.Type = routing.ProviderVLLM
	st := runStrataShareCycle(t, srv, server, app)

	if mp, _ := st.mappingOf("m1"); mp.ContextSize != 4096 || mp.MetricsSource != "" {
		t.Fatalf("vllm mapping = %d (source %q), want 4096 untouched", mp.ContextSize, mp.MetricsSource)
	}
	if st.ctxProbeSetCount() != 0 || st.capabilityWriteCount() != 0 {
		t.Fatalf("vllm app wrote %d context and %d capability updates, want none", st.ctxProbeSetCount(), st.capabilityWriteCount())
	}
}
