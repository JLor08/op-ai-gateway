// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
)

// The master sharing flag PAUSES sharing (spec 2026-10-15 section 13): with
// vendor_account_sharing_enabled off "share_level is ignored, every account behaves
// personal". The write gates and the delete therefore read the EFFECTIVE share level
// (effectiveVendorAccountShareLevel), not the stored one.

// TestSharingMasterFlagOffPausesTheCMReach: after enable -> raise -> disable a CM is a
// stranger again (404, not 403: at `none` they cannot see the account), the owner keeps
// full reach, and NOTHING stored is cleared: switching the flag back on restores the
// CM's reach. It is a global pause, not an un-share.
func TestSharingMasterFlagOffPausesTheCMReach(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	e.svc.settingsVolatile = true // a permitted credential PATCH really writes the key
	cm := token("usr_cm", "admin")
	owner := e.admin

	acc := e.apiKeyAccount("paused")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, owner, acc.ID, routing.VendorShareLevelFull, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise to full: %v", err)
	}
	shared := e.apiKeyAccount("paused-shared")
	if _, err := e.svc.RaiseVendorAccountShareLevel(e.ctx, owner, shared.ID, routing.VendorShareLevelShared, []string{e.ag.ID}); err != nil {
		t.Fatalf("raise to shared: %v", err)
	}

	// reach reports what a caller can do right now: operate (connection test),
	// credential (api key replacement) and the delete gate.
	reach := func(who auth.Token, id string) (operate, credential, deleteGate error) {
		_, operate = e.svc.TestVendorAccountConnection(e.ctx, who, id)
		credential = e.patch(who, id, UpdateVendorAccountRequest{APIKey: strPtr(patchNewKey)})
		deleteGate = e.svc.authorizeVendorAccountCredential(e.ctx, who, e.reload(id))
		return operate, credential, deleteGate
	}

	// ON: the CM of a full account operates, replaces the credential and may delete.
	op, cred, del := reach(cm, acc.ID)
	requireAuthzOutcome(t, "CM operate, sharing on", op, authzAllow)
	requireAuthzOutcome(t, "CM credential, sharing on", cred, authzAllow)
	requireAuthzOutcome(t, "CM delete gate, sharing on", del, authzAllow)

	// OFF: the SAME CM is a stranger on every path; the owner is unaffected.
	setVendorSharingFlags(t, e.svc, false, false)
	for _, id := range []string{acc.ID, shared.ID} {
		before := e.snapshot(id)
		op, cred, del := reach(cm, id)
		requireAuthzOutcome(t, "CM operate, sharing off, "+id, op, authzNotFound)
		requireAuthzOutcome(t, "CM credential, sharing off, "+id, cred, authzNotFound)
		requireAuthzOutcome(t, "CM delete gate, sharing off, "+id, del, authzNotFound)
		if _, err := e.svc.DeleteVendorAccount(e.ctx, cm, id); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("CM DeleteVendorAccount, sharing off, %s: err = %v, want ErrVendorAccountNotFound", id, err)
		}
		if err := e.patch(cm, id, UpdateVendorAccountRequest{Name: strPtr("hijacked")}); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("CM rename, sharing off, %s: err = %v, want ErrVendorAccountNotFound", id, err)
		}
		e.requireUnchanged("the CM's refused calls while sharing is off", before)
	}
	op, cred, del = reach(owner, acc.ID)
	requireAuthzOutcome(t, "owner operate, sharing off", op, authzAllow)
	requireAuthzOutcome(t, "owner credential, sharing off", cred, authzAllow)
	requireAuthzOutcome(t, "owner delete gate, sharing off", del, authzAllow)

	// Paused, not cleared: the level, the root and the groups are all still stored.
	e.requireShareState("paused full", acc.ID, routing.VendorShareLevelFull, e.rootID, e.ag.ID)
	e.requireShareState("paused shared", shared.ID, routing.VendorShareLevelShared, e.rootID, e.ag.ID)

	// ON again: the CM's reach is back, and it really can delete a full account.
	setVendorSharingFlags(t, e.svc, true, false)
	op, cred, del = reach(cm, acc.ID)
	requireAuthzOutcome(t, "CM operate, sharing back on", op, authzAllow)
	requireAuthzOutcome(t, "CM credential, sharing back on", cred, authzAllow)
	requireAuthzOutcome(t, "CM delete gate, sharing back on", del, authzAllow)
	// ...and at shared the credential gate answers 403 again (the CM can see it).
	requireAuthzOutcome(t, "CM credential at shared, sharing back on", e.svc.authorizeVendorAccountCredential(e.ctx, cm, e.reload(shared.ID)), authzForbidden)
	if ok, err := e.svc.DeleteVendorAccount(e.ctx, cm, acc.ID); err != nil || !ok {
		t.Fatalf("CM delete of a full account, sharing back on = %v, %v, want deleted", ok, err)
	}
}

// effectiveVendorAccountShareLevel is the stored level while sharing is on and `none`
// whenever it is off, whatever is stored; a Service with no settings store fails closed
// to `none`. The stored account is never modified.
func TestEffectiveVendorAccountShareLevel(t *testing.T) {
	e := newVendorLifecycleEnv(t)
	for _, stored := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull, "bogus"} {
		acc := routing.VendorAccount{ID: "va_x", ShareLevel: stored}

		setVendorSharingFlags(t, e.svc, true, false)
		if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != stored {
			t.Errorf("sharing on, stored %q: effective = %q, want the stored level", stored, got)
		}
		setVendorSharingFlags(t, e.svc, false, true)
		if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, acc); got != routing.VendorShareLevelNone {
			t.Errorf("sharing off, stored %q: effective = %q, want none", stored, got)
		}
		if acc.ShareLevel != stored {
			t.Errorf("effectiveVendorAccountShareLevel modified the account: %q, want %q", acc.ShareLevel, stored)
		}
	}
	e.svc.settings = nil
	if got := e.svc.effectiveVendorAccountShareLevel(e.ctx, routing.VendorAccount{ShareLevel: routing.VendorShareLevelFull}); got != routing.VendorShareLevelNone {
		t.Errorf("no settings store: effective = %q, want none (fail closed)", got)
	}
}
