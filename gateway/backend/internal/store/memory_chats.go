// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// defaultChatMemoryMaxBytes is the byte budget MemoryChatStore falls back to
// when constructed with maxBytes <= 0. It mirrors the capture RAM budget
// (64 MiB) — chats reuse the same volatile-RAM fallback shape as captures.
const defaultChatMemoryMaxBytes = 67108864

// MemoryChatStore is the volatile, in-process fallback for encrypted chats: it
// satisfies the same method set as SQLiteStore's chat surface
// (portal.ChatStore) but never touches disk. Blobs live only in process RAM and
// are gone on process exit — used when no encryption key is configured (sqlite
// driver, KeyVersion 0 plain gzip) or when the memory driver is active.
//
// It keeps entries in insertion order and evicts the OLDEST entries first once
// the sum of blob bytes exceeds maxBytes (a byte-FIFO, not a count-FIFO). A
// single blob larger than the whole budget is kept as the SOLE entry rather
// than evicted away to empty. The budget counts blob bytes only, NOT process
// RSS. Chats are owner-scoped: ChatsByUser filters by userID.
//
// The chat_blobs surface (PutChatBlob, ChatBlobByID, ChatBlobOwner,
// ChatBlobsByIDs) lives in the same store and under the same budget: an entry is
// a chat TOGETHER with its chat_blobs, the payload bytes of both count toward
// maxBytes, and DeleteChat or eviction of a chat drops its blobs with it (the
// in-RAM equivalent of the SQL chat_blobs.chat_id ON DELETE CASCADE).
type MemoryChatStore struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	order    []string // insertion order, oldest first
	records  map[string]Chat

	blobs       map[string]ChatBlob // chat_blobs by blob id
	chatBlobIDs map[string][]string // chat id -> ids of its blobs, in put order
}

// NewMemoryChatStore constructs a MemoryChatStore with the given byte budget.
// maxBytes <= 0 falls back to defaultChatMemoryMaxBytes (64 MiB).
func NewMemoryChatStore(maxBytes int) *MemoryChatStore {
	if maxBytes <= 0 {
		maxBytes = defaultChatMemoryMaxBytes
	}
	return &MemoryChatStore{
		maxBytes:    maxBytes,
		records:     make(map[string]Chat),
		blobs:       make(map[string]ChatBlob),
		chatBlobIDs: make(map[string][]string),
	}
}

// CreateChat stores chat, evicting the oldest entries (by insertion order)
// until the sum of blob bytes is back within maxBytes. Returns ErrConflict on a
// duplicate ID (mirrors the SQLite primary-key constraint) — a rejected
// duplicate never touches s.bytes/s.order, so it cannot double-count.
func (m *MemoryChatStore) CreateChat(ctx context.Context, chat Chat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[chat.ID]; exists {
		return ErrConflict
	}
	m.records[chat.ID] = chat
	m.order = append(m.order, chat.ID)
	m.bytes += len(chat.Blob)
	m.evict()
	return nil
}

// UpdateChat overwrites the mutable fields (title/blob/key_version/updated_at)
// of an existing chat, adjusting the byte budget for the new blob size and
// re-running eviction. A missing entry returns ErrNotFound.
func (m *MemoryChatStore) UpdateChat(ctx context.Context, chat Chat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[chat.ID]
	if !ok {
		return ErrNotFound
	}
	m.overwriteChat(rec, chat)
	m.evict()
	return nil
}

// overwriteChat applies chat's mutable fields (title/blob/key_version/
// updated_at) to the held record rec and re-counts its blob bytes. It does NOT
// evict: UpdateChat evicts right after, SaveChatWithBlobs only once the whole
// save is applied. Callers must hold m.mu and pass rec = m.records[chat.ID].
func (m *MemoryChatStore) overwriteChat(rec, chat Chat) {
	m.bytes -= len(rec.Blob)
	rec.Title = chat.Title
	rec.KeyVersion = chat.KeyVersion
	rec.Blob = chat.Blob
	rec.UpdatedAt = chat.UpdatedAt
	m.records[chat.ID] = rec
	m.bytes += len(rec.Blob)
}

