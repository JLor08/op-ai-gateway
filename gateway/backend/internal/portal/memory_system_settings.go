// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"sync"
	"time"
)

var _ SystemSettingsStore = (*MemorySystemSettings)(nil)

// MemorySystemSettings is the in-memory SystemSettingsStore for memory mode/tests.
type MemorySystemSettings struct {
	mu     sync.RWMutex
	values map[string]string
}

func NewMemorySystemSettings() *MemorySystemSettings {
	return &MemorySystemSettings{values: make(map[string]string)}
}

func (m *MemorySystemSettings) SystemSettings(_ context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.values))
	for k, v := range m.values {
		out[k] = v
	}
	return out, nil
}

func (m *MemorySystemSettings) SetSystemSetting(_ context.Context, key, value string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
	return nil
}
