// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strconv"
	"testing"
)

// This file is the management-group assignment contract of shared vendor accounts
// (#183, spec 2026-10-15 sections 6 and 9): SetVendorAccountAdminGroups re-assigns an
// already shared account's management groups, and VendorAccountAdminGroupCandidates
// lists the groups the caller may name.
//
//   - AUTHORIZATION is the CREDENTIAL gate's shape (spec 6: "Management-group
//     assignment" is owner-only at `shared`, owner + CM at `full`): the owner always,
//     a CM at `full` only; a CM at `shared` is ErrVendorAccountForbidden, everyone
//     else ErrVendorAccountNotFound. An account at `none` has nothing to assign groups
//     to (ErrVendorAccountNotShared).
//   - The set always keeps >= 1 group, a non-system caller names only groups they
//     manage vendor accounts through, and the containment root is immutable.
//   - While vendor_account_sharing_enabled is off both methods are a 409.

// sharedAccountAt creates an api_key account owned by the admin owner and puts it at
// level with the given management groups and the shared root, the state the raise
// produces. The account gets a name of its own: the matrix creates many.
func (e *vendorLifecycleEnv) sharedAccountAt(level string, groups ...string) routing.VendorAccount {
	e.t.Helper()
	e.accountSeq++
	acc := e.apiKeyAccount("assign-" + level + "-" + strconv.Itoa(e.accountSeq))
	if level == routing.VendorShareLevelNone {
		return acc
	}
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, level); err != nil {
		e.t.Fatalf("set level of %s: %v", acc.ID, err)
	}
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, acc.ID, e.rootID); err != nil {
		e.t.Fatalf("set root of %s: %v", acc.ID, err)
	}
	for _, gid := range groups {
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, gid); err != nil {
			e.t.Fatalf("assign %s to %s: %v", acc.ID, gid, err)
		}
	}
	return e.reload(acc.ID)
}

func (e *vendorLifecycleEnv) assign(p auth.Token, id string, groups ...string) error {
	_, err := e.svc.SetVendorAccountAdminGroups(e.ctx, p, id, groups)
	return err
}

// vendorAssignPrincipals are the callers the matrix runs. The owner is the ADMIN
// owner (who manages vendor accounts through AG and AG-B, so he can name groups at
// all); the names are the ones vendorGateAllowed and vendorGateDenial know.
func (e *vendorLifecycleEnv) vendorAssignPrincipals() []struct {
	name string
	tok  auth.Token
} {
	return []struct {
		name string
		tok  auth.Token
	}{
		{"owner", e.admin},
		{"cm", token("usr_cm", "admin")},
		{"admin-group owner", token("usr_agowner", "admin")},
		{"co-manager without the vendor flag", token("usr_cmx", "admin")},
		{"CM of another admin group", token("usr_cm2", "admin")},
		{"owner of another admin group", token("usr_agowner2", "admin")},
		{"stranger", token("usr_stranger")},
		{"system scope", token("usr_s", "system", "admin")},
		{"no user identity", auth.Token{Scopes: []string{"gateway:use"}}},
	}
}

// --- authorization -------------------------------------------------------------

// TestSetVendorAccountAdminGroupsAuthorizationMatrix is the spec 6 row, for every
// kind of principal at every level: the owner always, a CM at `full` only (the
// credential gate's shape); a CM at `shared` is told 403 (the account is no secret
// to them), everyone else gets the 404 an unknown id gets; at `none` the owner is
// told there is nothing to assign (ErrVendorAccountNotShared) and everyone else is a
// stranger. A denied call writes nothing; an allowed one replaces [AG, AG-B] with
// [AG], which is observable.
func TestSetVendorAccountAdminGroupsAuthorizationMatrix(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, p := range e.vendorAssignPrincipals() {
			label := "assign " + level + "/" + p.name
			acc := e.sharedAccountAt(level, e.ag.ID, e.agB.ID)
			before := e.snapshot(acc.ID)

			err := e.assign(p.tok, acc.ID, e.ag.ID)

			switch {
			case level == routing.VendorShareLevelNone && p.name == "owner":
				if !errors.Is(err, ErrVendorAccountNotShared) {
					t.Errorf("%s: err = %v, want ErrVendorAccountNotShared", label, err)
				}
				e.requireUnchanged(label, before)
			case level != routing.VendorShareLevelNone && vendorGateAllowed(true, level, p.name):
				if err != nil {
					t.Errorf("%s: err = %v, want allow", label, err)
					continue
				}
				e.requireShareState(label, acc.ID, level, e.rootID, e.ag.ID)
			default:
				requireAuthzOutcome(t, label, err, vendorGateDenial(true, level, p.name))
				e.requireUnchanged(label, before)
			}
		}
	}
}

