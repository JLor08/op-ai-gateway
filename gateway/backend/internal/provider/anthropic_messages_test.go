// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// anthropicCapture records the one upstream request a test server receives, so
// the test goroutine can read it after the client call returns without racing
// the handler goroutine.
type anthropicCapture struct {
	mu     sync.Mutex
	method string
	path   string
	header http.Header
	raw    []byte
}

func (c *anthropicCapture) handler(respond http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.method, c.path, c.header, c.raw = r.Method, r.URL.Path, r.Header.Clone(), raw
		c.mu.Unlock()
		respond(w, r)
	}
}

func (c *anthropicCapture) request() (method, path string, header http.Header, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.header, c.raw
}

// body decodes the captured request body.
func (c *anthropicCapture) body(t *testing.T) map[string]any {
	t.Helper()
	_, _, _, raw := c.request()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("captured request body is not JSON: %v\n%s", err, raw)
	}
	return body
}

// assertJSON compares got (any decoded-JSON value) with the JSON literal want.
func assertJSON(t *testing.T, name string, got any, want string) {
	t.Helper()
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: bad expected JSON: %v", name, err)
	}
	// Round-trip got so Go-typed values compare like decoded JSON.
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal got: %v", name, err)
	}
	var gotValue any
	if err := json.Unmarshal(gotBytes, &gotValue); err != nil {
		t.Fatalf("%s: re-decode got: %v", name, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s mismatch\n got: %s\nwant: %s", name, gotBytes, want)
	}
}

func anthropicTextMsg(role inference.Role, text string) inference.Message {
	return inference.Message{Role: role, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}
}

// anthropicOKResponse is a minimal valid non-stream /v1/messages response.
const anthropicOKResponse = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func writeAnthropicOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(contentTypeHeader, jsonContentType)
	_, _ = io.WriteString(w, anthropicOKResponse)
}

func anthropicTarget(endpoint string) routing.Target {
	return routing.Target{Endpoint: endpoint, ProviderModel: "claude-sonnet-4-5-20250929", Timeout: 5 * time.Second}
}

func TestAnthropicClientImplementsProviderInterfaces(t *testing.T) {
	var _ Client = NewAnthropicClient(nil)
	var _ StreamingClient = NewAnthropicClient(nil)
	var _ NativeProxyClient = NewAnthropicClient(nil)
}

