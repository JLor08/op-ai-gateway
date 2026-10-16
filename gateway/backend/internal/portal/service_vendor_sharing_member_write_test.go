// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The WRITE denial of a MEMBER of a shared vendor account (#234, on top of the member
// read of #183 Phase 2): a principal that reaches an account only through a resource
// group it is provisioned into can READ it, so an operate PATCH, an api-key PATCH or a
// DELETE it attempts is answered ErrVendorAccountForbidden (403), exactly like a CM at
// `shared` is -- "not found" would be a lie to someone the account is already disclosed
// to (external-vendor-accounts.md section 8.1, "Two denials, on purpose").
//
// The invariant these tests pin from the other side is the security one: the 403 is
// returned ONLY to a principal who can already read the account
// (isMemberOfSharedVendorAccount, the very predicate the read gate discloses by).
// Everyone else keeps the identical 404 an unknown id gets, so the new answer opens no
// existence oracle.

// vendorWriteCall is one write a principal can attempt on an account, driven through the
// real service method behind the gate.
type vendorWriteCall struct {
	name string
	call func(principal auth.Token, id string) error
}

// vendorWriteCalls are the member-facing writes the issue names (the operate PATCH
// fields, the api-key PATCH and the DELETE) plus the three operate calls that open the
// credential (connection test, model refresh, usage refresh): every one of them sits
// behind one of the two write gates.
func (e *vendorAuthzEnv) vendorWriteCalls() []vendorWriteCall {
	patch := func(req UpdateVendorAccountRequest) func(auth.Token, string) error {
		return func(p auth.Token, id string) error {
			_, err := e.svc.UpdateVendorAccount(e.ctx, p, id, req)
			return err
		}
	}
	return []vendorWriteCall{
		{"operate PATCH status", patch(UpdateVendorAccountRequest{Status: strPtr(routing.VendorAccountStatusDisabled)})},
		{"operate PATCH name", patch(UpdateVendorAccountRequest{Name: strPtr("Hijacked")})},
		{"operate PATCH model_prefix", patch(UpdateVendorAccountRequest{ModelPrefix: strPtr("hijack/")})},
		{"api-key PATCH", patch(UpdateVendorAccountRequest{APIKey: strPtr("sk-member-attempt")})},
		{"api-key clear", patch(UpdateVendorAccountRequest{APIKey: strPtr("")})},
		{"DELETE", func(p auth.Token, id string) error {
			_, err := e.svc.DeleteVendorAccount(e.ctx, p, id)
			return err
		}},
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
}

// requireWritesAnswer drives both gates and every write call as principal against acc
// and asserts each answers want, with nothing written by any of them.
func (e *vendorMemberEnv) requireWritesAnswer(label string, principal auth.Token, acc routing.VendorAccount, want authzOutcome) {
	e.t.Helper()
	before := e.snapshot(acc.ID)
	requireAuthzOutcome(e.t, label+"/operate gate", e.svc.authorizeVendorAccountOperate(e.ctx, principal, acc), want)
	requireAuthzOutcome(e.t, label+"/credential gate", e.svc.authorizeVendorAccountCredential(e.ctx, principal, acc), want)
	for _, w := range e.vendorWriteCalls() {
		requireAuthzOutcome(e.t, label+"/"+w.name, w.call(principal, acc.ID), want)
	}
	e.requireUnchanged(label+" (a denied write)", before)
}

// TestVendorAccountMemberWriteIsForbiddenNotNotFound: a pure member (provisioned into a
// resource group linked to a live shared/full account; not the owner, not a CM) is told
// 403 by both write gates and by every call behind them, at both levels where it can
// reach the account, and the denied call writes nothing. The member can read the account
// in the same state, which is the whole reason the answer is 403.
func TestVendorAccountMemberWriteIsForbiddenNotNotFound(t *testing.T) {
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		t.Run(level, func(t *testing.T) {
			e := newVendorMemberEnv(t)
			acc := e.accounts[level]
			e.link(acc.ID)

			// The premise: the member discloses the account by reading it.
			if _, err := e.svc.GetVendorAccount(e.ctx, e.member, acc.ID); err != nil {
				t.Fatalf("the member cannot read the account (%v), so the 403 below would not be a member's", err)
			}
			e.requireWritesAnswer("member/"+level, e.member, acc, authzForbidden)

			// The owner is unaffected by the member's presence (the control).
			requireAuthzOutcome(t, "owner/operate", e.svc.authorizeVendorAccountOperate(e.ctx, token(vendorAuthzOwnerID), acc), authzAllow)
			requireAuthzOutcome(t, "owner/credential", e.svc.authorizeVendorAccountCredential(e.ctx, token(vendorAuthzOwnerID), acc), authzAllow)
		})
	}
}

