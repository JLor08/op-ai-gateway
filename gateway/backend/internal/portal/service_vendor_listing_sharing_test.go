// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file pins the LISTING side of shared vendor accounts (#183, spec 2026-10-15
// sections 8 and 10): the model listing is built on the routing enumerator, so
// ADVERTISED == ROUTABLE (checked against a real routing.Resolver wired to the same
// service, name by name, across every flag and level change), and the management list
// is the wider union own + managed + member, the member rows key-free.

// newVendorListingEnv is the grant fixture with the vendor_accounts_enabled master flag ON (the
// grant itself never reads it; the model listing, the resolver and the management list
// all do).
func newVendorListingEnv(t *testing.T) *vendorGrantEnv {
	t.Helper()
	e := newVendorGrantEnv(t)
	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, true)
	return e
}

// fixtureModelNames is every model name the grant fixture serves: the name every
// account serves plus one name per account.
func fixtureModelNames(accountIDs ...string) []string {
	names := []string{grantDupModel}
	for _, id := range accountIDs {
		names = append(names, grantModel(id))
	}
	sort.Strings(names)
	return names
}

var allFixtureAccounts = []string{
	grantShared, grantFull, grantStale, grantUnlinked, grantOtherRG, grantNoProvRG,
	grantSub, grantPrivate, grantDup, grantOwnAccount,
}

