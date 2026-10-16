// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The READ side of shared vendor accounts (#183, spec 2026-10-15 sections 6, 10
// and 13): the DTO's management fields, the key-free read a CM gets, the read gate
// that lets a CM of a shared/full account in, and the management union the list
// returns. Everything here works on the EFFECTIVE share level, so with the master
// flag off an account reads as `none` and a CM is a stranger again.
//
// The load-bearing rule, asserted by planting secrets and looking for them on the
// wire: a CM gets the management fields and NEVER the owner's raw secret, only the
// api_key_set / subscription_connected booleans.

// vendorDTOAllowedFields is the credential-free allowlist of VendorAccountDTO's wire
// fields. The DTO is the ONLY thing a non-owner ever receives of an account, so
// adding a field is a deliberate act: it must be classified here (is it safe for a
// CM, and later for a member, to see?). A field that is not listed fails the test
// below, the same ratchet isVendorAccountPatchOperateField is for the PATCH.
var vendorDTOAllowedFields = map[string]string{
	"id":                     "identity",
	"vendor":                 "metadata",
	"auth_type":              "metadata",
	"name":                   "metadata",
	"status":                 "metadata",
	"model_prefix":           "metadata",
	"base_url":               "metadata (an upstream root, never credentials: it is validated to carry no userinfo or query)",
	"api_key_set":            "boolean credential indicator",
	"subscription_connected": "boolean credential indicator",
	"models":                 "metadata",
	"usage":                  "rate-limit snapshot",
	"created_at":             "metadata",
	"updated_at":             "metadata",
	"share_level":            "management",
	"admin_groups":           "management",
	"system_group_id":        "management",
	"system_group_name":      "management",
	"is_owner":               "viewer relation",
	"shared_by":              "viewer relation (the owner's display name)",
}

// TestVendorAccountDTOFieldsAreAnExplicitCredentialFreeAllowlist is the structural
// half of the key-free guarantee: every wire field of the DTO is on the allowlist
// above, none of them can hold the sealed key or the OAuth token set. A new field is
// refused until someone classifies it.
func TestVendorAccountDTOFieldsAreAnExplicitCredentialFreeAllowlist(t *testing.T) {
	typ := reflect.TypeFor[VendorAccountDTO]()
	have := map[string]bool{}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		have[name] = true
		if _, ok := vendorDTOAllowedFields[name]; !ok {
			t.Errorf("VendorAccountDTO field %q (json %q) is not on the credential-free allowlist: classify it in vendorDTOAllowedFields (is it safe for a CM and a member to see?)", typ.Field(i).Name, name)
		}
	}
	for name := range vendorDTOAllowedFields {
		if !have[name] {
			t.Errorf("the allowlist names %q, which VendorAccountDTO no longer has", name)
		}
	}
	// No allowlisted name may even look like a credential, except the two booleans.
	for name := range vendorDTOAllowedFields {
		if name == "api_key_set" || name == "subscription_connected" {
			continue
		}
		lower := strings.ToLower(name)
		for _, word := range []string{"secret", "token", "password", "credential", "oauth", "api_key", "refresh"} {
			if strings.Contains(lower, word) {
				t.Errorf("allowlisted field %q looks like credential material (%q)", name, word)
			}
		}
	}
}

// plantedVendorAccounts creates two accounts owned by usr_va_owner at level, each
// holding planted credentials: an api-key account (the key) and a subscription
// account (the OAuth access and refresh tokens). The store is volatile, so the
// secrets are stored as "plain:..." and are really in the row, which the helper
// asserts so that no case can go green by never planting anything.
func (e *vendorAuthzEnv) plantedVendorAccounts(level string) (apiKeyID, subscriptionID string) {
	e.t.Helper()
	e.svc.settingsVolatile = true
	apiKeyID = e.newAccountOf(level, CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "Planted key", APIKey: hygieneAPIKey,
	}).ID
	subscriptionID = e.newAccountOf(level, hygieneSubscription(routing.VendorOpenAI)).ID
	setDiscoveryTokens(e.t, e.svc, e.routes, subscriptionID, vendorauth.TokenSet{
		AccessToken: hygieneAccess, RefreshToken: hygieneRefresh, AccountID: discoveryTestAccount,
	})
	if got := e.reload(apiKeyID).APIKey; !strings.Contains(got, hygieneAPIKey) {
		e.t.Fatalf("the planted api key is not in the stored row (%q): the key-free assertions would be vacuous", got)
	}
	if got := e.reload(subscriptionID).OAuthTokens; !strings.Contains(got, hygieneAccess) || !strings.Contains(got, hygieneRefresh) {
		e.t.Fatalf("the planted token set is not in the stored row (%q): the key-free assertions would be vacuous", got)
	}
	return apiKeyID, subscriptionID
}

