// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
)

// This file pins the PER-TOKEN OPT-IN for shared vendor accounts (#183, spec
// 2026-10-15 sections 2.3 and 8). The routing grant (VendorAccountsForRouting)
// decides REACH; the token's vendor_provider_access policy decides USE, and for a
// SHARED account (one the token's principal does not own) the policy must NAME the
// account: `all:true`, and so a portal session, covers the principal's OWN accounts
// only and never auto-borrows a shared one.
//
// Everything below runs against the grant fixture (newVendorListingEnv) and a REAL
// routing.Resolver wired to the same service, so a pass here is what the gateway
// would actually route and advertise.

// allVendorAccounts is the policy of a token that opted into "all my accounts".
func allVendorAccounts(p auth.Token) auth.Token {
	p.VendorAccess = auth.VendorAccess{All: true}
	return p
}

// optedInTo is p with exactly the named accounts opted in by id, under their native
// prefixes: the explicit per-account entries a shared account needs.
func optedInTo(p auth.Token, accountIDs ...string) auth.Token {
	entries := make([]auth.VendorAccessEntry, 0, len(accountIDs))
	for _, id := range accountIDs {
		entries = append(entries, auth.VendorAccessEntry{AccountID: id})
	}
	p.VendorAccess = auth.VendorAccess{Accounts: entries}
	return p
}

// tokenService is a Service over the fixture's directory, routing store and settings
// that can also create tokens (the fixture's own service has no token store).
func (e *vendorGrantEnv) tokenService() *Service {
	return NewService(ServiceDeps{
		Users: e.dir, Tokens: e.dir, Groups: e.dir, Routes: e.routes,
		SystemSettings: e.svc.settings, Clock: fixedClock(),
	})
}

// requireAdvertisedNames asserts the fixture model names the listing advertises to
// principal on the openai dialect are exactly want.
func (e *vendorGrantEnv) requireAdvertisedNames(label string, svc *Service, principal auth.Token, want []string) {
	e.t.Helper()
	if got := e.advertised(svc, principal, routing.APIFlavorOpenAI); !slices.Equal(got, want) {
		e.t.Errorf("%s: advertised %v, want %v", label, got, want)
	}
}

// TestVendorOptInAllTrueNeverAutoBorrowsASharedAccount is the Review-Focus case: an
// all:true token covers the principal's OWN accounts and NOTHING shared, even a shared
// account the principal is fully provisioned into. The resolver and the listing agree.
func TestVendorOptInAllTrueNeverAutoBorrowsASharedAccount(t *testing.T) {
	e := newVendorListingEnv(t)
	r := e.liveResolver(e.svc)

	t.Run("a principal with an own account keeps it and borrows no shared one", func(t *testing.T) {
		direct := allVendorAccounts(userPrincipal(grantDirect)) // owns grantOwnAccount, provisioned into every granted account
		e.requireResolves("own account, all:true", r, direct, grantModel(grantOwnAccount), grantOwnAccount)
		for _, id := range grantedShared {
			e.requireNoRoute("shared "+id+", all:true", r, direct, grantModel(id))
		}
		e.requireAdvertisedNames("all:true direct", e.svc, direct, fixtureModelNames(grantOwnAccount))
		e.requireAdvertisedEqualsRoutable("all:true direct", e.svc, direct)
	})

	t.Run("a principal that owns nothing is granted nothing by all:true", func(t *testing.T) {
		for name, principal := range map[string]auth.Token{
			"admin-group member": allVendorAccounts(userPrincipal(grantAdminMember)),
			"user-group member":  allVendorAccounts(userPrincipal(grantUserGroupMember)),
			"service token":      allVendorAccounts(servicePrincipal(grantServiceOK)),
		} {
			for _, id := range grantedShared {
				e.requireNoRoute(name+" "+id, r, principal, grantModel(id))
			}
			e.requireNoRoute(name+" dup", r, principal, grantDupModel)
			e.requireAdvertisedNames(name, e.svc, principal, nil)
			e.requireAdvertisedEqualsRoutable(name, e.svc, principal)
		}
	})

	t.Run("the owner's all:true still covers every own account at every level", func(t *testing.T) {
		owner := allVendorAccounts(userPrincipal(grantOwner))
		for _, id := range allOwnerAccounts {
			e.requireResolves("owner "+id, r, owner, grantModel(id), id)
		}
	})

	t.Run("all:true plus an explicit entry reaches exactly the named shared account", func(t *testing.T) {
		direct := userPrincipal(grantDirect)
		direct.VendorAccess = auth.VendorAccess{All: true, Accounts: []auth.VendorAccessEntry{{AccountID: grantShared}}}
		e.requireResolves("own, all:true", r, direct, grantModel(grantOwnAccount), grantOwnAccount)
		e.requireResolves("named shared", r, direct, grantModel(grantShared), grantShared)
		e.requireNoRoute("reachable but not named", r, direct, grantModel(grantFull))
		e.requireNoRoute("reachable but not named", r, direct, grantModel(grantDup))
		e.requireAdvertisedEqualsRoutable("all:true plus a named shared account", e.svc, direct)
	})
}

