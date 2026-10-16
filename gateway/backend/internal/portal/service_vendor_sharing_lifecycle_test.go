// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strconv"
	"testing"
	"time"
)

// This file is the share-level lifecycle contract of shared vendor accounts (#183,
// spec 2026-10-15 sections 6, 7, 11 and 13): raising an account none -> shared ->
// full, lowering it again, and the master / subscription flags that gate both.
//
//   - RAISE is owner + a CURRENT admin role, on an owned account, and always names
//     >= 1 management group. The groups establish the containment root
//     (system_group_id); a non-system caller may only name groups they manage
//     accounts through, and all of them share one parent.
//   - LOWER is the CREDENTIAL gate's shape: the owner always (an owner must be able
//     to un-share even after losing the admin role), a CM at `full` only.
//   - A SUBSCRIPTION account is raised only to `shared`, and only while both
//     vendor_account_sharing_enabled AND vendor_subscription_sharing_enabled are on.
//   - While vendor_account_sharing_enabled is off, raise AND lower are a 409.

const vendorLifecycleAdminID = "usr_va_adm"

// vendorLifecycleEnv extends the authorization fixture with an ADMIN owner
// (usr_va_adm) who manages vendor accounts through AG (as a flagged co-manager) and
// through AG-B (which he owns), both under one system group, plus AG-X, which he
// owns under a DIFFERENT system group. AG2 (owned by usr_agowner2) stays outside his
// reach. Sharing is ON, subscription sharing OFF (its default).
type vendorLifecycleEnv struct {
	*vendorAuthzEnv
	admin       auth.Token
	agB         UserGroupDTO
	agX         UserGroupDTO
	rootID      string // the parent system group of AG and AG-B
	otherRootID string // the parent system group of AG-X
	accountSeq  int    // numbers the accounts sharedAccountAt creates
}