// TestVendorAccountWriteStaysNotFoundForEveryoneWhoCannotRead is the no-existence-leak
// guarantee: every principal/state below is one the read gate turns away with the
// ErrVendorAccountNotFound an unknown id gets (or, for system scope, one that is a
// stranger to both WRITE gates by design), so both gates and every write behind them
// must keep answering 404, never 403, and write nothing.
func TestVendorAccountWriteStaysNotFoundForEveryoneWhoCannotRead(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token)
	}{
		{"a stranger provisioned nowhere, at shared", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			return acc, token("usr_stranger")
		}},
		{"a stranger provisioned nowhere, at full", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelFull]
			e.link(acc.ID)
			return acc, token("usr_stranger")
		}},
		{"a principal with no user identity", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			return acc, auth.Token{Scopes: []string{"gateway:use"}}
		}},
		{"a service provisioned into the group (no user identity)", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindService, "svc_va_member")
			return acc, auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_va_member", Active: true, Scopes: []string{"gateway:use"}}
		}},
		{"system scope that is no member", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			return acc, token("usr_s", "system", "admin")
		}},
		{"a member of a none account whose link survived", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelNone]
			e.link(acc.ID)
			return acc, e.member
		}},
		{"a member of an account at an empty level", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, ""); err != nil {
				t.Fatalf("set level: %v", err)
			}
			return e.reload(acc.ID), e.member
		}},
		{"a member of an account at an unrecognised level", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, "bogus"); err != nil {
				t.Fatalf("set level: %v", err)
			}
			return e.reload(acc.ID), e.member
		}},
		{"a member while the master sharing flag is off", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			setVendorSharingFlags(t, e.svc, false, false)
			return acc, e.member
		}},
		{"a member of a subscription account without subscription sharing", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			setVendorSharingFlags(t, e.svc, true, false)
			_, subID := e.plantedVendorAccounts(routing.VendorShareLevelShared)
			e.link(subID)
			return e.reload(subID), e.member
		}},
		{"a principal whose resource group does not contain the account", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			other := e.mustCreateResourceGroup(routing.ResourceGroup{
				ID: "rgrp_va_other", Name: "Other", Status: routing.ServerStatusActive,
				SystemGroupID: e.ag.ParentGroupID, CreatedAt: e.now, UpdatedAt: e.now,
			})
			e.mustProvisionResourceGroup(other.ID, routing.ProvisionKindUser, "usr_stranger")
			e.link(acc.ID) // linked into RG, which the stranger is not provisioned into
			return acc, token("usr_stranger")
		}},
		{"a member of a shared account that is in no resource group", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			return e.accounts[routing.VendorShareLevelShared], e.member
		}},
		{"a member who was un-provisioned", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			if err := e.routes.RemoveResourceGroupProvision(e.ctx, e.rg.ID, routing.ProvisionKindUser, vendorMemberID); err != nil {
				t.Fatalf("unprovision: %v", err)
			}
			return acc, e.member
		}},
		{"a co-manager without the vendor flag who is not provisioned", func(e *vendorMemberEnv) (routing.VendorAccount, auth.Token) {
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			return acc, token("usr_cmx", "admin")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVendorMemberEnv(t)
			acc, principal := tc.setup(e)
			e.requireWritesAnswer(tc.name, principal, acc, authzNotFound)
		})
	}
}

// TestVendorAccountWriteForbiddenImpliesCanRead pins the invariant as a property over
// every principal and level of the matrix fixture, with the member's resource group
// linked: whenever either write gate answers 403, the same principal's READ of the same
// account succeeds. A 403 that outran the read gate would be an existence oracle.
func TestVendorAccountWriteForbiddenImpliesCanRead(t *testing.T) {
	e := newVendorMemberEnv(t)
	for _, acc := range e.accounts {
		e.link(acc.ID)
	}
	e.createUser("usr_va_unprovisioned", "user")
	principals := append(vendorAuthzPrincipals(), struct {
		name string
		tok  auth.Token
	}{"member", e.member}, struct {
		name string
		tok  auth.Token
	}{"unprovisioned user", token("usr_va_unprovisioned")})

	for _, flags := range []struct{ sharing, subscriptions bool }{{true, true}, {true, false}, {false, false}} {
		setVendorSharingFlags(t, e.svc, flags.sharing, flags.subscriptions)
		for level, acc := range e.accounts {
			for _, p := range principals {
				for gate, err := range map[string]error{
					"operate":    e.svc.authorizeVendorAccountOperate(e.ctx, p.tok, acc),
					"credential": e.svc.authorizeVendorAccountCredential(e.ctx, p.tok, acc),
				} {
					if !errors.Is(err, ErrVendorAccountForbidden) {
						continue
					}
					if _, readErr := e.svc.GetVendorAccount(e.ctx, p.tok, acc.ID); readErr != nil {
						t.Errorf("%s gate told %s (%+v) at %s 403 but its read of the account fails with %v: a 403 must never reach someone who cannot read", gate, p.name, flags, level, readErr)
					}
				}
			}
		}
	}
}

