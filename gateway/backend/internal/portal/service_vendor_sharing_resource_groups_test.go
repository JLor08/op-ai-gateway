// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"cmp"
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strconv"
	"testing"
)

// This file is the resource-group link contract of shared vendor accounts (#183, spec
// 2026-10-15 sections 6, 7 and 9): the two paths that put a shared account into a
// resource group, which is what makes it reachable to the group's members once the
// routing grant lands. Both mirror resource_group_servers.
//
//   - OWNER SELF-SERVICE (VendorAccountResourceGroups, AddVendorAccountToResourceGroup,
//     RemoveVendorAccountFromResourceGroup) mirrors AddServerToResourceGroup: the
//     account's OWNER, at `shared` or `full`, joins and leaves a resource group linked
//     to an admin group the owner is a MEMBER of, when the group's root is the account's.
//   - The RG-MANAGER side (SetResourceGroupVendorAccounts and its candidates) mirrors
//     SetResourceGroupServers: the caller manages the resource group
//     (authorizeResourceGroup) AND operates the account (authorizeVendorAccountOperate),
//     the account is `full` (at `shared` only the owner's self-service links it), and
//     the roots match.
//
// Every method is a 409 while vendor_account_sharing_enabled is off, before anything
// else. System scope never bypasses the account side of either path.

// vendorRGEnv is the lifecycle fixture plus resource groups (all of them in the root SG
// unless named otherwise):
//
//	rgm    linked to AG and AG-B  -- the happy-path group; usr_agowner (owner of AG), the
//	                                 admin owner (owner of AG-B) and usr_cmx (can_manage_resources
//	                                 in AG) manage it
//	rgA    linked to AG-B         -- sorts before rgm ("Alpha" < "Beta")
//	rgOther linked to AG2 only    -- the admin owner is no member of AG2, so cannot reach it
//	rgX    linked to AG-X (SG2)   -- reachable and managed by the admin owner, but another root
type vendorRGEnv struct {
	*vendorLifecycleEnv
	rgm, rgA, rgOther, rgX routing.ResourceGroup
	rgSeq                  int
}

func newVendorRGEnv(t *testing.T) *vendorRGEnv {
	t.Helper()
	e := &vendorRGEnv{vendorLifecycleEnv: newVendorLifecycleEnv(t)}
	ag2 := e.mustCreateGroup(token("usr_agowner2", "admin"), CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2-rg", ParentGroupID: e.rootID})
	e.rgm = e.newRG("rgrp_vrg_m", "Beta", e.rootID, e.ag.ID, e.agB.ID)
	e.rgA = e.newRG("rgrp_vrg_a", "Alpha", e.rootID, e.agB.ID)
	e.rgOther = e.newRG("rgrp_vrg_o", "Other", e.rootID, ag2.ID)
	e.rgX = e.newRG("rgrp_vrg_x", "Gamma", e.otherRootID, e.agX.ID)
	return e
}

func (e *vendorRGEnv) newRG(id, name, root string, adminGroups ...string) routing.ResourceGroup {
	e.t.Helper()
	rg := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: id, Name: name, Status: routing.ServerStatusActive, SystemGroupID: root, CreatedAt: e.now, UpdatedAt: e.now,
	})
	for _, gid := range adminGroups {
		e.mustLinkResourceGroupAdminGroup(rg.ID, gid)
	}
	return rg
}

// freshRG is a new resource group in the root SG linked to AG and AG-B, like rgm.
func (e *vendorRGEnv) freshRG() routing.ResourceGroup {
	e.t.Helper()
	e.rgSeq++
	return e.newRG("rgrp_vrg_f"+strconv.Itoa(e.rgSeq), "Fresh "+strconv.Itoa(e.rgSeq), e.rootID, e.ag.ID, e.agB.ID)
}

func (e *vendorRGEnv) rgAccounts(rgID string) []string {
	e.t.Helper()
	ids, err := e.routes.ResourceGroupVendorAccounts(e.ctx, rgID)
	if err != nil {
		e.t.Fatalf("ResourceGroupVendorAccounts(%s): %v", rgID, err)
	}
	slices.Sort(ids)
	return ids
}

func (e *vendorRGEnv) requireRGAccounts(label, rgID string, want ...string) {
	e.t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	if got := e.rgAccounts(rgID); !slices.Equal(got, want) {
		e.t.Errorf("%s: accounts linked into %s = %v, want %v", label, rgID, got, want)
	}
}

// link puts the link straight into the store, as the fixture of a later step.
func (e *vendorRGEnv) link(rgID, accountID string) {
	e.t.Helper()
	if err := e.routes.SetResourceGroupVendorAccount(e.ctx, rgID, accountID); err != nil {
		e.t.Fatalf("SetResourceGroupVendorAccount(%s, %s): %v", rgID, accountID, err)
	}
}

