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

// ChatBlob is one binary payload (an inline image today) that belongs to a chat
// and lives in its own chat_blobs row instead of inside the chat document, so
// the document stays small. Blob is opaque to the store: like Chat.Blob it is
// either nonce||AEAD(bytes) (KeyVersion > 0, sealed) or the plain bytes
// (KeyVersion == 0, the RAM-fallback path) -- sealing/opening, the content-type
// allow-list and the per-blob size cap are the service layer's job, this store
// is a dumb byte store. ContentType and ByteLen are plaintext metadata about
// the payload as it was before sealing, so the list/serve path never has to
// decrypt just to answer "what is this and how big".
type ChatBlob struct {
	ID          string
	ChatID      string
	KeyVersion  int
	Blob        []byte
	ContentType string
	ByteLen     int64
	CreatedAt   time.Time
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
	return s.updateChatRow(ctx, s.db, chat)
}

// updateChatRow is UpdateChat's statement and error mapping against an explicit
// execer, shared with SaveChatWithBlobs so the in-transaction update cannot
// drift from the standalone one.
func (s *SQLiteStore) updateChatRow(ctx context.Context, ex execer, chat Chat) error {
	res, err := s.execOn(ctx, ex, `
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

// SaveChatWithBlobs writes a chat, its newly-extracted chat_blobs and the
// deletion of chat_blobs it no longer references as ONE atomic unit: either the
// chat update, every insert and every delete persist, or none of them does. It
// is the primitive a run-commit uses to persist a chat document that now
// references images together with those images' bytes, so a crash or error
// between the writes can never leave a document pointing at blobs that do not
// exist (or blobs no document references). The same call is how a chat sheds
// blobs (a pruned or discarded audio answer): the document that stops
// referencing them and the rows themselves go together.
//
// The chat side is UpdateChat (title / key_version / blob / updated_at of an
// existing chat; owner and created_at are not touched) and each blob is a
// PutChatBlob insert; the error mapping is theirs: a chat that does not exist is
// ErrNotFound, a blob id already taken -- by another row or by an earlier blob
// of the same batch -- is ErrConflict. A blob whose ChatID is not chat.ID is
// refused before anything is written (a save only ever attaches blobs to the
// chat it saves). The context bounds the whole transaction.
//
// deleteBlobIDs names blobs of THIS chat to delete (`chat_id = ? and id in
// (...)`, so another chat's blob, an unknown id or a repeated id deletes nothing
// extra and is no error). The delete runs after the inserts, so an insert is
// judged against the rows as they were before the save, and the caller must not
// name an id it also inserts: the new row would be deleted again. nil or empty
// deleteBlobIDs is exactly the save without deletes.
//
// Atomicity is the transaction: the UPDATE, every INSERT and the DELETE run on
// one *sql.Tx and only the final Commit makes them visible; any failure returns
// before Commit and the deferred Rollback discards the lot. The blobs
// deliberately do NOT go through PutChatBlob / s.exec, which run on the pool and
// would commit one by one. The UPDATE runs first, so a missing chat is
// ErrNotFound without uploading a single blob byte or deleting a row, and the
// error precedence (missing chat, then taken id) matches the in-RAM store's. No
// blobs and no deletes is just UpdateChat -- a single statement is already
// atomic.
func (s *SQLiteStore) SaveChatWithBlobs(ctx context.Context, chat Chat, blobs []ChatBlob, deleteBlobIDs []string) error {
	if len(blobs) == 0 && len(deleteBlobIDs) == 0 {
		return s.UpdateChat(ctx, chat)
	}
	for _, blob := range blobs {
		if blob.ChatID != chat.ID {
			return fmt.Errorf("save chat with blobs: blob %q belongs to chat %q, not %q", blob.ID, blob.ChatID, chat.ID)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save chat with blobs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.updateChatRow(ctx, tx, chat); err != nil {
		return err
	}
	for _, blob := range blobs {
		if err := s.insertChatBlob(ctx, tx, blob); err != nil {
			return err
		}
	}
	if err := s.deleteChatBlobIDs(ctx, tx, chat.ID, deleteBlobIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save chat with blobs: %w", err)
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

// ChatOwner returns the user_id of chat chatID, or ErrNotFound. It selects that
// one column and never the sealed document: the chat-image serving path asks it
// once per <img>, and a chat of many images would otherwise read the whole
// transcript blob again for each one just to compare two strings.
func (s *SQLiteStore) ChatOwner(ctx context.Context, chatID string) (string, error) {
	var userID string
	err := s.queryRow(ctx, `select user_id from chats where id = ?`, chatID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("scan chat owner: %w", err)
	}
	return userID, nil
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