// SaveChatWithBlobs writes a chat and its newly-extracted chat_blobs as one
// all-or-nothing unit, mirroring SQLStore.SaveChatWithBlobs: the chat side is
// UpdateChat, each blob is a PutChatBlob, and the errors are theirs (a chat that
// is not held is ErrNotFound, a blob id already taken -- by a held blob or by an
// earlier blob of the same batch -- is ErrConflict). A blob whose ChatID is not
// chat.ID is refused too, exactly as the SQL driver does, so the two never
// disagree about what a save may attach.
//
// It is atomic by staging: m.mu is taken ONCE and every refusal is decided in a
// first pass that mutates nothing, so a refused save leaves the chat, the blobs,
// the indexes and the byte budget exactly as they were. Only a save that cannot
// fail any more is applied -- chat fields, blobs and their byte count -- and
// eviction then runs ONCE, after the whole save. It must not loop PutChatBlob:
// that takes the lock per blob and evicts after each one, so with the chat being
// saved as the oldest entry a mid-save eviction would drop that very chat (and
// the blobs already stored for it) and fail the rest of the batch with
// ErrNotFound. Applied as one step, the budget is judged against the finished
// state. The final eviction can still drop the saved chat if it is the oldest
// entry and the finished state is over budget -- as UpdateChat can evict the
// chat it just grew -- but then it goes together with all of its blobs, never
// half-saved. Like Chat.Blob, the blob byte slices are held, not copied.
func (m *MemoryChatStore) SaveChatWithBlobs(ctx context.Context, chat Chat, blobs []ChatBlob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[chat.ID]
	if !ok {
		return ErrNotFound
	}
	// Stage: decide every refusal before touching any state.
	batch := make(map[string]struct{}, len(blobs))
	for _, blob := range blobs {
		if blob.ChatID != chat.ID {
			return fmt.Errorf("save chat with blobs: blob %q belongs to chat %q, not %q", blob.ID, blob.ChatID, chat.ID)
		}
		if _, taken := m.blobs[blob.ID]; taken {
			return ErrConflict
		}
		if _, repeated := batch[blob.ID]; repeated {
			return ErrConflict
		}
		batch[blob.ID] = struct{}{}
	}
	// Apply: nothing below can fail.
	m.overwriteChat(rec, chat)
	for _, blob := range blobs {
		m.storeChatBlob(blob)
	}
	m.evict()
	return nil
}

// ChatByID returns the full stored record for id, or ErrNotFound.
func (m *MemoryChatStore) ChatByID(ctx context.Context, id string) (ChatRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return ChatRow{}, ErrNotFound
	}
	return ChatRow(rec), nil
}