// advertised is the set of model names the listing advertises to principal on the
// openai dialect, through the public listing (ModelsForFlavor, the reference /v1/models
// is pinned against), restricted to the fixture's vendor model names.
func (e *vendorGrantEnv) advertised(svc *Service, principal auth.Token, flavor string) []string {
	e.t.Helper()
	fixture := fixtureModelNames(allFixtureAccounts...)
	var out []string
	for _, name := range svc.ModelsForFlavor(e.ctx, principal, flavor) {
		if slices.Contains(fixture, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// liveResolver is a REAL routing.Resolver over the fixture's store whose three vendor
// accessors read svc's live settings and whose grant is svc: what it resolves is what
// the gateway would route under the flags as they stand right now.
func (e *vendorGrantEnv) liveResolver(svc *Service) *routing.Resolver {
	r := routing.NewResolver(e.routes, func() time.Time { return e.now }, nil)
	r.SetVendorAccountAccessors(
		func() bool { return svc.VendorAccountsEnabled(e.ctx) },
		func() string { return "vendor_first" },
		func() bool { return svc.VendorSubscriptionsEnabled(e.ctx) },
	)
	r.SetVendorAccountEnumerator(svc)
	return r
}

// inboundDialects pairs each coarse dialect the listings are keyed by with the fine
// request flavor a client of that dialect sends, so the parity pin can ask the
// resolver the question per (name, dialect): that is the granularity it routes at.
var inboundDialects = []struct{ coarse, request string }{
	{routing.APIFlavorOpenAI, "openai_chat"},
	{routing.APIFlavorAnthropic, "anthropic_messages"},
}

// routableNames is the fixture model names the resolver routes for principal on the
// request flavor, sorted.
func (e *vendorGrantEnv) routableNames(r *routing.Resolver, principal auth.Token, request string, accountIDs []string) []string {
	e.t.Helper()
	var out []string
	for _, name := range fixtureModelNames(accountIDs...) {
		target, err := r.Resolve(e.ctx, principal, inference.Request{Model: name, APIFlavor: request})
		if err == nil && target.VendorAccountID != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// requireAdvertisedEqualsRoutable is the parity pin: for every fixture model name AND
// each inbound dialect, the listing advertises the name on that dialect exactly when the
// resolver routes a request of that dialect for it (so neither a name nor a dialect is
// ever advertised that is not routable, or routable and not advertised).
func (e *vendorGrantEnv) requireAdvertisedEqualsRoutable(label string, svc *Service, principal auth.Token) {
	e.t.Helper()
	e.requireDialectParity(label, svc, principal, allFixtureAccounts)
}

// requireDialectParity is requireAdvertisedEqualsRoutable over the model names of
// accountIDs (a test that seeds further accounts names them here).
func (e *vendorGrantEnv) requireDialectParity(label string, svc *Service, principal auth.Token, accountIDs []string) {
	e.t.Helper()
	resolver := e.liveResolver(svc)
	for _, d := range inboundDialects {
		var advertised []string
		for _, name := range svc.ModelsForFlavor(e.ctx, principal, d.coarse) {
			if slices.Contains(fixtureModelNames(accountIDs...), name) {
				advertised = append(advertised, name)
			}
		}
		sort.Strings(advertised)
		if routable := e.routableNames(resolver, principal, d.request, accountIDs); !slices.Equal(advertised, routable) {
			e.t.Errorf("%s (%s): advertised %v != routable %v", label, d.coarse, advertised, routable)
		}
	}
}

// TestVendorListingAdvertisedEqualsRoutable: the advertised names equal the routable
// ones, for every kind of principal, through every state change the grant reacts to (the
// pause by the master sharing flag, a lowered level, subscription sharing, the vendor
// master flag). A shared account a principal can route to is advertised, one it cannot
// is not, and with sharing off only the principal's own accounts are.
func TestVendorListingAdvertisedEqualsRoutable(t *testing.T) {
	e := newVendorListingEnv(t)
	principals := map[string]auth.Token{
		"owner":                  userPrincipal(grantOwner),
		"direct (own + shared)":  userPrincipal(grantDirect),
		"admin-group member":     userPrincipal(grantAdminMember),
		"user-group member":      userPrincipal(grantUserGroupMember),
		"invited, not a member":  userPrincipal(grantInvited),
		"stranger":               userPrincipal(grantStranger),
		"provisioned service":    servicePrincipal(grantServiceOK),
		"unprovisioned service":  servicePrincipal(grantServiceOther),
		"service without its id": {ID: "tok_svc", Kind: "service", Active: true, VendorAccess: auth.VendorAccess{All: true}},
	}
	checkAll := func(phase string) {
		for name, principal := range principals {
			e.requireAdvertisedEqualsRoutable(phase+"/"+name, e.svc, principal)
		}
	}

	sharedOn := fixtureModelNames(grantShared, grantFull, grantDup)
	member := userPrincipal(grantAdminMember)

	checkAll("sharing on")
	if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); !slices.Equal(got, sharedOn) {
		t.Fatalf("a provisioned member advertises %v, want the shared accounts' models %v", got, sharedOn)
	}
	if got := e.advertised(e.svc, userPrincipal(grantStranger), routing.APIFlavorOpenAI); len(got) != 0 {
		t.Fatalf("a non-provisioned principal advertises %v, want nothing", got)
	}
	if got := e.advertised(e.svc, servicePrincipal(grantServiceOK), routing.APIFlavorOpenAI); !slices.Equal(got, sharedOn) {
		t.Fatalf("a provisioned service advertises %v, want the shared accounts' models %v", got, sharedOn)
	}
	// Both dialects: these accounts serve the openai and the anthropic dialect.
	if got := e.advertised(e.svc, member, routing.APIFlavorAnthropic); !slices.Equal(got, sharedOn) {
		t.Fatalf("a provisioned member advertises %v on the anthropic dialect, want %v", got, sharedOn)
	}

	e.flags(false, false)
	checkAll("sharing off")
	if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); len(got) != 0 {
		t.Fatalf("sharing off: a member advertises %v, want nothing (the master flag pauses the shared arm)", got)
	}
	if got := e.advertised(e.svc, userPrincipal(grantDirect), routing.APIFlavorOpenAI); !slices.Equal(got, fixtureModelNames(grantOwnAccount)) {
		t.Fatalf("sharing off: the owner of an account advertises %v, want only the own account's models", got)
	}

	e.flags(true, false)
	checkAll("sharing back on")
	if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); !slices.Equal(got, sharedOn) {
		t.Fatalf("sharing back on: a member advertises %v, want %v", got, sharedOn)
	}

	e.flags(true, true)
	checkAll("subscription sharing on")
	if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); !slices.Equal(got, fixtureModelNames(grantShared, grantFull, grantSub, grantDup)) {
		t.Fatalf("subscription sharing on: a member advertises %v, want the shared subscription account's models too", got)
	}
	e.flags(true, false)

	e.setLevel(grantShared, routing.VendorShareLevelNone)
	checkAll("a shared account lowered to none")
	if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); slices.Contains(got, grantModel(grantShared)) {
		t.Fatalf("a lowered account is still advertised to a member: %v", got)
	}
	e.setLevel(grantShared, routing.VendorShareLevelShared)

	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	checkAll("vendor accounts master flag off")
	if got := e.advertised(e.svc, userPrincipal(grantOwner), routing.APIFlavorOpenAI); len(got) != 0 {
		t.Fatalf("master flag off: the owner advertises %v, want nothing", got)
	}
}

