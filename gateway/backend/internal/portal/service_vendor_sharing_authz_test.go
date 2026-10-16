// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
	"time"
)

// This file is the authorization contract of shared vendor accounts (#183,
// spec 2026-10-15 section 6): the two write gates authorizeVendorAccountOperate
// and authorizeVendorAccountCredential, driven by the account's share level and
// by whether the principal is the owner or a CM (a co-manager holding
// can_manage_vendor_accounts in an admin group the account is assigned to).
//
// The 404-vs-403 split is load-bearing and asserted exactly:
//   - a principal who may not even SEE the account gets ErrVendorAccountNotFound
//     (404-no-leak: a stranger must not learn the account exists);
//   - a CM who can see (and operate) the account at `shared` but may not touch
//     its credential gets ErrVendorAccountForbidden (403): the account is no
//     secret to them, so "not found" would be a lie.
//
// At Phase 2 nobody is a "member" yet (resource-group routing and opt-in are
// Phase 3), so a non-owner non-CM is simply a stranger here.

// authzOutcome is what a gate must answer for one principal.
type authzOutcome int

const (
	authzAllow authzOutcome = iota
	authzNotFound
	authzForbidden
)

func (o authzOutcome) String() string {
	switch o {
	case authzAllow:
		return "allow"
	case authzNotFound:
		return "ErrVendorAccountNotFound (404)"
	default:
		return "ErrVendorAccountForbidden (403)"
	}
}

// requireAuthzOutcome fails unless err is exactly the sentinel want names: the
// 404 and the 403 are mutually exclusive, so a gate that answered the wrong one
// of the two fails even though both are "denied".
func requireAuthzOutcome(t *testing.T, label string, err error, want authzOutcome) {
	t.Helper()
	notFound := errors.Is(err, ErrVendorAccountNotFound)
	forbidden := errors.Is(err, ErrVendorAccountForbidden)
	switch want {
	case authzAllow:
		if err != nil {
			t.Errorf("%s: err = %v, want allow", label, err)
		}
	case authzNotFound:
		if !notFound || forbidden {
			t.Errorf("%s: err = %v, want exactly %s", label, err, want)
		}
	case authzForbidden:
		if !forbidden || notFound {
			t.Errorf("%s: err = %v, want exactly %s", label, err, want)
		}
	}
}

// vendorAuthzEnv is a group/user fixture with one admin group AG (owned by
// usr_agowner), a CM (can_manage_vendor_accounts), a co-manager WITHOUT that
// flag, and one vendor account per share level, all owned by usr_va_owner.
type vendorAuthzEnv struct {
	*groupTestEnv
	ag          UserGroupDTO
	accounts    map[string]routing.VendorAccount // keyed by share level
	discoverers *fakeVendorDiscoverers           // the vendor fetchers every operate call runs through
}

const vendorAuthzOwnerID = "usr_va_owner"

func newVendorAuthzEnv(t *testing.T) *vendorAuthzEnv {
	t.Helper()
	e := &vendorAuthzEnv{groupTestEnv: newGroupTestEnv(t), accounts: map[string]routing.VendorAccount{}}
	setVendorAccountsEnabled(t, e.svc, true)
	installFakeVendorValidators(e.svc)
	e.discoverers = installFakeVendorDiscoverers(e.svc)

	e.createUser("usr_s", "system_admin")
	e.createUser("usr_agowner", "admin")
	e.createUser("usr_cm", "admin")
	e.createUser("usr_cmx", "admin")
	e.createUser("usr_agowner2", "admin")
	e.createUser("usr_cm2", "admin")
	e.createUser("usr_stranger", "user")
	e.createUser(vendorAuthzOwnerID, "user")

	sysAdmin := token("usr_s", "system", "admin")
	agOwner := token("usr_agowner", "admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_agowner", "usr_cm", "usr_cmx", "usr_agowner2", "usr_cm2")
	e.ag = e.mustCreateGroup(agOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(agOwner, e.ag.ID, "usr_cm", "usr_cmx")
	// AG2 is a second admin group under the same system group, with its own owner
	// and a CM that holds the vendor flag -- but no account is ever assigned to it,
	// so neither may reach any account below.
	agOwner2 := token("usr_agowner2", "admin")
	ag2 := e.mustCreateGroup(agOwner2, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})
	e.mustAddMembers(agOwner2, ag2.ID, "usr_cm2")
	if err := e.svc.PromoteManager(e.ctx, agOwner2, ag2.ID, "usr_cm2", ManagerPermissions{CanManageVendorAccounts: true}); err != nil {
		t.Fatalf("promote usr_cm2: %v", err)
	}
	// usr_cm holds can_manage_vendor_accounts; usr_cmx holds every OTHER flag, so
	// it is a co-manager of the very same group with no vendor-account reach.
	if err := e.svc.PromoteManager(e.ctx, agOwner, e.ag.ID, "usr_cm", ManagerPermissions{CanManageVendorAccounts: true}); err != nil {
		t.Fatalf("promote usr_cm: %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, agOwner, e.ag.ID, "usr_cmx", ManagerPermissions{
		CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true,
	}); err != nil {
		t.Fatalf("promote usr_cmx: %v", err)
	}

	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		e.accounts[level] = e.newAccountAt(level)
	}
	return e
}