func TestAnthropicClientCompleteSendsVersionAndApiKeyHeaders(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	ctx := WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-test")

	_, err := client.Complete(ctx, anthropicTarget(upstream.URL), inference.Request{Model: "claude", Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	method, path, header, _ := capture.request()
	if method != http.MethodPost || path != "/v1/messages" {
		t.Fatalf("request = %s %s, want POST /v1/messages", method, path)
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := header.Get("x-api-key"); got != "sk-ant-test" {
		t.Fatalf("x-api-key = %q, want sk-ant-test", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none when the credential header is x-api-key", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestAnthropicClientCompleteSendsVersionWithBearerCredential(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	// An empty header name is the bearer form (the future subscription path):
	// the client is agnostic to the credential shape, but the version is intrinsic.
	ctx := WithUpstreamAuth(context.Background(), "", "oauth-token")

	_, err := client.Complete(ctx, anthropicTarget(upstream.URL), inference.Request{Model: "claude", Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	_, _, header, _ := capture.request()
	if got := header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer oauth-token", got)
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
	}
}

func TestAnthropicClientCompleteRendersConversationWithToolRoundTrip(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	req := inference.Request{
		Model: "gateway-claude",
		Messages: []inference.Message{
			anthropicTextMsg(inference.RoleSystem, "You are terse."),
			anthropicTextMsg(inference.RoleDeveloper, "Answer in German."),
			anthropicTextMsg(inference.RoleUser, "Weather in Berlin?"),
			{
				Role:      inference.RoleAssistant,
				Content:   []inference.ContentPart{{Type: inference.ContentText, Text: "Let me check."}},
				ToolCalls: []inference.ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: `{"city":"Berlin"}`}},
				Reasoning: "secret chain of thought",
			},
			{Role: inference.RoleTool, ToolCallID: "toolu_1", Content: []inference.ContentPart{{Type: inference.ContentText, Text: `{"temp":21}`}}},
			anthropicTextMsg(inference.RoleUser, "And tomorrow?"),
		},
	}

	_, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), req)
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	body := capture.body(t)
	if body["model"] != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %v, want the provider model, not the gateway model", body["model"])
	}
	if body["stream"] != false {
		t.Fatalf("stream = %v, want false", body["stream"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v, want the 4096 default for an unset MaxTokens", body["max_tokens"])
	}
	if body["system"] != "You are terse.\n\nAnswer in German." {
		t.Fatalf("system = %q", body["system"])
	}
	assertJSON(t, "messages", body["messages"], `[
		{"role":"user","content":[{"type":"text","text":"Weather in Berlin?"}]},
		{"role":"assistant","content":[
			{"type":"text","text":"Let me check."},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Berlin"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"temp\":21}"},
			{"type":"text","text":"And tomorrow?"}]}
	]`)
	if _, _, _, raw := capture.request(); strings.Contains(string(raw), "secret chain of thought") {
		t.Fatalf("assistant Reasoning leaked into the Anthropic request: %s", raw)
	}
}

func TestAnthropicClientCompleteRendersPureToolCallTurnWithEmptyArguments(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	req := inference.Request{Messages: []inference.Message{
		anthropicTextMsg(inference.RoleUser, "ping"),
		// No content, an empty-argument call and a call whose arguments are not a JSON object.
		{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{
			{ID: "toolu_a", Name: "noargs", Arguments: ""},
			{ID: "toolu_b", Name: "broken", Arguments: `not json`},
		}},
		{Role: inference.RoleTool, ToolCallID: "toolu_a", Content: []inference.ContentPart{{Type: inference.ContentText, Text: "a"}}},
		{Role: inference.RoleTool, ToolCallID: "toolu_b", Content: []inference.ContentPart{{Type: inference.ContentText, Text: "b"}}},
	}}

	if _, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	assertJSON(t, "messages", capture.body(t)["messages"], `[
		{"role":"user","content":[{"type":"text","text":"ping"}]},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_a","name":"noargs","input":{}},
			{"type":"tool_use","id":"toolu_b","name":"broken","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_a","content":"a"},
			{"type":"tool_result","tool_use_id":"toolu_b","content":"b"}]}
	]`)
}

func TestAnthropicClientCompleteRendersToolsToolChoiceAndSampling(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	temperature := 0.2
	req := inference.Request{
		Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")},
		Tools: []inference.Tool{
			{Name: "get_weather", Description: "Current weather", Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			}},
			{Name: "ping"},
		},
		ToolChoice:  map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		MaxTokens:   256,
		Temperature: &temperature,
		Stop:        []string{"END", ""},
		// Anthropic has no reasoning_effort; it must not be forwarded.
		ReasoningEffort: "high",
	}

	if _, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	body := capture.body(t)
	assertJSON(t, "tools", body["tools"], `[
		{"name":"get_weather","description":"Current weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
		{"name":"ping","input_schema":{"type":"object","properties":{}}}
	]`)
	assertJSON(t, "tool_choice", body["tool_choice"], `{"type":"tool","name":"get_weather"}`)
	if body["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v, want 256", body["max_tokens"])
	}
	if body["temperature"] != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", body["temperature"])
	}
	assertJSON(t, "stop_sequences", body["stop_sequences"], `["END"]`)
	if _, present := body["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort was forwarded: %v", body)
	}
	if _, present := body["stop"]; present {
		t.Fatalf("OpenAI-style stop key was forwarded: %v", body)
	}
}

func TestAnthropicClientCompleteOmitsToolChoiceWithoutTools(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	req := inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}, ToolChoice: "auto"}

	if _, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	body := capture.body(t)
	if _, present := body["tool_choice"]; present {
		t.Fatalf("tool_choice sent without tools (Anthropic rejects that): %v", body["tool_choice"])
	}
	if _, present := body["tools"]; present {
		t.Fatalf("tools = %v, want absent", body["tools"])
	}
	if _, present := body["system"]; present {
		t.Fatalf("system = %v, want absent when there is no system text", body["system"])
	}
}

