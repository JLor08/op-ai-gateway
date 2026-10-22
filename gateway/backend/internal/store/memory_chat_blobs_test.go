// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// chatBlobStore is the chat_blobs method set both SQLStore and MemoryChatStore
// expose, so the two drivers can sit behind one interface.
type chatBlobStore interface {
	PutChatBlob(ctx context.Context, blob ChatBlob) error
	ChatBlobByID(ctx context.Context, id string) (ChatBlob, error)
	ChatBlobOwner(ctx context.Context, id string) (string, error)
	ChatBlobsByIDs(ctx context.Context, chatID string, ids []string) (map[string]ChatBlob, error)
}

var (
	_ chatBlobStore = (*SQLStore)(nil)
	_ chatBlobStore = (*MemoryChatStore)(nil)
)

// memBlob builds a ChatBlob of n payload bytes belonging to chatID.
func memBlob(id, chatID string, n int, now time.Time) ChatBlob {
	return ChatBlob{
		ID:          id,
		ChatID:      chatID,
		KeyVersion:  1,
		Blob:        bytes.Repeat([]byte{'x'}, n),
		ContentType: "image/png",
		ByteLen:     int64(n),
		CreatedAt:   now,
	}
}

// memChat builds a Chat whose sealed transcript is n bytes.
func memChat(id string, n int, now time.Time) Chat {
	return Chat{ID: id, UserID: "u", Title: id, KeyVersion: 1, Blob: bytes.Repeat([]byte{'t'}, n), CreatedAt: now, UpdatedAt: now}
}

// mustCreateChat and mustPutBlob keep the cascade/budget tests readable: they
// are setup, never the thing under test.
func mustCreateChat(ctx context.Context, t *testing.T, m *MemoryChatStore, c Chat) {
	t.Helper()
	if err := m.CreateChat(ctx, c); err != nil {
		t.Fatalf("CreateChat %s: %v", c.ID, err)
	}
}

func mustPutBlob(ctx context.Context, t *testing.T, m *MemoryChatStore, b ChatBlob) {
	t.Helper()
	if err := m.PutChatBlob(ctx, b); err != nil {
		t.Fatalf("PutChatBlob %s: %v", b.ID, err)
	}
}

// TestMemoryChatBlobPutGetRoundTrip pins that the in-RAM store, like the SQL
// one, is a dumb byte store: every field of a ChatBlob comes back as put (the
// opaque bytes including NULs, key_version, content_type, a byte_len past int32
// and created_at), and ChatBlobOwner / ChatBlobsByIDs agree with ChatBlobByID.
func TestMemoryChatBlobPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_rt", 2, now))

	want := ChatBlob{
		ID:          "blob_rt",
		ChatID:      "c_rt",
		KeyVersion:  3,
		Blob:        []byte{0x00, 0xff, 0x10, 'x', 0x00, 0x80},
		ContentType: "image/png",
		ByteLen:     5_000_000_000,
		CreatedAt:   now.Add(time.Minute),
	}
	mustPutBlob(ctx, t, m, want)

	// The budget counts the SEALED payload (len(Blob)), never the ByteLen
	// metadata: want's ByteLen (5e9, the plaintext size a real blob would
	// report) differs from its 6 stored bytes on purpose, so counting ByteLen
	// instead would blow the budget by orders of magnitude and fail here.
	if wantBytes := 2 + len(want.Blob); m.bytes != wantBytes {
		t.Fatalf("bytes = %d after the put, want %d (the chat's 2 + len(Blob), not ByteLen)", m.bytes, wantBytes)
	}

	got, err := m.ChatBlobByID(ctx, "blob_rt")
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

	chatID, err := m.ChatBlobOwner(ctx, "blob_rt")
	if err != nil || chatID != "c_rt" {
		t.Fatalf("ChatBlobOwner = %q, %v, want c_rt", chatID, err)
	}
	byIDs, err := m.ChatBlobsByIDs(ctx, "c_rt", []string{"blob_rt"})
	if err != nil {
		t.Fatalf("ChatBlobsByIDs: %v", err)
	}
	if len(byIDs) != 1 || !bytes.Equal(byIDs["blob_rt"].Blob, want.Blob) {
		t.Fatalf("ChatBlobsByIDs = %#v, want the one blob", byIDs)
	}
}

