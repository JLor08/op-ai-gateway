// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cacheControlKey is the Messages wire key the upstream body carries on a cached
// block; its presence (or absence) is the observable effect of the policy.
const cacheControlKey = "cache_control"

// cachingPortalStub reports the anthropic_prompt_caching_enabled switch and nothing
// else (the same nil-embedded-interface trick as fakePortalMeshRequireTLS). reads
// counts the accessor calls so a test can prove the hot path is served from the
// gateway-side cache rather than the store.
type cachingPortalStub struct {
	portal.API
	on    bool
	reads *atomic.Int32
}

func (f cachingPortalStub) AnthropicPromptCachingEnabled(context.Context) bool {
	if f.reads != nil {
		f.reads.Add(1)
	}
	return f.on
}

func cacheTestText(role inference.Role, text string) inference.Message {
	return inference.Message{Role: role, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}
}

// cacheBigHistory is a four-message conversation whose 5000-char system prompt
// (about 1250 tokens) clears the default 1024-token minimum and which carries
// assistant history, i.e. the shape the auto-switch is meant to cache.
func cacheBigHistory() []inference.Message {
	return []inference.Message{
		cacheTestText(inference.RoleSystem, strings.Repeat("x", 5000)),
		cacheTestText(inference.RoleUser, "q1"),
		cacheTestText(inference.RoleAssistant, "a1"),
		cacheTestText(inference.RoleUser, "q2"),
	}
}

