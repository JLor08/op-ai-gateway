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

// PutChatBlob inserts one chat_blobs row. It is a dumb byte store: the bytes,
// key version, content type and byte length are written exactly as given (the
// service layer seals, allow-lists the content type and enforces the size cap
// before calling). The error mapping is CreateChat's: a blob whose chat does
// not exist is ErrNotFound (the foreign-key violation), a blob id already
// taken is ErrConflict.
func (s *SQLStore) PutChatBlob(ctx context.Context, blob ChatBlob) error {
	return s.insertChatBlob(ctx, s.db, blob)
}

// insertChatBlob is PutChatBlob's insert and error mapping against an explicit
// execer, so SaveChatWithBlobs can run the very same statement inside its
// transaction instead of through the pool (which would commit each blob on its
// own and defeat the all-or-nothing save).
func (s *SQLStore) insertChatBlob(ctx context.Context, ex execer, blob ChatBlob) error {
	_, err := s.execOn(ctx, ex, `
		insert into chat_blobs (id, chat_id, key_version, blob, content_type, byte_len, created_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		blob.ID, blob.ChatID, blob.KeyVersion, blob.Blob, blob.ContentType, blob.ByteLen, blob.CreatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("put chat blob: %w", err)
	}
	return nil
}

// ChatBlobByID returns the full row (payload included) for a blob id, or
// ErrNotFound. It does NOT check which chat the caller may read from: a caller
// that serves a blob to a user resolves ownership first (ChatBlobOwner, or
// ChatBlobsByIDs which scopes by chat in the query itself).
func (s *SQLStore) ChatBlobByID(ctx context.Context, id string) (ChatBlob, error) {
	row := s.queryRow(ctx, `
		select id, chat_id, key_version, blob, content_type, byte_len, created_at
		from chat_blobs where id = ?`, id)
	var blob ChatBlob
	err := row.Scan(&blob.ID, &blob.ChatID, &blob.KeyVersion, &blob.Blob, &blob.ContentType, &blob.ByteLen, &blob.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatBlob{}, ErrNotFound
	}
	if err != nil {
		return ChatBlob{}, fmt.Errorf("scan chat blob: %w", err)
	}
	return blob, nil
}

// ChatBlobOwner returns the id of the chat a blob belongs to, or ErrNotFound.
// It reads metadata only (chat_id), never the payload, so an ownership check
// does not pull up to a whole blob off the disk just to compare two strings.
func (s *SQLStore) ChatBlobOwner(ctx context.Context, id string) (string, error) {
	var chatID string
	err := s.queryRow(ctx, `select chat_id from chat_blobs where id = ?`, id).Scan(&chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("scan chat blob owner: %w", err)
	}
	return chatID, nil
}

// ChatBlobsByIDs reads the named blobs of ONE chat in a single query, keyed by
// blob id. Ownership is by construction: the WHERE clause is
// `chat_id = ? and id in (...)`, so an id that exists but belongs to another
// chat is simply not in the result, exactly like an id that does not exist at
// all -- the caller can tell neither apart and cannot read across chats by
// naming an id. An empty (or nil) ids returns an empty, non-nil map without
// querying.
//
// The id list is bound as one placeholder per id, so it is bounded by the
// driver's parameter limit (sqlite's SQLITE_MAX_VARIABLE_NUMBER, postgres'
// 65535 per statement); the one caller passes the image ids of a single chat
// document, far below either.
func (s *SQLStore) ChatBlobsByIDs(ctx context.Context, chatID string, ids []string) (map[string]ChatBlob, error) {
	out := make(map[string]ChatBlob, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, chatID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		"select id, chat_id, key_version, blob, content_type, byte_len, created_at "+
			"from chat_blobs where chat_id = ? and id in ("+strings.Join(placeholders, ",")+")",
		args...)
	if err != nil {
		return nil, fmt.Errorf("chat blobs by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blob ChatBlob
		if err := rows.Scan(&blob.ID, &blob.ChatID, &blob.KeyVersion, &blob.Blob, &blob.ContentType, &blob.ByteLen, &blob.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan chat blob: %w", err)
		}
		out[blob.ID] = blob
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat blobs by ids: %w", err)
	}
	return out, nil
}