func newVendorLifecycleEnv(t *testing.T) *vendorLifecycleEnv {
	t.Helper()
	e := &vendorLifecycleEnv{vendorAuthzEnv: newVendorAuthzEnv(t)}
	setVendorSharingFlags(t, e.svc, true, false)

	e.createUser(vendorLifecycleAdminID, "admin")
	e.admin = token(vendorLifecycleAdminID, "admin")
	sysAdmin := token("usr_s", "system", "admin")
	agOwner := token("usr_agowner", "admin")

	e.rootID = e.ag.ParentGroupID
	e.mustAddMembers(sysAdmin, e.rootID, vendorLifecycleAdminID)
	e.mustAddMembers(agOwner, e.ag.ID, vendorLifecycleAdminID)
	if err := e.svc.PromoteManager(e.ctx, agOwner, e.ag.ID, vendorLifecycleAdminID, ManagerPermissions{CanManageVendorAccounts: true}); err != nil {
		t.Fatalf("promote %s: %v", vendorLifecycleAdminID, err)
	}
	e.agB = e.mustCreateGroup(e.admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-B", ParentGroupID: e.rootID})

	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	e.otherRootID = sg2.ID
	e.mustAddMembers(sysAdmin, sg2.ID, vendorLifecycleAdminID)
	e.agX = e.mustCreateGroup(e.admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-X", ParentGroupID: sg2.ID})
	return e
}

// setVendorSetting writes one boolean system setting into svc's settings store
// (which setVendorAccountsEnabled has already created).
// setVendorSetting writes a boolean setting straight through the store and then drops
// the service's cached sharing flags, standing in for the settings PUT, which does both
// (persistSystemSettings invalidates after its write). Without the second step a test
// that flips a sharing switch after a decision has already been made would read the
// cached pre-flip value for the rest of the TTL.
func setVendorSetting(t *testing.T, svc *Service, key string, on bool) {
	t.Helper()
	if err := svc.settings.SetSystemSetting(context.Background(), key, strconv.FormatBool(on), time.Time{}); err != nil {
		t.Fatalf("SetSystemSetting(%s): %v", key, err)
	}
	svc.invalidateVendorSharingFlags()
}

func setVendorSharingFlags(t *testing.T, svc *Service, sharing, subscriptionSharing bool) {
	t.Helper()
	setVendorSetting(t, svc, vendorAccountSharingEnabledKey, sharing)
	setVendorSetting(t, svc, vendorSubscriptionSharingEnabledKey, subscriptionSharing)
}

// adminAccount creates an account owned by the admin owner, at share level none.
func (e *vendorLifecycleEnv) adminAccount(req CreateVendorAccountRequest) routing.VendorAccount {
	e.t.Helper()
	dto, err := e.svc.CreateVendorAccount(e.ctx, e.admin, req)
	if err != nil {
		e.t.Fatalf("CreateVendorAccount(%s): %v", req.Name, err)
	}
	return e.reload(dto.ID)
}

func (e *vendorLifecycleEnv) apiKeyAccount(name string) routing.VendorAccount {
	e.t.Helper()
	return e.adminAccount(CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: name})
}

func (e *vendorLifecycleEnv) subscriptionAccount(name string) routing.VendorAccount {
	e.t.Helper()
	return e.adminAccount(CreateVendorAccountRequest{Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: name})
}

// linkToResourceGroup puts an RG link on the account (the Phase 3 write path does
// not exist yet; the store method does), returning the resource group's id.
func (e *vendorLifecycleEnv) linkToResourceGroup(accountID string) string {
	e.t.Helper()
	rgID := "rgrp_lc_" + accountID
	if err := e.routes.CreateResourceGroup(e.ctx, routing.ResourceGroup{ID: rgID, Name: rgID, Status: routing.ServerStatusActive, SystemGroupID: e.rootID, CreatedAt: e.now, UpdatedAt: e.now}); err != nil {
		e.t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := e.routes.SetResourceGroupVendorAccount(e.ctx, rgID, accountID); err != nil {
		e.t.Fatalf("SetResourceGroupVendorAccount: %v", err)
	}
	return rgID
}

func (e *vendorLifecycleEnv) adminGroups(id string) []string {
	e.t.Helper()
	groups, err := e.routes.VendorAccountAdminGroups(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountAdminGroups(%s): %v", id, err)
	}
	slices.Sort(groups)
	return groups
}

func (e *vendorLifecycleEnv) resourceGroupLinks(id string) []string {
	e.t.Helper()
	rgs, err := e.routes.ResourceGroupsByVendorAccount(e.ctx, id)
	if err != nil {
		e.t.Fatalf("ResourceGroupsByVendorAccount(%s): %v", id, err)
	}
	ids := make([]string, 0, len(rgs))
	for _, rg := range rgs {
		ids = append(ids, rg.ID)
	}
	slices.Sort(ids)
	return ids
}

// requireShareState asserts, through the STORE, the level, the containment root and
// the management-group set of an account.
func (e *vendorLifecycleEnv) requireShareState(label, id, level, root string, groups ...string) {
	e.t.Helper()
	acc := e.reload(id)
	if acc.ShareLevel != level || acc.SystemGroupID != root {
		e.t.Errorf("%s: share_level/system_group_id = %q/%q, want %q/%q", label, acc.ShareLevel, acc.SystemGroupID, level, root)
	}
	want := slices.Clone(groups)
	slices.Sort(want)
	if got := e.adminGroups(id); !slices.Equal(got, want) {
		e.t.Errorf("%s: management groups = %v, want %v", label, got, want)
	}
}

// requirePrivate asserts the account is fully private again: level none, no root, no
// management group and no resource-group link.
func (e *vendorLifecycleEnv) requirePrivate(label, id string) {
	e.t.Helper()
	e.requireShareState(label, id, routing.VendorShareLevelNone, "")
	if links := e.resourceGroupLinks(id); len(links) != 0 {
		e.t.Errorf("%s: resource-group links = %v, want none", label, links)
	}
}

// failingStore wraps the routing store and fails ONE named write, so a test can see
// what a half-finished raise leaves behind.
type failingVendorStore struct {
	routing.Store
	failGroupLinkAt int // 1-based: the Nth SetVendorAccountAdminGroup call fails
	groupLinks      int
	failShareLevel  bool
}

var errInjectedStoreFailure = errors.New("injected store failure")

func (f *failingVendorStore) SetVendorAccountAdminGroup(ctx context.Context, accountID, groupID string) error {
	f.groupLinks++
	if f.failGroupLinkAt != 0 && f.groupLinks == f.failGroupLinkAt {
		return errInjectedStoreFailure
	}
	return f.Store.SetVendorAccountAdminGroup(ctx, accountID, groupID)
}

func (f *failingVendorStore) UpdateVendorAccountShareLevel(ctx context.Context, id, level string) error {
	if f.failShareLevel {
		return errInjectedStoreFailure
	}
	return f.Store.UpdateVendorAccountShareLevel(ctx, id, level)
}

// --- raise: the happy path ----------------------------------------------------

// TestRaiseVendorAccountShareLevelNoneToSharedToFull walks the whole ladder as the
// admin owner: none -> shared names two groups under one parent and establishes the
// root; shared -> full carries the group set again (>= 1, validated like the first
// time, and replacing the stored set). Everything is asserted through the store.
func TestRaiseVendorAccountShareLevelNoneToSharedToFull(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("ladder")
	e.requirePrivate("fresh", acc.ID)

	dto, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.ag.ID, e.agB.ID})
	if err != nil {
		t.Fatalf("raise none -> shared: %v", err)
	}
	if dto.ID != acc.ID {
		t.Errorf("returned dto id = %q, want %q", dto.ID, acc.ID)
	}
	e.requireShareState("shared", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID, e.agB.ID)

	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, []string{e.agB.ID}); err != nil {
		t.Fatalf("raise shared -> full: %v", err)
	}
	// The carried set REPLACES the stored one; the root is untouched.
	e.requireShareState("full", acc.ID, routing.VendorShareLevelFull, e.rootID, e.agB.ID)
}

// TestRaiseVendorAccountShareLevelNoneStraightToFull: the matrix allows none -> full
// in one step (an api_key account); it needs the same >= 1 group.
func TestRaiseVendorAccountShareLevelNoneStraightToFull(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("straight")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise none -> full: %v", err)
	}
	e.requireShareState("full", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)
}