func TestApplyAnthropicCachePolicy(t *testing.T) {
	s := &Server{Portal: cachingPortalStub{on: true}}
	antTarget := routing.Target{Provider: routing.ProviderVendorAnthropic, ProviderModel: "claude-sonnet-5-5"}
	// An Anthropic SUBSCRIPTION translate target (OAuth bearer + Claude-Code
	// masquerade) carries the SAME provider kind as the api-key one.
	subTarget := subscriptionTarget("acc_sub", "https://api.anthropic.com")
	bigHistory := cacheBigHistory()
	smallConvo := []inference.Message{
		cacheTestText(inference.RoleUser, "hi"),
		cacheTestText(inference.RoleAssistant, "yo"),
		cacheTestText(inference.RoleUser, "x"),
	}
	bigTool := inference.Tool{Name: "shell", Description: strings.Repeat("d", 6000)}
	huge := append([]inference.Message{cacheTestText(inference.RoleSystem, strings.Repeat("x", 17000))}, bigHistory[1:]...)

	cases := []struct {
		name   string
		target routing.Target
		req    inference.Request
		want   bool
	}{
		{"anthropic + history + big", antTarget, inference.Request{Messages: bigHistory}, true},
		{"anthropic subscription (masquerade) + history + big", subTarget, inference.Request{Messages: bigHistory}, true},
		{"anthropic + chat session, no history yet", antTarget, inference.Request{SessionSource: "chat", Messages: bigHistory[:2]}, true},
		{"anthropic + one-shot (no history, not chat)", antTarget, inference.Request{Messages: bigHistory[:2]}, false},
		{"anthropic + non-chat session source, no history", antTarget, inference.Request{SessionSource: "codex", Messages: bigHistory[:2]}, false},
		{"anthropic + too small prefix", antTarget, inference.Request{Messages: smallConvo}, false},
		{"anthropic + chat session but too small prefix", antTarget, inference.Request{SessionSource: "chat", Messages: smallConvo[:1]}, false},
		{"tools count toward the prefix estimate", antTarget, inference.Request{Messages: smallConvo, Tools: []inference.Tool{bigTool}}, true},
		{"non-anthropic target", routing.Target{Provider: routing.ProviderVendorOpenAI, ProviderModel: "gpt-4o"}, inference.Request{Messages: bigHistory}, false},
		{"openai subscription target", routing.Target{Provider: routing.ProviderVendorOpenAISubscription, ProviderModel: "gpt-5"}, inference.Request{Messages: bigHistory}, false},
		{"on-prem target", routing.Target{Provider: routing.ProviderOllama, ProviderModel: "llama3"}, inference.Request{Messages: bigHistory}, false},
		{"model minimum: opus-4-6 needs 4096 tokens, 1250-token prefix is below", routing.Target{Provider: routing.ProviderVendorAnthropic, ProviderModel: "claude-opus-4-6"}, inference.Request{Messages: bigHistory}, false},
		{"model minimum: opus-4-6 with a 4250-token prefix clears it", routing.Target{Provider: routing.ProviderVendorAnthropic, ProviderModel: "claude-opus-4-6"}, inference.Request{Messages: huge}, true},
		{"model minimum: opus-5 needs only 512 tokens", routing.Target{Provider: routing.ProviderVendorAnthropic, ProviderModel: "claude-opus-5"}, inference.Request{Messages: bigHistory}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			s.applyAnthropicCachePolicy(context.Background(), tc.target, &req)
			got := req.PromptCache != nil && req.PromptCache.Enabled
			if got != tc.want {
				t.Fatalf("PromptCache enabled = %v, want %v (directive %+v)", got, tc.want, req.PromptCache)
			}
			if !got && req.PromptCache != nil {
				t.Fatalf("an ineligible request must leave PromptCache nil, got %+v", req.PromptCache)
			}
			if got && req.PromptCache.TTL != "" {
				t.Fatalf("v1 uses the default 5m ephemeral TTL (empty), got %q", req.PromptCache.TTL)
			}
		})
	}

	// The policy mutates only the copy it is handed: the caller's own request and
	// its Messages slice are untouched.
	t.Run("mutates only PromptCache", func(t *testing.T) {
		req := inference.Request{Model: "m", Messages: bigHistory}
		s.applyAnthropicCachePolicy(context.Background(), antTarget, &req)
		if req.PromptCache == nil || len(req.Messages) != len(bigHistory) || req.Model != "m" {
			t.Fatalf("unexpected request after policy: %+v", req)
		}
	})

	// flag off -> never set, even for the eligible case
	t.Run("flag off never sets", func(t *testing.T) {
		sOff := &Server{Portal: cachingPortalStub{on: false}}
		req := inference.Request{Messages: bigHistory}
		sOff.applyAnthropicCachePolicy(context.Background(), antTarget, &req)
		if req.PromptCache != nil {
			t.Fatalf("flag off must not set PromptCache, got %+v", req.PromptCache)
		}
	})

	// A server with no portal (a test fixture) fails closed.
	t.Run("nil portal fails closed", func(t *testing.T) {
		sNil := &Server{}
		req := inference.Request{Messages: bigHistory}
		sNil.applyAnthropicCachePolicy(context.Background(), antTarget, &req)
		if req.PromptCache != nil {
			t.Fatalf("nil portal must not set PromptCache, got %+v", req.PromptCache)
		}
	})
}

