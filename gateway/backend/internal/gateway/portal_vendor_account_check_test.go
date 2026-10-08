// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"testing"
	"time"
)

// vaProbe is a controllable credential probe: it answers the verdict set on it
// and records every credential it was asked about, so a test can assert both the
// verdict on the wire and which credential reached the vendor seam.
type vaProbe struct {
	check vendorauth.CredentialCheck
	seen  []string
}

func (p *vaProbe) validate(_ context.Context, _ *http.Client, credential string) vendorauth.CredentialCheck {
	p.seen = append(p.seen, credential)
	return p.check
}

func (p *vaProbe) validators() portal.VendorCredentialValidators {
	return portal.VendorCredentialValidators{
		OpenAISubscription:    p.validate,
		AnthropicSubscription: p.validate,
		OpenAIAPIKey:          p.validate,
		AnthropicAPIKey:       p.validate,
	}
}

// newVendorCheckTestServer is newVendorAccountTestServer (flag ON, volatile
// RAM-mode sealing) with all four credential probes replaced by probe.
func newVendorCheckTestServer(t *testing.T, probe *vaProbe) (*Server, *routing.MemoryStore) {
	t.Helper()
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(deps *portal.ServiceDeps) {
		deps.VendorValidators = probe.validators()
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore
}

func vaCheckPath(id string) string {
	return "/api/portal/vendor-accounts/" + id + "/check"
}

// POST .../{id}/check answers 200 with the credential-free verdict for each of
// the three statuses, tested against the account's stored api key.
func TestVendorAccountCheckEndpointReportsEachVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		check  vendorauth.CredentialCheck
		status string
	}{
		{"valid", vendorauth.CredentialCheck{Status: vendorauth.StatusValid, Detail: "credential accepted (HTTP 200)"}, portal.VendorConnectionValid},
		{"invalid", vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "credential rejected (HTTP 401, invalid_api_key)"}, portal.VendorConnectionInvalid},
		{"unverifiable", vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, Detail: "vendor answered HTTP 429"}, portal.VendorConnectionUnverifiable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &vaProbe{check: tc.check}
			srv, _ := newVendorCheckTestServer(t, probe)
			acc := vaCreate(t, srv, vaOwnerSecret, "Key account")

			rec := vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID), vaOwnerSecret, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("check status = %d, want 200, body = %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), vaTestAPIKey) {
				t.Fatalf("check response leaks the api key: %s", rec.Body.String())
			}
			// The wire contract is exactly status / detail / checked_at.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
				t.Fatalf("unmarshal fields: %v (%s)", err, rec.Body.String())
			}
			if len(fields) != 3 || fields["status"] == nil || fields["detail"] == nil || fields["checked_at"] == nil {
				t.Fatalf("response fields = %s, want exactly status, detail and checked_at", rec.Body.String())
			}
			var got portal.VendorConnectionCheck
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal check: %v (%s)", err, rec.Body.String())
			}
			if got.Status != tc.status || got.Detail != tc.check.Detail || got.CheckedAt.IsZero() {
				t.Fatalf("check = %+v, want status %q, detail %q and a checked_at time", got, tc.status, tc.check.Detail)
			}
			if len(probe.seen) != 1 || probe.seen[0] != vaTestAPIKey {
				t.Fatalf("probe saw %v, want exactly the account's stored api key", probe.seen)
			}
		})
	}
}

// A subscription account is tested with its connected access token; one that was
// never connected is "unverifiable" without any vendor call.
func TestVendorAccountCheckEndpointSubscription(t *testing.T) {
	probe := &vaProbe{check: vendorauth.CredentialCheck{Status: vendorauth.StatusValid, Detail: "credential accepted (HTTP 200)"}}
	srv, _ := newVendorCheckTestServer(t, probe)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID), vaOwnerSecret, "")
	var got portal.VendorConnectionCheck
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Status != portal.VendorConnectionUnverifiable {
		t.Fatalf("check of an unconnected subscription = %d %s, want 200 unverifiable", rec.Code, rec.Body.String())
	}
	if len(probe.seen) != 0 {
		t.Fatalf("probe saw %v, want no vendor call without a credential", probe.seen)
	}

	if rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret, `{"access_token":"`+vaConnectAccess+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	probe.seen = nil
	rec = vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID), vaOwnerSecret, "")
	got = portal.VendorConnectionCheck{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Status != portal.VendorConnectionValid {
		t.Fatalf("check of a connected subscription = %d %s, want 200 valid", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), vaConnectAccess) {
		t.Fatalf("check response leaks the access token: %s", rec.Body.String())
	}
	if len(probe.seen) != 1 || probe.seen[0] != vaConnectAccess {
		t.Fatalf("probe saw %v, want exactly the connected access token", probe.seen)
	}
}

