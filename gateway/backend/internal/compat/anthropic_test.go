// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"strings"
	"testing"
)

func TestParseAnthropicMessages(t *testing.T) {
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"system":"You are careful.",
		"max_tokens":256,
		"stream":true,
		"messages":[{"role":"user","content":"Refactor this."}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}

	if req.APIFlavor != "anthropic_messages" {
		t.Fatalf("APIFlavor = %q", req.APIFlavor)
	}
	if req.Model != "claude-compatible-coder" {
		t.Fatalf("Model = %q", req.Model)
	}
	if req.MaxTokens != 256 {
		t.Fatalf("MaxTokens = %d, want 256", req.MaxTokens)
	}
	if !req.Stream {
		t.Fatalf("Stream = false, want true")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != inference.RoleSystem {
		t.Fatalf("first role = %q", req.Messages[0].Role)
	}
	if req.Messages[0].Text() != "You are careful." {
		t.Fatalf("system text = %q", req.Messages[0].Text())
	}
	if req.Messages[1].Text() != "Refactor this." {
		t.Fatalf("user text = %q", req.Messages[1].Text())
	}
}

func TestParseAnthropicMessagesContentArrayJoinsTextBlocks(t *testing.T) {
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hello"},
			{"type":"text","text":"world"}
		]}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}

	if got := req.Messages[0].Text(); got != "hello\nworld" {
		t.Fatalf("text = %q, want hello newline world", got)
	}
}

func TestParseAnthropicMessagesNullOrMissingContentReturnsError(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "null content",
			raw:  []byte(`{"model":"claude-compatible-coder","messages":[{"role":"user","content":null}]}`),
		},
		{
			name: "missing content",
			raw:  []byte(`{"model":"claude-compatible-coder","messages":[{"role":"user"}]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAnthropicMessages(tt.raw)

			requireInferenceError(t, err, "anthropic.content_required")
		})
	}
}

func TestParseAnthropicMessagesScalarContentReturnsUnsupported(t *testing.T) {
	// A content that is neither a string, an array, nor null (here a number) is
	// genuinely unrepresentable -> the one remaining content_unsupported path.
	raw := []byte(`{"model":"claude-compatible-coder","messages":[{"role":"user","content":42}]}`)

	_, err := ParseAnthropicMessages(raw)

	requireInferenceError(t, err, "anthropic.content_unsupported")
}

func TestParseAnthropicMessagesSkipsUnrepresentableBlocksTolerantly(t *testing.T) {
	// A block array whose only block is genuinely unmodeled (redacted_thinking, an
	// opaque encrypted block we intentionally skip) yields no message rather than
	// rejecting the whole request; with no other messages the conversation is empty
	// and Validate reports messages_required.
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":[
			{"type":"redacted_thinking","data":"AbCd=="}
		]}]
	}`)

	_, err := ParseAnthropicMessages(raw)

	requireInferenceError(t, err, "request.messages_required")
}

func TestParseAnthropicMessagesThreadsThinkingToReasoning(t *testing.T) {
	// Claude Code replays the assistant turn's thinking block in the tool loop
	// (thinking FIRST, then the tool_use). The thinking text is captured as the
	// assistant message's Reasoning so it threads back upstream as reasoning_content,
	// keeping the reasoning model's chain-of-thought. The signature is not validated.
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"run pwd"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"I should call the shell.","signature":"not-a-real-sig"},
				{"type":"tool_use","id":"toolu_1","name":"shell","input":{"cmd":"pwd"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"/home"}
			]}
		]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	// Find the assistant message that carries the tool call; it must also carry the
	// reasoning captured from the thinking block.
	var found bool
	for _, m := range req.Messages {
		if m.Role == inference.RoleAssistant && len(m.ToolCalls) == 1 {
			found = true
			if m.Reasoning != "I should call the shell." {
				t.Fatalf("assistant reasoning = %q, want %q", m.Reasoning, "I should call the shell.")
			}
		}
	}
	if !found {
		t.Fatalf("no assistant message with a tool call: %+v", req.Messages)
	}
}