// A raise de-duplicates and trims the group ids it was given.
func TestRaiseVendorAccountShareLevelNormalizesTheGroupIDs(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("dedup")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{" " + e.ag.ID + " ", e.ag.ID, "", "  "}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	e.requireShareState("shared", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
}

// --- raise: who may ----------------------------------------------------------

// TestRaiseVendorAccountShareLevelNeedsTheOwnerAndACurrentAdminRole: raise is
// owner + admin, and "admin" is read NOW. A plain-user owner, a stale admin token of
// a since-demoted owner, and an admin user whose token carries no admin scope are all
// turned away with 403 (they can see their own account, so it is not a 404); a
// non-owner is a 404 unless they can already see the account (a CM at shared), who
// gets the 403. System scope never bypasses it.
func TestRaiseVendorAccountShareLevelNeedsTheOwnerAndACurrentAdminRole(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	groups := []string{e.ag.ID}

	t.Run("plain user owner", func(t *testing.T) {
		acc := e.accounts[routing.VendorShareLevelNone]
		before := e.snapshot(acc.ID)
		for name, tok := range map[string]auth.Token{
			"no admin scope":                  token(vendorAuthzOwnerID),
			"stale admin scope, role is user": token(vendorAuthzOwnerID, "admin"),
		} {
			_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, tok, acc.ID, routing.VendorShareLevelShared, groups)
			requireAuthzOutcome(t, name, err, authzForbidden)
		}
		e.requireUnchanged("a refused raise", before)
	})

	t.Run("owner demoted after the token was issued", func(t *testing.T) {
		acc := e.apiKeyAccount("demoted")
		before := e.snapshot(acc.ID)
		e.setUserRole(vendorLifecycleAdminID, "user")
		_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups)
		e.setUserRole(vendorLifecycleAdminID, "admin")
		requireAuthzOutcome(t, "demoted owner with a stale admin token", err, authzForbidden)
		e.requireUnchanged("a refused raise", before)
		// Back in the admin role the very same call goes through.
		if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups); err != nil {
			t.Fatalf("raise after re-promotion: %v", err)
		}
	})

	t.Run("admin owner whose token has no admin scope", func(t *testing.T) {
		acc := e.apiKeyAccount("no-scope")
		before := e.snapshot(acc.ID)
		_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, token(vendorLifecycleAdminID), acc.ID, routing.VendorShareLevelShared, groups)
		requireAuthzOutcome(t, "inference-only token of an admin", err, authzForbidden)
		e.requireUnchanged("a refused raise", before)
	})

	t.Run("non-owners", func(t *testing.T) {
		private := e.apiKeyAccount("non-owners-none")
		shared := e.apiKeyAccount("non-owners-shared")
		if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, shared.ID, routing.VendorShareLevelShared, groups); err != nil {
			t.Fatalf("setup raise: %v", err)
		}
		cases := []struct {
			name string
			tok  auth.Token
			acc  routing.VendorAccount
			want authzOutcome
		}{
			{"stranger at none", token("usr_stranger"), private, authzNotFound},
			{"admin who is nobody's CM at none", token("usr_cmx", "admin"), private, authzNotFound},
			{"system scope at none", token("usr_s", "system", "admin"), private, authzNotFound},
			{"CM of AG at none (no reach yet)", token("usr_cm", "admin"), private, authzNotFound},
			{"no user identity", auth.Token{Scopes: []string{"gateway:use", "admin"}}, private, authzNotFound},
			{"stranger at shared", token("usr_stranger"), shared, authzNotFound},
			{"system scope at shared", token("usr_s", "system", "admin"), shared, authzNotFound},
			// A CM can see a shared account, so the refusal is a 403, never a 404.
			{"CM at shared", token("usr_cm", "admin"), shared, authzForbidden},
		}
		for _, tc := range cases {
			before := e.snapshot(tc.acc.ID)
			_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, tc.tok, tc.acc.ID, routing.VendorShareLevelFull, groups)
			requireAuthzOutcome(t, tc.name, err, tc.want)
			e.requireUnchanged(tc.name, before)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, "va_missing", routing.VendorShareLevelShared, groups)
		requireAuthzOutcome(t, "unknown id", err, authzNotFound)
	})
}

// --- raise: the management groups ---------------------------------------------

// TestRaiseVendorAccountShareLevelNeedsAtLeastOneManagementGroup: an empty set (nil,
// empty, or only blanks) is refused, whatever the target level, and writes nothing.
func TestRaiseVendorAccountShareLevelNeedsAtLeastOneManagementGroup(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("no-groups")
	before := e.snapshot(acc.ID)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for name, groups := range map[string][]string{"nil": nil, "empty": {}, "blanks": {"", "   "}} {
			_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, level, groups)
			if !errors.Is(err, ErrVendorAccountAdminGroupRequired) {
				t.Errorf("%s -> %s with %s groups: err = %v, want ErrVendorAccountAdminGroupRequired", acc.ShareLevel, level, name, err)
			}
		}
	}
	e.requireUnchanged("a raise without a group", before)
	e.requirePrivate("still private", acc.ID)
}

