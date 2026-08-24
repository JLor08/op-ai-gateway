// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenAIChatCompletionsRequiresBearerToken(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGatewayUseEndpointsRejectAgentOnlyToken(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "openai chat", method: http.MethodPost, path: "/openai/v1/chat/completions", body: `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`},
		{name: "openai responses", method: http.MethodPost, path: "/openai/v1/responses", body: `{"model":"qwen-coder","input":"hello"}`},
		{name: "anthropic messages", method: http.MethodPost, path: "/anthropic/v1/messages", body: `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`},
		{name: "anthropic count tokens", method: http.MethodPost, path: "/anthropic/v1/messages/count_tokens", body: `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`},
		{name: "openai models", method: http.MethodGet, path: "/openai/v1/models"},
		{name: "anthropic models", method: http.MethodGet, path: "/anthropic/v1/models"},
		{name: "usage", method: http.MethodGet, path: "/api/usage"},
		{name: "portal me", method: http.MethodGet, path: "/api/portal/me"},
		{name: "portal tokens", method: http.MethodGet, path: "/api/portal/tokens"},
		{name: "portal usage", method: http.MethodGet, path: "/api/portal/usage"},
		{name: "portal dashboard", method: http.MethodGet, path: "/api/portal/dashboard"},
		{name: "portal models", method: http.MethodGet, path: "/api/portal/models"},
		{name: "portal servers", method: http.MethodGet, path: "/api/portal/servers"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewTestServerWithTokenScopes([]string{"agent:report"})
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if body.Error.Code != "auth.insufficient_scope" {
				t.Fatalf("error code = %q", body.Error.Code)
			}
		})
	}
}

func TestOpenAIChatCompletionsReturnsMockResponseAndRecordsUsage(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Choices[0].Message.Content != "Mock response for qwen-coder: hello" {
		t.Fatalf("content = %q", body.Choices[0].Message.Content)
	}
	if len(srv.Usage.All()) != 1 {
		t.Fatalf("usage events = %d, want 1", len(srv.Usage.All()))
	}
}

func TestOpenAIChatUsesRouteResolverAndRecordsRouteID(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].RouteID != "route_mock_qwen" {
		t.Fatalf("RouteID = %q, want route_mock_qwen", events[0].RouteID)
	}
	if events[0].Provider != "mock" || events[0].Host != "mock-host-comp" {
		t.Fatalf("provider/host = %s/%s", events[0].Provider, events[0].Host)
	}
}

func TestOpenAIChatCompletionsRecordsEnrichedUsageMetrics(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	event := events[0]
	if event.ReqPath != "/v1/chat/completions" {
		t.Fatalf("ReqPath = %q, want /v1/chat/completions", event.ReqPath)
	}
	if event.HTTPStatus != http.StatusOK {
		t.Fatalf("HTTPStatus = %d, want 200", event.HTTPStatus)
	}
	if event.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", event.ContentType)
	}
	if event.Stream {
		t.Fatalf("Stream = true, want false")
	}
	if event.ProviderModel != "qwen-coder" {
		t.Fatalf("ProviderModel = %q, want qwen-coder", event.ProviderModel)
	}
	if event.TokenName != "Dev Token" {
		t.Fatalf("TokenName = %q, want Dev Token", event.TokenName)
	}
	if event.ServerName != "Mock Completion" {
		t.Fatalf("ServerName = %q, want Mock Completion", event.ServerName)
	}
}

func TestOpenAIChatStreamRecordsEventStreamContentType(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	event := events[0]
	if !event.Stream {
		t.Fatalf("Stream = false, want true")
	}
	if event.ContentType != "text/event-stream" {
		t.Fatalf("ContentType = %q, want text/event-stream", event.ContentType)
	}
	if event.HTTPStatus != http.StatusOK {
		t.Fatalf("HTTPStatus = %d, want 200", event.HTTPStatus)
	}
}

func TestOpenAIChatStreamResolveErrorRecordsJSONContentType(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such-model","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	event := events[0]
	// The request asked for streaming, but the pre-stream resolve failure wrote a
	// JSON error body — content_type must reflect what was actually sent.
	if !event.Stream {
		t.Fatalf("Stream = false, want true (request asked to stream)")
	}
	if event.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", event.ContentType)
	}
}

func TestOpenAIChatReturnsNoModelRoute(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"missing-model","messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "routing.no_model_route" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestInferenceUsageEventsUseUniqueIDs(t *testing.T) {
	srv := NewTestServer()

	first := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello one"}]}`)
	firstRec := httptest.NewRecorder()
	srv.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", firstRec.Code, firstRec.Body.String())
	}

	second := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello two"}]}`)
	secondRec := httptest.NewRecorder()
	srv.ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", secondRec.Code, secondRec.Body.String())
	}

	events := srv.Usage.All()
	if len(events) != 2 {
		t.Fatalf("usage events = %d, want 2", len(events))
	}
	if events[0].ID == "" || events[1].ID == "" {
		t.Fatalf("usage IDs must be non-empty: %#v", events)
	}
	if events[0].ID == events[1].ID {
		t.Fatalf("usage IDs are equal: %q", events[0].ID)
	}
}

func TestAnthropicMessagesReturnsMockResponse(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Content[0].Text != "Mock response for qwen-coder: hello" {
		t.Fatalf("content = %q", body.Content[0].Text)
	}
}

func TestOpenAIChatCompletionsAliasWorks(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello alias"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Choices[0].Message.Content != "Mock response for qwen-coder: hello alias" {
		t.Fatalf("content = %q", body.Choices[0].Message.Content)
	}
}

func TestOpenAIResponsesReturnsOutputTextAndRecordsUsage(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/responses", `{"model":"qwen-coder","input":"hello responses"}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Role    string `json:"role"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []any  `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.OutputText != "Mock response for qwen-coder: hello responses" {
		t.Fatalf("output_text = %q", body.OutputText)
	}
	if len(body.Output) != 1 {
		t.Fatalf("output items = %d, want 1", len(body.Output))
	}
	if body.Output[0].Type != "message" {
		t.Fatalf("output type = %q, want message", body.Output[0].Type)
	}
	if body.Output[0].Status != "completed" {
		t.Fatalf("output status = %q, want completed", body.Output[0].Status)
	}
	if body.Output[0].Role != "assistant" {
		t.Fatalf("output role = %q, want assistant", body.Output[0].Role)
	}
	if len(body.Output[0].Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(body.Output[0].Content))
	}
	if body.Output[0].Content[0].Type != "output_text" {
		t.Fatalf("content type = %q, want output_text", body.Output[0].Content[0].Type)
	}
	if body.Output[0].Content[0].Text != "Mock response for qwen-coder: hello responses" {
		t.Fatalf("content text = %q", body.Output[0].Content[0].Text)
	}
	if body.Output[0].Content[0].Annotations == nil {
		t.Fatalf("annotations = nil, want empty array")
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].UserID != "usr_dev" || events[0].TokenID != "tok_dev" {
		t.Fatalf("usage identity = %s/%s, want usr_dev/tok_dev", events[0].UserID, events[0].TokenID)
	}
	if events[0].APIFlavor != "openai_responses" {
		t.Fatalf("APIFlavor = %q, want openai_responses", events[0].APIFlavor)
	}
	if events[0].Provider != "mock" || events[0].Host != "mock-host-comp" {
		t.Fatalf("usage provider/host = %s/%s, want mock/mock-host-comp", events[0].Provider, events[0].Host)
	}
	if events[0].Status != "success" {
		t.Fatalf("Status = %q, want success", events[0].Status)
	}
	if events[0].TotalTokens == 0 {
		t.Fatalf("TotalTokens = 0, want recorded token count")
	}
	if events[0].CreatedAt.IsZero() {
		t.Fatalf("CreatedAt is zero")
	}
}

func TestOpenAIResponsesAliasWorks(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/v1/responses", `{"model":"qwen-coder","input":"hello responses alias"}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OutputText string `json:"output_text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.OutputText != "Mock response for qwen-coder: hello responses alias" {
		t.Fatalf("output_text = %q", body.OutputText)
	}
}

func TestRequestValidationPreservesInferenceErrorCode(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "openai.content_required" {
		t.Fatalf("error code = %q, want openai.content_required", body.Error.Code)
	}
	if len(srv.Usage.All()) != 0 {
		t.Fatalf("usage events = %d, want 0", len(srv.Usage.All()))
	}
}

func TestLargeControlPlaneBodyReturnsStableAPIError(t *testing.T) {
	srv := NewTestServer()
	largeContent := strings.Repeat("x", int(maxJSONBodyBytes)+1)
	req := newJSONRequest(http.MethodPost, "/api/auth/login", `{"email":"`+largeContent+`","password":"x"}`)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "request.body_too_large" {
		t.Fatalf("error code = %q, want request.body_too_large", body.Error.Code)
	}
}

func TestInferenceEndpointAcceptsLargeBody(t *testing.T) {
	srv := NewTestServer()
	largeContent := strings.Repeat("x", int(maxJSONBodyBytes)+1) // > 1 MiB
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"qwen-coder","messages":[{"role":"user","content":"`+largeContent+`"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("inference endpoint rejected a large body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// sseDataChunks returns the decoded JSON of every "data: {...}" SSE line in
// body (skipping the terminal "data: [DONE]" marker).
func sseDataChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("data line is not JSON: %q (%v)", payload, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// chunkDelta returns the choices[0].delta object of an SSE chunk, or nil.
func chunkDelta(chunk map[string]any) map[string]any {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

func TestOpenAIChatStreamEmitsToolCalls(t *testing.T) {
	srv := newStreamTestServerWithProvider(toolStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ls"}],"stream":true,"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object"}}}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Reconstruct the tool call by accumulating the tool_calls delta chunks (as an
	// OpenAI SDK client would), and read the terminal finish_reason.
	var gotID, gotName, gotArgs, finish string
	for _, c := range sseDataChunks(t, rec.Body.String()) {
		if delta := chunkDelta(c); delta != nil {
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, item := range tcs {
					tc, _ := item.(map[string]any)
					if idv, ok := tc["id"].(string); ok && idv != "" {
						gotID = idv
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							gotName = n
						}
						if a, ok := fn["arguments"].(string); ok {
							gotArgs += a
						}
					}
				}
			}
		}
		if choices, ok := c["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					finish = fr
				}
			}
		}
	}
	if gotID != "call_1" || gotName != "shell" || gotArgs != `{"cmd":"ls"}` {
		t.Fatalf("reconstructed tool call = id %q name %q args %q, want call_1/shell/{\"cmd\":\"ls\"}", gotID, gotName, gotArgs)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
}

func TestOpenAIChatStreamsSSE(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal data: [DONE], body = %s", body)
	}
	sawContent := false
	for _, chunk := range sseDataChunks(t, body) {
		delta := chunkDelta(chunk)
		if delta == nil {
			continue
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			sawContent = true
		}
	}
	if !sawContent {
		t.Fatalf("no delta with content in stream body = %s", body)
	}

	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events for usr_dev = %d, want 1", len(events))
	}
	if events[0].TotalTokens == 0 {
		t.Fatalf("TotalTokens = 0, want non-zero for streamed request")
	}
}

func TestOpenAIChatStreamAuthMatrix(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	now := time.Now().UTC()
	// A bearer token (no cookie) exercises the external-client path.
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("seed bearer token: %v", err)
	}
	// A login user backs the session-cookie path.
	seedLoginUser(t, dir, "usr_login", "login@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "login@example.test", "password-1")

	const body = `{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`

	// (a) bearer + stream works.
	bearer := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	bearer.Header.Set("Authorization", "Bearer dev-secret")
	bearerRec := httptest.NewRecorder()
	srv.ServeHTTP(bearerRec, bearer)
	if bearerRec.Code != http.StatusOK {
		t.Fatalf("bearer stream status = %d, body = %s", bearerRec.Code, bearerRec.Body.String())
	}
	if ct := bearerRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("bearer content-type = %q, want text/event-stream", ct)
	}

	// (b) valid session cookie + CSRF header works.
	cookieReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	cookieReq.AddCookie(cookie)
	cookieReq.Header.Set(csrfHeaderName, "1")
	cookieRec := httptest.NewRecorder()
	srv.ServeHTTP(cookieRec, cookieReq)
	if cookieRec.Code != http.StatusOK {
		t.Fatalf("cookie stream status = %d, body = %s", cookieRec.Code, cookieRec.Body.String())
	}
	if ct := cookieRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("cookie content-type = %q, want text/event-stream", ct)
	}

	// (c) same session request WITHOUT the CSRF header -> 403 auth.csrf_required.
	noCSRF := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	noCSRF.AddCookie(cookie)
	noCSRFRec := httptest.NewRecorder()
	srv.ServeHTTP(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("cookie without csrf status = %d, want 403, body = %s", noCSRFRec.Code, noCSRFRec.Body.String())
	}
	var csrfBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(noCSRFRec.Body.Bytes(), &csrfBody); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if csrfBody.Error.Code != "auth.csrf_required" {
		t.Fatalf("error code = %q, want auth.csrf_required", csrfBody.Error.Code)
	}

	// (d) no auth -> 401.
	anon := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	anonRec := httptest.NewRecorder()
	srv.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401, body = %s", anonRec.Code, anonRec.Body.String())
	}
}

func TestOpenAIChatStreamResolveErrorIsJSON(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such-model","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json (not SSE)", ct)
	}
}

// reasoningStreamer implements provider.Client + provider.StreamingClient and
// emits a single reasoning delta followed by a completed event.
type reasoningStreamer struct{}

func (reasoningStreamer) Complete(ctx context.Context, target routing.Target, req inference.Request) (provider.Response, error) {
	return provider.Response{Text: "th", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

func (reasoningStreamer) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: "th"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
}

func TestOpenAIChatStreamReasoning(t *testing.T) {
	srv := newStreamTestServerWithProvider(reasoningStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sawReasoning := false
	for _, chunk := range sseDataChunks(t, rec.Body.String()) {
		delta := chunkDelta(chunk)
		if delta == nil {
			continue
		}
		if r, ok := delta["reasoning_content"].(string); ok && r == "th" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatalf("no delta.reasoning_content == %q in body = %s", "th", rec.Body.String())
	}
}

// textStreamer emits a fixed list of text deltas then a completed event carrying
// usage — a deterministic StreamingClient for exercising the streaming edges.
type textStreamer struct {
	chunks []string
	usage  inference.Usage
}

func (textStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (s textStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for _, c := range s.chunks {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: c}); err != nil {
			return err
		}
	}
	u := s.usage
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

// reasoningAnswerStreamer emits reasoning deltas, then text deltas, then any tool
// calls — a reasoning model's turn — for exercising the Responses reasoning-item path.
type reasoningAnswerStreamer struct {
	reasoning []string
	text      []string
	toolCalls []inference.ToolCall
	usage     inference.Usage
}

func (reasoningAnswerStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (s reasoningAnswerStreamer) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for _, r := range s.reasoning {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: r}); err != nil {
			return err
		}
	}
	for _, c := range s.text {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: c}); err != nil {
			return err
		}
	}
	for i := range s.toolCalls {
		tc := s.toolCalls[i]
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &tc}); err != nil {
			return err
		}
	}
	u := s.usage
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

// sseEvent is one parsed "event: <type>\ndata: <json>" Responses SSE frame.
type sseEvent struct {
	Type string
	Data map[string]any
}

func responsesSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	var curType string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			curType = ev
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("bad SSE data json %q: %v", payload, err)
		}
		events = append(events, sseEvent{Type: curType, Data: data})
	}
	return events
}

// mustContainInOrder asserts want appears as an ordered subsequence of got.
func mustContainInOrder(t *testing.T, got []string, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("events %v do not contain %v in order (matched %d)", got, want, i)
	}
}

