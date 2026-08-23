// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
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
type MemoryChatStore struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	order    []string // insertion order, oldest first
	records  map[string]Chat
}

// NewMemoryChatStore constructs a MemoryChatStore with the given byte budget.
// maxBytes <= 0 falls back to defaultChatMemoryMaxBytes (64 MiB).
func NewMemoryChatStore(maxBytes int) *MemoryChatStore {
	if maxBytes <= 0 {
		maxBytes = defaultChatMemoryMaxBytes
	}
	return &MemoryChatStore{
		maxBytes: maxBytes,
		records:  make(map[string]Chat),
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
	m.bytes -= len(rec.Blob)
	rec.Title = chat.Title
	rec.KeyVersion = chat.KeyVersion
	rec.Blob = chat.Blob
	rec.UpdatedAt = chat.UpdatedAt
	m.records[chat.ID] = rec
	m.bytes += len(rec.Blob)
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

// DeleteChat removes id's blob and subtracts its bytes from the budget. Returns
// ErrNotFound if it is not (or no longer, e.g. already evicted) held.
func (m *MemoryChatStore) DeleteChat(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.records, id)
	m.bytes -= len(rec.Blob)
	for i, oid := range m.order {
		if oid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// evict drops the oldest entries (by insertion order) until the byte budget is
// satisfied. len(m.order) > 1 guarantees the most-recently touched entry is
// never evicted away to empty — a single oversized blob is kept as the sole
// entry. Callers must hold m.mu.
func (m *MemoryChatStore) evict() {
	for m.bytes > m.maxBytes && len(m.order) > 1 {
		oldest := m.order[0]
		m.order = m.order[1:]
		m.bytes -= len(m.records[oldest].Blob)
		delete(m.records, oldest)
	}
}