// TestVendorListingSharedArmFailsClosedLikeRouting: when the shared candidates cannot be
// computed the listing advertises the principal's OWN accounts and no shared one, exactly
// what the resolver routes in that window (one fail direction, so parity holds even in the
// error window); when the own read itself fails the set is nil and reported incomplete.
func TestVendorListingSharedArmFailsClosedLikeRouting(t *testing.T) {
	e := newVendorListingEnv(t)
	direct := userPrincipal(grantDirect)

	for name, fail := range map[string]failingGrantStore{
		"provisioned-group lookup fails":     {provisionTargets: true},
		"group-linked accounts lookup fails": {groupAccounts: true},
		"account read fails":                 {accountByID: true},
	} {
		t.Run(name, func(t *testing.T) {
			fail.Store = e.routes
			svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
			if got, want := e.advertised(svc, direct, routing.APIFlavorOpenAI), fixtureModelNames(grantOwnAccount); !slices.Equal(got, want) {
				t.Errorf("advertised = %v, want only the own account's models %v", got, want)
			}
			e.requireAdvertisedEqualsRoutable(name, svc, direct)
			if _, loaded := svc.ownVendorAccountModels(e.ctx, direct); !loaded {
				t.Error("a shared-arm failure reported the set incomplete; it is the routable set, the same one the resolver uses")
			}
		})
	}

	t.Run("the own read fails", func(t *testing.T) {
		fail := failingGrantStore{Store: e.routes, owner: true}
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
		owned, loaded := svc.ownVendorAccountModels(e.ctx, direct)
		if owned != nil || loaded {
			t.Errorf("own read failing: owned=%v loaded=%v, want nil/false (nothing shared leaks, the set is flagged incomplete)", owned, loaded)
		}
		if got := e.advertised(svc, direct, routing.APIFlavorOpenAI); len(got) != 0 {
			t.Errorf("own read failing: advertised %v, want nothing", got)
		}
	})
}

// A shared account that is not active is in the management list but not advertised: the
// two are different sets, and the resolver skips an inactive account.
func TestVendorListingAdvertisesOnlyActiveSharedAccounts(t *testing.T) {
	e := newVendorListingEnv(t)
	member := userPrincipal(grantAdminMember)

	for _, status := range []string{routing.VendorAccountStatusDisabled, routing.VendorAccountStatusNeedsReconnect} {
		acc, err := e.routes.VendorAccountByID(e.ctx, grantShared)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		acc.Status = status
		if err := e.routes.UpdateVendorAccount(e.ctx, acc); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}
		if got := e.advertised(e.svc, member, routing.APIFlavorOpenAI); slices.Contains(got, grantModel(grantShared)) {
			t.Errorf("%s shared account is advertised: %v", status, got)
		}
		e.requireAdvertisedEqualsRoutable(status, e.svc, member)

		list, err := e.svc.ListVendorAccounts(e.ctx, member)
		if err != nil {
			t.Fatalf("ListVendorAccounts: %v", err)
		}
		if !slices.Contains(listedIDs(list), grantShared) {
			t.Errorf("%s shared account is missing from the member's management list %v (a member sees what it cannot use as well)", status, listedIDs(list))
		}
	}
}

// The dashboard route table reads the same choke point, so a member's table carries the
// shared accounts' models under the account's name.
func TestVendorDashboardRoutesCarryTheSharedAccountsModels(t *testing.T) {
	e := newVendorListingEnv(t)
	member := userPrincipal(grantAdminMember)

	routes := e.svc.vendorDashboardRoutes(e.ctx, member)
	for _, id := range []string{grantShared, grantFull, grantDup} {
		if rows := routesForModel(routes, grantModel(id)); len(rows) != 1 || rows[0].Host != id || rows[0].ID != id+":"+grantModel(id) {
			t.Errorf("dashboard rows for %s = %+v, want one row on that account", grantModel(id), rows)
		}
	}
	if rows := routesForModel(routes, grantModel(grantStale)); len(rows) != 0 {
		t.Errorf("dashboard lists a model of a non-shared account: %+v", rows)
	}
	if got := e.svc.vendorDashboardRoutes(e.ctx, userPrincipal(grantStranger)); len(got) != 0 {
		t.Errorf("a non-provisioned principal's dashboard lists vendor routes: %+v", got)
	}
}

