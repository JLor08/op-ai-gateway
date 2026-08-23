// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/inference"
	"testing"
)

func TestParseOpenAIChatCompletions(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"Write a test."}
		],
		"tools":[
			{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}
		]
	}`)

	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIChatCompletions returned %v", err)
	}

	if req.APIFlavor != "openai_chat_completions" {
		t.Fatalf("APIFlavor = %q", req.APIFlavor)
	}
	if req.Model != "qwen-coder" {
		t.Fatalf("Model = %q", req.Model)
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
	if req.Messages[1].Text() != "Write a test." {
		t.Fatalf("user text = %q", req.Messages[1].Text())
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %#v", req.Tools)
	}
}

func TestParseOpenAIChatCompletionsParsesToolCallReplay(t *testing.T) {
	// The turn-2 shape an OpenAI-compatible coding agent (opencode) replays after
	// the model made a tool call: the assistant tool-call turn has null content +
	// a tool_calls array, followed by the tool-result message (role:"tool").
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"Read config.json"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"config.json\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"port\":8080}"}
		],
		"tools":[
			{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}
		]
	}`)

	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIChatCompletions returned %v", err)
	}

	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(req.Messages))
	}

	assistant := req.Messages[2]
	if assistant.Role != inference.RoleAssistant {
		t.Fatalf("assistant role = %q", assistant.Role)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls = %d, want 1", len(assistant.ToolCalls))
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool call = %#v", assistant.ToolCalls[0])
	}
	if assistant.ToolCalls[0].Arguments != `{"path":"config.json"}` {
		t.Fatalf("tool call arguments = %q", assistant.ToolCalls[0].Arguments)
	}

	tool := req.Messages[3]
	if tool.Role != inference.RoleTool {
		t.Fatalf("tool role = %q", tool.Role)
	}
	if tool.ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %q, want call_1", tool.ToolCallID)
	}
	if tool.Text() != `{"port":8080}` {
		t.Fatalf("tool content = %q", tool.Text())
	}
}

func TestParseOpenAIChatCompletionsForwardsToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],
		"tool_choice":"required"
	}`)

	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIChatCompletions returned %v", err)
	}
	if req.ToolChoice != "required" {
		t.Fatalf("ToolChoice = %#v, want \"required\"", req.ToolChoice)
	}
}

func TestParseOpenAIChatCompletionsReadsIncludeUsage(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"stream_options":{"include_usage":true}
	}`)
	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIChatCompletions returned %v", err)
	}
	if !req.IncludeUsage {
		t.Fatalf("IncludeUsage = false, want true")
	}

	raw2 := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req2, err := ParseOpenAIChatCompletions(raw2)
	if err != nil {
		t.Fatalf("parse2: %v", err)
	}
	if req2.IncludeUsage {
		t.Fatalf("IncludeUsage = true without stream_options, want false")
	}
}

func TestParseOpenAIResponsesWithStringInput(t *testing.T) {
	raw := []byte(`{"model":"gpt-oss-20b","input":"hello","stream":false}`)

	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}

	if req.APIFlavor != "openai_responses" {
		t.Fatalf("APIFlavor = %q", req.APIFlavor)
	}
	if req.Messages[0].Role != inference.RoleUser {
		t.Fatalf("role = %q", req.Messages[0].Role)
	}
	if req.Messages[0].Text() != "hello" {
		t.Fatalf("text = %q", req.Messages[0].Text())
	}
}

func TestParseOpenAIResponsesWithMessageArrayInput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-oss-20b",
		"input":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":[
				{"type":"input_text","text":"hello"},
				{"type":"input_text","text":"world"}
			]}
		]
	}`)

	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != inference.RoleSystem {
		t.Fatalf("first role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[0].Text() != "You are concise." {
		t.Fatalf("system text = %q", req.Messages[0].Text())
	}
	if req.Messages[1].Role != inference.RoleUser {
		t.Fatalf("second role = %q, want user", req.Messages[1].Role)
	}
	if req.Messages[1].Text() != "hello\nworld" {
		t.Fatalf("user text = %q, want hello newline world", req.Messages[1].Text())
	}
}

func TestParseOpenAIChatContentArrayJoinsTextBlocks(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"hello"},
				{"type":"text","text":"world"}
			]}
		]
	}`)

	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIChatCompletions returned %v", err)
	}

	if got := req.Messages[0].Text(); got != "hello\nworld" {
		t.Fatalf("text = %q, want hello newline world", got)
	}
}