// TestVendorOptInExplicitEntryReachesAReachableSharedAccount: a token that NAMES a
// reachable shared account uses it, and only it; naming an account the grant does not
// reach is harmless because routing re-checks (the grant is the gate).
func TestVendorOptInExplicitEntryReachesAReachableSharedAccount(t *testing.T) {
	e := newVendorListingEnv(t)
	r := e.liveResolver(e.svc)
	member := userPrincipal(grantAdminMember)

	t.Run("a named reachable account routes, an unnamed reachable one does not", func(t *testing.T) {
		p := optedInTo(member, grantShared)
		e.requireResolves("named", r, p, grantModel(grantShared), grantShared)
		e.requireResolves("the name every account serves, via the named one", r, p, grantDupModel, grantShared)
		e.requireNoRoute("reachable, not named (full)", r, p, grantModel(grantFull))
		e.requireNoRoute("reachable, not named (dup)", r, p, grantModel(grantDup))
		e.requireAdvertisedEqualsRoutable("named shared", e.svc, p)
	})

	t.Run("a service token opts in the same way", func(t *testing.T) {
		p := optedInTo(servicePrincipal(grantServiceOK), grantFull)
		e.requireResolves("service, named", r, p, grantModel(grantFull), grantFull)
		e.requireNoRoute("service, not named", r, p, grantModel(grantShared))
		e.requireAdvertisedEqualsRoutable("service, named", e.svc, p)
	})

	t.Run("naming an account the grant does not reach routes nothing", func(t *testing.T) {
		for name, id := range map[string]string{
			"none with a surviving link":     grantStale,
			"shared, in no group":            grantUnlinked,
			"shared, group not provisioned":  grantOtherRG,
			"shared, group has no provision": grantNoProvRG,
			"private, no link":               grantPrivate,
			"subscription, flag off":         grantSub,
		} {
			p := optedInTo(member, id)
			e.requireNoRoute(name, r, p, grantModel(id))
			e.requireAdvertisedNames(name, e.svc, p, nil)
		}
		stranger := optedInTo(userPrincipal(grantStranger), grantShared)
		e.requireNoRoute("unprovisioned user naming a reachable account", r, stranger, grantModel(grantShared))
		e.requireNoRoute("unprovisioned service naming a reachable account", r, optedInTo(servicePrincipal(grantServiceOther), grantShared), grantModel(grantShared))
	})

	t.Run("pausing sharing or lowering the account revokes a named account at once", func(t *testing.T) {
		p := optedInTo(member, grantShared)
		e.requireResolves("sharing on", r, p, grantModel(grantShared), grantShared)
		e.flags(false, false)
		e.requireNoRoute("sharing off", r, p, grantModel(grantShared))
		e.requireAdvertisedNames("sharing off", e.svc, p, nil)
		e.flags(true, false)
		e.requireResolves("sharing back on", r, p, grantModel(grantShared), grantShared)
		e.setLevel(grantShared, routing.VendorShareLevelNone)
		e.requireNoRoute("lowered to none", r, p, grantModel(grantShared))
	})
}

// TestVendorOptInPrefixOverrideOnASharedAccount: the optional per-account prefix
// override works for a shared account exactly as for an own one -- the account is
// served under the override (possibly empty) and not under its native name.
func TestVendorOptInPrefixOverrideOnASharedAccount(t *testing.T) {
	e := newVendorListingEnv(t)
	r := e.liveResolver(e.svc)
	member := userPrincipal(grantAdminMember)
	model := grantModel(grantShared)

	overridden := member
	overridden.VendorAccess = auth.VendorAccess{Accounts: []auth.VendorAccessEntry{
		{AccountID: grantShared, OverrideEnabled: true, OverridePrefix: "team/"},
	}}
	e.requireResolves("override prefix", r, overridden, "team/"+model, grantShared)
	e.requireNoRoute("native name under an override", r, overridden, model)
	if got := e.svc.ModelsForFlavor(e.ctx, overridden, routing.APIFlavorOpenAI); !slices.Contains(got, "team/"+model) || slices.Contains(got, model) {
		t.Errorf("advertised %v, want %q and not the native %q", got, "team/"+model, model)
	}

	bare := member
	bare.VendorAccess = auth.VendorAccess{Accounts: []auth.VendorAccessEntry{
		{AccountID: grantShared, OverrideEnabled: true, OverridePrefix: ""},
	}}
	e.requireResolves("empty override serves the bare upstream name", r, bare, model, grantShared)
}

