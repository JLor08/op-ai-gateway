// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"testing"
	"time"
)

// This file pins the ROUTING GRANT of shared vendor accounts (#183, spec 2026-10-15
// section 8): Service.VendorAccountsForRouting, the single routing-time gate that says
// which vendor accounts a principal may route to, and (end to end through a REAL
// routing.Resolver wired to it) what the resolver then does with the answer.
//
// The grant, in one sentence: an account is available iff it is OWNED, or it is
// effectively `shared`/`full` RIGHT NOW (read live, so a lowered, paused or master-off
// account grants nothing) AND linked to a resource group the principal is provisioned
// into. The shared arm fails CLOSED on a store error while the owner's own accounts are
// always returned. It does not read resource_provisioning_enforce and has no open pool.

const (
	grantOwner           = "usr_g_owner"
	grantDirect          = "usr_g_direct"   // provisioned directly (kind user); also owns grantOwnAccount
	grantAdminMember     = "usr_g_agm"      // member of AG, which is provisioned (kind admin_group)
	grantUserGroupMember = "usr_g_ugm"      // member of UG, which is provisioned (kind user_group)
	grantInvited         = "usr_g_invited"  // INVITED to AG, never a member
	grantStranger        = "usr_g_stranger" // provisioned nowhere
	grantServiceOK       = "svc_g_ok"       // provisioned directly (kind service)
	grantServiceOther    = "svc_g_other"    // provisioned nowhere

	// accounts, named for the state they pin. The ids sort in numeric order, so the
	// expected id order below reads off the names.
	grantShared     = "vacc_g_01_shared"
	grantFull       = "vacc_g_02_full"
	grantStale      = "vacc_g_03_stale"    // level none, RG link survives (torn downgrade)
	grantUnlinked   = "vacc_g_04_unlinked" // shared, in no resource group
	grantOtherRG    = "vacc_g_05_otherrg"  // shared, in a group nobody here is provisioned into
	grantNoProvRG   = "vacc_g_06_noprov"   // shared, in a group with NO provisions at all
	grantSub        = "vacc_g_07_sub"      // subscription, shared, in the granted group
	grantPrivate    = "vacc_g_08_private"  // level none, no link
	grantDup        = "vacc_g_09_dup"      // shared, in BOTH granted groups (listed once)
	grantOwnAccount = "vacc_g_zz_own"      // owned by grantDirect: sorts AFTER every shared id

	// grantDupModel is served by every account, so a test can make accounts collide on
	// one public name; grantModel(id) is served by that account alone.
	grantDupModel = "dup-model"
)

func grantModel(accountID string) string { return "m-" + accountID }

type vendorGrantEnv struct {
	*groupTestEnv
	ag, ug                                   UserGroupDTO
	rgGranted, rgGranted2, rgOther, rgNoProv routing.ResourceGroup
}

