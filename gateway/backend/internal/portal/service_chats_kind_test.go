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

// The first send establishes the kind and it is persisted.
func TestPrepareChatRunPinsTheKindOnFirstSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings:    ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "image" {
		t.Fatalf("Kind = %q, want image", settings.Kind)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"image"`) {
		t.Fatalf("kind not persisted: %s", got.Content)
	}
}

// THE POINT OF THE TASK: a later send cannot change it, however the client
// submits it. ChatRunSettings is the POST body verbatim, so this is reachable
// from the API, not just from our own UI.
func TestPrepareChatRunRefusesToRepinTheKind(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
		Settings:    ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send submits the OPPOSITE kind (and an empty one, below).
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"hello"}`),
		Settings:    ChatRunSettings{Model: "llama", Kind: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "image" {
		t.Fatalf("Kind = %q, want the pinned image -- a later send must not unpin it", settings.Kind)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"image"`) {
		t.Fatalf("the pin was lost from the document: %s", got.Content)
	}
}

// A text-pinned thread must not be flippable to image by a later send, any
// more than an image-pinned thread can be flipped to text. A guard of
// `stored.Kind != ""` would only protect the image direction (an absent
// "kind" key is indistinguishable from "never sent"), silently letting a
// second send with Kind "image" turn a plain text thread into one whose next
// run goes to the images endpoint -- with no history at all, since that
// endpoint carries none.
func TestPrepareChatRunRefusesToPinTextThreadToImage(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	// First send: plain text, no kind submitted.
	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
		Settings:    ChatRunSettings{Model: "llama"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send on the SAME thread submits "image".
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"a cat"}`),
		Settings:    ChatRunSettings{Model: "sd-turbo", Kind: "image"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != "" {
		t.Fatalf("Kind = %q, want empty -- a text-pinned thread must not be flippable to image", settings.Kind)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if strings.Contains(string(got.Content), "kind") {
		t.Fatalf("the text pin must persist with no kind key at all: %s", got.Content)
	}
}

// A text chat stays byte-identical: an absent kind must not add the key. This
// is what `omitempty` plus last-in-the-struct buys, and it matters because the
// whole document is one blob that every save rewrites.
func TestPrepareChatRunOmitsAnEmptyKind(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
		Settings:    ChatRunSettings{Model: "llama"},
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := svc.GetChat(ctx, owner, created.ID)
	if strings.Contains(string(got.Content), "kind") {
		t.Fatalf("an empty kind must not appear in the persisted settings at all: %s", got.Content)
	}
}