// TestRaiseVendorAccountShareLevelEnforcesTheGroupScope: a non-system caller names
// only groups they manage vendor accounts through (AG as a flagged co-manager, AG-B
// as owner), every group must be an existing ADMIN-tier group, and all share one
// parent. A system-scope owner is not limited to a manage set.
func TestRaiseVendorAccountShareLevelEnforcesTheGroupScope(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	ag2 := e.mustCreateGroup(token("usr_agowner2", "admin"), CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2-b", ParentGroupID: e.rootID})
	sysUserGroup := e.mustCreateGroup(token("usr_agowner", "admin"), CreateGroupInput{Tier: store.GroupTierUser, Name: "Users", ParentGroupID: e.ag.ID})

	acc := e.apiKeyAccount("scope")
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
		{"a user-tier group", []string{sysUserGroup.ID}, ErrVendorAccountAdminGroupInvalid},
		{"groups under different parents", []string{e.ag.ID, e.agX.ID}, ErrVendorAccountAdminGroupParentMismatch},
	}
	for _, tc := range cases {
		_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, tc.groups)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	e.requireUnchanged("a raise with a bad group set", before)
	e.requirePrivate("still private", acc.ID)

	// The root follows the groups: AG-X alone sits under the other system group.
	other := e.apiKeyAccount("scope-other-root")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, other.ID, routing.VendorShareLevelShared, []string{e.agX.ID}); err != nil {
		t.Fatalf("raise into AG-X: %v", err)
	}
	e.requireShareState("other root", other.ID, routing.VendorShareLevelShared, e.otherRootID, e.agX.ID)

	// A system-scope OWNER skips the manage-set check (but not the tier / parent ones).
	sysOwned, err := e.svc.CreateVendorAccount(e.ctx, token("usr_s", "system", "admin"), CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "system-owned"})
	if err != nil {
		t.Fatalf("create as system: %v", err)
	}
	sys := token("usr_s", "system", "admin")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, sys, sysOwned.ID, routing.VendorShareLevelShared, []string{e.ag.ID, e.agX.ID}); !errors.Is(err, ErrVendorAccountAdminGroupParentMismatch) {
		t.Errorf("system owner, mixed parents: err = %v, want ErrVendorAccountAdminGroupParentMismatch", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, sys, sysOwned.ID, routing.VendorShareLevelShared, []string{ag2.ID}); err != nil {
		t.Fatalf("system owner naming a group outside any manage set: %v", err)
	}
	e.requireShareState("system owner", sysOwned.ID, routing.VendorShareLevelShared, e.rootID, ag2.ID)
}

// The containment root is immutable while the account stays shared: a shared -> full
// raise whose groups sit under ANOTHER parent is refused, and nothing moves.
func TestRaiseVendorAccountShareLevelKeepsTheContainmentRoot(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("root")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise to shared: %v", err)
	}
	before := e.snapshot(acc.ID)
	_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, []string{e.agX.ID})
	if !errors.Is(err, ErrVendorAccountRootMismatch) {
		t.Fatalf("shared -> full under another root: err = %v, want ErrVendorAccountRootMismatch", err)
	}
	e.requireUnchanged("a root-moving raise", before)
	e.requireShareState("kept", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
}

// --- raise: the level, and what a half-finished raise leaves ------------------

func TestRaiseVendorAccountShareLevelRefusesNonRaises(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	groups := []string{e.ag.ID}
	acc := e.apiKeyAccount("non-raise")

	for _, level := range []string{"", "bogus", "SHARED"} {
		if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, level, groups); !errors.Is(err, ErrVendorAccountShareLevelInvalid) {
			t.Errorf("raise to %q: err = %v, want ErrVendorAccountShareLevelInvalid", level, err)
		}
	}
	// `none` is a level, but not a raise.
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelNone, groups); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
		t.Errorf("raise to none: err = %v, want ErrVendorAccountShareLevelUnchanged", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups); err != nil {
		t.Fatalf("raise to shared: %v", err)
	}
	// Already at (or above) the requested level: nothing to raise.
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
		t.Errorf("shared -> shared: err = %v, want ErrVendorAccountShareLevelUnchanged", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, groups); err != nil {
		t.Fatalf("raise to full: %v", err)
	}
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, level, groups); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
			t.Errorf("full -> %s via raise: err = %v, want ErrVendorAccountShareLevelUnchanged", level, err)
		}
	}
	e.requireShareState("unchanged", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)

	// An account whose stored level is not one we know is private: a raise treats it as none.
	corrupt := e.apiKeyAccount("corrupt-level")
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, corrupt.ID, "bogus"); err != nil {
		t.Fatalf("corrupt level: %v", err)
	}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, corrupt.ID, routing.VendorShareLevelShared, groups); err != nil {
		t.Fatalf("raise from an unrecognised level: %v", err)
	}
	e.requireShareState("healed", corrupt.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
}

// The level is written LAST: a raise that fails while linking the groups leaves the
// account at `none` (private), never shared with a partial group set. Links and a
// root that were already written are harmless (every gate reads the level first)
// and are overwritten by the next raise.
func TestRaiseVendorAccountShareLevelWritesTheLevelLast(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	groups := []string{e.ag.ID, e.agB.ID}

	acc := e.apiKeyAccount("torn-groups")
	e.svc.routes = &failingVendorStore{Store: e.svc.routes, failGroupLinkAt: 2}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups); !errors.Is(err, errInjectedStoreFailure) {
		t.Fatalf("raise with a failing group link: err = %v, want the injected failure", err)
	}
	if got := e.reload(acc.ID); got.ShareLevel != routing.VendorShareLevelNone {
		t.Fatalf("share_level after a failed raise = %q, want %q (the level is written last)", got.ShareLevel, routing.VendorShareLevelNone)
	}
	// A CM sees nothing of an account that is still `none`, links or not.
	requireAuthzOutcome(t, "CM after a torn raise", e.svc.authorizeVendorAccountOperate(e.ctx, token("usr_cm", "admin"), e.reload(acc.ID)), authzNotFound)

	// The same raise, retried on a healthy store, completes.
	e.svc.routes = e.routes
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, groups); err != nil {
		t.Fatalf("retry: %v", err)
	}
	e.requireShareState("retried", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID, e.agB.ID)

	// And a failure on the level write itself leaves the account private as well.
	last := e.apiKeyAccount("torn-level")
	e.svc.routes = &failingVendorStore{Store: e.svc.routes, failShareLevel: true}
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, last.ID, routing.VendorShareLevelShared, groups); !errors.Is(err, errInjectedStoreFailure) {
		t.Fatalf("raise with a failing level write: err = %v, want the injected failure", err)
	}
	e.svc.routes = e.routes
	if got := e.reload(last.ID); got.ShareLevel != routing.VendorShareLevelNone {
		t.Fatalf("share_level after a failed level write = %q, want none", got.ShareLevel)
	}
}

