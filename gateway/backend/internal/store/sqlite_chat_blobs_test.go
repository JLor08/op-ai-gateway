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

// seedChatForBlobs creates a user and one chat so chat_blobs rows have a chat
// to reference (chat_blobs.chat_id is a foreign key to chats(id)).
func seedChatForBlobs(ctx context.Context, t *testing.T, s *SQLStore, userID, chatID string, now time.Time) {
	t.Helper()
	if err := s.CreateUser(ctx, newTestUser(userID, userID+"@example.test", now)); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
	if err := s.CreateChat(ctx, Chat{
		ID: chatID, UserID: userID, Title: "t", KeyVersion: 1, Blob: []byte("{}"),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed chat %s: %v", chatID, err)
	}
}

// TestChatBlobPutGetRoundTrip pins that the store is a dumb byte store: every
// field of a ChatBlob (the opaque bytes -- including NULs and non-UTF-8 --
// plus key_version, content_type, byte_len and created_at) comes back exactly
// as it was put, on both dialects. byte_len is deliberately past int32, since
// the column is bigint and the DTO field is int64.
func TestChatBlobPutGetRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		seedChatForBlobs(ctx, t, s, "u_rt", "c_rt", now)

		want := ChatBlob{
			ID:          "blob_rt",
			ChatID:      "c_rt",
			KeyVersion:  3,
			Blob:        []byte{0x00, 0xff, 0x10, 'x', 0x00, 0x80},
			ContentType: "image/png",
			ByteLen:     5_000_000_000,
			CreatedAt:   now.Add(time.Minute),
		}
		if err := s.PutChatBlob(ctx, want); err != nil {
			t.Fatalf("PutChatBlob: %v", err)
		}

		got, err := s.ChatBlobByID(ctx, "blob_rt")
		if err != nil {
			t.Fatalf("ChatBlobByID: %v", err)
		}
		if got.ID != want.ID || got.ChatID != want.ChatID || got.KeyVersion != want.KeyVersion ||
			got.ContentType != want.ContentType || got.ByteLen != want.ByteLen {
			t.Fatalf("ChatBlobByID = %#v, want metadata of %#v", got, want)
		}
		if !bytes.Equal(got.Blob, want.Blob) {
			t.Fatalf("blob = %x, want %x", got.Blob, want.Blob)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("created_at = %v, want %v", got.CreatedAt, want.CreatedAt)
		}
	})
}

// TestChatBlobPutRefusesDuplicateAndOrphan pins the write-side error mapping,
// the same one CreateChat uses: a blob id already taken is ErrConflict (the
// first row survives untouched), and a blob for a chat that does not exist is
// ErrNotFound (the foreign-key violation), not an opaque driver error.
func TestChatBlobPutRefusesDuplicateAndOrphan(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		seedChatForBlobs(ctx, t, s, "u_dup", "c_dup", now)

		first := ChatBlob{ID: "blob_dup", ChatID: "c_dup", KeyVersion: 1, Blob: []byte("first"), ContentType: "image/png", ByteLen: 5, CreatedAt: now}
		if err := s.PutChatBlob(ctx, first); err != nil {
			t.Fatalf("PutChatBlob: %v", err)
		}

		second := first
		second.Blob = []byte("second")
		if err := s.PutChatBlob(ctx, second); !errors.Is(err, ErrConflict) {
			t.Fatalf("PutChatBlob with a taken id = %v, want ErrConflict", err)
		}
		got, err := s.ChatBlobByID(ctx, "blob_dup")
		if err != nil {
			t.Fatalf("ChatBlobByID: %v", err)
		}
		if !bytes.Equal(got.Blob, []byte("first")) {
			t.Fatalf("blob after a refused duplicate = %q, want the original %q", got.Blob, "first")
		}

		orphan := ChatBlob{ID: "blob_orphan", ChatID: "c_missing", KeyVersion: 1, Blob: []byte("x"), ContentType: "image/png", ByteLen: 1, CreatedAt: now}
		if err := s.PutChatBlob(ctx, orphan); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PutChatBlob for a missing chat = %v, want ErrNotFound", err)
		}
	})
}

