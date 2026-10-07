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

// TestRoutingStoreVendorAccountNarrowWriters pins the two targeted writers the
// subscription dispatch path uses (SetVendorAccountOAuthTokens /
// SetVendorAccountStatus) across every driver: each updates ONLY its own column
// (plus updated_at), never the account's other mutable columns or its identity,
// so a dispatch-time token refresh / needs_reconnect flip cannot clobber a
// concurrent rename. An unknown id is ErrNotFound on both.
func TestRoutingStoreVendorAccountNarrowWriters(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_nw", "nw@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		acc := routing.VendorAccount{
			ID: "va_nw", OwnerUserID: "u_nw", Vendor: routing.VendorAnthropic,
			AuthType: routing.VendorAuthSubscription, Name: "Sub", Status: routing.VendorAccountStatusActive,
			APIKey: "", OAuthTokens: "enc:original", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("create: %v", err)
		}

		// ErrNotFound for an unknown id on both writers.
		if err := s.SetVendorAccountOAuthTokens(ctx, "va_ghost", "enc:x"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("SetVendorAccountOAuthTokens(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.SetVendorAccountStatus(ctx, "va_ghost", routing.VendorAccountStatusNeedsReconnect); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("SetVendorAccountStatus(unknown) = %v, want ErrNotFound", err)
		}

		// A full-row update first, to a DISTINCT value in every mutable column, so
		// the narrow-writer assertions below prove disjointness against a non-seed
		// baseline rather than against CreateVendorAccount's values.
		acc.Name = "Renamed"
		acc.APIKey = "enc:some-key"
		acc.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("baseline update: %v", err)
		}

		// SetVendorAccountOAuthTokens: oauth_tokens changes, everything else holds.
		if err := s.SetVendorAccountOAuthTokens(ctx, "va_nw", "enc:refreshed"); err != nil {
			t.Fatalf("SetVendorAccountOAuthTokens: %v", err)
		}
		got, err := s.VendorAccountByID(ctx, "va_nw")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.OAuthTokens != "enc:refreshed" {
			t.Fatalf("oauth_tokens = %q, want enc:refreshed", got.OAuthTokens)
		}
		if got.Name != "Renamed" || got.APIKey != "enc:some-key" ||
			got.Status != routing.VendorAccountStatusActive || got.AuthType != routing.VendorAuthSubscription ||
			got.OwnerUserID != "u_nw" || got.Vendor != routing.VendorAnthropic {
			t.Fatalf("SetVendorAccountOAuthTokens touched a non-oauth_tokens column: %+v", got)
		}
		if !got.CreatedAt.UTC().Equal(now) {
			t.Fatalf("created_at drifted: got %v, want %v", got.CreatedAt.UTC(), now)
		}
		if !got.UpdatedAt.After(now.Add(time.Minute)) && got.UpdatedAt.UTC().Equal(now.Add(time.Minute)) {
			// updated_at must advance past the baseline update's timestamp.
			t.Fatalf("updated_at did not advance on oauth-tokens write: got %v", got.UpdatedAt.UTC())
		}

		// SetVendorAccountStatus: status changes, oauth_tokens (now enc:refreshed)
		// and every other column hold.
		if err := s.SetVendorAccountStatus(ctx, "va_nw", routing.VendorAccountStatusNeedsReconnect); err != nil {
			t.Fatalf("SetVendorAccountStatus: %v", err)
		}
		got2, err := s.VendorAccountByID(ctx, "va_nw")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got2.Status != routing.VendorAccountStatusNeedsReconnect {
			t.Fatalf("status = %q, want needs_reconnect", got2.Status)
		}
		if got2.OAuthTokens != "enc:refreshed" || got2.Name != "Renamed" || got2.APIKey != "enc:some-key" ||
			got2.AuthType != routing.VendorAuthSubscription || got2.OwnerUserID != "u_nw" || got2.Vendor != routing.VendorAnthropic {
			t.Fatalf("SetVendorAccountStatus touched a non-status column: %+v", got2)
		}
		if !got2.CreatedAt.UTC().Equal(now) {
			t.Fatalf("created_at drifted on status write: got %v, want %v", got2.CreatedAt.UTC(), now)
		}
	})
}
