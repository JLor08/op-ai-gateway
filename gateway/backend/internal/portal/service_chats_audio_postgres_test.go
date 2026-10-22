// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"op-ai-gateway/internal/store"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The retroactive prune on PostgreSQL, through the service (#281).
//
// forEachRetentionLeg stops at memory and SQLite on purpose: internal/store owns the
// shared test database (OP_AI_GATEWAY_TEST_POSTGRES_DSN) and every one of its
// subtests runs `drop schema public cascade` on it, so a second package migrating and
// writing into that database would be dropped from under, or would drop from under,
// the store's tests whenever `go test ./...` runs the two packages side by side. The
// atomic delete the prune is built on is pinned on PostgreSQL there already
// (TestSaveChatWithBlobsDeletes* in internal/store); what this adds is the COMPOSED
// path -- document rewrite, marker, blob delete -- over the real PostgreSQL driver.
//
// It does not touch the shared database's schema at all. A second schema in the same
// database would not do: the store's tests ask the catalog about tables, columns,
// indexes and constraints by bare name (`pg_indexes where indexname = ?`,
// `information_schema.columns`, `pg_constraint`), so a copy of the schema next to
// theirs would be counted as theirs. Instead the test creates a DATABASE of its own
// on the same server, from template0, with a unique name, migrates and uses that, and
// drops it afterwards. Nothing in it is visible from the shared database, and the
// only thing the two packages hold in common is the server: CREATE DATABASE and DROP
// DATABASE lock no table of the shared one, and the connection that issues them holds
// none either.

// A name this long is still a valid identifier (63 bytes at most); it is hex, so it
// needs no quoting.
const retentionPostgresDBPrefix = "op_portal_retention_"

// retentionPostgresTimeout bounds create, open, migrate and drop (the test body is not
// bounded by it): each is under a few seconds against a local server.
const retentionPostgresTimeout = 90 * time.Second

// newPostgresRetentionEnv is the PostgreSQL counterpart of one forEachRetentionLeg leg:
// a Service over a freshly migrated database that exists only for the calling test.
// It skips, like every PostgreSQL leg, when the DSN is not set; it also skips (saying
// why) when the DSN cannot be used to create a database, because the store's own
// tests need no more than a schema to drop and so a restricted role can run them.
func newPostgresRetentionEnv(t *testing.T) *retentionEnv {
	t.Helper()
	dsn := os.Getenv("OP_AI_GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run the postgres retention test")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Skip("the postgres retention test needs a URL-form DSN to derive its own database's DSN from")
	}
	ctx, cancel := context.WithTimeout(context.Background(), retentionPostgresTimeout)
	defer cancel()

	// The admin connection only issues CREATE DATABASE and DROP DATABASE; it never
	// writes a table of the database the DSN names.
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	admin.SetMaxOpenConns(1)
	t.Cleanup(func() { admin.Close() })

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := retentionPostgresDBPrefix + hex.EncodeToString(suffix[:])
	if _, err := admin.ExecContext(ctx, `create database `+name+` template template0`); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
			t.Skipf("the DSN's role may not create a database, which this test needs to stay out of the store tests' database: %v", err)
		}
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), retentionPostgresTimeout)
		defer dropCancel()
		// WITH (FORCE) so a connection the test failed to close cannot leave the
		// database behind.
		if _, err := admin.ExecContext(dropCtx, `drop database if exists `+name+` with (force)`); err != nil {
			t.Errorf("drop database %s: %v (drop it by hand)", name, err)
		}
	})

	own := *u
	own.Path = "/" + name
	st, err := store.OpenPostgres(ctx, own.String())
	if err != nil {
		t.Fatalf("OpenPostgres(%s): %v", name, err)
	}
	// Registered after the drop above, so it runs first: the pool is closed before the
	// database goes.
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(%s): %v", name, err)
	}

	owner := chatToken("usr_a")
	seedRetentionOwner(t, st, owner)
	rec := &recordingChatStore{ChatStore: st}
	return &retentionEnv{svc: NewService(ServiceDeps{Chats: rec}), rec: rec, owner: owner}
}

// The retroactive prune of TestPruneChatAudioToKeepLast, word for word, over the
// PostgreSQL driver: the newest N answers keep their audio, every older answer
// becomes the marker with ALL its blobs (one is a two-blob answer), the rewrite and the
// delete leave in one atomic save, and pruning again writes nothing.
func TestPruneChatAudioToKeepLastOnPostgres(t *testing.T) {
	pruneChatAudioToKeepLast(t, newPostgresRetentionEnv(t))
}