// newVendorGrantEnv builds the fixture: sharing ON, subscription sharing OFF (the
// defaults a deployment that enabled sharing starts from), the principals above, two
// resource groups the principals are provisioned into, one nobody is, one with no
// provisions, and one account per state the grant has to tell apart.
func newVendorGrantEnv(t *testing.T) *vendorGrantEnv {
	t.Helper()
	e := &vendorGrantEnv{groupTestEnv: newGroupTestEnv(t)}
	e.svc.settings = NewMemorySystemSettings()
	setVendorSharingFlags(t, e.svc, true, false)

	e.createUser("usr_s", "system_admin")
	for _, id := range []string{grantOwner, grantDirect, grantAdminMember, grantUserGroupMember, grantInvited, grantStranger} {
		e.createUser(id, "user")
	}
	sysAdmin := token("usr_s", "system", "admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, grantAdminMember, grantUserGroupMember, grantInvited)
	e.ag = e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, e.ag.ID, grantAdminMember)
	if err := e.dir.SetUserGroupMember(e.ctx, e.ag.ID, grantInvited, store.GroupStateInvited, ""); err != nil {
		t.Fatalf("SetUserGroupMember(invited): %v", err)
	}
	// UG hangs under a SECOND admin group that is provisioned nowhere, so the
	// user-group member's only path to the resource group is the user_group provision
	// (its creator is its owner and a member).
	ag2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag2.ID, grantUserGroupMember)
	e.ug = e.mustCreateGroup(token(grantUserGroupMember), CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag2.ID})

	newRG := func(id string) routing.ResourceGroup {
		return e.mustCreateResourceGroup(routing.ResourceGroup{ID: id, Name: id, Status: routing.ServerStatusActive, SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now})
	}
	e.rgGranted = newRG("rgrp_g_granted")
	e.rgGranted2 = newRG("rgrp_g_granted2")
	e.rgOther = newRG("rgrp_g_other")
	e.rgNoProv = newRG("rgrp_g_noprov")
	e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindUser, grantDirect)
	e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindAdminGroup, e.ag.ID)
	e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindUserGroup, e.ug.ID)
	e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindService, grantServiceOK)
	e.mustProvisionResourceGroup(e.rgGranted2.ID, routing.ProvisionKindUser, grantDirect)
	e.mustProvisionResourceGroup(e.rgOther.ID, routing.ProvisionKindAdminGroup, "ag_ghost")

	e.account(grantShared, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared, e.rgGranted.ID)
	e.account(grantFull, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelFull, e.rgGranted.ID)
	e.account(grantStale, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelNone, e.rgGranted.ID)
	e.account(grantUnlinked, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared)
	e.account(grantOtherRG, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared, e.rgOther.ID)
	e.account(grantNoProvRG, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared, e.rgNoProv.ID)
	e.account(grantSub, grantOwner, routing.VendorAuthSubscription, routing.VendorShareLevelShared, e.rgGranted.ID)
	e.account(grantPrivate, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelNone)
	e.account(grantDup, grantOwner, routing.VendorAuthAPIKey, routing.VendorShareLevelShared, e.rgGranted.ID, e.rgGranted2.ID)
	e.account(grantOwnAccount, grantDirect, routing.VendorAuthAPIKey, routing.VendorShareLevelNone)
	return e
}

// account seeds an ACTIVE account straight through the store (the grant reads the
// store, not the create path), serving grantDupModel and grantModel(id), at level,
// linked into rgIDs. A subscription account carries sealed OAuth tokens instead of a key.
func (e *vendorGrantEnv) account(id, owner, authType, level string, rgIDs ...string) routing.VendorAccount {
	e.t.Helper()
	acc := routing.VendorAccount{
		ID: id, OwnerUserID: owner, Vendor: routing.VendorOpenAI, AuthType: authType, Name: id,
		Status: routing.VendorAccountStatusActive, CreatedAt: e.now, UpdatedAt: e.now,
	}
	if authType == routing.VendorAuthSubscription {
		acc.Vendor = routing.VendorAnthropic
		acc.OAuthTokens = "enc:oauth-" + id
	} else {
		acc.APIKey = "enc:key-" + id
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
	got, err := e.routes.VendorAccountByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountByID(%s): %v", id, err)
	}
	return got
}

func (e *vendorGrantEnv) setLevel(id, level string) {
	e.t.Helper()
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, id, level); err != nil {
		e.t.Fatalf("UpdateVendorAccountShareLevel(%s, %s): %v", id, level, err)
	}
}

func (e *vendorGrantEnv) flags(sharing, subscriptionSharing bool) {
	e.t.Helper()
	setVendorSharingFlags(e.t, e.svc, sharing, subscriptionSharing)
}

// routable is what VendorAccountsForRouting returns for principal, as ids in order.
func (e *vendorGrantEnv) routable(svc *Service, principal auth.Token) []string {
	e.t.Helper()
	accounts, err := svc.VendorAccountsForRouting(e.ctx, principal)
	if err != nil {
		e.t.Fatalf("VendorAccountsForRouting(%+v): %v", principal, err)
	}
	return accountIDs(accounts)
}

func accountIDs(accounts []routing.VendorAccount) []string {
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	return ids
}

func (e *vendorGrantEnv) requireRoutable(label string, svc *Service, principal auth.Token, want ...string) {
	e.t.Helper()
	if got := e.routable(svc, principal); !slices.Equal(got, append([]string{}, want...)) {
		e.t.Errorf("%s: routable accounts = %v, want %v", label, got, want)
	}
}

