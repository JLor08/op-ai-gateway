// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
	"time"
)

// staticSettings is a minimal healthSettings source for the end-to-end context
// probe test (the MemoryStore does not implement SystemSettings itself).
type staticSettings map[string]string

func (s staticSettings) SystemSettings(context.Context) (map[string]string, error) {
	return s, nil
}

// TestContextProbePassE2EPopulatesContextSize drives the context-probe pass with
// the REAL provider client (via the same Multiplexer the app-health loop uses)
// against an httptest upstream serving llama.cpp's /props shape, and asserts the
// probed n_ctx is persisted onto the matching mapping's context_size.
func TestContextProbePassE2EPopulatesContextSize(t *testing.T) {
	shrinkRetryGap(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"up","default_generation_settings":{"n_ctx":131072}}`))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()

	server := routing.AIServer{
		ID: "s1", Name: "S1", Domain: u.Hostname(), Provider: routing.ProviderVLLM,
		Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("create server: %v", err)
	}
	app := routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderVLLM, Port: port, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, ContextProbePath: "/props",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}
	mapping := routing.ModelMapping{
		ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
		Status: routing.ServerStatusActive, ContextSize: 0,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateMapping(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	prober := providerClients(0, false, nil) // real Multiplexer -> OpenAICompatibleClient for vllm
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: store, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: staticSettings(nil), probeTimeout: time.Second, cipher: nil, now: func() time.Time { return now }}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, err := store.MappingByID(ctx, "m1")
	if err != nil {
		t.Fatalf("mapping by id: %v", err)
	}
	if got.ContextSize != 131072 {
		t.Fatalf("ContextSize = %d, want 131072 (probed from the real upstream /props)", got.ContextSize)
	}
	if got.MetricsSource != "probe" {
		t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "probe")
	}
	if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(now) {
		t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, now)
	}
}
