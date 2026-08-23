// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSQLiteCreateAndFindUser(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	user := User{
		ID:                "usr_1",
		Email:             "Admin@Example.Test",
		DisplayName:       "Admin User",
		Role:              "admin",
		Status:            UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}

	byID, err := st.UserByID(ctx, "usr_1")
	if err != nil {
		t.Fatalf("UserByID returned %v", err)
	}
	if byID.Email != "admin@example.test" {
		t.Fatalf("Email = %q, want admin@example.test", byID.Email)
	}
	if byID.DisplayName != "Admin User" || byID.Role != "admin" || byID.Status != UserStatusActive {
		t.Fatalf("user = %#v", byID)
	}
	if byID.PreferredLanguage != "de" {
		t.Fatalf("PreferredLanguage = %q, want de", byID.PreferredLanguage)
	}

	byEmail, err := st.UserByEmail(ctx, " admin@example.test ")
	if err != nil {
		t.Fatalf("UserByEmail returned %v", err)
	}
	if byEmail.ID != "usr_1" {
		t.Fatalf("ID = %q, want usr_1", byEmail.ID)
	}
}

func TestSQLiteCreateUserRejectsDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	first := User{ID: "usr_1", Email: "admin@example.test", DisplayName: "Admin", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}
	second := User{ID: "usr_2", Email: "ADMIN@example.test", DisplayName: "Other", Role: "user", Status: UserStatusActive, PreferredLanguage: "en", CreatedAt: now, UpdatedAt: now}

	if err := st.CreateUser(ctx, first); err != nil {
		t.Fatalf("CreateUser first returned %v", err)
	}
	err := st.CreateUser(ctx, second)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateUser duplicate error = %v, want ErrConflict", err)
	}
}

func TestSQLiteUserByEmailNormalizesEmail(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	user := User{ID: "usr_1", Email: "person@example.test", DisplayName: "Person", Role: "user", Status: UserStatusActive, PreferredLanguage: "en", CreatedAt: now, UpdatedAt: now}

	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}
	found, err := st.UserByEmail(ctx, " PERSON@EXAMPLE.TEST ")
	if err != nil {
		t.Fatalf("UserByEmail returned %v", err)
	}
	if found.ID != "usr_1" {
		t.Fatalf("ID = %q, want usr_1", found.ID)
	}
}

func TestSQLiteUserByIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	_, err := st.UserByID(ctx, "missing")

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByID error = %v, want ErrNotFound", err)
	}
}

func TestUserPasswordRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	user := User{ID: "usr_pw", Email: "pw@example.test", DisplayName: "PW", Role: "user", Status: UserStatusInvited, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	loaded, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if loaded.PasswordHash != "" || loaded.PasswordSetAt != nil {
		t.Fatalf("new user must have empty password, got hash=%q setAt=%v", loaded.PasswordHash, loaded.PasswordSetAt)
	}
	setAt := now.Add(time.Minute)
	loaded.PasswordHash = "hashed-value"
	loaded.Status = UserStatusActive
	loaded.PasswordSetAt = &setAt
	loaded.UpdatedAt = setAt
	if err := st.UpdateUser(ctx, loaded); err != nil {
		t.Fatalf("update user: %v", err)
	}
	again, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if again.PasswordHash != "hashed-value" || again.Status != UserStatusActive || again.PasswordSetAt == nil {
		t.Fatalf("password update not persisted: %+v", again)
	}
}

func TestUserChatFlagsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	user := User{ID: "usr_chat", Email: "chat@example.test", DisplayName: "Chat User", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	loaded, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if loaded.ChatLogCommunication || loaded.ChatSecret {
		t.Fatalf("new user chat flags must default false, got log=%v secret=%v", loaded.ChatLogCommunication, loaded.ChatSecret)
	}

	loaded.ChatLogCommunication = true
	loaded.ChatSecret = true
	loaded.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateUser(ctx, loaded); err != nil {
		t.Fatalf("update user: %v", err)
	}

	// All three SELECT paths must scan the two new columns in aligned positions:
	// byID, byEmail, and the list query. A misalignment would corrupt a
	// neighbouring string field, so assert those too.
	byID, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload by id: %v", err)
	}
	if !byID.ChatLogCommunication || !byID.ChatSecret {
		t.Fatalf("chat flags not persisted (byID): %+v", byID)
	}
	if byID.Email != "chat@example.test" || byID.DisplayName != "Chat User" || byID.Role != "user" || byID.PreferredLanguage != "de" {
		t.Fatalf("column misalignment corrupted fields (byID): %+v", byID)
	}

	byEmail, err := st.UserByEmail(ctx, "chat@example.test")
	if err != nil {
		t.Fatalf("reload by email: %v", err)
	}
	if !byEmail.ChatLogCommunication || !byEmail.ChatSecret {
		t.Fatalf("chat flags not persisted (byEmail): %+v", byEmail)
	}
	if byEmail.DisplayName != "Chat User" || byEmail.PreferredLanguage != "de" {
		t.Fatalf("column misalignment corrupted fields (byEmail): %+v", byEmail)
	}

	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || !users[0].ChatLogCommunication || !users[0].ChatSecret {
		t.Fatalf("chat flags not persisted (list): %+v", users)
	}
	if users[0].DisplayName != "Chat User" || users[0].PreferredLanguage != "de" {
		t.Fatalf("column misalignment corrupted fields (list): %+v", users[0])
	}
}

func TestListUsersOrdersByCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	base := time.Now().UTC()
	first := User{ID: "usr_a", Email: "a@example.test", DisplayName: "A", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: base, UpdatedAt: base}
	second := User{ID: "usr_b", Email: "b@example.test", DisplayName: "B", Role: "user", Status: UserStatusInvited, PreferredLanguage: "en", CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(time.Second)}
	if err := st.CreateUser(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := st.CreateUser(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 || users[0].ID != "usr_a" || users[1].ID != "usr_b" {
		t.Fatalf("unexpected user order: %+v", users)
	}
}