// fixtureOptIn is the vendor policy of a principal that has opted into EVERYTHING the
// grant fixture seeds, so that REACH (the grant under test) is the only variable the grant
// and listing tests below can see: all:true covers the principal's own accounts, and an
// explicit per-account entry covers each shared one, because a shared account is used
// only when the token names it (the per-account opt-in, pinned in
// service_vendor_sharing_optin_test.go). Naming an account the principal cannot reach
// is harmless: the grant still denies it.
func fixtureOptIn() auth.VendorAccess {
	access := auth.VendorAccess{All: true}
	for _, id := range allFixtureAccounts {
		access.Accounts = append(access.Accounts, auth.VendorAccessEntry{AccountID: id})
	}
	return access
}

// withOptIn is p additionally opted into the named accounts, for a test that seeds
// accounts beyond the fixture's.
func withOptIn(p auth.Token, accountIDs ...string) auth.Token {
	access := auth.VendorAccess{All: p.VendorAccess.All, Accounts: append([]auth.VendorAccessEntry(nil), p.VendorAccess.Accounts...)}
	for _, id := range accountIDs {
		access.Accounts = append(access.Accounts, auth.VendorAccessEntry{AccountID: id})
	}
	p.VendorAccess = access
	return p
}

func userPrincipal(id string) auth.Token {
	return auth.Token{ID: "tok_" + id, UserID: id, Active: true, VendorAccess: fixtureOptIn()}
}

func servicePrincipal(id string) auth.Token {
	return auth.Token{ID: "tok_" + id, Kind: "service", ServiceID: id, Active: true, VendorAccess: fixtureOptIn()}
}

// resolver is a REAL routing.Resolver over the fixture's store with vendor routing on
// and svc as its grant: whatever it resolves is what the gateway would route.
func (e *vendorGrantEnv) resolver(svc *Service) *routing.Resolver {
	r := routing.NewResolver(e.routes, func() time.Time { return e.now }, nil)
	r.SetVendorAccountAccessors(func() bool { return true }, func() string { return "vendor_first" }, nil)
	r.SetVendorAccountEnumerator(svc)
	return r
}

// resolvesTo returns the id of the vendor account the resolver routes model to for
// principal, or "" with the error when it does not.
func (e *vendorGrantEnv) resolvesTo(r *routing.Resolver, principal auth.Token, model string) (string, error) {
	e.t.Helper()
	target, err := r.Resolve(e.ctx, principal, inference.Request{Model: model, APIFlavor: "openai_chat"})
	if err != nil {
		return "", err
	}
	return target.VendorAccountID, nil
}

func (e *vendorGrantEnv) requireResolves(label string, r *routing.Resolver, principal auth.Token, model, wantAccount string) {
	e.t.Helper()
	got, err := e.resolvesTo(r, principal, model)
	if err != nil {
		e.t.Errorf("%s: Resolve(%s) = %v, want account %s", label, model, err, wantAccount)
		return
	}
	if got != wantAccount {
		e.t.Errorf("%s: Resolve(%s) routed to %q, want %q", label, model, got, wantAccount)
	}
}

func (e *vendorGrantEnv) requireNoRoute(label string, r *routing.Resolver, principal auth.Token, model string) {
	e.t.Helper()
	got, err := e.resolvesTo(r, principal, model)
	if !errors.Is(err, routing.ErrNoModelRoute) {
		e.t.Errorf("%s: Resolve(%s) = (%q, %v), want ErrNoModelRoute", label, model, got, err)
	}
}

var (
	allOwnerAccounts = []string{grantShared, grantFull, grantStale, grantUnlinked, grantOtherRG, grantNoProvRG, grantSub, grantPrivate, grantDup}
	grantedShared    = []string{grantShared, grantFull, grantDup}
)