func TestOpenAIResponsesStreamEmitsResponsesEventSequence(t *testing.T) {
	srv := newStreamTestServerWithProvider(textStreamer{chunks: []string{"Hello", " world"}, usage: inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-coder","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	// A Responses stream terminates with response.completed + close, NOT [DONE].
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Responses stream must not contain a chat-style [DONE] sentinel: %s", body)
	}

	events := responsesSSEEvents(t, body)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body = %s", body)
	}
	var order []string
	for _, e := range events {
		order = append(order, e.Type)
	}
	mustContainInOrder(t, order, []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	})

	// Each event's data.type mirrors its event: line, and sequence_number is a
	// gap-free run starting at 0.
	for i, e := range events {
		if e.Data["type"] != e.Type {
			t.Fatalf("event[%d] data.type = %v, event: line = %q", i, e.Data["type"], e.Type)
		}
		seq, _ := e.Data["sequence_number"].(float64)
		if int(seq) != i {
			t.Fatalf("event[%d] sequence_number = %v, want %d", i, e.Data["sequence_number"], i)
		}
	}

	// Concatenated text deltas reconstruct the full answer.
	var deltas strings.Builder
	for _, e := range events {
		if e.Type == "response.output_text.delta" {
			d, _ := e.Data["delta"].(string)
			deltas.WriteString(d)
		}
	}
	if deltas.String() != "Hello world" {
		t.Fatalf("concatenated deltas = %q, want %q", deltas.String(), "Hello world")
	}

	// The terminal response.completed carries the full text + Responses-shaped usage.
	last := events[len(events)-1]
	if last.Type != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", last.Type)
	}
	resp, _ := last.Data["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("completed response status = %v, want completed", resp["status"])
	}
	usageObj, _ := resp["usage"].(map[string]any)
	if ot, _ := usageObj["output_tokens"].(float64); int(ot) != 3 {
		t.Fatalf("usage.output_tokens = %v, want 3", usageObj["output_tokens"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output len = %d, want 1; response = %v", len(output), resp)
	}
	msg, _ := output[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("message content len = %d, want 1", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "Hello world" {
		t.Fatalf("output text = %v, want %q", block["text"], "Hello world")
	}

	// Usage is recorded exactly once, marked streaming with the SSE content type.
	evs := srv.Usage.All()
	if len(evs) != 1 {
		t.Fatalf("usage events = %d, want 1", len(evs))
	}
	if !evs[0].Stream || evs[0].ContentType != "text/event-stream" {
		t.Fatalf("usage event = %+v, want Stream=true ContentType=text/event-stream", evs[0])
	}
}

func TestOpenAIResponsesStreamEmptyAnswerIsCoherent(t *testing.T) {
	// A provider that streams zero text deltas must still emit a coherent, closed
	// Responses sequence: the item opens, output_text.done carries "", and the
	// terminal completed response holds an empty-text message.
	srv := newStreamTestServerWithProvider(textStreamer{chunks: nil, usage: inference.Usage{InputTokens: 1, OutputTokens: 0, TotalTokens: 1}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-coder","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())
	var order []string
	for _, e := range events {
		order = append(order, e.Type)
	}
	mustContainInOrder(t, order, []string{"response.output_item.added", "response.output_text.done", "response.output_item.done", "response.completed"})
	for _, e := range events {
		if e.Type == "response.output_text.done" {
			if e.Data["text"] != "" {
				t.Fatalf("empty answer output_text.done text = %v, want \"\"", e.Data["text"])
			}
		}
		if e.Type == "response.output_text.delta" {
			t.Fatalf("empty answer must not emit any output_text.delta")
		}
	}
	last := events[len(events)-1]
	if last.Type != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", last.Type)
	}
}

func TestOpenAIResponsesStreamResolveErrorReturnsJSON(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"no-such-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	// A pre-stream resolve failure has no stream yet, so it returns a JSON error.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json (not SSE)", ct)
	}
}

// toolStreamer emits an assistant tool call (no text) then completes.
type toolStreamer struct{}

func (toolStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{ToolCalls: []inference.ToolCall{{ID: "call_1", Name: "shell", Arguments: `{"cmd":"ls"}`}}, Usage: inference.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}, nil
}

func (toolStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "call_1", Name: "shell", Arguments: `{"cmd":"ls"}`}}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}})
}

func TestOpenAIResponsesStreamEmitsFunctionCall(t *testing.T) {
	srv := newStreamTestServerWithProvider(toolStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-coder","stream":true,"input":"ls"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Responses stream must not contain [DONE]")
	}
	events := responsesSSEEvents(t, body)
	var order []string
	for _, e := range events {
		order = append(order, e.Type)
	}
	// The function-call event sequence (arguments delta/done for other clients) +
	// terminal completed.
	mustContainInOrder(t, order, []string{
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})

	// A tool-only turn: no text delta and no (empty) message item — the function
	// call takes output_index 0.
	for _, e := range events {
		if e.Type == "response.output_text.delta" {
			t.Fatalf("tool-only turn must not emit a text delta")
		}
		if e.Type == "response.output_item.done" {
			if item, _ := e.Data["item"].(map[string]any); item["type"] == "message" {
				t.Fatalf("tool-only turn must not emit a message item")
			}
		}
	}
	// Codex reconstructs the call from the function_call output_item.done — find it
	// and verify call_id / name / arguments / output_index 0.
	var found bool
	for _, e := range events {
		if e.Type != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		found = true
		if item["call_id"] != "call_1" || item["name"] != "shell" || item["arguments"] != `{"cmd":"ls"}` {
			t.Fatalf("function_call item = %v", item)
		}
		if idx, _ := e.Data["output_index"].(float64); int(idx) != 0 {
			t.Fatalf("function_call output_index = %v, want 0 (tool-only)", e.Data["output_index"])
		}
	}
	if !found {
		t.Fatalf("no function_call output_item.done in:\n%s", body)
	}

	// response.completed carries the function_call in its output.
	last := events[len(events)-1]
	resp, _ := last.Data["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	var sawFC bool
	for _, it := range out {
		if m, ok := it.(map[string]any); ok && m["type"] == "function_call" {
			sawFC = true
		}
	}
	if !sawFC {
		t.Fatalf("response.completed output missing function_call: %v", out)
	}
}

func TestOpenAIResponsesStreamEmitsReasoningItem(t *testing.T) {
	srv := newStreamTestServerWithProvider(reasoningAnswerStreamer{
		reasoning: []string{"let me ", "think"},
		text:      []string{"The ", "answer"},
		usage:     inference.Usage{InputTokens: 2, OutputTokens: 5, TotalTokens: 7},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-coder","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())

	// The FIRST output_item.added is the reasoning item at output_index 0; the
	// message item follows at index 1.
	reasoningAddedIdx, msgAddedIdx := -1.0, -1.0
	for _, e := range events {
		if e.Type != "response.output_item.added" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		switch item["type"] {
		case "reasoning":
			if reasoningAddedIdx < 0 {
				reasoningAddedIdx, _ = e.Data["output_index"].(float64)
			}
		case "message":
			if msgAddedIdx < 0 {
				msgAddedIdx, _ = e.Data["output_index"].(float64)
			}
		}
	}
	if reasoningAddedIdx != 0 {
		t.Fatalf("reasoning item output_index = %v, want 0", reasoningAddedIdx)
	}
	if msgAddedIdx != 1 {
		t.Fatalf("message item output_index = %v, want 1 (after reasoning)", msgAddedIdx)
	}

	// The reasoning_text deltas reconstruct the full chain-of-thought.
	var reasoningText strings.Builder
	sawReasoningDelta := false
	for _, e := range events {
		if e.Type == "response.reasoning_text.delta" {
			sawReasoningDelta = true
			if d, ok := e.Data["delta"].(string); ok {
				reasoningText.WriteString(d)
			}
		}
	}
	if !sawReasoningDelta {
		t.Fatalf("no response.reasoning_text.delta events: %s", rec.Body.String())
	}
	if reasoningText.String() != "let me think" {
		t.Fatalf("reconstructed reasoning = %q, want %q", reasoningText.String(), "let me think")
	}

	// A completed reasoning output_item.done carries the full reasoning text; the
	// response.completed output contains BOTH the reasoning and message items.
	sawReasoningDone := false
	for _, e := range events {
		if e.Type == "response.output_item.done" {
			item, _ := e.Data["item"].(map[string]any)
			if item["type"] == "reasoning" {
				content, _ := item["content"].([]any)
				if len(content) == 1 {
					blk, _ := content[0].(map[string]any)
					if blk["type"] == "reasoning_text" && blk["text"] == "let me think" {
						sawReasoningDone = true
					}
				}
			}
		}
	}
	if !sawReasoningDone {
		t.Fatalf("no completed reasoning item with the full text: %s", rec.Body.String())
	}
	last := events[len(events)-1]
	if last.Type != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", last.Type)
	}
	respObj, _ := last.Data["response"].(map[string]any)
	out, _ := respObj["output"].([]any)
	types := map[string]bool{}
	for _, it := range out {
		if m, ok := it.(map[string]any); ok {
			types[fmt.Sprint(m["type"])] = true
		}
	}
	if !types["reasoning"] || !types["message"] {
		t.Fatalf("response.completed output missing reasoning/message: %v", out)
	}
}

func TestOpenAIResponsesStreamReasoningThenToolCall(t *testing.T) {
	// A tool-only turn WITH reasoning: reasoning item @0, function_call @1, no message.
	srv := newStreamTestServerWithProvider(reasoningAnswerStreamer{
		reasoning: []string{"plan the call"},
		toolCalls: []inference.ToolCall{{ID: "call_7", Name: "shell", Arguments: `{"cmd":"ls"}`}},
		usage:     inference.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-coder","stream":true,"input":"ls"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())
	reasoningIdx, fcIdx := -1.0, -1.0
	sawMessage := false
	for _, e := range events {
		if e.Type != "response.output_item.added" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		switch item["type"] {
		case "reasoning":
			reasoningIdx, _ = e.Data["output_index"].(float64)
		case "function_call":
			fcIdx, _ = e.Data["output_index"].(float64)
		case "message":
			sawMessage = true
		}
	}
	if reasoningIdx != 0 {
		t.Fatalf("reasoning output_index = %v, want 0", reasoningIdx)
	}
	if fcIdx != 1 {
		t.Fatalf("function_call output_index = %v, want 1 (after reasoning, no message)", fcIdx)
	}
	if sawMessage {
		t.Fatalf("a tool-only turn must not emit a message item: %s", rec.Body.String())
	}
}

// textToolStreamer emits some text, THEN a tool call, then completes — exercising
// the combined text+tool branch (text block @0, tool_use @1).
type textToolStreamer struct{}

func (textToolStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{Text: "Sure.", ToolCalls: []inference.ToolCall{{ID: "call_9", Name: "shell", Arguments: `{"cmd":"pwd"}`}}, Usage: inference.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}}, nil
}

func (textToolStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "Sure."}); err != nil {
		return err
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "call_9", Name: "shell", Arguments: `{"cmd":"pwd"}`}}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10}})
}

func TestAnthropicMessagesStreamTextThenToolUseIndices(t *testing.T) {
	srv := newStreamTestServerWithProvider(textToolStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"where am i"}],"tools":[{"name":"shell","input_schema":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())

	// The text block is index 0; the tool_use block is index 1 (contiguous).
	textIdx, toolIdx := -1, -1
	for _, e := range events {
		if e.Type != "content_block_start" {
			continue
		}
		cb, _ := e.Data["content_block"].(map[string]any)
		idx, _ := e.Data["index"].(float64)
		switch cb["type"] {
		case "text":
			textIdx = int(idx)
		case "tool_use":
			toolIdx = int(idx)
		}
	}
	if textIdx != 0 || toolIdx != 1 {
		t.Fatalf("indices = text %d, tool_use %d; want 0 and 1 (contiguous)", textIdx, toolIdx)
	}

	// The tool_use's input_json_delta lives at index 1 and accumulates the args.
	var args string
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		if idx, _ := e.Data["index"].(float64); int(idx) != 1 {
			continue
		}
		if delta, _ := e.Data["delta"].(map[string]any); delta["type"] == "input_json_delta" {
			frag, _ := delta["partial_json"].(string)
			args += frag
		}
	}
	if args != `{"cmd":"pwd"}` {
		t.Fatalf("accumulated tool args = %q, want {\"cmd\":\"pwd\"}", args)
	}

	// Reconstructed text from index-0 deltas.
	var text strings.Builder
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		if idx, _ := e.Data["index"].(float64); int(idx) != 0 {
			continue
		}
		if delta, _ := e.Data["delta"].(map[string]any); delta["type"] == "text_delta" {
			s, _ := delta["text"].(string)
			text.WriteString(s)
		}
	}
	if text.String() != "Sure." {
		t.Fatalf("reconstructed text = %q, want %q", text.String(), "Sure.")
	}
	// stop_reason is tool_use when any tool call is present.
	stop := ""
	for _, e := range events {
		if e.Type == "message_delta" {
			if delta, _ := e.Data["delta"].(map[string]any); delta != nil {
				stop, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stop != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stop)
	}
}

