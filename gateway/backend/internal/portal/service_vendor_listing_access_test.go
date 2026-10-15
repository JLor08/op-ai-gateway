// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"reflect"
	"sort"
	"testing"
)

// TestVendorSetLoadedDistinguishesNoAccessFromAReadError (issue #227): the
// loaded flag of ownVendorAccountModels and vendorModelFlavorSets says whether
// the vendor set is COMPLETE, not whether it is non-empty. No vendor access (flag
// off, no routing store, a principal with no user id, no accounts) is a
// legitimately empty set, loaded; a failed accounts read or a failed per-account
// model read is not. A partial read still returns what it collected, so the
// listing overlay can fold it in (fail-open), but it is flagged incomplete so the
// alias derivation can refuse it (fail-closed).
func TestVendorSetLoadedDistinguishesNoAccessFromAReadError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("vendor read boom")
	cases := []struct {
		name      string
		setup     func(t *testing.T, fx *tokenSettingsFixture) auth.Token
		wantNames []string
		wantLoad  bool
	}{
		{"complete read", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			return fx.owner
		}, []string{"a/gpt-4o", "b/gpt-4o"}, true},
		{"flag off", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			setVendorAccountsEnabled(t, fx.svc, false)
			return fx.owner
		}, nil, true},
		{"no routing store", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			fx.svc.routes = nil
			return fx.owner
		}, nil, true},
		{"service principal", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			token := fx.owner
			token.UserID = ""
			return token
		}, nil, true},
		{"no accounts", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			token := fx.owner
			token.UserID = "usr_without_accounts"
			return token
		}, nil, true},
		{"token may use no account", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			token := fx.owner
			token.VendorAccess = auth.VendorAccess{}
			return token
		}, nil, true},
		{"accounts read error", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			fx.svc.routes = &failingVendorReadsStore{MemoryStore: fx.rs, ownerErr: boom}
			return fx.owner
		}, nil, false},
		{"one account's models unreadable", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			fx.svc.routes = &failingVendorAccountModelsStore{MemoryStore: fx.rs, failFor: map[string]bool{"acc_b": true}, err: boom}
			return fx.owner
		}, []string{"a/gpt-4o"}, false},
		{"every account's models unreadable", func(t *testing.T, fx *tokenSettingsFixture) auth.Token {
			fx.svc.routes = &failingVendorReadsStore{MemoryStore: fx.rs, modelsErr: boom}
			return fx.owner
		}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTokenVendorAccessFixture(t) // acc_a ("a/") + acc_b ("b/") owned by usr_1
			setVendorAccountsEnabled(t, fx.svc, true)
			fx.owner.VendorAccess = auth.VendorAccess{All: true}
			token := tc.setup(t, fx)

			owned, loaded := fx.svc.ownVendorAccountModels(ctx, token)
			if loaded != tc.wantLoad {
				t.Errorf("ownVendorAccountModels loaded = %v, want %v", loaded, tc.wantLoad)
			}
			var ownedNames []string
			for _, am := range owned {
				for _, m := range am.Models {
					ownedNames = append(ownedNames, m.GatewayModel)
				}
			}
			sort.Strings(ownedNames)
			if !reflect.DeepEqual(ownedNames, tc.wantNames) {
				t.Errorf("ownVendorAccountModels models = %v, want %v", ownedNames, tc.wantNames)
			}

			sets, loaded := fx.svc.vendorModelFlavorSets(ctx, token)
			if loaded != tc.wantLoad {
				t.Errorf("vendorModelFlavorSets loaded = %v, want %v", loaded, tc.wantLoad)
			}
			var setNames []string
			for name := range sets {
				setNames = append(setNames, name)
			}
			sort.Strings(setNames)
			if !reflect.DeepEqual(setNames, tc.wantNames) {
				t.Errorf("vendorModelFlavorSets names = %v, want %v", setNames, tc.wantNames)
			}
		})
	}
}

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
		sets, loaded := fx.svc.vendorModelFlavorSets(ctx, token)
		if !loaded {
			t.Fatal("vendorModelFlavorSets reports an incomplete set on a healthy store")
		}
		for name := range sets {
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
