// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"sync"
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

// llama.cpp with timings_per_token attaches its timings object to PARTIAL chunks,
// which carry no usage object at all. Before this change that branch was dead.
func TestCompleteStreamProgressFromChunkTimingsWithoutUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hallo"}}],"timings":{"predicted_n":7,"predicted_per_second":42.5}}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var got *inference.StreamProgress
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderLlamaCPP, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
		if ev.Type == inference.StreamEventTextDelta {
			got = ev.Progress
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if got == nil {
		t.Fatal("no progress on the text delta: per-chunk timings were dropped")
	}
	if got.OutputTokens != 7 || got.TokensPerSecond != 42.5 {
		t.Fatalf("progress = %+v, want {7 42.5}", *got)
	}
}

// vLLM's continuous usage stats put an exact running completion-token count on
// every chunk but report no rate; the consumer derives a rate from the count.
func TestCompleteStreamProgressFromContinuousUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var got *inference.StreamProgress
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
		if ev.Type == inference.StreamEventTextDelta {
			got = ev.Progress
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if got == nil {
		t.Fatal("no progress on the text delta: continuous usage was dropped")
	}
	if got.OutputTokens != 3 || got.TokensPerSecond != 0 {
		t.Fatalf("progress = %+v, want {3 0}", *got)
	}
}

// The two live-progress request parameters must reach only the allow-listed
// upstreams. LiteLLM forwards unrecognized body keys downstream and fails the
// whole request on them, so it must see stream_options with ONLY include_usage.
func TestCompleteStreamSendsLiveProgressParamsOnlyForAllowedUpstreams(t *testing.T) {
	newUpstream := func(bodyCh chan []byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			bodyCh <- raw
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
	}
	req := inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	client := NewOpenAICompatibleClient(http.DefaultClient)

	t.Run("llama_cpp", func(t *testing.T) {
		bodyCh := make(chan []byte, 1)
		upstream := newUpstream(bodyCh)
		defer upstream.Close()
		if err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderLlamaCPP, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, req, func(inference.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("CompleteStream returned %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(<-bodyCh, &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if body["timings_per_token"] != true {
			t.Fatalf("timings_per_token = %#v, want true", body["timings_per_token"])
		}
		opts, ok := body["stream_options"].(map[string]any)
		if !ok || opts["include_usage"] != true || opts["continuous_usage_stats"] != true {
			t.Fatalf("stream_options = %#v, want include_usage and continuous_usage_stats true", body["stream_options"])
		}
	})

	t.Run("litellm", func(t *testing.T) {
		bodyCh := make(chan []byte, 1)
		upstream := newUpstream(bodyCh)
		defer upstream.Close()
		if err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderLiteLLM, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, req, func(inference.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("CompleteStream returned %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(<-bodyCh, &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if _, ok := body["timings_per_token"]; ok {
			t.Fatalf("timings_per_token must be absent for litellm, got %#v", body["timings_per_token"])
		}
		opts, ok := body["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options missing: %#v", body["stream_options"])
		}
		if len(opts) != 1 || opts["include_usage"] != true {
			t.Fatalf("stream_options = %#v, want exactly {include_usage: true}", opts)
		}
	})
}

// TestCompleteStreamTruncatedVLLMStreamKeepsLastPartialUsage pins the
// consequence of continuous_usage_stats: chunk.Usage is now non-nil on EVERY
// vLLM chunk, so a connection that dies mid-stream (no final usage chunk, no
// [DONE]) still returns the existing truncation error, but the progress
// observed via the already-emitted text deltas carries the last partial figure
// rather than nothing.
func TestCompleteStreamTruncatedVLLMStreamKeepsLastPartialUsage(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
			`data: {"choices":[{"delta":{"content":" there"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
			f.Flush()
		}
		// Simulate the upstream dying mid-stream: no final usage chunk, no [DONE],
		// connection severed abruptly rather than closed cleanly.
		upstream.CloseClientConnections()
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var lastProgress *inference.StreamProgress
	var sawCompleted bool
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
		switch ev.Type {
		case inference.StreamEventTextDelta:
			lastProgress = ev.Progress
		case inference.StreamEventCompleted:
			sawCompleted = true
		}
		return nil
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want the existing truncation error (ErrUnavailable)", err)
	}
	if sawCompleted {
		t.Fatal("StreamEventCompleted was emitted despite the truncated connection")
	}
	if lastProgress == nil {
		t.Fatal("no progress observed before truncation")
	}
	if lastProgress.OutputTokens != 5 {
		t.Fatalf("last observed OutputTokens = %d, want 5 (the last partial continuous-usage figure, not zero)", lastProgress.OutputTokens)
	}
}

// TestCompleteStreamTerminalUsageUnchangedWithContinuousUsage asserts the
// complete-stream case is unchanged: intermediate chunks carry partial usage
// without prompt_tokens_details, the final chunk carries the complete usage +
// timings, and the terminal event's Usage must equal the FINAL chunk's values,
// including CachedTokens.
func TestCompleteStreamTerminalUsageUnchangedWithContinuousUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
			`data: {"choices":[{"delta":{"content":" there"}}],"usage":{"prompt_tokens":10,"completion_tokens":8}}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":12,"total_tokens":22,"prompt_tokens_details":{"cached_tokens":4}},"timings":{"prompt_per_second":123.4,"predicted_per_second":56.7}}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(http.DefaultClient)

	var completed *inference.StreamEvent
	err := client.CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM, ProviderModel: "qwen-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}, func(ev inference.StreamEvent) error {
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
	if completed.Usage.OutputTokens != 12 || completed.Usage.TotalTokens != 22 {
		t.Fatalf("Usage = %#v, want the FINAL chunk's completion_tokens=12/total=22, not an intermediate partial", completed.Usage)
	}
	if completed.Usage.CachedTokens != 4 {
		t.Fatalf("Usage.CachedTokens = %v, want 4 (from the final chunk, not lost to an earlier partial chunk without details)", completed.Usage.CachedTokens)
	}
	if completed.Usage.TokensPerSecond != 56.7 {
		t.Fatalf("Usage.TokensPerSecond = %v, want 56.7 (from the final chunk's timings)", completed.Usage.TokensPerSecond)
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

// liveProgressUpstream is a test upstream for the live-progress retry: it records
// every request body it receives and answers each one from the supplied handler,
// so a test can assert both what was sent and how many round trips happened.
type liveProgressUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (u *liveProgressUpstream) serve(reply func(w http.ResponseWriter, attempt int, body []byte)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, raw)
		attempt := len(u.bodies)
		u.mu.Unlock()
		reply(w, attempt, raw)
	}))
}