func TestAnthropicToolChoiceMapsOpenAIForms(t *testing.T) {
	tests := []struct {
		name   string
		choice any
		want   string // JSON, or "" for omitted
	}{
		{"auto", "auto", `{"type":"auto"}`},
		{"required becomes any", "required", `{"type":"any"}`},
		{"none", "none", `{"type":"none"}`},
		{"named function", map[string]any{"type": "function", "function": map[string]any{"name": "f"}}, `{"type":"tool","name":"f"}`},
		{"named function without a name", map[string]any{"type": "function", "function": map[string]any{}}, ""},
		{"responses flat forced function", map[string]any{"type": "function", "name": "f"}, `{"type":"tool","name":"f"}`},
		{"nested function name wins over a flat one", map[string]any{"type": "function", "name": "flat", "function": map[string]any{"name": "nested"}}, `{"type":"tool","name":"nested"}`},
		{"function with no name anywhere", map[string]any{"type": "function"}, ""},
		{"anthropic shaped tool", map[string]any{"type": "tool", "name": "f"}, `{"type":"tool","name":"f"}`},
		{"anthropic shaped any", map[string]any{"type": "any"}, `{"type":"any"}`},
		{"unknown string", "bogus", ""},
		{"nil", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicToolChoiceFor(tc.choice)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("anthropicToolChoiceFor(%v) = %#v, want nil", tc.choice, got)
				}
				return
			}
			assertJSON(t, "tool_choice", got, tc.want)
		})
	}
}

func TestAnthropicImageSourceForParsesDataURIs(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string // JSON of the source, or "" when the image is dropped
	}{
		{"plain base64", "data:image/png;base64,QUJD", `{"type":"base64","media_type":"image/png","data":"QUJD"}`},
		{"extra params stay out of media_type", "data:image/png;charset=utf-8;base64,QUJD", `{"type":"base64","media_type":"image/png","data":"QUJD"}`},
		{"base64 marker is case-insensitive", "data:image/jpeg;BASE64,QUJD", `{"type":"base64","media_type":"image/jpeg","data":"QUJD"}`},
		{"extra params and mixed-case marker", "data:image/webp;name=a.webp;Base64,QUJD", `{"type":"base64","media_type":"image/webp","data":"QUJD"}`},
		{"non-base64 data URI is dropped", "data:image/svg+xml;utf8,%3Csvg%3E", ""},
		{"no media type is dropped", "data:;base64,QUJD", ""},
		{"empty payload is dropped", "data:image/png;base64,", ""},
		{"no comma is dropped", "data:image/png;base64", ""},
		{"http URL", "https://example.test/cat.jpg", `{"type":"url","url":"https://example.test/cat.jpg"}`},
		{"file URL is dropped", "file:///etc/passwd", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicImageSourceFor(tc.url)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("anthropicImageSourceFor(%q) = %#v, want nil", tc.url, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("anthropicImageSourceFor(%q) = nil, want %s", tc.url, tc.want)
			}
			assertJSON(t, "source", got, tc.want)
		})
	}
}

func TestAnthropicClientCompleteRendersImagesAndClampsTemperature(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	temperature := 1.7 // valid for OpenAI (0..2), a 400 for Anthropic (0..1)
	req := inference.Request{
		Temperature: &temperature,
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: "what is this?"},
			{Type: inference.ContentImage, ImageURL: "data:image/png;base64,QUJD"},
			{Type: inference.ContentImage, ImageURL: "https://example.test/cat.jpg"},
			{Type: inference.ContentImage, ImageURL: "file:///etc/passwd"},
			{Type: inference.ContentText, Text: ""},
		}}},
	}

	if _, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	body := capture.body(t)
	assertJSON(t, "messages", body["messages"], `[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},
		{"type":"image","source":{"type":"url","url":"https://example.test/cat.jpg"}}
	]}]`)
	if body["temperature"] != float64(1) {
		t.Fatalf("temperature = %v, want clamped to 1", body["temperature"])
	}
}