// TestMemoryChatBlobPutRefusesDuplicateAndOrphan pins the write-side error
// mapping SQLStore.PutChatBlob has: a blob whose chat does not exist is
// ErrNotFound (the foreign-key violation), an id already taken is ErrConflict,
// and a refused put never touches the stored blob or the byte budget.
func TestMemoryChatBlobPutRefusesDuplicateAndOrphan(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_dup", 2, now))
	mustPutBlob(ctx, t, m, ChatBlob{ID: "blob_dup", ChatID: "c_dup", KeyVersion: 1, Blob: []byte("first"), ContentType: "image/png", ByteLen: 5, CreatedAt: now})
	before := m.bytes

	second := ChatBlob{ID: "blob_dup", ChatID: "c_dup", KeyVersion: 1, Blob: []byte("second!!"), ContentType: "image/png", ByteLen: 8, CreatedAt: now}
	if err := m.PutChatBlob(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("PutChatBlob with a taken id = %v, want ErrConflict", err)
	}
	got, err := m.ChatBlobByID(ctx, "blob_dup")
	if err != nil {
		t.Fatalf("ChatBlobByID: %v", err)
	}
	if !bytes.Equal(got.Blob, []byte("first")) {
		t.Fatalf("blob = %q after a refused duplicate, want the original %q", got.Blob, "first")
	}

	orphan := ChatBlob{ID: "blob_orphan", ChatID: "c_missing", KeyVersion: 1, Blob: []byte("x"), ContentType: "image/png", ByteLen: 1, CreatedAt: now}
	if err := m.PutChatBlob(ctx, orphan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PutChatBlob for a missing chat = %v, want ErrNotFound", err)
	}
	if _, err := m.ChatBlobByID(ctx, "blob_orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan blob was stored anyway: ChatBlobByID = %v, want ErrNotFound", err)
	}
	if m.bytes != before {
		t.Fatalf("bytes = %d after refused puts, want %d (a refused put must not be counted)", m.bytes, before)
	}
}

// TestMemoryChatBlobMissingIsErrNotFound pins the miss path of the two
// single-id reads: ErrNotFound and the zero value.
func TestMemoryChatBlobMissingIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryChatStore(0)

	got, err := m.ChatBlobByID(ctx, "blob_nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChatBlobByID(missing) = %v, want ErrNotFound", err)
	}
	if got.ID != "" || got.Blob != nil {
		t.Fatalf("ChatBlobByID(missing) = %#v, want the zero ChatBlob", got)
	}
	if chatID, err := m.ChatBlobOwner(ctx, "blob_nope"); !errors.Is(err, ErrNotFound) || chatID != "" {
		t.Fatalf("ChatBlobOwner(missing) = %q, %v, want \"\", ErrNotFound", chatID, err)
	}
}

// TestMemoryChatBlobsByIDsIsScopedToTheChat pins ownership by construction,
// exactly as the SQL driver has it: an id that exists but belongs to another
// chat is simply absent from the result, like an id that does not exist at all.
func TestMemoryChatBlobsByIDsIsScopedToTheChat(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_a", 2, now))
	mustCreateChat(ctx, t, m, memChat("c_b", 2, now))
	for _, b := range []ChatBlob{
		memBlob("blob_a1", "c_a", 3, now),
		memBlob("blob_a2", "c_a", 3, now),
		memBlob("blob_b1", "c_b", 3, now),
	} {
		mustPutBlob(ctx, t, m, b)
	}

	got, err := m.ChatBlobsByIDs(ctx, "c_a", []string{"blob_a1", "blob_a2", "blob_b1", "blob_ghost"})
	if err != nil {
		t.Fatalf("ChatBlobsByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ChatBlobsByIDs returned %d rows %v, want exactly blob_a1 and blob_a2", len(got), got)
	}
	if _, leaked := got["blob_b1"]; leaked {
		t.Fatalf("ChatBlobsByIDs(c_a) returned blob_b1, which belongs to c_b")
	}
	for _, id := range []string{"blob_a1", "blob_a2"} {
		if got[id].ID != id || got[id].ChatID != "c_a" {
			t.Fatalf("ChatBlobsByIDs[%s] = %#v, want that blob of c_a", id, got[id])
		}
	}

	other, err := m.ChatBlobsByIDs(ctx, "c_b", []string{"blob_a1", "blob_a2", "blob_b1"})
	if err != nil {
		t.Fatalf("ChatBlobsByIDs(c_b): %v", err)
	}
	if len(other) != 1 || other["blob_b1"].ChatID != "c_b" {
		t.Fatalf("ChatBlobsByIDs(c_b) = %#v, want only blob_b1", other)
	}
}

