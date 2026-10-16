// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// GET /api/portal/vendor-accounts and GET /api/portal/vendor-accounts/{id} for a MEMBER of
// a shared vendor account (#183, spec 2026-10-15 sections 6 and 10): a principal that
// reaches the account only through a resource group it is provisioned into reads it as a
// MINIMAL key-free DTO. portal.Service decides everything; these tests pin what reaches the
// client: the borrower-facing fields, is_owner false and shared_by, and above all that
// neither the owner's secret nor any management field (share level, management groups,
// containment root, base url, usage) is anywhere in the body.

// vaMemberVisibleKeys are the keys a member's body may carry with a value; every other key
// of the DTO is on the wire (the shape is constant) but empty.
var vaMemberVisibleKeys = map[string]bool{
	"id": true, "vendor": true, "auth_type": true, "name": true, "status": true, "model_prefix": true,
	"api_key_set": true, "subscription_connected": true, "models": true, "is_owner": true, "shared_by": true,
}

// vaMemberFixture is the admin-groups fixture (the system user owns the account at level,
// AG's CM manages it) plus a resource group RG containing the account and provisioning
// usr_va_b, whose token is vaOtherSecret: the member. usr_va_a (vaOwnerSecret) is provisioned
// nowhere and is the stranger.
func vaMemberFixture(t *testing.T, level string) (f *adminGroupsFixture, accountID string) {
	t.Helper()
	f = newAdminGroupsFixture(t)
	accountID = f.accountAt(t, level)
	rg := f.resourceGroup(t, "RG", f.ag)
	ctx := context.Background()
	if err := f.routes.SetResourceGroupVendorAccount(ctx, rg, accountID); err != nil {
		t.Fatalf("link the account into the resource group: %v", err)
	}
	if err := f.routes.SetResourceGroupProvision(ctx, rg, routing.ProvisionKindUser, "usr_va_b"); err != nil {
		t.Fatalf("provision the member: %v", err)
	}
	// A usage snapshot, so the member's view lacking it is not vacuous.
	reset := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := f.routes.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{
		AccountID: accountID, FiveHourPct: 42, FiveHourResetAt: &reset, WeeklyPct: 7,
		SpendUnit: "USD", SpendLimit: "100", SpendUsed: "55", SpendUsedPct: 55, UpdatedAt: reset,
	}); err != nil {
		t.Fatalf("seed the usage snapshot: %v", err)
	}
	return f, accountID
}

// requireVaMemberBody asserts a decoded account body is the member's view: the borrower
// fields filled, is_owner false, shared_by the owner's name, the credential booleans only,
// every management field empty and the usage snapshot not on the wire at all.
func requireVaMemberBody(t *testing.T, label string, body map[string]any, raw string) {
	t.Helper()
	if strings.Contains(raw, vaTestAPIKey) {
		t.Fatalf("%s leaks the owner's api key: %s", label, raw)
	}
	if body["is_owner"] != false || body["shared_by"] != "System" {
		t.Errorf("%s: is_owner/shared_by = %v/%v, want false/System", label, body["is_owner"], body["shared_by"])
	}
	if body["api_key_set"] != true || body["subscription_connected"] != false {
		t.Errorf("%s: api_key_set/subscription_connected = %v/%v, want the booleans true/false", label, body["api_key_set"], body["subscription_connected"])
	}
	if body["share_level"] != "" || body["system_group_id"] != "" || body["system_group_name"] != "" || body["base_url"] != "" {
		t.Errorf("%s: a member sees management fields: share_level %v, system group %v/%v, base_url %v",
			label, body["share_level"], body["system_group_id"], body["system_group_name"], body["base_url"])
	}
	if groups, ok := body["admin_groups"].([]any); !ok || len(groups) != 0 {
		t.Errorf("%s: admin_groups = %v, want []", label, body["admin_groups"])
	}
	if _, ok := body["usage"]; ok {
		t.Errorf("%s: a member's body carries the usage snapshot: %v", label, body["usage"])
	}
	models, _ := body["models"].([]any)
	if len(models) == 0 {
		t.Errorf("%s: a member sees no models, want the names it can request", label)
	}
	for _, m := range models {
		entry, _ := m.(map[string]any)
		if entry["upstream_model"] != "" || entry["gateway_model"] == "" || entry["api_flavor"] == "" {
			t.Errorf("%s: model %v: a member sees the gateway name and dialect, never the vendor's own id", label, m)
		}
	}
	for key, value := range body {
		if vaMemberVisibleKeys[key] {
			continue
		}
		switch v := value.(type) {
		case string:
			zeroTime := (key == "created_at" || key == "updated_at") && strings.HasPrefix(v, "0001-01-01")
			if v != "" && !zeroTime {
				t.Errorf("%s: field %q = %q is outside the member allowlist", label, key, v)
			}
		case []any:
			if len(v) != 0 {
				t.Errorf("%s: field %q = %v is outside the member allowlist", label, key, v)
			}
		case bool, nil:
		default:
			t.Errorf("%s: field %q = %v is outside the member allowlist", label, key, v)
		}
	}
	for key := range body {
		if key == "api_key_set" || key == "subscription_connected" {
			continue
		}
		for _, word := range []string{"secret", "token", "password", "credential", "api_key", "refresh"} {
			if strings.Contains(strings.ToLower(key), word) {
				t.Errorf("%s carries a credential-looking field %q", label, key)
			}
		}
	}
}

