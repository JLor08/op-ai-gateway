// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSetPasswordTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	mustCreateUser(t, st, "usr_spt", "spt@example.test")
	tok := SetPasswordToken{ID: "spt_1", UserID: "usr_spt", SecretHash: "spt-hash", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := st.CreateSetPasswordToken(ctx, tok); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.SetPasswordTokenBySecret(ctx, "spt-hash")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if loaded.UsedAt != nil {
		t.Fatalf("token should be unused, got %v", loaded.UsedAt)
	}
	used := now.Add(time.Minute)
	if err := st.MarkSetPasswordTokenUsed(ctx, "spt_1", used); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	loaded, _ = st.SetPasswordTokenBySecret(ctx, "spt-hash")
	if loaded.UsedAt == nil {
		t.Fatal("token should now be used")
	}
}

func TestInvalidateUserSetPasswordTokens(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC()
	mustCreateUser(t, st, "usr_inv", "inv@example.test")
	if err := st.CreateSetPasswordToken(ctx, SetPasswordToken{ID: "spt_x", UserID: "usr_inv", SecretHash: "x", ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.InvalidateUserSetPasswordTokens(ctx, "usr_inv"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	loaded, err := st.SetPasswordTokenBySecret(ctx, "x")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return
		}
		t.Fatalf("lookup: %v", err)
	}
	if loaded.UsedAt == nil {
		t.Fatal("invalidated token must be marked used")
	}
}
