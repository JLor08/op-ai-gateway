// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The MEMBER read of a shared vendor account (#183, spec 2026-10-15 sections 6 and 10):
// a principal who reaches an account only through a resource group it is provisioned
// into (the shared arm of the routing grant) may READ it, and gets a MINIMAL key-free
// DTO -- what a borrower needs to use the account and nothing of how it is managed.
//
// The three viewer tiers, asserted here against each other so that the absence of a
// field in the member's view is never vacuous (the same account's owner and CM views
// carry it):
//
//   - the OWNER sees everything (is_owner true);
//   - a CM sees the management fields, never the secret;
//   - a MEMBER sees identity, what the account is, whether it works, the names it serves
//     and who shares it -- and NOT share_level, admin_groups, system_group_id/name,
//     base_url, usage, upstream model ids, timestamps or any credential.
//
// The member's reach is REACH (provisioned into a resource group that contains the
// account, the account live shared/full), not the per-token opt-in: the opt-in gates
// USING an account, reading is how a member finds one to opt into.

// vendorMemberDTOFields is the allowlist of VendorAccountDTO fields a MEMBER may receive
// non-zero: a deny-by-default subset of vendorDTOAllowedFields. A field that is not here
// is withheld from a member, so adding a field to the DTO leaves it invisible to members
// until someone decides, here, that a borrower needs it.
var vendorMemberDTOFields = map[string]string{
	"id":                     "identity",
	"vendor":                 "what the account is (the provider)",
	"auth_type":              "what the account is (api key or subscription)",
	"name":                   "what the member picks the account by",
	"status":                 "whether the account works right now (active, disabled, needs_reconnect)",
	"model_prefix":           "the namespace the member requests the account's models under",
	"api_key_set":            "boolean credential indicator",
	"subscription_connected": "boolean credential indicator",
	"models":                 "the gateway names the member can request (gateway_model, api_flavor, display_name; upstream_model is withheld)",
	"is_owner":               "viewer relation (always false for a member)",
	"shared_by":              "viewer relation (the owner's display name)",
}

// memberDTOExtraFields returns the wire fields of dto that hold a value although a member
// may not receive them (see vendorMemberDTOFields). An empty slice and a nil pointer are
// "not received".
func memberDTOExtraFields(dto VendorAccountDTO) []string {
	var extra []string
	v := reflect.ValueOf(dto)
	typ := v.Type()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		f := v.Field(i)
		if f.IsZero() || (f.Kind() == reflect.Slice && f.Len() == 0) {
			continue
		}
		if _, ok := vendorMemberDTOFields[name]; !ok {
			extra = append(extra, name)
		}
	}
	return extra
}

// zeroDTOFields returns the wire fields of dto that are empty (the complement used to
// prove a fully populated fixture really populates everything the tiers are told apart by).
func zeroDTOFields(dto VendorAccountDTO) []string {
	var zero []string
	v := reflect.ValueOf(dto)
	typ := v.Type()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		f := v.Field(i)
		if f.IsZero() || (f.Kind() == reflect.Slice && f.Len() == 0) {
			zero = append(zero, name)
		}
	}
	return zero
}

// TestVendorMemberDTOAllowlistIsASubsetOfTheCredentialFreeAllowlist: the member allowlist
// names only real DTO fields, only ones already classified credential-free, and nothing
// that looks like a credential except the two booleans.
func TestVendorMemberDTOAllowlistIsASubsetOfTheCredentialFreeAllowlist(t *testing.T) {
	have := map[string]bool{}
	typ := reflect.TypeFor[VendorAccountDTO]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		have[name] = true
	}
	for name := range vendorMemberDTOFields {
		if !have[name] {
			t.Errorf("the member allowlist names %q, which VendorAccountDTO does not have", name)
		}
		if _, ok := vendorDTOAllowedFields[name]; !ok {
			t.Errorf("the member allowlist names %q, which is not on the credential-free allowlist", name)
		}
		if name == "api_key_set" || name == "subscription_connected" {
			continue
		}
		for _, word := range []string{"secret", "token", "password", "credential", "oauth", "api_key", "refresh"} {
			if strings.Contains(strings.ToLower(name), word) {
				t.Errorf("member-visible field %q looks like credential material (%q)", name, word)
			}
		}
	}
	// The management fields are exactly the ones a member must not receive.
	for _, name := range []string{"share_level", "admin_groups", "system_group_id", "system_group_name", "base_url", "usage", "created_at", "updated_at"} {
		if _, ok := vendorMemberDTOFields[name]; ok {
			t.Errorf("%q is on the member allowlist, but a member must not receive it", name)
		}
	}
}