func TestAnthropicClientCompleteParsesTextToolUseThinkingAndCanonicalUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(contentTypeHeader, jsonContentType)
		_, _ = io.WriteString(w, `{
			"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929",
			"content":[
				{"type":"thinking","thinking":"Weather needs a tool.","signature":"sig=="},
				{"type":"text","text":"Checking. "},
				{"type":"text","text":"One sec."},
				{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"Berlin"}}
			],
			"stop_reason":"tool_use","stop_sequence":null,
			"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"output_tokens":25}
		}`)
	}))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	if resp.Text != "Checking. One sec." {
		t.Fatalf("Text = %q", resp.Text)
	}
	if resp.Reasoning != "Weather needs a tool." {
		t.Fatalf("Reasoning = %q", resp.Reasoning)
	}
	wantCalls := []inference.ToolCall{{ID: "toolu_9", Name: "get_weather", Arguments: `{"city":"Berlin"}`}}
	if !reflect.DeepEqual(resp.ToolCalls, wantCalls) {
		t.Fatalf("ToolCalls = %#v, want %#v", resp.ToolCalls, wantCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	// OpenAI convention: InputTokens INCLUDES the cache read and cache write subsets.
	wantUsage := inference.Usage{InputTokens: 150, OutputTokens: 25, TotalTokens: 175, CachedTokens: 40, CacheWriteTokens: 10}
	if resp.Usage != wantUsage {
		t.Fatalf("Usage = %#v, want %#v", resp.Usage, wantUsage)
	}
}

func TestAnthropicClientCompleteAcceptsEmptyContentWithStopReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":0}}`)
	}))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Complete returned %v, want a valid empty reply (the usage was still billed)", err)
	}
	if resp.Text != "" || resp.FinishReason != "stop" || resp.Usage.InputTokens != 7 || resp.Usage.TotalTokens != 7 {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestAnthropicClientCompleteRejectsUnusableBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     `<html>`,
		"empty object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			client := NewAnthropicClient(http.DefaultClient)

			_, err := client.Complete(context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})

			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Complete error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestAnthropicFinishReasonMapsStopReasons(t *testing.T) {
	tests := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"pause_turn":                    "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"refusal":                       "content_filter",
		"some_future_reason":            "stop",
		"":                              "",
		"  tool_use ":                   "tool_calls",
		"MAX_TOKENS":                    "length",
	}
	for stopReason, want := range tests {
		if got := anthropicFinishReason(stopReason); got != want {
			t.Errorf("anthropicFinishReason(%q) = %q, want %q", stopReason, got, want)
		}
	}
}

