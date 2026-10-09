// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

func masqueradeReq() inference.Request {
	return inference.Request{
		Model: "claude-sonnet",
		Messages: []inference.Message{
			{Role: inference.RoleSystem, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "Be terse."}}},
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}},
		},
	}
}

// TestAnthropicMasqueradePrependsClaudeCodeSystemBlock proves that with
// Masquerade == claude_code the `system` field is an ARRAY whose FIRST block is
// EXACTLY the Claude-Code line and whose second block is the caller's own system
// text.
func TestAnthropicMasqueradePrependsClaudeCodeSystemBlock(t *testing.T) {
	target := routing.Target{Masquerade: routing.MasqueradeClaudeCode}
	raw, err := anthropicRequestBody(target, masqueradeReq(), false)
	if err != nil {
		t.Fatalf("anthropicRequestBody: %v", err)
	}
	var body struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("system is not an array of blocks: %v: %s", err, raw)
	}
	if len(body.System) != 2 {
		t.Fatalf("system has %d blocks, want 2 (masquerade + user system): %s", len(body.System), raw)
	}
	if body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("first system block = %q, want the exact Claude-Code line", body.System[0].Text)
	}
	if body.System[0].Type != "text" {
		t.Fatalf("first system block type = %q, want text", body.System[0].Type)
	}
	if body.System[1].Text != "Be terse." {
		t.Fatalf("second system block = %q, want the caller's system text", body.System[1].Text)
	}
}

// TestAnthropicMasqueradeWithNoUserSystem proves the masquerade still emits the
// Claude-Code block as the sole system block when the caller sent no system text.
func TestAnthropicMasqueradeWithNoUserSystem(t *testing.T) {
	req := inference.Request{
		Model:    "claude-sonnet",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}},
	}
	raw, err := anthropicRequestBody(routing.Target{Masquerade: routing.MasqueradeClaudeCode}, req, false)
	if err != nil {
		t.Fatalf("anthropicRequestBody: %v", err)
	}
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("system is not an array: %v: %s", err, raw)
	}
	if len(body.System) != 1 || body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("system = %+v, want a single exact Claude-Code block", body.System)
	}
}

