// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

func chatToken(userID string) auth.Token {
	return auth.Token{UserID: userID, Scopes: []string{"gateway:use"}}
}

// TestServiceChatSealOpenRoundTripWithCipher: content in -> GetChat content out
// identical, and the stored blob is sealed (KeyVersion 1).
func TestServiceChatSealOpenRoundTripWithCipher(t *testing.T) {
	ctx := context.Background()
	cipher, err := capture.New(captureTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	chatStore := store.NewMemoryChatStore(0)
	svc := NewService(ServiceDeps{Chats: chatStore, Cipher: cipher})

	content := `{"settings":{"model":"m"},"messages":[{"role":"user","content":"hi"}]}`
	created, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: "  My Chat  ", Content: []byte(content)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if created.Title != "My Chat" {
		t.Fatalf("title = %q, want trimmed %q", created.Title, "My Chat")
	}
	if string(created.Content) != content {
		t.Fatalf("echoed content = %s, want %s", created.Content, content)
	}

	// The stored blob must be sealed (KeyVersion 1), not plain.
	row, err := chatStore.ChatByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("store ChatByID: %v", err)
	}
	if row.KeyVersion != capture.KeyVersion {
		t.Fatalf("stored KeyVersion = %d, want %d (sealed)", row.KeyVersion, capture.KeyVersion)
	}

	got, err := svc.GetChat(ctx, chatToken("usr_a"), created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if string(got.Content) != content {
		t.Fatalf("GetChat content = %s, want %s", got.Content, content)
	}
}

// TestServiceChatSealOpenRoundTripNoCipher: without a cipher the blob is stored
// plain (KeyVersion 0) and still round-trips.
func TestServiceChatSealOpenRoundTripNoCipher(t *testing.T) {
	ctx := context.Background()
	chatStore := store.NewMemoryChatStore(0)
	svc := NewService(ServiceDeps{Chats: chatStore}) // no cipher

	content := `{"messages":[]}`
	created, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: "Plain", Content: []byte(content)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	row, err := chatStore.ChatByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("store ChatByID: %v", err)
	}
	if row.KeyVersion != 0 {
		t.Fatalf("stored KeyVersion = %d, want 0 (plain, RAM fallback)", row.KeyVersion)
	}
	got, err := svc.GetChat(ctx, chatToken("usr_a"), created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if string(got.Content) != content {
		t.Fatalf("GetChat content = %s, want %s", got.Content, content)
	}
}

// TestServiceChatSaveUpdatesTitleAndContent: a PUT sets both; an unset content
// preserves the stored blob.
func TestServiceChatSaveUpdatesTitleAndContent(t *testing.T) {
	ctx := context.Background()
	cipher, _ := capture.New(captureTestKey)
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0), Cipher: cipher})
	created, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: "v1", Content: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Save both title + content.
	newTitle := "v2"
	newContent := []byte(`{"n":2,"extra":true}`)
	rawContent := json.RawMessage(newContent)
	saved, err := svc.SaveChat(ctx, chatToken("usr_a"), created.ID, UpdateChatRequest{Title: &newTitle, Content: &rawContent})
	if err != nil {
		t.Fatalf("SaveChat: %v", err)
	}
	if saved.Title != "v2" || string(saved.Content) != string(newContent) {
		t.Fatalf("saved = %#v", saved)
	}
	got, _ := svc.GetChat(ctx, chatToken("usr_a"), created.ID)
	if string(got.Content) != string(newContent) {
		t.Fatalf("GetChat after save = %s, want %s", got.Content, newContent)
	}

	// Save title only; content must be preserved (re-opened + re-sealed).
	onlyTitle := "v3"
	saved2, err := svc.SaveChat(ctx, chatToken("usr_a"), created.ID, UpdateChatRequest{Title: &onlyTitle})
	if err != nil {
		t.Fatalf("SaveChat title-only: %v", err)
	}
	if saved2.Title != "v3" || string(saved2.Content) != string(newContent) {
		t.Fatalf("title-only save changed content: %#v", saved2)
	}
}