// The test uses the owner's stored credential, so it is owner-only: a stranger
// (and a system-scope principal) gets the same 404 as an unknown id, and the
// vendor is never called on their behalf.
func TestVendorAccountCheckEndpointIsOwnerOnly(t *testing.T) {
	probe := &vaProbe{check: vendorauth.CredentialCheck{Status: vendorauth.StatusValid, Detail: "credential accepted (HTTP 200)"}}
	srv, _ := newVendorCheckTestServer(t, probe)
	acc := vaCreate(t, srv, vaOwnerSecret, "Key account")

	for _, tc := range []struct{ name, id, secret string }{
		{"stranger", acc.ID, vaOtherSecret},
		{"system scope", acc.ID, vaSystemSecret},
		{"unknown id", "va_missing", vaOwnerSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, http.MethodPost, vaCheckPath(tc.id), tc.secret, "")
			if rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
				t.Fatalf("check = %d %s, want 404 %s", rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
			}
		})
	}
	if len(probe.seen) != 0 {
		t.Fatalf("probe saw %v, want no vendor call for a refused principal", probe.seen)
	}
}

// Only POST is allowed (the action calls the vendor), authentication comes
// first, and a deeper path under /check is the item's own 404.
func TestVendorAccountCheckEndpointRouting(t *testing.T) {
	probe := &vaProbe{check: vendorauth.CredentialCheck{Status: vendorauth.StatusValid}}
	srv, _ := newVendorCheckTestServer(t, probe)
	acc := vaCreate(t, srv, vaOwnerSecret, "Key account")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, srv, method, vaCheckPath(acc.ID), vaOwnerSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s check = %d (Allow %q), want 405 Allow: POST", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID)+"/extra", vaOwnerSecret, ""); rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
		t.Fatalf("POST check/extra = %d %s, want 404 %s", rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
	}
	if rec := vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous check = %d, want 401", rec.Code)
	}
	if len(probe.seen) != 0 {
		t.Fatalf("probe saw %v, want no vendor call from a refused request", probe.seen)
	}
}

// A token import the vendor definitively rejects is a 400 (never a 401: the
// portal reads a 401 from this API as an expired session) with its own stable
// code, echoes no token, and persists nothing.
func TestVendorAccountConnectImportEndpointInvalidCredentialsIs400(t *testing.T) {
	probe := &vaProbe{check: vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "credential rejected (HTTP 401)"}}
	srv, routeStore := newVendorCheckTestServer(t, probe)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret,
		`{"access_token":"`+vaConnectAccess+`","refresh_token":"`+vaConnectRefresh+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "vendor_account.connect_invalid_credentials" {
		t.Fatalf("code = %q, want vendor_account.connect_invalid_credentials", code)
	}
	for _, secret := range []string{vaConnectAccess, vaConnectRefresh} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("error body echoes a token: %s", rec.Body.String())
		}
	}
	if row, err := routeStore.VendorAccountByID(context.Background(), acc.ID); err != nil || row.OAuthTokens != "" {
		t.Fatalf("account after the refused import = %+v, %v, want no stored tokens", row, err)
	}
}

