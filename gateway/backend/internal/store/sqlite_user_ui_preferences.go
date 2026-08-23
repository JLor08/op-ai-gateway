// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"time"
)

// UIPreferences returns the given user's UI preferences ordered by key. A user
// with no stored preferences yields an empty (non-nil) slice.
func (s *SQLiteStore) UIPreferences(ctx context.Context, userID string) ([]UserUIPreference, error) {
	rows, err := s.query(ctx,
		`select key, value_json, updated_at from user_ui_preferences where user_id = ? order by key`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserUIPreference, 0)
	for rows.Next() {
		pref := UserUIPreference{UserID: userID}
		if err := rows.Scan(&pref.Key, &pref.ValueJSON, &pref.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, pref)
	}
	return out, rows.Err()
}

// SetUIPreference upserts a single opaque JSON value under (userID, key).
func (s *SQLiteStore) SetUIPreference(ctx context.Context, userID, key, valueJSON string) error {
	_, err := s.exec(ctx,
		`insert into user_ui_preferences (user_id, key, value_json, updated_at) values (?, ?, ?, ?)
		 on conflict(user_id, key) do update set value_json = excluded.value_json, updated_at = excluded.updated_at`,
		userID, key, valueJSON, time.Now().UTC())
	return err
}
