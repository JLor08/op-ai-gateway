// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// vendorAccountParityRows is how many vendor_accounts rows the column-parity
// fixture seeds. Three is the smallest count that lets the three-valued status
// column (active / disabled / needs_reconnect) carry a different value in every
// row, so a reader that returned the wrong ROW is caught on status as well as on
// every free-text column.
const vendorAccountParityRows = 3

// TestConformanceVendorAccountReadersAgreeOnEveryColumn is the vendor_accounts
// sibling of TestConformanceAIServerReadersAgreeOnEveryColumn: the column list
// is hand-maintained in three readers (VendorAccountByID, VendorAccounts,
// VendorAccountsByOwner) that all feed the single scanVendorAccount. An OMITTED
// column already fails loudly (Scan's fixed destination count); a REORDERED list
// does not -- two same-typed columns swapped in one reader silently return the
// wrong values from that reader.
//
// So every reader must return the EXACT same routing.VendorAccount value for the
// same row, and the ByID baseline must carry the value seeded into each field.
// vendor_accounts has no integer-boolean column, so no bit-pattern table is
// needed: every one of the eleven fields holds a distinct non-zero value per row
// (the seven text credential/identity columns are each different from every
// other, so a swapped pair is observable), and the two owners make
// VendorAccountsByOwner's filter observable too.
func TestConformanceVendorAccountReadersAgreeOnEveryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		owners := []string{"usr_vacols_a", "usr_vacols_b"}
		for _, id := range owners {
			if err := s.CreateUser(ctx, newTestUser(id, id+"@example.test", now)); err != nil {
				t.Fatalf("create owner %s: %v", id, err)
			}
		}

		vendors := []string{routing.VendorOpenAI, routing.VendorAnthropic, routing.VendorOpenAI}
		authTypes := []string{routing.VendorAuthAPIKey, routing.VendorAuthSubscription, routing.VendorAuthAPIKey}
		statuses := []string{
			routing.VendorAccountStatusActive, routing.VendorAccountStatusDisabled, routing.VendorAccountStatusNeedsReconnect,
		}
		// Rows 0 and 2 belong to owner A, row 1 to owner B.
		rowOwner := []string{owners[0], owners[1], owners[0]}

		want := make([]routing.VendorAccount, 0, vendorAccountParityRows)
		for i := 0; i < vendorAccountParityRows; i++ {
			idx := strconv.Itoa(i)
			want = append(want, routing.VendorAccount{
				ID: "va_cols_" + idx, OwnerUserID: rowOwner[i], Vendor: vendors[i], AuthType: authTypes[i],
				Name: "Column Parity " + idx, Status: statuses[i],
				APIKey: "enc:api-key-" + idx, OAuthTokens: "enc:oauth-tokens-" + idx,
				ModelPrefix: "prefix-" + idx + "/",
				CreatedAt:   now.Add(time.Duration(-10-i) * time.Minute),
				UpdatedAt:   now.Add(time.Duration(-3-i) * time.Minute),
			})
		}
		for _, acc := range want {
			if err := s.CreateVendorAccount(ctx, acc); err != nil {
				t.Fatalf("create %s: %v", acc.ID, err)
			}
		}

		list, err := s.VendorAccounts(ctx)
		if err != nil {
			t.Fatalf("VendorAccounts: %v", err)
		}
		byOwnerA, err := s.VendorAccountsByOwner(ctx, owners[0])
		if err != nil {
			t.Fatalf("VendorAccountsByOwner(A): %v", err)
		}
		byOwnerB, err := s.VendorAccountsByOwner(ctx, owners[1])
		if err != nil {
			t.Fatalf("VendorAccountsByOwner(B): %v", err)
		}
		if len(list) != vendorAccountParityRows {
			t.Fatalf("VendorAccounts returned %d accounts, want %d", len(list), vendorAccountParityRows)
		}
		if len(byOwnerA) != 2 || len(byOwnerB) != 1 {
			t.Fatalf("VendorAccountsByOwner returned %d (A) / %d (B) accounts, want 2 / 1", len(byOwnerA), len(byOwnerB))
		}
		readers := []struct {
			name string
			got  []routing.VendorAccount
		}{
			{"VendorAccounts", list},
			{"VendorAccountsByOwner", append(append([]routing.VendorAccount{}, byOwnerA...), byOwnerB...)},
		}

		for _, expected := range want {
			byID, err := s.VendorAccountByID(ctx, expected.ID)
			if err != nil {
				t.Fatalf("VendorAccountByID(%s): %v", expected.ID, err)
			}
			// The baseline the other readers are compared against is itself
			// checked field-by-field against the seeded value, so a column
			// missing from ALL readers cannot hide behind them agreeing on the
			// same zero.
			if !reflect.DeepEqual(normalizeVendorAccountForCompare(byID), normalizeVendorAccountForCompare(expected)) {
				t.Fatalf("VendorAccountByID(%s) lost or reordered a column:\n got  %+v\n want %+v", expected.ID, byID, expected)
			}
			for _, reader := range readers {
				got, ok := findVendorAccountByID(reader.got, expected.ID)
				if !ok {
					t.Fatalf("%s did not return %s at all", reader.name, expected.ID)
				}
				if !reflect.DeepEqual(normalizeVendorAccountForCompare(got), normalizeVendorAccountForCompare(byID)) {
					t.Fatalf("%s disagrees with VendorAccountByID on at least one column of %s:\n got  %+v\n want %+v",
						reader.name, expected.ID, got, byID)
				}
			}
		}
	})
}

// findVendorAccountByID returns the account with the given id from a list
// reader's result. Matching by id rather than by position keeps the comparison
// independent of each reader's ORDER BY.
func findVendorAccountByID(accounts []routing.VendorAccount, id string) (routing.VendorAccount, bool) {
	for _, acc := range accounts {
		if acc.ID == id {
			return acc, true
		}
	}
	return routing.VendorAccount{}, false
}

// TestVendorAccountsSchemaColumnsAllCovered ties the vendor_accounts parity
// fixture to the LIVE migrated schema (the #84 guard): a column added to
// vendor_accounts and wired into scanVendorAccount but not into the fixture
// above would read back its zero from every reader and stay green. The table has
// no integer-boolean column, so every column is seeded.
func TestVendorAccountsSchemaColumnsAllCovered(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		assertColumnCoverage(context.Background(), t, s, "vendor_accounts", columnCoverage{
			bools: nil,
			seeded: []string{
				"api_key", "auth_type", "created_at", "id", "model_prefix", "name", "oauth_tokens",
				"owner_user_id", "status", "updated_at", "vendor",
			},
			ignored: map[string]string{},
		})
	})
}