// A stranger must never learn an account exists from this endpoint: at every level,
// each caller who cannot see the account gets exactly the answer an unknown id gets.
func TestSetVendorAccountAdminGroupsNeverLeaksAStrangerAs403(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	cannotSee := map[string]bool{
		"co-manager without the vendor flag": true, "CM of another admin group": true,
		"owner of another admin group": true, "stranger": true, "system scope": true, "no user identity": true,
	}
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := e.sharedAccountAt(level, e.ag.ID)
		for _, p := range e.vendorAssignPrincipals() {
			if !cannotSee[p.name] {
				continue
			}
			unknown := e.assign(p.tok, "va_missing", e.ag.ID)
			known := e.assign(p.tok, acc.ID, e.ag.ID)
			if !errors.Is(unknown, ErrVendorAccountNotFound) || !errors.Is(known, ErrVendorAccountNotFound) || errors.Is(known, ErrVendorAccountForbidden) {
				t.Errorf("%s at %s: unknown id = %v, existing account = %v, want ErrVendorAccountNotFound for both", p.name, level, unknown, known)
			}
		}
	}
}

// The CM's reach is the LIVE intersection of the account's groups and the groups the
// CM manages vendor accounts through: dropping the CM's group from the account takes
// the endpoint away from him on the very next call, and the owner keeps it.
func TestSetVendorAccountAdminGroupsRevokesWithTheCMsReach(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	cm := token("usr_cm", "admin")
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID, e.agB.ID)

	// The CM manages AG, not AG-B, so he may not name AG-B.
	if err := e.assign(cm, acc.ID, e.agB.ID); !errors.Is(err, ErrVendorAccountAdminGroupInvalid) {
		t.Fatalf("CM naming a group he does not manage: err = %v, want ErrVendorAccountAdminGroupInvalid", err)
	}
	// The owner moves the account to AG-B; AG is no longer one of its groups, so the
	// CM (who reaches the account only through AG) is a stranger.
	if err := e.assign(e.admin, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("owner re-assign: %v", err)
	}
	requireAuthzOutcome(t, "CM after losing the group", e.assign(cm, acc.ID, e.ag.ID), authzNotFound)
	e.requireShareState("owner's set kept", acc.ID, routing.VendorShareLevelFull, e.rootID, e.agB.ID)
}

// --- the set: replace, >= 1, scope -----------------------------------------------

// The carried set REPLACES the stored one (add the missing, drop the surplus), is
// trimmed and de-duplicated, and the response is the account's credential-free view.
func TestSetVendorAccountAdminGroupsReplacesTheSet(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)

	dto, err := e.svc.SetVendorAccountAdminGroups(e.ctx, e.admin, acc.ID, []string{" " + e.agB.ID + " ", e.agB.ID, "", e.ag.ID})
	if err != nil {
		t.Fatalf("add AG-B: %v", err)
	}
	if dto.ID != acc.ID {
		t.Errorf("returned dto id = %q, want %q", dto.ID, acc.ID)
	}
	e.requireShareState("added", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID, e.agB.ID)

	if err := e.assign(e.admin, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("drop AG: %v", err)
	}
	e.requireShareState("replaced", acc.ID, routing.VendorShareLevelShared, e.rootID, e.agB.ID)

	// Assigning the same set again is not an error (the endpoint is a PUT).
	if err := e.assign(e.admin, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("same set again: %v", err)
	}
	e.requireShareState("idempotent", acc.ID, routing.VendorShareLevelShared, e.rootID, e.agB.ID)
	// The level, the credential and everything else are untouched.
	if got := e.reload(acc.ID); got.APIKey != acc.APIKey || got.Name != acc.Name || got.Status != acc.Status {
		t.Errorf("the assignment changed the account: %+v -> %+v", acc, got)
	}
}