// plantedSecrets is everything a non-owner must never be sent: the plaintext
// secrets AND the stored (sealed) column values they sit in.
func (e *vendorAuthzEnv) plantedSecrets(ids ...string) []string {
	e.t.Helper()
	secrets := hygieneStoredSecrets()
	for _, id := range ids {
		row := e.reload(id)
		for _, stored := range []string{row.APIKey, row.OAuthTokens} {
			if stored != "" {
				secrets = append(secrets, stored)
			}
		}
	}
	return secrets
}

// requireManagementFields asserts the management view of an account raised through
// the fixture: level, the AG assignment and the SG containment root.
func requireManagementFields(t *testing.T, label string, dto VendorAccountDTO, e *vendorAuthzEnv, level string) {
	t.Helper()
	if dto.ShareLevel != level {
		t.Errorf("%s: share_level = %q, want %q", label, dto.ShareLevel, level)
	}
	if dto.AdminGroups == nil {
		t.Errorf("%s: admin_groups is nil, want a non-nil slice (it must serialize as [])", label)
	}
	if level == routing.VendorShareLevelNone {
		if len(dto.AdminGroups) != 0 || dto.SystemGroupID != "" || dto.SystemGroupName != "" {
			t.Errorf("%s: a private account reports groups/root %+v / %q / %q, want none", label, dto.AdminGroups, dto.SystemGroupID, dto.SystemGroupName)
		}
		return
	}
	if want := []GroupRefDTO{{ID: e.ag.ID, Name: "AG"}}; !reflect.DeepEqual(dto.AdminGroups, want) {
		t.Errorf("%s: admin_groups = %+v, want %+v", label, dto.AdminGroups, want)
	}
	if dto.SystemGroupID != e.ag.ParentGroupID || dto.SystemGroupName != "SG" {
		t.Errorf("%s: system group = %q / %q, want %q / %q", label, dto.SystemGroupID, dto.SystemGroupName, e.ag.ParentGroupID, "SG")
	}
}

// TestVendorAccountDTOReportsTheManagementFields: the owner's read carries the share
// level, the management groups, the containment root (id and name), is_owner true
// and no shared_by (the owner is not sharing it with themselves).
func TestVendorAccountDTOReportsTheManagementFields(t *testing.T) {
	e := newVendorAuthzEnv(t)
	for level, acc := range e.accounts {
		dto, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), acc.ID)
		if err != nil {
			t.Fatalf("owner read at %s: %v", level, err)
		}
		requireManagementFields(t, "owner/"+level, dto, e, level)
		if !dto.IsOwner || dto.SharedBy != "" {
			t.Errorf("owner/%s: is_owner/shared_by = %v/%q, want true/\"\"", level, dto.IsOwner, dto.SharedBy)
		}
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if level == routing.VendorShareLevelNone && !strings.Contains(string(raw), `"admin_groups":[]`) {
			t.Errorf("owner/%s: admin_groups did not serialize as []: %s", level, raw)
		}
	}
}