func TestAnthropicClientMapsUpstreamStatusToProviderErrors(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrAuthRejected},
		{http.StatusForbidden, ErrAuthRejected},
		{http.StatusServiceUnavailable, ErrUpstreamStarting},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusBadRequest, ErrUnavailable},
	}
	for _, tc := range tests {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"x","message":"y"}}`)
		}))
		client := NewAnthropicClient(http.DefaultClient)
		req := inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}}

		_, completeErr := client.Complete(context.Background(), anthropicTarget(upstream.URL), req)
		streamErr := client.CompleteStream(context.Background(), anthropicTarget(upstream.URL), req, func(inference.StreamEvent) error { return nil })
		upstream.Close()

		if !errors.Is(completeErr, tc.want) || !errors.Is(completeErr, ErrUnavailable) {
			t.Errorf("Complete(%d) error = %v, want %v", tc.status, completeErr, tc.want)
		}
		if !errors.Is(streamErr, tc.want) || !errors.Is(streamErr, ErrUnavailable) {
			t.Errorf("CompleteStream(%d) error = %v, want %v", tc.status, streamErr, tc.want)
		}
	}
}

func TestAnthropicClientCompleteReturnsErrTimeoutWhenTargetTimeoutElapses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: the server only notices the client hanging up
		// (and cancels r.Context()) once the request body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	target := anthropicTarget(upstream.URL)
	target.Timeout = 50 * time.Millisecond

	_, err := client.Complete(context.Background(), target, inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Complete error = %v, want ErrTimeout", err)
	}
}

func TestAnthropicClientCompleteReturnsUnavailableWhenUpstreamIsDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	_, err := client.Complete(context.Background(), anthropicTarget(url), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Complete error = %v, want ErrUnavailable", err)
	}
}

// anthropicSSE joins event/data pairs into an SSE body the way api.anthropic.com
// frames them (an `event:` line, a `data:` line, a blank line).
func anthropicSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(f), &event)
		b.WriteString("event: " + event.Type + "\ndata: " + f + "\n\n")
	}
	return b.String()
}

func writeAnthropicSSE(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(contentTypeHeader, "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// collectStream runs CompleteStream and returns every emitted event.
func collectStream(t *testing.T, client *AnthropicClient, ctx context.Context, target routing.Target, req inference.Request) ([]inference.StreamEvent, error) {
	t.Helper()
	var events []inference.StreamEvent
	err := client.CompleteStream(ctx, target, req, func(ev inference.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func TestAnthropicClientCompleteStreamTranslatesTextThinkingAndToolUse(t *testing.T) {
	capture := &anthropicCapture{}
	sse := anthropicSSE(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"output_tokens":1}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Need "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weather."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig=="}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_7","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"ty\":\"Ber"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"lin\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_8","name":"ping","input":{}}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	)
	// A comment line (the SSE keepalive form) sits between frames.
	sse = strings.Replace(sse, "event: ping", ": keepalive\n\nevent: ping", 1)
	upstream := httptest.NewServer(capture.handler(writeAnthropicSSE(sse)))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	activity := 0
	ctx := WithStreamActivity(WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-test"), func() { activity++ })
	target := anthropicTarget(upstream.URL)

	events, err := collectStream(t, client, ctx, target, inference.Request{Model: "gw", Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}, Stream: true})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}

	method, path, header, _ := capture.request()
	if method != http.MethodPost || path != "/v1/messages" {
		t.Fatalf("request = %s %s, want POST /v1/messages", method, path)
	}
	if header.Get("anthropic-version") != "2023-06-01" || header.Get("x-api-key") != "sk-ant-test" {
		t.Fatalf("headers = %v, want anthropic-version + x-api-key", header)
	}
	body := capture.body(t)
	if body["stream"] != true || body["model"] != "claude-sonnet-4-5-20250929" || body["max_tokens"] != float64(4096) {
		t.Fatalf("stream body = %v", body)
	}
	if activity != 1 {
		t.Fatalf("stream activity hook ran %d times, want 1 (the one SSE comment line)", activity)
	}

	wantUsage := &inference.Usage{InputTokens: 150, OutputTokens: 42, TotalTokens: 192, CachedTokens: 40, CacheWriteTokens: 10}
	want := []inference.StreamEvent{
		{Type: inference.StreamEventTextDelta, Reasoning: "Need "},
		{Type: inference.StreamEventTextDelta, Reasoning: "weather."},
		{Type: inference.StreamEventTextDelta, Text: "Hel"},
		{Type: inference.StreamEventTextDelta, Text: "lo"},
		{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "toolu_7", Name: "get_weather", Arguments: `{"city":"Berlin"}`}},
		{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "toolu_8", Name: "ping", Arguments: `{}`}},
		{Type: inference.StreamEventCompleted, Usage: wantUsage, FinishReason: "tool_calls"},
	}
	if !reflect.DeepEqual(events, want) {
		got, _ := json.MarshalIndent(events, "", " ")
		exp, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("events mismatch\n got: %s\nwant: %s", got, exp)
	}
}

func TestAnthropicClientCompleteStreamReadsUsageFromMessageDeltaWhenPresent(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"A"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"B"}}`,
		// A final message_delta may restate the input side cumulatively.
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":12,"cache_read_input_tokens":3,"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	)
	upstream := httptest.NewServer(writeAnthropicSSE(sse))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	events, err := collectStream(t, client, context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}

	if len(events) != 3 || events[0].Text != "A" || events[1].Text != "B" {
		t.Fatalf("events = %#v, want the initial block text then the delta, then Completed", events)
	}
	last := events[len(events)-1]
	wantUsage := &inference.Usage{InputTokens: 15, OutputTokens: 9, TotalTokens: 24, CachedTokens: 3}
	if last.Type != inference.StreamEventCompleted || !reflect.DeepEqual(last.Usage, wantUsage) || last.FinishReason != "length" {
		t.Fatalf("Completed = %#v, want usage %#v finish length", last, wantUsage)
	}
}

