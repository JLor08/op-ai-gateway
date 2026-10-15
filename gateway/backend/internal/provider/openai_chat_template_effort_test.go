// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func effortRequest(effort string, viaKwargs bool) inference.Request {
	return inference.Request{
		ReasoningEffort:                  effort,
		ReasoningEffortViaTemplateKwargs: viaKwargs,
		Messages:                         []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}},
	}
}

// assertEffortFields checks the effort-related fields of an upstream chat body:
// the top-level reasoning_effort is present exactly when want != "", and
// chat_template_kwargs.reasoning_effort exactly when wantKwargs.
func assertEffortFields(t *testing.T, body map[string]any, want string, wantKwargs bool) {
	t.Helper()
	if want == "" {
		if _, ok := body["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort must be omitted when unset: %v", body)
		}
	} else if body["reasoning_effort"] != want {
		t.Fatalf("top-level reasoning_effort = %v, want %q (it is kept even when the kwarg is added)", body["reasoning_effort"], want)
	}
	kwargs, present := body["chat_template_kwargs"]
	if !wantKwargs {
		if present {
			t.Fatalf("chat_template_kwargs must be absent without the directive: %v", body)
		}
		return
	}
	obj, ok := kwargs.(map[string]any)
	if !ok || len(obj) != 1 || obj["reasoning_effort"] != want {
		t.Fatalf("chat_template_kwargs = %#v, want exactly {reasoning_effort: %q}", kwargs, want)
	}
}

func TestOpenAIChatTemplateReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		req  inference.Request
		seed map[string]any
		want map[string]any
	}{
		{"directive + effort adds the kwarg", effortRequest("low", true), map[string]any{}, map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "low"}}},
		{"no directive adds nothing", effortRequest("low", false), map[string]any{}, map[string]any{}},
		{"directive without effort adds nothing", effortRequest("", true), map[string]any{}, map[string]any{}},
		{"neither adds nothing", effortRequest("", false), map[string]any{}, map[string]any{}},
		{
			"existing kwargs object is merged into",
			effortRequest("high", true),
			map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}},
			map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true, "reasoning_effort": "high"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.seed
			openAIChatTemplateReasoningEffort(body, tc.req)
			got, _ := json.Marshal(body)
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Fatalf("body = %s, want %s", got, want)
			}
		})
	}
}

// TestOpenAICompatibleCompleteEmitsChatTemplateReasoningEffort proves the
// NON-STREAM chat body carries chat_template_kwargs.reasoning_effort IN ADDITION
// to the top-level reasoning_effort when the gateway directive is set, and the
// top-level field alone otherwise (a direct OpenAI chat request).
func TestOpenAICompatibleCompleteEmitsChatTemplateReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        inference.Request
		wantEffort string
		wantKwargs bool
	}{
		{"directive + effort", effortRequest("low", true), "low", true},
		{"effort without directive is top-level only", effortRequest("low", false), "low", false},
		{"no effort", effortRequest("", false), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			if _, err := client.Complete(context.Background(), routing.Target{Endpoint: upstream.URL, ProviderModel: "qwen", Timeout: 5 * time.Second}, tc.req); err != nil {
				t.Fatalf("Complete returned %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(<-bodyCh, &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			assertEffortFields(t, body, tc.wantEffort, tc.wantKwargs)
		})
	}
}

// TestStreamRequestBodyEmitsChatTemplateReasoningEffort is the streaming twin:
// streamRequestBody is the one builder CompleteStream (and its live-progress
// retry) renders from, so asserting on it covers every streamed attempt.
func TestStreamRequestBodyEmitsChatTemplateReasoningEffort(t *testing.T) {
	target := routing.Target{ProviderModel: "qwen"}
	for _, tc := range []struct {
		name       string
		req        inference.Request
		wantEffort string
		wantKwargs bool
	}{
		{"directive + effort", effortRequest("low", true), "low", true},
		{"effort without directive is top-level only", effortRequest("low", false), "low", false},
		{"no effort", effortRequest("", false), "", false},
	} {
		for _, withLiveProgress := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				raw, err := streamRequestBody(target, tc.req, withLiveProgress)
				if err != nil {
					t.Fatalf("streamRequestBody returned %v", err)
				}
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatalf("unmarshal body: %v", err)
				}
				assertEffortFields(t, body, tc.wantEffort, tc.wantKwargs)
			})
		}
	}
}