// TestVendorAccountCMReadIsKeyFree is the security-relevant rule of the task: a CM
// of a shared or full account reads it, gets the management fields, is_owner false and
// the owner's name as shared_by -- and NOTHING of the owner's secret. The owner's own
// read of the same account is the control (is_owner true, same indicators).
func TestVendorAccountCMReadIsKeyFree(t *testing.T) {
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		t.Run(level, func(t *testing.T) {
			e := newVendorAuthzEnv(t)
			keyID, subID := e.plantedVendorAccounts(level)
			secrets := e.plantedSecrets(keyID, subID)
			cases := []struct {
				id                 string
				apiKeySet, subConn bool
			}{{keyID, true, false}, {subID, false, true}}

			for _, tc := range cases {
				for _, who := range []struct {
					name string
					tok  auth.Token
				}{{"cm", token("usr_cm", "admin")}, {"admin-group owner", token("usr_agowner", "admin")}} {
					dto, err := e.svc.GetVendorAccount(e.ctx, who.tok, tc.id)
					if err != nil {
						t.Fatalf("%s read of %s at %s: %v", who.name, tc.id, level, err)
					}
					label := who.name + "/" + level + "/" + tc.id
					requireManagementFields(t, label, dto, e, level)
					if dto.IsOwner {
						t.Errorf("%s: is_owner = true for a non-owner", label)
					}
					if dto.SharedBy != vendorAuthzOwnerID {
						t.Errorf("%s: shared_by = %q, want the owner's name %q", label, dto.SharedBy, vendorAuthzOwnerID)
					}
					if dto.APIKeySet != tc.apiKeySet || dto.SubscriptionConnected != tc.subConn {
						t.Errorf("%s: api_key_set/subscription_connected = %v/%v, want the booleans %v/%v", label, dto.APIKeySet, dto.SubscriptionConnected, tc.apiKeySet, tc.subConn)
					}
					if leaked := leakedSecrets(dto, secrets); len(leaked) > 0 {
						t.Errorf("%s: the CM's DTO leaks the owner's secret %v: %+v", label, leaked, dto)
					}
				}

				ownerDTO, err := e.svc.GetVendorAccount(e.ctx, token(vendorAuthzOwnerID), tc.id)
				if err != nil {
					t.Fatalf("owner read of %s: %v", tc.id, err)
				}
				if !ownerDTO.IsOwner || ownerDTO.SharedBy != "" || ownerDTO.APIKeySet != tc.apiKeySet || ownerDTO.SubscriptionConnected != tc.subConn {
					t.Errorf("owner/%s: is_owner/shared_by/api_key_set/subscription_connected = %v/%q/%v/%v, want true/\"\"/%v/%v",
						tc.id, ownerDTO.IsOwner, ownerDTO.SharedBy, ownerDTO.APIKeySet, ownerDTO.SubscriptionConnected, tc.apiKeySet, tc.subConn)
				}
			}

			// The list the CM sees carries the same accounts key-free: the two planted
			// ones plus the fixture's own shared and full accounts.
			list, err := e.svc.ListVendorAccounts(e.ctx, token("usr_cm", "admin"))
			if err != nil {
				t.Fatalf("CM list: %v", err)
			}
			wantIDs := e.oldestFirst(keyID, subID, e.accounts[routing.VendorShareLevelShared].ID, e.accounts[routing.VendorShareLevelFull].ID)
			if got := listedIDs(list); !reflect.DeepEqual(got, wantIDs) {
				t.Fatalf("CM list at %s = %v, want the managed accounts %v", level, got, wantIDs)
			}
			if leaked := leakedSecrets(list, secrets); len(leaked) > 0 {
				t.Errorf("the CM's list leaks the owner's secret %v: %+v", leaked, list)
			}
		})
	}
}