// TestMemoryChatBlobsByIDsEmptyIDs pins the empty-ids path: an empty, non-nil
// map and no error, for both an empty and a nil id list.
func TestMemoryChatBlobsByIDsEmptyIDs(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryChatStore(0)
	for name, ids := range map[string][]string{"empty": {}, "nil": nil} {
		got, err := m.ChatBlobsByIDs(ctx, "c_any", ids)
		if err != nil {
			t.Fatalf("%s ids: ChatBlobsByIDs: %v", name, err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("%s ids: ChatBlobsByIDs = %#v, want an empty non-nil map", name, got)
		}
	}
}

// TestMemoryChatBlobBytesCountTowardTheBudget pins that blob bytes are budgeted
// like transcript bytes: the two chats' transcripts alone (4 bytes) sit far
// inside the 20-byte budget and would never evict, so the eviction below can
// only come from the blobs. Once it fires, the oldest chat goes -- and the
// remaining chat is kept as the sole entry even though its own blobs still
// exceed the budget (the "single oversize entry is kept" rule, applied to a
// chat together with its blobs).
func TestMemoryChatBlobBytesCountTowardTheBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(20)
	mustCreateChat(ctx, t, m, memChat("c_old", 2, now))
	mustCreateChat(ctx, t, m, memChat("c_new", 2, now))
	if m.bytes != 4 {
		t.Fatalf("bytes = %d after two 2-byte chats, want 4", m.bytes)
	}

	mustPutBlob(ctx, t, m, memBlob("blob_1", "c_new", 10, now))
	if m.bytes != 14 {
		t.Fatalf("bytes = %d after a 10-byte blob, want 14 (blob bytes count)", m.bytes)
	}
	if _, err := m.ChatByID(ctx, "c_old"); err != nil {
		t.Fatalf("c_old = %v, want present while within budget", err)
	}

	mustPutBlob(ctx, t, m, memBlob("blob_2", "c_new", 10, now))
	if _, err := m.ChatByID(ctx, "c_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c_old = %v, want ErrNotFound (blob bytes pushed the store over budget)", err)
	}
	if _, err := m.ChatByID(ctx, "c_new"); err != nil {
		t.Fatalf("c_new = %v, want present (sole entry is kept)", err)
	}
	for _, id := range []string{"blob_1", "blob_2"} {
		if _, err := m.ChatBlobByID(ctx, id); err != nil {
			t.Fatalf("ChatBlobByID(%s) = %v, want present (its chat is the sole entry)", id, err)
		}
	}
	if m.bytes != 22 {
		t.Fatalf("bytes = %d, want 22 (c_new's 2 + two 10-byte blobs; c_old's 2 subtracted)", m.bytes)
	}
}

// TestMemoryChatEvictDropsTheChatsBlobs pins cascade-on-evict: when the byte
// FIFO drops the oldest chat, that chat's blobs go with it -- unreachable by
// id, by owner and by chat -- and their bytes are subtracted from the budget,
// so the survivors' accounting is exact and the dropped blobs cannot linger as
// uncounted RAM.
func TestMemoryChatEvictDropsTheChatsBlobs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(40)
	mustCreateChat(ctx, t, m, memChat("c_old", 2, now))
	mustPutBlob(ctx, t, m, memBlob("old_1", "c_old", 8, now))
	mustPutBlob(ctx, t, m, memBlob("old_2", "c_old", 8, now))
	mustCreateChat(ctx, t, m, memChat("c_mid", 2, now))
	mustPutBlob(ctx, t, m, memBlob("mid_1", "c_mid", 8, now))
	if m.bytes != 28 {
		t.Fatalf("bytes = %d before eviction, want 28 (2+8+8 + 2+8)", m.bytes)
	}

	// 28 + 14 = 42 > 40: the oldest chat (c_old, 18 bytes with its blobs) must
	// go, and only it (42 - 18 = 24 fits).
	mustCreateChat(ctx, t, m, memChat("c_new", 14, now))

	if _, err := m.ChatByID(ctx, "c_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c_old = %v, want ErrNotFound (evicted)", err)
	}
	for _, id := range []string{"old_1", "old_2"} {
		if _, err := m.ChatBlobByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(%s) = %v, want ErrNotFound (cascade on evict)", id, err)
		}
		if _, err := m.ChatBlobOwner(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobOwner(%s) = %v, want ErrNotFound (cascade on evict)", id, err)
		}
	}
	if got, err := m.ChatBlobsByIDs(ctx, "c_old", []string{"old_1", "old_2"}); err != nil || len(got) != 0 {
		t.Fatalf("ChatBlobsByIDs(c_old) = %v, %v, want empty", got, err)
	}
	if _, err := m.ChatBlobByID(ctx, "mid_1"); err != nil {
		t.Fatalf("mid_1 = %v, want present (its chat was not evicted)", err)
	}
	if m.bytes != 24 {
		t.Fatalf("bytes = %d after eviction, want 24 (c_mid 2+8, c_new 14; c_old's 18 subtracted)", m.bytes)
	}
	if len(m.chatBlobIDs) != 1 || len(m.blobs) != 1 {
		t.Fatalf("index holds %d chats / %d blobs after eviction, want 1 / 1 (no leftovers for c_old)", len(m.chatBlobIDs), len(m.blobs))
	}
}