// accountAtRoot is sharedAccountAt for a root other than the fixture's own: the account
// is put at level under root, assigned to group.
func (e *vendorRGEnv) accountAtRoot(level, root, group string) routing.VendorAccount {
	e.t.Helper()
	acc := e.sharedAccountAt(level)
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, acc.ID, root); err != nil {
		e.t.Fatalf("set root of %s: %v", acc.ID, err)
	}
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, group); err != nil {
		e.t.Fatalf("assign %s: %v", acc.ID, err)
	}
	return e.reload(acc.ID)
}

// --- owner self-service: who may, at which level -----------------------------------

// ownerSelfServiceCalls are the three owner self-service methods, each as a call that
// reports only its error, so one matrix can drive them all against one resource group.
func (e *vendorRGEnv) ownerSelfServiceCalls(rgID string) map[string]func(p auth.Token, accID string) error {
	return map[string]func(p auth.Token, accID string) error{
		"list": func(p auth.Token, accID string) error {
			_, err := e.svc.VendorAccountResourceGroups(e.ctx, p, accID)
			return err
		},
		"add": func(p auth.Token, accID string) error {
			return e.svc.AddVendorAccountToResourceGroup(e.ctx, p, accID, rgID)
		},
		"remove": func(p auth.Token, accID string) error {
			return e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, p, accID, rgID)
		},
	}
}

// makeMemberOfAG makes userID a plain member of AG (and of its parent SG, which AG
// membership needs).
func (e *vendorRGEnv) makeMemberOfAG(userID string) {
	e.t.Helper()
	e.mustAddMembers(token("usr_s", "system", "admin"), e.rootID, userID)
	e.mustAddMembers(token("usr_agowner", "admin"), e.ag.ID, userID)
}

// The owner self-service is the OWNER's, at `shared` and `full`: the owner joins and
// leaves a group they are a member of at both levels; a CM of the account who can see
// it is 403 (their path is the resource-group manager side); everyone else -- a
// stranger, a co-manager without the vendor flag, system scope, a principal with no
// user identity -- is the 404 an unknown id gets. At `none` the owner is told there is
// nothing to link (ErrVendorAccountNotShared) and everyone else is a stranger. A denied
// call writes nothing.
func TestVendorAccountResourceGroupSelfServiceAuthorizationMatrix(t *testing.T) {
	e := newVendorRGEnv(t)
	e.makeMemberOfAG(vendorAuthzOwnerID)
	owner := token(vendorAuthzOwnerID)
	principals := []struct {
		name string
		tok  auth.Token
		// want is the answer per level: nil = allowed.
		none, shared, full error
	}{
		{"owner", owner, ErrVendorAccountNotShared, nil, nil},
		{"CM", token("usr_cm", "admin"), ErrVendorAccountNotFound, ErrVendorAccountForbidden, ErrVendorAccountForbidden},
		{"admin-group owner (a CM through the group)", token("usr_agowner", "admin"), ErrVendorAccountNotFound, ErrVendorAccountForbidden, ErrVendorAccountForbidden},
		{"co-manager without the vendor flag", token("usr_cmx", "admin"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"CM of another admin group", token("usr_cm2", "admin"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"stranger", token("usr_stranger"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"system scope", token("usr_s", "system", "admin"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"no user identity", auth.Token{Scopes: []string{"gateway:use"}}, ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
	}
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, p := range principals {
			want := map[string]error{routing.VendorShareLevelNone: p.none, routing.VendorShareLevelShared: p.shared, routing.VendorShareLevelFull: p.full}[level]
			for name, call := range e.ownerSelfServiceCalls(e.rgm.ID) {
				acc := e.newAccountAt(level)
				label := name + " " + level + "/" + p.name
				err := call(p.tok, acc.ID)
				if want == nil {
					if err != nil {
						t.Errorf("%s: err = %v, want allow", label, err)
					}
				} else if !errors.Is(err, want) {
					t.Errorf("%s: err = %v, want %v", label, err, want)
				}
				if want != nil {
					e.requireNoRGLink(label, acc.ID)
				}
			}
		}
	}
}

func (e *vendorRGEnv) requireNoRGLink(label, accID string) {
	e.t.Helper()
	if links := e.resourceGroupLinks(accID); len(links) != 0 {
		e.t.Errorf("%s: resource-group links = %v, want none", label, links)
	}
}

// The account gate runs first: a caller who may not see the account learns nothing
// about the resource group, so a stranger naming an unknown group still gets the
// account's 404, while the owner naming an unknown group gets the group's.
func TestVendorAccountResourceGroupSelfServiceAnswersTheAccountFirst(t *testing.T) {
	e := newVendorRGEnv(t)
	e.makeMemberOfAG(vendorAuthzOwnerID)
	acc := e.newAccountAt(routing.VendorShareLevelShared)

	stranger := token("usr_stranger")
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, stranger, acc.ID, "rgrp_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("stranger, unknown group: err = %v, want ErrVendorAccountNotFound", err)
	}
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, stranger, "va_missing", e.rgm.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("stranger, unknown account: err = %v, want ErrVendorAccountNotFound", err)
	}
	owner := token(vendorAuthzOwnerID)
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, owner, acc.ID, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Errorf("owner, unknown group: err = %v, want ErrResourceGroupNotFound", err)
	}
	if err := e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, owner, acc.ID, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Errorf("owner, unknown group (remove): err = %v, want ErrResourceGroupNotFound", err)
	}
}