// TestChatBlobByIDMissingIsErrNotFound pins that a missing id is the
// store.ErrNotFound sentinel (sql.ErrNoRows mapped, as ChatByID does) and
// yields the zero value, so a handler can errors.Is it into a 404.
func TestChatBlobByIDMissingIsErrNotFound(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		got, err := s.ChatBlobByID(context.Background(), "blob_nope")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(missing) = %v, want ErrNotFound", err)
		}
		if got.ID != "" || got.Blob != nil {
			t.Fatalf("ChatBlobByID(missing) = %#v, want the zero ChatBlob", got)
		}
	})
}

// TestChatBlobOwner pins that ChatBlobOwner returns the owning chat id from
// metadata alone (it selects chat_id only, never the blob) and maps a missing
// id to ErrNotFound.
func TestChatBlobOwner(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		seedChatForBlobs(ctx, t, s, "u_own", "c_own", now)
		if err := s.PutChatBlob(ctx, ChatBlob{ID: "blob_own", ChatID: "c_own", KeyVersion: 1, Blob: []byte("x"), ContentType: "image/png", ByteLen: 1, CreatedAt: now}); err != nil {
			t.Fatalf("PutChatBlob: %v", err)
		}

		chatID, err := s.ChatBlobOwner(ctx, "blob_own")
		if err != nil {
			t.Fatalf("ChatBlobOwner: %v", err)
		}
		if chatID != "c_own" {
			t.Fatalf("ChatBlobOwner = %q, want c_own", chatID)
		}

		if chatID, err := s.ChatBlobOwner(ctx, "blob_nope"); !errors.Is(err, ErrNotFound) || chatID != "" {
			t.Fatalf("ChatBlobOwner(missing) = %q, %v, want \"\", ErrNotFound", chatID, err)
		}
	})
}

// TestChatBlobsByIDsIsScopedToTheChat pins ownership by construction: the
// batch read is ONE `chat_id = ? and id in (...)` query, so a blob id that
// exists but belongs to ANOTHER chat is simply not returned -- the caller
// cannot read across chats by naming an id, and a service layer that forgets a
// separate owner check still cannot leak. An id that does not exist is absent
// too, and the returned rows carry their real payload.
func TestChatBlobsByIDsIsScopedToTheChat(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		seedChatForBlobs(ctx, t, s, "u_a", "c_a", now)
		seedChatForBlobs(ctx, t, s, "u_b", "c_b", now)

		for _, b := range []ChatBlob{
			{ID: "blob_a1", ChatID: "c_a", KeyVersion: 1, Blob: []byte("a1-bytes"), ContentType: "image/png", ByteLen: 8, CreatedAt: now},
			{ID: "blob_a2", ChatID: "c_a", KeyVersion: 0, Blob: []byte("a2"), ContentType: "image/jpeg", ByteLen: 2, CreatedAt: now},
			{ID: "blob_b1", ChatID: "c_b", KeyVersion: 1, Blob: []byte("b1-secret"), ContentType: "image/webp", ByteLen: 9, CreatedAt: now},
		} {
			if err := s.PutChatBlob(ctx, b); err != nil {
				t.Fatalf("PutChatBlob %s: %v", b.ID, err)
			}
		}

		// blob_b1 belongs to c_b; blob_ghost does not exist at all.
		got, err := s.ChatBlobsByIDs(ctx, "c_a", []string{"blob_a1", "blob_a2", "blob_b1", "blob_ghost"})
		if err != nil {
			t.Fatalf("ChatBlobsByIDs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ChatBlobsByIDs returned %d rows %v, want exactly blob_a1 and blob_a2", len(got), got)
		}
		if _, leaked := got["blob_b1"]; leaked {
			t.Fatalf("ChatBlobsByIDs(c_a) returned blob_b1, which belongs to c_b")
		}
		a1, ok := got["blob_a1"]
		if !ok || a1.ChatID != "c_a" || a1.ContentType != "image/png" || a1.KeyVersion != 1 || a1.ByteLen != 8 || !bytes.Equal(a1.Blob, []byte("a1-bytes")) {
			t.Fatalf("blob_a1 = %#v (present=%v)", a1, ok)
		}
		a2, ok := got["blob_a2"]
		if !ok || a2.ContentType != "image/jpeg" || a2.KeyVersion != 0 || !bytes.Equal(a2.Blob, []byte("a2")) {
			t.Fatalf("blob_a2 = %#v (present=%v)", a2, ok)
		}

		// The same id list under the other chat flips the answer: ownership
		// is decided by the chat_id argument, not by the id.
		other, err := s.ChatBlobsByIDs(ctx, "c_b", []string{"blob_a1", "blob_a2", "blob_b1"})
		if err != nil {
			t.Fatalf("ChatBlobsByIDs(c_b): %v", err)
		}
		if len(other) != 1 || !bytes.Equal(other["blob_b1"].Blob, []byte("b1-secret")) {
			t.Fatalf("ChatBlobsByIDs(c_b) = %v, want only blob_b1", other)
		}
	})
}