// Every management-group PUT keeps >= 1 group: an empty set (nil, [], blanks) is
// refused at shared and full, and the account keeps what it had.
func TestSetVendorAccountAdminGroupsNeedsAtLeastOneGroup(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := e.sharedAccountAt(level, e.ag.ID, e.agB.ID)
		before := e.snapshot(acc.ID)
		for name, groups := range map[string][]string{"nil": nil, "empty": {}, "blanks": {"", "   "}} {
			_, err := e.svc.SetVendorAccountAdminGroups(e.ctx, e.admin, acc.ID, groups)
			if !errors.Is(err, ErrVendorAccountAdminGroupRequired) {
				t.Errorf("%s with %s groups: err = %v, want ErrVendorAccountAdminGroupRequired", level, name, err)
			}
		}
		e.requireUnchanged("an empty assignment at "+level, before)
	}
}

// A non-system caller names only groups they manage vendor accounts through (AG as a
// flagged co-manager, AG-B as owner); each must be an existing ADMIN-tier group and
// all share one parent. A system-scope OWNER is not limited to a manage set.
func TestSetVendorAccountAdminGroupsEnforcesTheGroupScope(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	ag2 := e.mustCreateGroup(token("usr_agowner2", "admin"), CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2-b", ParentGroupID: e.rootID})
	userGroup := e.mustCreateGroup(token("usr_agowner", "admin"), CreateGroupInput{Tier: store.GroupTierUser, Name: "Users", ParentGroupID: e.ag.ID})

	acc := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	before := e.snapshot(acc.ID)
	cases := []struct {
		name   string
		groups []string
		want   error
	}{
		{"a group outside the caller's manage set", []string{ag2.ID}, ErrVendorAccountAdminGroupInvalid},
		{"one managed group and one outside", []string{e.ag.ID, ag2.ID}, ErrVendorAccountAdminGroupInvalid},
		{"a group that does not exist", []string{"ugrp_missing"}, ErrVendorAccountAdminGroupInvalid},
		{"a system-tier group", []string{e.rootID}, ErrVendorAccountAdminGroupInvalid},
		{"a user-tier group", []string{userGroup.ID}, ErrVendorAccountAdminGroupInvalid},
		{"groups under different parents", []string{e.ag.ID, e.agX.ID}, ErrVendorAccountAdminGroupParentMismatch},
	}
	for _, tc := range cases {
		if err := e.assign(e.admin, acc.ID, tc.groups...); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	e.requireUnchanged("an assignment with a bad group set", before)

	// A system-scope OWNER skips the manage-set check (but not the tier, parent or
	// root ones).
	sys := token("usr_s", "system", "admin")
	sysAcc, err := e.svc.CreateVendorAccount(e.ctx, sys, CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "system-owned"})
	if err != nil {
		t.Fatalf("create as system: %v", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, sys, sysAcc.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise as system: %v", err)
	}
	if err := e.assign(sys, sysAcc.ID, ag2.ID); err != nil {
		t.Fatalf("system owner naming a group outside any manage set: %v", err)
	}
	e.requireShareState("system owner", sysAcc.ID, routing.VendorShareLevelShared, e.rootID, ag2.ID)
	if err := e.assign(sys, sysAcc.ID, e.ag.ID, e.agX.ID); !errors.Is(err, ErrVendorAccountAdminGroupParentMismatch) {
		t.Errorf("system owner, mixed parents: err = %v, want ErrVendorAccountAdminGroupParentMismatch", err)
	}
}

// --- the containment root is immutable -----------------------------------------

// While the account stays shared/full its root cannot move: groups under another
// system group are ErrVendorAccountRootMismatch (a 400, though the caller manages
// those groups perfectly well), for every principal, system scope included.
func TestSetVendorAccountAdminGroupsKeepsTheContainmentRoot(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := e.sharedAccountAt(level, e.ag.ID)
		before := e.snapshot(acc.ID)
		if err := e.assign(e.admin, acc.ID, e.agX.ID); !errors.Is(err, ErrVendorAccountRootMismatch) {
			t.Errorf("%s: moving to AG-X (another root): err = %v, want ErrVendorAccountRootMismatch", level, err)
		}
		// Naming a group of the old root next to one of the new one is a parent
		// mismatch of the SET, which answers first.
		if err := e.assign(e.admin, acc.ID, e.ag.ID, e.agX.ID); !errors.Is(err, ErrVendorAccountAdminGroupParentMismatch) {
			t.Errorf("%s: AG + AG-X: err = %v, want ErrVendorAccountAdminGroupParentMismatch", level, err)
		}
		e.requireUnchanged("a root-moving assignment at "+level, before)
		e.requireShareState("root kept at "+level, acc.ID, level, e.rootID, e.ag.ID)
	}

	// The guard is for EVERY principal: a system-scope owner may name any group, but
	// not one under another root.
	sys := token("usr_s", "system", "admin")
	sysAcc, err := e.svc.CreateVendorAccount(e.ctx, sys, CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "system-root"})
	if err != nil {
		t.Fatalf("create as system: %v", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, sys, sysAcc.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise as system: %v", err)
	}
	if err := e.assign(sys, sysAcc.ID, e.agX.ID); !errors.Is(err, ErrVendorAccountRootMismatch) {
		t.Errorf("system owner moving the root: err = %v, want ErrVendorAccountRootMismatch", err)
	}
	e.requireShareState("system root kept", sysAcc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
}

// The edge of spec section 5: a group delete can cascade away the LAST management-group
// row while the account stays shared with its root. The account then has zero groups, the
// owner still manages it, and re-adding a group must match the RETAINED root.
func TestSetVendorAccountAdminGroupsAfterTheLastGroupVanished(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("simulate the cascade: %v", err)
	}
	e.requireShareState("orphaned", acc.ID, routing.VendorShareLevelShared, e.rootID)

	if err := e.assign(e.admin, acc.ID, e.agX.ID); !errors.Is(err, ErrVendorAccountRootMismatch) {
		t.Fatalf("re-adding a group under another root: err = %v, want ErrVendorAccountRootMismatch", err)
	}
	e.requireShareState("still orphaned", acc.ID, routing.VendorShareLevelShared, e.rootID)
	if err := e.assign(e.admin, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("re-adding a group under the retained root: %v", err)
	}
	e.requireShareState("re-adopted", acc.ID, routing.VendorShareLevelShared, e.rootID, e.agB.ID)
}

// A shared account that somehow has no root (never produced by the raise, but a store
// written directly could) takes the root of the first assignment, like an ungrouped
// server does, so the immutability guard holds from then on.
func TestSetVendorAccountAdminGroupsFillsAMissingRoot(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, acc.ID, ""); err != nil {
		t.Fatalf("clear the root: %v", err)
	}
	if err := e.assign(e.admin, acc.ID, e.agX.ID); err != nil {
		t.Fatalf("assign into AG-X on a root-less account: %v", err)
	}
	e.requireShareState("root filled", acc.ID, routing.VendorShareLevelShared, e.otherRootID, e.agX.ID)
	if err := e.assign(e.admin, acc.ID, e.ag.ID); !errors.Is(err, ErrVendorAccountRootMismatch) {
		t.Errorf("after the root was filled: err = %v, want ErrVendorAccountRootMismatch", err)
	}
}