// openAISubscriptionAccount seeds an ACTIVE OpenAI SUBSCRIPTION account (the openai
// dialect only: the ChatGPT backend speaks Responses only) serving grantDupModel and
// grantModel(id), at level, linked into rgIDs.
func (e *vendorGrantEnv) openAISubscriptionAccount(id, owner, level string, rgIDs ...string) {
	e.t.Helper()
	acc := routing.VendorAccount{
		ID: id, OwnerUserID: owner, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: id,
		Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:oauth-" + id, CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.routes.CreateVendorAccount(e.ctx, acc); err != nil {
		e.t.Fatalf("CreateVendorAccount(%s): %v", id, err)
	}
	if err := e.routes.SetVendorAccountModels(e.ctx, id, []routing.VendorAccountModel{
		{AccountID: id, GatewayModel: grantModel(id), UpstreamModel: grantModel(id), APIFlavor: routing.APIFlavorOpenAI},
		{AccountID: id, GatewayModel: grantDupModel, UpstreamModel: grantDupModel, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		e.t.Fatalf("SetVendorAccountModels(%s): %v", id, err)
	}
	e.setLevel(id, level)
	for _, rgID := range rgIDs {
		if err := e.routes.SetResourceGroupVendorAccount(e.ctx, rgID, id); err != nil {
			e.t.Fatalf("SetResourceGroupVendorAccount(%s, %s): %v", rgID, id, err)
		}
	}
}

// flavorsOf is the dialects the listing advertises name under for principal.
func (e *vendorGrantEnv) flavorsOf(principal auth.Token, name string) []string {
	e.t.Helper()
	sets, loaded := e.svc.vendorModelFlavorSets(e.ctx, principal)
	if !loaded {
		e.t.Fatal("vendorModelFlavorSets reported an incomplete set on a healthy store")
	}
	var out []string
	for f := range sets[name] {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// resolvesOn is the id of the vendor account the resolver routes a request of the fine
// flavor to for principal, or "" when it routes none.
func (e *vendorGrantEnv) resolvesOn(r *routing.Resolver, principal auth.Token, model, request string) string {
	e.t.Helper()
	target, err := r.Resolve(e.ctx, principal, inference.Request{Model: model, APIFlavor: request})
	if err != nil {
		return ""
	}
	return target.VendorAccountID
}

// TestVendorListingFlavorSetIsPerDialectRoutable (spec section 8, Precedence & parity):
// the resolver chooses an account per (name, dialect), in precedence order (own block
// first, then the shared accounts by id): an OpenAI SUBSCRIPTION account (the openai
// dialect only) is skipped for the anthropic dialect and the request falls through to the
// next account that serves the name. So the dialects advertised for a name are the union
// of the dialects its eligible accounts serve, each dialect backed by the first account
// that serves it, never the first account's dialects alone: a name whose first account is
// openai-only is still advertised, and routable, on the anthropic dialect through a later
// both-dialect account. The union never over-advertises: a dialect no account serves (a
// name only an openai-only account has) is not advertised.
func TestVendorListingFlavorSetIsPerDialectRoutable(t *testing.T) {
	both := []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}
	openAIOnly := []string{routing.APIFlavorOpenAI}
	const (
		openAIRequest    = "openai_chat"
		anthropicRequest = "anthropic_messages"
	)

	t.Run("an own openai-only account first, a later own both-dialect account (the own-only regression)", func(t *testing.T) {
		e := newVendorListingEnv(t)
		const (
			user    = "usr_g_flavor_own"
			subID   = "vacc_g_aa_ownsub"
			keyID   = "vacc_g_bb_ownkey"
			subOnly = "m-" + subID
		)
		e.createUser(user, "user")
		e.openAISubscriptionAccount(subID, user, routing.VendorShareLevelNone)
		e.account(keyID, user, routing.VendorAuthAPIKey, routing.VendorShareLevelNone)
		principal := userPrincipal(user)
		r := e.liveResolver(e.svc)

		if got := e.routable(e.svc, principal); !slices.Equal(got, []string{subID, keyID}) {
			t.Fatalf("routable accounts = %v, want the two own accounts, the openai-only one first", got)
		}
		// The first account serves the name on the openai dialect, the later one on both:
		// the resolver serves each dialect from the first account that serves it.
		if got := e.resolvesOn(r, principal, grantDupModel, openAIRequest); got != subID {
			t.Fatalf("an openai request for %s routes to %q, want the first account %q", grantDupModel, got, subID)
		}
		if got := e.resolvesOn(r, principal, grantDupModel, anthropicRequest); got != keyID {
			t.Fatalf("an anthropic request for %s routes to %q, want the later both-dialect account %q", grantDupModel, got, keyID)
		}
		// ... so the name is advertised on both dialects.
		if got := e.flavorsOf(principal, grantDupModel); !slices.Equal(got, both) {
			t.Errorf("%s flavors = %v, want %v (anthropic is routable through the later account)", grantDupModel, got, both)
		}
		if got := e.svc.ModelsForFlavor(e.ctx, principal, routing.APIFlavorAnthropic); !slices.Contains(got, grantDupModel) {
			t.Errorf("/anthropic/v1/models does not list %s, which the resolver routes: %v", grantDupModel, got)
		}
		// A name only the openai-only account serves is NOT advertised on the anthropic
		// dialect: the union adds only dialects some account really serves.
		if got := e.flavorsOf(principal, subOnly); !slices.Equal(got, openAIOnly) {
			t.Errorf("%s flavors = %v, want %v (no account serves it on the anthropic dialect)", subOnly, got, openAIOnly)
		}
		if got := e.resolvesOn(r, principal, subOnly, anthropicRequest); got != "" {
			t.Errorf("an anthropic request for %s routes to %q, want none", subOnly, got)
		}
		e.requireDialectParity("own openai-only first, own both-dialect later", e.svc, principal, []string{subID, keyID})
	})

	t.Run("an own openai-only account first, shared both-dialect accounts later", func(t *testing.T) {
		e := newVendorListingEnv(t)
		const (
			user  = "usr_g_flavor"
			subID = "vacc_g_zz_ownsub"
		)
		e.createUser(user, "user")
		e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindUser, user)
		e.openAISubscriptionAccount(subID, user, routing.VendorShareLevelNone)
		principal := userPrincipal(user)
		r := e.liveResolver(e.svc)

		// grantShared, grantFull and grantDup serve grantDupModel on BOTH dialects and are
		// routable for this principal, after the own account.
		if got := e.routable(e.svc, principal); !slices.Equal(got, append([]string{subID}, grantedShared...)) {
			t.Fatalf("routable accounts = %v, want own first then the shared ones", got)
		}
		if got := e.resolvesOn(r, principal, grantDupModel, openAIRequest); got != subID {
			t.Errorf("an openai request routes to %q, want the own account %q (own block first)", got, subID)
		}
		if got := e.resolvesOn(r, principal, grantDupModel, anthropicRequest); got != grantShared {
			t.Errorf("an anthropic request routes to %q, want the first shared account %q", got, grantShared)
		}
		if got := e.flavorsOf(principal, grantDupModel); !slices.Equal(got, both) {
			t.Errorf("%s flavors = %v, want %v", grantDupModel, got, both)
		}
		if got := e.flavorsOf(principal, grantModel(grantShared)); !slices.Equal(got, both) {
			t.Errorf("%s flavors = %v, want %v", grantModel(grantShared), got, both)
		}
		if got := e.svc.ModelsForFlavor(e.ctx, principal, routing.APIFlavorAnthropic); !slices.Contains(got, grantDupModel) {
			t.Errorf("/anthropic/v1/models does not list %s: %v", grantDupModel, got)
		}
		e.requireDialectParity("own openai-only first, shared later", e.svc, principal, append([]string{subID}, allFixtureAccounts...))
	})

	t.Run("among shared accounts the first active by id serves each dialect", func(t *testing.T) {
		e := newVendorListingEnv(t)
		e.flags(true, true) // a shared subscription account is routable only with this
		const subID = "vacc_g_00_subshared"
		e.openAISubscriptionAccount(subID, grantOwner, routing.VendorShareLevelShared, e.rgGranted.ID)
		member := withOptIn(userPrincipal(grantAdminMember), subID) // the extra shared account must be named too
		r := e.liveResolver(e.svc)
		accounts := append([]string{subID}, allFixtureAccounts...)

		if got := e.routable(e.svc, member); got[0] != subID {
			t.Fatalf("routable accounts = %v, want the subscription account first by id", got)
		}
		if got := e.resolvesOn(r, member, grantDupModel, openAIRequest); got != subID {
			t.Errorf("an openai request routes to %q, want the first shared account %q", got, subID)
		}
		if got := e.resolvesOn(r, member, grantDupModel, anthropicRequest); got != grantShared {
			t.Errorf("an anthropic request routes to %q, want the next shared account %q", got, grantShared)
		}
		if got := e.flavorsOf(member, grantDupModel); !slices.Equal(got, both) {
			t.Errorf("%s flavors = %v, want %v", grantDupModel, got, both)
		}
		e.requireDialectParity("first shared openai-only", e.svc, member, accounts)

		// An inactive first account serves nothing: the next account serves both dialects.
		acc, err := e.routes.VendorAccountByID(e.ctx, subID)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		acc.Status = routing.VendorAccountStatusDisabled
		if err := e.routes.UpdateVendorAccount(e.ctx, acc); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}
		if got := e.resolvesOn(r, member, grantDupModel, openAIRequest); got != grantShared {
			t.Errorf("an openai request routes to %q with the first account disabled, want %q", got, grantShared)
		}
		if got := e.flavorsOf(member, grantDupModel); !slices.Equal(got, both) {
			t.Errorf("%s flavors with the first account disabled = %v, want %v", grantDupModel, got, both)
		}
		if got := e.flavorsOf(member, "m-"+subID); len(got) != 0 {
			t.Errorf("a disabled account's own model is advertised: %v", got)
		}
		e.requireDialectParity("first shared disabled", e.svc, member, accounts)
	})

	t.Run("an own both-dialect account first keeps both against a shared openai-only one", func(t *testing.T) {
		e := newVendorListingEnv(t)
		e.flags(true, true)
		const subID = "vacc_g_00_subshared"
		e.openAISubscriptionAccount(subID, grantOwner, routing.VendorShareLevelShared, e.rgGranted.ID)
		// grantDirect owns grantOwnAccount (api_key, both dialects) and is provisioned.
		direct := withOptIn(userPrincipal(grantDirect), subID)
		r := e.liveResolver(e.svc)
		if got := e.flavorsOf(direct, grantDupModel); !slices.Equal(got, both) {
			t.Errorf("%s flavors = %v, want %v", grantDupModel, got, both)
		}
		for _, request := range []string{openAIRequest, anthropicRequest} {
			if got := e.resolvesOn(r, direct, grantDupModel, request); got != grantOwnAccount {
				t.Errorf("a %s request routes to %q, want the own account %q", request, got, grantOwnAccount)
			}
		}
		e.requireDialectParity("own both-dialect first", e.svc, direct, append([]string{subID}, allFixtureAccounts...))
	})
}

// TestVendorListingServiceTokenParity: the listing asks the same eligibility question the
// resolver does (a user, or a service token carrying its service id), so a service token
// that is provisioned and holds vendor access advertises what it routes, and the
// identity-less principals advertise nothing and are not an error.
func TestVendorListingServiceTokenParity(t *testing.T) {
	e := newVendorListingEnv(t)
	svc := e.svc
	if got, loaded := svc.ownVendorAccountModels(e.ctx, servicePrincipal(grantServiceOK)); !loaded || len(got) != len(grantedShared) {
		t.Fatalf("provisioned service: %d accounts, loaded=%v, want %d shared accounts, loaded", len(got), loaded, len(grantedShared))
	}
	for name, principal := range map[string]auth.Token{
		"unprovisioned service":  servicePrincipal(grantServiceOther),
		"service without its id": {Kind: "service", Active: true, VendorAccess: auth.VendorAccess{All: true}},
		"no identity":            {Active: true, VendorAccess: auth.VendorAccess{All: true}},
	} {
		if got, loaded := svc.ownVendorAccountModels(e.ctx, principal); len(got) != 0 || !loaded {
			t.Errorf("%s: %d accounts, loaded=%v, want none, loaded", name, len(got), loaded)
		}
	}
	// A service token without the opt-in (the zero VendorAccess) advertises nothing.
	optedOut := servicePrincipal(grantServiceOK)
	optedOut.VendorAccess = auth.VendorAccess{}
	if got, _ := svc.ownVendorAccountModels(e.ctx, optedOut); len(got) != 0 {
		t.Errorf("a service token with no vendor access advertises %d accounts, want none", len(got))
	}
}

// --- The management list: own + managed + member ----------------------------------

// vendorListUnion is the grant fixture plus the management fixture the union needs: a
// principal (grantAdminMember) who is BOTH a member (provisioned through AG) and a CM of
// AG, and accounts that each fall in exactly one arm.
type vendorListUnion struct {
	*vendorGrantEnv
	cm            auth.Token
	ownAccount    string // owned by the CM, level none
	managedOnly   string // linked to AG (the CM manages it), in no resource group
	memberAndCM   string // linked to AG AND routable through the RG: listed once
	memberOnly    string // routable through the RG, linked to no admin group
	sharedNoLinks string // shared, no resource group and no admin group
}

func newVendorListUnion(t *testing.T) *vendorListUnion {
	t.Helper()
	e := &vendorListUnion{vendorGrantEnv: newVendorListingEnv(t)}
	e.cm = token(grantAdminMember)
	e.setUserRole(grantAdminMember, "admin")   // a co-manager must hold the admin role
	owner := token("usr_s", "system", "admin") // creator and owner of AG
	if err := e.svc.PromoteManager(e.ctx, owner, e.ag.ID, grantAdminMember, ManagerPermissions{CanManageVendorAccounts: true}); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}

	e.ownAccount = "vacc_g_30_cmown"
	e.account(e.ownAccount, grantAdminMember, routing.VendorAuthAPIKey, routing.VendorShareLevelNone)
	e.managedOnly = "vacc_g_31_managed"
	e.account(e.managedOnly, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared)
	e.manage(e.managedOnly)
	e.memberAndCM = grantFull // routable through rgGranted, and now managed through AG too
	e.manage(e.memberAndCM)
	e.memberOnly = grantShared
	e.sharedNoLinks = grantUnlinked
	return e
}