// --- owner self-service: the groups an owner may reach -------------------------------

// The owner joins and leaves only a group linked to an admin group they are a MEMBER
// of: a group linked to someone else's admin group is ErrResourceGroupNotFound (the
// 404 of an unknown id), and an owner who is a member of no admin group reaches none.
func TestVendorAccountResourceGroupSelfServiceNeedsMembership(t *testing.T) {
	e := newVendorRGEnv(t)
	owner := token(vendorAuthzOwnerID)
	acc := e.newAccountAt(routing.VendorShareLevelFull)

	// usr_va_owner is a member of no admin group yet: nothing is reachable.
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, owner, acc.ID, e.rgm.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("add as a non-member: err = %v, want ErrResourceGroupNotFound", err)
	}
	if list, err := e.svc.VendorAccountResourceGroups(e.ctx, owner, acc.ID); err != nil || len(list) != 0 {
		t.Fatalf("list as a non-member = %v, %v, want an empty list", list, err)
	}
	e.requireNoRGLink("non-member", acc.ID)

	e.makeMemberOfAG(vendorAuthzOwnerID)
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, owner, acc.ID, e.rgm.ID); err != nil {
		t.Fatalf("add as a member of AG: %v", err)
	}
	e.requireRGAccounts("joined", e.rgm.ID, acc.ID)
	// AG2's group stays out of reach: AG2 is not one of the owner's groups.
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, owner, acc.ID, e.rgOther.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Errorf("add to a group of another admin group: err = %v, want ErrResourceGroupNotFound", err)
	}
	if err := e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, owner, acc.ID, e.rgOther.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Errorf("remove from a group of another admin group: err = %v, want ErrResourceGroupNotFound", err)
	}
	e.requireRGAccounts("other group untouched", e.rgOther.ID)
}

// The list is what the self-service panel shows: the groups the owner may reach AND
// whose root is the account's (an RG under another system group is never offered),
// each with Member = the account is in it, sorted by name; joining flips Member.
func TestVendorAccountResourceGroupsListsReachableGroupsWithTheMemberFlag(t *testing.T) {
	e := newVendorRGEnv(t)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := e.sharedAccountAt(level, e.ag.ID)
		want := func(alphaMember, betaMember bool) []VendorAccountResourceGroupDTO {
			return []VendorAccountResourceGroupDTO{
				{ID: e.rgA.ID, Name: "Alpha", Member: alphaMember},
				{ID: e.rgm.ID, Name: "Beta", Member: betaMember},
			}
		}
		got, err := e.svc.VendorAccountResourceGroups(e.ctx, e.admin, acc.ID)
		if err != nil || !slices.Equal(got, want(false, false)) {
			t.Fatalf("%s: list = %v, %v, want %v (rgX has another root, rgOther is out of reach)", level, got, err, want(false, false))
		}
		if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); err != nil {
			t.Fatalf("%s: join: %v", level, err)
		}
		got, err = e.svc.VendorAccountResourceGroups(e.ctx, e.admin, acc.ID)
		if err != nil || !slices.Equal(got, want(false, true)) {
			t.Fatalf("%s: list after join = %v, %v, want %v", level, got, err, want(false, true))
		}
		if err := e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); err != nil {
			t.Fatalf("%s: leave: %v", level, err)
		}
		got, err = e.svc.VendorAccountResourceGroups(e.ctx, e.admin, acc.ID)
		if err != nil || !slices.Equal(got, want(false, false)) {
			t.Fatalf("%s: list after leave = %v, %v, want %v", level, got, err, want(false, false))
		}
	}
}

