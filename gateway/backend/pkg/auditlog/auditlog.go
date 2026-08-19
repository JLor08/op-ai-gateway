// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package auditlog provides privacy-preserving audit events for gateway actions.
// It deliberately records only a SHA-256 digest of payloads, never prompts or
// completions themselves.
package auditlog

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Event is safe to send to an audit sink or expose to an administrator.
type Event struct {
	Action        string    `json:"action"`
	PayloadSHA256 string    `json:"payload_sha256"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// Record creates a minimal, privacy-preserving audit event.
func Record(action string, payload []byte) Event {
	digest := sha256.Sum256(payload)
	return Event{
		Action:        action,
		PayloadSHA256: hex.EncodeToString(digest[:]),
		RecordedAt:    time.Now().UTC(),
	}
}