// --- the subscription cap and the two flags (spec 11, 13) --------------------

// TestRaiseVendorAccountShareLevelSubscriptionCap: a SUBSCRIPTION account is raised
// only to `shared` and only while vendor_account_sharing_enabled AND
// vendor_subscription_sharing_enabled are on (the second is subordinate to the
// master); an api_key account never looks at the subscription flag.
func TestRaiseVendorAccountShareLevelSubscriptionCap(t *testing.T) {
	cases := []struct {
		name          string
		subscription  bool
		master, subOn bool
		target        string
		want          error
	}{
		{"subscription -> shared, both flags on", true, true, true, routing.VendorShareLevelShared, nil},
		{"subscription -> full is capped, both flags on", true, true, true, routing.VendorShareLevelFull, ErrVendorAccountSubscriptionShareCapped},
		{"subscription -> shared, subscription flag off", true, true, false, routing.VendorShareLevelShared, ErrVendorSubscriptionSharingDisabled},
		{"subscription -> full, subscription flag off (the cap answers first)", true, true, false, routing.VendorShareLevelFull, ErrVendorAccountSubscriptionShareCapped},
		{"subscription -> shared, subscription flag on but master off", true, false, true, routing.VendorShareLevelShared, ErrVendorAccountSharingDisabled},
		{"api_key -> shared, subscription flag off", false, true, false, routing.VendorShareLevelShared, nil},
		{"api_key -> full, subscription flag off", false, true, false, routing.VendorShareLevelFull, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVendorLifecycleEnv(t)
			var acc routing.VendorAccount
			if tc.subscription {
				acc = e.subscriptionAccount("sub")
			} else {
				acc = e.apiKeyAccount("key")
			}
			setVendorSharingFlags(t, e.svc, tc.master, tc.subOn)
			before := e.snapshot(acc.ID)

			_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, tc.target, []string{e.ag.ID})
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				e.requireUnchanged("a refused raise", before)
				e.requirePrivate("still private", acc.ID)
				return
			}
			if err != nil {
				t.Fatalf("raise: %v", err)
			}
			e.requireShareState("raised", acc.ID, tc.target, e.rootID, e.ag.ID)
		})
	}
}

// A subscription account can ALWAYS be lowered, whatever the flags say: an owner must
// be able to un-share even after the operator switched subscription sharing (or all
// sharing... see TestShareLevelEndpointsAreRefusedWhileSharingIsOff) off.
func TestLowerVendorAccountShareLevelIgnoresTheSubscriptionFlag(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	setVendorSharingFlags(t, e.svc, true, true)
	acc := e.subscriptionAccount("sub-lower")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	setVendorSharingFlags(t, e.svc, true, false)
	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelNone); err != nil {
		t.Fatalf("lower with subscription sharing off: %v", err)
	}
	e.requirePrivate("lowered", acc.ID)
}

// TestShareLevelEndpointsAreRefusedWhileSharingIsOff: with the master sharing flag
// off, raise, lower and the endpoint's dispatcher are ALL a 409
// (ErrVendorAccountSharingDisabled), for the owner, a stranger and an id that does not
// exist alike (the answer is global, so it reveals nothing about an account), and
// nothing is written. The vendor-accounts MODULE flag still answers first.
func TestShareLevelEndpointsAreRefusedWhileSharingIsOff(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("flag-off")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise while on: %v", err)
	}
	setVendorSharingFlags(t, e.svc, false, true)

	calls := map[string]func(p auth.Token, id string) error{
		"raise": func(p auth.Token, id string) error {
			_, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, p, id, routing.VendorShareLevelFull, []string{e.ag.ID})
			return err
		},
		"lower": func(p auth.Token, id string) error {
			_, err := e.svc.LowerVendorAccountShareLevel(e.ctx, p, id, routing.VendorShareLevelNone)
			return err
		},
		"set (raise)": func(p auth.Token, id string) error {
			_, err := e.svc.SetVendorAccountShareLevel(e.ctx, p, id, SetVendorAccountShareLevelRequest{Level: routing.VendorShareLevelFull, AdminGroupIDs: []string{e.ag.ID}})
			return err
		},
		"set (lower)": func(p auth.Token, id string) error {
			_, err := e.svc.SetVendorAccountShareLevel(e.ctx, p, id, SetVendorAccountShareLevelRequest{Level: routing.VendorShareLevelNone})
			return err
		},
	}
	before := e.snapshot(acc.ID)
	for name, call := range calls {
		for who, tok := range map[string]auth.Token{"owner": e.admin, "stranger": token("usr_stranger"), "system": token("usr_s", "system", "admin")} {
			for _, id := range []string{acc.ID, "va_missing"} {
				if err := call(tok, id); !errors.Is(err, ErrVendorAccountSharingDisabled) {
					t.Errorf("%s as %s on %s: err = %v, want ErrVendorAccountSharingDisabled", name, who, id, err)
				}
			}
		}
	}
	e.requireUnchanged("calls refused while sharing is off", before)
	e.requireShareState("untouched", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)

	// The module flag is the more fundamental switch and answers first.
	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	for name, call := range calls {
		if err := call(e.admin, acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
			t.Errorf("%s with the module off: err = %v, want ErrVendorAccountsDisabled", name, err)
		}
	}
}