// manage links the account to AG with the containment root, the state a raise produces.
func (e *vendorListUnion) manage(id string) {
	e.t.Helper()
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, id, e.ag.ID); err != nil {
		e.t.Fatalf("SetVendorAccountAdminGroup(%s): %v", id, err)
	}
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, id, e.ag.ParentGroupID); err != nil {
		e.t.Fatalf("UpdateVendorAccountSystemGroup(%s): %v", id, err)
	}
}

// TestListVendorAccountsReturnsOwnManagedAndMemberSets: the management list is the full
// union (spec section 8). A CM sees an account it manages even though it is not
// provisioned into it; a member sees every account it can route; an account that is both
// is listed once; the order is own, managed, member (each oldest first, then by id).
func TestListVendorAccountsReturnsOwnManagedAndMemberSets(t *testing.T) {
	e := newVendorListUnion(t)

	list, err := e.svc.ListVendorAccounts(e.ctx, e.cm)
	if err != nil {
		t.Fatalf("ListVendorAccounts: %v", err)
	}
	// own: the CM's own account. managed (AG): managedOnly, memberAndCM. member (RG):
	// grantShared, grantDup (grantFull is listed once, as managed).
	want := []string{e.ownAccount, e.memberAndCM, e.managedOnly, grantShared, grantDup}
	if got := listedIDs(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want own, managed, member: %v (grantFull once, as managed; the unlinked, stale and unprovisioned accounts absent)", got, want)
	}
	byID := map[string]VendorAccountDTO{}
	for _, dto := range list.Data {
		byID[dto.ID] = dto
	}
	if dto := byID[e.ownAccount]; !dto.IsOwner || dto.SharedBy != "" {
		t.Errorf("own account: is_owner/shared_by = %v/%q, want true/\"\"", dto.IsOwner, dto.SharedBy)
	}
	for _, id := range []string{e.managedOnly, e.memberAndCM, grantShared, grantDup} {
		if dto := byID[id]; dto.IsOwner || dto.SharedBy != grantOwner {
			t.Errorf("%s: is_owner/shared_by = %v/%q, want false/%q", id, dto.IsOwner, dto.SharedBy, grantOwner)
		}
	}
	if len(byID[e.managedOnly].AdminGroups) != 1 {
		t.Errorf("the managed row carries no management groups: %+v", byID[e.managedOnly])
	}
}

