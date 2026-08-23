// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"sync"
)

// defaultCaptureMemoryMaxBytes is the byte budget MemoryCaptureStore falls
// back to when constructed with maxBytes <= 0 (mirrors config default
// OP_AI_GATEWAY_CAPTURE_MEMORY_MAX_BYTES = 64 MiB).
const defaultCaptureMemoryMaxBytes = 67108864

// MemoryCaptureStore is the volatile, in-process fallback for payload capture
// (SP-C+ P4): it satisfies the same interfaces as SQLiteStore's capture
// surface (gateway.CaptureStore for writes, portal.CaptureReader for reads)
// but never touches disk. Blobs live only in process RAM and are gone on
// process exit — used when no encryption key is configured (sqlite driver) or
// when the memory driver is active.
//
// It keeps entries in insertion order and evicts the OLDEST entries first
// once the sum of blob bytes exceeds maxBytes (a byte-FIFO, not a
// count-FIFO). A single blob larger than the whole budget is kept as the SOLE
// entry rather than evicted away to empty. The budget counts blob bytes only,
// NOT process RSS (gzip overhead, map/slice bookkeeping are not counted).
type MemoryCaptureStore struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	order    []string // insertion order, oldest first
	records  map[string]Capture
}

// NewMemoryCaptureStore constructs a MemoryCaptureStore with the given byte
// budget. maxBytes <= 0 falls back to defaultCaptureMemoryMaxBytes (64 MiB).
func NewMemoryCaptureStore(maxBytes int) *MemoryCaptureStore {
	if maxBytes <= 0 {
		maxBytes = defaultCaptureMemoryMaxBytes
	}
	return &MemoryCaptureStore{
		maxBytes: maxBytes,
		records:  make(map[string]Capture),
	}
}

// SaveCapture stores capture, evicting the oldest entries (by insertion
// order) until the sum of blob bytes is back within maxBytes. Returns
// ErrConflict on a duplicate UsageEventID (mirrors the SQLite unique
// constraint) — a rejected duplicate never touches s.bytes/s.order, so it
// cannot double-count.
func (m *MemoryCaptureStore) SaveCapture(ctx context.Context, capture Capture) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[capture.UsageEventID]; exists {
		return ErrConflict
	}
	m.records[capture.UsageEventID] = capture
	m.order = append(m.order, capture.UsageEventID)
	m.bytes += len(capture.Blob)
	// Evict the oldest entries first. len(m.order) > 1 guarantees the entry
	// just inserted is never evicted away to empty — a single oversized blob
	// is kept as the sole entry instead.
	for m.bytes > m.maxBytes && len(m.order) > 1 {
		oldest := m.order[0]
		m.order = m.order[1:]
		m.bytes -= len(m.records[oldest].Blob)
		delete(m.records, oldest)
	}
	return nil
}

// Capture returns the stored record for usageEventID, or ErrNotFound.
func (m *MemoryCaptureStore) Capture(ctx context.Context, usageEventID string) (CaptureRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[usageEventID]
	if !ok {
		return CaptureRow{}, ErrNotFound
	}
	return CaptureRow{
		OwnerUserID: rec.OwnerUserID,
		APIFlavor:   rec.APIFlavor,
		HTTPStatus:  rec.HTTPStatus,
		KeyVersion:  rec.KeyVersion,
		Blob:        rec.Blob,
		CreatedAt:   rec.CreatedAt,
		Secret:      rec.Secret,
	}, nil
}

// HasCaptures reports, for each of ids, the CapturePresence (secret + owner)
// of any capture currently held in memory. An empty ids returns an empty map
// without taking the lock (mirrors the SQLite path's "no query on an empty
// IN()" short-circuit). Unknown ids are simply absent from the result, so a
// caller reading got[id] for an unknown id gets the zero CapturePresence —
// matching the documented "unknown -> absent" contract.
func (m *MemoryCaptureStore) HasCaptures(ctx context.Context, ids []string) (map[string]CapturePresence, error) {
	out := make(map[string]CapturePresence, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if rec, ok := m.records[id]; ok {
			out[id] = CapturePresence{Secret: rec.Secret, OwnerUserID: rec.OwnerUserID}
		}
	}
	return out, nil
}

// SetCaptureSecret flips the secret flag on an in-memory capture record.
// Returns ErrNotFound if it is not (or no longer) held, mirroring the SQLite
// RowsAffected == 0 contract.
func (m *MemoryCaptureStore) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[usageEventID]
	if !ok {
		return ErrNotFound
	}
	rec.Secret = secret
	m.records[usageEventID] = rec
	return nil
}

// DeleteCapture removes usageEventID's blob and subtracts its bytes from the
// budget. Returns ErrNotFound if it is not (or no longer, e.g. already
// evicted) held.
func (m *MemoryCaptureStore) DeleteCapture(ctx context.Context, usageEventID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[usageEventID]
	if !ok {
		return ErrNotFound
	}
	delete(m.records, usageEventID)
	m.bytes -= len(rec.Blob)
	for i, id := range m.order {
		if id == usageEventID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}