func TestAnthropicClientCompleteStreamDoesNotApplyTargetTimeout(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // longer than target.Timeout: the stream idle watchdog governs, not the target timeout
		writeAnthropicSSE(sse)(w, r)
	}))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	target := anthropicTarget(upstream.URL)
	target.Timeout = 50 * time.Millisecond

	events, err := collectStream(t, client, context.Background(), target, inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("CompleteStream returned %v, want the target timeout not to apply to a stream", err)
	}
	if len(events) != 1 || events[0].Type != inference.StreamEventCompleted {
		t.Fatalf("events = %#v", events)
	}
}

func TestAnthropicClientCompleteStreamBoundsToolArguments(t *testing.T) {
	fragment := strings.Repeat("a", 600*1024)
	frames := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_big","name":"big","input":{}}}`,
	}
	for range 3 {
		frames = append(frames, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+fragment+`"}}`)
	}
	frames = append(frames,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
	upstream := httptest.NewServer(writeAnthropicSSE(anthropicSSE(frames...)))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	events, err := collectStream(t, client, context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}

	if len(events) != 2 || events[0].ToolCall == nil {
		t.Fatalf("events = %d, want a tool call then Completed", len(events))
	}
	if got := len(events[0].ToolCall.Arguments); got != maxToolArgumentsBytes {
		t.Fatalf("accumulated arguments = %d bytes, want exactly the %d byte bound", got, maxToolArgumentsBytes)
	}
}

func TestAnthropicClientCompleteStreamReturnsUnavailableOnErrorEvent(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	)
	upstream := httptest.NewServer(writeAnthropicSSE(sse))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	events, err := collectStream(t, client, context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})

	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "overloaded_error") || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("CompleteStream error = %v, want ErrUnavailable naming the upstream error", err)
	}
	if len(events) != 1 || events[0].Text != "partial" {
		t.Fatalf("events = %#v, want only the delta emitted before the error (no Completed)", events)
	}
}

func TestAnthropicClientCompleteStreamReturnsUnavailableWhenStreamEndsEarly(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut off"}}`,
	)
	upstream := httptest.NewServer(writeAnthropicSSE(sse))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)

	events, err := collectStream(t, client, context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CompleteStream error = %v, want ErrUnavailable for a stream that ends without message_stop", err)
	}
	for _, ev := range events {
		if ev.Type == inference.StreamEventCompleted {
			t.Fatalf("a truncated stream was reported as Completed: %#v", events)
		}
	}
}

func TestAnthropicClientCompleteStreamAbortsWhenEmitFails(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
		`{"type":"message_stop"}`,
	)
	upstream := httptest.NewServer(writeAnthropicSSE(sse))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	gone := errors.New("client disconnected")

	err := client.CompleteStream(context.Background(), anthropicTarget(upstream.URL), inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}}, func(inference.StreamEvent) error { return gone })

	if !errors.Is(err, gone) {
		t.Fatalf("CompleteStream error = %v, want the emit error", err)
	}
}

