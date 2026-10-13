// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

// requestRecordingProvider is the upstream end of a chat run: a streaming
// provider that keeps the provider-neutral request it was handed -- exactly what
// a remote adapter (Anthropic, OpenAI) turns into its own wire format -- and
// answers with one short delta.
type requestRecordingProvider struct {
	mu   sync.Mutex
	reqs []inference.Request
}

func (*requestRecordingProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *requestRecordingProvider) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
}

func (p *requestRecordingProvider) requests() []inference.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]inference.Request(nil), p.reqs...)
}

// A later turn over a thread whose image was stored by reference reaches the
// upstream provider with the image's real bytes, as a standard base64 data: url,
// and with no gateway url anywhere in what it is handed.
//
// This is the property that makes a remote provider work at all: an adapter
// that is given a url it cannot fetch drops the image WITHOUT an error (see
// TestAnthropicImageSourceForParsesDataURIs in the provider package), so a
// reference that slipped through would not fail the run -- the model would
// answer a question about a picture it was never shown.
func TestRunOverAStoredImageHandsTheProviderRealBytes(t *testing.T) {
	prov := &requestRecordingProvider{}
	srv, owner, chatID := newRunTestServerWithProvider(t, prov)
	ctx := context.Background()

	raw := make([]byte, 1001) // not a multiple of 3, so the base64 is padded
	for i := range raw {
		raw[i] = byte(i*7 + 3) // arbitrary non-text bytes
	}
	uploaded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	// Turn 1 stores the image: the run commit moves it into chat_blobs and the
	// transcript keeps only a reference.
	if _, _, err := srv.Portal.PrepareChatRun(ctx, owner, chatID, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":%q}}]`, uploaded)),
		Settings:    portal.ChatRunSettings{Model: "qwen-coder"},
	}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	stored, err := srv.Portal.GetChat(ctx, owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Content), "/api/portal/chat-blobs/") || strings.Contains(string(stored.Content), "data:image") {
		t.Fatalf("precondition: the transcript must hold the image by reference: %.300s", stored.Content)
	}

	// Turn 2 is a plain question; its history is what the run sends upstream.
	history, settings, err := srv.Portal.PrepareChatRun(ctx, owner, chatID, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(`"what is in it?"`),
		Settings:    portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{History: history, Settings: settings})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("run status = %q, want completed", got)
	}

	reqs := prov.requests()
	if len(reqs) != 1 {
		t.Fatalf("the provider saw %d requests, want 1", len(reqs))
	}
	var images []string
	for _, msg := range reqs[0].Messages {
		for _, part := range msg.Content {
			if strings.Contains(part.ImageURL, "chat-blobs") || strings.Contains(part.Text, "chat-blobs") {
				t.Fatalf("the provider was handed a gateway url: %q", part.ImageURL)
			}
			if part.Type == inference.ContentImage {
				images = append(images, part.ImageURL)
			}
		}
	}
	if len(images) != 1 {
		t.Fatalf("the provider got %d images, want the 1 stored one", len(images))
	}
	if images[0] != uploaded {
		t.Fatalf("the provider's image is not the uploaded bytes as a standard base64 data url:\n got  %.120s\n want %.120s", images[0], uploaded)
	}
}