func TestAnthropicMessagesStreamEmptyAnswerIsCoherent(t *testing.T) {
	// No text, no tool calls: the stream must still be a coherent Anthropic message
	// — an (empty) text block at index 0, stop_reason end_turn, terminated cleanly.
	srv := newStreamTestServerWithProvider(textStreamer{chunks: nil, usage: inference.Usage{InputTokens: 1, OutputTokens: 0, TotalTokens: 1}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())
	var order []string
	for _, e := range events {
		order = append(order, e.Type)
	}
	// A text block still opens+closes at index 0 even with no deltas.
	mustContainInOrder(t, order, []string{
		"message_start",
		"content_block_start",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	textBlockIdx := -1
	for _, e := range events {
		if e.Type == "content_block_start" {
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb["type"] == "text" {
				idx, _ := e.Data["index"].(float64)
				textBlockIdx = int(idx)
			}
		}
	}
	if textBlockIdx != 0 {
		t.Fatalf("empty-answer text block index = %d, want 0", textBlockIdx)
	}
	stop := ""
	for _, e := range events {
		if e.Type == "message_delta" {
			if delta, _ := e.Data["delta"].(map[string]any); delta != nil {
				stop, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stop != "end_turn" {
		t.Fatalf("empty-answer stop_reason = %q, want end_turn", stop)
	}
	if events[len(events)-1].Type != "message_stop" {
		t.Fatalf("last event = %q, want message_stop", events[len(events)-1].Type)
	}
}

func TestAnthropicMessagesStreamEmitsEventSequence(t *testing.T) {
	srv := newStreamTestServerWithProvider(textStreamer{chunks: []string{"Hello", " world"}, usage: inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	// Anthropic ends on message_stop, never a chat-style [DONE].
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream must not contain [DONE]: %s", body)
	}

	events := responsesSSEEvents(t, body) // reused generic event:/data: parser
	var order []string
	for _, e := range events {
		order = append(order, e.Type)
	}
	mustContainInOrder(t, order, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	// Every event's data.type mirrors its event: line.
	for i, e := range events {
		if e.Data["type"] != e.Type {
			t.Fatalf("event[%d] data.type = %v, event: line = %q", i, e.Data["type"], e.Type)
		}
	}

	// message_start carries an assistant message envelope with empty content.
	first := events[0]
	if first.Type != "message_start" {
		t.Fatalf("first event = %q, want message_start", first.Type)
	}
	msg, _ := first.Data["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Fatalf("message_start role = %v, want assistant", msg["role"])
	}
	if content, _ := msg["content"].([]any); len(content) != 0 {
		t.Fatalf("message_start content = %v, want empty", msg["content"])
	}

	// Concatenated text_delta fragments reconstruct the full answer.
	var text strings.Builder
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		delta, _ := e.Data["delta"].(map[string]any)
		if delta["type"] == "text_delta" {
			s, _ := delta["text"].(string)
			text.WriteString(s)
		}
	}
	if text.String() != "Hello world" {
		t.Fatalf("reconstructed text = %q, want %q", text.String(), "Hello world")
	}

	// message_delta carries stop_reason end_turn (no tool calls) + cumulative usage.
	var sawDelta bool
	for _, e := range events {
		if e.Type != "message_delta" {
			continue
		}
		sawDelta = true
		delta, _ := e.Data["delta"].(map[string]any)
		if delta["stop_reason"] != "end_turn" {
			t.Fatalf("message_delta stop_reason = %v, want end_turn", delta["stop_reason"])
		}
		usage, _ := e.Data["usage"].(map[string]any)
		if got, _ := usage["output_tokens"].(float64); int(got) != 3 {
			t.Fatalf("message_delta output_tokens = %v, want 3", usage["output_tokens"])
		}
	}
	if !sawDelta {
		t.Fatalf("no message_delta event in:\n%s", body)
	}

	// Usage is recorded exactly once.
	if evs := srv.Usage.ByUser("usr_dev"); len(evs) != 1 || evs[0].OutputTokens != 3 {
		t.Fatalf("usage events = %+v, want one with 3 output tokens", evs)
	}
}

func TestAnthropicMessagesStreamEmitsToolUse(t *testing.T) {
	srv := newStreamTestServerWithProvider(toolStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"ls"}],"tools":[{"name":"shell","input_schema":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream must not contain [DONE]")
	}
	events := responsesSSEEvents(t, body)

	// A tool-only turn (no text): the tool_use content block takes index 0, so there
	// is no text_delta.
	for _, e := range events {
		if e.Type == "content_block_delta" {
			if delta, _ := e.Data["delta"].(map[string]any); delta["type"] == "text_delta" {
				t.Fatalf("tool-only turn must not emit a text_delta")
			}
		}
	}

	// Reconstruct the tool call as an Anthropic client does: content_block_start
	// carries id + name + empty input; input_json_delta.partial_json accumulates the
	// arguments; content_block_stop closes it.
	var gotID, gotName, gotArgs string
	toolIndex := -1
	for _, e := range events {
		switch e.Type {
		case "content_block_start":
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				gotID, _ = cb["id"].(string)
				gotName, _ = cb["name"].(string)
				if idx, ok := e.Data["index"].(float64); ok {
					toolIndex = int(idx)
				}
				// input starts as an empty object.
				if in, _ := cb["input"].(map[string]any); in == nil || len(in) != 0 {
					t.Fatalf("tool_use content_block_start input = %v, want empty object", cb["input"])
				}
			}
		case "content_block_delta":
			if delta, _ := e.Data["delta"].(map[string]any); delta["type"] == "input_json_delta" {
				frag, _ := delta["partial_json"].(string)
				gotArgs += frag
			}
		}
	}
	if toolIndex != 0 {
		t.Fatalf("tool_use index = %d, want 0 (tool-only turn)", toolIndex)
	}
	if gotID != "call_1" || gotName != "shell" || gotArgs != `{"cmd":"ls"}` {
		t.Fatalf("reconstructed tool call = id %q name %q args %q, want call_1/shell/{\"cmd\":\"ls\"}", gotID, gotName, gotArgs)
	}

	// message_delta stop_reason flips to tool_use.
	var stop string
	for _, e := range events {
		if e.Type == "message_delta" {
			if delta, _ := e.Data["delta"].(map[string]any); delta != nil {
				stop, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stop != "tool_use" {
		t.Fatalf("message_delta stop_reason = %q, want tool_use", stop)
	}

	// The stream terminates with message_stop.
	if events[len(events)-1].Type != "message_stop" {
		t.Fatalf("last event = %q, want message_stop", events[len(events)-1].Type)
	}
}

func TestAnthropicMessagesStreamResolveErrorReturnsJSON(t *testing.T) {
	srv := newStreamTestServerWithProvider(textStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"no-such-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	// A pre-stream resolve failure has no stream yet, so it returns a JSON error.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json (not SSE)", ct)
	}
}

func TestAnthropicMessagesStreamEmitsThinkingBlock(t *testing.T) {
	// A reasoning turn streams a thinking block at index 0, fully closed (with an
	// empty signature_delta) BEFORE the text block at index 1 — the Anthropic
	// ordering rule (thinking → text) with contiguous indices.
	srv := newStreamTestServerWithProvider(reasoningAnswerStreamer{
		reasoning: []string{"let me ", "think"},
		text:      []string{"Hi."},
		usage:     inference.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())

	// The thinking block is index 0, the text block index 1 (contiguous).
	thinkingIdx, textIdx := -1, -1
	thinkingStartPos, thinkingStopPos, textStartPos := -1, -1, -1
	for i, e := range events {
		switch e.Type {
		case "content_block_start":
			cb, _ := e.Data["content_block"].(map[string]any)
			idx, _ := e.Data["index"].(float64)
			switch cb["type"] {
			case "thinking":
				thinkingIdx = int(idx)
				thinkingStartPos = i
			case "text":
				textIdx = int(idx)
				textStartPos = i
			}
		case "content_block_stop":
			if idx, _ := e.Data["index"].(float64); int(idx) == thinkingIdx && thinkingStopPos < 0 {
				thinkingStopPos = i
			}
		}
	}
	if thinkingIdx != 0 || textIdx != 1 {
		t.Fatalf("indices = thinking %d, text %d; want 0 and 1", thinkingIdx, textIdx)
	}
	// The thinking block must be fully STOPPED before the text block starts.
	if !(thinkingStartPos >= 0 && thinkingStopPos > thinkingStartPos && textStartPos > thinkingStopPos) {
		t.Fatalf("ordering: thinking start %d, thinking stop %d, text start %d; want start<stop<text", thinkingStartPos, thinkingStopPos, textStartPos)
	}

	// thinking_delta fragments reconstruct the full chain-of-thought.
	var reasoning strings.Builder
	var sawSignature bool
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		delta, _ := e.Data["delta"].(map[string]any)
		switch delta["type"] {
		case "thinking_delta":
			s, _ := delta["thinking"].(string)
			reasoning.WriteString(s)
		case "signature_delta":
			sawSignature = true
			if sig, _ := delta["signature"].(string); sig != "" {
				t.Fatalf("signature_delta signature = %q, want empty", sig)
			}
		}
	}
	if reasoning.String() != "let me think" {
		t.Fatalf("reconstructed reasoning = %q, want %q", reasoning.String(), "let me think")
	}
	if !sawSignature {
		t.Fatalf("no signature_delta emitted before the thinking block closed: %s", rec.Body.String())
	}

	// Text is reconstructed from the index-1 deltas.
	var text strings.Builder
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		if idx, _ := e.Data["index"].(float64); int(idx) != 1 {
			continue
		}
		if delta, _ := e.Data["delta"].(map[string]any); delta["type"] == "text_delta" {
			s, _ := delta["text"].(string)
			text.WriteString(s)
		}
	}
	if text.String() != "Hi." {
		t.Fatalf("reconstructed text = %q, want %q", text.String(), "Hi.")
	}
}

func TestAnthropicMessagesStreamThinkingThenToolUse(t *testing.T) {
	// A tool-only turn WITH reasoning: thinking at index 0, tool_use at index 1, no
	// text block in between.
	srv := newStreamTestServerWithProvider(reasoningAnswerStreamer{
		reasoning: []string{"plan the call"},
		toolCalls: []inference.ToolCall{{ID: "toolu_1", Name: "shell", Arguments: `{"cmd":"pwd"}`}},
		usage:     inference.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"pwd"}],"tools":[{"name":"shell","input_schema":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())

	thinkingIdx, toolIdx := -1, -1
	sawText := false
	for _, e := range events {
		if e.Type != "content_block_start" {
			continue
		}
		cb, _ := e.Data["content_block"].(map[string]any)
		idx, _ := e.Data["index"].(float64)
		switch cb["type"] {
		case "thinking":
			thinkingIdx = int(idx)
		case "tool_use":
			toolIdx = int(idx)
		case "text":
			sawText = true
		}
	}
	if thinkingIdx != 0 || toolIdx != 1 {
		t.Fatalf("indices = thinking %d, tool_use %d; want 0 and 1", thinkingIdx, toolIdx)
	}
	if sawText {
		t.Fatalf("tool-only turn with reasoning must not emit a text block")
	}
	// stop_reason is tool_use.
	stop := ""
	for _, e := range events {
		if e.Type == "message_delta" {
			if delta, _ := e.Data["delta"].(map[string]any); delta != nil {
				stop, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stop != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stop)
	}
}

// finishStreamer emits fixed text then a completed event carrying a chosen
// finish_reason + usage — for exercising the stop_reason mapping and cache usage.
type finishStreamer struct {
	text         []string
	finishReason string
	usage        inference.Usage
}

func (finishStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (s finishStreamer) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for _, c := range s.text {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: c}); err != nil {
			return err
		}
	}
	u := s.usage
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u, FinishReason: s.finishReason})
}

func TestAnthropicMessagesStreamStopReasonAndCacheUsage(t *testing.T) {
	// finish_reason "length" -> stop_reason "max_tokens"; cached prompt tokens are
	// reported in cache_read_input_tokens and EXCLUDED from input_tokens.
	srv := newStreamTestServerWithProvider(finishStreamer{
		text:         []string{"partial"},
		finishReason: "length",
		usage:        inference.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105, CachedTokens: 40},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := responsesSSEEvents(t, rec.Body.String())
	var sawDelta bool
	for _, e := range events {
		if e.Type != "message_delta" {
			continue
		}
		sawDelta = true
		delta, _ := e.Data["delta"].(map[string]any)
		if delta["stop_reason"] != "max_tokens" {
			t.Fatalf("stop_reason = %v, want max_tokens", delta["stop_reason"])
		}
		usage, _ := e.Data["usage"].(map[string]any)
		if got, _ := usage["input_tokens"].(float64); int(got) != 60 {
			t.Fatalf("input_tokens = %v, want 60 (100-40 cached)", usage["input_tokens"])
		}
		if got, _ := usage["cache_read_input_tokens"].(float64); int(got) != 40 {
			t.Fatalf("cache_read_input_tokens = %v, want 40", usage["cache_read_input_tokens"])
		}
	}
	if !sawDelta {
		t.Fatalf("no message_delta event in:\n%s", rec.Body.String())
	}
}

// TestRecordedUsageInputExcludesCachedTokens pins the gateway-wide accounting
// convention: the STORED/displayed usage event counts only NEW (freshly processed)
// tokens in input_tokens, with cache reads reported separately in cached_tokens
// (input = canonical-includes-cached MINUS cached). total_tokens stays the full
// total, so input + cached + output == total. The client-facing wire response is a
// separate, protocol-specific concern and is unaffected (see the stream test above,
// which keeps Anthropic input_tokens = 60 on the wire).
func TestRecordedUsageInputExcludesCachedTokens(t *testing.T) {
	srv := newStreamTestServerWithProvider(finishStreamer{
		text:         []string{"partial"},
		finishReason: "stop",
		usage:        inference.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105, CachedTokens: 40},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.InputTokens != 60 {
		t.Fatalf("event input_tokens = %d, want 60 (100 - 40 cached = new tokens only)", ev.InputTokens)
	}
	if ev.CachedTokens != 40 {
		t.Fatalf("event cached_tokens = %d, want 40", ev.CachedTokens)
	}
	if ev.TotalTokens != 105 {
		t.Fatalf("event total_tokens = %d, want 105 (full total, unchanged)", ev.TotalTokens)
	}
}

// TestRecordedUsageSplitsCacheReadAndWrite pins the three disjoint prompt buckets:
// with canonical input 100 that includes 40 cache-read + 25 cache-write, the stored
// event carries input_tokens=35 (fresh), cached_tokens=40 (read), cache_write_tokens=25
// (write), and total_tokens stays the full 130 (so input+cached+write+output == total).
func TestRecordedUsageSplitsCacheReadAndWrite(t *testing.T) {
	srv := newStreamTestServerWithProvider(finishStreamer{
		text:         []string{"partial"},
		finishReason: "stop",
		usage:        inference.Usage{InputTokens: 100, OutputTokens: 30, TotalTokens: 130, CachedTokens: 40, CacheWriteTokens: 25},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qwen-coder","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.InputTokens != 35 { // 100 - 40 read - 25 write
		t.Fatalf("event input_tokens = %d, want 35 (fresh only)", ev.InputTokens)
	}
	if ev.CachedTokens != 40 {
		t.Fatalf("event cached_tokens = %d, want 40 (read)", ev.CachedTokens)
	}
	if ev.CacheWriteTokens != 25 {
		t.Fatalf("event cache_write_tokens = %d, want 25 (write)", ev.CacheWriteTokens)
	}
	if ev.TotalTokens != 130 {
		t.Fatalf("event total_tokens = %d, want 130", ev.TotalTokens)
	}
	if ev.InputTokens+ev.CachedTokens+ev.CacheWriteTokens+ev.OutputTokens != ev.TotalTokens {
		t.Fatalf("buckets don't sum to total: %d+%d+%d+%d != %d", ev.InputTokens, ev.CachedTokens, ev.CacheWriteTokens, ev.OutputTokens, ev.TotalTokens)
	}
}

// recordingProxyProvider implements Client + StreamingClient + NativeProxyClient.
// It records the last ProxyNative call and returns a canned body, so the native
// passthrough path can be exercised and asserted; CompleteStream lets the same
// provider back the translate-path fallback (so a non-native app still works).
type recordingProxyProvider struct {
	respBody   string
	proxyCalls int
	gotPath    string
	gotBody    []byte
	gotModel   string
}

func (p *recordingProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{Text: "translated", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

func (p *recordingProxyProvider) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "translated"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
}

func (p *recordingProxyProvider) ProxyNative(_ context.Context, _ routing.Target, path string, body []byte) (*provider.ProxyResponse, error) {
	p.proxyCalls++
	p.gotPath = path
	p.gotBody = append([]byte(nil), body...)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	p.gotModel = probe.Model
	return &provider.ProxyResponse{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(p.respBody)),
	}, nil
}

// newNativeProxyTestServer seeds one vLLM upstream + app + mapping (gateway model
// "gw-model" -> upstream "upstream-model", so the model-rewrite is observable),
// with the given native-passthrough flags.
func newNativeProxyTestServer(prov provider.Client, nativeResponses, nativeMessages bool) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-native", Name: "Native Upstream", Domain: "native.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://native.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-native", ServerID: "srv-native", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, NativeResponses: nativeResponses, NativeMessages: nativeMessages, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-native", ApplicationID: "app-native", GatewayModelName: "gw-model", AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-native", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		panic(err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

func TestOpenAIResponsesNativePassthroughProxiesRawBody(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":7,\"total_tokens\":10}}}\n\n"}
	srv := newNativeProxyTestServer(prov, true, false)
	// A rich Codex-style body: the translate parser would drop `tools`, but
	// passthrough must forward it verbatim (only `model` is rewritten).
	body := `{"model":"gw-model","stream":true,"input":"hi","tools":[{"type":"function","name":"shell"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("X-OP-AI-Gateway-Session-ID", "sess-xyz")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
	}
	if prov.gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", prov.gotPath)
	}
	// model rewritten to the mapped upstream name; everything else preserved.
	if prov.gotModel != "upstream-model" {
		t.Fatalf("upstream model = %q, want upstream-model (rewritten)", prov.gotModel)
	}
	if !strings.Contains(string(prov.gotBody), `"tools"`) || !strings.Contains(string(prov.gotBody), "shell") {
		t.Fatalf("upstream body dropped fields: %s", prov.gotBody)
	}
	// Response streamed back verbatim.
	if rec.Body.String() != prov.respBody {
		t.Fatalf("client body = %q, want the upstream body verbatim %q", rec.Body.String(), prov.respBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	// Usage best-effort parsed from the response.completed event.
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.InputTokens != 3 || ev.OutputTokens != 7 || ev.TotalTokens != 10 {
		t.Fatalf("usage tokens = %d/%d/%d, want 3/7/10", ev.InputTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if !ev.Stream || ev.ContentType != "text/event-stream" || ev.Model != "gw-model" {
		t.Fatalf("usage event = %+v", ev)
	}
	// SessionID from the header must be attributed to native-passthrough usage too.
	if ev.SessionID != "sess-xyz" {
		t.Fatalf("usage SessionID = %q, want sess-xyz", ev.SessionID)
	}
}

func TestOpenAIResponsesNonNativeUsesTranslatePath(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "unused"}
	srv := newNativeProxyTestServer(prov, false, false) // native OFF
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0 (translate path expected)", prov.proxyCalls)
	}
	// The translate path emits the Responses event sequence it builds itself.
	if !strings.Contains(rec.Body.String(), "response.output_text.delta") {
		t.Fatalf("expected translated Responses events, got: %s", rec.Body.String())
	}
}

func TestAnthropicNativePassthroughProxiesRawBody(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n"}
	srv := newNativeProxyTestServer(prov, false, true) // Claude Code native ON
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/messages" {
		t.Fatalf("proxyCalls=%d path=%q, want 1 /v1/messages", prov.proxyCalls, prov.gotPath)
	}
	if prov.gotModel != "upstream-model" {
		t.Fatalf("upstream model = %q, want upstream-model", prov.gotModel)
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].OutputTokens != 42 {
		t.Fatalf("usage = %+v, want 1 event with 42 output tokens", events)
	}
}

func TestRewriteModelField(t *testing.T) {
	// Rewrites model, preserves other fields incl. large-int precision.
	out := rewriteModelField([]byte(`{"model":"gw","input":"hi","max_output_tokens":9007199254740993}`), "up")
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["model"] != "up" {
		t.Fatalf("model = %v, want up", m["model"])
	}
	if m["input"] != "hi" {
		t.Fatalf("input not preserved: %v", m["input"])
	}
	if n, _ := m["max_output_tokens"].(json.Number); n.String() != "9007199254740993" {
		t.Fatalf("large int lost precision: %v", m["max_output_tokens"])
	}
	// No-op cases: empty providerModel, already-equal, non-object.
	same := `{"model":"gw"}`
	if string(rewriteModelField([]byte(same), "")) != same {
		t.Fatalf("empty providerModel should be a no-op")
	}
	if string(rewriteModelField([]byte(`{"model":"up"}`), "up")) != `{"model":"up"}` {
		t.Fatalf("already-equal should be a no-op")
	}
	notObj := `["a","b"]`
	if string(rewriteModelField([]byte(notObj), "up")) != notObj {
		t.Fatalf("non-object should be a no-op")
	}
}

func TestParsePassthroughUsage(t *testing.T) {
	// Responses stream: usage from response.completed.
	respSSE := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":22,\"total_tokens\":33}}}\n\n"
	if u := parsePassthroughUsage("openai_responses", []byte(respSSE)); u.InputTokens != 11 || u.OutputTokens != 22 || u.TotalTokens != 33 {
		t.Fatalf("responses SSE usage = %+v", u)
	}
	// Responses non-stream: top-level usage; total derived.
	respJSON := `{"id":"r","usage":{"input_tokens":5,"output_tokens":6}}`
	if u := parsePassthroughUsage("openai_responses", []byte(respJSON)); u.InputTokens != 5 || u.OutputTokens != 6 || u.TotalTokens != 11 {
		t.Fatalf("responses JSON usage = %+v", u)
	}
	// Anthropic stream: input from message_start, output from message_delta.
	antSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"
	if u := parsePassthroughUsage("anthropic_messages", []byte(antSSE)); u.InputTokens != 8 || u.OutputTokens != 40 || u.TotalTokens != 48 {
		t.Fatalf("anthropic SSE usage = %+v", u)
	}
	// Absent usage yields zero, never an error.
	if u := parsePassthroughUsage("openai_responses", []byte("event: ping\ndata: {}\n\n")); u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Fatalf("absent usage = %+v, want zero", u)
	}
	// A buffered JSON body whose string content contains "data:" must NOT be
	// mistaken for an SSE stream (which would parse zero usage).
	jsonWithDataURI := `{"id":"r","output":[{"text":"see data:image/png;base64,AAAA"}],"usage":{"input_tokens":7,"output_tokens":8}}`
	if u := parsePassthroughUsage("openai_responses", []byte(jsonWithDataURI)); u.InputTokens != 7 || u.OutputTokens != 8 {
		t.Fatalf("json-with-data-uri usage = %+v, want 7/8", u)
	}
}

// TestParsePassthroughUsageCachedTokens pins that native passthrough surfaces the
// upstream's prompt-cache token counts, so the Activity "Cached" column is not
// stuck at 0 (llama-swap reports them, we must parse them). The canonical
// inference.Usage uses OpenAI semantics: InputTokens INCLUDES the cached-read
// subset (see compat.AnthropicInputTokens, the inverse); Anthropic reports
// input_tokens EXCLUDING cache reads/creations, so the parser folds them back in.
func TestParsePassthroughUsageCachedTokens(t *testing.T) {
	// Anthropic stream: message_start carries input_tokens (fresh, excl. cache) +
	// cache_read_input_tokens + cache_creation_input_tokens; message_delta carries
	// the final cumulative output_tokens.
	antSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":2,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"
	u := parsePassthroughUsage("anthropic_messages", []byte(antSSE))
	if u.CachedTokens != 40 {
		t.Fatalf("anthropic cached_tokens = %d, want 40", u.CachedTokens)
	}
	if u.CacheWriteTokens != 2 {
		t.Fatalf("anthropic cache_write_tokens = %d, want 2 (cache creation)", u.CacheWriteTokens)
	}
	if u.InputTokens != 50 { // 8 fresh + 40 cache read + 2 cache creation
		t.Fatalf("anthropic input_tokens = %d, want 50 (incl. cache)", u.InputTokens)
	}
	if u.OutputTokens != 50 {
		t.Fatalf("anthropic output_tokens = %d, want 50", u.OutputTokens)
	}
	if u.TotalTokens != 100 { // 50 input (incl. cache) + 50 output
		t.Fatalf("anthropic total_tokens = %d, want 100", u.TotalTokens)
	}

	// Anthropic non-stream JSON: top-level usage with cache_read only.
	antJSON := `{"type":"message","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":5}}`
	if u := parsePassthroughUsage("anthropic_messages", []byte(antJSON)); u.CachedTokens != 90 || u.InputTokens != 100 || u.OutputTokens != 5 {
		t.Fatalf("anthropic JSON usage = %+v, want cached=90 input=100 output=5", u)
	}

	// Responses: OpenAI input_tokens ALREADY includes the cached subset
	// (input_tokens_details.cached_tokens), so InputTokens is unchanged.
	respJSON := `{"id":"r","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":6}}`
	if u := parsePassthroughUsage("openai_responses", []byte(respJSON)); u.CachedTokens != 80 || u.InputTokens != 100 || u.OutputTokens != 6 {
		t.Fatalf("responses JSON usage = %+v, want cached=80 input=100 output=6", u)
	}
}

// erroringStreamer implements provider.Client + provider.StreamingClient and
// emits one text delta before failing, exercising the in-band error frame and
// error-status usage path.
type erroringStreamer struct{}

func (erroringStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (erroringStreamer) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "partial"}); err != nil {
		return err
	}
	return fmt.Errorf("%w: boom", provider.ErrUnavailable)
}

func TestOpenAIChatStreamProviderErrorMidStream(t *testing.T) {
	srv := newStreamTestServerWithProvider(erroringStreamer{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	// Headers were already flushed before the provider failed, so the client
	// still sees a 200 SSE stream carrying an in-band error frame.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal data: [DONE], body = %s", body)
	}
	sawPartial := false
	sawError := false
	for _, chunk := range sseDataChunks(t, body) {
		if delta := chunkDelta(chunk); delta != nil {
			if content, ok := delta["content"].(string); ok && content == "partial" {
				sawPartial = true
			}
		}
		if errObj, ok := chunk["error"].(map[string]any); ok {
			if code, _ := errObj["code"].(string); code == "provider.unavailable" {
				sawError = true
			}
		}
	}
	if !sawPartial {
		t.Fatalf("missing partial content chunk, body = %s", body)
	}
	if !sawError {
		t.Fatalf("missing in-band error frame, body = %s", body)
	}

	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events for usr_dev = %d, want 1", len(events))
	}
	if events[0].Status != "error" {
		t.Fatalf("usage Status = %q, want error", events[0].Status)
	}
}

func TestAnthropicCountTokensRequiresAuthAndAliasWorks(t *testing.T) {
	srv := NewTestServer()
	unauthenticated := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello token count"}]}`))
	unauthenticatedRec := httptest.NewRecorder()

	srv.ServeHTTP(unauthenticatedRec, unauthenticated)

	if unauthenticatedRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticatedRec.Code)
	}

	req := newJSONRequest(http.MethodPost, "/v1/messages/count_tokens", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello token count"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.InputTokens != 3 {
		t.Fatalf("input_tokens = %d, want 3", body.InputTokens)
	}
	if len(srv.Usage.All()) != 0 {
		t.Fatalf("usage events = %d, want 0", len(srv.Usage.All()))
	}
}

func TestAPIUsageRequiresBearerTokenAndReturnsCurrentUserUsage(t *testing.T) {
	srv := NewTestServer()
	srv.Usage.Record(usage.Event{ID: "req_1", UserID: "usr_dev", TokenID: "tok_dev", Model: "qwen-coder", TotalTokens: 5, CreatedAt: time.Now().UTC()})
	srv.Usage.Record(usage.Event{ID: "req_2", UserID: "usr_other", TokenID: "tok_other", Model: "qwen-coder", TotalTokens: 7, CreatedAt: time.Now().UTC()})

	unauthenticated := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	unauthenticatedRec := httptest.NewRecorder()

	srv.ServeHTTP(unauthenticatedRec, unauthenticated)

	if unauthenticatedRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticatedRec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []usage.Event `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("usage events = %d, want 1", len(body.Data))
	}
	if body.Data[0].UserID != "usr_dev" {
		t.Fatalf("UserID = %q, want usr_dev", body.Data[0].UserID)
	}
}

func TestPortalEndpointsRequireBearerToken(t *testing.T) {
	paths := []string{
		"/api/portal/me",
		"/api/portal/tokens",
		"/api/portal/usage",
		"/api/portal/dashboard",
		"/api/portal/models",
		"/api/portal/health-check-interval",
		"/api/portal/agent-presence-timeout",
		"/api/portal/model-group-servers",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			srv := NewTestServer()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestPortalHealthCheckIntervalReturnsDefault(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodGet, "/api/portal/health-check-interval", "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Seconds int `json:"health_check_interval_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Seconds != 30 {
		t.Fatalf("health_check_interval_seconds = %d, want 30", body.Seconds)
	}
}

func TestPortalHealthCheckIntervalRejectsNonGet(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/api/portal/health-check-interval", "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestPortalAgentPresenceTimeoutReturnsDefault(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodGet, "/api/portal/agent-presence-timeout", "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Seconds int `json:"seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Seconds != 15 {
		t.Fatalf("seconds = %d, want 15", body.Seconds)
	}
}

func TestPortalAgentPresenceTimeoutRejectsNonGet(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/api/portal/agent-presence-timeout", "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestPortalMeReturnsCurrentUser(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodGet, "/api/portal/me", "")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.ID != "usr_dev" || body.Email != "dev@example.test" || body.DisplayName != "Dev User" || body.Role != "admin" {
		t.Fatalf("body = %#v", body)
	}
}

func TestPortalMeMapsNotFoundAndInternalErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "not found", err: store.ErrNotFound, want: http.StatusNotFound, code: "portal.user_not_found"},
		{name: "internal", err: errors.New("database unavailable"), want: http.StatusInternalServerError, code: "portal.user_lookup_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := auth.NewTokenStore()
			tokens.AddPlainToken(auth.Token{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Active: true, Scopes: []string{"gateway:use"}}, "dev-secret")
			srv := New(ServerDeps{
				Tokens:   tokens,
				Usage:    usage.NewRecorder(),
				Provider: provider.NewMock(),
				Portal:   portal.NewService(portal.ServiceDeps{Users: errorUsers{err: tt.err}}),
			})
			req := newJSONRequest(http.MethodGet, "/api/portal/me", "")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if body.Error.Code != tt.code {
				t.Fatalf("error code = %q, want %s", body.Error.Code, tt.code)
			}
		})
	}
}

