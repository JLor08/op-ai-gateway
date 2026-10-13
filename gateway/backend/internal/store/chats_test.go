// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// chatSaver is the surface the SaveChatWithBlobs tests need from a driver: the
// chat and chat_blobs method sets plus the atomic primitive itself. Memory,
// sqlite and postgres all sit behind it, so one body of assertions is the
// three-way parity proof.
type chatSaver interface {
	chatStore
	chatBlobStore
	SaveChatWithBlobs(ctx context.Context, chat Chat, blobs []ChatBlob) error
}

var (
	_ chatSaver = (*SQLStore)(nil)
	_ chatSaver = (*MemoryChatStore)(nil)
)

// forEachChatSaver runs run against a fresh store of every driver: the memory
// store always, sqlite always, postgres only when OP_AI_GATEWAY_TEST_POSTGRES_DSN
// is set (forEachDialect skips that leg otherwise -- the DSN must be provisioned
// like CI to get the third leg, or the postgres subtest silently does not run).
// The SQL drivers get the user "u" the memChat fixtures belong to, so a chat
// created in run satisfies chats.user_id's foreign key.
func forEachChatSaver(t *testing.T, run func(t *testing.T, s chatSaver)) {
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryChatStore(0))
	})
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		if err := s.CreateUser(context.Background(), newTestUser("u", "u@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		run(t, s)
	})
}

// requireBlobAbsent fails unless id is not stored at all: not by id, not by
// owner, and not in its chat's ChatBlobsByIDs.
func requireBlobAbsent(ctx context.Context, t *testing.T, s chatSaver, chatID, id string) {
	t.Helper()
	if _, err := s.ChatBlobByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChatBlobByID(%s) = %v, want ErrNotFound", id, err)
	}
	if _, err := s.ChatBlobOwner(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChatBlobOwner(%s) = %v, want ErrNotFound", id, err)
	}
	if got, err := s.ChatBlobsByIDs(ctx, chatID, []string{id}); err != nil || len(got) != 0 {
		t.Fatalf("ChatBlobsByIDs(%s, %s) = %v, %v, want empty", chatID, id, got, err)
	}
}

// requireBlobEquals fails unless the stored blob matches want in every field.
func requireBlobEquals(ctx context.Context, t *testing.T, s chatSaver, want ChatBlob) {
	t.Helper()
	got, err := s.ChatBlobByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("ChatBlobByID(%s): %v", want.ID, err)
	}
	if got.ID != want.ID || got.ChatID != want.ChatID || got.KeyVersion != want.KeyVersion ||
		got.ContentType != want.ContentType || got.ByteLen != want.ByteLen {
		t.Fatalf("ChatBlobByID(%s) = %#v, want metadata of %#v", want.ID, got, want)
	}
	if !bytes.Equal(got.Blob, want.Blob) {
		t.Fatalf("ChatBlobByID(%s) payload = %x, want %x", want.ID, got.Blob, want.Blob)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("ChatBlobByID(%s) created_at = %v, want %v", want.ID, got.CreatedAt, want.CreatedAt)
	}
}

// requireChatEquals fails unless ChatByID(want.ID) returns exactly want: the
// mutable columns a save writes and the owner and creation time it must not.
func requireChatEquals(ctx context.Context, t *testing.T, s chatSaver, want Chat) {
	t.Helper()
	got, err := s.ChatByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("ChatByID(%s): %v", want.ID, err)
	}
	if got.UserID != want.UserID || got.Title != want.Title || got.KeyVersion != want.KeyVersion ||
		!bytes.Equal(got.Blob, want.Blob) || !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("ChatByID(%s) = %#v, want %#v", want.ID, got, want)
	}
}