// TestMemoryDeleteChatDropsTheChatsBlobs pins cascade-on-DeleteChat: deleting a
// chat removes its blobs (a later read is ErrNotFound), subtracts their bytes
// from the budget, leaves every other chat's blobs alone, and a second
// DeleteChat is still ErrNotFound.
func TestMemoryDeleteChatDropsTheChatsBlobs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_gone", 2, now))
	mustCreateChat(ctx, t, m, memChat("c_stay", 3, now))
	mustPutBlob(ctx, t, m, memBlob("gone_1", "c_gone", 5, now))
	mustPutBlob(ctx, t, m, memBlob("gone_2", "c_gone", 7, now))
	mustPutBlob(ctx, t, m, memBlob("stay_1", "c_stay", 11, now))
	if m.bytes != 2+3+5+7+11 {
		t.Fatalf("bytes = %d before delete, want %d", m.bytes, 2+3+5+7+11)
	}

	if err := m.DeleteChat(ctx, "c_gone"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}

	for _, id := range []string{"gone_1", "gone_2"} {
		if _, err := m.ChatBlobByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(%s) after DeleteChat = %v, want ErrNotFound (cascade)", id, err)
		}
		if _, err := m.ChatBlobOwner(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobOwner(%s) after DeleteChat = %v, want ErrNotFound (cascade)", id, err)
		}
	}
	if got, err := m.ChatBlobsByIDs(ctx, "c_gone", []string{"gone_1", "gone_2"}); err != nil || len(got) != 0 {
		t.Fatalf("ChatBlobsByIDs(c_gone) = %v, %v, want empty", got, err)
	}
	if got, err := m.ChatBlobByID(ctx, "stay_1"); err != nil || got.ChatID != "c_stay" {
		t.Fatalf("stay_1 = %#v, %v, want it untouched by the other chat's delete", got, err)
	}
	if m.bytes != 3+11 {
		t.Fatalf("bytes = %d after DeleteChat, want %d (c_gone's 2+5+7 subtracted)", m.bytes, 3+11)
	}
	if err := m.DeleteChat(ctx, "c_gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteChat = %v, want ErrNotFound", err)
	}
	if m.bytes != 3+11 {
		t.Fatalf("bytes = %d after a refused second DeleteChat, want %d", m.bytes, 3+11)
	}

	// A chat can be re-created under the freed id and holds no stale blobs.
	mustCreateChat(ctx, t, m, memChat("c_gone", 2, now))
	if got, err := m.ChatBlobsByIDs(ctx, "c_gone", []string{"gone_1"}); err != nil || len(got) != 0 {
		t.Fatalf("re-created c_gone resurrected a blob: %v, %v", got, err)
	}
}

// TestMemoryChatBlobsConcurrentAccess is a -race tripwire: every blob map and
// index access sits under the store's one mutex, so puts, reads, deletes and
// budget-driven evictions of different chats can run together without a data
// race and without the byte accounting drifting from what is actually held.
func TestMemoryChatBlobsConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	// A small budget so evictions really interleave with the writers.
	m := NewMemoryChatStore(256)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				chatID := fmt.Sprintf("c_%d_%d", w, i)
				blobID := "b_" + chatID
				_ = m.CreateChat(ctx, memChat(chatID, 4, now))
				_ = m.PutChatBlob(ctx, memBlob(blobID, chatID, 16, now))
				_, _ = m.ChatBlobByID(ctx, blobID)
				_, _ = m.ChatBlobOwner(ctx, blobID)
				_, _ = m.ChatBlobsByIDs(ctx, chatID, []string{blobID})
				if i%3 == 0 {
					_ = m.DeleteChat(ctx, chatID)
				}
			}
		}()
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	held := 0
	for _, rec := range m.records {
		held += len(rec.Blob)
	}
	for _, b := range m.blobs {
		held += len(b.Blob)
	}
	if m.bytes != held {
		t.Fatalf("bytes = %d, but the store holds %d: accounting drifted under concurrency", m.bytes, held)
	}
	for id, b := range m.blobs {
		if _, ok := m.records[b.ChatID]; !ok {
			t.Fatalf("blob %s outlived its chat %s", id, b.ChatID)
		}
	}
}