func (u *liveProgressUpstream) recorded() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// hasLiveProgressParams reports whether a recorded request body carries the two
// live-progress parameters, and (for the negative case) that stream_options was
// restored to EXACTLY {"include_usage": true} rather than dropped -- include_usage
// is what makes the terminal usage chunk arrive at all.
func hasLiveProgressParams(t *testing.T, raw []byte) bool {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal recorded body: %v", err)
	}
	opts, ok := body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true in every body", body["stream_options"])
	}
	_, timings := body["timings_per_token"]
	if timings != (opts["continuous_usage_stats"] == true) {
		t.Fatalf("the two live-progress parameters must move together, got %#v", body)
	}
	if !timings && len(opts) != 1 {
		t.Fatalf("stream_options = %#v, want exactly {include_usage: true} without live progress", opts)
	}
	return timings
}

// TestCompleteStreamRetriesWithoutLiveProgressParamsOnSchemaRejection is the
// heart of the fix: an upstream that refuses the two advisory parameters with a
// 400 must not cost the request. The rejection is known BEFORE the first emit, so
// re-issuing the same request without them is invisible to the client -- the
// stream proceeds, minus one advisory number.
func TestCompleteStreamRetriesWithoutLiveProgressParamsOnSchemaRejection(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			up := &liveProgressUpstream{}
			server := up.serve(func(w http.ResponseWriter, _ int, body []byte) {
				if bytes.Contains(body, []byte("timings_per_token")) {
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			})
			defer server.Close()

			client := NewOpenAICompatibleClient(server.Client())
			var text string
			var usage *inference.Usage
			err := client.CompleteStream(context.Background(),
				routing.Target{Endpoint: server.URL, Provider: routing.ProviderLlamaCPP, RouteID: "map_live", ProviderModel: "m", Timeout: 5 * time.Second},
				streamTestRequest(),
				func(ev inference.StreamEvent) error {
					text += ev.Text
					if ev.Usage != nil {
						usage = ev.Usage
					}
					return nil
				})
			if err != nil {
				t.Fatalf("CompleteStream returned %v, want nil (a rejection of an advisory parameter must be a non-event)", err)
			}
			if text != "hi" {
				t.Fatalf("emitted text = %q, want %q", text, "hi")
			}
			if usage == nil || usage.OutputTokens != 2 {
				t.Fatalf("terminal usage = %+v, want completion_tokens 2 (include_usage must survive the retry)", usage)
			}
			bodies := up.recorded()
			if len(bodies) != 2 {
				t.Fatalf("upstream requests = %d, want 2 (one with the parameters, one without)", len(bodies))
			}
			if !hasLiveProgressParams(t, bodies[0]) {
				t.Fatal("first attempt did not carry the live-progress parameters")
			}
			if hasLiveProgressParams(t, bodies[1]) {
				t.Fatal("retry still carried the live-progress parameters")
			}
		})
	}
}