const vendorMemberID = "usr_va_member"

// vendorMemberEnv is the authorization fixture (an owner, a CM of AG, one account per
// share level) plus a resource group RG the member is provisioned into directly. Accounts
// reach RG only through link().
type vendorMemberEnv struct {
	*vendorAuthzEnv
	rg     routing.ResourceGroup
	member auth.Token
}

func newVendorMemberEnv(t *testing.T) *vendorMemberEnv {
	t.Helper()
	e := &vendorMemberEnv{vendorAuthzEnv: newVendorAuthzEnv(t)}
	e.createUser(vendorMemberID, "user")
	e.rg = e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_va_member", Name: "RG", Status: routing.ServerStatusActive,
		SystemGroupID: e.ag.ParentGroupID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindUser, vendorMemberID)
	e.member = token(vendorMemberID)
	return e
}

// link puts the accounts into RG (the link alone: the level is the account's own).
func (e *vendorMemberEnv) link(ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if err := e.routes.SetResourceGroupVendorAccount(e.ctx, e.rg.ID, id); err != nil {
			e.t.Fatalf("SetResourceGroupVendorAccount(%s, %s): %v", e.rg.ID, id, err)
		}
	}
}

// requireMemberView asserts dto is the member's view of the account: the borrower-facing
// fields filled from acc, is_owner false, shared_by the owner, the credential booleans only,
// and every management field empty (admin_groups as a non-nil [], so it serializes as []).
func requireMemberView(t *testing.T, label string, dto VendorAccountDTO, acc routing.VendorAccount) {
	t.Helper()
	if dto.ID != acc.ID || dto.Vendor != acc.Vendor || dto.AuthType != acc.AuthType || dto.Name != acc.Name ||
		dto.Status != acc.Status || dto.ModelPrefix != acc.ModelPrefix {
		t.Errorf("%s: id/vendor/auth_type/name/status/model_prefix = %q/%q/%q/%q/%q/%q, want the account's %q/%q/%q/%q/%q/%q", label,
			dto.ID, dto.Vendor, dto.AuthType, dto.Name, dto.Status, dto.ModelPrefix,
			acc.ID, acc.Vendor, acc.AuthType, acc.Name, acc.Status, acc.ModelPrefix)
	}
	if dto.IsOwner || dto.SharedBy != acc.OwnerUserID {
		t.Errorf("%s: is_owner/shared_by = %v/%q, want false/%q (the owner's display name)", label, dto.IsOwner, dto.SharedBy, acc.OwnerUserID)
	}
	if dto.APIKeySet != (acc.APIKey != "") || dto.SubscriptionConnected != (acc.OAuthTokens != "") {
		t.Errorf("%s: api_key_set/subscription_connected = %v/%v, want the booleans %v/%v", label,
			dto.APIKeySet, dto.SubscriptionConnected, acc.APIKey != "", acc.OAuthTokens != "")
	}
	if dto.ShareLevel != "" || dto.SystemGroupID != "" || dto.SystemGroupName != "" || dto.BaseURL != "" || dto.Usage != nil {
		t.Errorf("%s: a member sees management fields: share_level %q, system group %q/%q, base_url %q, usage %+v",
			label, dto.ShareLevel, dto.SystemGroupID, dto.SystemGroupName, dto.BaseURL, dto.Usage)
	}
	if dto.AdminGroups == nil || len(dto.AdminGroups) != 0 {
		t.Errorf("%s: admin_groups = %+v, want a non-nil empty slice (it must serialize as [])", label, dto.AdminGroups)
	}
	if !dto.CreatedAt.IsZero() || !dto.UpdatedAt.IsZero() {
		t.Errorf("%s: a member sees timestamps created_at/updated_at = %v/%v", label, dto.CreatedAt, dto.UpdatedAt)
	}
	for _, m := range dto.Models {
		if m.UpstreamModel != "" || m.GatewayModel == "" || m.APIFlavor == "" {
			t.Errorf("%s: model %+v: a member sees the gateway name and dialect only, never the vendor's own id", label, m)
		}
	}
	if extra := memberDTOExtraFields(dto); len(extra) > 0 {
		t.Errorf("%s: the member's DTO carries fields outside the member allowlist: %v", label, extra)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	if !strings.Contains(string(raw), `"admin_groups":[]`) {
		t.Errorf("%s: admin_groups did not serialize as []: %s", label, raw)
	}
	for _, key := range []string{`"usage"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("%s: the member's JSON carries %s: %s", label, key, raw)
		}
	}
}

// TestVendorAccountMemberReadIsMinimalAndKeyFree is the security-relevant rule of the task:
// a provisioned member of a shared or full account reads it, gets the minimal DTO with
// is_owner false and the owner's name as shared_by -- and NOTHING of the owner's secret and
// none of the management fields. The secrets are planted (an api key; an OAuth access and
// refresh token) and searched for on the wire; the CM's read of the very same account is
// the control that proves the management fields exist to be withheld.
func TestVendorAccountMemberReadIsMinimalAndKeyFree(t *testing.T) {
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		t.Run(level, func(t *testing.T) {
			e := newVendorMemberEnv(t)
			setVendorSharingFlags(t, e.svc, true, true) // the subscription account is reachable too
			keyID, subID := e.plantedVendorAccounts(level)
			e.link(keyID, subID)
			secrets := e.plantedSecrets(keyID, subID)
			cm := token("usr_cm", "admin")

			for _, id := range []string{keyID, subID} {
				label := "member/" + level + "/" + id
				before := e.snapshot(id)
				dto, err := e.svc.GetVendorAccount(e.ctx, e.member, id)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				requireMemberView(t, label, dto, e.reload(id))
				if leaked := leakedSecrets(dto, secrets); len(leaked) > 0 {
					t.Errorf("%s: the member's DTO leaks the owner's secret %v: %+v", label, leaked, dto)
				}
				e.requireUnchanged(label, before)

				// The control: the CM's read of the same account carries what the member's
				// lacks, and the owner's is the full view.
				cmDTO, err := e.svc.GetVendorAccount(e.ctx, cm, id)
				if err != nil {
					t.Fatalf("CM read of %s: %v", id, err)
				}
				requireManagementFields(t, "cm/"+level+"/"+id, cmDTO, e.vendorAuthzEnv, level)
				ownerDTO, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), id)
				if err != nil || !ownerDTO.IsOwner || ownerDTO.SharedBy != "" {
					t.Fatalf("owner read of %s = %+v, %v, want the full view with is_owner true", id, ownerDTO, err)
				}
				requireManagementFields(t, "owner/"+level+"/"+id, ownerDTO, e.vendorAuthzEnv, level)
			}
		})
	}
}

// TestVendorAccountMemberReadOfAFullyPopulatedAccount: an account with EVERY field the
// tiers are told apart by filled in (a preset base url, a model prefix, a usage snapshot
// with spend figures, a management group and root, a model catalog, a planted key) reads
// as follows. The owner's and the CM's views are fully populated (so nothing below can be
// vacuous); the member's holds only the allowlisted fields.
func TestVendorAccountMemberReadOfAFullyPopulatedAccount(t *testing.T) {
	e := newVendorMemberEnv(t)
	e.svc.settingsVolatile = true
	acc := e.newAccountOf(routing.VendorShareLevelFull, CreateVendorAccountRequest{
		Vendor: routing.VendorXAI, AuthType: routing.VendorAuthAPIKey, Name: "Everything", APIKey: hygieneAPIKey, ModelPrefix: "team/",
	})
	if len(e.snapshot(acc.ID).Models) == 0 {
		if err := e.routes.SetVendorAccountModels(e.ctx, acc.ID, []routing.VendorAccountModel{
			{AccountID: acc.ID, GatewayModel: "team/grok-x", UpstreamModel: "grok-x", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "Grok X"},
		}); err != nil {
			t.Fatalf("SetVendorAccountModels: %v", err)
		}
	}
	reset := e.now.Add(time.Hour)
	if err := e.routes.UpsertVendorAccountUsage(e.ctx, routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: 42, FiveHourResetAt: &reset, WeeklyPct: 7, WeeklyResetAt: &reset,
		CreditBalance: "12.5", SpendUnit: "USD", SpendLimit: "100", SpendUsed: "55", SpendRemaining: "45",
		SpendUsedPct: 55, SpendResetAt: &reset, CreditStatus: "has_credits", UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}
	e.link(acc.ID)
	secrets := e.plantedSecrets(acc.ID)

	owner, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), acc.ID)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if zero := zeroDTOFields(owner); !slices.Equal(zero, []string{"subscription_connected", "shared_by"}) {
		t.Fatalf("the owner's view of the fixture has empty fields %v, want only subscription_connected (an api key account) and shared_by: the fixture does not populate everything, so the member assertions would be vacuous", zero)
	}
	cm, err := e.svc.GetVendorAccount(e.ctx, token("usr_cm", "admin"), acc.ID)
	if err != nil {
		t.Fatalf("CM read: %v", err)
	}
	if zero := zeroDTOFields(cm); !slices.Equal(zero, []string{"subscription_connected", "is_owner"}) {
		t.Fatalf("the CM's view has empty fields %v, want only subscription_connected and is_owner (the management view is whole)", zero)
	}
	if leaked := leakedSecrets(cm, secrets); len(leaked) > 0 {
		t.Fatalf("the CM's view leaks %v", leaked)
	}

	member, err := e.svc.GetVendorAccount(e.ctx, e.member, acc.ID)
	if err != nil {
		t.Fatalf("member read: %v", err)
	}
	requireMemberView(t, "member/fully populated", member, e.reload(acc.ID))
	if member.ModelPrefix != "team/" || len(member.Models) == 0 || member.Status != routing.VendorAccountStatusActive {
		t.Errorf("a member must see what it needs to call the account: prefix %q, %d models, status %q", member.ModelPrefix, len(member.Models), member.Status)
	}
	if leaked := leakedSecrets(member, secrets); len(leaked) > 0 {
		t.Errorf("the member's view leaks %v", leaked)
	}
	wantModels := e.snapshot(acc.ID).Models
	if len(member.Models) != len(wantModels) {
		t.Fatalf("member sees %d models, the account serves %d", len(member.Models), len(wantModels))
	}
	for i, m := range wantModels {
		if got := member.Models[i]; got.GatewayModel != m.GatewayModel || got.APIFlavor != m.APIFlavor || got.DisplayName != m.DisplayName {
			t.Errorf("model %d = %+v, want the gateway name, dialect and display name of %+v", i, got, m)
		}
	}
}

// TestVendorAccountMemberReadGate is the member column of the read gate, at every level:
// RG-reached members read a shared/full account; the same principal reading a `none`
// account (a link that survived a torn downgrade), an account in no resource group, or
// with no provisioning is a stranger and gets the 404 an unknown id gets (never a 403).
func TestVendorAccountMemberReadGate(t *testing.T) {
	e := newVendorMemberEnv(t)
	none, shared, full := e.accounts[routing.VendorShareLevelNone], e.accounts[routing.VendorShareLevelShared], e.accounts[routing.VendorShareLevelFull]
	e.link(none.ID, shared.ID, full.ID)
	unlinked := e.newAccountAt(routing.VendorShareLevelShared) // shared, in no resource group

	e.createUser("usr_va_unprovisioned", "user")
	provisionedService := auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_va_member", Active: true, Scopes: []string{"gateway:use"}}
	e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindService, "svc_va_member")

	cases := []struct {
		name    string
		tok     auth.Token
		account routing.VendorAccount
		allowed bool
	}{
		{"member at shared", e.member, shared, true},
		{"member at full", e.member, full, true},
		{"member of a none account whose RG link survived", e.member, none, false},
		{"member of a shared account in no resource group", e.member, unlinked, false},
		{"principal provisioned nowhere, at shared", token("usr_va_unprovisioned"), shared, false},
		{"principal provisioned nowhere, at full", token("usr_va_unprovisioned"), full, false},
		{"stranger of the fixture, at shared", token("usr_stranger"), shared, false},
		{"no user identity", auth.Token{Scopes: []string{"gateway:use"}}, shared, false},
		{"a service provisioned into the group (the page is personal)", provisionedService, shared, false},
	}
	for _, tc := range cases {
		before := e.snapshot(tc.account.ID)
		dto, err := e.svc.GetVendorAccount(e.ctx, tc.tok, tc.account.ID)
		if tc.allowed {
			if err != nil {
				t.Errorf("%s: %v, want the member view", tc.name, err)
				continue
			}
			requireMemberView(t, tc.name, dto, e.reload(tc.account.ID))
		} else {
			requireAuthzOutcome(t, tc.name, err, authzNotFound)
			if dto.ID != "" {
				t.Errorf("%s: a denied read still returned %+v", tc.name, dto)
			}
		}
		e.requireUnchanged(tc.name, before)
	}
	if _, err := e.svc.GetVendorAccount(e.ctx, e.member, "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("member read of an unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
}

// TestVendorAccountMemberReadIsReachNotOptIn: reading is how a member finds an account to
// opt into, so the read needs REACH only. A member whose token names no vendor account at
// all (the zero VendorAccess), or only some other one, reads exactly the same.
func TestVendorAccountMemberReadIsReachNotOptIn(t *testing.T) {
	e := newVendorMemberEnv(t)
	shared := e.accounts[routing.VendorShareLevelShared]
	e.link(shared.ID)

	for name, access := range map[string]auth.VendorAccess{
		"no vendor access at all":    {},
		"all accounts, but no entry": {All: true},
		"an entry naming another":    {Accounts: []auth.VendorAccessEntry{{AccountID: "va_somebody_elses"}}},
	} {
		tok := e.member
		tok.VendorAccess = access
		dto, err := e.svc.GetVendorAccount(e.ctx, tok, shared.ID)
		if err != nil || dto.ID != shared.ID {
			t.Errorf("%s: read = %+v, %v, want the member view (the opt-in gates use, not reading)", name, dto, err)
		}
	}
}

// TestVendorAccountMemberReadFollowsTheLiveGrant: the read is exactly as live as the grant
// it is built on. Pausing sharing (master flag off: effective level none), lowering the
// account, unlinking it from the resource group or un-provisioning the member each revoke
// the read on the very next call, and putting the state back restores it.
func TestVendorAccountMemberReadFollowsTheLiveGrant(t *testing.T) {
	e := newVendorMemberEnv(t)
	shared := e.accounts[routing.VendorShareLevelShared]
	e.link(shared.ID)
	read := func() error {
		_, err := e.svc.GetVendorAccount(e.ctx, e.member, shared.ID)
		return err
	}
	requireReadable := func(label string) {
		t.Helper()
		if err := read(); err != nil {
			t.Errorf("%s: %v, want the member view", label, err)
		}
	}
	requireHidden := func(label string) {
		t.Helper()
		requireAuthzOutcome(t, label, read(), authzNotFound)
		if got := e.svc.visibleVendorAccountName(e.ctx, e.member, shared.ID); got != "" {
			t.Errorf("%s: the member still sees the name %q", label, got)
		}
	}
	requireReadable("baseline")
	if got := e.svc.visibleVendorAccountName(e.ctx, e.member, shared.ID); got != shared.Name {
		t.Errorf("baseline: the member sees the name %q, want %q", got, shared.Name)
	}

	setVendorSharingFlags(t, e.svc, false, false)
	requireHidden("sharing paused (master flag off)")
	if dto, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), shared.ID); err != nil || !dto.IsOwner {
		t.Errorf("the owner's read while sharing is paused = %+v, %v, want the owner's view", dto, err)
	}
	setVendorSharingFlags(t, e.svc, true, false)
	requireReadable("sharing back on")

	for _, level := range []string{routing.VendorShareLevelNone, "", "bogus"} {
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, shared.ID, level); err != nil {
			t.Fatalf("set level %q: %v", level, err)
		}
		requireHidden("level " + level)
	}
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, shared.ID, routing.VendorShareLevelFull); err != nil {
		t.Fatalf("set level full: %v", err)
	}
	requireReadable("raised to full")

	if err := e.routes.RemoveResourceGroupVendorAccount(e.ctx, e.rg.ID, shared.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	requireHidden("unlinked from the resource group")
	e.link(shared.ID)
	requireReadable("linked again")

	if err := e.routes.RemoveResourceGroupProvision(e.ctx, e.rg.ID, routing.ProvisionKindUser, vendorMemberID); err != nil {
		t.Fatalf("unprovision: %v", err)
	}
	requireHidden("member un-provisioned")
	e.mustProvisionResourceGroup(e.rg.ID, routing.ProvisionKindUser, vendorMemberID)
	requireReadable("provisioned again")

	// The vendor-accounts master flag keeps its place in front of everything.
	setVendorAccountsEnabled(t, e.svc, false)
	if _, err := e.svc.GetVendorAccount(e.ctx, e.member, shared.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Errorf("member read with the module off: err = %v, want ErrVendorAccountsDisabled", err)
	}
}

// TestVendorAccountMemberReadOfASubscriptionNeedsSubscriptionSharing: the member's read is
// the shared arm of the grant, which gates a subscription account on
// vendor_subscription_sharing_enabled (link creation does not look at it). Reading what the
// member can neither route to nor see in its list would be the one inconsistency, so the
// read says 404 while the flag is off and the member view once it is on.
func TestVendorAccountMemberReadOfASubscriptionNeedsSubscriptionSharing(t *testing.T) {
	e := newVendorMemberEnv(t)
	_, subID := e.plantedVendorAccounts(routing.VendorShareLevelShared)
	e.link(subID)

	setVendorSharingFlags(t, e.svc, true, false)
	requireAuthzOutcome(t, "subscription sharing off", func() error {
		_, err := e.svc.GetVendorAccount(e.ctx, e.member, subID)
		return err
	}(), authzNotFound)

	setVendorSharingFlags(t, e.svc, true, true)
	dto, err := e.svc.GetVendorAccount(e.ctx, e.member, subID)
	if err != nil {
		t.Fatalf("subscription sharing on: %v", err)
	}
	requireMemberView(t, "member/subscription", dto, e.reload(subID))
	if !dto.SubscriptionConnected || dto.APIKeySet {
		t.Errorf("subscription_connected/api_key_set = %v/%v, want true/false", dto.SubscriptionConnected, dto.APIKeySet)
	}
}

// TestVendorAccountMemberReadStoreErrorFailsTheRead: a store failure while deciding the
// member's reach is an error, never a grant and never a DTO. (The Activity list's name
// lookup logs it and shows no name.)
func TestVendorAccountMemberReadStoreErrorFailsTheRead(t *testing.T) {
	e := newVendorListingEnv(t)
	for name, fail := range map[string]failingGrantStore{
		"the principal's provisioned groups": {Store: e.routes, provisionTargets: true},
		"the account's resource groups":      {Store: e.routes, accountGroups: true},
	} {
		svc := NewService(ServiceDeps{Users: e.dir, Groups: e.dir, Routes: &fail, SystemSettings: e.svc.settings, Clock: fixedClock()})
		dto, err := svc.GetVendorAccount(e.ctx, token(grantAdminMember), grantShared)
		if err == nil || errors.Is(err, ErrVendorAccountNotFound) || dto.ID != "" {
			t.Errorf("%s fails: read = %+v, %v, want the store error and no DTO", name, dto, err)
		}
		if got := svc.visibleVendorAccountName(e.ctx, token(grantAdminMember), grantShared); got != "" {
			t.Errorf("%s fails: the name lookup shows %q, want none", name, got)
		}
	}
}

// TestVendorAccountReadTiersAreOrderedOwnerCMMember: a principal who is more than one of
// them gets the widest view they qualify for. A CM who is also provisioned into the
// account's resource group reads the MANAGEMENT view of it, and the same principal reading
// an account it can only route to gets the member view.
func TestVendorAccountReadTiersAreOrderedOwnerCMMember(t *testing.T) {
	e := newVendorListUnion(t)

	managedAndMember, err := e.svc.GetVendorAccount(e.ctx, e.cm, e.memberAndCM)
	if err != nil {
		t.Fatalf("CM+member read: %v", err)
	}
	if managedAndMember.ShareLevel != routing.VendorShareLevelFull || len(managedAndMember.AdminGroups) != 1 || managedAndMember.SystemGroupID == "" {
		t.Errorf("a CM who is also a member got %+v, want the management view", managedAndMember)
	}
	memberOnly, err := e.svc.GetVendorAccount(e.ctx, e.cm, e.memberOnly)
	if err != nil {
		t.Fatalf("member-only read: %v", err)
	}
	requireMemberView(t, "the CM reading an account it only routes to", memberOnly, e.accountRow(e.memberOnly))
	// And the managed-only account (the CM manages it, is not provisioned into it).
	managedOnly, err := e.svc.GetVendorAccount(e.ctx, e.cm, e.managedOnly)
	if err != nil || managedOnly.ShareLevel != routing.VendorShareLevelShared || len(managedOnly.AdminGroups) != 1 {
		t.Errorf("managed-only read = %+v, %v, want the management view", managedOnly, err)
	}
}

// accountRow reads an account straight from the store.
func (e *vendorGrantEnv) accountRow(id string) routing.VendorAccount {
	e.t.Helper()
	acc, err := e.routes.VendorAccountByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("VendorAccountByID(%s): %v", id, err)
	}
	return acc
}

// TestListVendorAccountsMemberOnlyRowsAreMinimal: the member-only rows of the management
// list are the SAME minimal view the single read gives. The fixture gives every row the
// management fields (an admin group and root on the shared accounts, a usage snapshot), a
// pure member (no CM reach) lists them without any, and the CM's list of the same account
// is the control that they exist.
func TestListVendorAccountsMemberOnlyRowsAreMinimal(t *testing.T) {
	e := newVendorListUnion(t)
	e.flags(true, true)
	for _, id := range []string{grantShared, grantDup, grantSub} {
		e.manage(id)
	}
	reset := e.now.Add(time.Hour)
	if err := e.routes.UpsertVendorAccountUsage(e.ctx, routing.VendorAccountUsage{
		AccountID: grantShared, FiveHourPct: 42, FiveHourResetAt: &reset, WeeklyPct: 7,
		SpendUnit: "USD", SpendLimit: "100", SpendUsed: "55", SpendUsedPct: 55, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	// grantUserGroupMember reaches the accounts through the user_group provision only: it
	// owns nothing and manages nothing.
	pure := token(grantUserGroupMember)
	list, err := e.svc.ListVendorAccounts(e.ctx, pure)
	if err != nil {
		t.Fatalf("pure member list: %v", err)
	}
	want := []string{grantShared, grantFull, grantSub, grantDup}
	if got := listedIDs(list); !slices.Equal(got, want) {
		t.Fatalf("pure member list = %v, want %v", got, want)
	}
	var secrets []string
	for _, dto := range list.Data {
		requireMemberView(t, "list/"+dto.ID, dto, e.accountRow(dto.ID))
		acc := e.accountRow(dto.ID)
		secrets = append(secrets, acc.APIKey, acc.OAuthTokens)
	}
	secrets = slices.DeleteFunc(secrets, func(s string) bool { return s == "" })
	if len(secrets) != len(want) {
		t.Fatalf("planted %d secrets for %d accounts: the key-free assertions would be vacuous", len(secrets), len(want))
	}
	if leaked := leakedSecrets(list, secrets); len(leaked) > 0 {
		t.Errorf("a member's list leaks %v", leaked)
	}
	// The single read and the list row of one account are the same view.
	single, err := e.svc.GetVendorAccount(e.ctx, pure, grantShared)
	if err != nil {
		t.Fatalf("pure member read: %v", err)
	}
	if !reflect.DeepEqual(single, list.Data[0]) {
		t.Errorf("the member's read and list row of %s differ:\n read %+v\n list %+v", grantShared, single, list.Data[0])
	}

	// The control: the CM of AG lists the very same account with its management fields
	// and its usage snapshot, so their absence above is the member tier's doing.
	cmList, err := e.svc.ListVendorAccounts(e.ctx, e.cm)
	if err != nil {
		t.Fatalf("CM list: %v", err)
	}
	for _, dto := range cmList.Data {
		if dto.ID != grantShared {
			continue
		}
		if dto.ShareLevel != routing.VendorShareLevelShared || len(dto.AdminGroups) != 1 || dto.SystemGroupID == "" || dto.Usage == nil {
			t.Errorf("the CM's row of %s = %+v, want the management view with usage", grantShared, dto)
		}
		return
	}
	t.Fatalf("the CM's list lacks %s: %v", grantShared, listedIDs(cmList))
}

// TestVendorAccountMemberReadMatchesTheListedMemberSet: the single read and the list
// answer "may this principal see that account as a member" the same way, for every
// fixture account and every kind of principal, through every flag state. A gate that
// drifted from the list would let a member read what it cannot list or the reverse.
func TestVendorAccountMemberReadMatchesTheListedMemberSet(t *testing.T) {
	e := newVendorListingEnv(t)
	principals := map[string]auth.Token{
		"owner":                 token(grantOwner),
		"direct (own + shared)": token(grantDirect),
		"admin-group member":    token(grantAdminMember),
		"user-group member":     token(grantUserGroupMember),
		"invited, not a member": token(grantInvited),
		"stranger":              token(grantStranger),
	}
	check := func(phase string) {
		t.Helper()
		for name, principal := range principals {
			list, err := e.svc.ListVendorAccounts(e.ctx, principal)
			if err != nil {
				t.Fatalf("%s/%s: list: %v", phase, name, err)
			}
			listed := listedIDs(list)
			for _, id := range allFixtureAccounts {
				_, err := e.svc.GetVendorAccount(e.ctx, principal, id)
				if err != nil && !errors.Is(err, ErrVendorAccountNotFound) {
					t.Fatalf("%s/%s/%s: %v", phase, name, id, err)
				}
				if readable, isListed := err == nil, slices.Contains(listed, id); readable != isListed {
					t.Errorf("%s/%s/%s: readable = %v but listed = %v", phase, name, id, readable, isListed)
				}
			}
		}
	}
	check("subscription sharing off")
	e.flags(true, true)
	check("subscription sharing on")
	e.flags(false, false)
	check("sharing paused")
	e.flags(true, false)
	e.setLevel(grantShared, routing.VendorShareLevelNone)
	check("a shared account lowered")
}
