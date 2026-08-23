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

// TestBenchmarkTriggerEndToEnd drives the benchmark trigger endpoints against a
// fake Chat Completions SSE upstream (translate path). It asserts:
//   - POST /api/portal/mappings/{id}/benchmark returns 202 and reports a running run;
//   - a SECOND POST while the first run is still in flight returns 409;
//   - after the run finishes (polled via the status GET), the mapping's
//     gen_tokens_per_second is persisted from the upstream timings and
//     metrics_source is "benchmark".
//
// The upstream blocks on `release` until the test closes it, so the concurrent
// 409 is deterministic (the first run is provably still registered), not a race.
func TestBenchmarkTriggerEndToEnd(t *testing.T) {
	ctx := context.Background()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-release // hold every benchmark request until the test releases them
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range []string{
			`{"choices":[{"delta":{"content":"ok"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20},"timings":{"prompt_per_second":900,"predicted_per_second":42}}`,
		} {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())

	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	seed, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if err := seed.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	_ = seed.CreateUser(ctx, store.User{ID: "u", Email: "u@e.test", DisplayName: "U", Role: "user", Status: "active", CreatedAt: now, UpdatedAt: now})
	if err := seed.CreatePlainToken(ctx, store.TokenRecord{ID: "tk", UserID: "u", Name: "portal", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "portal-secret-1234567890"); err != nil {
		t.Fatalf("token: %v", err)
	}
	_ = seed.CreateAIServer(ctx, routing.AIServer{ID: "s", Name: "Up", Domain: u.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})
	// Make the token's user the server owner so the owner-scoped authorize passes.
	if err := seed.SetServerOwners(ctx, "s", []string{"u"}); err != nil {
		t.Fatalf("owners: %v", err)
	}
	// Translate path: upstream speaks only Chat Completions, so native flags OFF.
	if err := seed.CreateApplication(ctx, routing.Application{ID: "a", ServerID: "s", Type: routing.ProviderVLLM, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, AlwaysReachable: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("app: %v", err)
	}
	if err := seed.CreateMapping(ctx, routing.ModelMapping{ID: "m", ApplicationID: "a", GatewayModelName: "gw", AppModelName: "up", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	_ = seed.Close()

	srv, cleanup, err := buildGatewayServer(config.Config{Addr: "127.0.0.1:8080", DBDriver: "sqlite", SQLitePath: dbPath, AutoMigrate: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer cleanup()

	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer portal-secret-1234567890")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	type benchStatus struct {
		Running  bool   `json:"running"`
		ServerID string `json:"server_id"`
		Scope    string `json:"scope"`
		Total    int    `json:"total"`
		Done     int    `json:"done"`
	}

	// --- Start the benchmark: 202 + running. ---
	rec := do(http.MethodPost, "/api/portal/mappings/m/benchmark")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started benchStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start body: %v (%s)", err, rec.Body.String())
	}
	if !started.Running || started.ServerID != "s" || started.Scope != "mapping" || started.Total != 1 {
		t.Fatalf("start status body = %#v", started)
	}

	// --- A second POST while the first run is in flight (blocked on release) -> 409. ---
	rec = do(http.MethodPost, "/api/portal/mappings/m/benchmark")
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent start status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "benchmark.already_running") {
		t.Fatalf("concurrent start body missing already_running: %s", rec.Body.String())
	}

	// Release the upstream so the run can complete.
	close(release)

	// --- Poll the status GET until the run finishes. ---
	deadline := time.Now().Add(5 * time.Second)
	var last benchStatus
	for time.Now().Before(deadline) {
		rec = do(http.MethodGet, "/api/portal/servers/s/benchmark/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("status GET = %d, body = %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &last); err != nil {
			t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
		}
		if !last.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last.Running {
		t.Fatalf("benchmark did not finish within the deadline: %#v", last)
	}
	if last.Done != 1 {
		t.Fatalf("finished status done = %d, want 1: %#v", last.Done, last)
	}

	// --- The mapping's metrics are persisted from the upstream timings. ---
	rec = do(http.MethodGet, "/api/portal/applications/a/mappings")
	if rec.Code != http.StatusOK {
		t.Fatalf("mappings list = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Data []struct {
			ID                 string  `json:"id"`
			GenTokensPerSecond float64 `json:"gen_tokens_per_second"`
			MetricsSource      string  `json:"metrics_source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode mappings: %v (%s)", err, rec.Body.String())
	}
	var found bool
	for _, m := range listResp.Data {
		if m.ID != "m" {
			continue
		}
		found = true
		if m.GenTokensPerSecond != 42 {
			t.Fatalf("gen_tokens_per_second = %v, want 42", m.GenTokensPerSecond)
		}
		if m.MetricsSource != "benchmark" {
			t.Fatalf("metrics_source = %q, want benchmark", m.MetricsSource)
		}
	}
	if !found {
		t.Fatalf("mapping m not found in %s", rec.Body.String())
	}
}
