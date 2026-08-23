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

// TestCaptureSinkComplete verifies the OpenAI-compatible client records the
// upstream request (headers+body) and the raw upstream response into a sink
// threaded via context, on the non-stream path.
func TestCaptureSinkComplete(t *testing.T) {
	const upstreamResp = `{"choices":[{"message":{"content":"hi there"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, upstreamResp)
	}))
	defer upstream.Close()

	client := NewOpenAICompatibleClient(http.DefaultClient)
	sink := NewCaptureSink(1 << 20)
	ctx := WithCaptureSink(context.Background(), sink)
	req := inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}}

	if _, err := client.Complete(ctx, routing.Target{Endpoint: upstream.URL, ProviderModel: "up", Timeout: 5 * time.Second}, req); err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	if got := string(sink.RequestBody()); !strings.Contains(got, `"model":"up"`) || !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("sink request body = %q, want the translated chat-completions body", got)
	}
	if sink.RequestHeaders().Get("Content-Type") != "application/json" {
		t.Fatalf("sink request headers = %v, want Content-Type application/json", sink.RequestHeaders())
	}
	if got := string(sink.ResponseBody()); got != upstreamResp {
		t.Fatalf("sink response body = %q, want the raw upstream response", got)
	}
	if sink.ResponseHeaders().Get("X-Upstream") != "yes" {
		t.Fatalf("sink response headers = %v, want X-Upstream yes", sink.ResponseHeaders())
	}
}

// TestCaptureSinkCompleteStream verifies the streaming path tees the raw upstream
// SSE into the sink and records request+response headers.
func TestCaptureSinkCompleteStream(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"he"}}]}`,
		`data: {"choices":[{"delta":{"content":"llo"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		"data: [DONE]",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	client := NewOpenAICompatibleClient(http.DefaultClient)
	sink := NewCaptureSink(1 << 20)
	ctx := WithCaptureSink(context.Background(), sink)
	req := inference.Request{Stream: true, Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hey"}}}}}

	var text strings.Builder
	err := client.CompleteStream(ctx, routing.Target{Endpoint: upstream.URL, ProviderModel: "up", Timeout: 5 * time.Second}, req, func(ev inference.StreamEvent) error {
		if ev.Type == inference.StreamEventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream returned %v", err)
	}
	if text.String() != "hello" {
		t.Fatalf("streamed text = %q, want hello", text.String())
	}
	if got := string(sink.RequestBody()); !strings.Contains(got, `"stream":true`) || !strings.Contains(got, `"content":"hey"`) {
		t.Fatalf("sink request body = %q, want the streamed chat-completions body", got)
	}
	// The tee captured the raw upstream SSE, including the [DONE] framing.
	resp := string(sink.ResponseBody())
	for _, want := range []string{`"delta":{"content":"he"}`, `"delta":{"content":"llo"}`, "[DONE]"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("sink response body missing %q:\n%s", want, resp)
		}
	}
	if sink.ResponseHeaders().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sink response headers = %v, want text/event-stream", sink.ResponseHeaders())
	}
}

// TestCaptureSinkNilNoop verifies that with no sink in context the provider still
// works (the not-capturing path) and every sink method is nil-safe.
func TestCaptureSinkNilNoop(t *testing.T) {
	if s := CaptureSinkFrom(context.Background()); s != nil {
		t.Fatalf("CaptureSinkFrom on a bare context = %v, want nil", s)
	}
	var s *CaptureSink
	s.RecordRequest(http.Header{}, []byte("x")) // must not panic
	s.WriteResponse([]byte("y"))
	if s.ResponseWriter() != nil {
		t.Fatalf("nil sink ResponseWriter must be nil")
	}
	if s.RequestBody() != nil || s.ResponseBody() != nil {
		t.Fatalf("nil sink getters must be nil")
	}
}

// TestCaptureSinkResponseCap verifies the response buffer is bounded by respCap.
func TestCaptureSinkResponseCap(t *testing.T) {
	sink := NewCaptureSink(4)
	sink.WriteResponse([]byte("ab"))
	sink.WriteResponse([]byte("cdef")) // only "cd" fits
	if got := string(sink.ResponseBody()); got != "abcd" {
		t.Fatalf("bounded response = %q, want abcd", got)
	}
}