// Joining and leaving are idempotent (a PUT and a DELETE), and joining a second group
// leaves the first alone.
func TestVendorAccountResourceGroupSelfServiceIsIdempotent(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	for range 2 {
		if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	e.requireRGAccounts("joined twice", e.rgm.ID, acc.ID)
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgA.ID); err != nil {
		t.Fatalf("join a second group: %v", err)
	}
	for range 2 {
		if err := e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); err != nil {
			t.Fatalf("leave: %v", err)
		}
	}
	e.requireRGAccounts("left twice", e.rgm.ID)
	e.requireRGAccounts("the other group is kept", e.rgA.ID, acc.ID)
}

// An RG is joined only under the account's own root: a group under another system group
// is ErrResourceGroupVendorAccountSystemGroupMismatch (a 400) on JOIN, even though the
// owner reaches it (a member of AG-X). LEAVING needs no root check, like the server
// self-service, so a stale link to another root can always be dropped.
func TestVendorAccountResourceGroupSelfServiceChecksTheRootOnJoinOnly(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgX.ID); !errors.Is(err, ErrResourceGroupVendorAccountSystemGroupMismatch) {
		t.Fatalf("join under another root: err = %v, want ErrResourceGroupVendorAccountSystemGroupMismatch", err)
	}
	e.requireRGAccounts("not joined", e.rgX.ID)

	e.link(e.rgX.ID, acc.ID)
	if err := e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, e.admin, acc.ID, e.rgX.ID); err != nil {
		t.Fatalf("leave a group of another root: %v", err)
	}
	e.requireRGAccounts("stale link dropped", e.rgX.ID)
}

// An account whose stored root is empty (a store written directly; the raise never
// produces it) is never linked: an empty root must not "match" a group's empty root.
func TestVendorAccountRootMatchesFailsClosedOnAnEmptyRoot(t *testing.T) {
	for _, tc := range []struct {
		name         string
		acc, rg      string
		wantMatching bool
	}{
		{"equal roots", "sg_1", "sg_1", true},
		{"different roots", "sg_1", "sg_2", false},
		{"an empty account root against a root", "", "sg_1", false},
		{"an empty account root against an empty one", "", "", false},
		{"a root against an empty group root", "sg_1", "", false},
	} {
		got := vendorAccountRootMatches(routing.VendorAccount{SystemGroupID: tc.acc}, routing.ResourceGroup{SystemGroupID: tc.rg})
		if got != tc.wantMatching {
			t.Errorf("%s: vendorAccountRootMatches = %v, want %v", tc.name, got, tc.wantMatching)
		}
	}
}

// --- the sharing flags ---------------------------------------------------------------

// With the master sharing flag off every resource-group method is a 409
// (ErrVendorAccountSharingDisabled), for the owner, a stranger and an id that does not
// exist alike (the answer is global), and nothing is written; the vendor-accounts MODULE
// flag still answers first. This is the pause: an account that is `shared` in the store
// is not linkable while sharing is paused.
func TestVendorAccountResourceGroupMethodsAreRefusedWhileSharingIsOff(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	e.link(e.rgm.ID, acc.ID)
	setVendorSharingFlags(t, e.svc, false, true)

	calls := map[string]func(p auth.Token, accID, rgID string) error{
		"owner list": func(p auth.Token, accID, _ string) error {
			_, err := e.svc.VendorAccountResourceGroups(e.ctx, p, accID)
			return err
		},
		"owner add": func(p auth.Token, accID, rgID string) error {
			return e.svc.AddVendorAccountToResourceGroup(e.ctx, p, accID, rgID)
		},
		"owner remove": func(p auth.Token, accID, rgID string) error {
			return e.svc.RemoveVendorAccountFromResourceGroup(e.ctx, p, accID, rgID)
		},
		"manager set": func(p auth.Token, accID, rgID string) error {
			_, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, p, rgID, []string{accID})
			return err
		},
		"manager candidates": func(p auth.Token, _, rgID string) error {
			_, err := e.svc.ResourceGroupVendorAccountCandidates(e.ctx, p, rgID)
			return err
		},
	}
	principals := map[string]auth.Token{
		"owner": e.admin, "stranger": token("usr_stranger"), "system": token("usr_s", "system", "admin"),
		"resource-group manager": token("usr_agowner", "admin"),
	}
	check := func(want error) {
		t.Helper()
		for name, call := range calls {
			for who, tok := range principals {
				for _, ids := range [][2]string{{acc.ID, e.rgm.ID}, {"va_missing", "rgrp_missing"}} {
					if err := call(tok, ids[0], ids[1]); !errors.Is(err, want) {
						t.Errorf("%s as %s on %v: err = %v, want %v", name, who, ids, err, want)
					}
				}
			}
		}
	}
	check(ErrVendorAccountSharingDisabled)
	e.requireRGAccounts("calls refused while sharing is off", e.rgm.ID, acc.ID)

	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	check(ErrVendorAccountsDisabled)
}