// newAccountAt creates an api-key account owned by usr_va_owner through the
// service (so it takes the real create path) and puts it at level. A shared/full
// account is assigned to AG with a containment root, exactly the state the
// lifecycle will produce; a `none` account has neither.
func (e *vendorAuthzEnv) newAccountAt(level string) routing.VendorAccount {
	e.t.Helper()
	return e.newAccountOf(level, CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "authz-" + level,
	})
}

// newAccountOf is newAccountAt for any account shape: req is created through the
// service as usr_va_owner and then put at level.
func (e *vendorAuthzEnv) newAccountOf(level string, req CreateVendorAccountRequest) routing.VendorAccount {
	e.t.Helper()
	dto, err := e.svc.CreateVendorAccount(e.ctx, token(vendorAuthzOwnerID), req)
	if err != nil {
		e.t.Fatalf("CreateVendorAccount(%s): %v", level, err)
	}
	if level != routing.VendorShareLevelNone {
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, dto.ID, level); err != nil {
			e.t.Fatalf("raise %s to %s: %v", dto.ID, level, err)
		}
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, dto.ID, e.ag.ID); err != nil {
			e.t.Fatalf("assign %s to AG: %v", dto.ID, err)
		}
		if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, dto.ID, e.ag.ParentGroupID); err != nil {
			e.t.Fatalf("set root of %s: %v", dto.ID, err)
		}
	}
	return e.reload(dto.ID)
}

func (e *vendorAuthzEnv) reload(id string) routing.VendorAccount {
	e.t.Helper()
	acc, err := e.routes.VendorAccountByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountByID(%s): %v", id, err)
	}
	return acc
}

// vendorAccountState is everything a denied call could have written for one
// account: the row (credentials included), its management groups, its model
// catalog and its usage snapshot.
type vendorAccountState struct {
	Account  routing.VendorAccount
	Groups   []string
	Models   []routing.VendorAccountModel
	Usage    routing.VendorAccountUsage
	HasUsage bool
}

func (e *vendorAuthzEnv) snapshot(id string) vendorAccountState {
	e.t.Helper()
	groups, err := e.routes.VendorAccountAdminGroups(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountAdminGroups(%s): %v", id, err)
	}
	models, err := e.routes.VendorAccountModels(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountModels(%s): %v", id, err)
	}
	usage, hasUsage, err := e.routes.VendorAccountUsageByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountUsageByID(%s): %v", id, err)
	}
	return vendorAccountState{Account: e.reload(id), Groups: groups, Models: models, Usage: usage, HasUsage: hasUsage}
}

func (e *vendorAuthzEnv) requireUnchanged(label string, before vendorAccountState) {
	e.t.Helper()
	after := e.snapshot(before.Account.ID)
	if !reflect.DeepEqual(before, after) {
		e.t.Errorf("%s changed state although it was denied:\n before %+v\n after  %+v", label, before, after)
	}
}

