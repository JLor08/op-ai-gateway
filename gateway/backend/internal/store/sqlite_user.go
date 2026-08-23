// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *SQLiteStore) CreateUser(ctx context.Context, user User) error {
	user.Email = normalizeEmail(user.Email)
	_, err := s.exec(ctx, `
		insert into users (
			id, email, display_name, role, status, preferred_language,
			chat_log_communication, chat_secret,
			totp_secret, totp_pending_secret, totp_enabled, totp_confirmed_at,
			password_hash, password_set_at, created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID,
		user.Email,
		user.DisplayName,
		user.Role,
		user.Status,
		user.PreferredLanguage,
		user.ChatLogCommunication,
		user.ChatSecret,
		user.TOTPSecret,
		user.TOTPPendingSecret,
		user.TOTPEnabled,
		user.TOTPConfirmedAt,
		user.PasswordHash,
		user.PasswordSetAt,
		user.CreatedAt,
		user.UpdatedAt,
	)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UserByID(ctx context.Context, id string) (User, error) {
	row := s.queryRow(ctx, `
		select id, email, display_name, role, status, preferred_language,
			chat_log_communication, chat_secret,
			totp_secret, totp_pending_secret, totp_enabled, totp_confirmed_at,
			password_hash, password_set_at, created_at, updated_at
		from users
		where id = ?`, id)
	return scanUser(row)
}

func (s *SQLiteStore) UserByEmail(ctx context.Context, email string) (User, error) {
	row := s.queryRow(ctx, `
		select id, email, display_name, role, status, preferred_language,
			chat_log_communication, chat_secret,
			totp_secret, totp_pending_secret, totp_enabled, totp_confirmed_at,
			password_hash, password_set_at, created_at, updated_at
		from users
		where email = ?`, normalizeEmail(email))
	return scanUser(row)
}

func (s *SQLiteStore) UpdateUser(ctx context.Context, user User) error {
	user.Email = normalizeEmail(user.Email)
	result, err := s.exec(ctx, `
		update users set
			email = ?, display_name = ?, role = ?, status = ?,
			preferred_language = ?, chat_log_communication = ?, chat_secret = ?,
			totp_secret = ?, totp_pending_secret = ?, totp_enabled = ?, totp_confirmed_at = ?,
			password_hash = ?, password_set_at = ?, updated_at = ?
		where id = ?`,
		user.Email,
		user.DisplayName,
		user.Role,
		user.Status,
		user.PreferredLanguage,
		user.ChatLogCommunication,
		user.ChatSecret,
		user.TOTPSecret,
		user.TOTPPendingSecret,
		user.TOTPEnabled,
		user.TOTPConfirmedAt,
		user.PasswordHash,
		user.PasswordSetAt,
		user.UpdatedAt,
		user.ID,
	)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update user: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.query(ctx, `
		select id, email, display_name, role, status, preferred_language,
			chat_log_communication, chat_secret,
			totp_secret, totp_pending_secret, totp_enabled, totp_confirmed_at,
			password_hash, password_set_at, created_at, updated_at
		from users
		order by created_at asc, id asc`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var user User
	var passwordSetAt sql.NullTime
	var chatLog, chatSecret int64
	var totpEnabled int64
	var totpConfirmedAt sql.NullTime
	err := row.Scan(
		&user.ID,
		&user.Email,
		&user.DisplayName,
		&user.Role,
		&user.Status,
		&user.PreferredLanguage,
		&chatLog,
		&chatSecret,
		&user.TOTPSecret,
		&user.TOTPPendingSecret,
		&totpEnabled,
		&totpConfirmedAt,
		&user.PasswordHash,
		&passwordSetAt,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	if passwordSetAt.Valid {
		user.PasswordSetAt = &passwordSetAt.Time
	}
	user.ChatLogCommunication = chatLog != 0
	user.ChatSecret = chatSecret != 0
	user.TOTPEnabled = totpEnabled != 0
	if totpConfirmedAt.Valid {
		user.TOTPConfirmedAt = &totpConfirmedAt.Time
	}
	return user, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
