// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"reflect"
	"testing"
	"time"
)

// TestRoutingStoreVendorAccountModels pins the vendor_account_models writer and
// reader on every driver: a set round-trips sorted by gateway_model, a second
// set REPLACES the first wholesale, one account's rows never bleed into
// another's, an unknown account is ErrNotFound (even for an empty set), and a
// duplicate gateway_model within one set is ErrConflict that leaves the
// previous set untouched.
func TestRoutingStoreVendorAccountModels(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_vam", "vam@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for _, id := range []string{"va_models", "va_models_other"} {
			if err := s.CreateVendorAccount(ctx, routing.VendorAccount{
				ID: id, OwnerUserID: "u_vam", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
				Name: id, Status: routing.VendorAccountStatusActive, APIKey: "enc:k", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}

		// No rows yet: an empty, non-nil slice (it serializes as [], never null).
		got, err := s.VendorAccountModels(ctx, "va_models")
		if err != nil {
			t.Fatalf("read before any write: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("models before any write = %#v, want a non-nil empty slice", got)
		}

		// Written out of order; read back sorted by gateway_model.
		first := []routing.VendorAccountModel{
			{AccountID: "va_models", GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o-2024-08-06", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-4o"},
			{AccountID: "va_models", GatewayModel: "gpt-4.1", UpstreamModel: "gpt-4.1", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-4.1"},
			{AccountID: "va_models", GatewayModel: "o3", UpstreamModel: "o3", APIFlavor: routing.APIFlavorOpenAI},
		}
		if err := s.SetVendorAccountModels(ctx, "va_models", first); err != nil {
			t.Fatalf("set first: %v", err)
		}
		want := []routing.VendorAccountModel{first[1], first[0], first[2]}
		got, err = s.VendorAccountModels(ctx, "va_models")
		if err != nil {
			t.Fatalf("read first: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
		}

		// The reader hands back a copy: scribbling on it must not alter the store.
		got[0].UpstreamModel = "scribbled"
		if again, _ := s.VendorAccountModels(ctx, "va_models"); !reflect.DeepEqual(again, want) {
			t.Fatalf("reader aliased stored state: %+v", again)
		}

		// The caller's AccountID is authoritative-by-argument: a row carrying a
		// stale or empty AccountID is stored under the account it was set for.
		other := []routing.VendorAccountModel{{GatewayModel: "claude-opus-4", UpstreamModel: "claude-opus-4", APIFlavor: routing.APIFlavorAnthropic, DisplayName: "Claude Opus 4"}}
		if err := s.SetVendorAccountModels(ctx, "va_models_other", other); err != nil {
			t.Fatalf("set other: %v", err)
		}

		// Replace-all: the second set overwrites the first entirely.
		second := []routing.VendorAccountModel{
			{AccountID: "va_models", GatewayModel: "gpt-5", UpstreamModel: "gpt-5", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-5"},
		}
		if err := s.SetVendorAccountModels(ctx, "va_models", second); err != nil {
			t.Fatalf("set second: %v", err)
		}
		got, _ = s.VendorAccountModels(ctx, "va_models")
		if !reflect.DeepEqual(got, second) {
			t.Fatalf("replace-all mismatch:\n got  %+v\n want %+v", got, second)
		}

		// ...and did not touch the other account's rows.
		gotOther, _ := s.VendorAccountModels(ctx, "va_models_other")
		wantOther := []routing.VendorAccountModel{{AccountID: "va_models_other", GatewayModel: "claude-opus-4", UpstreamModel: "claude-opus-4", APIFlavor: routing.APIFlavorAnthropic, DisplayName: "Claude Opus 4"}}
		if !reflect.DeepEqual(gotOther, wantOther) {
			t.Fatalf("other account's models changed:\n got  %+v\n want %+v", gotOther, wantOther)
		}

		// A duplicate gateway_model hits the (account_id, gateway_model) key:
		// ErrConflict, and the previous set survives (the delete rolls back).
		dup := []routing.VendorAccountModel{
			{GatewayModel: "dup", UpstreamModel: "a", APIFlavor: routing.APIFlavorOpenAI},
			{GatewayModel: "dup", UpstreamModel: "b", APIFlavor: routing.APIFlavorOpenAI},
		}
		if err := s.SetVendorAccountModels(ctx, "va_models", dup); !errors.Is(err, storeerr.ErrConflict) {
			t.Fatalf("duplicate gateway_model err = %v, want ErrConflict", err)
		}
		if got, _ = s.VendorAccountModels(ctx, "va_models"); !reflect.DeepEqual(got, second) {
			t.Fatalf("a rejected set altered the stored one: %+v", got)
		}

		// An empty set clears the account's rows.
		if err := s.SetVendorAccountModels(ctx, "va_models", nil); err != nil {
			t.Fatalf("set empty: %v", err)
		}
		if got, _ = s.VendorAccountModels(ctx, "va_models"); got == nil || len(got) != 0 {
			t.Fatalf("models after an empty set = %#v, want a non-nil empty slice", got)
		}

		// An unknown account is ErrNotFound, even with an empty set.
		for _, models := range [][]routing.VendorAccountModel{nil, first} {
			if err := s.SetVendorAccountModels(ctx, "va_nope", models); !errors.Is(err, storeerr.ErrNotFound) {
				t.Fatalf("unknown account (%d rows) err = %v, want ErrNotFound", len(models), err)
			}
		}
	})
}

// TestVendorAccountModelsSchemaColumnsAllCovered ties the vendor_account_models
// round-trip fixture above to the LIVE migrated schema (the #84 guard): a column
// added to the table but not seeded with a distinct value in
// TestRoutingStoreVendorAccountModels would read back its zero from every reader
// and stay green. The table has no integer-boolean column, so every column is
// seeded.
func TestVendorAccountModelsSchemaColumnsAllCovered(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		assertColumnCoverage(context.Background(), t, s, "vendor_account_models", columnCoverage{
			bools: nil,
			seeded: []string{
				"account_id", "api_flavor", "display_name", "gateway_model", "upstream_model",
			},
			ignored: map[string]string{},
		})
	})
}