// vendorAuthzPrincipals are the callers the matrix runs. usr_agowner owns AG, and
// manageGroupIDs counts a group OWNER as holding every co-manager permission,
// so it is a CM too (by the primitive's own definition, not by a flag).
func vendorAuthzPrincipals() []struct {
	name string
	tok  auth.Token
} {
	return []struct {
		name string
		tok  auth.Token
	}{
		{"owner", token(vendorAuthzOwnerID)},
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

// TestVendorAccountAuthorizationMatrix is the spec section 6 table, for both write
// gates, at every share level, for every kind of principal. Reading it row by row
// is the review: operate = owner OR (shared/full AND CM); credential = owner OR
// (full AND CM), with a CM at `shared` answered 403 rather than 404.
func TestVendorAccountAuthorizationMatrix(t *testing.T) {
	e := newVendorAuthzEnv(t)

	type row struct {
		operate, credential authzOutcome
	}
	// want[level][principal name]
	want := map[string]map[string]row{
		routing.VendorShareLevelNone: {
			// At `none` the account has no management group, so no CM exists:
			// owner-only falls out, and every other caller is a stranger.
			"owner":                              {authzAllow, authzAllow},
			"cm":                                 {authzNotFound, authzNotFound},
			"admin-group owner":                  {authzNotFound, authzNotFound},
			"co-manager without the vendor flag": {authzNotFound, authzNotFound},
			"CM of another admin group":          {authzNotFound, authzNotFound},
			"owner of another admin group":       {authzNotFound, authzNotFound},
			"stranger":                           {authzNotFound, authzNotFound},
			"system scope":                       {authzNotFound, authzNotFound},
			"no user identity":                   {authzNotFound, authzNotFound},
		},
		routing.VendorShareLevelShared: {
			// A CM operates but cannot touch the credential: 403, not 404 (the CM
			// can see the account).
			"owner":                              {authzAllow, authzAllow},
			"cm":                                 {authzAllow, authzForbidden},
			"admin-group owner":                  {authzAllow, authzForbidden},
			"co-manager without the vendor flag": {authzNotFound, authzNotFound},
			"CM of another admin group":          {authzNotFound, authzNotFound},
			"owner of another admin group":       {authzNotFound, authzNotFound},
			"stranger":                           {authzNotFound, authzNotFound},
			"system scope":                       {authzNotFound, authzNotFound},
			"no user identity":                   {authzNotFound, authzNotFound},
		},
		routing.VendorShareLevelFull: {
			// `full` is complete owner-equivalent delegation to a CM.
			"owner":                              {authzAllow, authzAllow},
			"cm":                                 {authzAllow, authzAllow},
			"admin-group owner":                  {authzAllow, authzAllow},
			"co-manager without the vendor flag": {authzNotFound, authzNotFound},
			"CM of another admin group":          {authzNotFound, authzNotFound},
			"owner of another admin group":       {authzNotFound, authzNotFound},
			"stranger":                           {authzNotFound, authzNotFound},
			"system scope":                       {authzNotFound, authzNotFound},
			"no user identity":                   {authzNotFound, authzNotFound},
		},
	}

	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		// The gates take the account as the store RETURNS it, so a share_level that
		// is not readable off the record would make every row below fail.
		acc := e.accounts[level]
		if acc.ShareLevel != level {
			t.Fatalf("account read back share_level = %q, want %q", acc.ShareLevel, level)
		}
		for _, p := range vendorAuthzPrincipals() {
			w, ok := want[level][p.name]
			if !ok {
				t.Fatalf("no expectation for %s at %s", p.name, level)
			}
			before := e.snapshot(acc.ID)

			requireAuthzOutcome(t, "operate/"+level+"/"+p.name, e.svc.authorizeVendorAccountOperate(e.ctx, p.tok, acc), w.operate)
			requireAuthzOutcome(t, "credential/"+level+"/"+p.name, e.svc.authorizeVendorAccountCredential(e.ctx, p.tok, acc), w.credential)

			// The gates are decisions, not actions: nothing about the account moved.
			e.requireUnchanged("the gates for "+p.name+" at "+level, before)
		}
	}
}

// TestVendorAccountCredentialGateNeverLeaksAStrangerAs403 pins the half of the
// split that is easy to get backwards: only a principal who can already SEE the
// account (a CM at shared) may be told "forbidden". Everyone else must get the
// identical 404 an unknown id gets, at every level.
func TestVendorAccountCredentialGateNeverLeaksAStrangerAs403(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cannotSee := map[string]bool{
		"stranger": true, "co-manager without the vendor flag": true, "system scope": true, "no user identity": true,
		"CM of another admin group": true, "owner of another admin group": true,
	}
	for level, acc := range e.accounts {
		for _, p := range vendorAuthzPrincipals() {
			if !cannotSee[p.name] {
				continue
			}
			err := e.svc.authorizeVendorAccountCredential(e.ctx, p.tok, acc)
			if errors.Is(err, ErrVendorAccountForbidden) {
				t.Errorf("%s at %s learned the account exists (403): %v", p.name, level, err)
			}
			if !errors.Is(err, ErrVendorAccountNotFound) {
				t.Errorf("%s at %s: err = %v, want ErrVendorAccountNotFound", p.name, level, err)
			}
		}
	}
}

// TestVendorAccountGatesFailClosedWithoutALiveSharedLevel covers a CM link that
// outlives the level that justified it. The CM is derived from the admin-group
// link, but the link alone grants nothing: the account's own share_level decides
// whether a non-owner can see it at all, and an unrecognised level is `none`.
func TestVendorAccountGatesFailClosedWithoutALiveSharedLevel(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cm := token("usr_cm", "admin")

	for _, level := range []string{routing.VendorShareLevelNone, "", "bogus"} {
		acc := e.newAccountAt(routing.VendorShareLevelNone)
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil { // a stale link
			t.Fatalf("link: %v", err)
		}
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, level); err != nil {
			t.Fatalf("set level %q: %v", level, err)
		}
		acc = e.reload(acc.ID)

		requireAuthzOutcome(t, "operate/stale link at "+level, e.svc.authorizeVendorAccountOperate(e.ctx, cm, acc), authzNotFound)
		// Not 403: a CM with a stale link still cannot see the account.
		requireAuthzOutcome(t, "credential/stale link at "+level, e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), authzNotFound)
		// The owner is unaffected.
		requireAuthzOutcome(t, "operate/owner at "+level, e.svc.authorizeVendorAccountOperate(e.ctx, token(vendorAuthzOwnerID), acc), authzAllow)
		requireAuthzOutcome(t, "credential/owner at "+level, e.svc.authorizeVendorAccountCredential(e.ctx, token(vendorAuthzOwnerID), acc), authzAllow)
	}
}