// TestAnthropicNoMasqueradeKeepsStringSystem proves the ordinary (no masquerade)
// path is unchanged: `system` is the plain joined string, and absent when empty.
func TestAnthropicNoMasqueradeKeepsStringSystem(t *testing.T) {
	raw, err := anthropicRequestBody(routing.Target{}, masqueradeReq(), false)
	if err != nil {
		t.Fatalf("anthropicRequestBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := body["system"].(string); !ok || got != "Be terse." {
		t.Fatalf("system = %v (%T), want the plain string \"Be terse.\"", body["system"], body["system"])
	}

	// And absent entirely when there is no system text.
	noSys := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	raw, err = anthropicRequestBody(routing.Target{}, noSys, false)
	if err != nil {
		t.Fatalf("anthropicRequestBody: %v", err)
	}
	var body2 map[string]any
	if err := json.Unmarshal(raw, &body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body2["system"]; present {
		t.Fatalf("system present = %v, want absent when there is no system text and no masquerade", body2["system"])
	}
}

// ---- ProxyNative: subscription masquerade injection ----

// claudeCodeBlockJSON is the Claude-Code system block as it must appear on the wire.
const claudeCodeBlockJSON = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}`

func masqueradeTarget(endpoint string) routing.Target {
	target := anthropicTarget(endpoint)
	target.Masquerade = routing.MasqueradeClaudeCode
	return target
}

// proxyNativeCapture relays body through ProxyNative against a fake upstream and
// returns the capture of the one request the upstream saw.
func proxyNativeCapture(t *testing.T, target func(endpoint string) routing.Target, body string) *anthropicCapture {
	t.Helper()
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	ctx := WithUpstreamAuth(context.Background(), "", "sk-ant-oat-test")

	resp, err := NewAnthropicClient(http.DefaultClient).ProxyNative(ctx, target(upstream.URL), "/v1/messages", []byte(body))
	if err != nil {
		t.Fatalf("ProxyNative returned %v", err)
	}
	resp.Body.Close()
	return capture
}

// TestAnthropicClientProxyNativeMasqueradeInjectsClaudeCodeBlock pins the
// subscription (OAuth) passthrough: the Claude-Code line becomes the FIRST system
// block whatever shape the client's `system` had, every other byte-level VALUE
// reaches the upstream untouched.
func TestAnthropicClientProxyNativeMasqueradeInjectsClaudeCodeBlock(t *testing.T) {
	tests := []struct {
		name       string
		system     string // the client's `system` member, including its key and a leading comma; empty = absent
		wantSystem string
	}{
		{"absent", ``, `[` + claudeCodeBlockJSON + `]`},
		{"null", `,"system":null`, `[` + claudeCodeBlockJSON + `]`},
		{"empty string", `,"system":""`, `[` + claudeCodeBlockJSON + `]`},
		{"string", `,"system":"Be terse."`, `[` + claudeCodeBlockJSON + `,{"type":"text","text":"Be terse."}]`},
		{"empty array", `,"system":[]`, `[` + claudeCodeBlockJSON + `]`},
		{
			"array without the block keeps every block and its cache_control",
			`,"system":[{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"Second","x_extra":[1,2]}]`,
			`[` + claudeCodeBlockJSON + `,{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"Second","x_extra":[1,2]}]`,
		},
		{
			"array whose first block is a different Claude Code line",
			`,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude. Extra."}]`,
			`[` + claudeCodeBlockJSON + `,{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude. Extra."}]`,
		},
		{
			"array with a non-object first element is still prepended to",
			`,"system":["Be terse."]`,
			`[` + claudeCodeBlockJSON + `,"Be terse."]`,
		},
		{
			// The text equals the Claude-Code line exactly; only the `type` conjunct of
			// startsWithClaudeCodeBlock says it is not the block, so it must NOT be
			// taken for the one already present.
			"array whose first block has the exact text but type image",
			`,"system":[{"type":"image","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`,
			`[` + claudeCodeBlockJSON + `,{"type":"image","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`,
		},
		{
			"array whose first block has the exact text but no type",
			`,"system":[{"text":"You are Claude Code, Anthropic's official CLI for Claude."}]`,
			`[` + claudeCodeBlockJSON + `,{"text":"You are Claude Code, Anthropic's official CLI for Claude."}]`,
		},
		{
			"array holding the block but not first",
			`,"system":[{"type":"text","text":"Be terse."},` + claudeCodeBlockJSON + `]`,
			`[` + claudeCodeBlockJSON + `,{"type":"text","text":"Be terse."},` + claudeCodeBlockJSON + `]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// 12345678901234567890 exceeds float64 precision, and 1.50 would lose its
			// trailing zero as a float64: a decode that is not value-lossless
			// (UseNumber) would rewrite either.
			in := `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"stream":true` + tc.system +
				`,"messages":[{"role":"user","content":"h\u00e9llo <b>"}],"x_big":12345678901234567890,"x_float":1.50,"x_unknown":{"b":1,"a":[3,2,1]}}`

			capture := proxyNativeCapture(t, masqueradeTarget, in)

			_, _, _, raw := capture.request()
			body := capture.body(t)
			assertJSON(t, "system", body["system"], tc.wantSystem)
			// Everything but `system` is the client's, value for value.
			var want map[string]any
			dec := json.NewDecoder(strings.NewReader(in))
			dec.UseNumber()
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("decode input: %v", err)
			}
			delete(want, "system")
			got := map[string]any{}
			dec = json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decode forwarded body: %v", err)
			}
			delete(got, "system")
			assertJSON(t, "body without system", got, mustJSON(t, want))
			if !strings.Contains(string(raw), "12345678901234567890") {
				t.Fatalf("forwarded body = %s, want the big integer relayed value-lossless", raw)
			}
			if !strings.Contains(string(raw), `"x_float":1.50`) {
				t.Fatalf("forwarded body = %s, want the float's spelling 1.50 relayed value-lossless", raw)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAnthropicClientProxyNativeMasqueradeIsIdempotent pins the real Claude Code
// client: it already sends the exact block first, so the injection is a no-op and
// the body goes out exactly as the client wrote it (not re-marshalled).
func TestAnthropicClientProxyNativeMasqueradeIsIdempotent(t *testing.T) {
	tests := []struct{ name, body string }{
		{
			"block first, more blocks follow",
			"{ \"model\":\"m\",\n \"system\":[" + claudeCodeBlockJSON + `,{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral"}}],"x":1.50 }`,
		},
		{
			"block first and alone, with its own cache_control",
			`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],"model":"m"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture := proxyNativeCapture(t, masqueradeTarget, tc.body)

			_, _, _, raw := capture.request()
			if string(raw) != tc.body {
				t.Fatalf("forwarded body = %q, want it unchanged %q", raw, tc.body)
			}
		})
	}
}

// TestAnthropicClientProxyNativeWithoutMasqueradeRelaysBodyByteIdentical pins the
// api-key path (#185): a target with no masquerade never has its body touched, so
// the client's `system` -- string, array, or absent -- reaches the upstream as
// written, byte for byte.
func TestAnthropicClientProxyNativeWithoutMasqueradeRelaysBodyByteIdentical(t *testing.T) {
	bodies := map[string]string{
		"string system": anthropicNativeBody,
		"array system":  "{\"model\":\"m\",\n\"system\":[{\"type\":\"text\",\"text\":\"Be terse.\"}],\"x\":1.50}",
		"absent system": `{ "model" : "m" ,"messages":[]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			capture := proxyNativeCapture(t, anthropicTarget, body)

			_, _, _, raw := capture.request()
			if string(raw) != body {
				t.Fatalf("forwarded body = %q, want it byte-identical %q", raw, body)
			}
		})
	}
}