// TestVendorOptInSaveTimeReachIsComputedAgainstTheTokenOwner: a token policy may NAME a
// reachable shared account. Reach is asked of the TOKEN's own principal (the owning
// user), so an unprovisioned user cannot name an account and a provisioned one can. The
// check is advisory (routing is the gate), but it keeps unreachable ids out of new
// policies exactly as an unknown own id has always been refused.
func TestVendorOptInSaveTimeReachIsComputedAgainstTheTokenOwner(t *testing.T) {
	e := newVendorListingEnv(t)
	svc := e.tokenService()
	ctx := context.Background()
	scopes := []string{"gateway:use"}
	member := auth.Token{UserID: grantAdminMember, Scopes: scopes}
	stranger := auth.Token{UserID: grantStranger, Scopes: scopes}
	named := func(ids ...string) *VendorAccessDTO {
		d := &VendorAccessDTO{}
		for _, id := range ids {
			d.Accounts = append(d.Accounts, VendorAccessEntryDTO{AccountID: id})
		}
		return d
	}

	t.Run("a provisioned user can name a reachable shared account", func(t *testing.T) {
		resp, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "named", Scopes: scopes, VendorAccess: named(grantShared)})
		if err != nil {
			t.Fatalf("CreateToken(named shared): %v", err)
		}
		if got := resp.Token.VendorAccess; got == nil || len(got.Accounts) != 1 || got.Accounts[0].AccountID != grantShared {
			t.Fatalf("stored policy = %+v, want the named shared account", got)
		}
	})

	t.Run("a prefix override on a shared account is validated and stored", func(t *testing.T) {
		d := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: grantFull, PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: " team/ "}}}}
		resp, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "override", Scopes: scopes, VendorAccess: d})
		if err != nil {
			t.Fatalf("CreateToken(override): %v", err)
		}
		rec, err := e.dir.TokenByID(ctx, resp.Token.ID)
		if err != nil {
			t.Fatalf("TokenByID: %v", err)
		}
		if rec.VendorProviderAccess != `{"accounts":[{"account_id":"`+grantFull+`","prefix_override":{"enabled":true,"value":"team/"}}]}` {
			t.Fatalf("stored column = %s", rec.VendorProviderAccess)
		}
		bad := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: grantFull, PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "no spaces allowed"}}}}
		if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "bad-override", Scopes: scopes, VendorAccess: bad}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Fatalf("CreateToken(bad override on a shared account) = %v, want ErrTokenVendorAccessInvalid", err)
		}
	})

	t.Run("two shared accounts that would offer one public name conflict", func(t *testing.T) {
		// grantShared and grantFull both serve grantDupModel under the empty native prefix.
		_, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "clash", Scopes: scopes, VendorAccess: named(grantShared, grantFull)})
		if !errors.Is(err, ErrTokenVendorAccessConflict) {
			t.Fatalf("CreateToken(colliding shared accounts) = %v, want ErrTokenVendorAccessConflict", err)
		}
	})

	t.Run("an unprovisioned user cannot name the same account", func(t *testing.T) {
		_, err := svc.CreateToken(ctx, stranger, CreateTokenRequest{Name: "denied", Scopes: scopes, VendorAccess: named(grantShared)})
		if !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Fatalf("CreateToken(unreachable) = %v, want ErrTokenVendorAccessInvalid", err)
		}
	})

	t.Run("an account the grant does not reach is refused like an unknown id", func(t *testing.T) {
		for name, id := range map[string]string{
			"none with a surviving link":    grantStale,
			"shared, in no group":           grantUnlinked,
			"shared, group not provisioned": grantOtherRG,
			"private, no link":              grantPrivate,
			"subscription, flag off":        grantSub,
		} {
			if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "u-" + id, Scopes: scopes, VendorAccess: named(id)}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
				t.Errorf("%s: err = %v, want ErrTokenVendorAccessInvalid", name, err)
			}
		}
	})

	t.Run("pausing sharing makes a shared account unnameable", func(t *testing.T) {
		// svc is a second Service over the fixture's settings store, so e.flags drops
		// only e.svc's cached sharing switches: standing in for the settings PUT, which
		// invalidates the one Service of a real process, svc's cache is dropped by hand.
		flip := func(sharing bool) {
			e.flags(sharing, false)
			svc.invalidateVendorSharingFlags()
		}
		flip(false)
		defer flip(true)
		if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "paused", Scopes: scopes, VendorAccess: named(grantShared)}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Fatalf("CreateToken(sharing paused) = %v, want ErrTokenVendorAccessInvalid", err)
		}
	})

	t.Run("an update may name a shared account too", func(t *testing.T) {
		created, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "to-update", Scopes: scopes})
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		dto, err := svc.UpdateToken(ctx, member, created.Token.ID, UpdateTokenRequest{VendorAccess: named(grantDup)})
		if err != nil {
			t.Fatalf("UpdateToken(named shared): %v", err)
		}
		if dto.VendorAccess == nil || len(dto.VendorAccess.Accounts) != 1 || dto.VendorAccess.Accounts[0].AccountID != grantDup {
			t.Fatalf("policy after update = %+v", dto.VendorAccess)
		}
		if _, err := svc.UpdateToken(ctx, stranger, created.Token.ID, UpdateTokenRequest{VendorAccess: named(grantDup)}); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("UpdateToken by a non-owner = %v, want ErrTokenNotFound", err)
		}
	})

	t.Run("the owner's own accounts are still nameable", func(t *testing.T) {
		own := auth.Token{UserID: grantDirect, Scopes: scopes}
		if _, err := svc.CreateToken(ctx, own, CreateTokenRequest{Name: "own", Scopes: scopes, VendorAccess: named(grantOwnAccount)}); err != nil {
			t.Fatalf("CreateToken(own): %v", err)
		}
		// Own and shared together: both serve grantDupModel, so the shared one needs
		// its own prefix (the collision check spans own and shared accounts alike).
		_, err := svc.CreateToken(ctx, own, CreateTokenRequest{Name: "own-clash", Scopes: scopes, VendorAccess: named(grantOwnAccount, grantShared)})
		if !errors.Is(err, ErrTokenVendorAccessConflict) {
			t.Fatalf("CreateToken(own + shared, same names) = %v, want ErrTokenVendorAccessConflict", err)
		}
		both := &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: grantOwnAccount},
			{AccountID: grantShared, PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "shared/"}},
		}}
		if _, err := svc.CreateToken(ctx, own, CreateTokenRequest{Name: "own-and-shared", Scopes: scopes, VendorAccess: both}); err != nil {
			t.Fatalf("CreateToken(own + shared under a prefix): %v", err)
		}
	})
}

