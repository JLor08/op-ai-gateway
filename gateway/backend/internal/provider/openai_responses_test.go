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
	"strconv"
	"strings"
	"testing"
	"time"
)

// openaiResponsesSSE frames each JSON payload the way the ChatGPT backend does: an
// `event:` line (the payload's own type), a `data:` line, and a blank line.
func openaiResponsesSSE(frames ...string) string {
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

func writeOpenAIResponsesSSE(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// openaiResponsesCompletedFrame is a terminal response.completed frame carrying the
// given usage (input_tokens already includes cached, OpenAI semantics).
func openaiResponsesCompletedFrame(input, output, cached int) string {
	return `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":` +
		strconv.Itoa(input) + `,"output_tokens":` + strconv.Itoa(output) + `,"total_tokens":` + strconv.Itoa(input+output) +
		`,"input_tokens_details":{"cached_tokens":` + strconv.Itoa(cached) + `}}}}`
}

func openaiResponsesTarget(endpoint string) routing.Target {
	return routing.Target{
		Endpoint:      endpoint,
		Provider:      routing.ProviderVendorOpenAISubscription,
		ProviderModel: "gpt-5-codex-upstream",
		Subscription:  true,
		Timeout:       5 * time.Second,
	}
}

func openaiResponsesTextMsg(role inference.Role, text string) inference.Message {
	return inference.Message{Role: role, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}
}

func TestOpenAIResponsesClientImplementsProviderInterfaces(t *testing.T) {
	var _ Client = NewOpenAIResponsesClient(nil)
	var _ StreamingClient = NewOpenAIResponsesClient(nil)
	var _ NativeProxyClient = NewOpenAIResponsesClient(nil)
}

func TestOpenAIResponsesClientCompleteRendersBodyWithToolRoundTrip(t *testing.T) {
	capture := &anthropicCapture{}
	sse := openaiResponsesSSE(
		`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
		openaiResponsesCompletedFrame(1, 1, 0),
	)
	upstream := httptest.NewServer(capture.handler(writeOpenAIResponsesSSE(sse)))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	req := inference.Request{
		Model:           "gw-model",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []inference.Message{
			openaiResponsesTextMsg(inference.RoleSystem, "You are terse."),
			openaiResponsesTextMsg(inference.RoleDeveloper, "Answer in German."),
			openaiResponsesTextMsg(inference.RoleUser, "Weather in Berlin?"),
			{
				Role:      inference.RoleAssistant,
				Content:   []inference.ContentPart{{Type: inference.ContentText, Text: "Let me check."}},
				ToolCalls: []inference.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Berlin"}`}},
				Reasoning: "secret chain of thought",
			},
			{Role: inference.RoleTool, ToolCallID: "call_1", Content: []inference.ContentPart{{Type: inference.ContentText, Text: `{"temp":21}`}}},
			openaiResponsesTextMsg(inference.RoleUser, "And tomorrow?"),
		},
		Tools: []inference.Tool{
			{Name: "get_weather", Description: "Current weather", Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			}},
		},
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	}

	if _, err := client.Complete(context.Background(), openaiResponsesTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	method, path, _, _ := capture.request()
	if method != http.MethodPost || path != "/responses" {
		t.Fatalf("request = %s %s, want POST /responses", method, path)
	}
	body := capture.body(t)
	if body["model"] != "gpt-5-codex-upstream" {
		t.Fatalf("model = %v, want the provider model", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true (the backend is stream-only)", body["stream"])
	}
	if body["instructions"] != "You are terse.\n\nAnswer in German." {
		t.Fatalf("instructions = %q", body["instructions"])
	}
	if body["max_output_tokens"] != float64(256) {
		t.Fatalf("max_output_tokens = %v, want 256", body["max_output_tokens"])
	}
	assertJSON(t, "reasoning", body["reasoning"], `{"effort":"high"}`)
	assertJSON(t, "tools", body["tools"], `[
		{"type":"function","name":"get_weather","description":"Current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
	]`)
	assertJSON(t, "tool_choice", body["tool_choice"], `{"type":"function","name":"get_weather"}`)
	assertJSON(t, "input", body["input"], `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Weather in Berlin?"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check."}]},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Berlin\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":21}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"And tomorrow?"}]}
	]`)
	if _, _, _, raw := capture.request(); strings.Contains(string(raw), "secret chain of thought") {
		t.Fatalf("assistant Reasoning leaked into the Responses request: %s", raw)
	}
}

func TestOpenAIResponsesClientCompleteRendersImagesAndOmitsToolChoiceWithoutTools(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeOpenAIResponsesSSE(openaiResponsesSSE(openaiResponsesCompletedFrame(1, 1, 0)))))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	req := inference.Request{
		ToolChoice: "auto", // present, but no tools -> must be dropped
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: "what is this?"},
			{Type: inference.ContentImage, ImageURL: "data:image/png;base64,QUJD"},
			{Type: inference.ContentImage, ImageURL: "https://example.test/cat.jpg"},
			{Type: inference.ContentImage, ImageURL: "file:///etc/passwd"},
			{Type: inference.ContentText, Text: ""},
		}}},
	}

	if _, err := client.Complete(context.Background(), openaiResponsesTarget(upstream.URL), req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	body := capture.body(t)
	assertJSON(t, "input", body["input"], `[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"what is this?"},
		{"type":"input_image","image_url":"data:image/png;base64,QUJD"},
		{"type":"input_image","image_url":"https://example.test/cat.jpg"}
	]}]`)
	if _, present := body["tool_choice"]; present {
		t.Fatalf("tool_choice sent without tools: %v", body["tool_choice"])
	}
	if _, present := body["tools"]; present {
		t.Fatalf("tools = %v, want absent", body["tools"])
	}
	if _, present := body["instructions"]; present {
		t.Fatalf("instructions = %v, want absent when there is no system text", body["instructions"])
	}
}