func TestPortalVendorAccountMemberReadIsMinimalOverHTTP(t *testing.T) {
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		t.Run(level, func(t *testing.T) {
			f, id := vaMemberFixture(t, level)

			code, member, raw := vaGetRaw(t, f, vaOtherSecret, id)
			if code != http.StatusOK {
				t.Fatalf("member GET at %s = %d %s, want 200", level, code, raw)
			}
			requireVaMemberBody(t, "member GET at "+level, member, raw)
			if member["id"] != id || member["name"] != "to assign "+level || member["status"] != "active" {
				t.Errorf("member GET at %s: id/name/status = %v/%v/%v, want the account's", level, member["id"], member["name"], member["status"])
			}

			// The controls: the CM's body of the same account carries what the member's
			// lacks (so the absence above is the member tier's doing), the owner's is whole.
			code, cm, body := vaGetRaw(t, f, vaCMSecret, id)
			if code != http.StatusOK || cm["share_level"] != level || cm["usage"] == nil {
				t.Fatalf("CM GET at %s = %d %s, want the management view with usage", level, code, body)
			}
			if groups, _ := cm["admin_groups"].([]any); len(groups) != 1 {
				t.Errorf("CM GET at %s: admin_groups = %v, want [AG]", level, cm["admin_groups"])
			}
			code, owner, body := vaGetRaw(t, f, vaSystemSecret, id)
			if code != http.StatusOK || owner["is_owner"] != true || owner["share_level"] != level || owner["usage"] == nil {
				t.Fatalf("owner GET at %s = %d %s, want the full view", level, code, body)
			}

			// A stranger (provisioned nowhere) gets the 404 an unknown id gets.
			if code, _, body := vaGetRaw(t, f, vaOwnerSecret, id); code != http.StatusNotFound || strings.Contains(body, vaTestAPIKey) {
				t.Errorf("stranger GET at %s = %d %s, want 404", level, code, body)
			}
			if code, _, _ := vaGetRaw(t, f, vaOtherSecret, "va_missing"); code != http.StatusNotFound {
				t.Errorf("member GET of an unknown id = %d, want 404", code)
			}

			// The member's list row is the same minimal view.
			rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/vendor-accounts", vaOtherSecret, "")
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), vaTestAPIKey) {
				t.Fatalf("member list = %d %s, want 200 without the key", rec.Code, rec.Body.String())
			}
			var list struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("unmarshal list: %v", err)
			}
			if len(list.Data) != 1 || list.Data[0]["id"] != id {
				t.Fatalf("member list = %s, want exactly the one shared account", rec.Body.String())
			}
			entry, err := json.Marshal(list.Data[0])
			if err != nil {
				t.Fatal(err)
			}
			requireVaMemberBody(t, "member list row at "+level, list.Data[0], string(entry))
		})
	}
}

// With the master sharing flag off an account behaves personal: a member's GET is a 404 and
// its list is empty, over the wire as in the service, and switching the flag back on
// restores the read.
func TestPortalVendorAccountMemberReadIsPausedOverHTTPWhileSharingIsOff(t *testing.T) {
	f, id := vaMemberFixture(t, routing.VendorShareLevelShared)
	putVendorSystemSetting(t, f.srv, `{"vendor_account_sharing_enabled":false}`)

	if code, _, body := vaGetRaw(t, f, vaOtherSecret, id); code != http.StatusNotFound {
		t.Errorf("member GET, sharing off = %d %s, want 404", code, body)
	}
	rec := vaDo(t, f.srv, http.MethodGet, "/api/portal/vendor-accounts", vaOtherSecret, "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Data) != 0 {
		t.Errorf("member list, sharing off = %d %s, want 200 and an empty list", rec.Code, rec.Body.String())
	}

	putVendorSystemSetting(t, f.srv, `{"vendor_account_sharing_enabled":true}`)
	if code, _, body := vaGetRaw(t, f, vaOtherSecret, id); code != http.StatusOK {
		t.Errorf("member GET, sharing back on = %d %s, want 200", code, body)
	}
}