func TestPortalTokensNeverExposeSecrets(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodGet, "/api/portal/tokens", "")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dev-secret") || strings.Contains(rec.Body.String(), auth.HashSecret("dev-secret")) {
		t.Fatalf("token response leaked secret material: %s", rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			SecretPrefix  string   `json:"secret_prefix"`
			Scopes        []string `json:"scopes"`
			IsChatSession bool     `json:"is_chat_session"`
			Deletable     bool     `json:"deletable"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	// The synthetic non-deletable ChatSession row is prepended before real tokens.
	if len(body.Data) != 2 {
		t.Fatalf("body = %#v", body)
	}
	if body.Data[0].ID != "chat-session" || !body.Data[0].IsChatSession || body.Data[0].Deletable {
		t.Fatalf("synthetic row = %#v", body.Data[0])
	}
	if body.Data[1].ID != "tok_dev" || body.Data[1].SecretPrefix == "" || !body.Data[1].Deletable {
		t.Fatalf("real row = %#v", body.Data[1])
	}
	if len(body.Data[1].Scopes) != 2 {
		t.Fatalf("scopes = %#v", body.Data[1].Scopes)
	}
}

func TestPortalTokenItemRejectsChatSessionID(t *testing.T) {
	srv := NewTestServer()
	for _, tc := range []struct {
		method string
		body   string
	}{
		{http.MethodDelete, ""},
		{http.MethodPatch, `{"name":"x"}`},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(tc.method, "/api/portal/tokens/chat-session", tc.body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s chat-session = %d, want 400; body=%s", tc.method, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "token.not_deletable") {
			t.Fatalf("%s chat-session body = %s, want token.not_deletable", tc.method, rec.Body.String())
		}
	}
}

func TestPortalTokenRotateReplacesSecret(t *testing.T) {
	srv := NewTestServer()

	// Create a fresh token to rotate (leaves dev-secret intact for auth).
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"Rotate Me","scopes":["gateway:use"]}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}

	chatStatus := func(secret string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := chatStatus(created.Secret); got != http.StatusOK {
		t.Fatalf("created secret status before rotate = %d, want 200", got)
	}

	rotateRec := httptest.NewRecorder()
	srv.ServeHTTP(rotateRec, newJSONRequest(http.MethodPost, "/api/portal/tokens/"+created.Token.ID+"/rotate", ""))
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body=%s", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated struct {
		Token struct {
			ID           string `json:"id"`
			SecretPrefix string `json:"secret_prefix"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("unmarshal rotate: %v", err)
	}
	if rotated.Token.ID != created.Token.ID {
		t.Fatalf("rotate kept token id = %q, want %q", rotated.Token.ID, created.Token.ID)
	}
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatalf("rotate must return a new secret; got %q (old %q)", rotated.Secret, created.Secret)
	}
	if rotated.Token.SecretPrefix == "" {
		t.Fatalf("rotate token missing secret_prefix")
	}

	// Old secret is dead, new secret authenticates.
	if got := chatStatus(created.Secret); got != http.StatusUnauthorized {
		t.Fatalf("old secret status after rotate = %d, want 401", got)
	}
	if got := chatStatus(rotated.Secret); got != http.StatusOK {
		t.Fatalf("new secret status after rotate = %d, want 200", got)
	}
}

func TestPortalTokenRotateRejectsChatSessionID(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens/chat-session/rotate", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rotate chat-session = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "portal.token_not_found") {
		t.Fatalf("body = %s, want portal.token_not_found", rec.Body.String())
	}
}

