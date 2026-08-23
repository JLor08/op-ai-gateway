// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleClientCompletesChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "openai compatible answer"}}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
		})
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if resp.Text != "openai compatible answer" {
		t.Fatalf("Text = %q", resp.Text)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("Usage = %#v", resp.Usage)
	}
}

func TestOpenAICompatibleClientCompletesReasoningOnly(t *testing.T) {
	// A reasoning model can return empty content with only reasoning_content (e.g.
	// truncated in its analysis channel). That is NOT a missing-content error — the
	// reasoning must be captured and returned, matching the streaming path.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "", "reasoning_content": "still thinking"}, "finish_reason": "length"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "gpt-oss", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Complete returned %v, want nil (reasoning-only is valid)", err)
	}
	if resp.Text != "" {
		t.Fatalf("Text = %q, want empty", resp.Text)
	}
	if resp.Reasoning != "still thinking" {
		t.Fatalf("Reasoning = %q, want %q", resp.Reasoning, "still thinking")
	}
}

func TestOpenAICompatibleClientProbeReachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/health" {
			t.Fatalf("path = %s, want /v1/health", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	if err := client.Probe(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second}, "/v1/health"); err != nil {
		t.Fatalf("Probe returned %v, want nil", err)
	}
}

func TestOpenAICompatibleClientProbeUnreachableIncludesStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	err := client.Probe(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second}, "/v1/health")
	if err == nil {
		t.Fatal("Probe returned nil, want an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("Probe error = %v, want it to include the status code 500", err)
	}
}

func TestOpenAICompatibleClientCompletesChatWithCachedTokensAndTimings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "openai compatible answer"}}},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 4,
				},
			},
			"timings": map[string]any{
				"prompt_per_second":    123.4,
				"predicted_per_second": 56.7,
			},
		})
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if resp.Usage.CachedTokens != 4 {
		t.Fatalf("Usage.CachedTokens = %v, want 4", resp.Usage.CachedTokens)
	}
	if resp.Usage.PromptPerSecond != 123.4 {
		t.Fatalf("Usage.PromptPerSecond = %v, want 123.4", resp.Usage.PromptPerSecond)
	}
	if resp.Usage.TokensPerSecond != 56.7 {
		t.Fatalf("Usage.TokensPerSecond = %v, want 56.7", resp.Usage.TokensPerSecond)
	}
}

func TestOpenAICompatibleClientListModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s, want /v1/models", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "m1"}, {"id": "m2"}},
		})
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	models, err := client.ListModels(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ListModels returned %v", err)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "m2" {
		t.Fatalf("models = %#v, want [m1 m2]", models)
	}
}

func TestOpenAICompatibleClientListModelsReturnsUnavailableForNon2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	_, err := client.ListModels(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ListModels error = %v, want ErrUnavailable", err)
	}
}

