// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Capture is the write DTO for a persisted capture of one request/response
// exchange. It is keyed 1:1 to a usage_events row via the shared event ID
// (UsageEventID == usage.Event.ID). Blob is either nonce||AEAD(gzip(envelope))
// (KeyVersion > 0, sealed) or plain gzip(envelope) (KeyVersion == 0, the
// RAM-fallback path — SP-C+ P4).
//
// OwnerUserID/APIFlavor/HTTPStatus are additive (SP-C+ P4), for cross-store
// parity: SQLiteStore.SaveCapture ignores them (SQLiteStore.Capture keeps
// resolving them via its usage_events JOIN, unchanged); MemoryCaptureStore has
// no JOIN to fall back on, so it stores and returns them verbatim. The two
// stores are therefore interchangeable from the caller's point of view — see
// TestCrossStoreCaptureParity in internal/store/capture_parity_test.go.
type Capture struct {
	UsageEventID string
	OwnerUserID  string
	APIFlavor    string
	HTTPStatus   int
	KeyVersion   int
	Blob         []byte
	CreatedAt    time.Time
	// Secret is inherited from the token/principal at write time (SP-2b). A
	// secret capture is visible only to its owner, never to admins.
	Secret bool
}

// CaptureRow is the read DTO returned by Capture: the stored blob plus fields
// resolved via a JOIN on usage_events (the captures table keeps no user_id /
// api_flavor / http_status of its own — the FK links the two). OwnerUserID is
// the access gate; APIFlavor + HTTPStatus feed the P6 CaptureDetail (they are
// NOT in the encrypted envelope, which stays 5 fields).
type CaptureRow struct {
	OwnerUserID string
	APIFlavor   string
	HTTPStatus  int
	KeyVersion  int
	Blob        []byte
	CreatedAt   time.Time
	// Secret comes from the captures row itself (not the usage_events JOIN);
	// it gates read/delete/toggle to the owner only (SP-2b/2c).
	Secret bool
}

// CapturePresence is the per-usage-event capture presence returned by
// HasCaptures: an entry exists iff a capture exists for that id. Secret and
// OwnerUserID let Service.Usage decide, per viewer, whether the row is
// openable (has_capture) or shown only as a lock (capture_locked, SP-2e).
type CapturePresence struct {
	Secret      bool
	OwnerUserID string
}

func (s *SQLiteStore) SaveCapture(ctx context.Context, capture Capture) error {
	_, err := s.exec(ctx, `
		insert into captures (usage_event_id, key_version, blob, created_at, secret)
		values (?, ?, ?, ?, ?)`,
		capture.UsageEventID, capture.KeyVersion, capture.Blob, capture.CreatedAt, capture.Secret,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("save capture: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Capture(ctx context.Context, usageEventID string) (CaptureRow, error) {
	row := s.queryRow(ctx, `
		select e.user_id, e.api_flavor, e.http_status, c.key_version, c.blob, c.created_at, c.secret
		from captures as c
		join usage_events as e on e.id = c.usage_event_id
		where c.usage_event_id = ?`, usageEventID)
	var capture CaptureRow
	var secret int64
	err := row.Scan(&capture.OwnerUserID, &capture.APIFlavor, &capture.HTTPStatus, &capture.KeyVersion, &capture.Blob, &capture.CreatedAt, &secret)
	if errors.Is(err, sql.ErrNoRows) {
		return CaptureRow{}, ErrNotFound
	}
	if err != nil {
		return CaptureRow{}, fmt.Errorf("scan capture: %w", err)
	}
	capture.Secret = secret != 0
	return capture, nil
}

// HasCaptures resolves which of the given usage-event IDs have a stored
// capture, for the portal Activity list's has_capture flag (relocated out of
// the Query SELECT so it stays correct regardless of which CaptureReader —
// SQLite here, or the in-RAM store added later — backs a given deployment).
// Empty ids does no lookup at all and returns an empty map; an id absent
// from the result is simply not captured (callers treat a missing key as
// false via Go's zero-value map lookup).
func (s *SQLiteStore) HasCaptures(ctx context.Context, ids []string) (map[string]CapturePresence, error) {
	out := make(map[string]CapturePresence, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.query(ctx,
		"select c.usage_event_id, c.secret, e.user_id from captures as c "+
			"join usage_events as e on e.id = c.usage_event_id "+
			"where c.usage_event_id in ("+strings.Join(placeholders, ",")+")",
		args...)
	if err != nil {
		return nil, fmt.Errorf("has captures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var secret int64
		var owner string
		if err := rows.Scan(&id, &secret, &owner); err != nil {
			return nil, fmt.Errorf("scan has captures: %w", err)
		}
		out[id] = CapturePresence{Secret: secret != 0, OwnerUserID: owner}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate has captures: %w", err)
	}
	return out, nil
}

// SetCaptureSecret flips the secret flag on an existing capture row. It touches
// only the captures row (never usage_events). A missing capture returns
// ErrNotFound (RowsAffected == 0), so the owner-only gate above it cannot leak
// existence of a row the caller may not touch.
func (s *SQLiteStore) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	res, err := s.exec(ctx, `update captures set secret = ? where usage_event_id = ?`, secret, usageEventID)
	if err != nil {
		return fmt.Errorf("set capture secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set capture secret rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) PruneCaptures(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.exec(ctx, `delete from captures where created_at < ?`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune captures: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune captures rows affected: %w", err)
	}
	return n, nil
}

// DeleteCapture removes the persisted blob for usageEventID. The owning
// usage_events row is untouched: captures and usage_events are separate
// tables linked only by the FK, so deleting a captures row never cascades up.
func (s *SQLiteStore) DeleteCapture(ctx context.Context, usageEventID string) error {
	res, err := s.exec(ctx, `delete from captures where usage_event_id = ?`, usageEventID)
	if err != nil {
		return fmt.Errorf("delete capture: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete capture rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
