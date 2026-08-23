// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *SQLiteStore) CreateSession(ctx context.Context, session Session) error {
	_, err := s.exec(ctx, `
		insert into sessions (id, user_id, secret_hash, created_at, expires_at, last_seen_at)
		values (?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.SecretHash, session.CreatedAt, session.ExpiresAt, session.LastSeenAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SessionBySecret(ctx context.Context, secretHash string) (Session, error) {
	row := s.queryRow(ctx, `
		select id, user_id, secret_hash, created_at, expires_at, last_seen_at, elevated_until
		from sessions where secret_hash = ?`, secretHash)
	var session Session
	var elevated sql.NullTime
	err := row.Scan(&session.ID, &session.UserID, &session.SecretHash, &session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt, &elevated)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("scan session: %w", err)
	}
	if elevated.Valid {
		session.ElevatedUntil = elevated.Time.UTC()
	}
	return session, nil
}

func (s *SQLiteStore) TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error {
	if _, err := s.exec(ctx, `update sessions set last_seen_at = ? where id = ?`, lastSeenAt, id); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SetSessionElevation(ctx context.Context, id string, until time.Time) error {
	var val any
	if !until.IsZero() {
		val = until.UTC()
	}
	if _, err := s.exec(ctx, `update sessions set elevated_until = ? where id = ?`, val, id); err != nil {
		return fmt.Errorf("set session elevation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.exec(ctx, `delete from sessions where id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteSessionsByUser(ctx context.Context, userID string) error {
	if _, err := s.exec(ctx, `delete from sessions where user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions by user: %w", err)
	}
	return nil
}