// TestVendorAccountCMWriteResponsesAreViewerAware: the operate calls a CM may make
// answer the account's DTO too; it is rendered for the CALLER (is_owner false,
// shared_by the owner), not for the owner, and carries no secret.
func TestVendorAccountCMWriteResponsesAreViewerAware(t *testing.T) {
	e := newVendorAuthzEnv(t)
	keyID, _ := e.plantedVendorAccounts(routing.VendorShareLevelShared)
	secrets := e.plantedSecrets(keyID)
	cm := token("usr_cm", "admin")

	renamed := "renamed by the CM"
	dto, err := e.svc.UpdateVendorAccount(e.ctx, cm, keyID, UpdateVendorAccountRequest{Name: &renamed})
	if err != nil {
		t.Fatalf("CM rename: %v", err)
	}
	if dto.IsOwner || dto.SharedBy != vendorAuthzOwnerID || dto.Name != renamed || dto.ShareLevel != routing.VendorShareLevelShared {
		t.Errorf("CM PATCH response is_owner/shared_by/name/share_level = %v/%q/%q/%q, want false/%q/%q/shared",
			dto.IsOwner, dto.SharedBy, dto.Name, dto.ShareLevel, vendorAuthzOwnerID, renamed)
	}
	if leaked := leakedSecrets(dto, secrets); len(leaked) > 0 {
		t.Errorf("CM PATCH response leaks %v", leaked)
	}

	refreshed, _, err := e.svc.RefreshVendorAccountModels(e.ctx, cm, keyID)
	if err != nil {
		t.Fatalf("CM model refresh: %v", err)
	}
	if refreshed.IsOwner || refreshed.SharedBy != vendorAuthzOwnerID {
		t.Errorf("CM refresh response is_owner/shared_by = %v/%q, want false/%q", refreshed.IsOwner, refreshed.SharedBy, vendorAuthzOwnerID)
	}
	if leaked := leakedSecrets(refreshed, secrets); len(leaked) > 0 {
		t.Errorf("CM refresh response leaks %v", leaked)
	}

	// The owner's response to the very same call says is_owner true.
	ownerDTO, err := e.svc.UpdateVendorAccount(e.ctx, token(vendorAuthzOwnerID), keyID, UpdateVendorAccountRequest{Name: &renamed})
	if err != nil || !ownerDTO.IsOwner || ownerDTO.SharedBy != "" {
		t.Errorf("owner PATCH response = %+v, %v, want is_owner true and no shared_by", ownerDTO, err)
	}
}

// TestVendorAccountReadGate is the read column of the spec section 6 table, for every
// kind of principal at every level: the owner always; a CM of a shared/full account
// (the admin-group owner is one by the primitive's definition); system scope keeps its
// read of everything; every other caller is a stranger and gets the same 404 an unknown
// id gets -- never a 403, which would confirm the account exists.
func TestVendorAccountReadGate(t *testing.T) {
	e := newVendorAuthzEnv(t)
	allowed := map[string]map[string]bool{
		routing.VendorShareLevelNone:   {"owner": true, "system scope": true},
		routing.VendorShareLevelShared: {"owner": true, "cm": true, "admin-group owner": true, "system scope": true},
		routing.VendorShareLevelFull:   {"owner": true, "cm": true, "admin-group owner": true, "system scope": true},
	}
	for level, acc := range e.accounts {
		for _, p := range vendorAuthzPrincipals() {
			label := "read/" + level + "/" + p.name
			before := e.snapshot(acc.ID)
			dto, err := e.svc.GetVendorAccount(e.ctx, p.tok, acc.ID)
			if allowed[level][p.name] {
				if err != nil || dto.ID != acc.ID {
					t.Errorf("%s: = %+v, %v, want the account", label, dto, err)
					continue
				}
				if wantOwner := p.name == "owner"; dto.IsOwner != wantOwner {
					t.Errorf("%s: is_owner = %v, want %v", label, dto.IsOwner, wantOwner)
				}
			} else {
				requireAuthzOutcome(t, label, err, authzNotFound)
			}
			e.requireUnchanged(label, before)
		}
	}
}