// TestServiceChatOwnershipIsolation: user B cannot see, save, or delete user A's
// chat; each attempt is ErrChatNotFound. B's list stays empty.
func TestServiceChatOwnershipIsolation(t *testing.T) {
	ctx := context.Background()
	cipher, _ := capture.New(captureTestKey)
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0), Cipher: cipher})
	a, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: "A's", Content: []byte(`{"secret":true}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if _, err := svc.GetChat(ctx, chatToken("usr_b"), a.ID); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("B GetChat = %v, want ErrChatNotFound", err)
	}
	title := "hijack"
	if _, err := svc.SaveChat(ctx, chatToken("usr_b"), a.ID, UpdateChatRequest{Title: &title}); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("B SaveChat = %v, want ErrChatNotFound", err)
	}
	if err := svc.DeleteChat(ctx, chatToken("usr_b"), a.ID); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("B DeleteChat = %v, want ErrChatNotFound", err)
	}
	bList, err := svc.ListChats(ctx, chatToken("usr_b"))
	if err != nil {
		t.Fatalf("B ListChats: %v", err)
	}
	if len(bList) != 0 {
		t.Fatalf("B ListChats = %#v, want empty", bList)
	}
	// A can still read + delete its own.
	if _, err := svc.GetChat(ctx, chatToken("usr_a"), a.ID); err != nil {
		t.Fatalf("A GetChat own: %v", err)
	}
	if err := svc.DeleteChat(ctx, chatToken("usr_a"), a.ID); err != nil {
		t.Fatalf("A DeleteChat own: %v", err)
	}
}

func TestServiceChatMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	if _, err := svc.GetChat(ctx, chatToken("usr_a"), "chat_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChat(unknown) = %v, want store.ErrNotFound", err)
	}
}

func TestServiceChatTitleTooLongRejected(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	long := strings.Repeat("x", maxChatTitleLen+1)
	if _, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: long, Content: []byte(`{}`)}); !errors.Is(err, ErrChatTitleInvalid) {
		t.Fatalf("CreateChat(long title) = %v, want ErrChatTitleInvalid", err)
	}
}

func TestServiceChatContentTooLargeRejected(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	big := make([]byte, maxChatContentBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := svc.CreateChat(ctx, chatToken("usr_a"), CreateChatRequest{Title: "big", Content: big}); !errors.Is(err, ErrChatTooLarge) {
		t.Fatalf("CreateChat(too large) = %v, want ErrChatTooLarge", err)
	}
}

func TestPrepareChatRunAppendsUserAndReturnsHistory(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, err := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{"model":"m"},"messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	history, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`"hallo"`),
		Settings:    ChatRunSettings{Model: "m", SystemPrompt: "be nice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// system + user
	if len(history) != 2 || history[0].Role != "system" || history[1].Role != "user" {
		t.Fatalf("unexpected history: %+v", history)
	}
	// The chat doc now ends with the user message.
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"hallo"`) {
		t.Fatalf("user message not persisted: %s", got.Content)
	}
	// Auto-title from the first user message.
	if got.Title != "hallo" {
		t.Fatalf("expected auto title, got %q", got.Title)
	}
}

func TestPrepareChatRunEditedHistoryReplaces(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"a","role":"user","content":"old"}]}`),
	})
	history, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{json.RawMessage(`{"id":"b","role":"user","content":"new"}`)},
		Settings:      ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || string(history[0].Content) != `"new"` {
		t.Fatalf("expected replaced history, got %+v", history)
	}
}

func TestCheckpointThenCommitAssistant(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u1","role":"user","content":"hi"}]}`),
	})
	turn := AssistantTurn{Reasoning: "think", Content: "partial", TTFTMs: 5}

	if err := svc.CheckpointAssistant(ctx, owner, created.ID, turn); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"status":"pending"`) ||
		!strings.Contains(string(got.Content), `"partial"`) {
		t.Fatalf("checkpoint not written: %s", got.Content)
	}

	final := AssistantTurn{Reasoning: "think", Content: "full answer", TTFTMs: 5, TPS: 12.5}
	if err := svc.CommitAssistant(ctx, owner, created.ID, final, "complete"); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"status":"complete"`) ||
		!strings.Contains(string(got.Content), `"full answer"`) {
		t.Fatalf("commit not written: %s", got.Content)
	}
	// Still exactly one assistant message (checkpoint upserts, does not append).
	if strings.Count(string(got.Content), `"role":"assistant"`) != 1 {
		t.Fatalf("expected single assistant message: %s", got.Content)
	}
}
