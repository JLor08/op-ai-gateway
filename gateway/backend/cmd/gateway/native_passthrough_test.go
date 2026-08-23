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
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNativePassthroughEndToEnd drives a real streaming /v1/responses request
// through the fully-wired gateway (real Multiplexer -> real OpenAICompatibleClient)
// to a fake Codex-capable upstream, proving that an application with
// native_responses=true proxies the raw body verbatim (rewriting only the model),
// hits the upstream's own /v1/responses path, and streams the response back.
func TestNativePassthroughEndToEnd(t *testing.T) {
	ctx := context.Background()

	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"usage\":{\"input_tokens\":4,\"output_tokens\":9,\"total_tokens\":13}}}\n\n")
	}))
	defer upstream.Close()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	// Seed user + token + a vLLM app with native_responses=true (always_reachable
	// so no probe gating) mapping gateway model "gw-model" -> upstream "up-model".
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	seed, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := seed.CreateUser(ctx, store.User{ID: "usr_c", Email: "c@example.test", DisplayName: "Codex User", Role: "user", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := seed.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_c", UserID: "usr_c", Name: "codex", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "codex-secret-1234567890"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := seed.CreateAIServer(ctx, routing.AIServer{ID: "srv_c", Name: "Codex Upstream", Domain: u.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := seed.CreateApplication(ctx, routing.Application{ID: "app_c", ServerID: "srv_c", Type: routing.ProviderVLLM, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, AlwaysReachable: true, NativeResponses: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := seed.CreateMapping(ctx, routing.ModelMapping{ID: "map_c", ApplicationID: "app_c", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	srv, cleanup, err := buildGatewayServer(config.Config{Addr: "127.0.0.1:8080", DBDriver: "sqlite", SQLitePath: dbPath, AutoMigrate: true})
	if err != nil {
		t.Fatalf("buildGatewayServer: %v", err)
	}
	defer cleanup()

	reqBody := `{"model":"gw-model","stream":true,"input":"hi","tools":[{"type":"function","name":"shell"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer codex-secret-1234567890")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	// The upstream must receive the raw body with ONLY the model rewritten.
	var upReq struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(gotBody, &upReq); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, gotBody)
	}
	if upReq.Model != "up-model" {
		t.Fatalf("upstream model = %q, want up-model (rewritten from gw-model)", upReq.Model)
	}
	if len(upReq.Tools) == 0 || !strings.Contains(string(upReq.Tools), "shell") {
		t.Fatalf("upstream body dropped tools (translate path would): %s", gotBody)
	}
	// The upstream SSE must be streamed back to the client verbatim.
	if !strings.Contains(rec.Body.String(), "response.completed") || !strings.Contains(rec.Body.String(), "resp_up") {
		t.Fatalf("client did not receive the upstream response: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
}
