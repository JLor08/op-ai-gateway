// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// usageStreamer emits one text delta then a Completed event carrying usage, so
// completeStream has token counts to forward in a final usage chunk.
type usageStreamer struct{ provider.Mock }

func (usageStreamer) CompleteStream(ctx context.Context, tgt routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "hi"}); err != nil {
		return err
	}
	u := inference.Usage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14, CachedTokens: 8}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func TestChatStreamEmitsUsageChunkWhenRequested(t *testing.T) {
	srv := newCaptureTestServer(t, usageStreamer{}, &fakeCaptureStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"qwen-coder","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"prompt_tokens":11`) ||
		!strings.Contains(body, `"completion_tokens":3`) ||
		!strings.Contains(body, `"total_tokens":14`) {
		t.Fatalf("usage chunk missing/incomplete:\n%s", body)
	}
	if !strings.Contains(body, `"cached_tokens":8`) {
		t.Fatalf("cached_tokens missing:\n%s", body)
	}
	if iu, id := strings.Index(body, `"usage"`), strings.Index(body, "[DONE]"); iu < 0 || id < 0 || iu > id {
		t.Fatalf("usage chunk not before [DONE]:\n%s", body)
	}
}

func TestChatStreamNoUsageChunkWhenNotRequested(t *testing.T) {
	srv := newCaptureTestServer(t, usageStreamer{}, &fakeCaptureStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("usage chunk emitted without include_usage:\n%s", rec.Body.String())
	}
}
