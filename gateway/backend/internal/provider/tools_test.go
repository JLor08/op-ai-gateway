// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// TestOpenAICompatibleCompleteToolRoundTrip verifies the request serializes tools
// + a tool-call history (assistant tool_calls + tool-role result) and that the
// response's tool_calls are parsed back.
func TestOpenAICompatibleCompleteToolRoundTrip(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_2","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	c := NewOpenAICompatibleClient(upstream.Client())
	req := inference.Request{
		Model: "m",
		Messages: []inference.Message{
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "run pwd"}}},
			{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{ID: "call_1", Name: "shell", Arguments: `{"cmd":"ls"}`}}},
			{Role: inference.RoleTool, ToolCallID: "call_1", Content: []inference.ContentPart{{Type: inference.ContentText, Text: "a.txt"}}},
		},
		Tools:      []inference.Tool{{Name: "shell", Description: "run", Parameters: map[string]any{"type": "object"}}},
		ToolChoice: "auto",
	}
	resp, err := c.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM}, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Request serialization: tools + assistant tool_calls + tool-role result.
	for _, want := range []string{`"tools"`, `"name":"shell"`, `"tool_calls"`, `"tool_call_id":"call_1"`, `"role":"tool"`, `"tool_choice":"auto"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("upstream body missing %s:\n%s", want, gotBody)
		}
	}
	// Response parse: a tool-call-only response is valid (empty content).
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("resp tool calls = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_2" || resp.ToolCalls[0].Name != "shell" || resp.ToolCalls[0].Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("tool call = %+v", resp.ToolCalls[0])
	}
}

// TestOpenAICompatibleStreamToolCalls verifies streamed tool_call fragments are
// accumulated by index and emitted as one StreamEventToolCall.
func TestOpenAICompatibleStreamToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	c := NewOpenAICompatibleClient(upstream.Client())
	req := inference.Request{
		Model:    "m",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "ls"}}}},
		Tools:    []inference.Tool{{Name: "shell"}},
	}
	var toolCalls []inference.ToolCall
	var completed bool
	err := c.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM}, req, func(ev inference.StreamEvent) error {
		switch ev.Type {
		case inference.StreamEventToolCall:
			if ev.ToolCall != nil {
				toolCalls = append(toolCalls, *ev.ToolCall)
			}
		case inference.StreamEventCompleted:
			completed = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if !completed {
		t.Fatalf("no completed event")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(toolCalls), toolCalls)
	}
	if toolCalls[0].ID != "call_7" || toolCalls[0].Name != "shell" || toolCalls[0].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("accumulated tool call = %+v", toolCalls[0])
	}
}
