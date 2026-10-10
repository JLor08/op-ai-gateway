// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

const cacheControlKey = "cache_control"

// cachedBody renders req for target and decodes the Messages body.
func cachedBody(t *testing.T, target routing.Target, req inference.Request) map[string]any {
	t.Helper()
	raw, err := anthropicRequestBody(target, req, true)
	if err != nil {
		t.Fatalf("anthropicRequestBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}
	return body
}

// assertEphemeral fails unless block carries exactly {"type":"ephemeral"} —
// v1 is the 5-minute default, so no ttl field may be emitted.
func assertEphemeral(t *testing.T, what string, block map[string]any) {
	t.Helper()
	cc, ok := block[cacheControlKey].(map[string]any)
	if !ok {
		t.Fatalf("%s: no cache_control: %v", what, block)
	}
	if cc["type"] != "ephemeral" {
		t.Fatalf("%s: cache_control type = %v, want ephemeral", what, cc["type"])
	}
	if _, hasTTL := cc["ttl"]; hasTTL || len(cc) != 1 {
		t.Fatalf("%s: cache_control must be exactly {type:ephemeral}, got %v", what, cc)
	}
}

// lastTurnLastBlock returns the last content block of the last message.
func lastTurnLastBlock(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in body: %v", body)
	}
	content, ok := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("last message has no content blocks: %v", msgs[len(msgs)-1])
	}
	return content[len(content)-1].(map[string]any)
}

func cacheTestMessages() []inference.Message {
	return []inference.Message{
		anthropicTextMsg(inference.RoleSystem, "you are helpful and this system text is long enough"),
		anthropicTextMsg(inference.RoleUser, "hi"),
		anthropicTextMsg(inference.RoleAssistant, "hello"),
		anthropicTextMsg(inference.RoleUser, "again"),
	}
}