// TestVendorAccountsForRoutingOwnerGetsEveryOwnAccountAtEveryLevel: the owner's own
// accounts are returned whatever their level, link state or the sharing flags, id-sorted,
// each once.
func TestVendorAccountsForRoutingOwnerGetsEveryOwnAccountAtEveryLevel(t *testing.T) {
	e := newVendorGrantEnv(t)
	owner := userPrincipal(grantOwner)

	e.requireRoutable("sharing on", e.svc, owner, allOwnerAccounts...)
	e.flags(false, false)
	e.requireRoutable("master flag off", e.svc, owner, allOwnerAccounts...)
	e.flags(true, true)
	e.requireRoutable("subscription sharing on", e.svc, owner, allOwnerAccounts...)

	// An owner who is ALSO provisioned into the group their shared accounts are linked
	// into still gets each account once, as their own.
	e.flags(true, false)
	e.mustProvisionResourceGroup(e.rgGranted.ID, routing.ProvisionKindUser, grantOwner)
	e.requireRoutable("owner provisioned into the group", e.svc, owner, allOwnerAccounts...)
}

// TestVendorAccountsForRoutingProvisionedPrincipalsGetTheSharedAccounts: every way a
// principal can be provisioned into the group (a direct user provision, an admin-group
// membership, a user-group membership, a direct service provision) reaches exactly the
// live shared/full accounts linked there, own block first, then shared, each block
// id-sorted, an account linked into two granted groups once.
func TestVendorAccountsForRoutingProvisionedPrincipalsGetTheSharedAccounts(t *testing.T) {
	e := newVendorGrantEnv(t)

	// The direct user owns an account whose id sorts AFTER every shared id: the own
	// block still comes first (the precedence rule is the order of the list).
	e.requireRoutable("direct user provision", e.svc, userPrincipal(grantDirect),
		append([]string{grantOwnAccount}, grantedShared...)...)
	e.requireRoutable("admin-group member", e.svc, userPrincipal(grantAdminMember), grantedShared...)
	e.requireRoutable("user-group member", e.svc, userPrincipal(grantUserGroupMember), grantedShared...)
	e.requireRoutable("service, direct provision", e.svc, servicePrincipal(grantServiceOK), grantedShared...)
}

// TestVendorAccountsForRoutingNonProvisionedPrincipalsGetNothing: an unprovisioned user,
// an INVITED (not member) admin-group user, an unprovisioned service and a principal with
// no identity are granted nothing, however shared the accounts are.
func TestVendorAccountsForRoutingNonProvisionedPrincipalsGetNothing(t *testing.T) {
	e := newVendorGrantEnv(t)

	e.requireRoutable("unprovisioned user", e.svc, userPrincipal(grantStranger))
	e.requireRoutable("invited, not a member of AG", e.svc, userPrincipal(grantInvited))
	e.requireRoutable("unprovisioned service", e.svc, servicePrincipal(grantServiceOther))
	e.requireRoutable("no user and no service identity", e.svc, auth.Token{ID: "tok_empty", Active: true})
	e.requireRoutable("service token without a service id", e.svc, auth.Token{ID: "tok_svc", Kind: "service", Active: true})
}

// TestVendorAccountsForRoutingServiceMatchesOnlyADirectServiceProvision: a service is in
// no management/user group, so the group provisions never reach it, and a provision of
// another KIND naming the service's id is not a service provision.
func TestVendorAccountsForRoutingServiceMatchesOnlyADirectServiceProvision(t *testing.T) {
	e := newVendorGrantEnv(t)

	// A service whose id happens to equal an id provisioned under another kind.
	e.requireRoutable("service id == provisioned admin group id", e.svc, servicePrincipal(e.ag.ID))
	e.requireRoutable("service id == provisioned user group id", e.svc, servicePrincipal(e.ug.ID))
	e.requireRoutable("service id == provisioned user id", e.svc, servicePrincipal(grantDirect))
	// A user whose id equals the provisioned SERVICE id is not that service.
	e.createUser(grantServiceOK, "user")
	e.requireRoutable("user id == provisioned service id", e.svc, userPrincipal(grantServiceOK))
}

