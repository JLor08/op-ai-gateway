// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	mustCreateUser(t, st, "usr_s", "s@example.test")
	session := Session{ID: "sess_1", UserID: "usr_s", SecretHash: "hash-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	if err := st.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	loaded, err := st.SessionBySecret(ctx, "hash-1")
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if loaded.ID != "sess_1" || loaded.UserID != "usr_s" {
		t.Fatalf("unexpected session: %+v", loaded)
	}
	touched := now.Add(10 * time.Minute)
	if err := st.TouchSession(ctx, "sess_1", touched); err != nil {
		t.Fatalf("touch session: %v", err)
	}
	loaded, _ = st.SessionBySecret(ctx, "hash-1")
	if !loaded.LastSeenAt.Equal(touched) {
		t.Fatalf("last seen not updated: %v", loaded.LastSeenAt)
	}
	if err := st.DeleteSession(ctx, "sess_1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := st.SessionBySecret(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session should be gone, got %v", err)
	}
}

func TestDeleteSessionsByUser(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	mustCreateUser(t, st, "usr_multi", "multi@example.test")
	for _, id := range []string{"sess_a", "sess_b"} {
		if err := st.CreateSession(ctx, Session{ID: id, UserID: "usr_multi", SecretHash: "h-" + id, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := st.DeleteSessionsByUser(ctx, "usr_multi"); err != nil {
		t.Fatalf("delete by user: %v", err)
	}
	if _, err := st.SessionBySecret(ctx, "h-sess_a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sessions should be revoked, got %v", err)
	}
}

func mustCreateUser(t *testing.T, st *SQLiteStore, id, email string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateUser(context.Background(), User{ID: id, Email: email, DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}
