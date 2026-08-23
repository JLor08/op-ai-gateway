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

func (s *SQLiteStore) CreateSetPasswordToken(ctx context.Context, tok SetPasswordToken) error {
	_, err := s.exec(ctx, `
		insert into set_password_tokens (id, user_id, secret_hash, expires_at, used_at, created_at)
		values (?, ?, ?, ?, ?, ?)`,
		tok.ID, tok.UserID, tok.SecretHash, tok.ExpiresAt, tok.UsedAt, tok.CreatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create set-password token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SetPasswordTokenBySecret(ctx context.Context, secretHash string) (SetPasswordToken, error) {
	row := s.queryRow(ctx, `
		select id, user_id, secret_hash, expires_at, used_at, created_at
		from set_password_tokens where secret_hash = ?`, secretHash)
	var tok SetPasswordToken
	var usedAt sql.NullTime
	err := row.Scan(&tok.ID, &tok.UserID, &tok.SecretHash, &tok.ExpiresAt, &usedAt, &tok.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SetPasswordToken{}, ErrNotFound
	}
	if err != nil {
		return SetPasswordToken{}, fmt.Errorf("scan set-password token: %w", err)
	}
	if usedAt.Valid {
		tok.UsedAt = &usedAt.Time
	}
	return tok, nil
}

func (s *SQLiteStore) MarkSetPasswordTokenUsed(ctx context.Context, id string, usedAt time.Time) error {
	if _, err := s.exec(ctx, `update set_password_tokens set used_at = ? where id = ?`, usedAt, id); err != nil {
		return fmt.Errorf("mark set-password token used: %w", err)
	}
	return nil
}

func (s *SQLiteStore) InvalidateUserSetPasswordTokens(ctx context.Context, userID string) error {
	if _, err := s.exec(ctx, `update set_password_tokens set used_at = created_at where user_id = ? and used_at is null`, userID); err != nil {
		return fmt.Errorf("invalidate set-password tokens: %w", err)
	}
	return nil
}