// TestMemorySaveChatWithBlobsEvictsOnceAtTheEnd pins why the save is staged and
// evicted a single time rather than looping PutChatBlob. The store is seeded so
// the chat being saved is the OLDEST entry and its old transcript is large: the
// save shrinks that transcript from 20 to 4 bytes and adds two 10-byte blobs, so
// the finished state (2 + 4 + 20 = 26) fits the 30-byte budget, but applying the
// blobs one by one on top of the old transcript (2 + 20 + 10 = 32 after the
// first) would not -- and a per-blob eviction would drop this very chat (and
// then fail the second blob with ErrNotFound). Nothing must be evicted here.
func TestMemorySaveChatWithBlobsEvictsOnceAtTheEnd(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(30)
	mustCreateChat(ctx, t, m, memChat("c_save", 20, now)) // oldest
	mustCreateChat(ctx, t, m, memChat("c_other", 2, now))
	if m.bytes != 22 {
		t.Fatalf("bytes = %d before the save, want 22", m.bytes)
	}

	shrunk := Chat{ID: "c_save", Title: "c_save", KeyVersion: 1, Blob: bytes.Repeat([]byte{'s'}, 4), UpdatedAt: now.Add(time.Minute)}
	blobs := []ChatBlob{memBlob("blob_1", "c_save", 10, now), memBlob("blob_2", "c_save", 10, now)}
	if err := m.SaveChatWithBlobs(ctx, shrunk, blobs, nil); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	for _, id := range []string{"c_save", "c_other"} {
		if _, err := m.ChatByID(ctx, id); err != nil {
			t.Fatalf("ChatByID(%s) = %v, want present (the finished state fits the budget)", id, err)
		}
	}
	for _, id := range []string{"blob_1", "blob_2"} {
		if got, err := m.ChatBlobByID(ctx, id); err != nil || got.ChatID != "c_save" {
			t.Fatalf("ChatBlobByID(%s) = %#v, %v, want present", id, got, err)
		}
	}
	if m.bytes != 26 {
		t.Fatalf("bytes = %d, want 26 (transcript 4 + c_other 2 + two 10-byte blobs)", m.bytes)
	}
}

// TestMemorySaveChatWithBlobsEvictsTheOldestAfterwards pins the eviction that
// does happen: once the save is applied, the byte FIFO runs and drops the oldest
// OTHER chat together with its blobs, with the budget accounting exact -- the
// saved chat and both new blobs survive.
func TestMemorySaveChatWithBlobsEvictsTheOldestAfterwards(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(40)
	mustCreateChat(ctx, t, m, memChat("c_old", 2, now))
	mustPutBlob(ctx, t, m, memBlob("old_1", "c_old", 8, now))
	mustCreateChat(ctx, t, m, memChat("c_save", 3, now))
	if m.bytes != 13 {
		t.Fatalf("bytes = %d before the save, want 13", m.bytes)
	}

	// 13 - 3 + 5 + 2*15 = 45 > 40: c_old (2 + 8 = 10) must go, and only it.
	grown := Chat{ID: "c_save", Title: "c_save", KeyVersion: 1, Blob: bytes.Repeat([]byte{'s'}, 5), UpdatedAt: now.Add(time.Minute)}
	blobs := []ChatBlob{memBlob("new_1", "c_save", 15, now), memBlob("new_2", "c_save", 15, now)}
	if err := m.SaveChatWithBlobs(ctx, grown, blobs, nil); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	if _, err := m.ChatByID(ctx, "c_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c_old = %v, want ErrNotFound (evicted, oldest)", err)
	}
	if _, err := m.ChatBlobByID(ctx, "old_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old_1 = %v, want ErrNotFound (cascade on evict)", err)
	}
	for _, id := range []string{"new_1", "new_2"} {
		if _, err := m.ChatBlobByID(ctx, id); err != nil {
			t.Fatalf("ChatBlobByID(%s) = %v, want present", id, err)
		}
	}
	if m.bytes != 35 {
		t.Fatalf("bytes = %d, want 35 (c_save 5 + two 15-byte blobs; c_old's 10 subtracted)", m.bytes)
	}
	if len(m.blobs) != 2 || len(m.chatBlobIDs) != 1 || len(m.records) != 1 || len(m.order) != 1 {
		t.Fatalf("store holds %d blobs / %d blob indexes / %d chats / %d order entries, want 2 / 1 / 1 / 1",
			len(m.blobs), len(m.chatBlobIDs), len(m.records), len(m.order))
	}
}