func TestPortalUsageReturnsCurrentUserUsage(t *testing.T) {
	srv := NewTestServer()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	srv.Usage.Record(usage.Event{ID: "req_user", UserID: "usr_dev", TokenID: "tok_dev", Model: "qwen-coder", TotalTokens: 5, CreatedAt: now})
	srv.Usage.Record(usage.Event{ID: "req_other", UserID: "usr_other", TokenID: "tok_other", Model: "qwen-coder", TotalTokens: 7, CreatedAt: now})
	req := newJSONRequest(http.MethodGet, "/api/portal/usage?range=all", "")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []usage.Event `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "req_user" {
		t.Fatalf("usage response = %#v", body.Data)
	}
}

func TestPortalDashboardAndModelsReturnPortalShape(t *testing.T) {
	srv := NewTestServer()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	srv.Usage.Record(usage.Event{ID: "req_user", UserID: "usr_dev", TokenID: "tok_dev", Model: "qwen-coder", TotalTokens: 11, LatencyMS: 22, CreatedAt: now})

	dashboardReq := newJSONRequest(http.MethodGet, "/api/portal/dashboard", "")
	dashboardRec := httptest.NewRecorder()
	srv.ServeHTTP(dashboardRec, dashboardReq)
	if dashboardRec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", dashboardRec.Code, dashboardRec.Body.String())
	}
	var dashboard struct {
		Metrics struct {
			Requests24h  int    `json:"requests_24h"`
			Tokens24h    int    `json:"tokens_24h"`
			HealthyHosts string `json:"healthy_hosts"`
			LatencyP95MS int64  `json:"latency_p95_ms"`
		} `json:"metrics"`
		Routes []struct {
			Model  string `json:"model"`
			Status string `json:"status"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(dashboardRec.Body.Bytes(), &dashboard); err != nil {
		t.Fatalf("Unmarshal dashboard returned %v", err)
	}
	if dashboard.Metrics.Requests24h != 1 || dashboard.Metrics.Tokens24h != 11 || dashboard.Metrics.LatencyP95MS != 22 {
		t.Fatalf("dashboard metrics = %#v", dashboard.Metrics)
	}
	if len(dashboard.Routes) != 1 || dashboard.Routes[0].Model != "qwen-coder" || dashboard.Routes[0].Status != "active" {
		t.Fatalf("dashboard routes = %#v", dashboard.Routes)
	}

	modelsReq := newJSONRequest(http.MethodGet, "/api/portal/models", "")
	modelsRec := httptest.NewRecorder()
	srv.ServeHTTP(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRec.Code, modelsRec.Body.String())
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modelsRec.Body.Bytes(), &models); err != nil {
		t.Fatalf("Unmarshal models returned %v", err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "qwen-coder" {
		t.Fatalf("models = %#v", models.Data)
	}
}

func TestPortalServersAndRoutesReturnRouteStoreData(t *testing.T) {
	srv := NewTestServer()
	serversReq := newJSONRequest(http.MethodGet, "/api/portal/servers", "")
	serversRec := httptest.NewRecorder()
	srv.ServeHTTP(serversRec, serversReq)
	if serversRec.Code != http.StatusOK {
		t.Fatalf("servers status = %d, body = %s", serversRec.Code, serversRec.Body.String())
	}
	routesReq := newJSONRequest(http.MethodGet, "/api/portal/routes", "")
	routesRec := httptest.NewRecorder()
	srv.ServeHTTP(routesRec, routesReq)
	if routesRec.Code != http.StatusNotFound {
		t.Fatalf("routes status = %d, want 404 (endpoint removed), body = %s", routesRec.Code, routesRec.Body.String())
	}
}

func TestPortalServersCreateListPatchDelete(t *testing.T) {
	// NewTestServerWithGroups: CreateServer now requires admin_group_ids to
	// reference an existing admin-tier group the caller manages (Phase B,
	// spec 2026-08-10); NewTestServer() has no Groups store wired at all.
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	// create
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers", `{"name":"GPU 1","domain":"gpu1.example.test","status":"active","owner_ids":["usr_dev"],"admin_group_ids":["`+testAdminGroupID+`"]}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Owners []struct {
			ID string `json:"id"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Name != "GPU 1" || created.Domain != "gpu1.example.test" || len(created.Owners) != 1 {
		t.Fatalf("created = %#v", created)
	}

	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, newJSONRequest(http.MethodGet, "/api/portal/servers", ""))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, item := range list.Data {
		if item.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created server not in list: %#v", list.Data)
	}

	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/servers/"+created.ID, `{"name":"GPU One"}`))
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRec.Code, patchRec.Body.String())
	}

	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, newJSONRequest(http.MethodDelete, "/api/portal/servers/"+created.ID, ""))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
	var delBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(delRec.Body.Bytes(), &delBody); err != nil {
		t.Fatalf("unmarshal delete: %v", err)
	}
	if !delBody.OK {
		t.Fatalf("delete body = %#v", delBody)
	}
}

func TestPortalServerCreateRejectsNonAdmin(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use"}) // non-admin token
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers", `{"name":"X","domain":"x.example.test"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != "server.forbidden" {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

func TestPortalServerItemUnknownReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/servers/srv_missing", `{"name":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != "server.not_found" {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

func TestPortalServerItemGet(t *testing.T) {
	// NewTestServerWithGroups: see TestPortalServersCreateListPatchDelete.
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers", `{"name":"GetMe","domain":"getme.example.test","owner_ids":["usr_dev"],"admin_group_ids":["`+testAdminGroupID+`"]}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, newJSONRequest(http.MethodGet, "/api/portal/servers/"+created.ID, ""))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var got struct{ Name, Domain string }
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if got.Name != "GetMe" || got.Domain != "getme.example.test" {
		t.Fatalf("got = %#v", got)
	}

	missRec := httptest.NewRecorder()
	srv.ServeHTTP(missRec, newJSONRequest(http.MethodGet, "/api/portal/servers/srv_missing", ""))
	if missRec.Code != http.StatusNotFound {
		t.Fatalf("missing get status = %d", missRec.Code)
	}
}

func TestPortalServerAgentTokenGenerateStatusRevoke(t *testing.T) {
	// system-scope: mock-host-qwen has no owner (seedGatewayTestRoutes) and no
	// Groups store is wired, so the Phase B group-scoped authorizeServer
	// rewrite (spec 2026-08-10) needs the unconditional system bypass here,
	// not the plain "admin" scope NewTestServer() otherwise defaults to.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	const path = "/api/portal/servers/mock-host-qwen/agent-token"

	// POST -> generate: 200 with a non-empty secret and token.exists=true.
	genRec := httptest.NewRecorder()
	srv.ServeHTTP(genRec, newJSONRequest(http.MethodPost, path, ""))
	if genRec.Code != http.StatusOK {
		t.Fatalf("generate status = %d, body = %s", genRec.Code, genRec.Body.String())
	}
	var gen struct {
		Secret string `json:"secret"`
		Token  struct {
			Exists       bool   `json:"exists"`
			SecretPrefix string `json:"secret_prefix"`
		} `json:"token"`
	}
	if err := json.Unmarshal(genRec.Body.Bytes(), &gen); err != nil {
		t.Fatalf("unmarshal generate: %v", err)
	}
	if gen.Secret == "" {
		t.Fatalf("expected non-empty secret, body = %s", genRec.Body.String())
	}
	if !gen.Token.Exists {
		t.Fatalf("expected token.exists=true, body = %s", genRec.Body.String())
	}

	// GET -> status: 200 exists=true with the same secret_prefix.
	statusRec := httptest.NewRecorder()
	srv.ServeHTTP(statusRec, newJSONRequest(http.MethodGet, path, ""))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status status = %d, body = %s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		Exists       bool   `json:"exists"`
		SecretPrefix string `json:"secret_prefix"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !status.Exists {
		t.Fatalf("expected exists=true after generate")
	}
	if status.SecretPrefix != gen.Token.SecretPrefix {
		t.Fatalf("secret_prefix mismatch: status=%q generate=%q", status.SecretPrefix, gen.Token.SecretPrefix)
	}

	// POST again -> rotate: 200 with a NEW secret over the HTTP layer.
	rotateRec := httptest.NewRecorder()
	srv.ServeHTTP(rotateRec, newJSONRequest(http.MethodPost, path, ""))
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotateRec.Code, rotateRec.Body.String())
	}
	var rotate struct {
		Secret string `json:"secret"`
		Token  struct {
			Exists bool `json:"exists"`
		} `json:"token"`
	}
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotate); err != nil {
		t.Fatalf("unmarshal rotate: %v", err)
	}
	if !rotate.Token.Exists {
		t.Fatalf("expected token.exists=true after rotate, body = %s", rotateRec.Body.String())
	}
	if rotate.Secret == "" || rotate.Secret == gen.Secret {
		t.Fatalf("expected rotate to return a new secret, first=%q rotated=%q", gen.Secret, rotate.Secret)
	}

	// DELETE -> revoke: 200.
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, newJSONRequest(http.MethodDelete, path, ""))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}

	// DELETE again -> revoke is idempotent at the HTTP layer: still 200.
	delAgainRec := httptest.NewRecorder()
	srv.ServeHTTP(delAgainRec, newJSONRequest(http.MethodDelete, path, ""))
	if delAgainRec.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, body = %s", delAgainRec.Code, delAgainRec.Body.String())
	}

	// GET -> exists=false after revoke.
	afterRec := httptest.NewRecorder()
	srv.ServeHTTP(afterRec, newJSONRequest(http.MethodGet, path, ""))
	if afterRec.Code != http.StatusOK {
		t.Fatalf("post-revoke status = %d, body = %s", afterRec.Code, afterRec.Body.String())
	}
	var after struct {
		Exists bool `json:"exists"`
	}
	if err := json.Unmarshal(afterRec.Body.Bytes(), &after); err != nil {
		t.Fatalf("unmarshal post-revoke: %v", err)
	}
	if after.Exists {
		t.Fatalf("expected exists=false after revoke, body = %s", afterRec.Body.String())
	}
}

func newMeshConfigResponseTestServer() (*Server, portal.GatewayMeshCertificateMaterial) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	material := portal.GatewayMeshCertificateMaterial{
		Domain:            "gateway.mesh.test",
		FullchainPEM:      "-----BEGIN CERTIFICATE-----\nPUBLIC-LEAF\n-----END CERTIFICATE-----\n",
		KeyPEM:            "-----BEGIN PRIVATE KEY-----\nGATEWAY-PRIVATE-KEY-SENTINEL\n-----END PRIVATE KEY-----\n",
		CABundlePEM:       "-----BEGIN CERTIFICATE-----\nPUBLIC-ROOT-BUNDLE\n-----END CERTIFICATE-----\n",
		Fingerprint:       strings.Repeat("a", 64),
		IssuerFingerprint: strings.Repeat("b", 64),
		NotAfter:          time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
	srv.Portal = &gatewayMeshMaterialPortal{
		API: srv.Portal, material: material, downloadOnly: true, dns: material.Domain,
	}
	srv.SetAgentListener(true, "100.64.0.10:9443")
	srv.SetAgentListenerTLSState(AgentListenerTLSState{
		Active: true, Address: "100.64.0.10:9443", Fingerprint: material.Fingerprint, NotAfter: material.NotAfter,
	})
	return srv, material
}

func TestAgentTokenResponsesAndDirectDownloadShareConfigMaterial(t *testing.T) {
	srv, material := newMeshConfigResponseTestServer()
	materialPortal := srv.Portal.(*gatewayMeshMaterialPortal)
	const tokenPath = "/api/portal/servers/mock-host-qwen/agent-token"

	genRec := httptest.NewRecorder()
	srv.ServeHTTP(genRec, newJSONRequest(http.MethodPost, tokenPath, ""))
	if genRec.Code != http.StatusOK {
		t.Fatalf("generate status = %d, want %d", genRec.Code, http.StatusOK)
	}
	if materialPortal.materialReads != 1 {
		t.Fatalf("generate material reads = %d, want exactly 1 coherent snapshot", materialPortal.materialReads)
	}
	var generated struct {
		Secret string `json:"secret"`
		Token  struct {
			Config            agentConfigMaterial `json:"config"`
			AgentDownloadBase string              `json:"agent_download_base"`
		} `json:"token"`
	}
	if err := json.Unmarshal(genRec.Body.Bytes(), &generated); err != nil {
		t.Fatalf("decode generate: %v", err)
	}

	statusRec := httptest.NewRecorder()
	srv.ServeHTTP(statusRec, newJSONRequest(http.MethodGet, tokenPath, ""))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", statusRec.Code, http.StatusOK)
	}
	if materialPortal.materialReads != 2 {
		t.Fatalf("GET material reads total = %d, want exactly 1 additional coherent snapshot", materialPortal.materialReads)
	}
	var status struct {
		Config            agentConfigMaterial `json:"config"`
		AgentDownloadBase string              `json:"agent_download_base"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	downloadReq := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/config", nil)
	downloadReq.Header.Set("Authorization", "Bearer "+generated.Secret)
	downloadRec := httptest.NewRecorder()
	srv.agentMux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want %d", downloadRec.Code, http.StatusOK)
	}
	if materialPortal.materialReads != 3 {
		t.Fatalf("direct download material reads total = %d, want exactly 1 additional snapshot", materialPortal.materialReads)
	}
	downloaded := parseAgentConfigJSONC(t, downloadRec.Body.String())

	want := agentConfigMaterial{
		GatewayURL:  "https://gateway.mesh.test:9443",
		CACacheFile: "server-agent-ca.pem",
		CAPEM:       material.CABundlePEM,
	}
	if generated.Token.Config != want {
		t.Fatalf("generate config = %+v, want %+v", generated.Token.Config, want)
	}
	if status.Config != want {
		t.Fatalf("status config = %+v, want %+v", status.Config, want)
	}
	if generated.Token.AgentDownloadBase != want.GatewayURL || status.AgentDownloadBase != want.GatewayURL {
		t.Fatalf("download bases = generate %q status %q, want %q", generated.Token.AgentDownloadBase, status.AgentDownloadBase, want.GatewayURL)
	}
	for key, value := range map[string]string{
		"gateway_url":   want.GatewayURL,
		"ca_file":       want.CAFile,
		"ca_cache_file": want.CACacheFile,
		"ca_pem":        want.CAPEM,
	} {
		if downloaded[key] != value {
			t.Errorf("downloaded %s = %v, want %q", key, downloaded[key], value)
		}
	}
}

func TestAgentTokenResponsesUsePublicDownloadOriginWithoutRestrictedListener(t *testing.T) {
	for _, tc := range []struct {
		name         string
		downloadOnly bool
	}{
		{name: "restriction off", downloadOnly: false},
		{name: "restricted but listener inactive", downloadOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAgentBinTestServerWithDownloadOnly(t, t.TempDir(), tc.downloadOnly)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/servers/mock-host-qwen/agent-token", ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var got struct {
				AgentDownloadBase string `json:"agent_download_base"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode token status: %v", err)
			}
			if got.AgentDownloadBase != "http://example.com" {
				t.Fatalf("agent_download_base = %q, want request origin", got.AgentDownloadBase)
			}
		})
	}
}

func TestAgentConfigResponsesNeverContainGatewayPrivateKey(t *testing.T) {
	srv, material := newMeshConfigResponseTestServer()
	const tokenPath = "/api/portal/servers/mock-host-qwen/agent-token"

	genRec := httptest.NewRecorder()
	srv.ServeHTTP(genRec, newJSONRequest(http.MethodPost, tokenPath, ""))
	if genRec.Code != http.StatusOK {
		t.Fatalf("generate status = %d, want %d", genRec.Code, http.StatusOK)
	}
	var generated struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(genRec.Body.Bytes(), &generated); err != nil {
		t.Fatalf("decode generate: %v", err)
	}
	statusRec := httptest.NewRecorder()
	srv.ServeHTTP(statusRec, newJSONRequest(http.MethodGet, tokenPath, ""))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", statusRec.Code, http.StatusOK)
	}
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/config", nil)
	downloadReq.Header.Set("Authorization", "Bearer "+generated.Secret)
	downloadRec := httptest.NewRecorder()
	srv.agentMux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want %d", downloadRec.Code, http.StatusOK)
	}

	for name, body := range map[string]string{
		"generate": genRec.Body.String(),
		"status":   statusRec.Body.String(),
		"download": downloadRec.Body.String(),
	} {
		if strings.Contains(body, material.KeyPEM) || strings.Contains(body, "GATEWAY-PRIVATE-KEY-SENTINEL") {
			t.Fatalf("%s response exposed gateway private-key material", name)
		}
		if !strings.Contains(body, "PUBLIC-ROOT-BUNDLE") {
			t.Fatalf("%s response omitted public CA material; no-leak assertion would be vacuous", name)
		}
	}
}

func TestAgentTokenEndpointRBAC(t *testing.T) {
	// Non-admin, non-owner principal (seeded servers have no owners).
	srv := NewTestServerWithTokenScopes([]string{"gateway:use"})
	const path = "/api/portal/servers/mock-host-qwen/agent-token"
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(method, path, ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, body = %s", method, rec.Code, rec.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error.Code != "server.not_found" {
			t.Fatalf("%s error code = %q", method, body.Error.Code)
		}
	}
}

func TestPortalTokenCreationReturnsOneTimeSecretAndNewTokenAuthenticates(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"Codex Local","scopes":["gateway:use"]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			SecretPrefix string   `json:"secret_prefix"`
			Scopes       []string `json:"scopes"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Secret == "" {
		t.Fatalf("secret is empty")
	}
	if body.Token.ID == "" || body.Token.Name != "Codex Local" || body.Token.SecretPrefix == "" {
		t.Fatalf("token = %#v", body.Token)
	}

	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello with created token"}]}`))
	chatReq.Header.Set("Authorization", "Bearer "+body.Secret)
	chatRec := httptest.NewRecorder()
	srv.ServeHTTP(chatRec, chatReq)

	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chatRec.Code, chatRec.Body.String())
	}
}

func TestPortalTokenCreationValidatesName(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"   "}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "portal.token_name_required" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalTokenCreationMapsConflictAndInternalErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "conflict", err: store.ErrConflict, want: http.StatusConflict, code: "portal.token_conflict"},
		{name: "internal", err: errors.New("entropy unavailable"), want: http.StatusInternalServerError, code: "portal.token_create_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := auth.NewTokenStore()
			tokens.AddPlainToken(auth.Token{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Active: true, Scopes: []string{"gateway:use"}}, "dev-secret")
			recorder := usage.NewRecorder()
			srv := New(ServerDeps{
				Tokens:   tokens,
				Usage:    recorder,
				Provider: provider.NewMock(),
				Portal: portal.NewService(portal.ServiceDeps{
					Tokens: failingTokens{err: tt.err},
					Usage:  recorder,
					SecretGenerator: func() (string, error) {
						return "created-secret", nil
					},
					IDGenerator: func() string {
						return "tok_created"
					},
				}),
			})
			req := newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"Codex Local"}`)
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if body.Error.Code != tt.code {
				t.Fatalf("error code = %q, want %s", body.Error.Code, tt.code)
			}
		})
	}
}

func TestPortalTokenCreationMapsScopePolicyErrors(t *testing.T) {
	tests := []struct {
		name        string
		ownerScopes []string
		body        string
		want        int
		code        string
	}{
		{name: "privileged scope forbidden", ownerScopes: []string{"gateway:use"}, body: `{"name":"Admin Token","scopes":["gateway:use","admin"]}`, want: http.StatusForbidden, code: "portal.token_scope_forbidden"},
		{name: "unknown scope invalid", ownerScopes: []string{"gateway:use", "admin"}, body: `{"name":"Unknown Token","scopes":["unknown:scope"]}`, want: http.StatusBadRequest, code: "portal.token_scope_invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewTestServerWithTokenScopes(tt.ownerScopes)
			req := newJSONRequest(http.MethodPost, "/api/portal/tokens", tt.body)
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if body.Error.Code != tt.code {
				t.Fatalf("error code = %q, want %s", body.Error.Code, tt.code)
			}
		})
	}
}

func TestModelsRequireBearerTokenAndReturnModelList(t *testing.T) {
	tests := []string{
		"/openai/v1/models",
		"/v1/models",
		"/anthropic/v1/models",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			srv := NewTestServer()
			unauthenticated := httptest.NewRequest(http.MethodGet, path, nil)
			unauthenticatedRec := httptest.NewRecorder()

			srv.ServeHTTP(unauthenticatedRec, unauthenticated)

			if unauthenticatedRec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status = %d, want 401", unauthenticatedRec.Code)
			}

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if len(body.Data) == 0 {
				t.Fatalf("models list is empty")
			}
			if body.Data[0].ID != "qwen-coder" {
				t.Fatalf("first model = %q, want qwen-coder", body.Data[0].ID)
			}
		})
	}
}

// newModelsManageTestServer builds a server whose route store carries one
// OFFERABLE gateway model ("secret-model") flagged hidden, plus a token with the
// given scopes, so the ?manage=1 admin gate can be exercised.
func newModelsManageTestServer(t *testing.T, scopes []string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	ctx := context.Background()
	// An OFFERABLE gateway model that model_settings marks hidden.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_secret", Name: "SecretBox", Domain: "secret.example.test", Provider: routing.ProviderMock, Endpoint: "mock://secret", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_secret", ServerID: "srv_secret", Type: routing.ProviderMock, Port: 8200, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_secret", ApplicationID: "app_secret", GatewayModelName: "secret-model", AppModelName: "secret-up", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := routeStore.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "secret-model", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert model setting: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// TestPortalModelsManageGate: ?manage=1 as an admin returns the UNSUPPRESSED
// listing (the hidden model is present); the default path and a non-admin caller
// (even with ?manage=1) both get the SUPPRESSED listing (hidden model absent).
func TestPortalModelsManageGate(t *testing.T) {
	modelPresent := func(t *testing.T, srv *Server, path string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID         string `json:"id"`
				Visibility string `json:"visibility"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, m := range body.Data {
			if m.ID == "secret-model" {
				if m.Visibility != "hidden" {
					t.Fatalf("secret-model visibility = %q, want hidden", m.Visibility)
				}
				return true
			}
		}
		return false
	}

	admin := newModelsManageTestServer(t, []string{"gateway:use", "admin"})
	if modelPresent(t, admin, "/api/portal/models") {
		t.Fatalf("default (suppressed) listing must NOT include the hidden model")
	}
	if !modelPresent(t, admin, "/api/portal/models?manage=1") {
		t.Fatalf("admin ?manage=1 must include the hidden model")
	}
	if !modelPresent(t, admin, "/api/portal/models?manage=true") {
		t.Fatalf("admin ?manage=true must include the hidden model")
	}

	nonAdmin := newModelsManageTestServer(t, []string{"gateway:use"})
	if modelPresent(t, nonAdmin, "/api/portal/models?manage=1") {
		t.Fatalf("non-admin ?manage=1 must be ignored (suppressed listing, no hidden model)")
	}
}

