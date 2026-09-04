// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"fmt"
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

// SetSystemSettings upserts several settings in ONE transaction: either every
// pair is applied or none is. This is what lets UpdateSystemSettings flip a group
// of related keys (e.g. cert_enabled together with cert_issuer_mode) without ever
// exposing a partial state to a concurrent reader such as the certificate
// reconcile loop, and it rolls the whole batch back on any error instead of
// leaving earlier keys written. An empty batch is a no-op (no transaction).
func (s *SQLiteStore) SetSystemSettings(ctx context.Context, values map[string]string, now time.Time) error {
	if len(values) == 0 {
		return nil
	}
	ts := now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set system settings tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(
			`insert into system_settings (key, value, updated_at) values (?, ?, ?)
			 on conflict(key) do update set value = excluded.value, updated_at = excluded.updated_at`),
			key, value, ts); err != nil {
			return fmt.Errorf("set system setting %q: %w", key, err)
		}
	}
	return tx.Commit()
}