// TestSaveChatWithBlobsPersistsChatAndBlobs pins the success path on all three
// drivers: one call overwrites the chat's mutable columns (title, key version,
// sealed document, updated_at) and stores both new blobs, every field exactly as
// given. The chat's owner and creation time are not part of the update, and a
// blob the chat already had is left alone.
func TestSaveChatWithBlobsPersistsChatAndBlobs(t *testing.T) {
	forEachChatSaver(t, func(t *testing.T, s chatSaver) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		original := memChat("c_save", 3, now)
		if err := s.CreateChat(ctx, original); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		existing := memBlob("blob_existing", "c_save", 4, now)
		if err := s.PutChatBlob(ctx, existing); err != nil {
			t.Fatalf("PutChatBlob: %v", err)
		}

		updated := Chat{
			ID: "c_save", UserID: "ignored-by-update", Title: "renamed", KeyVersion: 2,
			Blob: []byte("new sealed document"), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Minute),
		}
		first := ChatBlob{
			ID: "blob_one", ChatID: "c_save", KeyVersion: 2, Blob: []byte{0x00, 0xff, 0x10, 'x'},
			ContentType: "image/png", ByteLen: 4, CreatedAt: now.Add(time.Minute),
		}
		second := ChatBlob{
			ID: "blob_two", ChatID: "c_save", KeyVersion: 2, Blob: []byte("second payload"),
			ContentType: "image/jpeg", ByteLen: 5_000_000_000, CreatedAt: now.Add(2 * time.Minute),
		}
		if err := s.SaveChatWithBlobs(ctx, updated, []ChatBlob{first, second}); err != nil {
			t.Fatalf("SaveChatWithBlobs: %v", err)
		}

		// UserID and CreatedAt are not updated: the original ones survive.
		requireChatEquals(ctx, t, s, Chat{
			ID: "c_save", UserID: original.UserID, Title: "renamed", KeyVersion: 2,
			Blob: updated.Blob, CreatedAt: original.CreatedAt, UpdatedAt: updated.UpdatedAt,
		})
		requireBlobEquals(ctx, t, s, first)
		requireBlobEquals(ctx, t, s, second)
		requireBlobEquals(ctx, t, s, existing)
		for _, id := range []string{"blob_one", "blob_two"} {
			if owner, err := s.ChatBlobOwner(ctx, id); err != nil || owner != "c_save" {
				t.Fatalf("ChatBlobOwner(%s) = %q, %v, want c_save", id, owner, err)
			}
		}
		byIDs, err := s.ChatBlobsByIDs(ctx, "c_save", []string{"blob_one", "blob_two", "blob_existing"})
		if err != nil || len(byIDs) != 3 {
			t.Fatalf("ChatBlobsByIDs = %v, %v, want all three blobs of c_save", byIDs, err)
		}
	})
}

// TestSaveChatWithBlobsEmptyBlobsIsAChatUpdate pins that a save with nothing to
// extract (nil or empty blobs -- the common run that produced no new image) is
// exactly UpdateChat, including ErrNotFound for a chat that is not there.
func TestSaveChatWithBlobsEmptyBlobsIsAChatUpdate(t *testing.T) {
	forEachChatSaver(t, func(t *testing.T, s chatSaver) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		original := memChat("c_plain", 3, now)
		if err := s.CreateChat(ctx, original); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}

		for i, blobs := range [][]ChatBlob{nil, {}} {
			updated := Chat{
				ID: "c_plain", Title: fmt.Sprintf("title %d", i), KeyVersion: 1, Blob: []byte{byte('p' + i)},
				UpdatedAt: now.Add(time.Duration(i+1) * time.Minute),
			}
			if err := s.SaveChatWithBlobs(ctx, updated, blobs); err != nil {
				t.Fatalf("SaveChatWithBlobs(blobs=%#v): %v", blobs, err)
			}
			requireChatEquals(ctx, t, s, Chat{
				ID: "c_plain", UserID: original.UserID, Title: updated.Title, KeyVersion: 1,
				Blob: updated.Blob, CreatedAt: original.CreatedAt, UpdatedAt: updated.UpdatedAt,
			})
		}

		missing := Chat{ID: "c_nope", Title: "x", Blob: []byte("y"), UpdatedAt: now}
		if err := s.SaveChatWithBlobs(ctx, missing, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SaveChatWithBlobs(missing chat, no blobs) = %v, want ErrNotFound", err)
		}
	})
}

