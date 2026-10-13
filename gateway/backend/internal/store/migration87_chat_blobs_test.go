// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMigration87CreatesChatBlobs pins the shape migration 87 owes the chat
// binary-payload store (#124): a chat_blobs table whose columns are wide enough
// for the Go values written to them (byte_len is bigint -- integer is int4 on
// postgres -- and blob is the dialect's binary type), plus the lookup index on
// chat_id. It runs on both dialects, so a postgres-only type slip shows up here.
func TestMigration87CreatesChatBlobs(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()

		decls := tableColumnDecls(ctx, t, s, "chat_blobs")
		wantBlob, wantTS := "blob", "timestamp"
		if s.dl.name() == "postgres" {
			// information_schema spells timestamptz out in full.
			wantBlob, wantTS = "bytea", "timestamp with time zone"
		}
		for col, wantType := range map[string]string{
			"id":           "text",
			"chat_id":      "text",
			"key_version":  "integer",
			"blob":         wantBlob,
			"content_type": "text",
			"byte_len":     "bigint",
			"created_at":   wantTS,
		} {
			raw, ok := decls[col]
			if !ok {
				t.Errorf("chat_blobs lacks column %s: %v", col, sortedColumnNames(decls))
				continue
			}
			// sqlite reports the declared type verbatim (any case), postgres
			// lower-case; the nullability marker follows the type.
			d := strings.ToLower(raw)
			if !strings.HasPrefix(d, wantType+" ") {
				t.Errorf("chat_blobs.%s = %q, want type %q", col, raw, wantType)
			}
			// A sqlite "text primary key" reports notnull=0 (the PK implies it
			// only on postgres), so id is the one column not asserted here.
			if col != "id" && !strings.Contains(d, "not null") {
				t.Errorf("chat_blobs.%s = %q, want NOT NULL", col, raw)
			}
		}
		if len(decls) != 7 {
			t.Errorf("chat_blobs has %d columns %v, want exactly 7", len(decls), sortedColumnNames(decls))
		}

		indexQuery := `select count(*) from sqlite_master where type = 'index' and name = ?`
		if s.dl.name() == "postgres" {
			indexQuery = `select count(*) from pg_indexes where schemaname = 'public' and indexname = ?`
		}
		var n int
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(indexQuery), "idx_chat_blobs_chat").Scan(&n); err != nil {
			t.Fatalf("look up idx_chat_blobs_chat: %v", err)
		}
		if n != 1 {
			t.Errorf("idx_chat_blobs_chat count = %d, want 1", n)
		}
	})
}

// TestMigration87ChatBlobsCascadeWithTheChat proves chat_blobs.chat_id is a
// foreign key to chats(id) declared ON DELETE CASCADE: a blob for a missing
// chat is refused, and deleting the chat takes its blobs with it (a plain
// reference would instead make the DeleteChat itself fail).
func TestMigration87ChatBlobsCascadeWithTheChat(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, newTestUser("u_m87", "m87@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if err := s.CreateChat(ctx, Chat{
			ID: "c_m87", UserID: "u_m87", Title: "t", KeyVersion: 1, Blob: []byte("{}"),
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed chat: %v", err)
		}

		insert := s.dl.rebind(`insert into chat_blobs (id, chat_id, key_version, blob, content_type, byte_len, created_at)
			values (?, ?, ?, ?, ?, ?, ?)`)
		if _, err := s.db.ExecContext(ctx, insert, "b_orphan", "c_missing", 1, []byte("x"), "image/png", int64(1), now); err == nil {
			t.Fatalf("insert for a missing chat succeeded; chat_id must reference chats(id)")
		} else if !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("insert for a missing chat = %v, want a foreign-key violation", err)
		}

		// byte_len is bigint: a value past int32 must round-trip.
		const big = int64(5_000_000_000)
		if _, err := s.db.ExecContext(ctx, insert, "b_m87", "c_m87", 1, []byte("sealed"), "image/png", big, now); err != nil {
			t.Fatalf("insert blob: %v", err)
		}
		var gotLen int64
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select byte_len from chat_blobs where id = ?`), "b_m87").Scan(&gotLen); err != nil {
			t.Fatalf("read byte_len: %v", err)
		}
		if gotLen != big {
			t.Fatalf("byte_len = %d, want %d", gotLen, big)
		}

		if err := s.DeleteChat(ctx, "c_m87"); err != nil {
			t.Fatalf("delete chat: %v", err)
		}
		var left int
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from chat_blobs where chat_id = ?`), "c_m87").Scan(&left); err != nil {
			t.Fatalf("count blobs: %v", err)
		}
		if left != 0 {
			t.Fatalf("chat_blobs rows after deleting the chat = %d, want 0 (ON DELETE CASCADE)", left)
		}
	})
}