func TestOpenAIResponsesClientCompleteStreamTranslatesTextReasoningAndToolCall(t *testing.T) {
	capture := &anthropicCapture{}
	sse := openaiResponsesSSE(
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"Think "}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"hard."}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Hel"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"lo"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"get_weather"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"ci"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"ty\":\"Ber"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"lin\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":2,"item_id":"fc_1","arguments":"IGNORED"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}}`,
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":12}}}`,
		openaiResponsesCompletedFrame(100, 42, 40),
	)
	// A keepalive comment line sits between frames (drives the activity hook).
	sse = strings.Replace(sse, "event: response.created", ": keepalive\n\nevent: response.created", 1)
	upstream := httptest.NewServer(capture.handler(writeOpenAIResponsesSSE(sse)))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)
	activity := 0
	ctx := WithStreamActivity(context.Background(), func() { activity++ })

	events, err := collectResponsesStream(t, client, ctx, openaiResponsesTarget(upstream.URL), inference.Request{Model: "gw", Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}, Stream: true})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if activity != 1 {
		t.Fatalf("stream activity hook ran %d times, want 1", activity)
	}

	wantUsage := &inference.Usage{InputTokens: 100, OutputTokens: 42, TotalTokens: 142, CachedTokens: 40}
	want := []inference.StreamEvent{
		{Type: inference.StreamEventTextDelta, Reasoning: "Think "},
		{Type: inference.StreamEventTextDelta, Reasoning: "hard."},
		{Type: inference.StreamEventTextDelta, Text: "Hel"},
		{Type: inference.StreamEventTextDelta, Text: "lo"},
		{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "call_9", Name: "get_weather", Arguments: `{"city":"Berlin"}`}},
		{Type: inference.StreamEventCompleted, Usage: wantUsage, FinishReason: "tool_calls"},
	}
	if !reflect.DeepEqual(events, want) {
		got, _ := json.MarshalIndent(events, "", " ")
		exp, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("events mismatch\n got: %s\nwant: %s", got, exp)
	}
}

