// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"slices"
	"sort"
	"strings"
	"testing"
)

const groupListingPrefix = "pool/"

// groupListingEnv is the grant fixture plus one group "pool/" owned by grantOwner
// over two of the owner's own accounts (grantPrivate, grantFull), both OpenAI api_key
// accounts serving dup-model and one exclusive model each.
func groupListingEnv(t *testing.T) *vendorGrantEnv {
	t.Helper()
	e := newVendorListingEnv(t)
	e.group("vag_pool", groupListingPrefix, grantPrivate, grantFull)
	return e
}

func (e *vendorGrantEnv) group(id, prefix string, members ...string) {
	e.t.Helper()
	if err := e.routes.CreateVendorAccountGroup(e.ctx, routing.VendorAccountGroup{
		ID: id, OwnerUserID: grantOwner, Vendor: routing.VendorOpenAI, Name: "Pool", ModelPrefix: prefix,
		Status:               routing.VendorAccountGroupStatusActive,
		ThresholdFiveHourPct: 99, ThresholdWeeklyPct: 99, ThresholdCreditsPct: 99,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("CreateVendorAccountGroup: %v", err)
	}
	if err := e.routes.SetVendorAccountGroupMembers(e.ctx, id, members); err != nil {
		e.t.Fatalf("SetVendorAccountGroupMembers: %v", err)
	}
}

func groupOwnerToken(includeGroups bool) auth.Token {
	return auth.Token{
		ID: "tok_grp", UserID: grantOwner, Active: true,
		VendorAccess: auth.VendorAccess{All: true, IncludeGroups: includeGroups},
	}
}

// listedGroupModels returns the group-prefixed model ids /v1/models-style listing
// (modelsResponse) advertises, with their flavors, and fails on a duplicate id.
func (e *vendorGrantEnv) listedGroupModels(token auth.Token) map[string][]string {
	e.t.Helper()
	out := map[string][]string{}
	for _, m := range e.svc.modelsResponse(e.ctx, token, true).Data {
		if !strings.HasPrefix(m.ID, groupListingPrefix) {
			continue
		}
		if _, dup := out[m.ID]; dup {
			e.t.Errorf("model %s listed more than once", m.ID)
		}
		f := slices.Clone(m.Flavors)
		sort.Strings(f)
		out[m.ID] = f
	}
	return out
}

func TestVendorListingAdvertisesGroupModels(t *testing.T) {
	e := groupListingEnv(t)
	got := e.listedGroupModels(groupOwnerToken(true))
	both := []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}
	want := map[string][]string{
		groupListingPrefix + grantDupModel:            both,
		groupListingPrefix + grantModel(grantPrivate): both,
		groupListingPrefix + grantModel(grantFull):    both,
	}
	if len(got) != len(want) {
		t.Fatalf("listed group models = %v, want %v", got, want)
	}
	for id, flavors := range want {
		if !slices.Equal(got[id], flavors) {
			t.Errorf("%s flavors = %v, want %v", id, got[id], flavors)
		}
	}
}

func TestVendorListingSkipsGroupWithoutTokenOptIn(t *testing.T) {
	e := groupListingEnv(t)
	if got := e.listedGroupModels(groupOwnerToken(false)); len(got) != 0 {
		t.Errorf("token without include_groups lists group models: %v", got)
	}
}

func TestVendorListingGroupSkipsUnusableMembers(t *testing.T) {
	e := groupListingEnv(t)
	acc, err := e.routes.VendorAccountByID(e.ctx, grantPrivate)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	acc.Status = routing.VendorAccountStatusDisabled
	if err := e.routes.UpdateVendorAccount(e.ctx, acc); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}
	got := e.listedGroupModels(groupOwnerToken(true))
	if _, ok := got[groupListingPrefix+grantModel(grantPrivate)]; ok {
		t.Errorf("disabled member's exclusive model is listed under the group: %v", got)
	}
	if _, ok := got[groupListingPrefix+grantModel(grantFull)]; !ok {
		t.Errorf("active member's model is missing: %v", got)
	}
}

func TestVendorListingDashboardLabelsGroupRows(t *testing.T) {
	e := groupListingEnv(t)
	var found bool
	for _, r := range e.svc.vendorDashboardRoutes(e.ctx, groupOwnerToken(true)) {
		if r.Model == groupListingPrefix+grantModel(grantFull) {
			found = true
			if r.Host != "Pool · "+grantFull {
				t.Errorf("group row host = %q, want %q", r.Host, "Pool · "+grantFull)
			}
		}
	}
	if !found {
		t.Error("no dashboard row for the group model")
	}
}

// Every listed group name resolves through a real resolver for the same token on
// its dialect, to a group target; and a group name the listing omits does not.
func TestVendorListingGroupParityWithResolver(t *testing.T) {
	e := groupListingEnv(t)
	resolver := e.liveResolver(e.svc)
	for _, includeGroups := range []bool{true, false} {
		token := groupOwnerToken(includeGroups)
		listed := e.listedGroupModels(token)
		for _, name := range []string{
			groupListingPrefix + grantDupModel,
			groupListingPrefix + grantModel(grantPrivate),
			groupListingPrefix + grantModel(grantFull),
			groupListingPrefix + "no-such-model",
		} {
			for _, d := range inboundDialects {
				target, err := resolver.Resolve(e.ctx, token, inference.Request{Model: name, APIFlavor: d.request})
				routable := err == nil && target.VendorGroupID != ""
				advertised := slices.Contains(listed[name], d.coarse)
				if routable != advertised {
					t.Errorf("includeGroups=%v %s on %s: routable=%v advertised=%v", includeGroups, name, d.coarse, routable, advertised)
				}
			}
		}
	}
}