// TestVendorAccountReadIsPausedWhileSharingIsOff: with vendor_account_sharing_enabled
// off every account behaves personal (spec section 13), so the CM's management read is
// gone -- 404 like a stranger -- while the owner (who sees the STORED level: paused, not
// cleared) and system scope are unaffected. Back on, the CM reads again.
func TestVendorAccountReadIsPausedWhileSharingIsOff(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cm := token("usr_cm", "admin")
	owner := token(vendorAuthzOwnerID)
	sys := token("usr_s", "system", "admin")

	setVendorSharingFlags(t, e.svc, false, false)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := e.accounts[level]
		if _, err := e.svc.GetVendorAccount(e.ctx, cm, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("CM read at %s, sharing off: err = %v, want ErrVendorAccountNotFound", level, err)
		}
		if got := e.svc.visibleVendorAccountName(e.ctx, cm, acc.ID); got != "" {
			t.Errorf("CM sees the name %q of a paused %s account, want none", got, level)
		}
		dto, err := e.svc.GetVendorAccount(e.ctx, owner, acc.ID)
		if err != nil || dto.ShareLevel != level || !dto.IsOwner {
			t.Errorf("owner read at %s, sharing off = %+v, %v, want the account with its stored level", level, dto, err)
		}
		if _, err := e.svc.GetVendorAccount(e.ctx, sys, acc.ID); err != nil {
			t.Errorf("system read at %s, sharing off: %v", level, err)
		}
	}
	list, err := e.svc.ListVendorAccounts(e.ctx, cm)
	if err != nil || len(list.Data) != 0 {
		t.Errorf("CM list, sharing off = %+v, %v, want an empty list (every account behaves personal)", list, err)
	}

	setVendorSharingFlags(t, e.svc, true, false)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		if _, err := e.svc.GetVendorAccount(e.ctx, cm, e.accounts[level].ID); err != nil {
			t.Errorf("CM read at %s, sharing back on: %v", level, err)
		}
	}
}

// TestVendorAccountReadFailsClosedWithoutALiveSharedLevel: an admin-group link that
// outlives the level that justified it grants no read, whatever the stored level is
// (`none`, empty or garbage).
func TestVendorAccountReadFailsClosedWithoutALiveSharedLevel(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cm := token("usr_cm", "admin")
	for _, level := range []string{routing.VendorShareLevelNone, "", "bogus"} {
		acc := e.newAccountAt(routing.VendorShareLevelNone)
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
			t.Fatalf("link: %v", err)
		}
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, level); err != nil {
			t.Fatalf("set level %q: %v", level, err)
		}
		if _, err := e.svc.GetVendorAccount(e.ctx, cm, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Errorf("CM read with a stale link at %q: err = %v, want ErrVendorAccountNotFound", level, err)
		}
		if got := e.svc.visibleVendorAccountName(e.ctx, cm, acc.ID); got != "" {
			t.Errorf("CM sees the name %q of an account at %q with a stale link", got, level)
		}
	}
}

// TestVendorAccountReadRevokesAfterAdminGroupRemoval: the CM reach is the LIVE
// intersection, so demoting the co-manager or unassigning the group removes the read
// on the next call (like the write gates).
func TestVendorAccountReadRevokesAfterAdminGroupRemoval(t *testing.T) {
	e := newVendorAuthzEnv(t)
	acc := e.accounts[routing.VendorShareLevelShared]
	cm := token("usr_cm", "admin")
	if _, err := e.svc.GetVendorAccount(e.ctx, cm, acc.ID); err != nil {
		t.Fatalf("CM read before: %v", err)
	}
	if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if _, err := e.svc.GetVendorAccount(e.ctx, cm, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("CM read after unassign: err = %v, want ErrVendorAccountNotFound", err)
	}
}

// listedIDs returns the ids of a list, in order.
func listedIDs(list VendorAccountListResponse) []string {
	ids := make([]string, 0, len(list.Data))
	for _, dto := range list.Data {
		ids = append(ids, dto.ID)
	}
	return ids
}

// oldestFirst orders account ids as the list does (created_at, then id).
func (e *vendorAuthzEnv) oldestFirst(ids ...string) []string {
	e.t.Helper()
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := e.reload(out[i]), e.reload(out[j])
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out
}

// ownedAccount creates an account owned by userID and puts it at level, assigned to AG
// with the containment root (the state the lifecycle produces).
func (e *vendorAuthzEnv) ownedAccount(userID, name, level string) string {
	e.t.Helper()
	dto, err := e.svc.CreateVendorAccount(e.ctx, token(userID, "admin"), CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: name,
	})
	if err != nil {
		e.t.Fatalf("CreateVendorAccount(%s): %v", name, err)
	}
	if level != routing.VendorShareLevelNone {
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, dto.ID, level); err != nil {
			e.t.Fatalf("raise %s: %v", name, err)
		}
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, dto.ID, e.ag.ID); err != nil {
			e.t.Fatalf("assign %s: %v", name, err)
		}
		if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, dto.ID, e.ag.ParentGroupID); err != nil {
			e.t.Fatalf("root %s: %v", name, err)
		}
	}
	return dto.ID
}