// TestVendorAccountGatesRevokeAfterAdminGroupRemoval proves the CM reach is the
// LIVE intersection of the account's admin groups and the principal's managed
// groups, not something cached on the account: unassign the group (or demote the
// co-manager) and the CM is a stranger again on the very next call.
func TestVendorAccountGatesRevokeAfterAdminGroupRemoval(t *testing.T) {
	e := newVendorAuthzEnv(t)
	acc := e.accounts[routing.VendorShareLevelFull]
	cm := token("usr_cm", "admin")
	requireAuthzOutcome(t, "credential before", e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), authzAllow)

	// Demoting the co-manager (the flag off, every other flag on) removes the reach.
	if err := e.svc.SetManagerPermissions(e.ctx, token("usr_agowner", "admin"), e.ag.ID, "usr_cm", ManagerPermissions{
		CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true,
	}); err != nil {
		t.Fatalf("SetManagerPermissions: %v", err)
	}
	requireAuthzOutcome(t, "operate after demotion", e.svc.authorizeVendorAccountOperate(e.ctx, cm, acc), authzNotFound)
	requireAuthzOutcome(t, "credential after demotion", e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), authzNotFound)

	// Re-grant, then unassign the group from the account instead.
	if err := e.svc.SetManagerPermissions(e.ctx, token("usr_agowner", "admin"), e.ag.ID, "usr_cm", ManagerPermissions{CanManageVendorAccounts: true}); err != nil {
		t.Fatalf("SetManagerPermissions(regrant): %v", err)
	}
	requireAuthzOutcome(t, "credential after regrant", e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), authzAllow)
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("RemoveVendorAccountAdminGroup: %v", err)
	}
	requireAuthzOutcome(t, "operate after unassign", e.svc.authorizeVendorAccountOperate(e.ctx, cm, acc), authzNotFound)
	requireAuthzOutcome(t, "credential after unassign", e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), authzNotFound)
}