// TestSaveChatWithBlobsIsAllOrNothing is the atomicity proof, on all three
// drivers. Every case makes SaveChatWithBlobs fail, and in every case the
// failure comes AFTER something that would persist on its own: the chat update
// and the first, valid blob. All of it must be undone -- the chat row keeps its
// pre-save values, the valid blob is not stored, and the rows that were already
// there (the chat's own blob and another chat's) are untouched.
func TestSaveChatWithBlobsIsAllOrNothing(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		blobs   []ChatBlob // blob_ok is always valid and first; a later one is the failure
		wantErr error      // nil: any non-nil error
	}{
		{
			name:    "a later blob reuses an id already taken",
			blobs:   []ChatBlob{memBlob("blob_ok", "c_fail", 5, now), memBlob("blob_existing", "c_fail", 6, now)},
			wantErr: ErrConflict,
		},
		{
			name:    "a later blob reuses an id taken by another chat",
			blobs:   []ChatBlob{memBlob("blob_ok", "c_fail", 5, now), memBlob("blob_other", "c_fail", 6, now)},
			wantErr: ErrConflict,
		},
		{
			name:    "two blobs of the batch share an id",
			blobs:   []ChatBlob{memBlob("blob_ok", "c_fail", 5, now), memBlob("blob_twice", "c_fail", 6, now), memBlob("blob_twice", "c_fail", 7, now)},
			wantErr: ErrConflict,
		},
		{
			name:  "a later blob belongs to a different chat",
			blobs: []ChatBlob{memBlob("blob_ok", "c_fail", 5, now), memBlob("blob_stray", "c_other", 6, now)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachChatSaver(t, func(t *testing.T, s chatSaver) {
				ctx := context.Background()
				before := memChat("c_fail", 3, now)
				other := memChat("c_other", 2, now)
				for _, c := range []Chat{before, other} {
					if err := s.CreateChat(ctx, c); err != nil {
						t.Fatalf("CreateChat %s: %v", c.ID, err)
					}
				}
				existing := memBlob("blob_existing", "c_fail", 4, now)
				otherBlob := memBlob("blob_other", "c_other", 4, now)
				for _, b := range []ChatBlob{existing, otherBlob} {
					if err := s.PutChatBlob(ctx, b); err != nil {
						t.Fatalf("PutChatBlob %s: %v", b.ID, err)
					}
				}

				updated := Chat{
					ID: "c_fail", Title: "must not stick", KeyVersion: 9, Blob: []byte("must not stick"),
					UpdatedAt: now.Add(time.Hour),
				}
				err := s.SaveChatWithBlobs(ctx, updated, tc.blobs)
				if err == nil {
					t.Fatalf("SaveChatWithBlobs succeeded, want a failure")
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("SaveChatWithBlobs = %v, want %v", err, tc.wantErr)
				}

				requireChatEquals(ctx, t, s, before)
				requireChatEquals(ctx, t, s, other)
				requireBlobAbsent(ctx, t, s, "c_fail", "blob_ok")
				requireBlobAbsent(ctx, t, s, "c_fail", "blob_twice")
				requireBlobAbsent(ctx, t, s, "c_other", "blob_stray")
				requireBlobEquals(ctx, t, s, existing)
				requireBlobEquals(ctx, t, s, otherBlob)
			})
		})
	}
}

// TestSaveChatWithBlobsMissingChatPersistsNothing pins the other half of the
// error mapping: saving into a chat that does not exist is ErrNotFound and,
// again, none of the batch is stored (and the save does not create the chat).
func TestSaveChatWithBlobsMissingChatPersistsNothing(t *testing.T) {
	forEachChatSaver(t, func(t *testing.T, s chatSaver) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		ghost := Chat{ID: "c_ghost", Title: "x", KeyVersion: 1, Blob: []byte("y"), UpdatedAt: now}
		err := s.SaveChatWithBlobs(ctx, ghost, []ChatBlob{memBlob("blob_a", "c_ghost", 3, now), memBlob("blob_b", "c_ghost", 3, now)})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("SaveChatWithBlobs(missing chat) = %v, want ErrNotFound", err)
		}
		if _, err := s.ChatByID(ctx, "c_ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatByID(c_ghost) = %v, want ErrNotFound (a save must not create the chat)", err)
		}
		requireBlobAbsent(ctx, t, s, "c_ghost", "blob_a")
		requireBlobAbsent(ctx, t, s, "c_ghost", "blob_b")
	})
}