func TestOpenAIModelsReturnOpenAIShape(t *testing.T) {
	tests := []string{
		"/openai/v1/models",
		"/v1/models",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			srv := NewTestServer()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID      string `json:"id"`
					Object  string `json:"object"`
					OwnedBy string `json:"owned_by"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal returned %v", err)
			}
			if body.Object != "list" {
				t.Fatalf("object = %q, want list", body.Object)
			}
			if len(body.Data) == 0 {
				t.Fatalf("models list is empty")
			}
			if body.Data[0].Object != "model" {
				t.Fatalf("model object = %q, want model", body.Data[0].Object)
			}
			if body.Data[0].OwnedBy != "op-ai-gateway" {
				t.Fatalf("owned_by = %q, want op-ai-gateway", body.Data[0].OwnedBy)
			}
		})
	}
}

func TestAnthropicModelsReturnAnthropicShape(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if len(body.Data) == 0 {
		t.Fatalf("models list is empty")
	}
	if body.Data[0].ID != "qwen-coder" {
		t.Fatalf("id = %q, want qwen-coder", body.Data[0].ID)
	}
	if body.Data[0].Type != "model" {
		t.Fatalf("type = %q, want model", body.Data[0].Type)
	}
	if body.Data[0].DisplayName == "" {
		t.Fatalf("display_name is empty")
	}
	if body.Data[0].CreatedAt == "" {
		t.Fatalf("created_at is empty")
	}
}

func TestDiscoveryEndpointsFilterByFlavor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	scopesJSON, _ := json.Marshal([]string{"gateway:use", "admin"})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_o", Name: "O", Domain: "o.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("server o: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_o", ServerID: "srv_o", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("app o: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_o", ApplicationID: "app_o", GatewayModelName: "openai-model", AppModelName: "openai-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("map o: %v", err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_a", Name: "A", Domain: "a.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("server a: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_a", ServerID: "srv_a", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorAnthropic}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("app a: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_a", ApplicationID: "app_a", GatewayModelName: "anthropic-model", AppModelName: "anthropic-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("map a: %v", err)
	}
	recorder := usage.NewRecorder()
	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})

	ids := func(path string) []string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s unmarshal: %v", path, err)
		}
		out := make([]string, 0, len(body.Data))
		for _, m := range body.Data {
			out = append(out, m.ID)
		}
		return out
	}

	if got := ids("/v1/models"); !reflect.DeepEqual(got, []string{"openai-model"}) {
		t.Fatalf("/v1/models = %#v, want [openai-model]", got)
	}
	if got := ids("/anthropic/v1/models"); !reflect.DeepEqual(got, []string{"anthropic-model"}) {
		t.Fatalf("/anthropic/v1/models = %#v, want [anthropic-model]", got)
	}
}

func TestHealthzReturnsOKWithoutAuth(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if !body.OK {
		t.Fatalf("ok = false, want true")
	}
	if len(srv.Usage.All()) != 0 {
		t.Fatalf("usage events = %d, want 0", len(srv.Usage.All()))
	}
}

func TestInvalidJSONReturnsStableAPIError(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", body.Error.Code)
	}
	if len(srv.Usage.All()) != 0 {
		t.Fatalf("usage events = %d, want 0", len(srv.Usage.All()))
	}
}

func TestUnknownPathReturnsStableJSONNotFound(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", rec.Header().Get("Content-Type"))
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "request.not_found" {
		t.Fatalf("error code = %q, want request.not_found", body.Error.Code)
	}
}

func TestUnsupportedMethodReturnsMethodNotAllowed(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestProviderErrorRecordsUsageAndReturnsBadGateway(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/openai/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`)
	ctx := req.Context()
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Status != "error" {
		t.Fatalf("Status = %q, want error", events[0].Status)
	}
	if events[0].ErrorCode != "provider.unavailable" {
		t.Fatalf("ErrorCode = %q, want provider.unavailable", events[0].ErrorCode)
	}
}

