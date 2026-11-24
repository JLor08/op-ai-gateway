// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
)

// tokenGroupFixture is the token vendor-access fixture (usr_1 owns acc_a "a/" and
// acc_b "b/", usr_2 owns acc_other) plus acc_c and acc_d, two more usr_1 accounts
// with an EMPTY prefix serving gpt-4o, and groups seeded straight through the store
// (so group save's own conflict check cannot get in the way of the backstop under
// test): vag_c "" over acc_c, vag_a "g/" over acc_a, vag_d "" over acc_d, and
// vag_other owned by usr_2.
func newTokenGroupFixture(t *testing.T) *tokenSettingsFixture {
	t.Helper()
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()
	for _, id := range []string{"acc_c", "acc_d"} {
		if err := fx.rs.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: id, OwnerUserID: "usr_1", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
			Name: id, Status: routing.VendorAccountStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime,
		}); err != nil {
			t.Fatalf("CreateVendorAccount %s: %v", id, err)
		}
		if err := fx.rs.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
			{GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI},
		}); err != nil {
			t.Fatalf("SetVendorAccountModels %s: %v", id, err)
		}
	}
	group := func(id, owner, prefix string, members ...string) {
		t.Helper()
		if err := fx.rs.CreateVendorAccountGroup(ctx, routing.VendorAccountGroup{
			ID: id, OwnerUserID: owner, Vendor: routing.VendorOpenAI, Name: id, ModelPrefix: prefix,
			Status:               routing.VendorAccountGroupStatusActive,
			ThresholdFiveHourPct: 99, ThresholdWeeklyPct: 99, ThresholdCreditsPct: 99,
			CreatedAt: offeringTime, UpdatedAt: offeringTime,
		}); err != nil {
			t.Fatalf("CreateVendorAccountGroup %s: %v", id, err)
		}
		if err := fx.rs.SetVendorAccountGroupMembers(ctx, id, members); err != nil {
			t.Fatalf("SetVendorAccountGroupMembers %s: %v", id, err)
		}
	}
	group("vag_c", "usr_1", "", "acc_c")
	group("vag_a", "usr_1", "g/", "acc_a")
	group("vag_d", "usr_1", "", "acc_d")
	group("vag_other", "usr_2", "o/", "acc_other")
	return fx
}

func groupEntries(ids ...string) []VendorAccessGroupDTO {
	out := make([]VendorAccessGroupDTO, len(ids))
	for i, id := range ids {
		out[i] = VendorAccessGroupDTO{GroupID: id}
	}
	return out
}

func accountEntries(ids ...string) []VendorAccessEntryDTO {
	out := make([]VendorAccessEntryDTO, len(ids))
	for i, id := range ids {
		out[i] = VendorAccessEntryDTO{AccountID: id}
	}
	return out
}