func TestOpenAICompatibleCompleteStreamParsesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"reasoning_content":"th"}}]}`,
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var events []inference.StreamEvent
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}

	var text, reasoning string
	var completed *inference.StreamEvent
	for i := range events {
		switch events[i].Type {
		case inference.StreamEventTextDelta:
			text += events[i].Text
			reasoning += events[i].Reasoning
		case inference.StreamEventCompleted:
			completed = &events[i]
		}
	}
	if text != "Hello" {
		t.Fatalf("text = %q, want Hello", text)
	}
	if reasoning != "th" {
		t.Fatalf("reasoning = %q, want th", reasoning)
	}
	if completed == nil {
		t.Fatalf("no StreamEventCompleted event; events = %#v", events)
	}
	if completed.Usage == nil {
		t.Fatalf("completed.Usage is nil")
	}
	if completed.Usage.TotalTokens != 5 || completed.Usage.InputTokens != 3 || completed.Usage.OutputTokens != 2 {
		t.Fatalf("completed.Usage = %#v", completed.Usage)
	}
}

func TestOpenAICompatibleCompleteStreamParsesSSEWithCachedTokensAndTimings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":4}},"timings":{"prompt_per_second":123.4,"predicted_per_second":56.7}}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var completed *inference.StreamEvent
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
		if ev.Type == inference.StreamEventCompleted {
			c := ev
			completed = &c
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if completed == nil || completed.Usage == nil {
		t.Fatal("no usage in completed event")
	}
	if completed.Usage.CachedTokens != 4 {
		t.Fatalf("Usage.CachedTokens = %v, want 4", completed.Usage.CachedTokens)
	}
	if completed.Usage.PromptPerSecond != 123.4 {
		t.Fatalf("Usage.PromptPerSecond = %v, want 123.4", completed.Usage.PromptPerSecond)
	}
	if completed.Usage.TokensPerSecond != 56.7 {
		t.Fatalf("Usage.TokensPerSecond = %v, want 56.7", completed.Usage.TokensPerSecond)
	}
}

func TestOpenAICompatibleCompleteStreamRequestBody(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyCh <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	temp := 0.5
	req := inference.Request{
		Temperature: &temp,
		MaxTokens:   64,
		Stop:        []string{"STOP", "\n\nHuman:"},
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: "describe"},
			{Type: inference.ContentImage, ImageURL: "data:image/png;base64,AAAA"},
		}}},
	}
	if err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, req, func(inference.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(<-bodyCh, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %#v, want true", body["stream"])
	}
	opts, ok := body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true", body["stream_options"])
	}
	if body["temperature"] != 0.5 {
		t.Fatalf("temperature = %#v, want 0.5", body["temperature"])
	}
	if body["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %#v, want 64", body["max_tokens"])
	}
	stop, ok := body["stop"].([]any)
	if !ok || len(stop) != 2 || stop[0] != "STOP" || stop[1] != "\n\nHuman:" {
		t.Fatalf("stop = %#v, want [STOP, \\n\\nHuman:]", body["stop"])
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	msg := messages[0].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %#v", msg["content"])
	}
	var hasText, hasImage bool
	for _, block := range content {
		b := block.(map[string]any)
		switch b["type"] {
		case "text":
			hasText = true
		case "image_url":
			img, ok := b["image_url"].(map[string]any)
			if !ok || img["url"] != "data:image/png;base64,AAAA" {
				t.Fatalf("image_url block = %#v", b)
			}
			hasImage = true
		}
	}
	if !hasText || !hasImage {
		t.Fatalf("content missing text/image block: %#v", content)
	}
}

func TestOpenAICompatibleCompleteForwardsImages(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyCh <- raw
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	req := inference.Request{Messages: []inference.Message{
		{Role: inference.RoleSystem, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "sys"}}},
		{Role: inference.RoleUser, Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: "describe"},
			{Type: inference.ContentImage, ImageURL: "data:image/png;base64,AAAA"},
		}},
	}}
	if _, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(<-bodyCh, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	// Text-only message keeps a plain-string content.
	sys := messages[0].(map[string]any)
	if sys["content"] != "sys" {
		t.Fatalf("system content = %#v, want plain string \"sys\"", sys["content"])
	}
	// Image message serializes to a content array with an image_url block.
	user := messages[1].(map[string]any)
	content, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("user content is not an array: %#v", user["content"])
	}
	var hasText, hasImage bool
	for _, block := range content {
		b := block.(map[string]any)
		switch b["type"] {
		case "text":
			hasText = true
		case "image_url":
			img, ok := b["image_url"].(map[string]any)
			if !ok || img["url"] != "data:image/png;base64,AAAA" {
				t.Fatalf("image_url block = %#v", b)
			}
			hasImage = true
		}
	}
	if !hasText || !hasImage {
		t.Fatalf("user content missing text/image block: %#v", content)
	}
}

func TestOpenAICompatibleCompleteStreamSurfacesErrorFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"error\":{\"message\":\"boom\"}}\n\n")
	}))
	defer server.Close()
	c := NewOpenAICompatibleClient(server.Client())
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	err := c.CompleteStream(context.Background(), routing.Target{Endpoint: server.URL}, req, func(inference.StreamEvent) error { return nil })
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestOpenAICompatibleCompleteStreamNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := NewOpenAICompatibleClient(server.Client())
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	err := c.CompleteStream(context.Background(), routing.Target{Endpoint: server.URL}, req, func(inference.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestOpenAICompatibleCompleteStreamIgnoresTotalTimeout(t *testing.T) {
	// Upstream streams 3 SSE chunks with 30ms gaps (~90ms total), longer than the
	// tiny target.Timeout. With a caller ctx that has no deadline, the full stream
	// must arrive — proving CompleteStream no longer imposes target.Timeout as a
	// total deadline.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			time.Sleep(30 * time.Millisecond)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x%d\"}}]}\n\n", i)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	client := NewOpenAICompatibleClient(upstream.Client())
	var deltas int
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen-coder", Timeout: 10 * time.Millisecond},
		inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}},
		func(ev inference.StreamEvent) error {
			if ev.Type == inference.StreamEventTextDelta && ev.Text != "" {
				deltas++
			}
			return nil
		})
	if err != nil {
		t.Fatalf("CompleteStream returned %v, want nil", err)
	}
	if deltas != 3 {
		t.Fatalf("received %d text deltas, want 3 (total-timeout must not truncate)", deltas)
	}
}

func TestOpenAIMessagesThreadsReasoningContent(t *testing.T) {
	msgs := openAIMessages([]inference.Message{
		{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}},
		{Role: inference.RoleAssistant, Reasoning: "let me think", ToolCalls: []inference.ToolCall{{ID: "c1", Name: "shell", Arguments: `{}`}}},
	})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	// The user message carries no reasoning_content.
	if _, ok := msgs[0]["reasoning_content"]; ok {
		t.Fatalf("user message must not carry reasoning_content: %v", msgs[0])
	}
	// The assistant message threads reasoning_content for upstream continuity.
	if rc, _ := msgs[1]["reasoning_content"].(string); rc != "let me think" {
		t.Fatalf("assistant reasoning_content = %q, want %q (%v)", rc, "let me think", msgs[1])
	}
}

func TestOpenAISamplingForwardsReasoningEffort(t *testing.T) {
	body := map[string]any{}
	openAISamplingFields(body, inference.Request{ReasoningEffort: "high"})
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	// Absent effort adds no field.
	body2 := map[string]any{}
	openAISamplingFields(body2, inference.Request{})
	if _, ok := body2["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort must be omitted when unset: %v", body2)
	}
}