// TestCompleteStreamDoesNotRetryOn503 pins guard (2)'s exclusion: 503 means
// ErrUpstreamStarting, which the load runner consumes as "still warming up".
// Retrying it would both destroy that signal and hammer an upstream that is not
// ready, so a 503 must stay exactly one round trip and exactly that error.
func TestCompleteStreamDoesNotRetryOn503(t *testing.T) {
	up := &liveProgressUpstream{}
	server := up.serve(func(w http.ResponseWriter, _ int, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer server.Close()

	client := NewOpenAICompatibleClient(server.Client())
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: server.URL, Provider: routing.ProviderLlamaCPP, RouteID: "map_live", ProviderModel: "m", Timeout: 5 * time.Second},
		streamTestRequest(), func(inference.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUpstreamStarting) {
		t.Fatalf("err = %v, want ErrUpstreamStarting", err)
	}
	if n := len(up.recorded()); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (503 is not a schema rejection)", n)
	}
}

// TestCompleteStreamDoesNotRetryWhenTheParametersWereNotSent pins guard (1): a
// request that never carried the parameters has nothing to retry WITHOUT, so an
// ordinary 400 keeps its meaning and costs one round trip.
func TestCompleteStreamDoesNotRetryWhenTheParametersWereNotSent(t *testing.T) {
	up := &liveProgressUpstream{}
	server := up.serve(func(w http.ResponseWriter, _ int, _ []byte) {
		w.WriteHeader(http.StatusBadRequest)
	})
	defer server.Close()

	client := NewOpenAICompatibleClient(server.Client())
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: server.URL, Provider: routing.ProviderLiteLLM, RouteID: "map_litellm", ProviderModel: "m", Timeout: 5 * time.Second},
		streamTestRequest(), func(inference.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if n := len(up.recorded()); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (nothing to retry without)", n)
	}
}

