// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"reflect"
	"sort"
	"testing"
)

// TestVendorListingHonorsTokenAccess pins the listing half of per-token vendor
// access: ownVendorAccountModels (the single choke point every listing reads)
// keeps only the accounts the token may use and names their models under the
// token-effective prefix.
func TestVendorListingHonorsTokenAccess(t *testing.T) {
	fx := newTokenVendorAccessFixture(t) // acc_a ("a/") + acc_b ("b/") owned by usr_1, each serving gpt-4o
	setVendorAccountsEnabled(t, fx.svc, true)
	ctx := context.Background()

	names := func(access auth.VendorAccess) []string {
		token := fx.owner
		token.VendorAccess = access
		out := []string{}
		for name := range fx.svc.vendorModelFlavorSets(ctx, token) {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	if got := names(auth.VendorAccess{}); len(got) != 0 {
		t.Fatalf("strict default should list nothing, got %v", got)
	}
	if got := names(auth.VendorAccess{All: true}); !reflect.DeepEqual(got, []string{"a/gpt-4o", "b/gpt-4o"}) {
		t.Fatalf("all: got %v, want [a/gpt-4o b/gpt-4o]", got)
	}
	explicit := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: ""}}}
	if got := names(explicit); !reflect.DeepEqual(got, []string{"gpt-4o"}) {
		t.Fatalf("explicit acc_a empty-override: got %v, want [gpt-4o]", got)
	}
	nativeOnly := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_b"}}}
	if got := names(nativeOnly); !reflect.DeepEqual(got, []string{"b/gpt-4o"}) {
		t.Fatalf("listed acc_b without override: got %v, want [b/gpt-4o]", got)
	}
	overridden := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: "team/"}}}
	if got := names(overridden); !reflect.DeepEqual(got, []string{"team/gpt-4o"}) {
		t.Fatalf("acc_a override team/: got %v, want [team/gpt-4o]", got)
	}

	// The dashboard route table reads the same choke point: its rows carry the
	// token-effective name too.
	token := fx.owner
	token.VendorAccess = overridden
	routes := fx.svc.vendorDashboardRoutes(ctx, token)
	if len(routes) != 1 || routes[0].Model != "team/gpt-4o" || routes[0].ID != "acc_a:team/gpt-4o" {
		t.Fatalf("dashboard routes = %+v, want one row team/gpt-4o on acc_a", routes)
	}
}