// TestVendorAccountOperateEndpointsUseTheOperateGate drives the three operate
// operations that exist today (connection test, model refresh, usage refresh)
// through the real service methods: a CM at shared/full is let in, every denied
// caller gets the 404 and NOTHING is written (no-write-on-deny), and system scope
// stays out because these send the owner's sealed credential upstream.
func TestVendorAccountOperateEndpointsUseTheOperateGate(t *testing.T) {
	e := newVendorAuthzEnv(t)

	ops := []struct {
		name string
		call func(principal auth.Token, id string) error
	}{
		{"TestVendorAccountConnection", func(p auth.Token, id string) error {
			_, err := e.svc.TestVendorAccountConnection(e.ctx, p, id)
			return err
		}},
		{"RefreshVendorAccountModels", func(p auth.Token, id string) error {
			_, _, err := e.svc.RefreshVendorAccountModels(e.ctx, p, id)
			return err
		}},
		{"RefreshVendorAccountUsage", func(p auth.Token, id string) error {
			_, _, err := e.svc.RefreshVendorAccountUsage(e.ctx, p, id, time.Minute)
			return err
		}},
	}
	allowed := map[string]map[string]bool{
		routing.VendorShareLevelNone:   {"owner": true},
		routing.VendorShareLevelShared: {"owner": true, "cm": true, "admin-group owner": true},
		routing.VendorShareLevelFull:   {"owner": true, "cm": true, "admin-group owner": true},
	}
	for _, op := range ops {
		for level, acc := range e.accounts {
			for _, p := range vendorAuthzPrincipals() {
				label := op.name + "/" + level + "/" + p.name
				before := e.snapshot(acc.ID)
				err := op.call(p.tok, acc.ID)
				if allowed[level][p.name] {
					if err != nil {
						t.Errorf("%s: err = %v, want allow", label, err)
					}
					continue
				}
				requireAuthzOutcome(t, label, err, authzNotFound)
				e.requireUnchanged(label, before)
			}
		}
	}
}

// TestVendorAccountSystemScopeReadsButNeverWrites pins the read/write asymmetry:
// a system admin may inspect any account's key-free metadata at every level (the
// existing read rule), yet is a stranger to both write gates and to every
// credential-using operation (the matrix covers the gates; this covers the
// service methods behind them).
func TestVendorAccountSystemScopeReadsButNeverWrites(t *testing.T) {
	e := newVendorAuthzEnv(t)
	sys := token("usr_s", "system", "admin")
	stranger := token("usr_stranger")

	for level, acc := range e.accounts {
		got, err := e.svc.GetVendorAccount(e.ctx, sys, acc.ID)
		if err != nil || got.ID != acc.ID {
			t.Errorf("system read at %s = %+v, %v, want the account", level, got, err)
		}
		if _, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), acc.ID); err != nil {
			t.Errorf("owner read at %s: %v", level, err)
		}
		if _, err := e.svc.GetVendorAccount(e.ctx, stranger, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("stranger read at %s: err = %v, want ErrVendorAccountNotFound", level, err)
		}

		before := e.snapshot(acc.ID)
		if _, _, err := e.svc.RefreshVendorAccountModels(e.ctx, sys, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("system RefreshVendorAccountModels at %s: err = %v, want ErrVendorAccountNotFound", level, err)
		}
		if _, err := e.svc.TestVendorAccountConnection(e.ctx, sys, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("system TestVendorAccountConnection at %s: err = %v, want ErrVendorAccountNotFound", level, err)
		}
		e.requireUnchanged("system-scope write attempts at "+level, before)
	}
}

// TestCreateVendorAccountStartsPrivate: the share level is a column with a
// DEFAULT, not something the create request chooses. A freshly created account
// reads back share_level 'none' and no containment root -- never "" (which no gate
// recognises as shared, but which would still be a corrupt value).
func TestCreateVendorAccountStartsPrivate(t *testing.T) {
	e := newVendorAuthzEnv(t)
	dto, err := e.svc.CreateVendorAccount(e.ctx, token(vendorAuthzOwnerID), CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "fresh",
	})
	if err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	acc := e.reload(dto.ID)
	if acc.ShareLevel != routing.VendorShareLevelNone || acc.SystemGroupID != "" {
		t.Fatalf("fresh account share_level/system_group_id = %q/%q, want %q/\"\"", acc.ShareLevel, acc.SystemGroupID, routing.VendorShareLevelNone)
	}
}