// TestListVendorAccountsMemberOnlyRowsAreKeyFree: a principal who reaches an account only
// through a resource group gets a row for it that holds the booleans and NEVER the owner's
// secret, is_owner false and shared_by the owner's name. The planted secrets are the
// stored sealed credentials, and the row is checked on the wire.
func TestListVendorAccountsMemberOnlyRowsAreKeyFree(t *testing.T) {
	e := newVendorListingEnv(t)
	e.flags(true, true) // so the subscription account is reachable too
	member := token(grantAdminMember)

	list, err := e.svc.ListVendorAccounts(e.ctx, member)
	if err != nil {
		t.Fatalf("ListVendorAccounts: %v", err)
	}
	want := []string{grantShared, grantFull, grantSub, grantDup}
	if got := listedIDs(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("member list = %v, want the routable accounts %v", got, want)
	}

	var secrets []string
	for _, id := range want {
		acc, err := e.routes.VendorAccountByID(e.ctx, id)
		if err != nil {
			t.Fatalf("VendorAccountByID(%s): %v", id, err)
		}
		for _, stored := range []string{acc.APIKey, acc.OAuthTokens} {
			if stored != "" {
				secrets = append(secrets, stored)
			}
		}
	}
	if len(secrets) != len(want) {
		t.Fatalf("planted %d secrets for %d accounts: the key-free assertions would be vacuous", len(secrets), len(want))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Errorf("a member's list leaks the stored credential %q: %s", secret, raw)
		}
	}
	for _, dto := range list.Data {
		isSub := dto.ID == grantSub
		if dto.IsOwner || dto.SharedBy != grantOwner || dto.APIKeySet == isSub || dto.SubscriptionConnected != isSub {
			t.Errorf("%s: is_owner/shared_by/api_key_set/subscription_connected = %v/%q/%v/%v, want false/%q/%v/%v",
				dto.ID, dto.IsOwner, dto.SharedBy, dto.APIKeySet, dto.SubscriptionConnected, grantOwner, !isSub, isSub)
		}
	}
}