// TestMemorySaveChatWithBlobsEvictsTheSavedChatWhole pins the other edge of
// "evict once, at the end": when the saved chat is itself the oldest entry and
// the finished state is over budget, it is evicted like any chat -- but whole,
// with BOTH its blobs, not half-saved. The first blob alone (2 + 4 + 40) is
// already over the 30-byte budget, so evicting after each blob would drop the
// chat there and then store the second blob for a chat that is gone, leaving an
// orphan that outlives its chat as uncounted-for RAM.
func TestMemorySaveChatWithBlobsEvictsTheSavedChatWhole(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(30)
	mustCreateChat(ctx, t, m, memChat("c_save", 2, now)) // oldest
	mustCreateChat(ctx, t, m, memChat("c_other", 2, now))

	grown := Chat{ID: "c_save", Title: "c_save", KeyVersion: 1, Blob: bytes.Repeat([]byte{'s'}, 4), UpdatedAt: now.Add(time.Minute)}
	blobs := []ChatBlob{memBlob("blob_big", "c_save", 40, now), memBlob("blob_small", "c_save", 5, now)}
	if err := m.SaveChatWithBlobs(ctx, grown, blobs, nil); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	if _, err := m.ChatByID(ctx, "c_save"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c_save = %v, want ErrNotFound (oldest entry, finished state over budget)", err)
	}
	for _, id := range []string{"blob_big", "blob_small"} {
		if _, err := m.ChatBlobByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(%s) = %v, want ErrNotFound (an evicted chat takes ALL its blobs with it)", id, err)
		}
	}
	if _, err := m.ChatByID(ctx, "c_other"); err != nil {
		t.Fatalf("c_other = %v, want present", err)
	}
	if m.bytes != 2 || len(m.blobs) != 0 || len(m.chatBlobIDs) != 0 {
		t.Fatalf("bytes = %d, %d blobs, %d blob indexes, want 2 / 0 / 0 (only c_other's transcript is left)", m.bytes, len(m.blobs), len(m.chatBlobIDs))
	}
}

// TestMemorySaveChatWithBlobsRefusedLeavesTheStoreUntouched pins the staging
// half of the atomicity: every refusal -- a taken id, an id repeated inside the
// batch, a blob of another chat, a chat that is not held -- is decided before
// anything is applied, so the chat keeps its old fields and its old byte count,
// no blob of the batch (not even a valid one that came first) is stored or
// indexed, and the byte budget does not move. The refused saves also name a
// held blob for deletion (#281): none of them may delete it.
func TestMemorySaveChatWithBlobsRefusedLeavesTheStoreUntouched(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_a", 3, now))
	mustCreateChat(ctx, t, m, memChat("c_b", 2, now))
	mustPutBlob(ctx, t, m, memBlob("blob_a", "c_a", 4, now))
	mustPutBlob(ctx, t, m, memBlob("blob_b", "c_b", 4, now))
	bytesBefore := m.bytes
	orderBefore := append([]string(nil), m.order...)

	update := Chat{ID: "c_a", Title: "must not stick", KeyVersion: 7, Blob: []byte("much longer than before"), UpdatedAt: now.Add(time.Hour)}
	refusals := map[string]struct {
		chat    Chat
		blobs   []ChatBlob
		wantErr error // nil: any non-nil error
	}{
		"taken id":         {update, []ChatBlob{memBlob("blob_ok", "c_a", 5, now), memBlob("blob_a", "c_a", 5, now)}, ErrConflict},
		"id of other chat": {update, []ChatBlob{memBlob("blob_ok", "c_a", 5, now), memBlob("blob_b", "c_a", 5, now)}, ErrConflict},
		"id twice":         {update, []ChatBlob{memBlob("blob_ok", "c_a", 5, now), memBlob("blob_dup", "c_a", 5, now), memBlob("blob_dup", "c_a", 5, now)}, ErrConflict},
		"other chat":       {update, []ChatBlob{memBlob("blob_ok", "c_a", 5, now), memBlob("blob_x", "c_b", 5, now)}, nil},
		"missing chat":     {Chat{ID: "c_ghost", Title: "g", Blob: []byte("g")}, []ChatBlob{memBlob("blob_ok", "c_ghost", 5, now)}, ErrNotFound},
	}
	for name, tc := range refusals {
		err := m.SaveChatWithBlobs(ctx, tc.chat, tc.blobs, []string{"blob_a"})
		if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
			t.Fatalf("%s: SaveChatWithBlobs = %v, want %v", name, err, tc.wantErr)
		}
		if m.bytes != bytesBefore {
			t.Fatalf("%s: bytes = %d after a refused save, want %d", name, m.bytes, bytesBefore)
		}
		if len(m.blobs) != 2 || len(m.chatBlobIDs) != 2 || len(m.chatBlobIDs["c_a"]) != 1 || len(m.chatBlobIDs["c_b"]) != 1 {
			t.Fatalf("%s: store holds %d blobs, index %v, want only blob_a and blob_b", name, len(m.blobs), m.chatBlobIDs)
		}
		if !reflect.DeepEqual(m.order, orderBefore) {
			t.Fatalf("%s: order = %v after a refused save, want %v", name, m.order, orderBefore)
		}
		row, err := m.ChatByID(ctx, "c_a")
		if err != nil || row.Title != "c_a" || row.KeyVersion != 1 || len(row.Blob) != 3 || !row.UpdatedAt.Equal(now) {
			t.Fatalf("%s: c_a = %#v, %v after a refused save, want it unchanged", name, row, err)
		}
		if _, err := m.ChatBlobByID(ctx, "blob_ok"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: blob_ok = %v, want ErrNotFound (a refused batch stores none of its blobs)", name, err)
		}
	}
}

