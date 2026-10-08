// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
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
