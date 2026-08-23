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

// TestAnthropicThinkingThreadedEndToEnd drives the TRANSLATE path (native
// passthrough OFF) for a Claude Code /v1/messages turn against a fake Chat
// Completions upstream that emits reasoning_content, then a follow-up turn that
// replays the assistant's thinking block. Through the REAL OpenAICompatibleClient
// it asserts:
//   - turn 1: the upstream's reasoning_content surfaces to the client as an
//     Anthropic thinking block (content_block_start thinking → thinking_delta →
//     empty signature_delta → content_block_stop) BEFORE the text block, with
//     stop_reason "end_turn" and cache-aware usage (input_tokens excludes the
//     cached subset; cache_read_input_tokens carries it);
//   - turn 2: a replayed `thinking` block threads back to the upstream as
//     reasoning_content on the assistant message — the continuity fix.
func TestAnthropicThinkingThreadedEndToEnd(t *testing.T) {
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
		// Reasoning deltas, then the answer, then finish — with a cached-token subset.
		for _, f := range []string{
			`{"choices":[{"delta":{"reasoning_content":"I think "}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"carefully."}}]}`,
			`{"choices":[{"delta":{"content":"Hello!"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"prompt_tokens_details":{"cached_tokens":2}}}`,
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

	// --- Turn 1: the upstream reasons; the client must see a thinking block. ---
	rec := post(`{"model":"gw","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream must not contain [DONE]:\n%s", body)
	}
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"I think "`,
		`"type":"signature_delta"`,
		`"signature":""`,
		`"type":"text"`,
		`"text":"Hello!"`,
		`"stop_reason":"end_turn"`,
		`"cache_read_input_tokens":2`,
		`"input_tokens":3`, // 5 prompt - 2 cached
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("turn 1 client body missing %s:\n%s", want, body)
		}
	}
	// The thinking block (index 0) must be stopped before the text block (index 1)
	// starts (note: json.Marshal sorts map keys, so the text block start serializes
	// as {"text":"","type":"text"}).
	thinkStop := strings.Index(body, "event: content_block_stop")
	textStart := strings.Index(body, `"content_block":{"text":"","type":"text"}`)
	if thinkStop < 0 || textStart < 0 || thinkStop > textStart {
		t.Fatalf("thinking block not closed before the text block:\n%s", body)
	}

	// --- Turn 2: the client replays the assistant thinking block. ---
	rec2 := post(`{"model":"gw","max_tokens":64,"stream":true,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"earlier chain of thought","signature":""},{"type":"text","text":"Hello!"}]},
		{"role":"user","content":"continue"}
	]}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	up2 := lastUpstreamBody
	mu.Unlock()
	// The replayed thinking threads back to the upstream as reasoning_content.
	for _, want := range []string{`"reasoning_content"`, "earlier chain of thought"} {
		if !strings.Contains(up2, want) {
			t.Fatalf("turn 2 upstream body missing %s:\n%s", want, up2)
		}
	}
}