// TestListVendorAccountsReturnsOwnUnionManaged: a CM's list is their OWN accounts
// (first, oldest first) then the accounts they manage that are shared or full, once
// each; the managed ones are rendered for the CM (is_owner false, shared_by the owner).
// An account that is `none` but still carries an admin-group link (a raise that tore
// halfway) is NOT in it: the level, not the link, makes an account shared.
func TestListVendorAccountsReturnsOwnUnionManaged(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cm := token("usr_cm", "admin")

	cmOwnPrivate := e.ownedAccount("usr_cm", "cm-own-private", routing.VendorShareLevelNone)
	cmOwnShared := e.ownedAccount("usr_cm", "cm-own-shared", routing.VendorShareLevelShared) // own AND managed: once
	shared := e.accounts[routing.VendorShareLevelShared].ID
	full := e.accounts[routing.VendorShareLevelFull].ID
	private := e.accounts[routing.VendorShareLevelNone].ID
	orphan := e.newAccountAt(routing.VendorShareLevelNone).ID // none + a stale link
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, orphan, e.ag.ID); err != nil {
		t.Fatalf("stale link: %v", err)
	}

	list, err := e.svc.ListVendorAccounts(e.ctx, cm)
	if err != nil {
		t.Fatalf("CM list: %v", err)
	}
	want := append(e.oldestFirst(cmOwnPrivate, cmOwnShared), e.oldestFirst(shared, full)...)
	if got := listedIDs(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("CM list = %v\nwant own first (oldest first), then managed: %v\n(private %s and the orphaned none account %s must be absent)", got, want, private, orphan)
	}
	for _, dto := range list.Data {
		switch dto.ID {
		case cmOwnPrivate, cmOwnShared:
			if !dto.IsOwner || dto.SharedBy != "" {
				t.Errorf("own account %s: is_owner/shared_by = %v/%q, want true/\"\"", dto.ID, dto.IsOwner, dto.SharedBy)
			}
		default:
			if dto.IsOwner || dto.SharedBy != vendorAuthzOwnerID {
				t.Errorf("managed account %s: is_owner/shared_by = %v/%q, want false/%q", dto.ID, dto.IsOwner, dto.SharedBy, vendorAuthzOwnerID)
			}
			if dto.ShareLevel != e.reload(dto.ID).ShareLevel || len(dto.AdminGroups) != 1 {
				t.Errorf("managed account %s: management fields = %q / %+v", dto.ID, dto.ShareLevel, dto.AdminGroups)
			}
		}
	}
}

// TestListVendorAccountsManagementUnionByPrincipal: who gets what from the list. The
// owner lists every account they own at any level; a CM, and the owner of the admin
// group (a CM by definition), the managed ones; a co-manager without the vendor flag,
// the CM and owner of another admin group, a stranger and a principal with no user
// identity get no one else's account. System scope keeps its OWN-only list (the page is
// personal; it widens reads by id, never the list), even for a system-scope token whose
// user is a CM.
func TestListVendorAccountsManagementUnionByPrincipal(t *testing.T) {
	e := newVendorAuthzEnv(t)
	shared := e.accounts[routing.VendorShareLevelShared].ID
	full := e.accounts[routing.VendorShareLevelFull].ID
	private := e.accounts[routing.VendorShareLevelNone].ID
	ownCM := e.ownedAccount("usr_cm", "cm-own", routing.VendorShareLevelNone)

	cases := []struct {
		name string
		tok  auth.Token
		want []string
	}{
		{"owner", token(vendorAuthzOwnerID), e.oldestFirst(private, shared, full)},
		{"cm", token("usr_cm", "admin"), append([]string{ownCM}, e.oldestFirst(shared, full)...)},
		{"admin-group owner", token("usr_agowner", "admin"), e.oldestFirst(shared, full)},
		{"co-manager without the vendor flag", token("usr_cmx", "admin"), []string{}},
		{"CM of another admin group", token("usr_cm2", "admin"), []string{}},
		{"owner of another admin group", token("usr_agowner2", "admin"), []string{}},
		{"stranger", token("usr_stranger"), []string{}},
		{"system scope, user is no CM", token("usr_s", "system", "admin"), []string{}},
		{"system scope, user is a CM", token("usr_cm", "system", "admin"), []string{ownCM}},
		{"no user identity", auth.Token{Scopes: []string{"gateway:use"}}, []string{}},
	}
	for _, tc := range cases {
		list, err := e.svc.ListVendorAccounts(e.ctx, tc.tok)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := listedIDs(list); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: list = %v, want %v", tc.name, got, tc.want)
		}
		if list.Data == nil {
			t.Errorf("%s: Data is nil, want [] on the wire", tc.name)
		}
	}
}

