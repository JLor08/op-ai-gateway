// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/auth"
	"reflect"
	"testing"
	"time"
)

// TestTokenVendorProviderAccessPersists proves api_tokens.vendor_provider_access
// survives CreatePlainToken, surfaces on the auth.Token LookupBearer builds
// (decoded), and is rewritten by UpdateTokenMetadata (it is user-editable).
func TestTokenVendorProviderAccessPersists(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	rec := testTokenRecord(now)
	rec.VendorProviderAccess = `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":""}}]}`
	if err := st.CreatePlainToken(ctx, rec, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	tok, ok := st.LookupBearer("Bearer plain-secret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	want := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}
	if !reflect.DeepEqual(tok.VendorAccess, want) {
		t.Fatalf("VendorAccess = %#v, want %#v", tok.VendorAccess, want)
	}

	rec.VendorProviderAccess = `{"all":true}`
	rec.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata returned %v", err)
	}
	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.VendorProviderAccess != `{"all":true}` {
		t.Fatalf("VendorProviderAccess = %q, want {\"all\":true}", got.VendorProviderAccess)
	}
	tok, ok = st.LookupBearer("Bearer plain-secret")
	if !ok {
		t.Fatalf("LookupBearer after update returned ok=false")
	}
	if !tok.VendorAccess.All {
		t.Fatalf("VendorAccess after update = %#v, want All=true", tok.VendorAccess)
	}
}

// TestTokenVendorProviderAccessDefaultsStrict proves a token created
// without touching the field reads back the strict default (empty column, no
// vendor access) through both read paths.
func TestTokenVendorProviderAccessDefaultsStrict(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	rec := testTokenRecord(now)
	if err := st.CreatePlainToken(ctx, rec, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}
	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.VendorProviderAccess != "" {
		t.Fatalf("VendorProviderAccess = %q, want empty (strict default)", got.VendorProviderAccess)
	}
	tok, ok := st.LookupBearer("Bearer plain-secret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if tok.VendorAccess.All || len(tok.VendorAccess.Accounts) != 0 {
		t.Fatalf("VendorAccess = %#v, want the strict default", tok.VendorAccess)
	}
}
