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
			APIKey: "enc:seeded", OAuthTokens: "", ModelPrefix: "work/", CreatedAt: now, UpdatedAt: now,
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
		acc.ModelPrefix = "home-"
		acc.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got, _ := s.VendorAccountByID(ctx, "va_one"); got.Name != "Renamed" || got.ModelPrefix != "home-" {
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
			APIKey: "enc:key", ModelPrefix: "mine/", CreatedAt: now, UpdatedAt: now,
		}
		theirs := routing.VendorAccount{
			ID: "va_theirs", OwnerUserID: "u_va_b", Vendor: routing.VendorOpenAI,
			AuthType: routing.VendorAuthSubscription, Name: "Theirs", Status: routing.VendorAccountStatusNeedsReconnect,
			OAuthTokens: "enc:tokens", ModelPrefix: "theirs/", CreatedAt: now, UpdatedAt: now,
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
		upd.ModelPrefix = "edited-"
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
		want.ModelPrefix = upd.ModelPrefix
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

// normalizeVendorAccountUsageForCompare makes two routing.VendorAccountUsage
// values comparable across dialects: postgres returns timestamps in a different
// *time.Location, so every time (the two optional resets and UpdatedAt) is
// compared as a UTC wall-clock value. nil reset pointers are left nil.
func normalizeVendorAccountUsageForCompare(in routing.VendorAccountUsage) routing.VendorAccountUsage {
	out := in
	out.UpdatedAt = in.UpdatedAt.UTC()
	if in.FiveHourResetAt != nil {
		t := in.FiveHourResetAt.UTC()
		out.FiveHourResetAt = &t
	}
	if in.WeeklyResetAt != nil {
		t := in.WeeklyResetAt.UTC()
		out.WeeklyResetAt = &t
	}
	return out
}

func vendorAccountUsageEqual(a, b routing.VendorAccountUsage) bool {
	na, nb := normalizeVendorAccountUsageForCompare(a), normalizeVendorAccountUsageForCompare(b)
	timePtrEq := func(x, y *time.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(*y)
	}
	return na.AccountID == nb.AccountID && na.FiveHourPct == nb.FiveHourPct && na.WeeklyPct == nb.WeeklyPct &&
		na.CreditBalance == nb.CreditBalance && na.UpdatedAt.Equal(nb.UpdatedAt) &&
		timePtrEq(na.FiveHourResetAt, nb.FiveHourResetAt) && timePtrEq(na.WeeklyResetAt, nb.WeeklyResetAt)
}

// TestRoutingStoreVendorAccountUsageUpsertRoundTrip pins the rate-limit
// usage-snapshot store on every driver (memory + sqlite + postgres): upsert on an
// unknown account is ErrNotFound; a read before any upsert is ok=false; a first
// upsert round-trips every field (including a SET five-hour reset, a NIL weekly
// reset, an UNKNOWN -1 weekly percent, and a credit string); a second upsert
// OVERWRITES the whole row (the previously-set reset can be cleared back to nil,
// the unknown filled in).
func TestRoutingStoreVendorAccountUsageUpsertRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	reset5h := now.Add(5 * time.Hour)
	resetWk := now.Add(7 * 24 * time.Hour)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_vu", "vu@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()

		// Upsert against a non-existent account is ErrNotFound (the FK on the SQL
		// drivers; the explicit existence check in memory).
		if err := s.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{AccountID: "nope", UpdatedAt: now}); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("upsert unknown account err = %v, want ErrNotFound", err)
		}

		if err := s.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: "va_u", OwnerUserID: "u_vu", Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription,
			Name: "sub", Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// No snapshot yet.
		if _, ok, err := s.VendorAccountUsageByID(ctx, "va_u"); err != nil || ok {
			t.Fatalf("usage before any upsert: ok = %v, err = %v, want ok=false", ok, err)
		}

		first := routing.VendorAccountUsage{
			AccountID: "va_u", FiveHourPct: 42.5, FiveHourResetAt: &reset5h,
			WeeklyPct: -1, WeeklyResetAt: nil, CreditBalance: "12.34", UpdatedAt: now,
		}
		if err := s.UpsertVendorAccountUsage(ctx, first); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		got, ok, err := s.VendorAccountUsageByID(ctx, "va_u")
		if err != nil || !ok {
			t.Fatalf("read after first upsert: ok = %v, err = %v", ok, err)
		}
		if !vendorAccountUsageEqual(got, first) {
			t.Fatalf("first round-trip mismatch:\n got  %+v (5h=%v wk=%v)\n want %+v (5h=%v wk=%v)",
				got, got.FiveHourResetAt, got.WeeklyResetAt, first, first.FiveHourResetAt, first.WeeklyResetAt)
		}

		// A second upsert OVERWRITES the whole row: the previously-set five-hour
		// reset is cleared to nil, the weekly fields are filled in, the credit changes.
		second := routing.VendorAccountUsage{
			AccountID: "va_u", FiveHourPct: 0, FiveHourResetAt: nil,
			WeeklyPct: 88.75, WeeklyResetAt: &resetWk, CreditBalance: "", UpdatedAt: now.Add(time.Minute),
		}
		if err := s.UpsertVendorAccountUsage(ctx, second); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		got2, ok, err := s.VendorAccountUsageByID(ctx, "va_u")
		if err != nil || !ok {
			t.Fatalf("read after second upsert: ok = %v, err = %v", ok, err)
		}
		if !vendorAccountUsageEqual(got2, second) {
			t.Fatalf("second round-trip mismatch:\n got  %+v (5h=%v wk=%v)\n want %+v (5h=%v wk=%v)",
				got2, got2.FiveHourResetAt, got2.WeeklyResetAt, second, second.FiveHourResetAt, second.WeeklyResetAt)
		}
	})
}