// --- resource-group manager side ---------------------------------------------------

// SetResourceGroupVendorAccounts adds an account only to a caller who manages the group
// (can_manage_resources) AND operates the account AND the account is `full` AND the roots
// match. Per kind of principal and level, the answer is exactly one of:
//
//   - allowed (the owner who manages the group, or a CM who does: usr_agowner owns AG,
//     which is both a resource-group admin group and the account's management group);
//   - ErrResourceGroupNotFound: the caller does not manage the group (the gate that runs
//     first, so such a caller learns nothing of the account);
//   - ErrVendorAccountNotFound: the caller manages the group but has no operate reach on the
//     account (system scope included: it never bypasses a write), identical to an unknown id;
//   - ErrVendorAccountForbidden: the caller operates a `shared` account but the RG side is
//     `full`-only (they can see it, so "not found" would be a lie);
//   - ErrVendorAccountNotShared: the owner's account is `none`.
func TestSetResourceGroupVendorAccountsAuthorizationMatrix(t *testing.T) {
	e := newVendorRGEnv(t)
	principals := []struct {
		name string
		tok  auth.Token
		// the answer per level: nil = allowed.
		none, shared, full error
	}{
		{"owner who manages the group", e.admin, ErrVendorAccountNotShared, ErrVendorAccountForbidden, nil},
		{"CM who manages the group", token("usr_agowner", "admin"), ErrVendorAccountNotFound, ErrVendorAccountForbidden, nil},
		{"group manager without operate reach", token("usr_cmx", "admin"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"CM who does not manage the group", token("usr_cm", "admin"), ErrResourceGroupNotFound, ErrResourceGroupNotFound, ErrResourceGroupNotFound},
		{"stranger", token("usr_stranger"), ErrResourceGroupNotFound, ErrResourceGroupNotFound, ErrResourceGroupNotFound},
		{"system scope", token("usr_s", "system", "admin"), ErrVendorAccountNotFound, ErrVendorAccountNotFound, ErrVendorAccountNotFound},
		{"no user identity", auth.Token{Scopes: []string{"gateway:use"}}, ErrResourceGroupNotFound, ErrResourceGroupNotFound, ErrResourceGroupNotFound},
	}
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, p := range principals {
			want := map[string]error{routing.VendorShareLevelNone: p.none, routing.VendorShareLevelShared: p.shared, routing.VendorShareLevelFull: p.full}[level]
			label := "set " + level + "/" + p.name
			acc := e.sharedAccountAt(level, e.ag.ID)
			rg := e.freshRG()
			before := e.snapshot(acc.ID)

			_, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, p.tok, rg.ID, []string{acc.ID})

			if want == nil {
				if err != nil {
					t.Errorf("%s: err = %v, want allow", label, err)
					continue
				}
				e.requireRGAccounts(label, rg.ID, acc.ID)
				continue
			}
			if !errors.Is(err, want) {
				t.Errorf("%s: err = %v, want %v", label, err, want)
			}
			e.requireRGAccounts(label+" (denied)", rg.ID)
			e.requireUnchanged(label+" (denied)", before)
		}
	}
}

// A stranger must never learn that an account exists from this endpoint: an existing
// account the caller cannot operate and an unknown id are the same answer.
func TestSetResourceGroupVendorAccountsNeverLeaksAnAccount(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	for name, tok := range map[string]auth.Token{
		"group manager without operate reach": token("usr_cmx", "admin"),
		"system scope":                        token("usr_s", "system", "admin"),
	} {
		_, known := e.svc.SetResourceGroupVendorAccounts(e.ctx, tok, e.rgm.ID, []string{acc.ID})
		_, unknown := e.svc.SetResourceGroupVendorAccounts(e.ctx, tok, e.rgm.ID, []string{"va_missing"})
		if !errors.Is(known, ErrVendorAccountNotFound) || !errors.Is(unknown, ErrVendorAccountNotFound) || errors.Is(known, ErrVendorAccountForbidden) {
			t.Errorf("%s: existing = %v, unknown = %v, want ErrVendorAccountNotFound for both", name, known, unknown)
		}
	}
	e.requireRGAccounts("nothing written", e.rgm.ID)
}

// The CM's reach is the LIVE intersection of the account's management groups and the
// groups the CM manages vendor accounts through: dropping AG from the account takes the
// add away from a CM who is only a CM through AG.
func TestSetResourceGroupVendorAccountsRevokesWithTheCMsReach(t *testing.T) {
	e := newVendorRGEnv(t)
	cm := token("usr_agowner", "admin")
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID, e.agB.ID)
	rg := e.freshRG()
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("drop AG: %v", err)
	}
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("drop AG-B: %v", err)
	}
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("assign AG-B only: %v", err)
	}
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, cm, rg.ID, []string{acc.ID}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("CM after losing the group: err = %v, want ErrVendorAccountNotFound", err)
	}
	e.requireRGAccounts("nothing written", rg.ID)
}