// TestVendorAccountsForRoutingStaleLinkGrantsNothing is the Review-Focus "stale link"
// case: a link's existence is necessary, not sufficient. The account's level is read
// LIVE, in the same pass, and `none` grants nothing to a non-owner even when the group
// link and the provisioning both stand.
func TestVendorAccountsForRoutingStaleLinkGrantsNothing(t *testing.T) {
	e := newVendorGrantEnv(t)
	member := userPrincipal(grantAdminMember)

	if got := e.routable(e.svc, member); slices.Contains(got, grantStale) {
		t.Fatalf("a `none` account with a surviving RG link is routable for a member: %v", got)
	}

	t.Run("a live downgrade takes effect on the next call", func(t *testing.T) {
		e.setLevel(grantShared, routing.VendorShareLevelNone)
		e.requireRoutable("after lowering to none", e.svc, member, grantFull, grantDup)
		e.setLevel(grantShared, routing.VendorShareLevelShared)
		e.requireRoutable("after raising again", e.svc, member, grantedShared...)
	})

	t.Run("an unrecognised level is private", func(t *testing.T) {
		e.setLevel(grantFull, "bogus")
		e.requireRoutable("bogus level", e.svc, member, grantShared, grantDup)
		e.setLevel(grantFull, routing.VendorShareLevelFull)
	})

	t.Run("the owner keeps a stale account", func(t *testing.T) {
		if got := e.routable(e.svc, userPrincipal(grantOwner)); !slices.Contains(got, grantStale) {
			t.Fatalf("the owner lost their own `none` account: %v", got)
		}
	})
}

// TestVendorAccountsForRoutingMasterFlagPausesTheSharedArm: with
// vendor_account_sharing_enabled off every account behaves personal (effective level
// none): the shared arm is empty for a provisioned member, the owner is untouched, and
// switching the flag back on restores the grant (a pause, not an un-share).
func TestVendorAccountsForRoutingMasterFlagPausesTheSharedArm(t *testing.T) {
	e := newVendorGrantEnv(t)
	direct := userPrincipal(grantDirect)

	e.flags(false, true) // subscription sharing on cannot revive a paused master
	e.requireRoutable("master off (member)", e.svc, direct, grantOwnAccount)
	e.requireRoutable("master off (service)", e.svc, servicePrincipal(grantServiceOK))
	e.requireRoutable("master off (owner)", e.svc, userPrincipal(grantOwner), allOwnerAccounts...)

	e.flags(true, false)
	e.requireRoutable("master back on", e.svc, direct, append([]string{grantOwnAccount}, grantedShared...)...)

	t.Run("an unreadable settings store reads as off", func(t *testing.T) {
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: e.routes, SystemSettings: erroringSettings{}, Clock: fixedClock()})
		e.requireRoutable("settings unreadable (member)", svc, direct, grantOwnAccount)
		e.requireRoutable("no settings store (member)", NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: e.routes, Clock: fixedClock()}), direct, grantOwnAccount)
	})
}

// TestVendorAccountsForRoutingSubscriptionNeedsItsOwnFlag: a subscription account counts
// as shared only while vendor_subscription_sharing_enabled is ALSO on. Link creation does
// not look at that flag, so the routing grant must.
func TestVendorAccountsForRoutingSubscriptionNeedsItsOwnFlag(t *testing.T) {
	e := newVendorGrantEnv(t)
	member := userPrincipal(grantAdminMember)

	e.requireRoutable("subscription sharing off", e.svc, member, grantedShared...)
	e.flags(true, true)
	e.requireRoutable("subscription sharing on", e.svc, member, grantShared, grantFull, grantSub, grantDup)
	e.flags(true, false)
	e.requireRoutable("subscription sharing off again", e.svc, member, grantedShared...)

	t.Run("an api-key account is unaffected by the subscription flag", func(t *testing.T) {
		e.flags(true, false)
		if got := e.routable(e.svc, member); !slices.Contains(got, grantShared) {
			t.Fatalf("api-key shared account lost with subscription sharing off: %v", got)
		}
	})

	t.Run("an unrecognised auth type is treated as a subscription", func(t *testing.T) {
		e.account("vacc_g_10_weird", grantOwner, "mystery", routing.VendorShareLevelShared, e.rgGranted.ID)
		e.requireRoutable("unknown auth type, flag off", e.svc, member, grantedShared...)
		e.flags(true, true)
		e.requireRoutable("unknown auth type, flag on", e.svc, member, grantShared, grantFull, grantSub, grantDup, "vacc_g_10_weird")
	})
}