// TestListVendorAccountsManagedSetFailsClosedOnTheEffectiveLevel: the managed arm is
// filtered on the EFFECTIVE share level, so an account whose stored level is not
// shared or full (`none`, empty, garbage) stays out whatever links it carries, and
// with the master flag off the managed set is EMPTY (every account behaves personal)
// -- yet the CM's own accounts, and the owner's shared ones, are still listed.
func TestListVendorAccountsManagedSetFailsClosedOnTheEffectiveLevel(t *testing.T) {
	e := newVendorAuthzEnv(t)
	cm := token("usr_cm", "admin")
	owner := token(vendorAuthzOwnerID)

	for _, level := range []string{"", "bogus"} {
		acc := e.newAccountAt(routing.VendorShareLevelNone)
		if err := e.routes.SetVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
			t.Fatalf("link: %v", err)
		}
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, acc.ID, level); err != nil {
			t.Fatalf("set level %q: %v", level, err)
		}
	}
	shared := e.accounts[routing.VendorShareLevelShared].ID
	full := e.accounts[routing.VendorShareLevelFull].ID
	list, err := e.svc.ListVendorAccounts(e.ctx, cm)
	if err != nil {
		t.Fatalf("CM list: %v", err)
	}
	if got, want := listedIDs(list), e.oldestFirst(shared, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("CM list with unrecognised stored levels = %v, want only the shared/full accounts %v", got, want)
	}

	ownCM := e.ownedAccount("usr_cm", "cm-own", routing.VendorShareLevelShared)
	setVendorSharingFlags(t, e.svc, false, false)
	list, err = e.svc.ListVendorAccounts(e.ctx, cm)
	if err != nil {
		t.Fatalf("CM list, sharing off: %v", err)
	}
	if got, want := listedIDs(list), []string{ownCM}; !reflect.DeepEqual(got, want) {
		t.Errorf("CM list, sharing off = %v, want only the CM's own account %v", got, want)
	}
	ownerList, err := e.svc.ListVendorAccounts(e.ctx, owner)
	if err != nil || len(ownerList.Data) < 3 {
		t.Fatalf("owner list, sharing off = %d accounts, %v, want every owned account", len(ownerList.Data), err)
	}
	for _, dto := range ownerList.Data {
		if !dto.IsOwner {
			t.Errorf("owner list, sharing off: %s is_owner = false", dto.ID)
		}
	}
}

// visibleVendorAccountName is the Activity list's display-name lookup. It follows the
// read gate: the owner, a CM of a shared/full account and system scope see the name;
// a stranger and a CM of a paused or private account do not.
func TestVisibleVendorAccountNameCoversCMs(t *testing.T) {
	e := newVendorAuthzEnv(t)
	sys := token("usr_s", "system", "admin")
	for level, acc := range e.accounts {
		visible := map[string]bool{"owner": true, "system scope": true}
		if level != routing.VendorShareLevelNone {
			visible["cm"], visible["admin-group owner"] = true, true
		}
		for _, p := range vendorAuthzPrincipals() {
			got := e.svc.visibleVendorAccountName(e.ctx, p.tok, acc.ID)
			want := ""
			if visible[p.name] {
				want = acc.Name
			}
			if got != want {
				t.Errorf("visibleVendorAccountName(%s at %s) = %q, want %q", p.name, level, got, want)
			}
		}
	}
	if got := e.svc.visibleVendorAccountName(e.ctx, sys, "va_missing"); got != "" {
		t.Errorf("visibleVendorAccountName of an unknown id = %q, want \"\"", got)
	}
}