// TestCompleteStreamRetriesOnInStreamErrorBeforeFirstToken covers the case a
// 400 does not: some OpenAI-compatible proxies (LiteLLM, OpenRouter -- reachable
// behind a llama_swap `peer`) answer 200 and then report the refused body as an
// SSE error EVENT. Nothing has been emitted at that point, so the same retry
// applies.
func TestCompleteStreamRetriesOnInStreamErrorBeforeFirstToken(t *testing.T) {
	up := &liveProgressUpstream{}
	server := up.serve(func(w http.ResponseWriter, _ int, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		if bytes.Contains(body, []byte("timings_per_token")) {
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"Unrecognized request argument supplied: timings_per_token\"}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := NewOpenAICompatibleClient(server.Client())
	var text string
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: server.URL, Provider: routing.ProviderLlamaSwap, RouteID: "map_peer", ProviderModel: "m", Timeout: 5 * time.Second},
		streamTestRequest(),
		func(ev inference.StreamEvent) error { text += ev.Text; return nil })
	if err != nil {
		t.Fatalf("CompleteStream returned %v, want nil", err)
	}
	if text != "ok" {
		t.Fatalf("emitted text = %q, want %q", text, "ok")
	}
	if n := len(up.recorded()); n != 2 {
		t.Fatalf("upstream requests = %d, want 2", n)
	}
}

// TestCompleteStreamDoesNotRetryAfterTheFirstEmit pins guard (3), the invariant
// that makes the retry invisible. Here the error frame arrives AFTER a text delta
// the client has already seen, so re-issuing the request would duplicate served
// content: the error must be surfaced instead, in one round trip.
func TestCompleteStreamDoesNotRetryAfterTheFirstEmit(t *testing.T) {
	up := &liveProgressUpstream{}
	server := up.serve(func(w http.ResponseWriter, _ int, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"boom\"}}\n\n")
	})
	defer server.Close()

	client := NewOpenAICompatibleClient(server.Client())
	var text string
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: server.URL, Provider: routing.ProviderLlamaCPP, RouteID: "map_live", ProviderModel: "m", Timeout: 5 * time.Second},
		streamTestRequest(),
		func(ev inference.StreamEvent) error { text += ev.Text; return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable (an error after the first emit must be reported, not retried)", err)
	}
	if text != "Hel" {
		t.Fatalf("emitted text = %q, want %q", text, "Hel")
	}
	if n := len(up.recorded()); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (no retry once the client has seen a token)", n)
	}
}

// TestCompleteStreamMemoizesTheRejectionPerMapping proves the negative-only memo
// does its job: a genuinely incompatible upstream costs ONE wasted round trip for
// the mapping, not one per request. The second request skips the parameters
// outright; a different mapping is unaffected (no positive verdict is cached
// either way, so an empty memo still means "send them").
func TestCompleteStreamMemoizesTheRejectionPerMapping(t *testing.T) {
	up := &liveProgressUpstream{}
	server := up.serve(func(w http.ResponseWriter, _ int, body []byte) {
		if bytes.Contains(body, []byte("timings_per_token")) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := NewOpenAICompatibleClient(server.Client())
	target := routing.Target{Endpoint: server.URL, Provider: routing.ProviderServerAgent, RouteID: "map_custom", ProviderModel: "m", Timeout: 5 * time.Second}
	for i := range 3 {
		if err := client.CompleteStream(context.Background(), target, streamTestRequest(), func(inference.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("CompleteStream #%d returned %v", i+1, err)
		}
	}
	bodies := up.recorded()
	// Request 1: rejected attempt + retry. Requests 2 and 3: one attempt each,
	// already without the parameters.
	if len(bodies) != 4 {
		t.Fatalf("upstream requests = %d, want 4 (2 for the first call, 1 each afterwards)", len(bodies))
	}
	if !hasLiveProgressParams(t, bodies[0]) {
		t.Fatal("first attempt did not carry the parameters")
	}
	for i, raw := range bodies[1:] {
		if hasLiveProgressParams(t, raw) {
			t.Fatalf("request %d carried the parameters again after the rejection was memoized", i+2)
		}
	}

	// A different mapping is not covered by that memo entry.
	other := target
	other.RouteID = "map_other"
	if err := client.CompleteStream(context.Background(), other, streamTestRequest(), func(inference.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("CompleteStream for the other mapping returned %v", err)
	}
	bodies = up.recorded()
	if !hasLiveProgressParams(t, bodies[4]) {
		t.Fatal("a different mapping did not get the parameters (the memo must be per mapping)")
	}
}

// streamTestRequest is the minimal one-message request the live-progress retry
// tests above stream.
func streamTestRequest() inference.Request {
	return inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
}