// TestVendorOptInAllTrueSaveTimeValidatesOnlyNonOwnEntries: with all=true the account
// list stays ignored for the principal's OWN accounts (stored unvalidated, as before),
// but an entry naming an account the principal does NOT own is a live opt-in to a
// shared account, so it is held to the same reach check as under all=false.
func TestVendorOptInAllTrueSaveTimeValidatesOnlyNonOwnEntries(t *testing.T) {
	e := newVendorListingEnv(t)
	svc := e.tokenService()
	ctx := context.Background()
	scopes := []string{"gateway:use"}
	member := auth.Token{UserID: grantDirect, Scopes: scopes} // owns grantOwnAccount, provisioned into the granted group
	all := func(ids ...string) *VendorAccessDTO {
		d := &VendorAccessDTO{All: true}
		for _, id := range ids {
			d.Accounts = append(d.Accounts, VendorAccessEntryDTO{AccountID: id})
		}
		return d
	}

	if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "all-shared", Scopes: scopes, VendorAccess: all(grantShared)}); err != nil {
		t.Fatalf("all=true naming a reachable shared account: %v", err)
	}
	if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "all-own", Scopes: scopes, VendorAccess: all(grantOwnAccount)}); err != nil {
		t.Fatalf("all=true naming an own account (ignored): %v", err)
	}
	for name, id := range map[string]string{
		"an unreachable shared account": grantUnlinked,
		"an unknown id":                 "vacc_does_not_exist",
	} {
		if _, err := svc.CreateToken(ctx, member, CreateTokenRequest{Name: "all-" + id, Scopes: scopes, VendorAccess: all(id)}); !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Errorf("all=true naming %s: err = %v, want ErrTokenVendorAccessInvalid", name, err)
		}
	}
}
