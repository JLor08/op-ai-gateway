// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/routing"
	"testing"
)

// PATCH and DELETE /api/portal/vendor-accounts/{id} for a MEMBER of a shared vendor
// account (#234): a principal that reaches the account only through a resource group it
// is provisioned into can already read it (see the member read tests), so a write it
// attempts is a 403 vendor_account.forbidden, like a CM at `shared`, and not the 404 a
// stranger gets. portal.Service decides; these tests pin the status and code on the wire
// and, as the no-existence-leak half, that a stranger keeps the 404 for the very same
// requests.

// vaMemberWrites are the writes the issue names: the operate PATCH fields, the api-key
// PATCH (set and clear) and the DELETE. DELETE runs last, so a regression that let it
// through could not mask the others.
var vaMemberWrites = []struct {
	name, method, body string
}{
	{"operate PATCH status", http.MethodPatch, `{"status":"disabled"}`},
	{"operate PATCH name", http.MethodPatch, `{"name":"Hijacked"}`},
	{"operate PATCH model_prefix", http.MethodPatch, `{"model_prefix":"hijack-"}`},
	{"api-key PATCH", http.MethodPatch, `{"api_key":"sk-member-attempt"}`},
	{"api-key clear", http.MethodPatch, `{"api_key":""}`},
	{"DELETE", http.MethodDelete, ""},
}

func TestPortalVendorAccountMemberWriteIsForbiddenOverHTTP(t *testing.T) {
	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		t.Run(level, func(t *testing.T) {
			f, id := vaMemberFixture(t, level)
			path := "/api/portal/vendor-accounts/" + id
			_, _, before := vaGetRaw(t, f, vaSystemSecret, id)

			// The premise: the member discloses the account by reading it.
			if code, _, body := vaGetRaw(t, f, vaOtherSecret, id); code != http.StatusOK {
				t.Fatalf("member GET at %s = %d %s, want 200", level, code, body)
			}
			for _, w := range vaMemberWrites {
				requireStatusCode(t, "member "+w.name+" at "+level,
					vaDo(t, f.srv, w.method, path, vaOtherSecret, w.body), http.StatusForbidden, "vendor_account.forbidden")
				// A stranger (provisioned nowhere) cannot tell the account exists: the very
				// same request is the 404 an unknown id gets.
				requireStatusCode(t, "stranger "+w.name+" at "+level,
					vaDo(t, f.srv, w.method, path, vaOwnerSecret, w.body), http.StatusNotFound, "vendor_account.not_found")
				requireStatusCode(t, "member "+w.name+" of an unknown id",
					vaDo(t, f.srv, w.method, "/api/portal/vendor-accounts/va_missing", vaOtherSecret, w.body), http.StatusNotFound, "vendor_account.not_found")
			}

			// Nothing was written by any denied call: the owner's view is byte-identical.
			if code, _, after := vaGetRaw(t, f, vaSystemSecret, id); code != http.StatusOK || after != before {
				t.Errorf("a denied member write changed the account:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// While vendor_account_sharing_enabled is off every account behaves personal, so the
// member is a stranger again: its writes are 404, over the wire as in the service, and
// switching the flag back on restores the 403.
func TestPortalVendorAccountMemberWriteIsNotFoundOverHTTPWhileSharingIsOff(t *testing.T) {
	f, id := vaMemberFixture(t, routing.VendorShareLevelShared)
	path := "/api/portal/vendor-accounts/" + id

	putVendorSystemSetting(t, f.srv, `{"vendor_account_sharing_enabled":false}`)
	for _, w := range vaMemberWrites {
		requireStatusCode(t, "member "+w.name+", sharing off",
			vaDo(t, f.srv, w.method, path, vaOtherSecret, w.body), http.StatusNotFound, "vendor_account.not_found")
	}

	putVendorSystemSetting(t, f.srv, `{"vendor_account_sharing_enabled":true}`)
	requireStatusCode(t, "member operate PATCH, sharing back on",
		vaDo(t, f.srv, http.MethodPatch, path, vaOtherSecret, `{"name":"Hijacked"}`), http.StatusForbidden, "vendor_account.forbidden")
}

// A CM is answered by the manager branch, ahead of the member check, so its answers are
// what they were: operate allowed at shared and full, the credential gate 403 at shared
// and allowed at full -- also when the CM is provisioned into the account's group too.
func TestPortalVendorAccountCMWriteAnswersAreUnchangedOverHTTP(t *testing.T) {
	f, id := vaMemberFixture(t, routing.VendorShareLevelShared)
	// The CM is also a member of the shared account: its own resource group holds the
	// account and provisions the CM.
	ctx := context.Background()
	rg := f.resourceGroup(t, "RG-CM", f.ag)
	if err := f.routes.SetResourceGroupVendorAccount(ctx, rg, id); err != nil {
		t.Fatalf("link the account into the CM's resource group: %v", err)
	}
	if err := f.routes.SetResourceGroupProvision(ctx, rg, routing.ProvisionKindUser, "usr_va_cm"); err != nil {
		t.Fatalf("provision the CM: %v", err)
	}
	path := "/api/portal/vendor-accounts/" + id

	requireStatusCode(t, "CM operate PATCH at shared",
		vaDo(t, f.srv, http.MethodPatch, path, vaCMSecret, `{"name":"Renamed by CM"}`), http.StatusOK, "")
	requireStatusCode(t, "CM api-key PATCH at shared",
		vaDo(t, f.srv, http.MethodPatch, path, vaCMSecret, `{"api_key":"sk-cm-attempt"}`), http.StatusForbidden, "vendor_account.forbidden")
	requireStatusCode(t, "CM DELETE at shared",
		vaDo(t, f.srv, http.MethodDelete, path, vaCMSecret, ""), http.StatusForbidden, "vendor_account.forbidden")

	fullID := f.accountAt(t, routing.VendorShareLevelFull)
	fullPath := "/api/portal/vendor-accounts/" + fullID
	requireStatusCode(t, "CM operate PATCH at full",
		vaDo(t, f.srv, http.MethodPatch, fullPath, vaCMSecret, `{"name":"Renamed by CM"}`), http.StatusOK, "")
	requireStatusCode(t, "CM api-key PATCH at full",
		vaDo(t, f.srv, http.MethodPatch, fullPath, vaCMSecret, `{"api_key":"`+vaTestAPIKey+`"}`), http.StatusOK, "")
	requireStatusCode(t, "CM DELETE at full",
		vaDo(t, f.srv, http.MethodDelete, fullPath, vaCMSecret, ""), http.StatusOK, "")
}
