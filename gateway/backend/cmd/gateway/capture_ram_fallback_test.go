// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"testing"
	"time"
)

// TestVerifyNoPlaintextCaptureOnDiskWithoutKey reproduces the exact reviewer
// scenario end to end: DBDriver=sqlite, NO encryption key, capture_enabled
// default-true, and a token with LogCommunication=true. Before the fix this
// wrote a plain gzip (KeyVersion 0) blob into the on-disk captures table. The
// fix must keep the disk table empty (RAM fallback) while still capturing.
func TestVerifyNoPlaintextCaptureOnDiskWithoutKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	// Seed a user + token with gateway:use scope AND log_communication on.
	seed, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := seed.CreateUser(context.Background(), store.User{
		ID: "usr_log", Email: "log@example.test", DisplayName: "Log User",
		Role: "user", Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := seed.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: "tok_log", UserID: "usr_log", Name: "logging-token",
		Status: store.TokenStatusActive, LogCommunication: true,
		Scopes: `["gateway:use"]`,
	}, "seed-secret-value-1234567890"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	cfg := config.Config{
		Addr:                  "127.0.0.1:8080",
		DBDriver:              "sqlite",
		SQLitePath:            dbPath,
		AutoMigrate:           true,
		CaptureMemoryMaxBytes: 4096,
		// no CaptureEncryptionKey on purpose
	}
	srv, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer: %v", err)
	}
	defer cleanup()

	if srv.Cipher != nil {
		t.Fatalf("srv.Cipher = %v, want nil (no key)", srv.Cipher)
	}
	mem, ok := srv.Captures.(*store.MemoryCaptureStore)
	if !ok {
		t.Fatalf("srv.Captures = %T, want *store.MemoryCaptureStore", srv.Captures)
	}

	rec := performChatCompletionForModel(t, srv, "seed-secret-value-1234567890", "qwen-coder")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// SECURITY assertion: the on-disk captures table must have ZERO rows.
	//
	// srv is still live: buildGatewayServer auto-seeds a default AI server
	// (seedDefaultServerIfEmpty, since this test seeds none) and its app-health
	// probe loop persists that server's health in the background via an
	// ai_servers UPDATE. That concurrent writer briefly holds a SQLite write
	// lock, so a bare read connection races it and fails outright with
	// SQLITE_BUSY (~1 in 5). Every production connection sets busy_timeout(5000)
	// (see store.sqliteDSN); mirror it here so the read simply waits out the
	// transient lock. This does NOT weaken the assertion — the count below still
	// has to be 0; it just makes the read reliable.
	dsn := url.URL{Scheme: "file", Path: dbPath}
	q := dsn.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var diskCount int
	if err := db.QueryRow("select count(*) from captures").Scan(&diskCount); err != nil {
		t.Fatalf("count captures: %v", err)
	}
	if diskCount != 0 {
		t.Fatalf("on-disk captures = %d, want 0 (must never persist unencrypted to disk)", diskCount)
	}

	// The capture must still exist — in RAM. Find the usage event id and confirm.
	var eventID string
	if err := db.QueryRow("select id from usage_events order by created_at desc limit 1").Scan(&eventID); err != nil {
		t.Fatalf("select usage event id: %v", err)
	}
	has, err := mem.HasCaptures(context.Background(), []string{eventID})
	if err != nil {
		t.Fatalf("HasCaptures: %v", err)
	}
	if _, ok := has[eventID]; !ok {
		t.Fatalf("RAM store has no capture for %q; capture was lost", eventID)
	}
	t.Logf("OK: disk captures=0, RAM capture present for %q", eventID)
}
