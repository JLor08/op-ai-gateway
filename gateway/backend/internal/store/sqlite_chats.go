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

// Chat is the write DTO for a persisted chat-playground conversation (Feature:
// encrypted chats). Title + timestamps are plaintext metadata; Blob is one
// opaque, backend-agnostic payload that is either nonce||AEAD(gzip(content))
// (KeyVersion > 0, sealed) or plain gzip(content) (KeyVersion == 0, the
// RAM-fallback path). It reuses the SAME capture cipher — sealing/opening is
// done at the portal service layer, this store is a dumb byte store.
type Chat struct {
	ID         string
	UserID     string
	Title      string
	KeyVersion int
	Blob       []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ChatRow is the read DTO returned by ChatByID: the full row including Blob +
// KeyVersion so the service can Open the sealed payload.
type ChatRow struct {
	ID         string
	UserID     string
	Title      string
	KeyVersion int
	Blob       []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ChatSummary is the list DTO returned by ChatsByUser: plaintext metadata only,
// NO blob (the list view never decrypts).
type ChatSummary struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *SQLiteStore) CreateChat(ctx context.Context, chat Chat) error {
	_, err := s.exec(ctx, `
		insert into chats (id, user_id, title, key_version, blob, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		chat.ID, chat.UserID, chat.Title, chat.KeyVersion, chat.Blob, chat.CreatedAt, chat.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create chat: %w", err)
	}
	return nil
}

// UpdateChat overwrites the mutable columns (title/blob/key_version/updated_at)
// of an existing chat. A missing row returns ErrNotFound (RowsAffected == 0).
func (s *SQLiteStore) UpdateChat(ctx context.Context, chat Chat) error {
	res, err := s.exec(ctx, `
		update chats set title = ?, key_version = ?, blob = ?, updated_at = ? where id = ?`,
		chat.Title, chat.KeyVersion, chat.Blob, chat.UpdatedAt, chat.ID,
	)
	if err != nil {
		return fmt.Errorf("update chat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update chat rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ChatByID(ctx context.Context, id string) (ChatRow, error) {
	row := s.queryRow(ctx, `
		select id, user_id, title, key_version, blob, created_at, updated_at
		from chats where id = ?`, id)
	var chat ChatRow
	err := row.Scan(&chat.ID, &chat.UserID, &chat.Title, &chat.KeyVersion, &chat.Blob, &chat.CreatedAt, &chat.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatRow{}, ErrNotFound
	}
	if err != nil {
		return ChatRow{}, fmt.Errorf("scan chat: %w", err)
	}
	return chat, nil
}

// ChatsByUser lists the owner's chats as summaries (no blob), most-recently
// updated first.
func (s *SQLiteStore) ChatsByUser(ctx context.Context, userID string) ([]ChatSummary, error) {
	rows, err := s.query(ctx, `
		select id, title, created_at, updated_at
		from chats where user_id = ? order by updated_at desc`, userID)
	if err != nil {
		return nil, fmt.Errorf("chats by user: %w", err)
	}
	defer rows.Close()
	out := make([]ChatSummary, 0)
	for rows.Next() {
		var c ChatSummary
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan chat summary: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chats by user: %w", err)
	}
	return out, nil
}

// DeleteChat removes the chat by id. A missing row returns ErrNotFound.
func (s *SQLiteStore) DeleteChat(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from chats where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete chat rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