func TestTokenVendorGroupEntryRoundTrips(t *testing.T) {
	fx := newTokenGroupFixture(t)
	ctx := context.Background()
	want := &VendorAccessDTO{
		Accounts: []VendorAccessEntryDTO{},
		Groups: []VendorAccessGroupDTO{
			{GroupID: "vag_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "mine/"}},
			{GroupID: "vag_c"},
		},
	}
	resp, err := fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{
		Name: "grp", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Groups: []VendorAccessGroupDTO{
			{GroupID: " vag_a ", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: " mine/ "}},
			{GroupID: "vag_c", PrefixOverride: &PrefixOverrideDTO{Enabled: false, Value: "ignored"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !reflect.DeepEqual(resp.Token.VendorAccess, want) {
		t.Fatalf("create DTO = %+v, want %+v", resp.Token.VendorAccess, want)
	}
	list, err := fx.svc.ListTokens(ctx, fx.owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, tok := range list.Data {
		if tok.ID == resp.Token.ID && !reflect.DeepEqual(tok.VendorAccess, want) {
			t.Fatalf("listed DTO = %+v, want %+v", tok.VendorAccess, want)
		}
	}
	// all + include_groups round-trips as well (without the clashing vag_d / acc_d).
	for _, f := range []func() error{
		func() error { return fx.rs.DeleteVendorAccountGroup(ctx, "vag_d") },
		func() error { return fx.rs.DeleteVendorAccount(ctx, "acc_d") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	resp, err = fx.svc.CreateToken(ctx, fx.owner, CreateTokenRequest{
		Name: "all", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{All: true, IncludeGroups: true, Accounts: []VendorAccessEntryDTO{}},
	})
	if err != nil {
		t.Fatalf("CreateToken all+groups: %v", err)
	}
	if va := resp.Token.VendorAccess; va == nil || !va.All || !va.IncludeGroups {
		t.Fatalf("all+include_groups DTO = %+v", va)
	}
}

func TestTokenVendorGroupEntryValidation(t *testing.T) {
	fx := newTokenGroupFixture(t)
	ctx := context.Background()
	owner := userTokenPrincipal(fx.owner)
	cases := []struct {
		name string
		va   *VendorAccessDTO
		want error
	}{
		{"owned group", &VendorAccessDTO{Groups: groupEntries("vag_a")}, nil},
		{"foreign group", &VendorAccessDTO{Groups: groupEntries("vag_other")}, ErrTokenVendorAccessInvalid},
		{"unknown group", &VendorAccessDTO{Groups: groupEntries("vag_gone")}, ErrTokenVendorAccessInvalid},
		{"blank group id", &VendorAccessDTO{Groups: groupEntries(" ")}, ErrTokenVendorAccessInvalid},
		{"repeated group", &VendorAccessDTO{Groups: groupEntries("vag_a", "vag_a")}, ErrTokenVendorAccessInvalid},
		{"bad override prefix", &VendorAccessDTO{Groups: []VendorAccessGroupDTO{{GroupID: "vag_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "a b"}}}}, ErrTokenVendorAccessInvalid},
		{"foreign group under all+include_groups", &VendorAccessDTO{All: true, IncludeGroups: true, Groups: groupEntries("vag_other")}, ErrTokenVendorAccessInvalid},
		{"group and an unrelated account on one name", &VendorAccessDTO{Accounts: accountEntries("acc_d"), Groups: groupEntries("vag_c")}, ErrTokenVendorAccessConflict},
		{"group and its own member on one name", &VendorAccessDTO{Accounts: accountEntries("acc_c"), Groups: groupEntries("vag_c")}, nil},
		{"two groups on one name", &VendorAccessDTO{Groups: groupEntries("vag_c", "vag_d")}, ErrTokenVendorAccessConflict},
		{"two groups, one overridden away", &VendorAccessDTO{Groups: []VendorAccessGroupDTO{
			{GroupID: "vag_c"}, {GroupID: "vag_d", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "d/"}},
		}}, nil},
		{"all+include_groups over the whole fixture clashes", &VendorAccessDTO{All: true, IncludeGroups: true, Accounts: []VendorAccessEntryDTO{}}, ErrTokenVendorAccessConflict},
		{"old policy without groups", &VendorAccessDTO{Accounts: accountEntries("acc_a", "acc_b")}, nil},
		{"all without include_groups ignores group clashes", &VendorAccessDTO{All: true}, nil},
		{"include_groups without all is ignored", &VendorAccessDTO{IncludeGroups: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fx.svc.validateVendorAccess(ctx, owner, tc.va)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("validateVendorAccess = %v, want %v", err, tc.want)
			}
		})
	}
}

// vag_c "" and acc_c "" are group and member: fine. The same fixture's acc_d "" is a
// non-member own account of the same names, and vag_d "" a second group, so with the
// whole fixture `all + include_groups` clashes; with only the compatible pair it must
// pass.
func TestTokenVendorAllIncludeGroupsClaimsOwnGroups(t *testing.T) {
	ctx := context.Background()
	fx := newTokenGroupFixture(t)
	owner := userTokenPrincipal(fx.owner)
	all := &VendorAccessDTO{All: true, IncludeGroups: true}
	// Remove the competing sources: vag_d and acc_d (and the other own accounts with
	// prefixes never collide). What is left: group vag_c "" over its own member acc_c.
	if err := fx.rs.DeleteVendorAccountGroup(ctx, "vag_d"); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.validateVendorAccess(ctx, owner, all); !errors.Is(err, ErrTokenVendorAccessConflict) {
		t.Fatalf("group vs the non-member own account acc_d = %v, want a conflict (the backstop)", err)
	}
	if err := fx.rs.DeleteVendorAccount(ctx, "acc_d"); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.validateVendorAccess(ctx, owner, all); err != nil {
		t.Fatalf("group over its own member only = %v, want accepted", err)
	}
}

func TestTokenVendorGroupsNeverForServices(t *testing.T) {
	fx := newTokenGroupFixture(t)
	ctx := context.Background()
	svc := serviceTokenPrincipal("svc_1")
	for name, va := range map[string]*VendorAccessDTO{
		"group entry":              {Groups: groupEntries("vag_a")},
		"include_groups":           {All: true, IncludeGroups: true},
		"include_groups, no all":   {IncludeGroups: true},
		"group entry under all":    {All: true, Groups: groupEntries("vag_a")},
		"foreign group for a svc":  {Groups: groupEntries("vag_other")},
		"group entry with account": {Accounts: accountEntries("acc_a"), Groups: groupEntries("vag_a")},
	} {
		if err := fx.svc.validateVendorAccess(ctx, svc, va); !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Errorf("%s: validateVendorAccess = %v, want invalid", name, err)
		}
	}
	// A service token without groups is untouched.
	if err := fx.svc.validateVendorAccess(ctx, svc, &VendorAccessDTO{All: true}); err != nil {
		t.Errorf("service all:true = %v, want nil", err)
	}
}
