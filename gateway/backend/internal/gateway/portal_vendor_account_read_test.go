// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// GET /api/portal/vendor-accounts and GET /api/portal/vendor-accounts/{id} for a CM of a
// shared vendor account (#183, spec 2026-10-15 sections 6 and 10): the wire of the
// key-free management read. portal.Service decides everything; these tests pin what
// reaches the client -- the management fields, is_owner, shared_by, the 404 of a
// stranger, and above all that the owner's secret is nowhere in the body.

func vaGetRaw(t *testing.T, f *adminGroupsFixture, secret, id string) (int, map[string]any, string) {
	t.Helper()
	rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/vendor-accounts/"+id, secret, "")
	var raw map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, raw, rec.Body.String()
}

func TestPortalVendorAccountCMReadIsKeyFreeOverHTTP(t *testing.T) {
	f := newAdminGroupsFixture(t)
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		id := f.accountAt(t, level)

		// The control: the owner sees is_owner true and no shared_by.
		code, owner, body := vaGetRaw(t, f, vaSystemSecret, id)
		if code != http.StatusOK || owner["is_owner"] != true || owner["shared_by"] != "" || owner["share_level"] != level {
			t.Fatalf("owner GET at %s = %d %s, want 200, is_owner true, shared_by \"\", share_level %s", level, code, body, level)
		}

		code, cm, body := vaGetRaw(t, f, vaCMSecret, id)
		if code != http.StatusOK {
			t.Fatalf("CM GET at %s = %d %s, want 200", level, code, body)
		}
		if strings.Contains(body, vaTestAPIKey) {
			t.Fatalf("CM GET at %s leaks the owner's api key: %s", level, body)
		}
		if cm["is_owner"] != false || cm["shared_by"] != "System" || cm["share_level"] != level {
			t.Errorf("CM GET at %s: is_owner/shared_by/share_level = %v/%v/%v, want false/System/%s", level, cm["is_owner"], cm["shared_by"], cm["share_level"], level)
		}
		if cm["api_key_set"] != true || cm["subscription_connected"] != false {
			t.Errorf("CM GET at %s: api_key_set/subscription_connected = %v/%v, want the booleans true/false", level, cm["api_key_set"], cm["subscription_connected"])
		}
		groups, _ := cm["admin_groups"].([]any)
		if len(groups) != 1 || groups[0].(map[string]any)["id"] != f.ag || groups[0].(map[string]any)["name"] != "AG" {
			t.Errorf("CM GET at %s: admin_groups = %v, want [{AG}]", level, cm["admin_groups"])
		}
		if cm["system_group_id"] != f.sgID || cm["system_group_name"] != "SG" {
			t.Errorf("CM GET at %s: system group = %v / %v, want %s / SG", level, cm["system_group_id"], cm["system_group_name"], f.sgID)
		}
		// No key of the body may even look like a credential, except the two booleans.
		for key := range cm {
			if key == "api_key_set" || key == "subscription_connected" {
				continue
			}
			for _, word := range []string{"secret", "token", "password", "credential", "api_key", "refresh"} {
				if strings.Contains(strings.ToLower(key), word) {
					t.Errorf("CM GET at %s carries a credential-looking field %q", level, key)
				}
			}
		}

		// A stranger gets the same 404 an unknown id gets.
		if code, _, body := vaGetRaw(t, f, vaOtherSecret, id); code != http.StatusNotFound || strings.Contains(body, vaTestAPIKey) {
			t.Errorf("stranger GET at %s = %d %s, want 404", level, code, body)
		}
	}

	// The CM's list carries the managed accounts, key-free, with the viewer-relative fields.
	rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/vendor-accounts", vaCMSecret, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), vaTestAPIKey) {
		t.Fatalf("CM list = %d %s, want 200 without the key", rec.Code, rec.Body.String())
	}
	var list portal.VendorAccountListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("CM list = %d accounts, want the 2 managed ones: %s", len(list.Data), rec.Body.String())
	}
	for _, dto := range list.Data {
		if dto.IsOwner || dto.SharedBy != "System" {
			t.Errorf("CM list entry %s: is_owner/shared_by = %v/%q, want false/System", dto.ID, dto.IsOwner, dto.SharedBy)
		}
	}
}

// With the master sharing flag off an account behaves personal: the CM's GET is a 404
// and their list is empty, over the wire as in the service.
func TestPortalVendorAccountCMReadIsPausedOverHTTPWhileSharingIsOff(t *testing.T) {
	f := newAdminGroupsFixture(t)
	id := f.accountAt(t, routing.VendorShareLevelShared)
	putVendorSystemSetting(t, f.srv, `{"vendor_account_sharing_enabled":false}`)

	if code, _, body := vaGetRaw(t, f, vaCMSecret, id); code != http.StatusNotFound {
		t.Errorf("CM GET, sharing off = %d %s, want 404", code, body)
	}
	rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/vendor-accounts", vaCMSecret, "")
	var list portal.VendorAccountListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Data) != 0 {
		t.Errorf("CM list, sharing off = %d %s, want 200 and an empty list", rec.Code, rec.Body.String())
	}
	if code, owner, body := vaGetRaw(t, f, vaSystemSecret, id); code != http.StatusOK || owner["share_level"] != routing.VendorShareLevelShared {
		t.Errorf("owner GET, sharing off = %d %s, want 200 with the stored level", code, body)
	}
}