// --- an account that is not shared -----------------------------------------------

// Assigning management groups to an account that is not shared (level none, or one
// the store holds an unrecognised value for) is refused for its owner, and nothing is
// written: no group, no root.
func TestSetVendorAccountAdminGroupsRefusesAnUnsharedAccount(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelNone)
	if err := e.assign(e.admin, acc.ID, e.ag.ID); !errors.Is(err, ErrVendorAccountNotShared) {
		t.Fatalf("none: err = %v, want ErrVendorAccountNotShared", err)
	}
	e.requirePrivate("untouched", acc.ID)

	// The shape is not looked at before the state: an empty set on a `none` account is
	// the same refusal.
	if err := e.assign(e.admin, acc.ID); !errors.Is(err, ErrVendorAccountNotShared) {
		t.Errorf("none with no groups: err = %v, want ErrVendorAccountNotShared", err)
	}

	odd := e.sharedAccountAt(routing.VendorShareLevelShared, e.ag.ID)
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, odd.ID, "bogus"); err != nil {
		t.Fatalf("corrupt the level: %v", err)
	}
	if err := e.assign(e.admin, odd.ID, e.agB.ID); !errors.Is(err, ErrVendorAccountNotShared) {
		t.Errorf("unrecognised level: err = %v, want ErrVendorAccountNotShared (fail closed)", err)
	}
	if got := e.adminGroups(odd.ID); len(got) != 1 || got[0] != e.ag.ID {
		t.Errorf("groups of the unrecognised-level account = %v, want [AG] untouched", got)
	}
}

