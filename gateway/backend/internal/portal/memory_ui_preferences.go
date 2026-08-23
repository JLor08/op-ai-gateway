// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
	"sort"
	"sync"
	"time"
)

var _ UIPreferencesStore = (*MemoryUIPreferences)(nil)

// MemoryUIPreferences is the in-memory UIPreferencesStore for memory mode/tests.
// It mirrors MemorySystemSettings but is scoped per user: userID -> key -> value.
type MemoryUIPreferences struct {
	mu     sync.RWMutex
	values map[string]map[string]store.UserUIPreference
}

func NewMemoryUIPreferences() *MemoryUIPreferences {
	return &MemoryUIPreferences{values: make(map[string]map[string]store.UserUIPreference)}
}

func (m *MemoryUIPreferences) UIPreferences(_ context.Context, userID string) ([]store.UserUIPreference, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byKey := m.values[userID]
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]store.UserUIPreference, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out, nil
}

func (m *MemoryUIPreferences) SetUIPreference(_ context.Context, userID, key, valueJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values[userID] == nil {
		m.values[userID] = make(map[string]store.UserUIPreference)
	}
	m.values[userID][key] = store.UserUIPreference{
		UserID:    userID,
		Key:       key,
		ValueJSON: valueJSON,
		UpdatedAt: time.Now().UTC(),
	}
	return nil
}