// TestMemorySaveChatWithBlobsConcurrentAccess is a -race tripwire for the
// primitive: saves, reads and deletes of different chats run together under a
// budget small enough that evictions interleave, with no data race, no blob that
// outlives its chat, and byte accounting that still equals what is held.
func TestMemorySaveChatWithBlobsConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(256)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				chatID := fmt.Sprintf("c_%d_%d", w, i)
				_ = m.CreateChat(ctx, memChat(chatID, 4, now))
				_ = m.SaveChatWithBlobs(ctx,
					Chat{ID: chatID, Title: chatID, KeyVersion: 1, Blob: bytes.Repeat([]byte{'n'}, 6), UpdatedAt: now},
					[]ChatBlob{memBlob("b1_"+chatID, chatID, 16, now), memBlob("b2_"+chatID, chatID, 16, now)}, nil)
				_, _ = m.ChatBlobsByIDs(ctx, chatID, []string{"b1_" + chatID, "b2_" + chatID})
				if i%3 == 0 {
					_ = m.DeleteChat(ctx, chatID)
				}
			}
		}()
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	held := 0
	for _, rec := range m.records {
		held += len(rec.Blob)
	}
	for _, b := range m.blobs {
		held += len(b.Blob)
	}
	if m.bytes != held {
		t.Fatalf("bytes = %d, but the store holds %d: accounting drifted under concurrency", m.bytes, held)
	}
	for id, b := range m.blobs {
		if _, ok := m.records[b.ChatID]; !ok {
			t.Fatalf("blob %s outlived its chat %s", id, b.ChatID)
		}
	}
}