// TestListVendorAccountsMemberSetFollowsTheGrant: the member arm is the routing grant's
// shared arm, so it is gone with the master sharing flag, empty for a principal that is
// not provisioned (an invited, not yet accepted membership included), does not widen
// system scope, and drops a `none` account whose resource-group link survived or a
// subscription account while subscription sharing is off.
func TestListVendorAccountsMemberSetFollowsTheGrant(t *testing.T) {
	e := newVendorListingEnv(t)
	member := token(grantAdminMember)

	list, err := e.svc.ListVendorAccounts(e.ctx, member)
	if err != nil {
		t.Fatalf("ListVendorAccounts: %v", err)
	}
	if got := listedIDs(list); !reflect.DeepEqual(got, grantedShared) || slices.Contains(got, grantStale) || slices.Contains(got, grantSub) {
		t.Fatalf("member list = %v, want only the live shared/full api_key accounts %v (no stale link, no subscription while sharing it is off)", got, grantedShared)
	}

	for name, principal := range map[string]auth.Token{
		"stranger":                token(grantStranger),
		"invited, not a member":   token(grantInvited),
		"system scope, a member":  token(grantAdminMember, "system", "admin"),
		"no user identity":        {Scopes: []string{"gateway:use"}},
		"service token (no list)": {Kind: "service", ServiceID: grantServiceOK, Scopes: []string{"gateway:use"}},
	} {
		list, err := e.svc.ListVendorAccounts(e.ctx, principal)
		if err != nil || len(list.Data) != 0 {
			t.Errorf("%s: list = %v, %v, want empty", name, listedIDs(list), err)
		}
		if list.Data == nil {
			t.Errorf("%s: Data is nil, want [] on the wire", name)
		}
	}

	e.flags(false, false)
	if list, err := e.svc.ListVendorAccounts(e.ctx, member); err != nil || len(list.Data) != 0 {
		t.Errorf("sharing off: member list = %v, %v, want empty (every account behaves personal)", listedIDs(list), err)
	}
	e.flags(true, false)

	e.setLevel(grantShared, routing.VendorShareLevelNone)
	if list, err := e.svc.ListVendorAccounts(e.ctx, member); err != nil || slices.Contains(listedIDs(list), grantShared) {
		t.Errorf("a lowered account is still in the member list: %v, %v", listedIDs(list), err)
	}
	e.setLevel(grantShared, routing.VendorShareLevelShared)

	// The owner's list still holds their own accounts only (they are not provisioned).
	ownerList, err := e.svc.ListVendorAccounts(e.ctx, token(grantOwner))
	if err != nil || len(ownerList.Data) != len(allOwnerAccounts) {
		t.Errorf("owner list = %v, %v, want the %d owned accounts", listedIDs(ownerList), err, len(allOwnerAccounts))
	}

	// A principal who is provisioned AND owns an account lists it once, as their own, first.
	direct, err := e.svc.ListVendorAccounts(e.ctx, token(grantDirect))
	if err != nil {
		t.Fatalf("direct list: %v", err)
	}
	if got, want := listedIDs(direct), append([]string{grantOwnAccount}, grantedShared...); !reflect.DeepEqual(got, want) {
		t.Errorf("direct list = %v, want own first then the member accounts %v", got, want)
	}
	if !direct.Data[0].IsOwner {
		t.Errorf("the principal's own account is not rendered as theirs: %+v", direct.Data[0])
	}
}

// A member-arm store failure fails the management read, like the managed arm: the page is
// not a gate, so the safe direction is to show less, and a partial list that silently
// drops the shared accounts would hide them from a member without saying so.
func TestListVendorAccountsMemberArmStoreErrorFailsTheRead(t *testing.T) {
	e := newVendorListingEnv(t)
	fail := failingGrantStore{Store: e.routes, groupAccounts: true}
	svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
	if list, err := svc.ListVendorAccounts(e.ctx, token(grantAdminMember)); err == nil {
		t.Fatalf("ListVendorAccounts = %v, nil; want the store error", listedIDs(list))
	}
	// A principal whose member arm is empty by flag never reaches the failing read.
	// svc is a second Service over the fixture's settings store, so e.flags drops only
	// e.svc's cached sharing switches; svc's are dropped by hand, as the settings PUT
	// does for the one Service of a real process.
	e.flags(false, false)
	svc.invalidateVendorSharingFlags()
	if _, err := svc.ListVendorAccounts(e.ctx, token(grantAdminMember)); err != nil {
		t.Errorf("sharing off must not touch the shared arm: %v", err)
	}
}
