// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

// TestApplyChatTemplateEffortPolicy walks the directive over the request flavors
// and target kinds that matter. The APIFlavor rows are the load-bearing ones: a
// DIRECT OpenAI chat request (or a Responses request) that merely carries a
// reasoning effort is addressed to a target that already understands the
// top-level field, and must never acquire the chat_template_kwargs directive.
func TestApplyChatTemplateEffortPolicy(t *testing.T) {
	llama := routing.Target{Provider: routing.ProviderLlamaCPP}
	vllm := routing.Target{Provider: routing.ProviderVLLM}
	agentLlama := routing.Target{Provider: routing.ProviderServerAgent, LiveProgressSpecType: string(routing.RuntimeSpecTypeLlamaCpp)}
	cases := []struct {
		name string
		// target and req are the inputs; want is the directive the policy must set,
		// wantEffort is the ReasoningEffort the policy must LEAVE on the request.
		// For an Anthropic-translate request whose target does NOT read
		// chat_template_kwargs the effort is cleared (confining the whole feature --
		// top-level reasoning_effort, the kwargs directive, and the Gemini/Responses
		// renderings -- to capable targets); every other row leaves it untouched.
		target     routing.Target
		req        inference.Request
		want       bool
		wantEffort string
	}{
		{"anthropic + effort + llama.cpp", llama, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, true, "low"},
		{"anthropic + effort + vLLM", vllm, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "high"}, true, "high"},
		{"anthropic + effort + agent-managed llama.cpp", agentLlama, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "medium"}, true, "medium"},
		// Non-capable Anthropic-translate targets: the effort is CLEARED, so it never
		// reaches the generic-OpenAI top-level field, the Gemini thinkingConfig, or a
		// Responses reasoning.effort. (§3 scope / §4.7 "other backends unchanged".)
		{"anthropic + effort + ollama (does not read the kwarg)", routing.Target{Provider: routing.ProviderOllama}, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, false, ""},
		{"anthropic + effort + llama_swap (not llama.cpp)", routing.Target{Provider: routing.ProviderLlamaSwap}, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, false, ""},
		{"anthropic + effort + vendor OpenAI (no top-level reasoning_effort leak)", routing.Target{Provider: routing.ProviderVendorOpenAI}, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, false, ""},
		{"anthropic + effort + vendor Gemini (no thinkingConfig leak)", routing.Target{Provider: routing.ProviderVendorGemini}, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, false, ""},
		{"anthropic + effort + zero target", routing.Target{}, inference.Request{APIFlavor: "anthropic_messages", ReasoningEffort: "low"}, false, ""},
		{"anthropic + no effort + llama.cpp", llama, inference.Request{APIFlavor: "anthropic_messages"}, false, ""},
		// The direct-OpenAI guard: reasoning_effort set, capable target, wrong flavor.
		// These early-return on the APIFlavor gate, so the effort is left as the
		// caller sent it -- the gateway must not touch a direct request's own field.
		{"direct openai chat + reasoning_effort + llama.cpp", llama, inference.Request{APIFlavor: "openai_chat_completions", ReasoningEffort: "low"}, false, "low"},
		{"direct openai chat + reasoning_effort + vLLM", vllm, inference.Request{APIFlavor: "openai_chat_completions", ReasoningEffort: "high"}, false, "high"},
		{"openai responses + reasoning.effort + llama.cpp", llama, inference.Request{APIFlavor: inference.APIFlavorOpenAIResponses, ReasoningEffort: "low"}, false, "low"},
		{"unset flavor + effort + llama.cpp", llama, inference.Request{ReasoningEffort: "low"}, false, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			applyChatTemplateEffortPolicy(tc.target, &req)
			if req.ReasoningEffortViaTemplateKwargs != tc.want {
				t.Fatalf("ReasoningEffortViaTemplateKwargs = %v, want %v", req.ReasoningEffortViaTemplateKwargs, tc.want)
			}
			if req.ReasoningEffort != tc.wantEffort {
				t.Fatalf("ReasoningEffort = %q, want %q", req.ReasoningEffort, tc.wantEffort)
			}
		})
	}

	// The policy mutates only the directive on the copy it is handed.
	t.Run("mutates only the directive", func(t *testing.T) {
		req := inference.Request{APIFlavor: "anthropic_messages", Model: "m", ReasoningEffort: "low", MaxTokens: 7}
		applyChatTemplateEffortPolicy(llama, &req)
		if !req.ReasoningEffortViaTemplateKwargs || req.Model != "m" || req.MaxTokens != 7 || req.APIFlavor != "anthropic_messages" {
			t.Fatalf("unexpected request after policy: %+v", req)
		}
	})
}

// completeRecorder is a provider fake that records the neutral inference.Request
// each dispatch hands the provider, for BOTH the buffered (Complete) and the
// streaming (CompleteStream) hooks. It is what proves the directive is set on
// the upstream copy at the two real dispatch sites, not merely by the function.
type completeRecorder struct {
	mu   sync.Mutex
	reqs []inference.Request
}