func TestAnthropicMessageResponseEmitsThinkingBlock(t *testing.T) {
	// A reasoning turn renders a thinking block FIRST (before text), with an empty
	// signature, mirroring llama.cpp.
	resp := AnthropicMessageResponse("msg_r", "claude-compatible-coder", "The answer.", "let me think", nil, "stop", inference.Usage{InputTokens: 4, OutputTokens: 6})
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking + text)", len(resp.Content))
	}
	tb, ok := resp.Content[0].(AnthropicThinkingBlock)
	if !ok {
		t.Fatalf("content[0] = %#v, want AnthropicThinkingBlock (thinking first)", resp.Content[0])
	}
	if tb.Type != "thinking" || tb.Thinking != "let me think" || tb.Signature != "" {
		t.Fatalf("thinking block = %+v, want {thinking,\"let me think\",\"\"}", tb)
	}
	if txt, ok := resp.Content[1].(AnthropicTextBlock); !ok || txt.Text != "The answer." {
		t.Fatalf("content[1] = %#v, want the text block after thinking", resp.Content[1])
	}
	// The signature field is always present (even when empty), matching llama.cpp.
	data, _ := json.Marshal(resp)
	if !strings.Contains(string(data), `"signature":""`) {
		t.Fatalf("marshaled response missing empty signature: %s", data)
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish       string
		hasToolCalls bool
		want         string
	}{
		{"stop", false, "end_turn"},
		{"", false, "end_turn"},
		{"length", false, "max_tokens"},
		{"tool_calls", false, "tool_use"},
		{"content_filter", false, "end_turn"},
		{"stop", true, "tool_use"},     // a completed tool call wins over "stop"
		{"length", true, "tool_use"},   // tool call present -> tool_use
		{"unknown", false, "end_turn"}, // unknown falls back to end_turn
	}
	for _, tc := range cases {
		if got := AnthropicStopReason(tc.finish, tc.hasToolCalls); got != tc.want {
			t.Fatalf("AnthropicStopReason(%q, %v) = %q, want %q", tc.finish, tc.hasToolCalls, got, tc.want)
		}
	}
}

func TestAnthropicMessageResponseCacheUsage(t *testing.T) {
	// cache_read_input_tokens = CachedTokens; input_tokens EXCLUDES the cached subset
	// (OpenAI prompt_tokens includes it, Anthropic reports it separately).
	resp := AnthropicMessageResponse("msg_c", "m", "ok", "", nil, "stop", inference.Usage{InputTokens: 100, OutputTokens: 5, CachedTokens: 40})
	if resp.Usage.InputTokens != 60 {
		t.Fatalf("input_tokens = %d, want 60 (100-40 cached)", resp.Usage.InputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 40 {
		t.Fatalf("cache_read_input_tokens = %d, want 40", resp.Usage.CacheReadInputTokens)
	}
	// No cache -> the field is omitted from the JSON, input_tokens is the full count.
	plain := AnthropicMessageResponse("msg_p", "m", "ok", "", nil, "stop", inference.Usage{InputTokens: 10, OutputTokens: 2})
	if plain.Usage.InputTokens != 10 {
		t.Fatalf("plain input_tokens = %d, want 10", plain.Usage.InputTokens)
	}
	data, _ := json.Marshal(plain)
	if strings.Contains(string(data), "cache_read_input_tokens") {
		t.Fatalf("cache_read_input_tokens should be omitted when zero: %s", data)
	}
}

func TestParseAnthropicMessagesImageBlocks(t *testing.T) {
	// Claude Code pastes screenshots as image blocks; base64 -> a data: URI, url ->
	// passthrough, so the provider forwards them as OpenAI image_url content parts.
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe these"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
			{"type":"image","source":{"type":"url","url":"https://example.test/x.png"}}
		]}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(req.Messages))
	}
	parts := req.Messages[0].Content
	if len(parts) != 3 {
		t.Fatalf("content parts = %d (%+v), want 3 (text + 2 images)", len(parts), parts)
	}
	if parts[0].Type != inference.ContentText || parts[0].Text != "describe these" {
		t.Fatalf("part[0] = %+v, want the text", parts[0])
	}
	if parts[1].Type != inference.ContentImage || parts[1].ImageURL != "data:image/png;base64,AAAA" {
		t.Fatalf("part[1] = %+v, want the base64 data URI", parts[1])
	}
	if parts[2].Type != inference.ContentImage || parts[2].ImageURL != "https://example.test/x.png" {
		t.Fatalf("part[2] = %+v, want the url passthrough", parts[2])
	}
	// Text() still reflects only the text; an image-only message is no longer dropped.
	if req.Messages[0].Text() != "describe these" {
		t.Fatalf("Text() = %q, want %q", req.Messages[0].Text(), "describe these")
	}
}

func TestParseAnthropicMessagesImageOnlyTurnKept(t *testing.T) {
	// A turn whose only content is an image is now KEPT (an image content part),
	// not dropped — so a trailing screenshot-only user turn reaches the upstream.
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"ZZZZ"}}
		]}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Type != inference.ContentImage {
		t.Fatalf("messages = %+v, want one user message with a single image part", req.Messages)
	}
	if req.Messages[0].Content[0].ImageURL != "data:image/jpeg;base64,ZZZZ" {
		t.Fatalf("image url = %q", req.Messages[0].Content[0].ImageURL)
	}
}