// TestAnthropicClientProxyNativeMasqueradeFailsOpenOnUninjectableBody pins that a
// body the injection cannot safely edit is relayed unchanged -- the upstream's own
// 400 is the answer, not a gateway-made one.
func TestAnthropicClientProxyNativeMasqueradeFailsOpenOnUninjectableBody(t *testing.T) {
	bodies := map[string]string{
		"not JSON":            `not json at all`,
		"empty body":          ``,
		"truncated object":    `{"model":"m","system":`,
		"JSON array":          `[{"model":"m"}]`,
		"JSON null":           `null`,
		"JSON string":         `"hello"`,
		"trailing data":       `{"model":"m"} {"x":1}`,
		"system is an object": `{"model":"m","system":{"type":"text","text":"x"}}`,
		"system is a number":  `{"model":"m","system":7}`,
		"system is a bool":    `{"model":"m","system":true}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			capture := proxyNativeCapture(t, masqueradeTarget, body)

			_, _, _, raw := capture.request()
			if string(raw) != body {
				t.Fatalf("forwarded body = %q, want it relayed unchanged %q", raw, body)
			}
		})
	}
}

// TestAnthropicClientProxyNativeMasqueradeKeepsTransportContract pins that the
// injection changes the body only: the response is still relayed byte-for-byte,
// anthropic-version is still sent exactly once, and the (bearer) credential and a
// ctx-carried anthropic-beta ride along.
func TestAnthropicClientProxyNativeMasqueradeKeepsTransportContract(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicNativeSSE)
	}))
	defer upstream.Close()
	ctx := WithUpstreamAuthHeaders(context.Background(), "", "sk-ant-oat-test", map[string]string{"anthropic-beta": "oauth-2025-04-20"})

	resp, err := NewAnthropicClient(http.DefaultClient).ProxyNative(ctx, masqueradeTarget(upstream.URL+"/"), "/v1/messages", []byte(anthropicNativeBody))
	if err != nil {
		t.Fatalf("ProxyNative returned %v", err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read relayed body: %v", err)
	}
	if string(out) != anthropicNativeSSE {
		t.Fatalf("relayed SSE = %q, want it byte-for-byte %q", out, anthropicNativeSSE)
	}
	method, path, header, _ := capture.request()
	if method != http.MethodPost || path != "/v1/messages" {
		t.Fatalf("request = %s %s, want POST /v1/messages", method, path)
	}
	if got := header.Values("anthropic-version"); len(got) != 1 || got[0] != "2023-06-01" {
		t.Fatalf("anthropic-version = %v, want exactly [2023-06-01]", got)
	}
	if got := header.Get("Authorization"); got != "Bearer sk-ant-oat-test" {
		t.Fatalf("Authorization = %q, want the bearer from ctx", got)
	}
	if got := header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q, want the ctx value forwarded", got)
	}
	// No CLI-fingerprint headers: those are a deliberate follow-up, not this change.
	for _, name := range []string{"x-app", "X-Stainless-Lang", "X-Claude-Code-Session-Id"} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q, want it absent", name, got)
		}
	}
	var injected struct {
		System []map[string]any `json:"system"`
	}
	_, _, _, raw := capture.request()
	if err := json.Unmarshal(raw, &injected); err != nil || len(injected.System) != 2 {
		t.Fatalf("forwarded system = %s (err %v), want [block, original string as a block]", raw, err)
	}
}

// TestAnthropicClientProxyNativeMasqueradeDoesNotFollowRedirects pins that the
// masquerade target keeps the no-redirect guarantee: the credential never leaves
// for a redirect target.
func TestAnthropicClientProxyNativeMasqueradeDoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var hits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	ctx := WithUpstreamAuth(context.Background(), "", "sk-ant-oat-secret")

	resp, err := NewAnthropicClient(&http.Client{}).ProxyNative(ctx, masqueradeTarget(upstream.URL), "/v1/messages", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("ProxyNative returned %v, want the 307 handed back as a response", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("StatusCode = %d, want the 307 returned unfollowed", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the redirect target received %d request(s), want 0", hits)
	}
}