// A group under another system group takes no account of this root, however well the
// caller manages both: ErrResourceGroupVendorAccountSystemGroupMismatch. The check is the
// last one, so it confirms nothing to a caller the earlier gates refused.
func TestSetResourceGroupVendorAccountsChecksTheRoot(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	// The admin owner manages rgX (owner of AG-X) and operates the account (owner).
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, e.rgX.ID, []string{acc.ID}); !errors.Is(err, ErrResourceGroupVendorAccountSystemGroupMismatch) {
		t.Fatalf("add under another root: err = %v, want ErrResourceGroupVendorAccountSystemGroupMismatch", err)
	}
	e.requireRGAccounts("not added", e.rgX.ID)

	// An account of that other root is welcome there.
	other := e.accountAtRoot(routing.VendorShareLevelFull, e.otherRootID, e.agX.ID)
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, e.rgX.ID, []string{other.ID}); err != nil {
		t.Fatalf("add an account of the group's own root: %v", err)
	}
	e.requireRGAccounts("added", e.rgX.ID, other.ID)
}

// An account with no stored root, and a group with none, never match each other.
func TestSetResourceGroupVendorAccountsFailsClosedOnAnEmptyRoot(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, acc.ID, ""); err != nil {
		t.Fatalf("clear the root: %v", err)
	}
	rootless := e.newRG("rgrp_vrg_rootless", "Rootless", "", e.agB.ID)
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rootless.ID, []string{acc.ID}); !errors.Is(err, ErrResourceGroupVendorAccountSystemGroupMismatch) {
		t.Fatalf("empty root against empty root: err = %v, want ErrResourceGroupVendorAccountSystemGroupMismatch", err)
	}
	e.requireRGAccounts("not added", rootless.ID)
}

// The carried set REPLACES the stored one (add the missing, drop the surplus), is
// trimmed and de-duplicated, an empty set clears the group, and the response is the
// resource group with its accounts listed.
func TestSetResourceGroupVendorAccountsReplacesTheSet(t *testing.T) {
	e := newVendorRGEnv(t)
	a := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	b := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	rg := e.freshRG()

	dto, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{" " + a.ID + " ", a.ID, "", "  "})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	if dto.ID != rg.ID || len(dto.VendorAccounts) != 1 || dto.VendorAccounts[0] != (ResourceGroupVendorAccountRefDTO{ID: a.ID, Name: a.Name}) {
		t.Errorf("response = %+v, want the group with [%s %s]", dto, a.ID, a.Name)
	}
	e.requireRGAccounts("added a", rg.ID, a.ID)

	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{b.ID}); err != nil {
		t.Fatalf("replace a by b: %v", err)
	}
	e.requireRGAccounts("replaced", rg.ID, b.ID)
	// The same set again is not an error (the endpoint is a PUT).
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{b.ID}); err != nil {
		t.Fatalf("same set again: %v", err)
	}
	e.requireRGAccounts("idempotent", rg.ID, b.ID)

	dto, err = e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if dto.VendorAccounts == nil || len(dto.VendorAccounts) != 0 {
		t.Errorf("response accounts = %#v, want an empty, non-nil list", dto.VendorAccounts)
	}
	e.requireRGAccounts("cleared", rg.ID)
	// Another group's links are never touched.
	e.link(e.rgm.ID, a.ID)
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, nil); err != nil {
		t.Fatalf("clear again: %v", err)
	}
	e.requireRGAccounts("another group untouched", e.rgm.ID, a.ID)
}

// The write is all-or-nothing: one account the caller may not add aborts the whole call
// before anything is written, and the stored set is exactly what it was.
func TestSetResourceGroupVendorAccountsIsAllOrNothing(t *testing.T) {
	e := newVendorRGEnv(t)
	good := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	shared := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	kept := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	rg := e.freshRG()
	e.link(rg.ID, kept.ID)

	for _, tc := range []struct {
		name, id string
		want     error
	}{
		{"a shared account", shared.ID, ErrVendorAccountForbidden},
		{"an unknown id", "va_missing", ErrVendorAccountNotFound},
	} {
		if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{good.ID, tc.id}); !errors.Is(err, tc.want) {
			t.Errorf("%s in the set: err = %v, want %v", tc.name, err, tc.want)
		}
		e.requireRGAccounts("after a refused set with "+tc.name, rg.ID, kept.ID)
	}
}