// The helper Tasks 2.5 and 3.1 reuse: the vendor-accounts module flag first, then the
// sharing flag; and the subscription helper is the AND of both sharing flags.
func TestRequireVendorAccountSharingEnabled(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	cases := []struct {
		module, sharing, subSharing bool
		wantSharing                 error
		wantSubscription            error
	}{
		{true, true, true, nil, nil},
		{true, true, false, nil, ErrVendorSubscriptionSharingDisabled},
		{true, false, true, ErrVendorAccountSharingDisabled, ErrVendorAccountSharingDisabled},
		{true, false, false, ErrVendorAccountSharingDisabled, ErrVendorAccountSharingDisabled},
		{false, true, true, ErrVendorAccountsDisabled, ErrVendorAccountsDisabled},
	}
	sameError := func(got, want error) bool {
		if want == nil {
			return got == nil
		}
		return errors.Is(got, want)
	}
	for _, tc := range cases {
		setVendorSetting(t, e.svc, vendorAccountsEnabledKey, tc.module)
		setVendorSharingFlags(t, e.svc, tc.sharing, tc.subSharing)
		if err := e.svc.requireVendorAccountSharingEnabled(e.ctx); !sameError(err, tc.wantSharing) {
			t.Errorf("module=%v sharing=%v: requireVendorAccountSharingEnabled = %v, want %v", tc.module, tc.sharing, err, tc.wantSharing)
		}
		if err := e.svc.requireVendorSubscriptionSharingEnabled(e.ctx); !sameError(err, tc.wantSubscription) {
			t.Errorf("module=%v sharing=%v sub=%v: requireVendorSubscriptionSharingEnabled = %v, want %v", tc.module, tc.sharing, tc.subSharing, err, tc.wantSubscription)
		}
	}
	// No settings store at all fails closed.
	e.svc.settings = nil
	if err := e.svc.requireVendorAccountSharingEnabled(e.ctx); err == nil {
		t.Error("requireVendorAccountSharingEnabled without a settings store = nil, want a refusal")
	}
}

// --- lower ---------------------------------------------------------------------

// TestLowerVendorAccountShareLevelUsesTheCredentialGateShape: lowering to none is the
// CREDENTIAL gate's row (spec 6): the owner always, a CM at `full` only, a CM at
// `shared` is 403 (they can see the account), every other caller -- system scope
// included -- is the 404, and a denied call writes nothing. The owner here is a plain
// user: lowering needs ownership, not the admin role.
func TestLowerVendorAccountShareLevelUsesTheCredentialGateShape(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, p := range vendorAuthzPrincipals() {
			label := "lower " + level + " -> none/" + p.name
			acc := e.newAccountAt(level)
			if level != routing.VendorShareLevelNone {
				e.linkToResourceGroup(acc.ID)
			}
			before := e.snapshot(acc.ID)
			dto, err := e.svc.LowerVendorAccountShareLevel(e.ctx, p.tok, acc.ID, routing.VendorShareLevelNone)
			if vendorGateAllowed(true, level, p.name) {
				if err != nil {
					t.Errorf("%s: err = %v, want allow", label, err)
					continue
				}
				if dto.ID != acc.ID {
					t.Errorf("%s: dto id = %q, want %q", label, dto.ID, acc.ID)
				}
				e.requirePrivate(label, acc.ID)
				continue
			}
			requireAuthzOutcome(t, label, err, vendorGateDenial(true, level, p.name))
			e.requireUnchanged(label, before)
			if level != routing.VendorShareLevelNone && len(e.resourceGroupLinks(acc.ID)) != 1 {
				t.Errorf("%s: the denied call dropped the resource-group link", label)
			}
		}
	}
}