func TestParseOpenAIChatCompletionsReadsSamplingParams(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.4,"max_tokens":128}`)
	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.4 {
		t.Fatalf("temperature = %v, want 0.4", req.Temperature)
	}
	if req.MaxTokens != 128 {
		t.Fatalf("max_tokens = %d, want 128", req.MaxTokens)
	}
}

func TestParseOpenAIChatCompletionsParsesImageBlocks(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts := req.Messages[0].Content
	if len(parts) != 2 || parts[0].Type != inference.ContentText || parts[0].Text != "look" {
		t.Fatalf("parts[0] = %#v", parts)
	}
	if parts[1].Type != inference.ContentImage || parts[1].ImageURL != "data:image/png;base64,AAAA" {
		t.Fatalf("parts[1] = %#v", parts[1])
	}
}

func TestParseOpenAIChatImageOnlyContentParses(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]
	}`)
	req, err := ParseOpenAIChatCompletions(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts := req.Messages[0].Content
	if len(parts) != 1 {
		t.Fatalf("parts = %#v, want single content part", parts)
	}
	if parts[0].Type != inference.ContentImage || parts[0].ImageURL != "https://example.test/image.png" {
		t.Fatalf("parts[0] = %#v", parts[0])
	}
}

func TestParseOpenAIChatNullOrMissingContentReturnsError(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "null content",
			raw:  []byte(`{"model":"qwen-coder","messages":[{"role":"user","content":null}]}`),
		},
		{
			name: "missing content",
			raw:  []byte(`{"model":"qwen-coder","messages":[{"role":"user"}]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOpenAIChatCompletions(tt.raw)

			requireInferenceError(t, err, "openai.content_required")
		})
	}
}

func TestParseOpenAIChatUnsupportedArrayContentReturnsError(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "unsupported block",
			raw: []byte(`{
				"model":"qwen-coder",
				"messages":[{"role":"user","content":[{"type":"audio","audio":{"id":"audio_123"}}]}]
			}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOpenAIChatCompletions(tt.raw)

			requireInferenceError(t, err, "openai.content_unsupported")
		})
	}
}

func TestParseOpenAIChatContentBlockErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		code string
	}{
		{
			name: "empty image url",
			raw:  []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`),
			code: "openai.content_unsupported",
		},
		{
			name: "non-map array item",
			raw:  []byte(`{"model":"m","messages":[{"role":"user","content":["oops"]}]}`),
			code: "openai.content_unsupported",
		},
		{
			name: "whitespace text collapses to empty",
			raw:  []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"   "}]}]}`),
			code: "openai.content_required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOpenAIChatCompletions(tt.raw)

			requireInferenceError(t, err, tt.code)
		})
	}
}

func TestParseOpenAIResponsesMissingInputReturnsError(t *testing.T) {
	raw := []byte(`{"model":"gpt-oss-20b","stream":false}`)

	_, err := ParseOpenAIResponses(raw)

	requireInferenceError(t, err, "openai.input_required")
}