// TestVendorAccountsForRoutingIgnoresTheServerProvisioningModel: the grant has no open
// pool and does not read resource_provisioning_enforce. A shared account in NO group, or
// in a group with NO provisions (a server in such a group would be open to everyone
// under opt-in), is not routable, and the enforce setting changes nothing.
func TestVendorAccountsForRoutingIgnoresTheServerProvisioningModel(t *testing.T) {
	e := newVendorGrantEnv(t)
	stranger := userPrincipal(grantStranger)
	direct := userPrincipal(grantDirect)
	want := append([]string{grantOwnAccount}, grantedShared...)

	for _, enforce := range []bool{false, true} {
		mustSetProvisioningEnforce(e.groupTestEnv, enforce)
		e.requireRoutable("stranger", e.svc, stranger)
		e.requireRoutable("direct", e.svc, direct, want...)
	}
}

// TestVendorAccountsForRoutingSharedArmFailsClosedOwnIsAlwaysReturned: a store error
// while computing the shared candidates denies the shared candidates but STILL returns the
// owner's own accounts, with no error. One consistent fail direction (unlike
// AllowedServerIDs, which fails open).
func TestVendorAccountsForRoutingSharedArmFailsClosedOwnIsAlwaysReturned(t *testing.T) {
	e := newVendorGrantEnv(t)
	direct := userPrincipal(grantDirect)

	cases := []struct {
		name string
		fail failingGrantStore
	}{
		{"provisioned-group lookup fails", failingGrantStore{provisionTargets: true}},
		{"group-linked accounts lookup fails", failingGrantStore{groupAccounts: true}},
		{"account read fails", failingGrantStore{accountByID: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail := tc.fail
			fail.Store = e.routes
			svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
			e.requireRoutable(tc.name, svc, direct, grantOwnAccount)
		})
	}

	t.Run("user-group membership lookup fails", func(t *testing.T) {
		svc := NewService(ServiceDeps{Users: e.dir, Groups: failingUserGroups{GroupStore: e.dir}, Routes: e.routes, SystemSettings: e.svc.settings, Clock: fixedClock()})
		e.requireRoutable("groups unreadable", svc, direct, grantOwnAccount)
	})

	t.Run("a service has no own accounts to keep", func(t *testing.T) {
		fail := failingGrantStore{Store: e.routes, provisionTargets: true}
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
		e.requireRoutable("service, lookup fails", svc, servicePrincipal(grantServiceOK))
	})

	t.Run("a vanished account is skipped, not a failure of the whole arm", func(t *testing.T) {
		fail := failingGrantStore{Store: e.routes, vanished: map[string]bool{grantShared: true}}
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
		e.requireRoutable("one account vanished", svc, direct, grantOwnAccount, grantFull, grantDup)
	})

	t.Run("the own read failing is an error, never a silent empty answer", func(t *testing.T) {
		fail := failingGrantStore{Store: e.routes, owner: true}
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
		if accounts, err := svc.VendorAccountsForRouting(e.ctx, direct); err == nil {
			t.Fatalf("VendorAccountsForRouting = %v, nil; want the own-read error", accountIDs(accounts))
		}
	})
}

// failingGrantStore fails exactly the store calls the grant makes, one switch each.
type failingGrantStore struct {
	routing.Store
	provisionTargets bool
	groupAccounts    bool
	accountByID      bool
	owner            bool
	vanished         map[string]bool
}

var errGrantStore = errors.New("grant store down")

func (f *failingGrantStore) ResourceGroupIDsByProvisionTargets(ctx context.Context, kind string, ids []string) ([]string, error) {
	if f.provisionTargets {
		return nil, errGrantStore
	}
	return f.Store.ResourceGroupIDsByProvisionTargets(ctx, kind, ids)
}

func (f *failingGrantStore) ResourceGroupVendorAccounts(ctx context.Context, rgID string) ([]string, error) {
	if f.groupAccounts {
		return nil, errGrantStore
	}
	return f.Store.ResourceGroupVendorAccounts(ctx, rgID)
}

func (f *failingGrantStore) VendorAccountByID(ctx context.Context, id string) (routing.VendorAccount, error) {
	if f.accountByID {
		return routing.VendorAccount{}, errGrantStore
	}
	if f.vanished[id] {
		return routing.VendorAccount{}, store.ErrNotFound
	}
	return f.Store.VendorAccountByID(ctx, id)
}

func (f *failingGrantStore) VendorAccountsByOwner(ctx context.Context, userID string) ([]routing.VendorAccount, error) {
	if f.owner {
		return nil, errGrantStore
	}
	return f.Store.VendorAccountsByOwner(ctx, userID)
}