func TestParseAnthropicMessagesThreadsStopSequences(t *testing.T) {
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":"hi"}],
		"stop_sequences":["\n\nHuman:","END"]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if len(req.Stop) != 2 || req.Stop[0] != "\n\nHuman:" || req.Stop[1] != "END" {
		t.Fatalf("Stop = %#v, want the two stop sequences", req.Stop)
	}
}

func TestParseAnthropicMessagesToolUseAndToolResult(t *testing.T) {
	// The multi-turn tool-use shape Claude Code sends: an assistant tool_use turn,
	// then a user turn feeding the tool_result back (Anthropic carries the result
	// inside a user message; it must become a separate tool-role message emitted
	// BEFORE the follow-up user text).
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[
			{"role":"user","content":"list the files"},
			{"role":"assistant","content":[
				{"type":"text","text":"Let me look."},
				{"type":"tool_use","id":"toolu_abc","name":"shell","input":{"cmd":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_abc","content":[{"type":"text","text":"a.go\nb.go"}]},
				{"type":"text","text":"now summarize"}
			]}
		]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	// user, assistant(text+tool_call), tool(result), user(text) = 4 messages.
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d (%+v), want 4", len(req.Messages), req.Messages)
	}
	asst := req.Messages[1]
	if asst.Role != inference.RoleAssistant || asst.Text() != "Let me look." {
		t.Fatalf("assistant message = %+v", asst)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", len(asst.ToolCalls))
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "toolu_abc" || tc.Name != "shell" || tc.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("tool call = %+v, want id toolu_abc name shell args {\"cmd\":\"ls\"}", tc)
	}
	toolMsg := req.Messages[2]
	if toolMsg.Role != inference.RoleTool || toolMsg.ToolCallID != "toolu_abc" || toolMsg.Text() != "a.go\nb.go" {
		t.Fatalf("tool result message = %+v", toolMsg)
	}
	if last := req.Messages[3]; last.Role != inference.RoleUser || last.Text() != "now summarize" {
		t.Fatalf("trailing user message = %+v", last)
	}
}

func TestParseAnthropicMessagesToolResultStringContent(t *testing.T) {
	// tool_result.content can be a bare string (not only an array of blocks).
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_x","content":"42 degrees"}
		]}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(req.Messages))
	}
	m := req.Messages[0]
	if m.Role != inference.RoleTool || m.ToolCallID != "toolu_x" || m.Text() != "42 degrees" {
		t.Fatalf("tool result message = %+v", m)
	}
}

func TestParseAnthropicMessagesParsesToolsAndToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"shell","description":"run a shell command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
			{"name":"","description":"nameless dropped"}
		],
		"tool_choice":{"type":"tool","name":"shell"}
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1 (nameless dropped)", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Name != "shell" || tool.Description != "run a shell command" {
		t.Fatalf("tool = %+v", tool)
	}
	// input_schema maps to Parameters.
	if tool.Parameters == nil || tool.Parameters["type"] != "object" {
		t.Fatalf("tool.Parameters = %+v, want the input_schema object", tool.Parameters)
	}
	// tool_choice{type:tool,name} -> the Chat Completions specific-function selector.
	choice, ok := req.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("ToolChoice = %#v, want a map", req.ToolChoice)
	}
	if choice["type"] != "function" {
		t.Fatalf("ToolChoice.type = %v, want function", choice["type"])
	}
	fn, _ := choice["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Fatalf("ToolChoice.function.name = %v, want shell", fn["name"])
	}
}

func TestAnthropicToolChoiceMapping(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want any
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any->required", `{"type":"any"}`, "required"},
		{"none", `{"type":"none"}`, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":` + tt.in + `}`)
			req, err := ParseAnthropicMessages(raw)
			if err != nil {
				t.Fatalf("ParseAnthropicMessages returned %v", err)
			}
			if req.ToolChoice != tt.want {
				t.Fatalf("ToolChoice = %#v, want %#v", req.ToolChoice, tt.want)
			}
		})
	}
}