// Only NEW members are validated; a current member stays when the caller sends it again,
// even if the account no longer passes the add checks (lowered to `shared` since), and
// REMOVAL needs only the group gate: a group manager with no reach on the account can drop
// it, because what a group offers is its manager's call.
func TestSetResourceGroupVendorAccountsRemovalNeedsOnlyTheGroupGate(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	rg := e.freshRG()
	e.link(rg.ID, acc.ID)
	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared); err != nil {
		t.Fatalf("lower to shared: %v", err)
	}
	e.requireRGAccounts("the link survives the lower", rg.ID, acc.ID)

	// usr_cmx manages the group and has no reach on the account.
	cmx := token("usr_cmx", "admin")
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, cmx, rg.ID, []string{acc.ID}); err != nil {
		t.Fatalf("re-sending a current member: %v", err)
	}
	e.requireRGAccounts("kept", rg.ID, acc.ID)
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, cmx, rg.ID, nil); err != nil {
		t.Fatalf("a group manager dropping a link: %v", err)
	}
	e.requireRGAccounts("dropped", rg.ID)
}

// Additions are written, then removals; a store failure leaves the earlier writes in
// place and reports the error (no panic, no silent success).
func TestSetResourceGroupVendorAccountsReportsAStoreFailure(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	rg := e.freshRG()
	boom := errors.New("boom")
	e.svc.routes = &failingResourceGroupVendorStore{Store: e.svc.routes, err: boom}
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{acc.ID}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's", err)
	}
}

// failingResourceGroupVendorStore fails the resource-group/vendor-account link write.
type failingResourceGroupVendorStore struct {
	routing.Store
	err error
}

func (f *failingResourceGroupVendorStore) SetResourceGroupVendorAccount(_ context.Context, _, _ string) error {
	return f.err
}

// The group's DTO lists its accounts (id and name only), always non-nil.
func TestResourceGroupDTOListsItsVendorAccounts(t *testing.T) {
	e := newVendorRGEnv(t)
	dto, err := e.svc.GetResourceGroup(e.ctx, e.admin, e.rgm.ID)
	if err != nil {
		t.Fatalf("GetResourceGroup: %v", err)
	}
	if dto.VendorAccounts == nil || len(dto.VendorAccounts) != 0 {
		t.Fatalf("accounts of an empty group = %#v, want an empty, non-nil list", dto.VendorAccounts)
	}
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	e.link(e.rgm.ID, acc.ID)
	dto, err = e.svc.GetResourceGroup(e.ctx, e.admin, e.rgm.ID)
	if err != nil || len(dto.VendorAccounts) != 1 || dto.VendorAccounts[0] != (ResourceGroupVendorAccountRefDTO{ID: acc.ID, Name: acc.Name}) {
		t.Fatalf("GetResourceGroup = %+v, %v, want the account listed", dto.VendorAccounts, err)
	}

	// A link to an account that is no longer shared is still listed (the manager can drop
	// it), but with the vendor-accounts module off the area is switched off: the group
	// advertises nothing (and the list is still the non-nil empty one).
	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	dto, err = e.svc.GetResourceGroup(e.ctx, e.admin, e.rgm.ID)
	if err != nil || dto.VendorAccounts == nil || len(dto.VendorAccounts) != 0 {
		t.Fatalf("GetResourceGroup with the module off = %#v, %v, want an empty, non-nil list", dto.VendorAccounts, err)
	}
	list, err := e.svc.ListResourceGroups(e.ctx, e.admin)
	if err != nil || len(list) == 0 {
		t.Fatalf("ListResourceGroups with the module off = %v, %v", list, err)
	}
	for _, g := range list {
		if g.VendorAccounts == nil || len(g.VendorAccounts) != 0 {
			t.Errorf("group %s lists %#v with the module off, want an empty, non-nil list", g.ID, g.VendorAccounts)
		}
	}
}

// --- candidates --------------------------------------------------------------------