func TestParseOpenAIUnsupportedToolTypeReturnsError(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"file_search"}]
	}`)

	_, err := ParseOpenAIChatCompletions(raw)

	requireInferenceError(t, err, "openai.tool_type_unsupported")
}

func TestParseOpenAIFunctionToolEmptyNameReturnsError(t *testing.T) {
	raw := []byte(`{
		"model":"qwen-coder",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"description":"missing name","parameters":{"type":"object"}}}]
	}`)

	_, err := ParseOpenAIChatCompletions(raw)

	requireInferenceError(t, err, "openai.tool_name_required")
}

func TestOpenAIChatResponseUsesUsage(t *testing.T) {
	resp := OpenAIChatResponse("chatcmpl_123", "qwen-coder", "ok", nil, inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5})

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal returned %v", err)
	}

	if !json.Valid(data) {
		t.Fatalf("response is invalid json: %s", string(data))
	}
	if resp.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 2 {
		t.Fatalf("prompt_tokens = %d, want 2", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 3 {
		t.Fatalf("completion_tokens = %d, want 3", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("total tokens = %d, want 5", resp.Usage.TotalTokens)
	}
}

func TestOpenAIResponsesResponseUsesOutputMessageShape(t *testing.T) {
	resp := OpenAIResponsesResponse("resp_123", "qwen-coder", "ok", "", nil, inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5})

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal returned %v", err)
	}

	if !json.Valid(data) {
		t.Fatalf("response is invalid json: %s", string(data))
	}
	if resp.ID != "resp_123" {
		t.Fatalf("id = %q, want resp_123", resp.ID)
	}
	if resp.Object != "response" {
		t.Fatalf("object = %q, want response", resp.Object)
	}
	if resp.CreatedAt == 0 {
		t.Fatalf("created_at = 0, want unix timestamp")
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if resp.Model != "qwen-coder" {
		t.Fatalf("model = %q, want qwen-coder", resp.Model)
	}
	if resp.OutputText != "ok" {
		t.Fatalf("output_text = %q, want ok", resp.OutputText)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output items = %d, want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(OpenAIResponsesMessage)
	if !ok {
		t.Fatalf("output[0] = %T, want OpenAIResponsesMessage", resp.Output[0])
	}
	if msg.Type != "message" {
		t.Fatalf("output type = %q, want message", msg.Type)
	}
	if msg.ID == "" {
		t.Fatalf("output id is empty")
	}
	if msg.Status != "completed" {
		t.Fatalf("output status = %q, want completed", msg.Status)
	}
	if msg.Role != "assistant" {
		t.Fatalf("output role = %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(msg.Content))
	}
	if msg.Content[0].Type != "output_text" {
		t.Fatalf("content type = %q, want output_text", msg.Content[0].Type)
	}
	if msg.Content[0].Text != "ok" {
		t.Fatalf("content text = %q, want ok", msg.Content[0].Text)
	}
	if msg.Content[0].Annotations == nil {
		t.Fatalf("annotations = nil, want empty array")
	}
	if resp.Usage.InputTokens != 2 {
		t.Fatalf("input_tokens = %d, want 2", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 3 {
		t.Fatalf("output_tokens = %d, want 3", resp.Usage.OutputTokens)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("total_tokens = %d, want 5", resp.Usage.TotalTokens)
	}
}

func requireInferenceError(t *testing.T, err error, code string) {
	t.Helper()

	if err == nil {
		t.Fatalf("error = nil, want %s", code)
	}
	var got *inference.Error
	if !errors.As(err, &got) {
		t.Fatalf("error = %#v, want *inference.Error", err)
	}
	if got.Code != code {
		t.Fatalf("error code = %q, want %s", got.Code, code)
	}
}

func TestParseOpenAIResponsesToolCallRoundTrip(t *testing.T) {
	raw := []byte(`{
		"model": "m",
		"instructions": "be helpful",
		"tool_choice": "auto",
		"tools": [{"type":"function","name":"shell","description":"run a command","parameters":{"type":"object"}},
		          {"type":"web_search"}],
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt\nb.txt"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}
		]
	}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}
	// Only the function tool is forwarded; web_search is skipped.
	if len(req.Tools) != 1 || req.Tools[0].Name != "shell" {
		t.Fatalf("tools = %+v, want one function tool 'shell'", req.Tools)
	}
	if req.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %v, want auto", req.ToolChoice)
	}
	// reasoning is skipped -> system, user, assistant(tool call), tool(result).
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != inference.RoleSystem || req.Messages[0].Text() != "be helpful" {
		t.Fatalf("message[0] = %+v, want system instructions", req.Messages[0])
	}
	if req.Messages[1].Role != inference.RoleUser || req.Messages[1].Text() != "list files" {
		t.Fatalf("message[1] = %+v, want user", req.Messages[1])
	}
	asst := req.Messages[2]
	if asst.Role != inference.RoleAssistant || len(asst.ToolCalls) != 1 {
		t.Fatalf("message[2] = %+v, want assistant with one tool call", asst)
	}
	if asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Name != "shell" || asst.ToolCalls[0].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("tool call = %+v", asst.ToolCalls[0])
	}
	tool := req.Messages[3]
	if tool.Role != inference.RoleTool || tool.ToolCallID != "call_1" || tool.Text() != "a.txt\nb.txt" {
		t.Fatalf("message[3] = %+v, want tool result for call_1", tool)
	}
}