func TestOpenAIResponsesClientCompleteAggregatesStream(t *testing.T) {
	sse := openaiResponsesSSE(
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"Think."}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Answer "}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"text."}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_x","name":"do_it"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"a\":1}"}`,
		openaiResponsesCompletedFrame(10, 5, 2),
	)
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(sse))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if resp.Text != "Answer text." {
		t.Fatalf("Text = %q", resp.Text)
	}
	if resp.Reasoning != "Think." {
		t.Fatalf("Reasoning = %q", resp.Reasoning)
	}
	wantCalls := []inference.ToolCall{{ID: "call_x", Name: "do_it", Arguments: `{"a":1}`}}
	if !reflect.DeepEqual(resp.ToolCalls, wantCalls) {
		t.Fatalf("ToolCalls = %#v, want %#v", resp.ToolCalls, wantCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	wantUsage := inference.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedTokens: 2}
	if resp.Usage != wantUsage {
		t.Fatalf("Usage = %#v, want %#v", resp.Usage, wantUsage)
	}
}

func TestOpenAIResponsesClientIncompleteMapsToLength(t *testing.T) {
	sse := openaiResponsesSSE(
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":4,"output_tokens":9,"total_tokens":13}}}`,
	)
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(sse))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	events, err := collectResponsesStream(t, client, context.Background(), openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	last := events[len(events)-1]
	if last.Type != inference.StreamEventCompleted || last.FinishReason != "length" {
		t.Fatalf("last event = %#v, want Completed with finish length", last)
	}
	if last.Usage == nil || last.Usage.OutputTokens != 9 || last.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %#v", last.Usage)
	}
}

func TestOpenAIResponsesClientCompleteStreamReturnsUnavailableOnFailedEvent(t *testing.T) {
	sse := openaiResponsesSSE(
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`,
	)
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(sse))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	events, err := collectResponsesStream(t, client, context.Background(), openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CompleteStream error = %v, want ErrUnavailable", err)
	}
	for _, ev := range events {
		if ev.Type == inference.StreamEventCompleted {
			t.Fatalf("a failed stream was reported as Completed: %#v", events)
		}
	}
}

func TestOpenAIResponsesClientCompleteStreamReturnsUnavailableWhenStreamEndsEarly(t *testing.T) {
	sse := openaiResponsesSSE(`{"type":"response.output_text.delta","output_index":0,"delta":"cut off"}`)
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(sse))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	events, err := collectResponsesStream(t, client, context.Background(), openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CompleteStream error = %v, want ErrUnavailable for a stream that ends before response.completed", err)
	}
	for _, ev := range events {
		if ev.Type == inference.StreamEventCompleted {
			t.Fatalf("a truncated stream was reported as Completed: %#v", events)
		}
	}
}

func TestOpenAIResponsesClientMapsUpstreamStatusToProviderErrors(t *testing.T) {
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
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":{"message":"x"}}`)
		}))
		client := NewOpenAIResponsesClient(http.DefaultClient)
		req := inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}}

		_, completeErr := client.Complete(context.Background(), openaiResponsesTarget(upstream.URL), req)
		streamErr := client.CompleteStream(context.Background(), openaiResponsesTarget(upstream.URL), req, func(inference.StreamEvent) error { return nil })
		upstream.Close()

		if !errors.Is(completeErr, tc.want) || !errors.Is(completeErr, ErrUnavailable) {
			t.Errorf("Complete(%d) error = %v, want %v", tc.status, completeErr, tc.want)
		}
		if !errors.Is(streamErr, tc.want) || !errors.Is(streamErr, ErrUnavailable) {
			t.Errorf("CompleteStream(%d) error = %v, want %v", tc.status, streamErr, tc.want)
		}
	}
}

func TestOpenAIResponsesClientAttachesBearerAndHeadersFromCtx(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(writeOpenAIResponsesSSE(openaiResponsesSSE(openaiResponsesCompletedFrame(1, 1, 0)))))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)
	// The empty header name is the bearer form; the extra headers mirror what
	// subscriptionAuthCtx attaches for an OpenAI subscription target.
	ctx := WithUpstreamAuthHeaders(context.Background(), "", "oauth-token", map[string]string{
		"OpenAI-Beta":        "responses=experimental",
		"originator":         "codex_cli_rs",
		"chatgpt-account-id": "acc-123",
	})

	if _, err := client.Complete(ctx, openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}}); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	_, _, header, _ := capture.request()
	if got := header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer oauth-token", got)
	}
	if got := header.Get("OpenAI-Beta"); got != "responses=experimental" {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	if got := header.Get("originator"); got != "codex_cli_rs" {
		t.Fatalf("originator = %q", got)
	}
	if got := header.Get("chatgpt-account-id"); got != "acc-123" {
		t.Fatalf("chatgpt-account-id = %q", got)
	}
}

