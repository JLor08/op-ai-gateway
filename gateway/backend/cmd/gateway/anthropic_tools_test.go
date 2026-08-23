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

// TestAnthropicToolCallTranslateEndToEnd drives the TRANSLATE path (native
// passthrough OFF) for a Claude Code-style /v1/messages tool-calling turn against
// a fake Chat Completions upstream, then a follow-up turn whose messages carry the
// prior tool_use (assistant) + tool_result (user) — the multi-turn shape the old
// text-only parser rejected. It asserts, through the REAL OpenAICompatibleClient:
//   - turn 1: the upstream receives `tools`; the client gets the Anthropic tool_use
//     SSE sequence (content_block_start tool_use → input_json_delta → message_delta
//     stop_reason "tool_use" → message_stop), with no [DONE] sentinel;
//   - turn 2: the request parses (no invalid-body error) and the upstream receives
//     the assistant tool_calls + the tool-role result keyed by the same id.
func TestAnthropicToolCallTranslateEndToEnd(t *testing.T) {
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
		// Stream a single tool call, then finish (Chat Completions wire shape).
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
	if err := seed.CreatePlainToken(ctx, store.TokenRecord{ID: "tk", UserID: "u", Name: "claude", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "claude-secret-1234567890"); err != nil {
		t.Fatalf("token: %v", err)
	}
	// vLLM app, NOT native (so the translate path is used), always_reachable.
	_ = seed.CreateAIServer(ctx, routing.AIServer{ID: "s", Name: "Up", Domain: u.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})
	// The app serves the Claude Code (anthropic) edge but its upstream speaks only
	// Chat Completions, so native_messages stays OFF and the gateway translates.
	if err := seed.CreateApplication(ctx, routing.Application{ID: "a", ServerID: "s", Type: routing.ProviderVLLM, Port: port, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, AlwaysReachable: true, CreatedAt: now, UpdatedAt: now}); err != nil {
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
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyJSON))
		req.Header.Set("Authorization", "Bearer claude-secret-1234567890")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// --- Turn 1: user asks; model calls the shell tool. ---
	rec := post(`{"model":"gw","max_tokens":64,"stream":true,"tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"list files"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream must not contain [DONE]:\n%s", body)
	}
	for _, want := range []string{
		`"type":"tool_use"`,
		`"id":"call_x"`,
		`"name":"shell"`,
		`"type":"input_json_delta"`,
		`{\"cmd\":\"ls\"}`, // the arguments, JSON-escaped inside partial_json
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("turn 1 client body missing %s:\n%s", want, body)
		}
	}
	mu.Lock()
	up1 := lastUpstreamBody
	mu.Unlock()
	if !strings.Contains(up1, `"tools"`) || !strings.Contains(up1, `"name":"shell"`) {
		t.Fatalf("turn 1 upstream body missing tools:\n%s", up1)
	}

	// --- Turn 2: the client replays the tool_use + feeds the tool_result back. ---
	// Anthropic carries the tool_use in an assistant message and the tool_result in
	// a user message; both must translate to the assistant(tool_calls) → tool shape.
	rec2 := post(`{"model":"gw","max_tokens":64,"stream":true,"tools":[{"name":"shell","input_schema":{"type":"object"}}],"messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_x","name":"shell","input":{"cmd":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_x","content":[{"type":"text","text":"a.txt\nb.txt"}]}]}
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