func TestAdminHostsEndpointRemovedReturns404(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodGet, "/api/admin/hosts", "")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "request.not_found" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestAdminRoutesEndpointRemovedReturns404(t *testing.T) {
	srv := NewTestServer()
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/admin/routes", ""},
		{http.MethodPost, "/api/admin/routes", `{"id":"route_admin","model":"admin-model","host_id":"host_admin","provider_model":"admin-model","api_flavors":["openai"]}`},
		{http.MethodPatch, "/api/admin/routes/route_admin", `{"provider_model":"updated"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newJSONRequest(tc.method, tc.path, tc.body))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body.Error.Code != "request.not_found" {
				t.Fatalf("error code = %q", body.Error.Code)
			}
		})
	}
}

// newAgentTelemetryRequest builds a telemetry POST authenticated with a raw
// agent-token secret (not a user bearer).
func newAgentTelemetryRequest(secret string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/telemetry", bytes.NewBufferString(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req
}

func seedTestAgentToken(t *testing.T, srv *Server, id string, serverID string, secret string) {
	t.Helper()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.UpsertAgentToken(context.Background(), routing.AgentToken{ID: id, ServerID: serverID, SecretPrefix: "opaigw_", CreatedAt: now, UpdatedAt: now}, auth.HashSecret(secret)); err != nil {
		t.Fatalf("UpsertAgentToken: %v", err)
	}
}

func assertAgentTelemetryError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error code = %q, body = %s", body.Error.Code, rec.Body.String())
	}
}

func TestTelemetryIntakeBoundToAgentToken(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")
	// Body carries NO server_id: the intake derives the target from the token.
	// A distinct reported_at (2026-07-11) proves the row was updated.
	req := newAgentTelemetryRequest("agent-secret", `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accepted bool   `json:"accepted"`
		ServerID string `json:"server_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if !body.Accepted {
		t.Fatalf("accepted = false")
	}
	if body.ServerID != "mock-host-qwen" {
		t.Fatalf("server_id = %q, want mock-host-qwen", body.ServerID)
	}

	want := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	telemetry, ok, err := srv.Routes.TelemetryByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer ok=%v err=%v", ok, err)
	}
	if !telemetry.ReportedAt.Equal(want) {
		t.Fatalf("telemetry reported_at = %v, want %v", telemetry.ReportedAt, want)
	}
	server, err := srv.Routes.AIServerByID(context.Background(), "mock-host-qwen")
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if server.LastSeenAt == nil || !server.LastSeenAt.Equal(want) {
		t.Fatalf("server.LastSeenAt = %v, want %v", server.LastSeenAt, want)
	}
}

func TestTelemetryIntakeRejectsMissingAndMismatch(t *testing.T) {
	// A former agent:report user token exists on this server but no longer
	// authenticates the intake (only per-server agent tokens do).
	srv := NewTestServerWithTokenScopes([]string{"agent:report"})
	seedTestAgentToken(t, srv, "agt_a", "mock-host-qwen", "agent-secret")
	body := `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`

	// No bearer at all -> 401.
	noAuthRec := httptest.NewRecorder()
	srv.ServeHTTP(noAuthRec, newAgentTelemetryRequest("", body))
	assertAgentTelemetryError(t, noAuthRec, http.StatusUnauthorized, "auth.invalid_token")

	// Garbage bearer (no matching agent token) -> 401.
	garbageRec := httptest.NewRecorder()
	srv.ServeHTTP(garbageRec, newAgentTelemetryRequest("not-a-real-secret", body))
	assertAgentTelemetryError(t, garbageRec, http.StatusUnauthorized, "auth.invalid_token")

	// Token bound to server A, body claims server B -> 403 mismatch.
	mismatchBody := `{"server_id":"mock-host-comp","agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`
	mismatchRec := httptest.NewRecorder()
	srv.ServeHTTP(mismatchRec, newAgentTelemetryRequest("agent-secret", mismatchBody))
	assertAgentTelemetryError(t, mismatchRec, http.StatusForbidden, "agent.server_mismatch")

	// Former user bearer that still carries agent:report scope but has no agent
	// token -> 401 (scope no longer grants intake access).
	userRec := httptest.NewRecorder()
	srv.ServeHTTP(userRec, newAgentTelemetryRequest("dev-secret", body))
	assertAgentTelemetryError(t, userRec, http.StatusUnauthorized, "auth.invalid_token")
}

func TestTelemetryIntakePersistsRichSampleAndFansOut(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")

	// Subscribe before the POST so the fanned-out sample is captured.
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()

	// Rich body: no server_id (token-derived), a host section + two GPUs + one nic.
	body := `{
		"agent_version":"0.1.0",
		"reported_at":"2026-07-11T09:30:00Z",
		"os":"linux","arch":"amd64",
		"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,
		"provider_health":{},"capabilities":{},
		"host":{
			"cpu_util_pct":42,
			"mem_used_bytes":8000000000,"mem_total_bytes":16000000000,
			"swap_used_bytes":0,"swap_total_bytes":0,
			"load1":1.5,"load5":1.2,"load15":1.0,
			"net":[{"name":"eth0","rx_bytes":1000,"tx_bytes":2000}]
		},
		"gpus":[
			{"index":0,"name":"RTX 4090","uuid":"gpu-uuid-0","util_pct":88,"mem_used_bytes":1000,"mem_total_bytes":2000,"temp_c":71,"vram_temp_c":80,"power_w":320.5,"fan_pct":55},
			{"index":1,"name":"RTX 4090","uuid":"gpu-uuid-1","util_pct":77,"mem_used_bytes":3000,"mem_total_bytes":4000,"temp_c":69,"vram_temp_c":78,"power_w":300,"fan_pct":50}
		]
	}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("agent-secret", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || !accepted.Accepted {
		t.Fatalf("accepted = %v err = %v body = %s", accepted.Accepted, err, rec.Body.String())
	}

	ctx := context.Background()
	from := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	samples, err := srv.Routes.TelemetrySamples(ctx, "mock-host-qwen", from, to, 100)
	if err != nil {
		t.Fatalf("TelemetrySamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("TelemetrySamples len = %d, want 1", len(samples))
	}
	got := samples[0]
	if got.CPUUtilPct != 42 {
		t.Fatalf("sample CPUUtilPct = %v, want 42", got.CPUUtilPct)
	}
	if got.MemUsedBytes != 8000000000 {
		t.Fatalf("sample MemUsedBytes = %d, want 8000000000", got.MemUsedBytes)
	}
	if len(got.GPUs) != 2 || got.GPUs[0].UUID != "gpu-uuid-0" || got.GPUs[0].TempC != 71 {
		t.Fatalf("sample GPUs = %+v, want [gpu-uuid-0 temp 71, ...]", got.GPUs)
	}
	if len(got.Net) != 1 || got.Net[0].RxBytes != 1000 {
		t.Fatalf("sample Net = %+v, want [eth0 rx 1000]", got.Net)
	}

	// Routing summary is derived from the rich section (unchanged behavior).
	telemetry, ok, err := srv.Routes.TelemetryByServer(ctx, "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer ok=%v err=%v", ok, err)
	}
	if telemetry.CPULoad != 0.42 {
		t.Fatalf("telemetry CPULoad = %v, want 0.42", telemetry.CPULoad)
	}
	if telemetry.VRAMUsedBytes != 4000 || telemetry.VRAMTotalBytes != 6000 || telemetry.GPUCount != 2 {
		t.Fatalf("telemetry VRAM used/total/count = %d/%d/%d, want 4000/6000/2", telemetry.VRAMUsedBytes, telemetry.VRAMTotalBytes, telemetry.GPUCount)
	}

	// The live subscriber received the fanned-out sample.
	select {
	case fanned := <-ch:
		if fanned.ServerID != "mock-host-qwen" || fanned.CPUUtilPct != 42 {
			t.Fatalf("fanned sample = %+v, want mock-host-qwen cpu 42", fanned)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the fanned-out sample")
	}
}

func TestTelemetryIntakeLegacyPayloadStillWorks(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "agent-secret")

	// The existing legacy-only body (no host/gpus) must still be accepted.
	body := `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newAgentTelemetryRequest("agent-secret", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	telemetry, ok, err := srv.Routes.TelemetryByServer(ctx, "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer ok=%v err=%v", ok, err)
	}
	if telemetry.CPULoad != 0.4 {
		t.Fatalf("telemetry CPULoad = %v, want 0.4 (legacy verbatim)", telemetry.CPULoad)
	}

	from := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	samples, err := srv.Routes.TelemetrySamples(ctx, "mock-host-qwen", from, to, 100)
	if err != nil {
		t.Fatalf("TelemetrySamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("TelemetrySamples len = %d, want 1", len(samples))
	}
	if len(samples[0].GPUs) != 0 || len(samples[0].Net) != 0 {
		t.Fatalf("legacy sample GPUs/Net = %+v/%+v, want empty", samples[0].GPUs, samples[0].Net)
	}
}

func TestAgentTelemetryRawSummaryExcludesUnknownSensitiveFields(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_raw", "mock-host-qwen", "agent-secret")
	req := newAgentTelemetryRequest("agent-secret", `{"agent_version":"0.1.0","reported_at":"2026-07-10T12:00:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{"status":"ok"},"capabilities":{"models":["qwen-coder"]},"prompt":"do not store me","api_token":"secret-token"}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	telemetry, ok, err := srv.Routes.TelemetryByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer ok=%v err=%v", ok, err)
	}
	if strings.Contains(telemetry.RawSummary, "do not store me") || strings.Contains(telemetry.RawSummary, "secret-token") {
		t.Fatalf("RawSummary leaked sensitive fields: %s", telemetry.RawSummary)
	}
	if !strings.Contains(telemetry.RawSummary, `"server_id":"mock-host-qwen"`) {
		t.Fatalf("RawSummary missing allowed server_id: %s", telemetry.RawSummary)
	}
}

func TestAgentTelemetryRequestParsesRichFields(t *testing.T) {
	payload := `{
		"server_id":"mock-host-qwen",
		"agent_version":"0.1.0",
		"reported_at":"2026-07-11T09:30:00Z",
		"os":"linux",
		"arch":"amd64",
		"cpu_load":0.4,
		"active_requests":2,
		"queue_depth":1,
		"latency_ms":120,
		"error_rate":0.01,
		"provider_health":{},
		"capabilities":{},
		"host":{
			"cpu_util_pct":37.5,
			"mem_used_bytes":8000000000,
			"mem_total_bytes":16000000000,
			"swap_used_bytes":100000000,
			"swap_total_bytes":2000000000,
			"load1":1.5,
			"load5":1.2,
			"load15":0.9,
			"net":[{"name":"eth0","rx_bytes":1000,"tx_bytes":2000}]
		},
		"gpus":[{
			"index":0,
			"name":"RTX 4090",
			"uuid":"gpu-uuid-0",
			"util_pct":88.0,
			"mem_used_bytes":12000000000,
			"mem_total_bytes":24000000000,
			"temp_c":71,
			"vram_temp_c":80,
			"power_w":320.5,
			"fan_pct":55.0
		}]
	}`

	var req agentTelemetryRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if req.Host == nil {
		t.Fatalf("req.Host = nil, want a host report")
	}
	if req.Host.CPUUtilPct != 37.5 {
		t.Fatalf("req.Host.CPUUtilPct = %v, want 37.5", req.Host.CPUUtilPct)
	}
	if len(req.Host.Net) != 1 {
		t.Fatalf("len(req.Host.Net) = %d, want 1", len(req.Host.Net))
	}
	if req.Host.Net[0].RxBytes != 1000 {
		t.Fatalf("req.Host.Net[0].RxBytes = %d, want 1000", req.Host.Net[0].RxBytes)
	}
	if len(req.GPUs) != 1 {
		t.Fatalf("len(req.GPUs) = %d, want 1", len(req.GPUs))
	}
	if req.GPUs[0].UUID != "gpu-uuid-0" {
		t.Fatalf("req.GPUs[0].UUID = %q, want gpu-uuid-0", req.GPUs[0].UUID)
	}
	if req.GPUs[0].PowerW != 320.5 {
		t.Fatalf("req.GPUs[0].PowerW = %v, want 320.5", req.GPUs[0].PowerW)
	}
}

func TestTelemetrySampleFromRequestMapsRichFields(t *testing.T) {
	reported := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	req := agentTelemetryRequest{
		ServerID:       "mock-host-qwen",
		ReportedAt:     reported,
		ActiveRequests: 2,
		QueueDepth:     1,
		Host: &agentHostReport{
			CPUUtilPct:     37.5,
			MemUsedBytes:   8000000000,
			MemTotalBytes:  16000000000,
			SwapUsedBytes:  100000000,
			SwapTotalBytes: 2000000000,
			Load1:          1.5,
			Load5:          1.2,
			Load15:         0.9,
			Net:            []agentNetReport{{Name: "eth0", RxBytes: 1000, TxBytes: 2000}},
		},
		GPUs: []agentGPUReport{{
			Index:         0,
			Name:          "RTX 4090",
			UUID:          "gpu-uuid-0",
			UtilPct:       88,
			MemUsedBytes:  12000000000,
			MemTotalBytes: 24000000000,
			TempC:         71,
			VRAMTempC:     80,
			PowerW:        320.5,
			FanPct:        55,
		}},
	}

	sample, err := telemetrySampleFromRequest(req, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetrySampleFromRequest returned %v", err)
	}
	if sample.ServerID != "mock-host-qwen" {
		t.Fatalf("ServerID = %q, want mock-host-qwen", sample.ServerID)
	}
	if !sample.ReportedAt.Equal(reported) {
		t.Fatalf("ReportedAt = %v, want %v", sample.ReportedAt, reported)
	}
	if sample.CPUUtilPct != 37.5 {
		t.Fatalf("CPUUtilPct = %v, want 37.5", sample.CPUUtilPct)
	}
	if sample.MemUsedBytes != 8000000000 {
		t.Fatalf("MemUsedBytes = %d, want 8000000000", sample.MemUsedBytes)
	}
	if sample.Load1 != 1.5 {
		t.Fatalf("Load1 = %v, want 1.5", sample.Load1)
	}
	if sample.ActiveRequests != 2 || sample.QueueDepth != 1 {
		t.Fatalf("ActiveRequests/QueueDepth = %d/%d, want 2/1", sample.ActiveRequests, sample.QueueDepth)
	}
	if len(sample.GPUs) != 1 {
		t.Fatalf("len(GPUs) = %d, want 1", len(sample.GPUs))
	}
	if sample.GPUs[0].UUID != "gpu-uuid-0" {
		t.Fatalf("GPUs[0].UUID = %q, want gpu-uuid-0", sample.GPUs[0].UUID)
	}
	if len(sample.Net) != 1 {
		t.Fatalf("len(Net) = %d, want 1", len(sample.Net))
	}
}

func TestTelemetrySampleFromRequestSanitizesCPUCores(t *testing.T) {
	req := agentTelemetryRequest{
		ServerID: "srv",
		Host: &agentHostReport{
			CPUUtilPct: 40,
			// A bad core reading must be clamped/zeroed, not reject the sample.
			CPUCores: []float64{10, 120, math.NaN(), math.Inf(1), -5, 99.5},
		},
	}
	sample, err := telemetrySampleFromRequest(req, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetrySampleFromRequest returned %v", err)
	}
	want := []float64{10, 100, 0, 0, 0, 99.5}
	if len(sample.CPUCores) != len(want) {
		t.Fatalf("len(CPUCores) = %d, want %d", len(sample.CPUCores), len(want))
	}
	for i, w := range want {
		if sample.CPUCores[i] != w {
			t.Errorf("CPUCores[%d] = %v, want %v", i, sample.CPUCores[i], w)
		}
	}
	// The DTO must serialize a non-nil slice for both the SSE and DB-history paths.
	dto := perfPointFromSample(sample)
	if dto.CPUCores == nil {
		t.Fatal("perfPointDTO.CPUCores is nil, want non-nil []")
	}
	// A legacy sample with no cpu_cores → DTO carries an empty (non-nil) slice.
	if got := perfPointFromSample(routing.TelemetrySample{}); got.CPUCores == nil {
		t.Fatal("perfPointDTO.CPUCores from an empty sample is nil, want []")
	}
}

func TestTelemetrySampleFromRequestRejectsNegative(t *testing.T) {
	cases := map[string]agentTelemetryRequest{
		"negative host mem": {
			ServerID: "srv1",
			Host:     &agentHostReport{MemUsedBytes: -1},
		},
		"gpu util NaN": {
			ServerID: "srv1",
			GPUs:     []agentGPUReport{{UtilPct: math.NaN()}},
		},
	}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			if _, err := telemetrySampleFromRequest(req, time.Now().UTC()); err == nil {
				t.Fatalf("telemetrySampleFromRequest(%s) error = nil, want non-nil", name)
			}
		})
	}
}

func TestTelemetrySampleFromRequestNoHostIsEmpty(t *testing.T) {
	req := agentTelemetryRequest{
		ServerID:       "srv1",
		ActiveRequests: 3,
		QueueDepth:     0,
		CPULoad:        0.4,
	}

	sample, err := telemetrySampleFromRequest(req, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetrySampleFromRequest returned %v", err)
	}
	if sample.CPUUtilPct != 0 || sample.MemUsedBytes != 0 || sample.Load1 != 0 {
		t.Fatalf("host scalars not zeroed: %+v", sample)
	}
	if sample.GPUs == nil || len(sample.GPUs) != 0 {
		t.Fatalf("GPUs = %v, want empty non-nil slice", sample.GPUs)
	}
	if sample.Net == nil || len(sample.Net) != 0 {
		t.Fatalf("Net = %v, want empty non-nil slice", sample.Net)
	}
	if sample.ActiveRequests != 3 {
		t.Fatalf("ActiveRequests = %d, want 3", sample.ActiveRequests)
	}
}

func TestTelemetryFromRequestDerivesSummaryFromRich(t *testing.T) {
	req := agentTelemetryRequest{
		ServerID: "srv1",
		Host: &agentHostReport{
			CPUUtilPct:    42.0,
			MemUsedBytes:  8000000000,
			MemTotalBytes: 16000000000,
		},
		GPUs: []agentGPUReport{
			{Index: 0, MemUsedBytes: 1000, MemTotalBytes: 2000},
			{Index: 1, MemUsedBytes: 3000, MemTotalBytes: 4000},
		},
	}

	telemetry, err := telemetryFromRequest(req, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetryFromRequest returned %v", err)
	}
	if telemetry.CPULoad != 0.42 {
		t.Fatalf("CPULoad = %v, want 0.42", telemetry.CPULoad)
	}
	if telemetry.VRAMUsedBytes != 4000 {
		t.Fatalf("VRAMUsedBytes = %d, want 4000", telemetry.VRAMUsedBytes)
	}
	if telemetry.VRAMTotalBytes != 6000 {
		t.Fatalf("VRAMTotalBytes = %d, want 6000", telemetry.VRAMTotalBytes)
	}
	if telemetry.GPUCount != 2 {
		t.Fatalf("GPUCount = %d, want 2", telemetry.GPUCount)
	}
	if telemetry.RAMUsedBytes != 8000000000 {
		t.Fatalf("RAMUsedBytes = %d, want 8000000000", telemetry.RAMUsedBytes)
	}
	if telemetry.RAMTotalBytes != 16000000000 {
		t.Fatalf("RAMTotalBytes = %d, want 16000000000", telemetry.RAMTotalBytes)
	}
}

func TestTelemetryFromRequestLegacyOnlyUnchanged(t *testing.T) {
	req := agentTelemetryRequest{
		ServerID:      "srv1",
		CPULoad:       0.4,
		VRAMUsedBytes: 500,
	}

	telemetry, err := telemetryFromRequest(req, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetryFromRequest returned %v", err)
	}
	if telemetry.CPULoad != 0.4 {
		t.Fatalf("CPULoad = %v, want 0.4 (verbatim)", telemetry.CPULoad)
	}
	if telemetry.VRAMUsedBytes != 500 {
		t.Fatalf("VRAMUsedBytes = %d, want 500 (verbatim)", telemetry.VRAMUsedBytes)
	}
	if telemetry.GPUCount != 0 {
		t.Fatalf("GPUCount = %d, want 0 (no rich section)", telemetry.GPUCount)
	}
}

func createTestApplication(t *testing.T, srv *Server, serverID string, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create application status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal application: %v", err)
	}
	return created.ID
}

func TestPortalApplicationsCreateListPatchDelete(t *testing.T) {
	// system-scope: mock-host-qwen has no owner and no Groups store is wired
	// (see TestPortalServerAgentTokenGenerateStatusRevoke for the Phase B
	// rationale).
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/mock-host-qwen/applications", `{"type":"vllm","port":8000,"scheme":"https"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		ServerID string `json:"server_id"`
		Port     int    `json:"port"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ServerID != "mock-host-qwen" || created.Port != 8000 || created.Endpoint == "" {
		t.Fatalf("created = %#v", created)
	}

	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, newJSONRequest(http.MethodGet, "/api/portal/servers/mock-host-qwen/applications", ""))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, item := range list.Data {
		if item.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created application not in list: %#v", list.Data)
	}

	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID, `{"port":8001}`))
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRec.Code, patchRec.Body.String())
	}
	var patched struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patched.Port != 8001 {
		t.Fatalf("patched port = %d, want 8001", patched.Port)
	}

	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, newJSONRequest(http.MethodDelete, "/api/portal/applications/"+created.ID, ""))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
	var delBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(delRec.Body.Bytes(), &delBody); err != nil {
		t.Fatalf("unmarshal delete: %v", err)
	}
	if !delBody.OK {
		t.Fatalf("delete body = %#v", delBody)
	}
}

func TestPortalApplicationCreateDuplicatePortReturnsConflict(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8010,"scheme":"https"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/mock-host-qwen/applications", `{"type":"vllm","port":8010,"scheme":"https"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "application.port_conflict" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalApplicationCreateRejectsNegativeTuningReturns400(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/mock-host-qwen/applications", `{"type":"vllm","port":8011,"scheme":"https","priority":-1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "application.tuning_invalid" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalApplicationItemUnknownReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, "/api/portal/applications/app_missing", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "application.not_found" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalApplicationSyncModelsCreatesMappings(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8020,"scheme":"https"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/sync-models", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var summary struct {
		Added      int `json:"added"`
		Disabled   int `json:"disabled"`
		Unchanged  int `json:"unchanged"`
		Conflicted int `json:"conflicted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if summary.Added != 2 {
		t.Fatalf("summary = %#v, want added=2", summary)
	}

	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, newJSONRequest(http.MethodGet, "/api/portal/applications/"+appID+"/mappings", ""))
	if listRec.Code != http.StatusOK {
		t.Fatalf("mappings list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Data []struct {
			GatewayModelName string `json:"gateway_model_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("mappings = %#v, want 2", list.Data)
	}
}

func TestPortalMappingsCreatePatchDelete(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8030,"scheme":"https"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/mappings", `{"gateway_model_name":"qwen","app_model_name":"qwen2.5"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create mapping status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created mapping id is empty")
	}

	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/mappings/"+created.ID, `{"status":"disabled"}`))
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRec.Code, patchRec.Body.String())
	}
	var patched struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patched.Status != "disabled" {
		t.Fatalf("patched status = %q, want disabled", patched.Status)
	}

	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, newJSONRequest(http.MethodDelete, "/api/portal/mappings/"+created.ID, ""))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
	var delBody struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(delRec.Body.Bytes(), &delBody); err != nil {
		t.Fatalf("unmarshal delete: %v", err)
	}
	if !delBody.OK {
		t.Fatalf("delete body = %#v", delBody)
	}
}

// TestPortalMappingCreateNegativeMetricReturns400 guards that
// portal.ErrMappingMetricInvalid is mapped to a 400 (not a default 500) by the
// HTTP error writer, exercised through the real ServeHTTP mapping-create path.
func TestPortalMappingCreateNegativeMetricReturns400(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8030,"scheme":"https"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/mappings", `{"gateway_model_name":"qwen","app_model_name":"qwen2.5","load_time_ms":-5}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative metric status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if body.Error.Code != "mapping.metric_invalid" {
		t.Fatalf("error code = %q, want mapping.metric_invalid (body = %s)", body.Error.Code, rec.Body.String())
	}
}

func TestPortalMappingItemUnknownReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/mappings/map_missing", `{"status":"disabled"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "mapping.not_found" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func newJSONRequest(method string, path string, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	return req
}

// TestServerHasPerfRegistry proves gateway.New defaults a nil deps.ServerPerf to
// a fresh registry, so every Server (incl. a bare NewTestServer that leaves it
// unset) carries a usable per-server telemetry ring for the ingest publish path
// and the /perf SSE endpoints.
func TestServerHasPerfRegistry(t *testing.T) {
	if NewTestServer().ServerPerf == nil {
		t.Fatal("NewTestServer().ServerPerf = nil, want a non-nil registry (New must default it)")
	}
}

func NewTestServer() *Server {
	return NewTestServerWithTokenScopes([]string{"gateway:use", "admin"})
}

func NewTestServerWithTokenScopes(scopes []string) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		panic(err)
	}
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// testAdminGroupID is the fixed id of the admin-tier group
// NewTestServerWithGroups seeds, owned by usr_dev -- Phase B (spec
// 2026-08-10) requires CreateServer's admin_group_ids to reference an
// existing admin-tier group the (non-system) caller manages.
const testAdminGroupID = "ugrp_gwtest_admin"