// --- ordering and the master flag ---------------------------------------------------

// The missing links are added FIRST and the surplus removed after, so a store failure
// part-way never leaves the account without a group: a failing addition leaves the old
// set exactly as it was.
func TestSetVendorAccountAdminGroupsAddsBeforeItRemoves(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID)
	e.svc.routes = &failingVendorStore{Store: e.svc.routes, failGroupLinkAt: 1}
	if err := e.assign(e.admin, acc.ID, e.agB.ID); !errors.Is(err, errInjectedStoreFailure) {
		t.Fatalf("assign with a failing link: err = %v, want the injected failure", err)
	}
	e.svc.routes = e.routes
	e.requireShareState("old set kept", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)

	// The same call on a healthy store completes.
	if err := e.assign(e.admin, acc.ID, e.agB.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	e.requireShareState("retried", acc.ID, routing.VendorShareLevelFull, e.rootID, e.agB.ID)
}

// While vendor_account_sharing_enabled is off (the master flag), both methods are
// refused with ErrVendorAccountSharingDisabled -- before the account is looked at, so
// the owner, a stranger, system scope and an unknown id all get the same global
// answer -- and nothing changes. The vendor-accounts module flag is the more
// fundamental switch and answers first.
func TestVendorAccountAdminGroupMethodsAreRefusedWhileSharingIsOff(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.sharedAccountAt(routing.VendorShareLevelFull, e.ag.ID, e.agB.ID)
	before := e.snapshot(acc.ID)

	calls := map[string]func(p auth.Token, id string) error{
		"assign": func(p auth.Token, id string) error {
			_, err := e.svc.SetVendorAccountAdminGroups(e.ctx, p, id, []string{e.ag.ID})
			return err
		},
		"candidates": func(p auth.Token, _ string) error {
			_, err := e.svc.VendorAccountAdminGroupCandidates(e.ctx, p)
			return err
		},
	}
	setVendorSharingFlags(t, e.svc, false, true)
	for name, call := range calls {
		for _, p := range e.vendorAssignPrincipals() {
			for _, id := range []string{acc.ID, "va_missing"} {
				if err := call(p.tok, id); !errors.Is(err, ErrVendorAccountSharingDisabled) {
					t.Errorf("%s as %s on %s: err = %v, want ErrVendorAccountSharingDisabled", name, p.name, id, err)
				}
			}
		}
	}
	e.requireUnchanged("calls refused while sharing is off", before)

	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	for name, call := range calls {
		if err := call(e.admin, acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
			t.Errorf("%s with the module off: err = %v, want ErrVendorAccountsDisabled", name, err)
		}
	}

	// Switched back on, the owner's call goes through again.
	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, true)
	setVendorSharingFlags(t, e.svc, true, false)
	if err := e.assign(e.admin, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("assign after switching sharing back on: %v", err)
	}
	e.requireShareState("assigned", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)
}

