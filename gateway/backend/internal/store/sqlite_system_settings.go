// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"time"
)

func (s *SQLiteStore) SystemSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.query(ctx, `select key, value from system_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	_, err := s.exec(ctx,
		`insert into system_settings (key, value, updated_at) values (?, ?, ?)
		 on conflict(key) do update set value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now.UTC())
	return err
}