// TestRoutingStoreVendorAccountUsageMergeKeepsStoredFields pins, on every driver
// (memory + sqlite + postgres), the read -> routing.MergeVendorAccountUsage ->
// upsert sequence the usage writers run: the unknown sentinels (-1 / nil / "")
// must survive the persisted representation, so a partial snapshot merged over a
// stored one keeps the stored credit balance, reset and other window rather than
// blanking them, while the fields it does know are replaced.
func TestRoutingStoreVendorAccountUsageMergeKeepsStoredFields(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	resetWk := now.Add(7 * 24 * time.Hour)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_vm", "vm@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		if err := s.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: "va_m", OwnerUserID: "u_vm", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
			Name: "sub", Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// merge is the writers' sequence: read the stored snapshot, merge the
		// incoming one over it, upsert the result, read it back.
		merge := func(t *testing.T, incoming routing.VendorAccountUsage) routing.VendorAccountUsage {
			t.Helper()
			existing, found, err := s.VendorAccountUsageByID(ctx, "va_m")
			if err != nil {
				t.Fatalf("read existing: %v", err)
			}
			if found {
				incoming = routing.MergeVendorAccountUsage(existing, incoming)
			}
			if err := s.UpsertVendorAccountUsage(ctx, incoming); err != nil {
				t.Fatalf("upsert merged: %v", err)
			}
			got, ok, err := s.VendorAccountUsageByID(ctx, "va_m")
			if err != nil || !ok {
				t.Fatalf("read merged: ok = %v, err = %v", ok, err)
			}
			return got
		}

		// First write: only a weekly window and a credit balance are known.
		first := routing.VendorAccountUsage{
			AccountID: "va_m", FiveHourPct: -1, WeeklyPct: 30, WeeklyResetAt: &resetWk, CreditBalance: "12.34", UpdatedAt: now,
		}
		if got := merge(t, first); !vendorAccountUsageEqual(got, first) {
			t.Fatalf("first write (no stored row) mismatch:\n got  %+v\n want %+v", got, first)
		}

		// A partial snapshot (five-hour percent only) merged over it keeps the
		// stored weekly window, its reset and the credit balance.
		partial := routing.VendorAccountUsage{AccountID: "va_m", FiveHourPct: 55, WeeklyPct: -1, UpdatedAt: now.Add(time.Minute)}
		want := routing.VendorAccountUsage{
			AccountID: "va_m", FiveHourPct: 55, WeeklyPct: 30, WeeklyResetAt: &resetWk, CreditBalance: "12.34", UpdatedAt: now.Add(time.Minute),
		}
		if got := merge(t, partial); !vendorAccountUsageEqual(got, want) {
			t.Fatalf("partial merge mismatch:\n got  %+v (5h=%v wk=%v)\n want %+v (5h=%v wk=%v)",
				got, got.FiveHourResetAt, got.WeeklyResetAt, want, want.FiveHourResetAt, want.WeeklyResetAt)
		}
	})
}