// --- candidates ---------------------------------------------------------------------

func candidateNames(cands []AdminGroupCandidateDTO) []string {
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Name)
	}
	return names
}

func requireNames(t *testing.T, label string, cands []AdminGroupCandidateDTO, want ...string) {
	t.Helper()
	got := candidateNames(cands)
	if len(got) != len(want) {
		t.Errorf("%s: candidates = %v, want %v", label, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: candidates = %v, want %v", label, got, want)
			return
		}
	}
}

// The candidates are the admin-tier groups the caller may NAME: for a non-system
// caller exactly vendorAccountManageGroupIDs (owner of the group, or a co-manager
// holding can_manage_vendor_accounts), sorted by name with the parent system group's
// id and name; for system scope every admin-tier group. A co-manager without the
// vendor flag, a stranger and a principal without a user identity get an empty (not
// nil) list.
func TestVendorAccountAdminGroupCandidatesAreScopedToTheCallersReach(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	cands := func(p auth.Token) []AdminGroupCandidateDTO {
		t.Helper()
		out, err := e.svc.VendorAccountAdminGroupCandidates(e.ctx, p)
		if err != nil {
			t.Fatalf("VendorAccountAdminGroupCandidates: %v", err)
		}
		if out == nil {
			t.Fatal("candidates = nil, want an empty list (the endpoint answers [])")
		}
		return out
	}

	// The admin owner: AG (flagged co-manager), AG-B and AG-X (owner); never AG2.
	adminCands := cands(e.admin)
	requireNames(t, "admin owner", adminCands, "AG", "AG-B", "AG-X")
	for _, c := range adminCands {
		wantParent, wantParentName := e.rootID, "SG"
		if c.Name == "AG-X" {
			wantParent, wantParentName = e.otherRootID, "SG2"
		}
		if c.ParentGroupID != wantParent || c.ParentGroupName != wantParentName {
			t.Errorf("%s: parent = %q/%q, want %q/%q", c.Name, c.ParentGroupID, c.ParentGroupName, wantParent, wantParentName)
		}
	}

	requireNames(t, "flagged co-manager", cands(token("usr_cm", "admin")), "AG")
	requireNames(t, "group owner", cands(token("usr_agowner", "admin")), "AG")
	requireNames(t, "co-manager without the vendor flag", cands(token("usr_cmx", "admin")))
	requireNames(t, "stranger", cands(token("usr_stranger")))
	requireNames(t, "no user identity", cands(auth.Token{Scopes: []string{"gateway:use"}}))

	// System scope names any admin-tier group (never a system- or user-tier one).
	requireNames(t, "system scope", cands(token("usr_s", "system", "admin")), "AG", "AG-B", "AG-X", "AG2")

	// Demoting the co-manager takes the candidate away on the next call.
	if err := e.svc.SetManagerPermissions(e.ctx, token("usr_agowner", "admin"), e.ag.ID, "usr_cm", ManagerPermissions{CanManageServers: true, CanManageResources: true}); err != nil {
		t.Fatalf("SetManagerPermissions: %v", err)
	}
	requireNames(t, "demoted co-manager", cands(token("usr_cm", "admin")))
}

// A Service with no group store has no candidates, for system scope too (rather than
// a nil dereference).
func TestVendorAccountAdminGroupCandidatesWithoutAGroupStore(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	e.svc.groups = nil
	for _, p := range []auth.Token{e.admin, token("usr_s", "system", "admin")} {
		out, err := e.svc.VendorAccountAdminGroupCandidates(e.ctx, p)
		if err != nil || out == nil || len(out) != 0 {
			t.Errorf("candidates without a group store = %v, %v, want an empty list and no error", out, err)
		}
	}
}