func TestAnthropicClientCapturesTheTranslatedExchange(t *testing.T) {
	sse := anthropicSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)
	for name, run := range map[string]func(ctx context.Context, c *AnthropicClient, target routing.Target) error{
		"complete": func(ctx context.Context, c *AnthropicClient, target routing.Target) error {
			_, err := c.Complete(ctx, target, inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
			return err
		},
		"stream": func(ctx context.Context, c *AnthropicClient, target routing.Target) error {
			_, err := collectStream(t, c, ctx, target, inference.Request{Messages: []inference.Message{anthropicTextMsg(inference.RoleUser, "hi")}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			wantBody := sse
			respond := writeAnthropicSSE(sse)
			if name == "complete" {
				wantBody = anthropicOKResponse
				respond = writeAnthropicOK
			}
			upstream := httptest.NewServer(respond)
			defer upstream.Close()
			sink := NewCaptureSink(1 << 20)
			ctx := WithCaptureSink(WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-test"), sink)

			if err := run(ctx, NewAnthropicClient(http.DefaultClient), anthropicTarget(upstream.URL)); err != nil {
				t.Fatalf("call returned %v", err)
			}

			if !strings.Contains(string(sink.RequestBody()), `"model":"claude-sonnet-4-5-20250929"`) {
				t.Fatalf("captured request body = %s", sink.RequestBody())
			}
			if got := sink.RequestHeaders().Get("anthropic-version"); got != "2023-06-01" {
				t.Fatalf("captured request anthropic-version = %q", got)
			}
			if got := string(sink.ResponseBody()); got != wantBody {
				t.Fatalf("captured response = %q, want the raw upstream body %q", got, wantBody)
			}
			if sink.ResponseHeaders() == nil {
				t.Fatal("response headers were not captured")
			}
		})
	}
}

// anthropicNativeBody is deliberately NOT what encoding/json would emit for the
// same value (odd spacing, a newline inside the object, stream before model,
// \u00e9-style escapes that json.Marshal would turn back into raw runes, and the
// U+2028 escape), so a passthrough that decoded and re-encoded it would fail the
// byte-for-byte comparison below.
const anthropicNativeBody = `{ "stream":true,"model":"claude-sonnet-4-5-20250929",  "max_tokens": 64,
 "system":"s\u00e9 \u2028 caf\u00e9 \u2603","messages":[{"role":"user","content":[{"type":"text","text":"h\u00e9llo \u2603"}]}],"x_unknown":{"b":1,"a":[3,2,1]} }`

// anthropicNativeSSE is a Messages SSE reply with event: lines, a keepalive
// comment and a data: line with extra spacing; the passthrough must relay it
// unchanged, where the translate path would parse and drop all of that.
const anthropicNativeSSE = ": keepalive\n\n" +
	"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
	"event: message_stop\ndata:   {\"type\":\"message_stop\"}\n\n"

func TestAnthropicClientProxyNativeForwardsRawBodyVersionAndAuth(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, "text/event-stream")
		w.Header().Set("request-id", "req_native_1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicNativeSSE)
	}))
	defer upstream.Close()
	client := NewAnthropicClient(http.DefaultClient)
	ctx := WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-native")
	// An api-key target (no Masquerade) is relayed verbatim; the subscription
	// masquerade injection is pinned in anthropic_masquerade_test.go.
	resp, err := client.ProxyNative(ctx, anthropicTarget(upstream.URL+"/"), "/v1/messages", []byte(anthropicNativeBody))
	if err != nil {
		t.Fatalf("ProxyNative returned %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(contentTypeHeader); got != "text/event-stream" {
		t.Fatalf("response Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("request-id"); got != "req_native_1" {
		t.Fatalf("response request-id = %q, want the upstream header relayed", got)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read relayed body: %v", err)
	}
	if string(out) != anthropicNativeSSE {
		t.Fatalf("relayed SSE = %q, want it byte-for-byte %q", out, anthropicNativeSSE)
	}

	method, path, header, body := capture.request()
	if method != http.MethodPost || path != "/v1/messages" {
		t.Fatalf("request = %s %s, want POST /v1/messages", method, path)
	}
	if string(body) != anthropicNativeBody {
		t.Fatalf("forwarded body = %q, want it verbatim %q", body, anthropicNativeBody)
	}
	if got := header.Get(contentTypeHeader); got != jsonContentType {
		t.Fatalf("Content-Type = %q, want %q", got, jsonContentType)
	}
	if got := header.Values("anthropic-version"); len(got) != 1 || got[0] != "2023-06-01" {
		t.Fatalf("anthropic-version = %v, want exactly [2023-06-01]", got)
	}
	if got := header.Get("x-api-key"); got != "sk-ant-native" {
		t.Fatalf("x-api-key = %q, want the upstream credential from ctx", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none (an api-key target sends x-api-key only)", got)
	}
}

// TestAnthropicClientProxyNativeAlwaysSendsAnthropicVersion pins the design: the
// client itself guarantees anthropic-version (api.anthropic.com requires it), so
// it reaches the upstream whether or not the caller's ctx carried it.
func TestAnthropicClientProxyNativeAlwaysSendsAnthropicVersion(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no upstream auth on ctx", context.Background(), "2023-06-01"},
		{"credential without extra headers", WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-x"), "2023-06-01"},
		{"extra headers without the version", WithUpstreamAuthHeaders(context.Background(), "x-api-key", "sk-ant-x", map[string]string{"anthropic-beta": "b1"}), "2023-06-01"},
		{"target-carried version is the caller's choice", WithUpstreamAuthHeaders(context.Background(), "x-api-key", "sk-ant-x", map[string]string{"anthropic-version": "2099-01-01"}), "2099-01-01"},
		{"target-carried version in another case", WithUpstreamAuthHeaders(context.Background(), "x-api-key", "sk-ant-x", map[string]string{"Anthropic-Version": "2099-01-01"}), "2099-01-01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture := &anthropicCapture{}
			upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
			defer upstream.Close()

			resp, err := NewAnthropicClient(http.DefaultClient).ProxyNative(tc.ctx, anthropicTarget(upstream.URL), "/v1/messages", []byte(`{"model":"m"}`))
			if err != nil {
				t.Fatalf("ProxyNative returned %v", err)
			}
			resp.Body.Close()

			_, _, header, _ := capture.request()
			if got := header.Values("anthropic-version"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("anthropic-version = %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

func TestAnthropicClientProxyNativeForwardsTargetExtraHeaders(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeAnthropicOK))
	defer upstream.Close()
	ctx := WithUpstreamAuthHeaders(context.Background(), "x-api-key", "sk-ant-x", map[string]string{"anthropic-beta": "prompt-caching-2024-07-31"})

	resp, err := NewAnthropicClient(http.DefaultClient).ProxyNative(ctx, anthropicTarget(upstream.URL), "/v1/messages", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("ProxyNative returned %v", err)
	}
	resp.Body.Close()

	_, _, header, _ := capture.request()
	if got := header.Get("anthropic-beta"); got != "prompt-caching-2024-07-31" {
		t.Fatalf("anthropic-beta = %q, want the ctx extra header forwarded", got)
	}
}

func TestAnthropicClientProxyNativeRelaysUpstreamErrorStatusVerbatim(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, jsonContentType)
		w.Header().Set("retry-after", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, errBody)
	}))
	defer upstream.Close()

	resp, err := NewAnthropicClient(http.DefaultClient).ProxyNative(context.Background(), anthropicTarget(upstream.URL), "/v1/messages", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("ProxyNative returned %v, want the 429 relayed as a response", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("retry-after"); got != "7" {
		t.Fatalf("retry-after = %q, want 7", got)
	}
	if out, _ := io.ReadAll(resp.Body); string(out) != errBody {
		t.Fatalf("relayed body = %q, want %q", out, errBody)
	}
}

func TestAnthropicClientProxyNativeMapsTransportFailureToUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	endpoint := upstream.URL
	upstream.Close() // nothing listens any more

	_, err := NewAnthropicClient(http.DefaultClient).ProxyNative(context.Background(), anthropicTarget(endpoint), "/v1/messages", []byte(`{}`))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProxyNative error = %v, want ErrUnavailable", err)
	}
}

// TestAnthropicClientProxyNativeDoesNotFollowRedirects pins that the credential
// (x-api-key survives net/http's cross-host redirect header stripping, which only
// drops Authorization) never leaves for a redirect target, and that the 3xx is
// handed back as the upstream's answer instead. The caller's client is untouched.
func TestAnthropicClientProxyNativeDoesNotFollowRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
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
	httpClient := &http.Client{}
	ctx := WithUpstreamAuth(context.Background(), "x-api-key", "sk-ant-secret")

	resp, err := NewAnthropicClient(httpClient).ProxyNative(ctx, anthropicTarget(upstream.URL), "/v1/messages", []byte(`{"model":"m"}`))
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
		t.Fatalf("the redirect target received %d request(s), want 0 (the credential must not follow a redirect)", hits)
	}
	if httpClient.CheckRedirect != nil {
		t.Fatal("ProxyNative mutated the caller's http.Client.CheckRedirect")
	}
}