func TestParseAnthropicMessagesSystemAsArray(t *testing.T) {
	// Claude Code sends `system` as an array of text blocks (to attach a
	// cache_control breakpoint); the blocks' text must be concatenated.
	raw := []byte(`{
		"model":"claude-compatible-coder",
		"system":[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"Be concise.","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)

	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicMessages returned %v", err)
	}
	if req.Messages[0].Role != inference.RoleSystem {
		t.Fatalf("first role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[0].Text() != "You are Claude Code.\nBe concise." {
		t.Fatalf("system text = %q", req.Messages[0].Text())
	}
}

func TestParseAnthropicMessagesValidationFailures(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		code string
	}{
		{
			name: "missing model",
			raw:  []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			code: "request.model_required",
		},
		{
			name: "missing messages",
			raw:  []byte(`{"model":"claude-compatible-coder"}`),
			code: "request.messages_required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAnthropicMessages(tt.raw)

			requireInferenceError(t, err, tt.code)
		})
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	raw := []byte(`{"model":"claude-compatible-coder","messages":[{"role":"user","content":"one two three"}]}`)

	count, err := CountAnthropicTokens(raw)
	if err != nil {
		t.Fatalf("CountAnthropicTokens returned %v", err)
	}
	if count.InputTokens != 3 {
		t.Fatalf("InputTokens = %d, want 3", count.InputTokens)
	}
}

func TestAnthropicMessageResponseUsesUsage(t *testing.T) {
	resp := AnthropicMessageResponse("msg_123", "claude-compatible-coder", "ok", "", nil, "stop", inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5})

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal returned %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("response is invalid json: %s", string(data))
	}
	if resp.ID != "msg_123" {
		t.Fatalf("id = %q, want msg_123", resp.ID)
	}
	if resp.Type != "message" {
		t.Fatalf("type = %q, want message", resp.Type)
	}
	if resp.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", resp.Role)
	}
	if resp.Model != "claude-compatible-coder" {
		t.Fatalf("model = %q, want claude-compatible-coder", resp.Model)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %d, want 1", len(resp.Content))
	}
	block, ok := resp.Content[0].(AnthropicTextBlock)
	if !ok {
		t.Fatalf("content[0] = %#v, want AnthropicTextBlock", resp.Content[0])
	}
	if block.Type != "text" || block.Text != "ok" {
		t.Fatalf("content block = %+v, want text/ok", block)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.StopSequence != nil {
		t.Fatalf("stop_sequence = %#v, want nil", resp.StopSequence)
	}
	if resp.Usage.InputTokens != 2 {
		t.Fatalf("input_tokens = %d, want 2", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 3 {
		t.Fatalf("output_tokens = %d, want 3", resp.Usage.OutputTokens)
	}
}

func TestAnthropicMessageResponseRendersToolUse(t *testing.T) {
	toolCalls := []inference.ToolCall{
		{ID: "toolu_1", Name: "shell", Arguments: `{"cmd":"ls"}`},
		{ID: "", Name: "lookup", Arguments: ""}, // no id, empty args
	}
	resp := AnthropicMessageResponse("msg_9", "claude-compatible-coder", "Let me check.", "", toolCalls, "tool_calls", inference.Usage{InputTokens: 5, OutputTokens: 7})

	// stop_reason flips to tool_use whenever there are tool_use blocks.
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	// text block + two tool_use blocks, in order.
	if len(resp.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(resp.Content))
	}
	if tb, ok := resp.Content[0].(AnthropicTextBlock); !ok || tb.Text != "Let me check." {
		t.Fatalf("content[0] = %#v, want the text block", resp.Content[0])
	}
	first, ok := resp.Content[1].(AnthropicToolUseBlock)
	if !ok {
		t.Fatalf("content[1] = %#v, want AnthropicToolUseBlock", resp.Content[1])
	}
	if first.Type != "tool_use" || first.ID != "toolu_1" || first.Name != "shell" {
		t.Fatalf("first tool_use = %+v", first)
	}
	// input is an OBJECT reconstructed from the JSON-string arguments.
	if first.Input["cmd"] != "ls" {
		t.Fatalf("first tool_use input = %+v, want cmd=ls", first.Input)
	}
	second, ok := resp.Content[2].(AnthropicToolUseBlock)
	if !ok {
		t.Fatalf("content[2] = %#v, want AnthropicToolUseBlock", resp.Content[2])
	}
	// A missing id is synthesized as toolu_<name>; empty args -> empty object.
	if second.ID != "toolu_lookup" {
		t.Fatalf("second tool_use id = %q, want toolu_lookup", second.ID)
	}
	if second.Input == nil || len(second.Input) != 0 {
		t.Fatalf("second tool_use input = %+v, want empty object", second.Input)
	}
	// The whole thing round-trips to valid JSON with input as an object.
	data, err := json.Marshal(resp)
	if err != nil || !json.Valid(data) {
		t.Fatalf("Marshal returned %v / invalid json", err)
	}
	if !strings.Contains(string(data), `"input":{"cmd":"ls"}`) {
		t.Fatalf("serialized tool_use input not an object: %s", data)
	}
}
