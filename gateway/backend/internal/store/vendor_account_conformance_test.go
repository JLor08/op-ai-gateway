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

// normalizeVendorAccountForCompare makes two routing.VendorAccount values
// comparable with == across the dialects: postgres returns timestamptz in a
// different *time.Location than the sqlite driver (and than the UTC value the
// test wrote), so every time is compared as a UTC wall-clock value. Nothing
// else is touched -- in particular no field is zeroed, which would be the way
// to accidentally exclude a column from the comparison.
func normalizeVendorAccountForCompare(in routing.VendorAccount) routing.VendorAccount {
	out := in
	out.CreatedAt = in.CreatedAt.UTC()
	out.UpdatedAt = in.UpdatedAt.UTC()
	return out
}

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
		if normalizeVendorAccountForCompare(got) != normalizeVendorAccountForCompare(acc) {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, acc)
		}
		all, err := s.VendorAccounts(ctx)
		if err != nil || len(all) != 1 || normalizeVendorAccountForCompare(all[0]) != normalizeVendorAccountForCompare(acc) {
			t.Fatalf("list = %v, %v", all, err)
		}
		byOwner, err := s.VendorAccountsByOwner(ctx, "u_va")
		if err != nil || len(byOwner) != 1 || normalizeVendorAccountForCompare(byOwner[0]) != normalizeVendorAccountForCompare(acc) {
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

// TestRoutingStoreVendorAccountUpdateAndDeleteContract pins the parts of the
// store contract the happy-path CRUD test above does not reach, identically on
// every driver: an unknown id is ErrNotFound on Update and Delete, ownership
// filtering never leaks another user's accounts, and Update rewrites only the
// mutable columns -- the account's identity (id, owner, vendor, created_at)
// survives an Update that tries to change it.
func TestRoutingStoreVendorAccountUpdateAndDeleteContract(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		for _, id := range []string{"u_va_a", "u_va_b"} {
			if err := s.CreateUser(context.Background(), newTestUser(id, id+"@example.test", now)); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		ghost := routing.VendorAccount{
			ID: "va_ghost", OwnerUserID: "u_va_a", Vendor: routing.VendorAnthropic,
			AuthType: routing.VendorAuthAPIKey, Name: "Ghost", Status: routing.VendorAccountStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpdateVendorAccount(ctx, ghost); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("update unknown err = %v, want ErrNotFound", err)
		}
		if err := s.DeleteVendorAccount(ctx, "va_ghost"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("delete unknown err = %v, want ErrNotFound", err)
		}

		mine := routing.VendorAccount{
			ID: "va_mine", OwnerUserID: "u_va_a", Vendor: routing.VendorAnthropic,
			AuthType: routing.VendorAuthAPIKey, Name: "Mine", Status: routing.VendorAccountStatusActive,
			APIKey: "enc:key", CreatedAt: now, UpdatedAt: now,
		}
		theirs := routing.VendorAccount{
			ID: "va_theirs", OwnerUserID: "u_va_b", Vendor: routing.VendorOpenAI,
			AuthType: routing.VendorAuthSubscription, Name: "Theirs", Status: routing.VendorAccountStatusNeedsReconnect,
			OAuthTokens: "enc:tokens", CreatedAt: now, UpdatedAt: now,
		}
		for _, acc := range []routing.VendorAccount{theirs, mine} {
			if err := s.CreateVendorAccount(ctx, acc); err != nil {
				t.Fatalf("create %s: %v", acc.ID, err)
			}
		}

		all, err := s.VendorAccounts(ctx)
		if err != nil || len(all) != 2 || all[0].ID != "va_mine" || all[1].ID != "va_theirs" {
			t.Fatalf("list not ordered by id: %+v, %v", all, err)
		}
		owned, err := s.VendorAccountsByOwner(ctx, "u_va_a")
		if err != nil || len(owned) != 1 || owned[0].ID != "va_mine" {
			t.Fatalf("by owner leaked or lost rows: %+v, %v", owned, err)
		}
		none, err := s.VendorAccountsByOwner(ctx, "u_nobody")
		if err != nil || len(none) != 0 {
			t.Fatalf("by unknown owner = %+v, %v, want empty", none, err)
		}

		// Update every mutable column, and try to rewrite the identity too.
		upd := mine
		upd.OwnerUserID = "u_va_b"
		upd.Vendor = routing.VendorOpenAI
		upd.CreatedAt = now.Add(-time.Hour)
		upd.AuthType = routing.VendorAuthSubscription
		upd.Name = "Mine, edited"
		upd.Status = routing.VendorAccountStatusDisabled
		upd.APIKey = ""
		upd.OAuthTokens = "enc:new-tokens"
		upd.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateVendorAccount(ctx, upd); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, err := s.VendorAccountByID(ctx, "va_mine")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		want := mine
		want.AuthType = upd.AuthType
		want.Name = upd.Name
		want.Status = upd.Status
		want.APIKey = upd.APIKey
		want.OAuthTokens = upd.OAuthTokens
		want.UpdatedAt = upd.UpdatedAt
		if normalizeVendorAccountForCompare(got) != normalizeVendorAccountForCompare(want) {
			t.Fatalf("update touched identity or missed a column:\n got  %+v\n want %+v", got, want)
		}
		if other, _ := s.VendorAccountByID(ctx, "va_theirs"); normalizeVendorAccountForCompare(other) != normalizeVendorAccountForCompare(theirs) {
			t.Fatalf("update of va_mine changed another account: %+v", other)
		}

		if err := s.DeleteVendorAccount(ctx, "va_mine"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := s.DeleteVendorAccount(ctx, "va_mine"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("second delete err = %v, want ErrNotFound", err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_theirs"); err != nil {
			t.Fatalf("delete of va_mine removed another account: %v", err)
		}
	})
}
