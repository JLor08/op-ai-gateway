// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSqliteDialectRebindIsIdentity(t *testing.T) {
	d := sqliteDialect{}
	q := `select * from t where a = ? and b = ?`
	if got := d.rebind(q); got != q {
		t.Fatalf("sqlite rebind changed the query: %q", got)
	}
	if d.name() != "sqlite" || d.blobType() != "blob" || d.timestampType() != "timestamp" || d.ilike() != "like" {
		t.Fatalf("unexpected sqlite dialect constants")
	}
}

func TestSqliteDialectClassifiers(t *testing.T) {
	d := sqliteDialect{}
	if !d.isUniqueViolation(errors.New("UNIQUE constraint failed: users.email")) {
		t.Fatal("expected unique violation")
	}
	if !d.isForeignKeyViolation(errors.New("FOREIGN KEY constraint failed")) {
		t.Fatal("expected fk violation")
	}
	if d.isUniqueViolation(errors.New("some other error")) {
		t.Fatal("false positive")
	}
}

func TestPostgresDialectRebind(t *testing.T) {
	d := postgresDialect{}
	got := d.rebind(`insert into t (a,b,c) values (?,?,?)`)
	want := `insert into t (a,b,c) values ($1,$2,$3)`
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
	if d.name() != "postgres" || d.blobType() != "bytea" || d.timestampType() != "timestamptz" || d.ilike() != "ilike" {
		t.Fatalf("unexpected postgres dialect constants")
	}
}

func TestPostgresDialectClassifiers(t *testing.T) {
	d := postgresDialect{}
	uniq := &pgconn.PgError{Code: "23505"}
	fk := &pgconn.PgError{Code: "23503"}
	if !d.isUniqueViolation(uniq) || d.isUniqueViolation(fk) {
		t.Fatal("unique classifier wrong")
	}
	if !d.isForeignKeyViolation(fk) || d.isForeignKeyViolation(uniq) {
		t.Fatal("fk classifier wrong")
	}
	if d.isUniqueViolation(errors.New("plain")) {
		t.Fatal("false positive on non-pg error")
	}
}
