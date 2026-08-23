// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	// modernc.org/sqlite registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"
)

type SQLStore struct {
	db             *sql.DB
	dl             dialect
	usageErrMu     sync.RWMutex
	lastUsageError error
}

// SQLiteStore is retained as an alias so existing references keep compiling.
type SQLiteStore = SQLStore

func OpenSQLite(path string) (*SQLStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("sqlite path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(absolutePath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	return &SQLStore{db: db, dl: sqliteDialect{}}, nil
}

func sqliteDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLStore) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.dl.rebind(q), sanitizeArgs(args)...)
}

func (s *SQLStore) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.dl.rebind(q), sanitizeArgs(args)...)
}

func (s *SQLStore) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.dl.rebind(q), sanitizeArgs(args)...)
}

// sanitizeArgs converts every bool argument to an int64 0/1. Every boolean flag
// in this store's schema (chat_log_communication, chat_secret, log_communication,
// secret, always_reachable, stream, ...) is a plain integer column on BOTH
// dialects — there is no SQL BOOLEAN type in play. modernc.org/sqlite happens to
// silently coerce a Go bool into an integer column, but pgx's stdlib driver does
// not ("unable to encode false into binary format for int4: cannot find encode
// plan") — a real dialect bug the conformance suite caught (every write of a bool
// DTO field failed against real Postgres). Converting once at this single seam
// keeps every call site free to pass a native Go bool while staying valid on
// both drivers, instead of requiring each DTO-to-args mapping to remember an
// explicit int64(0)/int64(1) conversion by hand.
func sanitizeArgs(args []any) []any {
	out := args
	copied := false
	for i, a := range args {
		b, ok := a.(bool)
		if !ok {
			continue
		}
		if !copied {
			// copy-on-write: never mutate the caller's backing array.
			out = append([]any(nil), args...)
			copied = true
		}
		if b {
			out[i] = int64(1)
		} else {
			out[i] = int64(0)
		}
	}
	return out
}
