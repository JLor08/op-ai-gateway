// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// The service layer reaches the chat_blobs rows only through s.chats, the
// ChatStore interface. This pins that the interface carries the four methods the
// image-store service paths call (atomic save, by-id read, owner lookup, scoped
// batch read), so a store refactor cannot silently drop one. PutChatBlob is
// deliberately NOT on the interface: it is store-internal, used only inside
// SaveChatWithBlobs.
func TestChatStoreInterfaceCarriesBlobMethods(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var chats ChatStore = store.NewMemoryChatStore(0)

	chat := store.Chat{ID: "chat_1", UserID: "usr_a", Title: "t", KeyVersion: 1, Blob: []byte("doc"), CreatedAt: now, UpdatedAt: now}
	if err := chats.CreateChat(ctx, chat); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	blob := store.ChatBlob{ID: "blob_1", ChatID: "chat_1", KeyVersion: 1, Blob: []byte("png"), ContentType: "image/png", ByteLen: 3, CreatedAt: now}
	if err := chats.SaveChatWithBlobs(ctx, chat, []store.ChatBlob{blob}, nil); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	got, err := chats.ChatBlobByID(ctx, "blob_1")
	if err != nil {
		t.Fatalf("ChatBlobByID: %v", err)
	}
	if string(got.Blob) != "png" || got.ChatID != "chat_1" {
		t.Fatalf("ChatBlobByID = %+v, want the saved blob", got)
	}
	owner, err := chats.ChatBlobOwner(ctx, "blob_1")
	if err != nil {
		t.Fatalf("ChatBlobOwner: %v", err)
	}
	if owner != "chat_1" {
		t.Fatalf("ChatBlobOwner = %q, want chat_1", owner)
	}
	byID, err := chats.ChatBlobsByIDs(ctx, "chat_1", []string{"blob_1"})
	if err != nil {
		t.Fatalf("ChatBlobsByIDs: %v", err)
	}
	if len(byID) != 1 || string(byID["blob_1"].Blob) != "png" {
		t.Fatalf("ChatBlobsByIDs = %+v, want blob_1 only", byID)
	}
}