// NewTestServerWithGroups mirrors NewTestServerWithTokenScopes but ALSO
// wires a Groups store (portal.MemoryDirectory implements GroupStore) and
// seeds a system/admin group pair owned by + membered by usr_dev
// (testAdminGroupID), so a bearer-token test exercising the server WRITE
// path (POST /api/portal/servers, PUT .../admin-groups) can satisfy the
// Phase B admin-group-linkage gate/requirement without every other
// NewTestServer()-based test (which has no Groups store) needing it.
func NewTestServerWithGroups(scopes []string) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		panic(err)
	}
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	sysGroupID := "ugrp_gwtest_sys"
	if err := directory.CreateUserGroup(context.Background(), store.UserGroup{ID: sysGroupID, Tier: store.GroupTierSystem, Name: "GW Test System", CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := directory.CreateUserGroup(context.Background(), store.UserGroup{ID: testAdminGroupID, Tier: store.GroupTierAdmin, Name: "GW Test Admin", ParentGroupID: sysGroupID, OwnerUserID: "usr_dev", CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := directory.SetUserGroupMember(context.Background(), testAdminGroupID, "usr_dev", store.GroupStateMember, ""); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Groups: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// newStreamTestServerWithProvider mirrors NewTestServerWithTokenScopes but swaps
// in a custom provider, so streaming tests can drive a stub StreamingClient.
func newStreamTestServerWithProvider(prov provider.Client) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// newStreamTestServerWithProviderAndIdle mirrors newStreamTestServerWithProvider but
// sets an explicit stream idle timeout so watchdog behavior can be exercised.
func newStreamTestServerWithProviderAndIdle(prov provider.Client, idle time.Duration) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:            tokens,
		Usage:             recorder,
		Provider:          prov,
		Routes:            routeStore,
		Portal:            portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		StreamIdleTimeout: idle,
	})
}

func seedGatewayTestRoutes(routeStore *routing.MemoryStore, now time.Time) {
	ctx := context.Background()
	// "mock-host-qwen" backs the application/mapping/telemetry API tests.
	// Deliberately no application/mapping is seeded on this server so the
	// sync-models test can map "qwen-coder" here without a pre-existing
	// server-scoped conflict.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "mock-host-qwen", Name: "Mock Qwen", Domain: "qwen.example.test", Provider: routing.ProviderMock, Endpoint: "mock://qwen", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "mock-host-qwen", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		panic(err)
	}
	// "mock-host-comp" carries the application + active mapping the resolver
	// actually routes completions through. The mapping id is "route_mock_qwen"
	// so usage attribution (Target.RouteID) matches the existing expectation.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "mock-host-comp", Name: "Mock Completion", Domain: "comp.example.test", Provider: routing.ProviderMock, Endpoint: "mock://comp", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_mock_comp", ServerID: "mock-host-comp", Type: routing.ProviderMock, Port: 8100, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route_mock_qwen", ApplicationID: "app_mock_comp", GatewayModelName: "qwen-coder", AppModelName: "qwen-coder", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "mock-host-comp", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		panic(err)
	}
}

type errorUsers struct {
	err error
}

func (e errorUsers) UserByID(ctx context.Context, id string) (store.User, error) {
	return store.User{}, e.err
}

func (e errorUsers) ListUsers(ctx context.Context) ([]store.User, error) {
	return nil, e.err
}

type failingTokens struct {
	err error
}

func (f failingTokens) TokensByUser(ctx context.Context, userID string) ([]store.TokenRecord, error) {
	return nil, f.err
}

func (f failingTokens) TokensByService(ctx context.Context, serviceID string) ([]store.TokenRecord, error) {
	return nil, f.err
}

func (f failingTokens) TokensByProject(ctx context.Context, projectID string) ([]store.TokenRecord, error) {
	return nil, f.err
}

func (f failingTokens) TokenByID(ctx context.Context, id string) (store.TokenRecord, error) {
	return store.TokenRecord{}, f.err
}

func (f failingTokens) CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error {
	return f.err
}

func (f failingTokens) UpdateTokenMetadata(ctx context.Context, token store.TokenRecord) error {
	return f.err
}

func (f failingTokens) DeleteToken(ctx context.Context, id string) error {
	return f.err
}

func (f failingTokens) RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error {
	return f.err
}

func createEditableToken(t *testing.T, srv *Server, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"`+name+`","scopes":["gateway:use"]}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	return body.Token.ID
}

func TestPortalTokenItemUpdatesNameScopesAndStatus(t *testing.T) {
	srv := NewTestServer()
	id := createEditableToken(t, srv, "Editable")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"name":"Renamed","scopes":["gateway:use","admin"],"status":"disabled"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Name   string   `json:"name"`
		Status string   `json:"status"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if body.Name != "Renamed" || body.Status != "disabled" || len(body.Scopes) != 2 {
		t.Fatalf("patched token = %#v", body)
	}
}

func TestPortalTokenItemRejectsDuplicateName(t *testing.T) {
	srv := NewTestServer()
	createEditableToken(t, srv, "First")
	second := createEditableToken(t, srv, "Second")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+second, `{"name":"first"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "portal.token_name_conflict" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalTokenItemUnknownIDReturns404(t *testing.T) {
	srv := NewTestServer()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/tok_missing", `{"name":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "portal.token_not_found" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestPortalTokenItemDeleteRemovesTokenAndRevokesBearer(t *testing.T) {
	srv := NewTestServer()
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/tokens", `{"name":"Delete Me","scopes":["gateway:use"]}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}

	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, newJSONRequest(http.MethodDelete, "/api/portal/tokens/"+created.Token.ID, ""))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}

	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Authorization", "Bearer "+created.Secret)
	chatRec := httptest.NewRecorder()
	srv.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token still authenticates: status = %d", chatRec.Code)
	}
}

// newModelOverrideTestServer seeds a token whose ModelOverride is "qwen-coder"
// (the only gateway model name seedGatewayTestRoutes maps to a routable host)
// and returns the server plus its usage recorder. The bearer secret is
// "ovr-secret". Completion tests send a body model of "gpt-oss-20b", which is
// unroutable — so a 200 + a usage event with Model "qwen-coder" proves the
// override (not the request body) drove routing; without the override the
// resolver would return 502 routing.no_model_route.
func newModelOverrideTestServer(t *testing.T) (*Server, *usage.Recorder) {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ovr", UserID: "usr_dev", Name: "Override", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, ModelOverride: "qwen-coder"}, "ovr-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	srv := New(ServerDeps{Tokens: tokens, Usage: recorder, Provider: provider.NewMock(), Routes: routeStore, Portal: portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()})})
	return srv, recorder
}

// assertOverrideDroveRouting runs the request against srv and asserts the call
// succeeded and recorded exactly one usage event attributed to the override model.
func assertOverrideDroveRouting(t *testing.T, srv *Server, recorder *usage.Recorder, method, path, body string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ovr-secret")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	events := recorder.All()
	if len(events) != 1 || events[0].Model != "qwen-coder" {
		t.Fatalf("usage events = %#v (want one with Model qwen-coder)", events)
	}
	// Issue #7: the PRE-override name the client actually sent must be kept.
	if events[0].RequestedModel != "gpt-oss-20b" {
		t.Fatalf("RequestedModel = %q, want gpt-oss-20b (the client's original request)", events[0].RequestedModel)
	}
}

// TestChatCompletionAppliesTokenModelOverride verifies that a token carrying a
// model_override forces the served model for /v1/chat/completions regardless of
// what the client sent.
func TestChatCompletionAppliesTokenModelOverride(t *testing.T) {
	srv, recorder := newModelOverrideTestServer(t)
	assertOverrideDroveRouting(t, srv, recorder, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`)
}

// TestChatCompletionRecordsRequestedModelWithoutOverride: with no token
// override, requested and effective model are identical on the event.
func TestChatCompletionRecordsRequestedModelWithoutOverride(t *testing.T) {
	srv := NewTestServer()
	req := newJSONRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Model != "qwen-coder" || events[0].RequestedModel != "qwen-coder" {
		t.Fatalf("Model = %q, RequestedModel = %q, want both qwen-coder", events[0].Model, events[0].RequestedModel)
	}
}

// TestResponsesAppliesTokenModelOverride is the sibling of the chat-completions
// case for the OpenAI Responses endpoint (handleOpenAIResponses). Body shape
// mirrors the existing /v1/responses tests (uses "input"); non-streaming since
// this endpoint rejects streaming.
func TestResponsesAppliesTokenModelOverride(t *testing.T) {
	srv, recorder := newModelOverrideTestServer(t)
	assertOverrideDroveRouting(t, srv, recorder, http.MethodPost, "/v1/responses",
		`{"model":"gpt-oss-20b","input":"hi"}`)
}

// TestAnthropicMessagesAppliesTokenModelOverride is the sibling for the
// Anthropic Messages endpoint (handleAnthropicMessages). Body shape mirrors the
// existing /anthropic/v1/messages tests; non-streaming since this endpoint
// rejects streaming.
func TestAnthropicMessagesAppliesTokenModelOverride(t *testing.T) {
	srv, recorder := newModelOverrideTestServer(t)
	assertOverrideDroveRouting(t, srv, recorder, http.MethodPost, "/anthropic/v1/messages",
		`{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`)
}

// TestChatRunAsTokenAppliesOverrideAndUsage verifies that a session-authenticated
// chat request carrying the X-OP-Run-As-Token header swaps the principal to the
// named token (owned by the session user) before the model override and routing
// are applied, and that usage is attributed to that token rather than the bare
// session.
func TestChatRunAsTokenAppliesOverrideAndUsage(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ra", UserID: "usr_owner", Name: "RA", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, ModelOverride: "qwen-coder"}, "ra-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	cookie := loginAs(t, srv, "owner@example.test", "password-1")

	body := `{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(csrfHeaderName, "1")
	req.Header.Set(runAsHeaderName, "tok_ra")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].Model != "qwen-coder" || events[0].TokenID != "tok_ra" {
		t.Fatalf("usage events = %#v (want exactly one with Model=qwen-coder TokenID=tok_ra)", events)
	}
}

// TestChatRunAsTokenForbiddenForUnownedToken verifies that naming a run-as token
// the session user does not own (or that does not exist) is rejected with 403
// portal.token_forbidden and no chat completion or usage is recorded.
func TestChatRunAsTokenForbiddenForUnownedToken(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_other", "other@example.test", "password-1", "user")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other", UserID: "usr_other", Name: "Other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, ModelOverride: "qwen-coder"}, "other-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	cookie := loginAs(t, srv, "owner@example.test", "password-1")

	body := `{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(csrfHeaderName, "1")
	req.Header.Set(runAsHeaderName, "tok_other")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "portal.token_forbidden") {
		t.Fatalf("run-as of unowned token should be 403 portal.token_forbidden, got %d body=%s", rr.Code, rr.Body.String())
	}
	if events := srv.Usage.All(); len(events) != 0 {
		t.Fatalf("forbidden run-as should record no usage, got %#v", events)
	}
}

// TestChatWithoutRunAsHeaderUsesSessionToken verifies that omitting the run-as
// header leaves ordinary session-authenticated chat behavior unchanged: usage is
// attributed to the bare session principal, whose TokenID is empty.
func TestChatWithoutRunAsHeaderUsesSessionToken(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "owner@example.test", "password-1")

	body := `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].TokenID != "" || events[0].UserID != "usr_owner" {
		t.Fatalf("usage events = %#v (want exactly one with TokenID=\"\" UserID=usr_owner)", events)
	}
}

type stubCaptureStore struct{}

func (stubCaptureStore) SaveCapture(ctx context.Context, capture store.Capture) error {
	return nil
}

// *store.SQLiteStore must satisfy the gateway capture write interface (P4 wires it).
var _ CaptureStore = (*store.SQLiteStore)(nil)

func TestNewCapturesNilUnlessProvided(t *testing.T) {
	if New(ServerDeps{}).Captures != nil {
		t.Fatalf("Captures = non-nil, want nil when unset (fail-closed; New must not auto-default)")
	}
	if New(ServerDeps{Captures: stubCaptureStore{}}).Captures == nil {
		t.Fatalf("Captures = nil, want the provided store propagated")
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

func TestTelemetrySampleFromRequestMapsPowerFields(t *testing.T) {
	cpu := 65.0
	neg := -3.0
	req := agentTelemetryRequest{
		ServerID: "srv",
		Host: &agentHostReport{
			CPUPowerW:    &cpu,
			SystemPowerW: &neg, // negative -> treated as unavailable (nil)
		},
	}
	sample, err := telemetrySampleFromRequest(req, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetrySampleFromRequest: %v", err)
	}
	if sample.CPUPowerW == nil || *sample.CPUPowerW != 65.0 {
		t.Fatalf("CPUPowerW = %v, want 65.0", sample.CPUPowerW)
	}
	if sample.SystemPowerW != nil {
		t.Fatalf("SystemPowerW = %v, want nil (negative sanitized to unavailable)", *sample.SystemPowerW)
	}
	// A request with no host leaves both power scalars nil.
	empty, err := telemetrySampleFromRequest(agentTelemetryRequest{ServerID: "srv"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("no-host telemetrySampleFromRequest: %v", err)
	}
	if empty.CPUPowerW != nil || empty.SystemPowerW != nil {
		t.Fatalf("no-host power scalars = %v/%v, want nil/nil", empty.CPUPowerW, empty.SystemPowerW)
	}
}

func TestPerfPointFromSampleCarriesPower(t *testing.T) {
	cpu := 12.5
	sample := routing.TelemetrySample{CPUPowerW: &cpu} // system nil
	dto := perfPointFromSample(sample)
	if dto.CPUPowerW == nil || *dto.CPUPowerW != 12.5 {
		t.Fatalf("dto.CPUPowerW = %v, want 12.5", dto.CPUPowerW)
	}
	if dto.SystemPowerW != nil {
		t.Fatalf("dto.SystemPowerW = %v, want nil", *dto.SystemPowerW)
	}
	// A nil power scalar must marshal to JSON null (not 0).
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"system_power_w":null`)) {
		t.Fatalf("system_power_w should marshal to null; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"cpu_power_w":12.5`)) {
		t.Fatalf("cpu_power_w should marshal to 12.5; got %s", raw)
	}
}

func TestTelemetrySampleFromRequestMapsTempField(t *testing.T) {
	temp := 58.5
	req := agentTelemetryRequest{
		ServerID: "srv",
		Host: &agentHostReport{
			CPUTempC: &temp,
		},
	}
	sample, err := telemetrySampleFromRequest(req, time.Now().UTC())
	if err != nil {
		t.Fatalf("telemetrySampleFromRequest: %v", err)
	}
	if sample.CPUTempC == nil || *sample.CPUTempC != 58.5 {
		t.Fatalf("CPUTempC = %v, want 58.5", sample.CPUTempC)
	}
	// A request with no host leaves the temp scalar nil.
	empty, err := telemetrySampleFromRequest(agentTelemetryRequest{ServerID: "srv"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("no-host telemetrySampleFromRequest: %v", err)
	}
	if empty.CPUTempC != nil {
		t.Fatalf("no-host CPUTempC = %v, want nil", *empty.CPUTempC)
	}
	// A negative reading is sanitized to unavailable (nil), mirroring CPUPowerW.
	neg := -1.0
	negReq := agentTelemetryRequest{ServerID: "srv", Host: &agentHostReport{CPUTempC: &neg}}
	negSample, err := telemetrySampleFromRequest(negReq, time.Now().UTC())
	if err != nil {
		t.Fatalf("negative telemetrySampleFromRequest: %v", err)
	}
	if negSample.CPUTempC != nil {
		t.Fatalf("negative CPUTempC = %v, want nil (sanitized to unavailable)", *negSample.CPUTempC)
	}
}

func TestPerfPointFromSampleCarriesTemp(t *testing.T) {
	temp := 58.5
	sample := routing.TelemetrySample{CPUTempC: &temp}
	dto := perfPointFromSample(sample)
	if dto.CPUTempC == nil || *dto.CPUTempC != 58.5 {
		t.Fatalf("dto.CPUTempC = %v, want 58.5", dto.CPUTempC)
	}
	// A nil temp scalar must marshal to JSON null (not 0).
	empty := perfPointFromSample(routing.TelemetrySample{})
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"cpu_temp_c":null`)) {
		t.Fatalf("cpu_temp_c should marshal to null; got %s", raw)
	}
	raw2, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw2, []byte(`"cpu_temp_c":58.5`)) {
		t.Fatalf("cpu_temp_c should marshal to 58.5; got %s", raw2)
	}
}
