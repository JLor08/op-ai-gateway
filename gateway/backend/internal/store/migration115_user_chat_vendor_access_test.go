// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMigration115AddsUserChatVendorAccess runs migration 115 over a genuine
// version-114 database holding a user row and pins what an upgrade depends on:
// the column is absent before and present after on every dialect, declared
// with the default that makes every existing user read back as "unset" (the
// token-less chat keeps its all-own-accounts behaviour), and a replay is a
// no-op that leaves a stored value alone.
func TestMigration115AddsUserChatVendorAccess(t *testing.T) {
	forEachDialectMigratedTo(t, 114, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		const want = "text not null default ''"
		if _, ok := tableColumnDecls(ctx, t, s, "users")["chat_vendor_provider_access"]; ok {
			t.Fatalf("users already has chat_vendor_provider_access at v114")
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into users (id, email, display_name, role, status, preferred_language, created_at, updated_at)
			 values (?, ?, ?, ?, ?, ?, ?, ?)`),
			"usr_m115", "m115@example.test", "M115", "user", UserStatusActive, "de", now, now); err != nil {
			t.Fatalf("seed v114 user: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		got, ok := tableColumnDecls(ctx, t, s, "users")["chat_vendor_provider_access"]
		if !ok || !strings.EqualFold(strings.ReplaceAll(got, "::text", ""), want) {
			t.Fatalf("users.chat_vendor_provider_access = %q (present %v), want %q", got, ok, want)
		}
		user, err := s.UserByID(ctx, "usr_m115")
		if err != nil {
			t.Fatalf("read pre-migration user: %v", err)
		}
		if user.ChatVendorAccess != "" {
			t.Fatalf("pre-migration user must read unset (''), got %q", user.ChatVendorAccess)
		}

		user.ChatVendorAccess = `{"all":false}`
		if err := s.UpdateUser(ctx, user); err != nil {
			t.Fatalf("update: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration115Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration115Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		again, err := s.UserByID(ctx, "usr_m115")
		if err != nil || again.ChatVendorAccess != `{"all":false}` {
			t.Fatalf("replay disturbed the stored value: %q err=%v", again.ChatVendorAccess, err)
		}
	})
}

// TestUserChatVendorAccessRoundTrip pins that the column is written by
// CreateUser/UpdateUser and read back verbatim by every SELECT path, on both
// SQL dialects. An explicit "no providers" policy ({"all":false}) must stay
// distinguishable from the unset default (”).
func TestUserChatVendorAccessRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		const policy = `{"all":false}`
		user := User{
			ID: "usr_cva", Email: "cva@example.test", DisplayName: "CVA", Role: "user", Status: UserStatusActive,
			PreferredLanguage: "de", ChatVendorAccess: policy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		unset := User{
			ID: "usr_cva_unset", Email: "cva2@example.test", DisplayName: "Unset", Role: "user", Status: UserStatusActive,
			PreferredLanguage: "de", CreatedAt: now.Add(time.Second), UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, unset); err != nil {
			t.Fatalf("create unset: %v", err)
		}
		byID, err := s.UserByID(ctx, user.ID)
		if err != nil || byID.ChatVendorAccess != policy || byID.DisplayName != "CVA" || byID.PreferredLanguage != "de" {
			t.Fatalf("byID = %+v err=%v", byID, err)
		}
		byEmail, err := s.UserByEmail(ctx, "cva@example.test")
		if err != nil || byEmail.ChatVendorAccess != policy || byEmail.DisplayName != "CVA" {
			t.Fatalf("byEmail = %+v err=%v", byEmail, err)
		}
		users, err := s.ListUsers(ctx)
		if err != nil || len(users) != 2 || users[0].ChatVendorAccess != policy || users[1].ChatVendorAccess != "" {
			t.Fatalf("list = %+v err=%v", users, err)
		}

		byID.ChatVendorAccess = `{"all":true,"include_groups":true}`
		if err := s.UpdateUser(ctx, byID); err != nil {
			t.Fatalf("update: %v", err)
		}
		after, err := s.UserByID(ctx, user.ID)
		if err != nil || after.ChatVendorAccess != `{"all":true,"include_groups":true}` {
			t.Fatalf("after update = %+v err=%v", after, err)
		}
	})
}

// createUserAtBaseline inserts a user using only the columns that exist at
// every schema version before migration 115 (the chat flags keep their column
// defaults, which is all these tests seed). The migration tests that stop the
// ledger below 115 seed their user through it: CreateUser writes the
// chat_vendor_provider_access column, which such a database does not have yet.
func createUserAtBaseline(ctx context.Context, s *SQLStore, user User) error {
	user.Email = normalizeEmail(user.Email)
	_, err := s.db.ExecContext(ctx, s.dl.rebind(`
		insert into users (id, email, display_name, role, status, preferred_language,
			created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?)`),
		user.ID, user.Email, user.DisplayName, user.Role, user.Status, user.PreferredLanguage,
		user.CreatedAt, user.UpdatedAt)
	return err
}