// failingUserGroups fails the group-membership read of provisionedRGsForPrincipal.
type failingUserGroups struct{ GroupStore }

func (failingUserGroups) UserGroupsForUser(context.Context, string, string, string) ([]store.UserGroup, error) {
	return nil, errGrantStore
}

// --- End to end through a real routing.Resolver ----------------------------------

// TestVendorGrantResolveOwnerResolvesOwnAccount: the owner reaches their own account at
// every level (a private one included), with sharing on or off.
func TestVendorGrantResolveOwnerResolvesOwnAccount(t *testing.T) {
	e := newVendorGrantEnv(t)
	r := e.resolver(e.svc)
	owner := userPrincipal(grantOwner)

	e.requireResolves("owner, private account", r, owner, grantModel(grantPrivate), grantPrivate)
	e.requireResolves("owner, stale account", r, owner, grantModel(grantStale), grantStale)
	e.requireResolves("owner, shared account", r, owner, grantModel(grantShared), grantShared)
	e.flags(false, false)
	e.requireResolves("owner, master off", r, owner, grantModel(grantShared), grantShared)
}

// TestVendorGrantResolveProvisionedMemberResolvesSharedAccount: a principal provisioned
// into the group resolves the shared account, and the Target carries the owner's SEALED
// credential for the dispatch edge to open; a non-provisioned principal does not.
func TestVendorGrantResolveProvisionedMemberResolvesSharedAccount(t *testing.T) {
	e := newVendorGrantEnv(t)
	r := e.resolver(e.svc)
	model := grantModel(grantShared)

	for name, principal := range map[string]auth.Token{
		"direct user":        userPrincipal(grantDirect),
		"admin-group member": userPrincipal(grantAdminMember),
		"user-group member":  userPrincipal(grantUserGroupMember),
		"service (direct)":   servicePrincipal(grantServiceOK),
	} {
		e.requireResolves(name, r, principal, model, grantShared)
	}
	e.requireResolves("full account", r, userPrincipal(grantAdminMember), grantModel(grantFull), grantFull)

	target, err := r.Resolve(e.ctx, userPrincipal(grantAdminMember), inference.Request{Model: model, APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.APIToken != "enc:key-"+grantShared {
		t.Errorf("APIToken = %q, want the owner's sealed key carried verbatim to dispatch", target.APIToken)
	}

	e.requireNoRoute("unprovisioned user", r, userPrincipal(grantStranger), model)
	e.requireNoRoute("invited, not a member", r, userPrincipal(grantInvited), model)
	e.requireNoRoute("unprovisioned service", r, servicePrincipal(grantServiceOther), model)
	e.requireNoRoute("service whose id is a provisioned admin-group id", r, servicePrincipal(e.ag.ID), model)
}

// TestVendorGrantResolveStaleLinkNeverResolvesForANonOwner is the Review-Focus
// "stale link" case through the resolver: a `none`/paused account with a surviving RG
// link, an account in a group nobody is provisioned into, one in no group and one in a
// group with no provisions never resolve for a non-owner, and the master flag off
// (effective none) collapses the whole shared arm.
func TestVendorGrantResolveStaleLinkNeverResolvesForANonOwner(t *testing.T) {
	e := newVendorGrantEnv(t)
	r := e.resolver(e.svc)
	member := userPrincipal(grantDirect)

	for name, id := range map[string]string{
		"none with a surviving link":     grantStale,
		"shared, in no group":            grantUnlinked,
		"shared, group not provisioned":  grantOtherRG,
		"shared, group has no provision": grantNoProvRG,
		"private, no link":               grantPrivate,
		"subscription, flag off":         grantSub,
	} {
		e.requireNoRoute(name, r, member, grantModel(id))
	}

	t.Run("master flag off pauses a shared account; on restores it", func(t *testing.T) {
		model := grantModel(grantShared)
		e.requireResolves("sharing on", r, member, model, grantShared)
		e.flags(false, false)
		e.requireNoRoute("sharing off", r, member, model)
		e.flags(true, false)
		e.requireResolves("sharing back on", r, member, model, grantShared)
	})

	t.Run("lowering to none stops routing on the very next request", func(t *testing.T) {
		model := grantModel(grantFull)
		e.requireResolves("full", r, member, model, grantFull)
		e.setLevel(grantFull, routing.VendorShareLevelNone)
		e.requireNoRoute("lowered to none, link intact", r, member, model)
	})

	t.Run("subscription sharing on makes a shared subscription routable", func(t *testing.T) {
		e.flags(true, true)
		e.requireResolves("subscription, flag on", r, member, grantModel(grantSub), grantSub)
		e.flags(true, false)
		e.requireNoRoute("subscription, flag off", r, member, grantModel(grantSub))
	})
}

// TestVendorGrantResolveOwnWinsANameCollision: the own-block-first order makes the
// principal's OWN account win a name collision even when a shared account's id sorts
// first; among shared accounts the first active by id wins.
func TestVendorGrantResolveOwnWinsANameCollision(t *testing.T) {
	e := newVendorGrantEnv(t)
	r := e.resolver(e.svc)

	// grantDirect owns grantOwnAccount (id sorts after every shared id) and is
	// granted grantShared/grantFull/grantDup, all serving grantDupModel.
	e.requireResolves("own beats shared", r, userPrincipal(grantDirect), grantDupModel, grantOwnAccount)
	// With no own account the first shared account by id wins.
	e.requireResolves("first shared by id", r, userPrincipal(grantAdminMember), grantDupModel, grantShared)

	t.Run("an inactive first shared account falls through to the next", func(t *testing.T) {
		acc, err := e.routes.VendorAccountByID(e.ctx, grantShared)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		acc.Status = routing.VendorAccountStatusDisabled
		if err := e.routes.UpdateVendorAccount(e.ctx, acc); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}
		e.requireResolves("first shared disabled", r, userPrincipal(grantAdminMember), grantDupModel, grantFull)
	})

	t.Run("a lowered first shared account falls through to the next", func(t *testing.T) {
		e.setLevel(grantFull, routing.VendorShareLevelNone)
		e.requireResolves("second lowered too", r, userPrincipal(grantAdminMember), grantDupModel, grantDup)
	})
}

// TestVendorGrantResolveEmptyPrincipalIsDenied is the Review-Focus "eligibility" case
// end to end: a principal with neither a user nor a service identity resolves nothing,
// with sharing on and the accounts shared, while a service token with a service id and a
// direct service provision does resolve.
func TestVendorGrantResolveEmptyPrincipalIsDenied(t *testing.T) {
	e := newVendorGrantEnv(t)
	r := e.resolver(e.svc)
	model := grantModel(grantShared)

	e.requireNoRoute("no user, no service", r, auth.Token{ID: "tok_empty", Active: true, VendorAccess: auth.VendorAccess{All: true}}, model)
	e.requireNoRoute("service token, no service id", r, auth.Token{ID: "tok_svc", Kind: "service", Active: true, VendorAccess: auth.VendorAccess{All: true}}, model)
	e.requireResolves("service token with a provisioned service id", r, servicePrincipal(grantServiceOK), model, grantShared)
}

// TestVendorGrantResolveSharedArmFailsClosedKeepsOwn: when the shared arm cannot be
// computed the resolver still routes the principal's OWN account and routes no shared
// one; when the own read itself fails the resolver reports the store error.
func TestVendorGrantResolveSharedArmFailsClosedKeepsOwn(t *testing.T) {
	e := newVendorGrantEnv(t)
	direct := userPrincipal(grantDirect)

	fail := failingGrantStore{Store: e.routes, groupAccounts: true}
	svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
	r := e.resolver(svc)
	e.requireResolves("own account survives", r, direct, grantModel(grantOwnAccount), grantOwnAccount)
	e.requireNoRoute("shared account denied", r, direct, grantModel(grantShared))

	ownFail := failingGrantStore{Store: e.routes, owner: true}
	r = e.resolver(NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &ownFail, SystemSettings: e.svc.settings, Clock: fixedClock()}))
	if _, err := e.resolvesTo(r, direct, grantModel(grantOwnAccount)); err == nil || errors.Is(err, routing.ErrNoModelRoute) {
		t.Fatalf("Resolve with the own read failing = %v, want the store error", err)
	}
}