func TestOpenAIResponsesClientProxyNativeForwardsRawBodyAndAuth(t *testing.T) {
	capture := &anthropicCapture{}
	upstream := httptest.NewServer(capture.handler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {}\n\n")
	}))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)
	ctx := WithUpstreamAuthHeaders(context.Background(), "", "oauth-token", map[string]string{"chatgpt-account-id": "acc-123"})
	raw := []byte(`{"model":"gpt-5-codex","input":"hi","stream":true}`)

	resp, err := client.ProxyNative(ctx, openaiResponsesTarget(upstream.URL), "/responses", raw)
	if err != nil {
		t.Fatalf("ProxyNative returned %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}

	method, path, header, body := capture.request()
	if method != http.MethodPost || path != "/responses" {
		t.Fatalf("request = %s %s, want POST /responses", method, path)
	}
	if string(body) != string(raw) {
		t.Fatalf("forwarded body = %s, want it verbatim %s", body, raw)
	}
	if header.Get("Authorization") != "Bearer oauth-token" || header.Get("chatgpt-account-id") != "acc-123" {
		t.Fatalf("passthrough headers = %v, want the bearer + account id", header)
	}
}

func TestOpenAIResponsesToolChoiceMapsForms(t *testing.T) {
	tests := []struct {
		name   string
		choice any
		want   string // JSON, or "" for nil
	}{
		{"auto", "auto", `"auto"`},
		{"required", "required", `"required"`},
		{"none", "none", `"none"`},
		{"named function", map[string]any{"type": "function", "function": map[string]any{"name": "f"}}, `{"type":"function","name":"f"}`},
		{"flat responses function", map[string]any{"type": "function", "name": "f"}, `{"type":"function","name":"f"}`},
		{"named function without a name", map[string]any{"type": "function", "function": map[string]any{}}, ""},
		{"unknown string", "bogus", ""},
		{"nil", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := openaiResponsesToolChoiceFor(tc.choice)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("openaiResponsesToolChoiceFor(%v) = %#v, want nil", tc.choice, got)
				}
				return
			}
			assertJSON(t, "tool_choice", got, tc.want)
		})
	}
}

func TestOpenAIResponsesClientCompleteStreamBoundsToolArguments(t *testing.T) {
	fragment := strings.Repeat("a", 600*1024)
	frames := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_big","name":"big"}}`,
	}
	for range 3 {
		frames = append(frames, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"`+fragment+`"}`)
	}
	frames = append(frames, openaiResponsesCompletedFrame(1, 5, 0))
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(openaiResponsesSSE(frames...)))
	defer upstream.Close()
	client := NewOpenAIResponsesClient(http.DefaultClient)

	events, err := collectResponsesStream(t, client, context.Background(), openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}})
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

func TestOpenAIResponsesClientCapturesTheTranslatedExchange(t *testing.T) {
	sse := openaiResponsesSSE(
		`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
		openaiResponsesCompletedFrame(1, 1, 0),
	)
	upstream := httptest.NewServer(writeOpenAIResponsesSSE(sse))
	defer upstream.Close()
	sink := NewCaptureSink(1 << 20)
	ctx := WithCaptureSink(WithUpstreamAuthHeaders(context.Background(), "", "oauth-token", nil), sink)

	if _, err := collectResponsesStream(t, NewOpenAIResponsesClient(http.DefaultClient), ctx, openaiResponsesTarget(upstream.URL), inference.Request{Messages: []inference.Message{openaiResponsesTextMsg(inference.RoleUser, "hi")}}); err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if !strings.Contains(string(sink.RequestBody()), `"model":"gpt-5-codex-upstream"`) {
		t.Fatalf("captured request body = %s", sink.RequestBody())
	}
	if got := string(sink.ResponseBody()); got != sse {
		t.Fatalf("captured response = %q, want the raw upstream SSE", got)
	}
	if sink.ResponseHeaders() == nil {
		t.Fatal("response headers were not captured")
	}
}

// collectResponsesStream runs CompleteStream and returns every emitted event.
func collectResponsesStream(t *testing.T, client *OpenAIResponsesClient, ctx context.Context, target routing.Target, req inference.Request) ([]inference.StreamEvent, error) {
	t.Helper()
	var events []inference.StreamEvent
	err := client.CompleteStream(ctx, target, req, func(ev inference.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}