// ChatsByUser returns userID's chats as summaries (no blob), most-recently
// updated first (ties broken by ID descending for determinism).
func (m *MemoryChatStore) ChatsByUser(ctx context.Context, userID string) ([]ChatSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatSummary, 0)
	for _, rec := range m.records {
		if rec.UserID != userID {
			continue
		}
		out = append(out, ChatSummary{
			ID:        rec.ID,
			Title:     rec.Title,
			CreatedAt: rec.CreatedAt,
			UpdatedAt: rec.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// DeleteChat removes id's blob and its chat_blobs and subtracts all their bytes
// from the budget. Returns ErrNotFound if it is not (or no longer, e.g. already
// evicted) held.
func (m *MemoryChatStore) DeleteChat(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.records, id)
	m.bytes -= len(rec.Blob)
	m.dropChatBlobs(id)
	for i, oid := range m.order {
		if oid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// evict drops the oldest entries (by insertion order) until the byte budget is
// satisfied. An entry is a chat together with its chat_blobs: evicting it
// subtracts the transcript AND every blob's bytes. len(m.order) > 1 guarantees
// the most-recently touched entry is never evicted away to empty — a single
// oversized blob is kept as the sole entry. Callers must hold m.mu.
func (m *MemoryChatStore) evict() {
	for m.bytes > m.maxBytes && len(m.order) > 1 {
		oldest := m.order[0]
		m.order = m.order[1:]
		m.bytes -= len(m.records[oldest].Blob)
		delete(m.records, oldest)
		m.dropChatBlobs(oldest)
	}
}

// dropChatBlobs removes every chat_blobs entry of chatID and subtracts their
// payload bytes from the budget. It is the cascade DeleteChat and evict share,
// so a chat's blobs can never outlive it as uncounted RAM. A chat without blobs
// is a no-op. Callers must hold m.mu.
func (m *MemoryChatStore) dropChatBlobs(chatID string) {
	for _, id := range m.chatBlobIDs[chatID] {
		m.bytes -= len(m.blobs[id].Blob)
		delete(m.blobs, id)
	}
	delete(m.chatBlobIDs, chatID)
}

// PutChatBlob stores one chat_blobs entry, mirroring SQLStore.PutChatBlob: it
// is a dumb byte store (the service layer seals, allow-lists the content type
// and caps the size before calling), a blob whose chat is not held is
// ErrNotFound (the in-RAM foreign-key violation) and an id already taken is
// ErrConflict. A refused put never touches the budget. Like Chat.Blob, the byte
// slice is held, not copied: the caller must not mutate it after the call.
//
// len(blob.Blob) is added to the byte budget (ByteLen is metadata and is not
// consulted) and eviction re-runs, so enough blobs push the oldest chats out.
// The chat the blob belongs to is not exempt: if it is the oldest entry and the
// store is over budget it is evicted together with the blob it just received,
// exactly as UpdateChat can evict the chat it just grew.
func (m *MemoryChatStore) PutChatBlob(ctx context.Context, blob ChatBlob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[blob.ChatID]; !ok {
		return ErrNotFound
	}
	if _, taken := m.blobs[blob.ID]; taken {
		return ErrConflict
	}
	m.storeChatBlob(blob)
	m.evict()
	return nil
}

// storeChatBlob records blob in the blob map and its chat's index and adds its
// payload bytes to the budget. It does NOT evict and does not re-check the chat
// or the id: PutChatBlob and SaveChatWithBlobs have already decided the put is
// valid, and each evicts when it chooses. Callers must hold m.mu.
func (m *MemoryChatStore) storeChatBlob(blob ChatBlob) {
	m.blobs[blob.ID] = blob
	m.chatBlobIDs[blob.ChatID] = append(m.chatBlobIDs[blob.ChatID], blob.ID)
	m.bytes += len(blob.Blob)
}

// ChatBlobByID returns the full entry (payload included) for a blob id, or
// ErrNotFound. Like the SQL driver it does NOT check which chat the caller may
// read from: a caller that serves a blob to a user resolves ownership first
// (ChatBlobOwner, or ChatBlobsByIDs which scopes by chat itself).
func (m *MemoryChatStore) ChatBlobByID(ctx context.Context, id string) (ChatBlob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	blob, ok := m.blobs[id]
	if !ok {
		return ChatBlob{}, ErrNotFound
	}
	return blob, nil
}

// ChatBlobOwner returns the id of the chat a blob belongs to, or ErrNotFound.
func (m *MemoryChatStore) ChatBlobOwner(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	blob, ok := m.blobs[id]
	if !ok {
		return "", ErrNotFound
	}
	return blob.ChatID, nil
}

// ChatBlobsByIDs returns the named blobs of ONE chat keyed by blob id, with the
// SQL driver's ownership-by-construction: an id that exists but belongs to
// another chat is simply absent from the result, exactly like an id that does
// not exist at all. An empty (or nil) ids returns an empty, non-nil map.
func (m *MemoryChatStore) ChatBlobsByIDs(ctx context.Context, chatID string, ids []string) (map[string]ChatBlob, error) {
	out := make(map[string]ChatBlob, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if blob, ok := m.blobs[id]; ok && blob.ChatID == chatID {
			out[id] = blob
		}
	}
	return out, nil
}
