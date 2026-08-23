// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// chatStore is the method set both SQLiteStore and MemoryChatStore expose; the
// round-trip tests run against both to prove parity.
type chatStore interface {
	CreateChat(ctx context.Context, chat Chat) error
	UpdateChat(ctx context.Context, chat Chat) error
	ChatByID(ctx context.Context, id string) (ChatRow, error)
	ChatsByUser(ctx context.Context, userID string) ([]ChatSummary, error)
	DeleteChat(ctx context.Context, id string) error
}

var (
	_ chatStore = (*SQLiteStore)(nil)
	_ chatStore = (*MemoryChatStore)(nil)
)

func seedChatUser(t *testing.T, st *SQLiteStore, id string) {
	t.Helper()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := st.CreateUser(context.Background(), User{ID: id, Email: id + "@example.test", DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func runChatRoundTrip(t *testing.T, s chatStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)

	// Two chats for usr_a, one for usr_b; usr_a's second is updated later.
	chats := []Chat{
		{ID: "chat_a1", UserID: "usr_a", Title: "First", KeyVersion: 1, Blob: []byte("blob-a1"), CreatedAt: base, UpdatedAt: base},
		{ID: "chat_a2", UserID: "usr_a", Title: "Second", KeyVersion: 0, Blob: []byte("blob-a2"), CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)},
		{ID: "chat_b1", UserID: "usr_b", Title: "Bee", KeyVersion: 1, Blob: []byte("blob-b1"), CreatedAt: base, UpdatedAt: base},
	}
	for _, c := range chats {
		if err := s.CreateChat(ctx, c); err != nil {
			t.Fatalf("CreateChat %s: %v", c.ID, err)
		}
	}

	// ChatByID returns the full row (blob + key version + title).
	row, err := s.ChatByID(ctx, "chat_a1")
	if err != nil {
		t.Fatalf("ChatByID(chat_a1): %v", err)
	}
	if row.Title != "First" || row.KeyVersion != 1 || row.UserID != "usr_a" || !bytes.Equal(row.Blob, []byte("blob-a1")) {
		t.Fatalf("ChatByID(chat_a1) = %#v", row)
	}

	// ChatsByUser lists only usr_a's chats, most-recently-updated first, no blob.
	list, err := s.ChatsByUser(ctx, "usr_a")
	if err != nil {
		t.Fatalf("ChatsByUser(usr_a): %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ChatsByUser(usr_a) len = %d, want 2 (excludes usr_b)", len(list))
	}
	if list[0].ID != "chat_a2" || list[1].ID != "chat_a1" {
		t.Fatalf("ChatsByUser order = [%s, %s], want [chat_a2, chat_a1] (updated_at desc)", list[0].ID, list[1].ID)
	}

	// Update changes title + blob + key_version; ordering flips as updated_at moves.
	if err := s.UpdateChat(ctx, Chat{ID: "chat_a1", UserID: "usr_a", Title: "First Renamed", KeyVersion: 0, Blob: []byte("blob-a1-v2"), CreatedAt: base, UpdatedAt: base.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("UpdateChat(chat_a1): %v", err)
	}
	row, err = s.ChatByID(ctx, "chat_a1")
	if err != nil {
		t.Fatalf("ChatByID(chat_a1) post-update: %v", err)
	}
	if row.Title != "First Renamed" || row.KeyVersion != 0 || !bytes.Equal(row.Blob, []byte("blob-a1-v2")) {
		t.Fatalf("post-update row = %#v", row)
	}
	list, _ = s.ChatsByUser(ctx, "usr_a")
	if list[0].ID != "chat_a1" {
		t.Fatalf("post-update order head = %s, want chat_a1 (now most recent)", list[0].ID)
	}

	// Delete removes it; a second delete is ErrNotFound.
	if err := s.DeleteChat(ctx, "chat_a1"); err != nil {
		t.Fatalf("DeleteChat(chat_a1): %v", err)
	}
	if _, err := s.ChatByID(ctx, "chat_a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChatByID after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteChat(ctx, "chat_a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteChat = %v, want ErrNotFound", err)
	}

	// ErrNotFound paths for update/get of an unknown id.
	if _, err := s.ChatByID(ctx, "chat_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChatByID(unknown) = %v, want ErrNotFound", err)
	}
	if err := s.UpdateChat(ctx, Chat{ID: "chat_nope", Title: "x", Blob: []byte("y"), UpdatedAt: base}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateChat(unknown) = %v, want ErrNotFound", err)
	}

	// Duplicate id on create is ErrConflict.
	if err := s.CreateChat(ctx, Chat{ID: "chat_a2", UserID: "usr_a", Title: "dup", Blob: []byte("z"), CreatedAt: base, UpdatedAt: base}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate CreateChat = %v, want ErrConflict", err)
	}
}

func TestSQLiteChatRoundTrip(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	seedChatUser(t, st, "usr_a")
	seedChatUser(t, st, "usr_b")
	runChatRoundTrip(t, st)
}

func TestMemoryChatRoundTrip(t *testing.T) {
	runChatRoundTrip(t, NewMemoryChatStore(0))
}

// TestCrossStoreChatParity proves SQLiteStore and MemoryChatStore return
// identical ChatRow values for the same write DTO.
func TestCrossStoreChatParity(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 7, 17, 9, 30, 0, 0, time.UTC)
	write := Chat{ID: "chat_parity", UserID: "usr_owner", Title: "Parity", KeyVersion: 1, Blob: []byte("sealed-blob"), CreatedAt: created, UpdatedAt: created}

	st := openMigratedTestSQLite(t)
	defer st.Close()
	seedChatUser(t, st, "usr_owner")
	if err := st.CreateChat(ctx, write); err != nil {
		t.Fatalf("sqlite CreateChat: %v", err)
	}
	sqlRow, err := st.ChatByID(ctx, "chat_parity")
	if err != nil {
		t.Fatalf("sqlite ChatByID: %v", err)
	}

	mem := NewMemoryChatStore(0)
	if err := mem.CreateChat(ctx, write); err != nil {
		t.Fatalf("memory CreateChat: %v", err)
	}
	memRow, err := mem.ChatByID(ctx, "chat_parity")
	if err != nil {
		t.Fatalf("memory ChatByID: %v", err)
	}

	if sqlRow.ID != memRow.ID || sqlRow.UserID != memRow.UserID || sqlRow.Title != memRow.Title || sqlRow.KeyVersion != memRow.KeyVersion {
		t.Fatalf("parity mismatch: sqlite=%#v memory=%#v", sqlRow, memRow)
	}
	if !bytes.Equal(sqlRow.Blob, memRow.Blob) {
		t.Fatalf("Blob: sqlite=%v memory=%v", sqlRow.Blob, memRow.Blob)
	}
}

// TestMemoryChatStoreEvictsOldest proves the byte-FIFO eviction: over budget,
// the oldest chat is dropped while the newest survives.
func TestMemoryChatStoreEvictsOldest(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	// Budget fits ~1.5 blobs of 8 bytes; inserting two evicts the first.
	m := NewMemoryChatStore(12)
	if err := m.CreateChat(ctx, Chat{ID: "chat_old", UserID: "u", Title: "old", Blob: []byte("aaaaaaaa"), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateChat old: %v", err)
	}
	if err := m.CreateChat(ctx, Chat{ID: "chat_new", UserID: "u", Title: "new", Blob: []byte("bbbbbbbb"), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateChat new: %v", err)
	}
	if _, err := m.ChatByID(ctx, "chat_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("chat_old = %v, want ErrNotFound (evicted, oldest)", err)
	}
	if _, err := m.ChatByID(ctx, "chat_new"); err != nil {
		t.Fatalf("chat_new = %v, want present (newest survives)", err)
	}
}
