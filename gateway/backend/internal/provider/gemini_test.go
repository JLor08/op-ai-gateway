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
	"time"
)

func geminiTextMessage(text string) inference.Message {
	return inference.Message{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}
}

// collectGeminiStream runs CompleteStream, concatenating every text delta, and
// returns that text plus the full event slice.
func collectGeminiStream(t *testing.T, c *GeminiClient, target routing.Target, req inference.Request) (string, []inference.StreamEvent) {
	t.Helper()
	var (
		text   strings.Builder
		events []inference.StreamEvent
	)
	err := c.CompleteStream(context.Background(), target, req, func(ev inference.StreamEvent) error {
		events = append(events, ev)
		if ev.Type == inference.StreamEventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	return text.String(), events
}

func TestGeminiClientInterfaces(t *testing.T) {
	var _ Client = (*GeminiClient)(nil)
	var _ StreamingClient = (*GeminiClient)(nil)
	var _ NativeProxyClient = (*GeminiClient)(nil)
}

func TestGeminiCompletePathAuthAndParse(t *testing.T) {
	var gotPath, gotKey, gotAuth, gotQueryKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotQueryKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`))
	}))
	defer srv.Close()

	ctx := WithUpstreamAuthHeaders(context.Background(), "x-goog-api-key", "secret-key", nil)
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro", Timeout: 5 * time.Second}
	resp, err := c.Complete(ctx, tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "secret-key" || gotAuth != "" {
		t.Fatalf("auth: x-goog-api-key=%q authorization=%q", gotKey, gotAuth)
	}
	if gotQueryKey != "" {
		t.Fatalf("credential leaked into the query string: key=%q", gotQueryKey)
	}
	if resp.Text != "hi" || resp.FinishReason != "stop" || resp.Usage.TotalTokens != 3 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestGeminiCompleteStripsModelsPrefixAndEscapes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "models/gemini-2.5-pro"}
	if _, err := c.Complete(context.Background(), tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("x")}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGeminiComplete400SurfacesSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid thinkingConfig","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, err := c.Complete(context.Background(), tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("x")}})
	if err == nil || !strings.Contains(err.Error(), "Invalid thinkingConfig") {
		t.Fatalf("err = %v, want snippet", err)
	}
}

func TestGeminiCompleteStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("stream path/query = %q ? %q", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":2,\"totalTokenCount\":4}}\n\n"))
		fl.Flush()
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	text, events := collectGeminiStream(t, c, tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("hi")}})
	if text != "Hello" {
		t.Fatalf("text = %q", text)
	}
	last := events[len(events)-1]
	if last.Type != inference.StreamEventCompleted || last.FinishReason != "stop" || last.Usage == nil || last.Usage.TotalTokens != 4 {
		t.Fatalf("completed = %+v", last)
	}
}

// A functionCall on an earlier chunk with a terminal STOP chunk must still
// complete as tool_calls: ParseStreamChunk maps finishReason per chunk, so the
// client has to remember the tool call across chunks.
func TestGeminiCompleteStreamToolCallOnEarlierChunkUpgradesFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Oslo\"}}}]}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}\n\n"))
		fl.Flush()
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, events := collectGeminiStream(t, c, tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("weather?")}})
	var calls []inference.ToolCall
	for _, ev := range events {
		if ev.Type == inference.StreamEventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if len(calls) != 1 || calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"Oslo"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	last := events[len(events)-1]
	if last.Type != inference.StreamEventCompleted || last.FinishReason != "tool_calls" || last.Usage == nil || last.Usage.TotalTokens != 4 {
		t.Fatalf("completed = %+v", last)
	}
}

// Gemini reports cumulative usageMetadata on every chunk; only the last
// non-zero one is the answer's usage.
func TestGeminiCompleteStreamKeepsLastUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":1,\"totalTokenCount\":6}}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"b\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}\n\n"))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, events := collectGeminiStream(t, c, tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("hi")}})
	last := events[len(events)-1]
	if last.Usage == nil || last.Usage.OutputTokens != 2 || last.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want the cumulative last chunk (output 2, total 7)", last.Usage)
	}
}

func TestGeminiCompleteStreamEndsEarly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))
		fl.Flush()
		// no finishReason chunk, connection just closes
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	err := c.CompleteStream(context.Background(), tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("hi")}}, func(inference.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("want ErrUnavailable on a stream that ends without finishReason")
	}
}

func TestGeminiProxyNativeRelaysVerbatim(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("relayed"))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL}
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	pr, err := c.ProxyNative(context.Background(), tgt, "/v1beta/models/gemini-2.5-pro:generateContent", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusTeapot || gotBody != string(raw) {
		t.Fatalf("status=%d body=%q", pr.StatusCode, gotBody)
	}
}

func TestGeminiTranslateDoesNotFollowRedirect(t *testing.T) {
	var hitCount int
	var gotKeyAtTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		if r.URL.Path == "/redirected" {
			gotKeyAtTarget = r.Header.Get("x-goog-api-key")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"leaked"}]},"finishReason":"STOP"}]}`))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx := WithUpstreamAuthHeaders(context.Background(), "x-goog-api-key", "secret", nil)
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, err := c.Complete(ctx, tgt, inference.Request{Messages: []inference.Message{geminiTextMessage("hi")}})
	if err == nil {
		t.Fatal("want a non-2xx (the 307 relayed as the answer), not a followed redirect")
	}
	if gotKeyAtTarget != "" {
		t.Fatalf("x-goog-api-key followed the redirect: %q", gotKeyAtTarget)
	}
	if hitCount != 1 {
		t.Fatalf("upstream hit %d times, want 1 (redirect not followed)", hitCount)
	}
}

func TestGeminiProxyNativeDoesNotFollowRedirect(t *testing.T) {
	var gotKeyAtTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			gotKeyAtTarget = r.Header.Get("x-goog-api-key")
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx := WithUpstreamAuthHeaders(context.Background(), "x-goog-api-key", "secret", nil)
	c := NewGeminiClient(srv.Client())
	pr, err := c.ProxyNative(ctx, routing.Target{Endpoint: srv.URL}, "/v1beta/models/gemini-2.5-pro:generateContent", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 handed back", pr.StatusCode)
	}
	if gotKeyAtTarget != "" {
		t.Fatalf("x-goog-api-key followed the redirect: %q", gotKeyAtTarget)
	}
}
