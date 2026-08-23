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
	"testing"
	"time"
)

func TestOllamaClientCompletesNonStreamingChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path = %s, want /api/chat", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode returned %v", err)
		}
		if body["stream"] != false {
			t.Fatalf("stream = %#v, want false", body["stream"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]any{"role": "assistant", "content": "ollama answer"},
			"prompt_eval_count": 3,
			"eval_count":        4,
		})
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if resp.Text != "ollama answer" {
		t.Fatalf("Text = %q, want ollama answer", resp.Text)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 || resp.Usage.TotalTokens != 7 {
		t.Fatalf("Usage = %#v", resp.Usage)
	}
}

func TestOllamaClientCompletesNonStreamingChatWithSpeedMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":              map[string]any{"role": "assistant", "content": "ollama answer"},
			"prompt_eval_count":    10,
			"prompt_eval_duration": 500000000,
			"eval_count":           20,
			"eval_duration":        1000000000,
		})
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	resp, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 5 * time.Second}, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if diff := resp.Usage.PromptPerSecond - 20; diff > 0.001 || diff < -0.001 {
		t.Fatalf("Usage.PromptPerSecond = %v, want ~20", resp.Usage.PromptPerSecond)
	}
	if diff := resp.Usage.TokensPerSecond - 20; diff > 0.001 || diff < -0.001 {
		t.Fatalf("Usage.TokensPerSecond = %v, want ~20", resp.Usage.TokensPerSecond)
	}
}

func TestOllamaClientListModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/tags" {
			t.Fatalf("path = %s, want /api/tags", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "n1"}},
		})
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	models, err := client.ListModels(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ListModels returned %v", err)
	}
	if len(models) != 1 || models[0] != "n1" {
		t.Fatalf("models = %#v, want [n1]", models)
	}
}

func TestOllamaClientListModelsReturnsUnavailableForNon2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	_, err := client.ListModels(context.Background(), routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ListModels error = %v, want ErrUnavailable", err)
	}
}

func TestOllamaCompleteStreamParsesNDJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path = %s, want /api/chat", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"message":{"content":"Hel"},"done":false}`+"\n")
		_, _ = io.WriteString(w, `{"message":{"content":"lo"},"done":false}`+"\n")
		_, _ = io.WriteString(w, `{"done":true,"prompt_eval_count":3,"eval_count":2}`+"\n")
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	var text string
	var completed *inference.StreamEvent
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 5 * time.Second},
		inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}},
		func(ev inference.StreamEvent) error {
			switch ev.Type {
			case inference.StreamEventTextDelta:
				text += ev.Text
			case inference.StreamEventCompleted:
				c := ev
				completed = &c
			}
			return nil
		})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if text != "Hello" {
		t.Fatalf("text = %q, want Hello", text)
	}
	if completed == nil {
		t.Fatal("no StreamEventCompleted emitted")
	}
	if completed.Usage == nil {
		t.Fatal("completed event missing usage")
	}
	if completed.Usage.InputTokens != 3 || completed.Usage.OutputTokens != 2 || completed.Usage.TotalTokens != 5 {
		t.Fatalf("Usage = %#v, want In 3 Out 2 Total 5", completed.Usage)
	}
}

func TestOllamaCompleteStreamParsesNDJSONWithSpeedMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"message":{"content":"Hello"},"done":false}`+"\n")
		_, _ = io.WriteString(w, `{"done":true,"prompt_eval_count":10,"prompt_eval_duration":500000000,"eval_count":20,"eval_duration":1000000000}`+"\n")
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	var completed *inference.StreamEvent
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 5 * time.Second},
		inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}},
		func(ev inference.StreamEvent) error {
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
	if diff := completed.Usage.PromptPerSecond - 20; diff > 0.001 || diff < -0.001 {
		t.Fatalf("Usage.PromptPerSecond = %v, want ~20", completed.Usage.PromptPerSecond)
	}
	if diff := completed.Usage.TokensPerSecond - 20; diff > 0.001 || diff < -0.001 {
		t.Fatalf("Usage.TokensPerSecond = %v, want ~20", completed.Usage.TokensPerSecond)
	}
}

func TestOllamaCompleteStreamRequestBody(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode returned %v", err)
		}
		bodies <- body
		_, _ = io.WriteString(w, `{"done":true}`+"\n")
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	temperature := 0.5
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 5 * time.Second},
		inference.Request{
			Temperature: &temperature,
			MaxTokens:   64,
			Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{
				{Type: inference.ContentText, Text: "describe"},
				{Type: inference.ContentImage, ImageURL: "data:image/png;base64,AAAA"},
			}}},
		},
		func(ev inference.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	body := <-bodies
	if body["stream"] != true {
		t.Fatalf("stream = %#v, want true", body["stream"])
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want single message", body["messages"])
	}
	msg, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}
	if msg["content"] != "describe" {
		t.Fatalf("content = %#v, want describe", msg["content"])
	}
	images, ok := msg["images"].([]any)
	if !ok || len(images) != 1 || images[0] != "AAAA" {
		t.Fatalf("images = %#v, want [AAAA]", msg["images"])
	}
	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("options = %#v, want object", body["options"])
	}
	if options["temperature"] != 0.5 {
		t.Fatalf("options.temperature = %#v, want 0.5", options["temperature"])
	}
	if options["num_predict"] != float64(64) {
		t.Fatalf("options.num_predict = %#v, want 64", options["num_predict"])
	}
}

func TestOllamaCompleteStreamSurfacesErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"error":"boom"}`+"\n")
	}))
	defer upstream.Close()
	client := NewOllamaClient(http.DefaultClient)

	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, Timeout: 5 * time.Second},
		inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}},
		func(ev inference.StreamEvent) error { return nil })

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CompleteStream error = %v, want ErrUnavailable", err)
	}
}

func TestOllamaCompleteStreamNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := NewOllamaClient(server.Client())
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	err := c.CompleteStream(context.Background(), routing.Target{Endpoint: server.URL}, req, func(inference.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestOllamaCompleteStreamIgnoresTotalTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			time.Sleep(30 * time.Millisecond)
			fmt.Fprintf(w, "{\"message\":{\"content\":\"x%d\"},\"done\":false}\n", i)
			f.Flush()
		}
		fmt.Fprint(w, "{\"message\":{\"content\":\"\"},\"done\":true,\"prompt_eval_count\":1,\"eval_count\":3}\n")
		f.Flush()
	}))
	defer upstream.Close()

	client := NewOllamaClient(upstream.Client())
	var deltas int
	err := client.CompleteStream(context.Background(),
		routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen2.5-coder", Timeout: 10 * time.Millisecond},
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
