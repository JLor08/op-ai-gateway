// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// An image thread's settings round-trip: PrepareChatRun returns them and they
// land in the stored document.
func TestPrepareChatRunPersistsImageSettings(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	compression := 90
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings: ChatRunSettings{
			Model: "sd-turbo",
			Kind:  "image",
			Image: &ChatImageSettings{Size: "512x512", N: 2, OutputFormat: "webp", OutputCompression: &compression},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Image == nil || settings.Image.Size != "512x512" || settings.Image.N != 2 {
		t.Fatalf("image settings not returned: %+v", settings.Image)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"image":{`) || !strings.Contains(string(got.Content), `"512x512"`) {
		t.Fatalf("image settings not persisted: %s", got.Content)
	}
}

// A text thread must carry no "image" key -- the byte-identical guarantee the
// empty-kind test pins, now for the image settings blob. Even a raw client that
// submits image settings on a text thread (ChatRunSettings is the POST body
// verbatim) must not pollute the stored document: PrepareChatRun clears them for
// any non-image kind.
func TestPrepareChatRunOmitsImageSettingsForATextThread(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
		Settings: ChatRunSettings{
			Model: "llama",
			Image: &ChatImageSettings{Size: "512x512"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Image != nil {
		t.Fatalf("image settings must be cleared on a text thread, got %+v", settings.Image)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if strings.Contains(string(got.Content), "image") {
		t.Fatalf("a text thread must carry no image key at all: %s", got.Content)
	}
}

// Unlike the force-pinned kind, image settings are per-send: a later send to an
// already-pinned image thread replaces them. This is the composer changing
// size/n between runs, and what keeps them behaving like temperature/max_tokens
// rather than the immutable kind.
func TestPrepareChatRunUpdatesImageSettingsOnALaterSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	// First send pins the thread image with one size.
	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings: ChatRunSettings{
			Model: "sd-turbo",
			Kind:  "image",
			Image: &ChatImageSettings{Size: "512x512"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send (kind omitted, forced back to image) changes the size.
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"a dog"}`),
		Settings: ChatRunSettings{
			Model: "sd-turbo",
			Image: &ChatImageSettings{Size: "1024x1024", N: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "image" {
		t.Fatalf("Kind = %q, want the pinned image", settings.Kind)
	}
	if settings.Image == nil || settings.Image.Size != "1024x1024" || settings.Image.N != 3 {
		t.Fatalf("image settings not updated on the later send: %+v", settings.Image)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"1024x1024"`) || strings.Contains(string(got.Content), `"512x512"`) {
		t.Fatalf("the later send's image settings must replace the first's: %s", got.Content)
	}
}