func TestParseOpenAIResponsesTolerantOfUnknownItems(t *testing.T) {
	// A future/unknown item type must be skipped, not rejected (the bug that broke
	// Codex multi-turn). A valid message alongside it still parses.
	raw := []byte(`{"model":"m","input":[
		{"type":"some_future_item","payload":{"x":1}},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v (unknown items must be tolerated)", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Text() != "hi" {
		t.Fatalf("messages = %+v, want a single user 'hi'", req.Messages)
	}
}

func TestParseOpenAIResponsesFunctionCallOutputArray(t *testing.T) {
	// function_call_output.output may be an array of content items, not just a string.
	raw := []byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"result text"}]}
	]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[1].Text() != "result text" {
		t.Fatalf("messages = %+v, want tool result 'result text'", req.Messages)
	}
}

func TestParseOpenAIResponsesThreadsReasoning(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-oss",
		"reasoning":{"effort":"high"},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"reasoning","content":[{"type":"reasoning_text","text":"I should run ls"}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"c1","output":"a.txt"},
			{"type":"reasoning","content":[{"type":"reasoning_text","text":"now answer"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}
		]
	}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", req.ReasoningEffort)
	}
	// Expect: user, assistant(tool_calls c1, reasoning attached), tool(c1), assistant("Done.", reasoning attached).
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %#v", len(req.Messages), req.Messages)
	}
	asstCall := req.Messages[1]
	if asstCall.Role != inference.RoleAssistant || len(asstCall.ToolCalls) != 1 || asstCall.ToolCalls[0].ID != "c1" {
		t.Fatalf("messages[1] not the assistant tool call: %#v", asstCall)
	}
	if asstCall.Reasoning != "I should run ls" {
		t.Fatalf("tool-call reasoning = %q, want %q", asstCall.Reasoning, "I should run ls")
	}
	if req.Messages[2].Role != inference.RoleTool {
		t.Fatalf("messages[2] not the tool result: %#v", req.Messages[2])
	}
	asstMsg := req.Messages[3]
	if asstMsg.Role != inference.RoleAssistant || asstMsg.Text() != "Done." {
		t.Fatalf("messages[3] not the assistant answer: %#v", asstMsg)
	}
	if asstMsg.Reasoning != "now answer" {
		t.Fatalf("answer reasoning = %q, want %q", asstMsg.Reasoning, "now answer")
	}
}

func TestOpenAIResponsesResponseEmitsReasoningItem(t *testing.T) {
	resp := OpenAIResponsesResponse("resp_r", "m", "Answer.", "my thoughts", nil, inference.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3})
	// Reasoning item first, then the message item.
	if len(resp.Output) != 2 {
		t.Fatalf("output = %d items, want 2 (reasoning+message): %+v", len(resp.Output), resp.Output)
	}
	rs, ok := resp.Output[0].(OpenAIResponsesReasoning)
	if !ok {
		t.Fatalf("output[0] not a reasoning item: %T", resp.Output[0])
	}
	if rs.Type != "reasoning" || rs.ID != "rs_resp_r" || len(rs.Content) != 1 || rs.Content[0].Text != "my thoughts" {
		t.Fatalf("reasoning item malformed: %+v", rs)
	}
	if _, ok := resp.Output[1].(OpenAIResponsesMessage); !ok {
		t.Fatalf("output[1] not a message item: %T", resp.Output[1])
	}
}

func TestOpenAIResponsesResponseEmitsFunctionCall(t *testing.T) {
	resp := OpenAIResponsesResponse("resp_1", "m", "", "", []inference.ToolCall{
		{ID: "call_1", Name: "shell", Arguments: `{"cmd":"ls"}`},
	}, inference.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3})
	// No text + a tool call -> exactly one function_call output item (no message).
	if len(resp.Output) != 1 {
		t.Fatalf("output = %d items, want 1: %+v", len(resp.Output), resp.Output)
	}
	fc, ok := resp.Output[0].(OpenAIResponsesFunctionCall)
	if !ok {
		t.Fatalf("output[0] = %T, want OpenAIResponsesFunctionCall", resp.Output[0])
	}
	if fc.Type != "function_call" || fc.CallID != "call_1" || fc.Name != "shell" || fc.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("function_call = %+v", fc)
	}
	// Marshals to valid JSON.
	if data, err := json.Marshal(resp); err != nil || !json.Valid(data) {
		t.Fatalf("marshal: err=%v", err)
	}
}

func TestOpenAIChatResponseEmitsToolCalls(t *testing.T) {
	resp := OpenAIChatResponse("c1", "m", "", []inference.ToolCall{{ID: "call_9", Name: "f", Arguments: ""}}, inference.Usage{})
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_9" || msg.ToolCalls[0].Function.Name != "f" {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	// Empty arguments must serialize as "{}", not "".
	if msg.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("arguments = %q, want {}", msg.ToolCalls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestParseOpenAIResponsesMessageSkipsNonTextBlocks(t *testing.T) {
	// A message mixing text + a non-text block (image) keeps the text and skips
	// the image — it must NOT reject the whole request.
	raw := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"look at this"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
		]}
	]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v (non-text blocks must be tolerated)", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Text() != "look at this" {
		t.Fatalf("messages = %+v, want single user 'look at this'", req.Messages)
	}
}

func TestParseOpenAIResponsesSkipsEmptyMessage(t *testing.T) {
	// An echoed empty assistant message (e.g. a prior tool-only turn) must be
	// skipped, not rejected — the user message survives.
	raw := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}
	]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v (empty message must be skipped)", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != inference.RoleUser {
		t.Fatalf("messages = %+v, want single user message", req.Messages)
	}
}

func TestParseOpenAIResponsesMergesParallelToolCalls(t *testing.T) {
	raw := []byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"b","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"r1"},
		{"type":"function_call_output","call_id":"c2","output":"r2"}
	]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}
	// One assistant message with BOTH tool calls, then two tool results.
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant + 2 tool): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != inference.RoleAssistant || len(req.Messages[0].ToolCalls) != 2 {
		t.Fatalf("message[0] = %+v, want assistant with 2 tool calls", req.Messages[0])
	}
	if req.Messages[0].ToolCalls[0].ID != "c1" || req.Messages[0].ToolCalls[1].ID != "c2" {
		t.Fatalf("tool call ids = %+v, want c1,c2", req.Messages[0].ToolCalls)
	}
	if req.Messages[1].Role != inference.RoleTool || req.Messages[1].ToolCallID != "c1" {
		t.Fatalf("message[1] = %+v, want tool c1", req.Messages[1])
	}
}

func TestParseOpenAIResponsesBareTypedTextBlock(t *testing.T) {
	// A top-level input array of bare typed text blocks is captured as user text.
	raw := []byte(`{"model":"m","input":[{"type":"input_text","text":"one"},{"type":"input_text","text":"two"}]}`)
	req, err := ParseOpenAIResponses(raw)
	if err != nil {
		t.Fatalf("ParseOpenAIResponses returned %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Text() != "one\ntwo" {
		t.Fatalf("messages = %+v, want user 'one\\ntwo'", req.Messages)
	}
}

// TestParseOpenAIResponsesInputWrongTypeReturnsError proves parseOpenAIResponsesInput's
// default arm: an `input` that is neither a string, an array, nor absent (here a
// bare number) is rejected as unsupported content, not silently coerced or panicked
// on.
func TestParseOpenAIResponsesInputWrongTypeReturnsError(t *testing.T) {
	raw := []byte(`{"model":"m","input":42}`)

	_, err := ParseOpenAIResponses(raw)

	requireInferenceError(t, err, "openai.content_unsupported")
}

// TestParseOpenAIResponsesReasoningOnlyInputReturnsInputRequired proves that an
// `input` array containing ONLY a reasoning item (tolerantly buffered, per the
// comment on parseOpenAIResponsesInputArray's "reasoning" case, but never itself
// attached to any message since no message/function_call item follows it) still
// yields zero messages overall, and that empty-after-parsing case is rejected
// with the same openai.input_required a genuinely empty array would get —
// distinct from an array that IS accepted as tolerant-but-message-producing.
func TestParseOpenAIResponsesReasoningOnlyInputReturnsInputRequired(t *testing.T) {
	raw := []byte(`{"model":"m","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking out loud"}]}]}`)

	_, err := ParseOpenAIResponses(raw)

	requireInferenceError(t, err, "openai.input_required")
}

// TestParseOpenAIChatContentTextBlockNonStringTextReturnsError proves a chat
// message content array item of type "text" whose "text" field is present but
// not a string (here a number) is rejected, rather than silently treated as
// empty text.
func TestParseOpenAIChatContentTextBlockNonStringTextReturnsError(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":123}]}]}`)

	_, err := ParseOpenAIChatCompletions(raw)

	requireInferenceError(t, err, "openai.content_unsupported")
}

// TestParseOpenAIChatContentWrongTypeReturnsError proves parseOpenAIChatContent's
// default arm: a message "content" that is neither a string, an array, nor nil
// (here a bare number) is rejected as unsupported content.
func TestParseOpenAIChatContentWrongTypeReturnsError(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":42}]}`)

	_, err := ParseOpenAIChatCompletions(raw)

	requireInferenceError(t, err, "openai.content_unsupported")
}