// TestChatBlobsByIDsEmptyIDsDoesNoQuery pins the empty-slice contract: an empty
// (or nil) id list returns an empty non-nil map and no error WITHOUT touching
// the database. The cancelled context is the proof of "no query": any
// statement issued on it would fail, so a nil error means none was issued.
func TestChatBlobsByIDsEmptyIDsDoesNoQuery(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		for name, ids := range map[string][]string{"empty": {}, "nil": nil} {
			got, err := s.ChatBlobsByIDs(ctx, "c_any", ids)
			if err != nil {
				t.Fatalf("%s ids: ChatBlobsByIDs = %v, want nil error (and no query)", name, err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("%s ids: ChatBlobsByIDs = %#v, want an empty non-nil map", name, got)
			}
		}
	})
}

// TestSaveChatWithBlobsCancelledContextPersistsNothing pins that a context that
// is already cancelled fails the save before anything is written: the
// transaction never opens, so neither the chat update nor a blob can persist. It
// is SQL-only on purpose -- the in-RAM store has no I/O to cancel and, like every
// other MemoryChatStore method, does not consult its context.
func TestSaveChatWithBlobsCancelledContextPersistsNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		seedChatForBlobs(ctx, t, s, "u_cancel", "c_cancel", now)
		before, err := s.ChatByID(ctx, "c_cancel")
		if err != nil {
			t.Fatalf("ChatByID: %v", err)
		}

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		updated := Chat{ID: "c_cancel", Title: "must not stick", KeyVersion: 5, Blob: []byte("nope"), UpdatedAt: now.Add(time.Hour)}
		err = s.SaveChatWithBlobs(cancelled, updated, []ChatBlob{
			{ID: "blob_cancel", ChatID: "c_cancel", KeyVersion: 5, Blob: []byte("x"), ContentType: "image/png", ByteLen: 1, CreatedAt: now},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveChatWithBlobs(cancelled ctx) = %v, want context.Canceled", err)
		}

		after, err := s.ChatByID(ctx, "c_cancel")
		if err != nil {
			t.Fatalf("ChatByID: %v", err)
		}
		if after.Title != before.Title || after.KeyVersion != before.KeyVersion || !bytes.Equal(after.Blob, before.Blob) || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("chat after a cancelled save = %#v, want the unchanged %#v", after, before)
		}
		if _, err := s.ChatBlobByID(ctx, "blob_cancel"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(blob_cancel) = %v, want ErrNotFound", err)
		}
	})
}