func (p *completeRecorder) record(req inference.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
}

func (p *completeRecorder) only(t *testing.T) inference.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1", len(p.reqs))
	}
	return p.reqs[0]
}

func (p *completeRecorder) Complete(_ context.Context, _ routing.Target, req inference.Request) (provider.Response, error) {
	p.record(req)
	return provider.Response{Text: "ok", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

func (p *completeRecorder) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	p.record(req)
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
}

// translateMessagesBody is a Claude-Code-shaped Messages request that is
// TRANSLATED (the seeded application is not on passthrough), carrying effort.
func translateMessagesBody(stream bool, outputConfig string) string {
	b, _ := json.Marshal(stream)
	extra := ""
	if outputConfig != "" {
		extra = `,"output_config":` + outputConfig
	}
	return `{"model":"gw-model","max_tokens":64,"stream":` + string(b) + `,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

func postTo(srv *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestTranslateEffortReachesTheProviderAtBothDispatchSites drives real
// /v1/messages requests through the whole server onto the TRANSLATE path and
// asserts what the provider is handed: the effort and the template-kwargs
// directive when the token flag is on and the target is capable, and neither
// when the flag is off, even for a malformed output_config (which must not be
// newly rejected). Covered for stream:false (inference_complete.go) and
// stream:true (stream_session.go).
func TestTranslateEffortReachesTheProviderAtBothDispatchSites(t *testing.T) {
	for _, stream := range []bool{false, true} {
		suffix := map[bool]string{false: "/buffered", true: "/stream"}[stream]

		t.Run("flag on + llama.cpp sets effort and directive"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true, translate: true})
			rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":"low"}`))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := prov.only(t)
			if got.ReasoningEffort != "low" || !got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("provider request effort=%q directive=%v, want low + directive", got.ReasoningEffort, got.ReasoningEffortViaTemplateKwargs)
			}
		})

		t.Run("flag on + vLLM sets effort and directive"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderVLLM, flag: true, translate: true})
			if rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":"high"}`)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := prov.only(t)
			if got.ReasoningEffort != "high" || !got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("provider request effort=%q directive=%v, want high + directive", got.ReasoningEffort, got.ReasoningEffortViaTemplateKwargs)
			}
		})

		t.Run("flag on + non-capable target clears the effort and sets no directive"+suffix, func(t *testing.T) {
			// A non-capable Anthropic-translate target (ollama here, same for
			// generic-OpenAI and Gemini) must see NEITHER the directive NOR a
			// surviving ReasoningEffort: a leaked effort would turn into a top-level
			// reasoning_effort (openAISamplingFields) or a Gemini thinkingConfig on a
			// backend the operator never opted that route into (§3 scope / §4.7).
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderOllama, flag: true, translate: true})
			if rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":"low"}`)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := prov.only(t)
			if got.ReasoningEffort != "" || got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("provider request effort=%q directive=%v, want empty effort + NO directive", got.ReasoningEffort, got.ReasoningEffortViaTemplateKwargs)
			}
		})

		t.Run("flag off ignores output_config entirely"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: false, translate: true})
			if rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":"low"}`)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := prov.only(t)
			if got.ReasoningEffort != "" || got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("flag off: effort=%q directive=%v, want neither", got.ReasoningEffort, got.ReasoningEffortViaTemplateKwargs)
			}
		})

		t.Run("flag off never rejects a malformed output_config"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: false, translate: true})
			rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":3}`))
			if rec.Code != http.StatusOK {
				t.Fatalf("a flag-off request with a malformed output_config must parse as before: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := prov.only(t)
			if got.ReasoningEffort != "" || got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("flag off: effort=%q directive=%v, want neither", got.ReasoningEffort, got.ReasoningEffortViaTemplateKwargs)
			}
		})

		t.Run("flag on rejects a malformed effort before dispatch"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true, translate: true})
			rec := postTo(srv, "/v1/messages", translateMessagesBody(stream, `{"effort":3}`))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "messages.invalid_output_config") {
				t.Fatalf("body = %s, want the messages.invalid_output_config code", rec.Body.String())
			}
			prov.mu.Lock()
			defer prov.mu.Unlock()
			if len(prov.reqs) != 0 {
				t.Fatalf("provider saw %d requests, want 0 (rejected before upstream)", len(prov.reqs))
			}
		})

		// The direct-OpenAI guard at the dispatch level: a plain chat-completions
		// request never carries the directive, whatever the token flag or target.
		t.Run("direct OpenAI chat stays directive-free on llama.cpp"+suffix, func(t *testing.T) {
			prov := &completeRecorder{}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true, translate: true})
			b, _ := json.Marshal(stream)
			body := `{"model":"gw-model","stream":` + string(b) + `,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`
			if rec := postTo(srv, "/v1/chat/completions", body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := prov.only(t); got.ReasoningEffortViaTemplateKwargs {
				t.Fatalf("a direct OpenAI chat request must not acquire the directive: %+v", got)
			}
		})
	}
}