func TestAnthropicCacheMinTokens(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-6", 4096},
		{"claude-opus-4-5-20251101", 4096},
		{"claude-haiku-4-5-20251001", 4096},
		{"claude-opus-4-7", 2048},
		{"claude-opus-5", 512},
		{"claude-opus-5-1", 512},
		{"claude-sonnet-5-5", 512},
		{"claude-haiku-5-5", 512},
		{"claude-fable-5", 512},
		{"claude-opus-4-8", 1024},
		{"claude-sonnet-5", 1024},
		{"claude-sonnet-4-5-20250929", 1024},
		{"claude-3-7-sonnet", 1024},
		{"some-future-model", 1024},
		{"", 1024},
		{"CLAUDE-OPUS-4-6", 4096}, // case-insensitive
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := anthropicCacheMinTokens(tc.model); got != tc.want {
				t.Fatalf("anthropicCacheMinTokens(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}

func TestEstPrefixTokens(t *testing.T) {
	req := inference.Request{
		Messages: []inference.Message{
			cacheTestText(inference.RoleSystem, strings.Repeat("a", 400)),
			{Role: inference.RoleUser, Content: []inference.ContentPart{
				{Type: inference.ContentText, Text: strings.Repeat("b", 400)},
				{Type: inference.ContentImage, ImageURL: strings.Repeat("u", 4000)}, // not text: ignored
			}},
			{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{Name: "tool", Arguments: strings.Repeat("c", 392)}}},
		},
		Tools: []inference.Tool{{Name: strings.Repeat("n", 4), Description: strings.Repeat("d", 396)}},
	}
	// text 800 + tool call (4 + 392) + tool 400 = 1596 chars -> 399 tokens
	if got, want := estPrefixTokens(req), 399; got != want {
		t.Fatalf("estPrefixTokens = %d, want %d", got, want)
	}
	if got := estPrefixTokens(inference.Request{}); got != 0 {
		t.Fatalf("estPrefixTokens(empty) = %d, want 0", got)
	}
}

func TestHasAssistantHistory(t *testing.T) {
	if hasAssistantHistory(nil) {
		t.Fatal("no messages: want false")
	}
	if hasAssistantHistory([]inference.Message{cacheTestText(inference.RoleSystem, "s"), cacheTestText(inference.RoleUser, "u")}) {
		t.Fatal("system+user only: want false")
	}
	if !hasAssistantHistory(cacheBigHistory()) {
		t.Fatal("a prior assistant turn: want true")
	}
}

func TestIsAnthropicTranslateTarget(t *testing.T) {
	cases := []struct {
		name   string
		target routing.Target
		want   bool
	}{
		{"api-key vendor anthropic", routing.Target{Provider: routing.ProviderVendorAnthropic}, true},
		{"subscription vendor anthropic", subscriptionTarget("acc", "https://api.anthropic.com"), true},
		{"vendor openai", routing.Target{Provider: routing.ProviderVendorOpenAI}, false},
		{"openai subscription", routing.Target{Provider: routing.ProviderVendorOpenAISubscription}, false},
		{"on-prem ollama", routing.Target{Provider: routing.ProviderOllama}, false},
		{"zero target", routing.Target{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAnthropicTranslateTarget(tc.target); got != tc.want {
				t.Fatalf("isAnthropicTranslateTarget = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnthropicPromptCachingFlagIsReadThroughTheCache proves the per-request flag
// read is served from the gateway-side TTL cache (the inference hot path must not
// issue a system_settings read per request) and that invalidation drops it.
func TestAnthropicPromptCachingFlagIsReadThroughTheCache(t *testing.T) {
	reads := &atomic.Int32{}
	s := &Server{Portal: cachingPortalStub{on: true, reads: reads}}
	for range 5 {
		if !s.anthropicPromptCachingEnabledCached(context.Background()) {
			t.Fatal("flag on: want true")
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("portal reads = %d after 5 cached reads, want 1", got)
	}
	s.invalidateAnthropicPromptCachingCache()
	if !s.anthropicPromptCachingEnabledCached(context.Background()) {
		t.Fatal("flag on after invalidate: want true")
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("portal reads = %d after invalidate, want 2 (one re-read)", got)
	}
	if (&Server{}).anthropicPromptCachingEnabledCached(context.Background()) {
		t.Fatal("nil portal must fail closed")
	}
}

// anthropicChatBody is a portal-chat style OpenAI chat-completions body for the
// api-key Anthropic account seeded by seedAPIKeyAnthropicAccount.
func anthropicChatBody(t *testing.T, stream bool, msgs ...inference.Message) string {
	t.Helper()
	type chatMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	out := make([]chatMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, chatMsg{Role: string(m.Role), Content: m.Text()})
	}
	raw, err := json.Marshal(map[string]any{"model": apiKeyAnthropicModel, "stream": stream, "messages": out})
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(raw)
}

// anthropicCacheDispatchFixture is a real *Server (resolver, portal Service,
// settings handler) whose Anthropic api-key vendor account is served by a stub
// upstream, so a chat-completions request exercises BOTH dispatch hooks
// (inference_complete.go for stream:false, stream_session.go for stream:true).
type anthropicCacheDispatchFixture struct {
	srv     *Server
	upBody  func() []byte // the upstream body of the last dispatched request
	flagSet func(on bool) // flips the setting through the real settings endpoint
}

func newAnthropicCacheDispatchFixture(t *testing.T, stream bool) *anthropicCacheDispatchFixture {
	t.Helper()
	var (
		stubURL string
		upBody  func() []byte
	)
	if stream {
		st := newAnthropicPlatformMessagesStub(t)
		stubURL, upBody = st.srv.URL, func() []byte { return st.gotBody }
	} else {
		st := newAnthropicMessagesStub(t)
		stubURL, upBody = st.srv.URL, func() []byte { return st.gotBody }
	}
	u, err := url.Parse(stubURL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorAnthropic: provider.NewAnthropicClient(&http.Client{Transport: &rewriteHostTransport{stub: u}}),
	}, provider.NewMock())
	seedAPIKeyAnthropicAccount(t, routes, cipher, "usr_va_a", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	enableVendorAccountsFlag(t, srv)
	return &anthropicCacheDispatchFixture{
		srv:    srv,
		upBody: upBody,
		flagSet: func(on bool) {
			rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"anthropic_prompt_caching_enabled":`+map[bool]string{true: "true", false: "false"}[on]+`}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("set anthropic_prompt_caching_enabled=%v: status = %d, body = %s", on, rec.Code, rec.Body.String())
			}
		},
	}
}

func (f *anthropicCacheDispatchFixture) chat(t *testing.T, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestAnthropicCachePolicyAtDispatch drives a real chat-completions request through
// the gateway to an Anthropic api-key account and asserts what the UPSTREAM sees:
// cache_control only when the settings flag is on AND the request is eligible, on
// both the buffered (inference_complete.go) and streaming (stream_session.go)
// dispatch hooks. Flipping the flag through the real settings endpoint takes effect
// on the very next request (the gateway-side cache is invalidated on PUT).
func TestAnthropicCachePolicyAtDispatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			f := newAnthropicCacheDispatchFixture(t, stream)
			history := anthropicChatBody(t, stream, cacheBigHistory()...)
			oneShot := anthropicChatBody(t, stream, cacheBigHistory()[:2]...)

			// Default: the flag is off, so nothing changes for an eligible request.
			f.chat(t, history)
			if got := string(f.upBody()); strings.Contains(got, cacheControlKey) {
				t.Fatalf("flag off (default): upstream body must not carry cache_control: %s", got)
			}

			// Flag on + eligible (assistant history, large prefix): both breakpoints.
			f.flagSet(true)
			f.chat(t, history)
			got := string(f.upBody())
			if n := strings.Count(got, cacheControlKey); n != 2 {
				t.Fatalf("flag on + eligible: cache_control count = %d, want 2 (system + last turn): %s", n, got)
			}

			// Flag on but a one-shot request (no assistant history, not a chat
			// session): the cache write would never be read, so it is skipped.
			f.chat(t, oneShot)
			if got := string(f.upBody()); strings.Contains(got, cacheControlKey) {
				t.Fatalf("flag on + one-shot: upstream body must not carry cache_control: %s", got)
			}

			// Turning the flag back off applies to the very next request, even though
			// the previous request just primed the gateway-side cache with "on".
			f.flagSet(false)
			f.chat(t, history)
			if got := string(f.upBody()); strings.Contains(got, cacheControlKey) {
				t.Fatalf("flag turned off: upstream body must not carry cache_control (stale cache?): %s", got)
			}
		})
	}
}