// Lowering full -> shared only changes the level: the management groups, the root and
// the resource-group links stay. Owner and CM may do it at full; the CM then holds the
// shared-level reach (operate, but no credential).
func TestLowerVendorAccountShareLevelFullToSharedKeepsTheLinks(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for name, tok := range map[string]auth.Token{"owner": token(vendorAuthzOwnerID), "cm": token("usr_cm", "admin")} {
		acc := e.newAccountAt(routing.VendorShareLevelFull)
		rgID := e.linkToResourceGroup(acc.ID)
		if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, tok, acc.ID, routing.VendorShareLevelShared); err != nil {
			t.Fatalf("%s lowering full -> shared: %v", name, err)
		}
		e.requireShareState(name+" full -> shared", acc.ID, routing.VendorShareLevelShared, e.ag.ParentGroupID, e.ag.ID)
		if links := e.resourceGroupLinks(acc.ID); !slices.Equal(links, []string{rgID}) {
			t.Errorf("%s: resource-group links after full -> shared = %v, want [%s]", name, links, rgID)
		}
		requireAuthzOutcome(t, name+": CM credential gate after the lower", e.svc.authorizeVendorAccountCredential(e.ctx, token("usr_cm", "admin"), e.reload(acc.ID)), authzForbidden)
		requireAuthzOutcome(t, name+": CM operate gate after the lower", e.svc.authorizeVendorAccountOperate(e.ctx, token("usr_cm", "admin"), e.reload(acc.ID)), authzAllow)
	}

	// A CM at `shared` may not lower it (403) -- not even to shared's own cousin.
	shared := e.newAccountAt(routing.VendorShareLevelShared)
	_, err := e.svc.LowerVendorAccountShareLevel(e.ctx, token("usr_cm", "admin"), shared.ID, routing.VendorShareLevelNone)
	requireAuthzOutcome(t, "CM lowering a shared account", err, authzForbidden)
}

// A downgrade requires only OWNERSHIP at `shared` (spec 6, 7): an owner who lost the
// admin role since raising must still be able to un-share.
func TestLowerVendorAccountShareLevelNeedsNoAdminRole(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("demote-then-lower")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.ag.ID, e.agB.ID}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	rgID := e.linkToResourceGroup(acc.ID)
	e.setUserRole(vendorLifecycleAdminID, "user")

	// The very same (now stale) token: raising is refused, lowering is not.
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, []string{e.ag.ID}); !errors.Is(err, ErrVendorAccountForbidden) {
		t.Fatalf("raise by the demoted owner: err = %v, want ErrVendorAccountForbidden", err)
	}
	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, token(vendorLifecycleAdminID), acc.ID, routing.VendorShareLevelNone); err != nil {
		t.Fatalf("lower by the demoted owner: %v", err)
	}
	e.requirePrivate("lowered by a demoted owner", acc.ID)
	if links := e.resourceGroupLinks(acc.ID); len(links) != 0 {
		t.Errorf("resource-group links %v (was %s) survived the downgrade", links, rgID)
	}
}

// TestLowerVendorAccountShareLevelToNoneClearsLinksAndRoot is the store-level proof
// of the downgrade: both link sets and the root are gone, and the level is none.
func TestLowerVendorAccountShareLevelToNoneClearsLinksAndRoot(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("clear")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelFull, []string{e.ag.ID, e.agB.ID}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	rgID := e.linkToResourceGroup(acc.ID)
	e.requireShareState("before", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID, e.agB.ID)
	if links := e.resourceGroupLinks(acc.ID); !slices.Equal(links, []string{rgID}) {
		t.Fatalf("setup: resource-group links = %v, want [%s]", links, rgID)
	}

	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelNone); err != nil {
		t.Fatalf("lower to none: %v", err)
	}
	e.requirePrivate("after", acc.ID)
	// A fresh raise starts from a clean slate: a different parent is fine again.
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelShared, []string{e.agX.ID}); err != nil {
		t.Fatalf("re-raise under another root: %v", err)
	}
	e.requireShareState("re-raised", acc.ID, routing.VendorShareLevelShared, e.otherRootID, e.agX.ID)
}

// Lowering to none is idempotent, and also scrubs stale links an account at `none` may
// still carry (a dangling memory-mode entry, a torn raise).
func TestLowerVendorAccountShareLevelToNoneIsIdempotentAndHeals(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("idempotent")
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("stale link: %v", err)
	}
	e.linkToResourceGroup(acc.ID)
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, acc.ID, e.rootID); err != nil {
		t.Fatalf("stale root: %v", err)
	}
	for i := range 2 {
		if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, e.admin, acc.ID, routing.VendorShareLevelNone); err != nil {
			t.Fatalf("lower #%d: %v", i, err)
		}
		e.requirePrivate("healed", acc.ID)
	}
}

func TestLowerVendorAccountShareLevelRefusesNonLowers(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	owner := token(vendorAuthzOwnerID)
	shared := e.newAccountAt(routing.VendorShareLevelShared)
	none := e.newAccountAt(routing.VendorShareLevelNone)

	for _, level := range []string{"", "bogus", "None"} {
		if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, owner, shared.ID, level); !errors.Is(err, ErrVendorAccountShareLevelInvalid) {
			t.Errorf("lower to %q: err = %v, want ErrVendorAccountShareLevelInvalid", level, err)
		}
	}
	// Not a lowering: the account is already at (or below) the level.
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, owner, shared.ID, level); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
			t.Errorf("shared -> %s via lower: err = %v, want ErrVendorAccountShareLevelUnchanged", level, err)
		}
	}
	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, owner, none.ID, routing.VendorShareLevelShared); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
		t.Errorf("none -> shared via lower: err = %v, want ErrVendorAccountShareLevelUnchanged", err)
	}
	e.requireShareState("untouched", shared.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)

	// A stranger is told 404 before any of these validations.
	if _, err := e.svc.LowerVendorAccountShareLevel(e.ctx, token("usr_stranger"), shared.ID, "bogus"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("stranger with a bogus level: err = %v, want ErrVendorAccountNotFound", err)
	}
}

// --- the endpoint's dispatcher --------------------------------------------------