// A stored credential that cannot be OPENED is a state of the account, not a
// server fault and not a bad request: the check answers 409
// vendor_account.credential_unreadable for every way the open fails -- no
// encryption key configured, a blob sealed under another key, a blob that is not
// even base64, a value of no known shape, and (subscription) a token set that
// decrypts to something other than JSON. It must NOT be the write path's 400
// vendor_account.api_key_key_required (that message talks about STORING an api
// key, which is wrong for a read-only check and nonsensical on a subscription),
// nor a 500 check_failed, nor a 401 (the portal reads a 401 as an expired
// session). The body carries no credential, and no vendor is ever called.
func TestVendorAccountCheckEndpointUnreadableCredentialIs409(t *testing.T) {
	const (
		otherKeyHex = "ab"
		wantMessage = "the stored credential could not be read; reconnect the account"
	)
	cipher := newDispatchCipher(t)
	otherCipher, err := capture.New(strings.Repeat(otherKeyHex, 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	seal := func(c *capture.Cipher, plain string) string {
		t.Helper()
		sealed, err := capture.SealSecret(c, false, plain)
		if err != nil {
			t.Fatalf("SealSecret: %v", err)
		}
		return sealed
	}
	sealTokens := func(c *capture.Cipher) string {
		t.Helper()
		sealed, err := vendorauth.SealTokenSet(c, false, vendorauth.TokenSet{AccessToken: vaConnectAccess, RefreshToken: vaConnectRefresh})
		if err != nil {
			t.Fatalf("SealTokenSet: %v", err)
		}
		return sealed
	}

	for _, tc := range []struct {
		name       string
		authType   string
		serverKey  *capture.Cipher // the gateway's cipher; nil = the key is lost / never configured
		stored     string
		credential string // plaintext that must never reach the response
	}{
		{"api key, no encryption key configured", routing.VendorAuthAPIKey, nil, seal(cipher, vaTestAPIKey), vaTestAPIKey},
		{"api key, sealed under another key", routing.VendorAuthAPIKey, cipher, seal(otherCipher, vaTestAPIKey), vaTestAPIKey},
		{"api key, corrupt enc blob", routing.VendorAuthAPIKey, cipher, "enc:!!!not-base64!!!", vaTestAPIKey},
		{"api key, truncated enc blob", routing.VendorAuthAPIKey, cipher, seal(cipher, vaTestAPIKey)[:12], vaTestAPIKey},
		{"api key, value of no known shape", routing.VendorAuthAPIKey, cipher, "garbage-without-a-prefix", vaTestAPIKey},
		{"subscription, no encryption key configured", routing.VendorAuthSubscription, nil, sealTokens(cipher), vaConnectAccess},
		{"subscription, sealed under another key", routing.VendorAuthSubscription, cipher, sealTokens(otherCipher), vaConnectAccess},
		{"subscription, corrupt enc blob", routing.VendorAuthSubscription, cipher, "enc:%%%", vaConnectAccess},
		{"subscription, token set that is not JSON", routing.VendorAuthSubscription, cipher, seal(cipher, "this is not json"), vaConnectAccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &vaProbe{check: vendorauth.CredentialCheck{Status: vendorauth.StatusValid}}
			srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, false, func(deps *portal.ServiceDeps) {
				deps.VendorValidators = probe.validators()
				deps.Cipher = tc.serverKey
			})
			enableVendorAccountsFlag(t, srv)
			now := time.Now().UTC()
			acc := routing.VendorAccount{
				ID: "va_unreadable", OwnerUserID: "usr_va_a", Vendor: routing.VendorAnthropic, AuthType: tc.authType,
				Name: "Unreadable", Status: routing.VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now,
			}
			if tc.authType == routing.VendorAuthAPIKey {
				acc.APIKey = tc.stored
			} else {
				acc.OAuthTokens = tc.stored
			}
			if err := routeStore.CreateVendorAccount(context.Background(), acc); err != nil {
				t.Fatalf("CreateVendorAccount: %v", err)
			}

			rec := vaDo(t, srv, http.MethodPost, vaCheckPath(acc.ID), vaOwnerSecret, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("check status = %d, want 409, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal error body: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != "vendor_account.credential_unreadable" || body.Error.Message != wantMessage {
				t.Fatalf("error = %+v, want vendor_account.credential_unreadable / %q", body.Error, wantMessage)
			}
			for _, leak := range []string{tc.credential, tc.stored, vaConnectRefresh} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("error body leaks %q: %s", leak, rec.Body.String())
				}
			}
			if len(probe.seen) != 0 {
				t.Fatalf("probe saw %v, want no vendor call for an unreadable credential", probe.seen)
			}
		})
	}
}