// TestMemorySaveChatWithBlobsDeletesIDs pins the delete half of the save (#281)
// on the in-RAM store: the named blobs of the saved chat are dropped in the same
// locked pass as the chat update and the inserts, their payload bytes leave the
// budget, and the id index of the chat shrinks with them. Ids that are not the
// saved chat's -- another chat's blob, an unknown id, an id named twice -- are no
// error and change nothing but the single real row (the SQL driver's
// `chat_id = ? and id in (...)` does exactly that).
func TestMemorySaveChatWithBlobsDeletesIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(0)
	mustCreateChat(ctx, t, m, memChat("c_a", 3, now))
	mustPutBlob(ctx, t, m, memBlob("a1", "c_a", 5, now))
	mustPutBlob(ctx, t, m, memBlob("a2", "c_a", 7, now))
	mustPutBlob(ctx, t, m, memBlob("a3", "c_a", 11, now))
	mustCreateChat(ctx, t, m, memChat("c_b", 2, now))
	mustPutBlob(ctx, t, m, memBlob("b1", "c_b", 13, now))
	if m.bytes != 3+5+7+11+2+13 {
		t.Fatalf("bytes = %d before the save, want %d", m.bytes, 3+5+7+11+2+13)
	}

	// Transcript 3 -> 6, one new 10-byte blob, a1 and a2 deleted (a1 named
	// twice), plus b1 (another chat's) and a ghost id, which must be ignored.
	grown := Chat{ID: "c_a", Title: "c_a", KeyVersion: 1, Blob: bytes.Repeat([]byte{'s'}, 6), UpdatedAt: now.Add(time.Minute)}
	if err := m.SaveChatWithBlobs(ctx, grown, []ChatBlob{memBlob("n1", "c_a", 10, now)}, []string{"a1", "a2", "a1", "b1", "ghost"}); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	for _, id := range []string{"a1", "a2"} {
		if _, err := m.ChatBlobByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobByID(%s) = %v, want ErrNotFound (deleted by the save)", id, err)
		}
		if _, err := m.ChatBlobOwner(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatBlobOwner(%s) = %v, want ErrNotFound (deleted by the save)", id, err)
		}
	}
	for id, chatID := range map[string]string{"a3": "c_a", "n1": "c_a", "b1": "c_b"} {
		if got, err := m.ChatBlobByID(ctx, id); err != nil || got.ChatID != chatID {
			t.Fatalf("ChatBlobByID(%s) = %#v, %v, want it present under %s", id, got, err, chatID)
		}
	}
	// Chat 6 + a3 11 + n1 10 + c_b 2 + b1 13: the deleted a1/a2 (5 + 7) left
	// the budget exactly once, a1 named twice notwithstanding.
	if m.bytes != 6+11+10+2+13 {
		t.Fatalf("bytes = %d after the save, want %d (a1's and a2's bytes subtracted once each)", m.bytes, 6+11+10+2+13)
	}
	if !reflect.DeepEqual(m.chatBlobIDs["c_a"], []string{"a3", "n1"}) || !reflect.DeepEqual(m.chatBlobIDs["c_b"], []string{"b1"}) || len(m.blobs) != 3 {
		t.Fatalf("index = %v with %d blobs, want c_a [a3 n1], c_b [b1], 3 blobs", m.chatBlobIDs, len(m.blobs))
	}

	// Deleting every remaining blob of the chat leaves no index entry behind.
	if err := m.SaveChatWithBlobs(ctx, grown, nil, []string{"a3", "n1"}); err != nil {
		t.Fatalf("SaveChatWithBlobs(delete only): %v", err)
	}
	if m.bytes != 6+2+13 {
		t.Fatalf("bytes = %d after deleting the rest, want %d", m.bytes, 6+2+13)
	}
	if _, ok := m.chatBlobIDs["c_a"]; ok || len(m.blobs) != 1 {
		t.Fatalf("index = %v with %d blobs, want no c_a entry and only b1 held", m.chatBlobIDs, len(m.blobs))
	}
}

// TestMemorySaveChatWithBlobsDeletesBeforeTheOneEviction pins that the deleted
// blobs' bytes are already off the budget when the save's single eviction runs.
// The saved chat's two 10-byte blobs are replaced by one 15-byte blob: the
// finished state (12 + 3 + 15 = 30) fits the 40-byte budget, but 35 + 15 = 50
// would not -- so evicting before the deletes landed would throw the unrelated
// oldest chat out for bytes the very same save frees.
func TestMemorySaveChatWithBlobsDeletesBeforeTheOneEviction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m := NewMemoryChatStore(40)
	mustCreateChat(ctx, t, m, memChat("c_old", 2, now)) // oldest
	mustPutBlob(ctx, t, m, memBlob("old_1", "c_old", 10, now))
	mustCreateChat(ctx, t, m, memChat("c_save", 3, now))
	mustPutBlob(ctx, t, m, memBlob("s1", "c_save", 10, now))
	mustPutBlob(ctx, t, m, memBlob("s2", "c_save", 10, now))
	if m.bytes != 35 {
		t.Fatalf("bytes = %d before the save, want 35", m.bytes)
	}

	same := Chat{ID: "c_save", Title: "c_save", KeyVersion: 1, Blob: bytes.Repeat([]byte{'t'}, 3), UpdatedAt: now.Add(time.Minute)}
	if err := m.SaveChatWithBlobs(ctx, same, []ChatBlob{memBlob("s3", "c_save", 15, now)}, []string{"s1", "s2"}); err != nil {
		t.Fatalf("SaveChatWithBlobs: %v", err)
	}

	if _, err := m.ChatByID(ctx, "c_old"); err != nil {
		t.Fatalf("c_old = %v, want present (the deletes free what the new blob adds)", err)
	}
	if _, err := m.ChatBlobByID(ctx, "old_1"); err != nil {
		t.Fatalf("old_1 = %v, want present", err)
	}
	if m.bytes != 30 {
		t.Fatalf("bytes = %d, want 30 (c_old 2+10, c_save 3 + the 15-byte blob)", m.bytes)
	}
}