// The candidates are what the write path would accept: the accounts the caller OPERATES
// (their own, and the shared ones of a management group they are a CM of) that are `full`
// and under the group's root. A `shared` or `none` account, an account under another root
// and an account the caller has no reach on are never offered; the list is sorted by name
// and never nil.
func TestResourceGroupVendorAccountCandidatesAreScopedToWhatTheCallerCanAdd(t *testing.T) {
	e := newVendorRGEnv(t)
	own := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	e.sharedAccountAt(routing.VendorShareLevelNone)
	e.accountAtRoot(routing.VendorShareLevelFull, e.otherRootID, e.agX.ID)
	// Other owners' full accounts assigned to AG: the admin owner is a CM of AG. One is
	// the fixture's own, the other made here.
	managed := e.newAccountAt(routing.VendorShareLevelFull)
	fixtureFull := e.accounts[routing.VendorShareLevelFull]
	// Another owner's full account assigned to AG2 only: no reach.
	ag2Account := e.newAccountAt(routing.VendorShareLevelFull)
	ag2 := e.mustCreateGroup(token("usr_agowner2", "admin"), CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2-cand", ParentGroupID: e.rootID})
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, ag2Account.ID, e.ag.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, ag2Account.ID, ag2.ID); err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.ResourceGroupVendorAccountCandidates(e.ctx, e.admin, e.rgm.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	want := []ResourceGroupVendorAccountRefDTO{
		{ID: managed.ID, Name: managed.Name}, {ID: fixtureFull.ID, Name: fixtureFull.Name}, {ID: own.ID, Name: own.Name},
	}
	slices.SortFunc(want, func(a, b ResourceGroupVendorAccountRefDTO) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	if !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}

	// Every candidate is addable, and nothing else is.
	for _, c := range got {
		rg := e.freshRG()
		if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, rg.ID, []string{c.ID}); err != nil {
			t.Errorf("candidate %s is not addable: %v", c.ID, err)
		}
	}
}

// The candidates answer for a group the caller manages only (the 404-no-leak of the
// group's own gate), never widen for system scope (it owns and manages nothing here, so
// it is offered nothing), and are an empty, non-nil list when there is nothing to offer.
func TestResourceGroupVendorAccountCandidatesGateAndEmptyList(t *testing.T) {
	e := newVendorRGEnv(t)
	e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)

	for name, tok := range map[string]auth.Token{
		"CM who does not manage the group": token("usr_cm", "admin"),
		"stranger":                         token("usr_stranger"),
		"no user identity":                 {Scopes: []string{"gateway:use"}},
	} {
		if _, err := e.svc.ResourceGroupVendorAccountCandidates(e.ctx, tok, e.rgm.ID); !errors.Is(err, ErrResourceGroupNotFound) {
			t.Errorf("%s: err = %v, want ErrResourceGroupNotFound", name, err)
		}
	}
	if _, err := e.svc.ResourceGroupVendorAccountCandidates(e.ctx, e.admin, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Errorf("unknown group: err = %v, want ErrResourceGroupNotFound", err)
	}
	for name, tok := range map[string]auth.Token{
		"system scope":                        token("usr_s", "system", "admin"),
		"group manager without operate reach": token("usr_cmx", "admin"),
	} {
		got, err := e.svc.ResourceGroupVendorAccountCandidates(e.ctx, tok, e.rgm.ID)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("%s: candidates = %#v, %v, want an empty, non-nil list", name, got, err)
		}
	}
}

// --- downgrade -----------------------------------------------------------------------

// Lowering an account to `none` drops every resource-group link, whichever path made it:
// ClearVendorAccountSharing deletes them in the same transaction that sets the level, so a
// downgrade takes effect at once (the routing grant reads the live level besides). Lowering
// full -> shared changes only the level, so the links stay.
func TestLoweringALinkedVendorAccountDropsItsResourceGroupLinks(t *testing.T) {
	e := newVendorRGEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); err != nil {
		t.Fatalf("owner self-service join: %v", err)
	}
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, e.rgA.ID, []string{acc.ID}); err != nil {
		t.Fatalf("manager-side add: %v", err)
	}
	e.requireRGAccounts("linked by self-service", e.rgm.ID, acc.ID)
	e.requireRGAccounts("linked by the manager side", e.rgA.ID, acc.ID)

	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared); err != nil {
		t.Fatalf("lower to shared: %v", err)
	}
	e.requireRGAccounts("after full -> shared (self-service link)", e.rgm.ID, acc.ID)
	e.requireRGAccounts("after full -> shared (manager link)", e.rgA.ID, acc.ID)

	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelNone); err != nil {
		t.Fatalf("lower to none: %v", err)
	}
	e.requireRGAccounts("after none (self-service link)", e.rgm.ID)
	e.requireRGAccounts("after none (manager link)", e.rgA.ID)
	e.requirePrivate("lowered", acc.ID)

	// And a private account is no longer linkable by either path.
	if err := e.svc.AddVendorAccountToResourceGroup(e.ctx, e.admin, acc.ID, e.rgm.ID); !errors.Is(err, ErrVendorAccountNotShared) {
		t.Errorf("self-service join of a private account: err = %v, want ErrVendorAccountNotShared", err)
	}
	if _, err := e.svc.SetResourceGroupVendorAccounts(e.ctx, e.admin, e.rgA.ID, []string{acc.ID}); !errors.Is(err, ErrVendorAccountNotShared) {
		t.Errorf("manager-side add of a private account: err = %v, want ErrVendorAccountNotShared", err)
	}
}
