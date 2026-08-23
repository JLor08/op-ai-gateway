// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
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
	"sync"
	"testing"
	"time"
)

// TestResponsesToolCallTranslateEndToEnd drives the TRANSLATE path (native
// passthrough OFF) for a Codex-style /v1/responses tool-calling turn against a
// fake Chat Completions upstream, then a follow-up turn whose input carries the
// prior function_call + function_call_output (the exact multi-turn shape that
// previously failed to parse). It asserts, through the REAL OpenAICompatibleClient:
//   - turn 1: the upstream receives `tools`; the client gets a function_call item;
//   - turn 2: the request parses (no invalid-body error) and the upstream receives
//     the assistant tool_calls + the tool-role result.
func TestResponsesToolCallTranslateEndToEnd(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var lastUpstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastUpstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Stream a single tool call, then finish.
		for _, f := range []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
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
	if err := seed.CreatePlainToken(ctx, store.TokenRecord{ID: "tk", UserID: "u", Name: "codex", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "codex-secret-1234567890"); err != nil {
		t.Fatalf("token: %v", err)
	}
	// vLLM app, NOT native (so the translate path is used), always_reachable.
	_ = seed.CreateAIServer(ctx, routing.AIServer{ID: "s", Name: "Up", Domain: u.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})
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

	post := func(bodyJSON string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
		req.Header.Set("Authorization", "Bearer codex-secret-1234567890")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// --- Turn 1: user asks; model calls the shell tool. ---
	rec := post(`{"model":"gw","stream":true,"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"function_call"`) || !strings.Contains(body, `"call_id":"call_x"`) || !strings.Contains(body, `"name":"shell"`) {
		t.Fatalf("turn 1 client body missing the function_call item:\n%s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("turn 1 missing response.completed:\n%s", body)
	}
	mu.Lock()
	up1 := lastUpstreamBody
	mu.Unlock()
	if !strings.Contains(up1, `"tools"`) || !strings.Contains(up1, `"name":"shell"`) {
		t.Fatalf("turn 1 upstream body missing tools:\n%s", up1)
	}

	// --- Turn 2: the client replays the function_call + feeds the tool result. ---
	// This is the multi-turn shape that previously errored with content_unsupported.
	rec2 := post(`{"model":"gw","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
		{"type":"function_call","call_id":"call_x","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_x","output":"a.txt\nb.txt"}
	]}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d (multi-turn must parse), body = %s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	up2 := lastUpstreamBody
	mu.Unlock()
	// The upstream must receive the assistant tool_calls + the tool-role result.
	for _, want := range []string{`"tool_calls"`, `"call_x"`, `"role":"tool"`, `"tool_call_id":"call_x"`, "a.txt"} {
		if !strings.Contains(up2, want) {
			t.Fatalf("turn 2 upstream body missing %s:\n%s", want, up2)
		}
	}
}

// TestResponsesReasoningThreadedToUpstream drives the TRANSLATE path for a Codex
// reasoning turn: the input replays a `reasoning` item before the function_call, and
// the request carries `reasoning.effort`. It asserts, through the REAL
// OpenAICompatibleClient, that the upstream Chat Completions body receives the
// assistant `reasoning_content` (chain-of-thought continuity, like llama.cpp) AND
// `reasoning_effort` — so a reasoning model keeps its train of thought across turns.
func TestResponsesReasoningThreadedToUpstream(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var lastUpstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastUpstreamBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range []string{
			`{"choices":[{"delta":{"reasoning_content":"still thinking"}}]}`,
			`{"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
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
	if err := seed.CreatePlainToken(ctx, store.TokenRecord{ID: "tk", UserID: "u", Name: "codex", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "codex-secret-1234567890"); err != nil {
		t.Fatalf("token: %v", err)
	}
	_ = seed.CreateAIServer(ctx, routing.AIServer{ID: "s", Name: "Up", Domain: u.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})
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

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw","stream":true,"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
		{"type":"reasoning","content":[{"type":"reasoning_text","text":"the user wants ls"}]},
		{"type":"function_call","call_id":"call_x","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_x","output":"a.txt"}
	]}`))
	req.Header.Set("Authorization", "Bearer codex-secret-1234567890")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The client sees the reasoning item + the answer.
	cbody := rec.Body.String()
	if !strings.Contains(cbody, `"type":"reasoning"`) || !strings.Contains(cbody, "still thinking") {
		t.Fatalf("client body missing the streamed reasoning item:\n%s", cbody)
	}

	mu.Lock()
	up := lastUpstreamBody
	mu.Unlock()
	// The upstream must receive the assistant's prior reasoning (continuity) + the
	// requested reasoning effort.
	if !strings.Contains(up, `"reasoning_content":"the user wants ls"`) {
		t.Fatalf("upstream body missing threaded reasoning_content:\n%s", up)
	}
	if !strings.Contains(up, `"reasoning_effort":"high"`) {
		t.Fatalf("upstream body missing reasoning_effort:\n%s", up)
	}
}
