// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// dialect isolates the SQLite-vs-Postgres SQL differences. It is the ONLY place
// in the store package that knows which database is in use.
type dialect interface {
	name() string // "sqlite" | "postgres"
	rebind(query string) string
	blobType() string      // "blob"      | "bytea"
	timestampType() string // "timestamp" | "timestamptz"
	ilike() string         // "like" (sqlite LIKE is ASCII case-insensitive) | "ilike"
	isUniqueViolation(err error) bool
	isForeignKeyViolation(err error) bool
}

type sqliteDialect struct{}

func (sqliteDialect) name() string           { return "sqlite" }
func (sqliteDialect) rebind(q string) string { return q } // sqlite uses ? natively
func (sqliteDialect) blobType() string       { return "blob" }
func (sqliteDialect) timestampType() string  { return "timestamp" }
func (sqliteDialect) ilike() string          { return "like" }

// isUniqueViolation / isForeignKeyViolation classify modernc.org/sqlite errors by
// message text (moved verbatim from the old isSQLiteConstraintError/…ForeignKeyError).
func (sqliteDialect) isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint failed") || strings.Contains(msg, "unique constraint")
}

func (sqliteDialect) isForeignKeyViolation(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "foreign key")
}

type postgresDialect struct{}

func (postgresDialect) name() string          { return "postgres" }
func (postgresDialect) blobType() string      { return "bytea" }
func (postgresDialect) timestampType() string { return "timestamptz" }
func (postgresDialect) ilike() string         { return "ilike" }

// rebind rewrites ?-style placeholders to $1,$2,… . The store's SQL never
// contains a literal '?' outside a placeholder position (verified), so a simple
// left-to-right pass is correct.
func (postgresDialect) rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (postgresDialect) isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (postgresDialect) isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