// TestVendorAccountCMKeepsItsAnswersEvenWhenAlsoAMember: a CM is answered by the manager
// branch, which comes before the member check, so provisioning a CM into the account's
// resource group changes nothing for it: operate is allowed at shared and full, the
// credential gate is 403 at shared and allows at full.
func TestVendorAccountCMKeepsItsAnswersEvenWhenAlsoAMember(t *testing.T) {
	e := newVendorMemberEnv(t)
	e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindUser, "usr_cm")
	cm := token("usr_cm", "admin")

	want := map[string][2]authzOutcome{ // level -> operate, credential
		routing.VendorShareLevelShared: {authzAllow, authzForbidden},
		routing.VendorShareLevelFull:   {authzAllow, authzAllow},
	}
	for level, outcomes := range want {
		acc := e.accounts[level]
		e.link(acc.ID)
		if ok, err := e.svc.isMemberOfSharedVendorAccount(e.ctx, cm, acc); err != nil || !ok {
			t.Fatalf("the CM is not also a member at %s (%v, %v): the test would not exercise the branch order", level, ok, err)
		}
		before := e.snapshot(acc.ID)
		requireAuthzOutcome(t, "cm+member/operate/"+level, e.svc.authorizeVendorAccountOperate(e.ctx, cm, acc), outcomes[0])
		requireAuthzOutcome(t, "cm+member/credential/"+level, e.svc.authorizeVendorAccountCredential(e.ctx, cm, acc), outcomes[1])
		e.requireUnchanged("the gates for a CM who is also a member at "+level, before)
	}
}

// TestVendorAccountSubscriptionMemberWriteIsForbiddenAtTheGate: a member of a shared
// SUBSCRIPTION account is turned away 403 by the operate and credential gates before the
// owner-only subscription rule (requireOwnerForSubscriptionOperate, which runs only
// AFTER a passing operate gate) is ever reached, and nothing of the owner's token set
// moves. With subscription sharing off the same member is a stranger again (404).
func TestVendorAccountSubscriptionMemberWriteIsForbiddenAtTheGate(t *testing.T) {
	e := newVendorMemberEnv(t)
	setVendorSharingFlags(t, e.svc, true, true)
	_, subID := e.plantedVendorAccounts(routing.VendorShareLevelShared)
	e.link(subID)
	sub := e.reload(subID)
	if sub.AuthType != routing.VendorAuthSubscription {
		t.Fatalf("fixture account auth type = %q, want subscription", sub.AuthType)
	}

	e.requireWritesAnswer("member/subscription", e.member, sub, authzForbidden)

	setVendorSharingFlags(t, e.svc, true, false)
	e.requireWritesAnswer("member/subscription, subscription sharing off", e.member, sub, authzNotFound)
}

// TestVendorAccountMemberWriteStoreErrorFailsTheCall: a store failure while deciding
// whether the principal is a member (the account's resource groups, the principal's
// provisioned groups) fails the gate with that error. It is never read as a grant (nil),
// and it is neither the 403 nor the 404 either: nothing about the account is confirmed or
// denied, and nothing is written.
func TestVendorAccountMemberWriteStoreErrorFailsTheCall(t *testing.T) {
	for name, fail := range map[string]failingGrantStore{
		"the account's resource groups":      {accountGroups: true},
		"the principal's provisioned groups": {provisionTargets: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newVendorMemberEnv(t)
			acc := e.accounts[routing.VendorShareLevelShared]
			e.link(acc.ID)
			before := e.snapshot(acc.ID)

			fail.Store = e.routes
			e.svc.routes = &fail

			for _, p := range []auth.Token{e.member, token("usr_stranger")} {
				for gate, err := range map[string]error{
					"operate":    e.svc.authorizeVendorAccountOperate(e.ctx, p, acc),
					"credential": e.svc.authorizeVendorAccountCredential(e.ctx, p, acc),
				} {
					// Both principals are linked-account callers, so both reach the lookup.
					if !errors.Is(err, errGrantStore) {
						t.Errorf("%s gate for %s: err = %v, want the store error", gate, p.UserID, err)
					}
				}
			}
			for _, w := range e.vendorWriteCalls() {
				if err := w.call(e.member, acc.ID); !errors.Is(err, errGrantStore) {
					t.Errorf("%s: err = %v, want the store error", w.name, err)
				}
			}
			e.svc.routes = e.routes
			e.requireUnchanged(name+" (a failed write)", before)
		})
	}
}

// TestVendorAccountSystemScopeThatIsAlsoAMemberIsForbiddenNotNotFound documents the one
// place system scope differs from a stranger on a write: a system-scope token whose OWN
// user is provisioned into the account's resource group is a member like any other, and
// is told 403. That reveals nothing, since system scope reads every account anyway; the
// scope itself still grants no write (the matrix and the no-member case above).
func TestVendorAccountSystemScopeThatIsAlsoAMemberIsForbiddenNotNotFound(t *testing.T) {
	e := newVendorMemberEnv(t)
	acc := e.accounts[routing.VendorShareLevelShared]
	e.link(acc.ID)
	e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindUser, "usr_s")
	sys := token("usr_s", "system", "admin")

	if _, err := e.svc.GetVendorAccount(e.ctx, sys, acc.ID); err != nil {
		t.Fatalf("system scope cannot read the account (%v): the premise of the 403 is gone", err)
	}
	e.requireWritesAnswer("system scope + member", sys, acc, authzForbidden)
}
