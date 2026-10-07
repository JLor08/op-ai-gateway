// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"testing"
	"time"
)

func TestRoutingStoreVendorAccountCRUD(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_va", "va@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		// The memory store has no FK, so it needs no user seed; the SQL legs
		// were seeded above.
		acc := routing.VendorAccount{
			ID: "va_one", OwnerUserID: "u_va", Vendor: routing.VendorOpenAI,
			AuthType: routing.VendorAuthAPIKey, Name: "My OpenAI", Status: routing.VendorAccountStatusActive,
			APIKey: "enc:seeded", OAuthTokens: "", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := s.CreateVendorAccount(ctx, acc); !errors.Is(err, storeerr.ErrConflict) {
			t.Fatalf("duplicate create err = %v, want ErrConflict", err)
		}
		got, err := s.VendorAccountByID(ctx, "va_one")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got != acc {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, acc)
		}
		all, err := s.VendorAccounts(ctx)
		if err != nil || len(all) != 1 || all[0] != acc {
			t.Fatalf("list = %v, %v", all, err)
		}
		byOwner, err := s.VendorAccountsByOwner(ctx, "u_va")
		if err != nil || len(byOwner) != 1 || byOwner[0] != acc {
			t.Fatalf("by owner = %v, %v", byOwner, err)
		}
		acc.Name = "Renamed"
		acc.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got, _ := s.VendorAccountByID(ctx, "va_one"); got.Name != "Renamed" {
			t.Fatalf("update not applied: %+v", got)
		}
		if err := s.DeleteVendorAccount(ctx, "va_one"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_one"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("after delete err = %v, want ErrNotFound", err)
		}
	})
}