// TestSetVendorAccountShareLevelPicksTheDirection: the endpoint carries only a target
// level (and the groups, for a raise); the service compares it with the stored level
// and runs the raise or the lower, each with its own authorization.
func TestSetVendorAccountShareLevelPicksTheDirection(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	acc := e.apiKeyAccount("dispatch")
	set := func(p auth.Token, id, level string, groups ...string) error {
		_, err := e.svc.SetVendorAccountShareLevel(e.ctx, p, id, SetVendorAccountShareLevelRequest{Level: level, AdminGroupIDs: groups})
		return err
	}

	if err := set(e.admin, acc.ID, routing.VendorShareLevelShared, e.ag.ID); err != nil {
		t.Fatalf("none -> shared: %v", err)
	}
	e.requireShareState("shared", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
	if err := set(e.admin, acc.ID, routing.VendorShareLevelFull, e.ag.ID); err != nil {
		t.Fatalf("shared -> full: %v", err)
	}
	e.requireShareState("full", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)
	if err := set(e.admin, acc.ID, routing.VendorShareLevelShared); err != nil {
		t.Fatalf("full -> shared: %v", err)
	}
	e.requireShareState("lowered", acc.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)
	if err := set(e.admin, acc.ID, routing.VendorShareLevelNone); err != nil {
		t.Fatalf("shared -> none: %v", err)
	}
	e.requirePrivate("private", acc.ID)

	// The same level again is no change; a malformed level is refused before the account is
	// even looked up; a raise by a non-admin owner is 403; a stranger or an unknown id is 404.
	if err := set(e.admin, acc.ID, routing.VendorShareLevelShared, e.ag.ID); err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	if err := set(e.admin, acc.ID, routing.VendorShareLevelShared, e.ag.ID); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
		t.Errorf("shared -> shared: err = %v, want ErrVendorAccountShareLevelUnchanged", err)
	}
	if err := set(e.admin, "va_missing", "bogus"); !errors.Is(err, ErrVendorAccountShareLevelInvalid) {
		t.Errorf("bogus level, unknown id: err = %v, want ErrVendorAccountShareLevelInvalid", err)
	}
	if err := set(e.admin, "va_missing", routing.VendorShareLevelShared, e.ag.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
	if err := set(token("usr_stranger"), acc.ID, routing.VendorShareLevelFull, e.ag.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("stranger raise: err = %v, want ErrVendorAccountNotFound", err)
	}
	plain := e.accounts[routing.VendorShareLevelNone]
	if err := set(token(vendorAuthzOwnerID), plain.ID, routing.VendorShareLevelShared, e.ag.ID); !errors.Is(err, ErrVendorAccountForbidden) {
		t.Errorf("non-admin owner raise: err = %v, want ErrVendorAccountForbidden", err)
	}
	// A CM at `full` sending the level it already has is a lower-shaped no-op (they may lower), not a 403.
	full := e.newAccountAt(routing.VendorShareLevelFull)
	if err := set(token("usr_cm", "admin"), full.ID, routing.VendorShareLevelFull, e.ag.ID); !errors.Is(err, ErrVendorAccountShareLevelUnchanged) {
		t.Errorf("CM at full, same level: err = %v, want ErrVendorAccountShareLevelUnchanged", err)
	}
	// A CM at `shared` asking for `full` is a raise, which is owner + admin only.
	shared := e.newAccountAt(routing.VendorShareLevelShared)
	if err := set(token("usr_cm", "admin"), shared.ID, routing.VendorShareLevelFull, e.ag.ID); !errors.Is(err, ErrVendorAccountForbidden) {
		t.Errorf("CM at shared raising: err = %v, want ErrVendorAccountForbidden", err)
	}
}

// --- delete (carried from the PATCH task: spec 6 Delete row = the credential gate) ----

// TestDeleteVendorAccountUsesTheCredentialGate: delete is owner at none, owner-only at
// shared, owner + CM at full. A CM at shared is 403 (they can see the account), every
// other caller -- system scope included -- is the 404, and a refused delete leaves the
// account, its groups and its resource-group links in place.
func TestDeleteVendorAccountUsesTheCredentialGate(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, p := range vendorAuthzPrincipals() {
			label := "delete " + level + "/" + p.name
			acc := e.newAccountAt(level)
			if level != routing.VendorShareLevelNone {
				e.linkToResourceGroup(acc.ID)
			}
			before := e.snapshot(acc.ID)
			ok, err := e.svc.DeleteVendorAccount(e.ctx, p.tok, acc.ID)
			if vendorGateAllowed(true, level, p.name) {
				if err != nil || !ok {
					t.Errorf("%s: = %v, %v, want deleted", label, ok, err)
				}
				if _, getErr := e.routes.VendorAccountByID(e.ctx, acc.ID); !errors.Is(getErr, store.ErrNotFound) {
					t.Errorf("%s: account still readable after delete: %v", label, getErr)
				}
				if groups := e.adminGroups(acc.ID); len(groups) != 0 {
					t.Errorf("%s: management groups %v survived the delete", label, groups)
				}
				if links := e.resourceGroupLinks(acc.ID); len(links) != 0 {
					t.Errorf("%s: resource-group links %v survived the delete", label, links)
				}
				continue
			}
			if ok {
				t.Errorf("%s: reported a removal for a denied caller", label)
			}
			requireAuthzOutcome(t, label, err, vendorGateDenial(true, level, p.name))
			e.requireUnchanged(label, before)
		}
	}
}