func TestAnthropicRequestBodyCacheControl(t *testing.T) {
	req := inference.Request{
		Messages:    cacheTestMessages(),
		PromptCache: &inference.PromptCacheDirective{Enabled: true},
	}
	target := routing.Target{ProviderModel: "claude-sonnet-5-5"}

	t.Run("api-key string system becomes a block array with cache_control on the last block", func(t *testing.T) {
		raw, err := anthropicRequestBody(target, req, true)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
		}

		sys, ok := body["system"].([]any)
		if !ok {
			t.Fatalf("system is not a block array: %T", body["system"])
		}
		if len(sys) != 1 {
			t.Fatalf("system blocks = %d, want 1", len(sys))
		}
		last := sys[len(sys)-1].(map[string]any)
		if last["text"] != "you are helpful and this system text is long enough" {
			t.Fatalf("system text changed: %v", last["text"])
		}
		assertEphemeral(t, "last system block", last)

		assertEphemeral(t, "last turn's last block", lastTurnLastBlock(t, body))

		// Exactly two breakpoints: no earlier turn is marked.
		if n := strings.Count(string(raw), cacheControlKey); n != 2 {
			t.Fatalf("cache_control count = %d, want 2 (system + last turn)\n%s", n, raw)
		}
	})

	t.Run("only the last block of a multi-block last turn is marked", func(t *testing.T) {
		multi := req
		multi.Messages = append(append([]inference.Message{}, cacheTestMessages()...),
			// A tool-result turn merged with following user text: two blocks in one turn.
			inference.Message{Role: inference.RoleTool, ToolCallID: "toolu_1", Content: []inference.ContentPart{{Type: inference.ContentText, Text: "result"}}},
			anthropicTextMsg(inference.RoleUser, "thanks"),
		)
		body := cachedBody(t, target, multi)
		msgs := body["messages"].([]any)
		content := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
		if len(content) < 2 {
			t.Fatalf("expected a multi-block last turn, got %v", content)
		}
		for i, b := range content[:len(content)-1] {
			if _, marked := b.(map[string]any)[cacheControlKey]; marked {
				t.Fatalf("block %d of the last turn must not carry cache_control: %v", i, b)
			}
		}
		assertEphemeral(t, "last block", content[len(content)-1].(map[string]any))
	})

	t.Run("flag off is byte-identical to no directive", func(t *testing.T) {
		reqOff := req
		reqOff.PromptCache = nil
		off, err := anthropicRequestBody(target, reqOff, true)
		if err != nil {
			t.Fatal(err)
		}
		bare, err := anthropicRequestBody(target, inference.Request{Messages: req.Messages}, true)
		if err != nil {
			t.Fatal(err)
		}
		if string(off) != string(bare) {
			t.Fatalf("directive-nil render differs from bare render\n off: %s\nbare: %s", off, bare)
		}
		if strings.Contains(string(off), cacheControlKey) {
			t.Fatalf("flag-off body must not contain cache_control: %s", off)
		}
		var body map[string]any
		if err := json.Unmarshal(off, &body); err != nil {
			t.Fatal(err)
		}
		if _, isString := body["system"].(string); !isString {
			t.Fatalf("flag-off system must stay a plain string, got %T", body["system"])
		}

		// An explicit Enabled=false directive is also inert.
		reqDisabled := req
		reqDisabled.PromptCache = &inference.PromptCacheDirective{Enabled: false}
		disabled, err := anthropicRequestBody(target, reqDisabled, true)
		if err != nil {
			t.Fatal(err)
		}
		if string(disabled) != string(bare) {
			t.Fatalf("Enabled=false render differs from bare render\n got: %s\nbare: %s", disabled, bare)
		}
	})

	t.Run("a 1h TTL on the directive still emits no ttl field in v1", func(t *testing.T) {
		reqTTL := req
		reqTTL.PromptCache = &inference.PromptCacheDirective{Enabled: true, TTL: "1h"}
		raw, err := anthropicRequestBody(target, reqTTL, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"ttl"`) {
			t.Fatalf("v1 must not emit ttl: %s", raw)
		}
	})
}

func TestAnthropicRequestBodyCacheControlMasquerade(t *testing.T) {
	target := routing.Target{ProviderModel: "claude-sonnet-5-5", Masquerade: routing.MasqueradeClaudeCode}

	t.Run("caller system text: first block untouched, last block cached", func(t *testing.T) {
		req := inference.Request{
			Messages:    cacheTestMessages(),
			PromptCache: &inference.PromptCacheDirective{Enabled: true},
		}
		body := cachedBody(t, target, req)
		sys, ok := body["system"].([]any)
		if !ok || len(sys) != 2 {
			t.Fatalf("system = %v, want a 2-block array", body["system"])
		}
		first := sys[0].(map[string]any)
		if first["text"] != claudeCodeSystemPrompt {
			t.Fatalf("first system block text = %v, want the exact claudeCodeSystemPrompt", first["text"])
		}
		if _, marked := first[cacheControlKey]; marked {
			t.Fatalf("first (masquerade) block must not carry cache_control: %v", first)
		}
		second := sys[1].(map[string]any)
		if second["text"] != "you are helpful and this system text is long enough" {
			t.Fatalf("second system block text = %v", second["text"])
		}
		assertEphemeral(t, "last system block", second)
		assertEphemeral(t, "last turn's last block", lastTurnLastBlock(t, body))
	})

	t.Run("no caller system text: the sole masquerade block is both first and last", func(t *testing.T) {
		req := inference.Request{
			Messages: []inference.Message{
				anthropicTextMsg(inference.RoleUser, "hi"),
			},
			PromptCache: &inference.PromptCacheDirective{Enabled: true},
		}
		body := cachedBody(t, target, req)
		sys, ok := body["system"].([]any)
		if !ok || len(sys) != 1 {
			t.Fatalf("system = %v, want a 1-block array", body["system"])
		}
		only := sys[0].(map[string]any)
		if only["text"] != claudeCodeSystemPrompt {
			t.Fatalf("system block text = %v, want the exact claudeCodeSystemPrompt", only["text"])
		}
		assertEphemeral(t, "sole system block", only)
		assertEphemeral(t, "last turn's last block", lastTurnLastBlock(t, body))
	})

	t.Run("flag off keeps the masquerade render free of cache_control", func(t *testing.T) {
		req := inference.Request{Messages: cacheTestMessages()}
		raw, err := anthropicRequestBody(target, req, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), cacheControlKey) {
			t.Fatalf("flag-off masquerade body must not contain cache_control: %s", raw)
		}
	})
}

func TestAnthropicRequestBodyCacheControlEmptySystem(t *testing.T) {
	// An api-key request with no system text and caching enabled: nothing to mark
	// on the system side (the field stays omitted), but the last turn is still
	// marked and the body is valid JSON.
	req := inference.Request{
		Messages: []inference.Message{
			anthropicTextMsg(inference.RoleUser, "hi"),
			anthropicTextMsg(inference.RoleAssistant, "hello"),
			anthropicTextMsg(inference.RoleUser, "again"),
		},
		PromptCache: &inference.PromptCacheDirective{Enabled: true},
	}
	target := routing.Target{ProviderModel: "claude-sonnet-5-5"}

	raw, err := anthropicRequestBody(target, req, true)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}
	if sys, present := body["system"]; present {
		t.Fatalf("system must be omitted when there is no system text, got %v", sys)
	}
	assertEphemeral(t, "last turn's last block", lastTurnLastBlock(t, body))
	if n := strings.Count(string(raw), cacheControlKey); n != 1 {
		t.Fatalf("cache_control count = %d, want 1 (last turn only)\n%s", n, raw)
	}
}

// stripCacheMarkers returns a deep copy of the decoded Messages turns with every
// cache_control field removed, so two renders can be compared on content alone.
func stripCacheMarkers(t *testing.T, turns []json.RawMessage) []any {
	t.Helper()
	out := make([]any, 0, len(turns))
	for _, raw := range turns {
		var turn map[string]any
		if err := json.Unmarshal(raw, &turn); err != nil {
			t.Fatalf("turn is not valid JSON: %v\n%s", err, raw)
		}
		for _, b := range turn["content"].([]any) {
			delete(b.(map[string]any), cacheControlKey)
		}
		out = append(out, turn)
	}
	return out
}

// TestAnthropicCachedPrefixIsStableAcrossTurns pins the property prompt caching
// lives on: across two consecutive turns of a growing conversation, everything the
// first turn cached is rendered byte-for-byte the same in the second. The `system`
// and `tools` fields must be byte-identical (not merely structurally equal, so a
// non-deterministic key order would fail), and the first turn's messages -- minus
// the moving cache_control marker on its last block -- must be exactly the leading
// messages of the second. No live API is involved.
func TestAnthropicCachedPrefixIsStableAcrossTurns(t *testing.T) {
	tools := []inference.Tool{{
		Name:        "shell",
		Description: "run a shell command",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd": map[string]any{"type": "string", "description": "the command"},
				"cwd": map[string]any{"type": "string", "description": "working directory"},
				"env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"tty": map[string]any{"type": "boolean"},
			},
			"required": []any{"cmd"},
		},
	}}
	sys := anthropicTextMsg(inference.RoleSystem, strings.Repeat("sys ", 300))
	cache := &inference.PromptCacheDirective{Enabled: true}
	turn1 := inference.Request{
		Messages: []inference.Message{
			sys,
			anthropicTextMsg(inference.RoleUser, "q1"),
			anthropicTextMsg(inference.RoleAssistant, "a1"),
			anthropicTextMsg(inference.RoleUser, "q2"),
		},
		Tools:       tools,
		PromptCache: cache,
	}
	turn2 := turn1
	turn2.Messages = append(append([]inference.Message{}, turn1.Messages...),
		anthropicTextMsg(inference.RoleAssistant, "a2"),
		anthropicTextMsg(inference.RoleUser, "q3"),
	)

	targets := map[string]routing.Target{
		"api-key":                 {ProviderModel: "claude-sonnet-5-5"},
		"subscription masquerade": {ProviderModel: "claude-sonnet-5-5", Masquerade: routing.MasqueradeClaudeCode},
	}
	for name, target := range targets {
		t.Run(name, func(t *testing.T) {
			b1, err := anthropicRequestBody(target, turn1, true)
			if err != nil {
				t.Fatal(err)
			}
			b2, err := anthropicRequestBody(target, turn2, true)
			if err != nil {
				t.Fatal(err)
			}

			// The same request renders to the same bytes every time (Go map
			// iteration order must not leak into the body).
			for i := range 20 {
				again, err := anthropicRequestBody(target, turn1, true)
				if err != nil {
					t.Fatal(err)
				}
				if string(again) != string(b1) {
					t.Fatalf("render %d of the same request differs:\n%s\n%s", i, b1, again)
				}
			}

			var f1, f2 map[string]json.RawMessage
			if err := json.Unmarshal(b1, &f1); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b2, &f2); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"system", "tools"} {
				if len(f1[field]) == 0 {
					t.Fatalf("turn 1 has no %s field: %s", field, b1)
				}
				if string(f1[field]) != string(f2[field]) {
					t.Fatalf("%s (cached stable prefix) differs across turns:\n%s\n%s", field, f1[field], f2[field])
				}
			}

			var m1, m2 []json.RawMessage
			if err := json.Unmarshal(f1["messages"], &m1); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(f2["messages"], &m2); err != nil {
				t.Fatal(err)
			}
			if len(m2) <= len(m1) {
				t.Fatalf("turn 2 must extend turn 1: %d vs %d messages", len(m2), len(m1))
			}
			prefix1, prefix2 := stripCacheMarkers(t, m1), stripCacheMarkers(t, m2[:len(m1)])
			j1, _ := json.Marshal(prefix1)
			j2, _ := json.Marshal(prefix2)
			if string(j1) != string(j2) {
				t.Fatalf("turn 1's messages are not the leading messages of turn 2 (marker aside):\n%s\n%s", j1, j2)
			}
		})
	}
}
