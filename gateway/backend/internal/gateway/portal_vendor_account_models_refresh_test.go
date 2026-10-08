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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// vaDiscovery is a controllable model-discovery fetch: it answers the list and
// status set on it for every vendor/auth kind and records each credential it was
// asked with, so a test can assert both what is on the wire and which credential
// reached the vendor seam.
type vaDiscovery struct {
	mu     sync.Mutex
	models []vendorauth.DiscoveredModel
	status vendorauth.DiscoveryStatus
	seen   []string
}

func (d *vaDiscovery) answer(credential string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, credential)
	return append([]vendorauth.DiscoveredModel(nil), d.models...), d.status
}

func (d *vaDiscovery) credentials() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func (d *vaDiscovery) discoverers() portal.VendorModelDiscoverers {
	keyed := func(_ context.Context, _ *http.Client, credential string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		return d.answer(credential)
	}
	return portal.VendorModelDiscoverers{
		OpenAISubscription: func(_ context.Context, _ *http.Client, accessToken, _, _ string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
			return d.answer(accessToken)
		},
		AnthropicSubscription: keyed,
		OpenAIAPIKey:          keyed,
		AnthropicAPIKey:       keyed,
	}
}

// newVendorRefreshTestServer is newVendorAccountTestServer (flag ON, volatile
// RAM-mode sealing) with every model-discovery fetcher replaced by disc.
func newVendorRefreshTestServer(t *testing.T, disc *vaDiscovery) (*Server, *routing.MemoryStore) {
	t.Helper()
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(deps *portal.ServiceDeps) {
		deps.VendorDiscoverers = disc.discoverers()
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore
}

func vaRefreshPath(id string) string {
	return "/api/portal/vendor-accounts/" + id + "/models/refresh"
}

// vaRefreshBody is the decoded POST .../models/refresh response.
type vaRefreshBody struct {
	Account portal.VendorAccountDTO `json:"account"`
	Refresh portal.RefreshResult    `json:"refresh"`
}

func vaDecodeRefresh(t *testing.T, raw []byte) vaRefreshBody {
	t.Helper()
	var body vaRefreshBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal refresh response: %v (%s)", err, raw)
	}
	return body
}

// POST .../{id}/models/refresh asks the vendor which models the stored credential
// can use and answers 200 with the credential-free account (now serving them) and
// the refresh result the portal shows ("N models" / "could not refresh").
func TestVendorAccountModelsRefreshEndpointReturnsTheDiscoveredModelsAndTheResult(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{
		{Slug: "gpt-6-luna", DisplayName: "GPT-6 Luna"},
		{Slug: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol"},
		{Slug: "text-embedding-3-large", DisplayName: "Embedding"},
	}}
	srv, routeStore := newVendorRefreshTestServer(t, disc)
	created := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret,
		`{"vendor":"openai","auth_type":"api_key","name":"Keyed","api_key":"`+vaTestAPIKey+`","model_prefix":"work/"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", created.Code, created.Body.String())
	}
	acc := vaDecode(t, created)

	rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), vaTestAPIKey) {
		t.Fatalf("refresh response leaks the api key: %s", rec.Body.String())
	}

	// The wire contract: exactly account + refresh, the latter exactly
	// status / discovered / detail.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("unmarshal fields: %v (%s)", err, rec.Body.String())
	}
	if len(fields) != 2 || fields["account"] == nil || fields["refresh"] == nil {
		t.Fatalf("response fields = %s, want exactly account and refresh", rec.Body.String())
	}
	var refreshFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["refresh"], &refreshFields); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	if len(refreshFields) != 3 || refreshFields["status"] == nil || refreshFields["discovered"] == nil || refreshFields["detail"] == nil {
		t.Fatalf("refresh = %s, want exactly status, discovered and detail", fields["refresh"])
	}

	body := vaDecodeRefresh(t, rec.Body.Bytes())
	// The OpenAI api-key listing is narrowed to chat models (the embedding one is
	// dropped); the rest is served under the account's prefix.
	if body.Refresh.Status != portal.VendorRefreshOK || body.Refresh.Discovered != 2 || !strings.Contains(body.Refresh.Detail, "2") {
		t.Fatalf("refresh = %+v, want ok / 2 / a detail naming the count", body.Refresh)
	}
	wantModels := []portal.VendorAccountModelDTO{
		{GatewayModel: "work/gpt-6-luna", UpstreamModel: "gpt-6-luna", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6 Luna"},
		{GatewayModel: "work/gpt-6.1-sol", UpstreamModel: "gpt-6.1-sol", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6.1 Sol"},
	}
	if body.Account.ID != acc.ID || body.Account.ModelPrefix != "work/" || !reflect.DeepEqual(body.Account.Models, wantModels) {
		t.Fatalf("account = %+v, want %s serving %+v under the work/ prefix", body.Account, acc.ID, wantModels)
	}
	if got := disc.credentials(); !reflect.DeepEqual(got, []string{vaTestAPIKey}) {
		t.Fatalf("discovery saw %v, want exactly the account's stored api key", got)
	}

	// The rows are really stored (a later GET serves them too).
	rows, err := routeStore.VendorAccountModels(context.Background(), acc.ID)
	if err != nil || len(rows) != 2 || rows[0].GatewayModel != "work/gpt-6-luna" {
		t.Fatalf("stored rows = %+v, %v, want the two discovered models", rows, err)
	}
	if got := vaDecode(t, vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, "")); !reflect.DeepEqual(got.Models, wantModels) {
		t.Fatalf("GET after refresh models = %+v, want %+v", got.Models, wantModels)
	}
}

// A vendor that cannot be asked is NOT an error: 200 with status unverifiable, the
// reason in detail, and the account's models exactly as they were.
func TestVendorAccountModelsRefreshEndpointIsFailSoft(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryUnverifiable}
	srv, _ := newVendorRefreshTestServer(t, disc)
	acc := vaCreate(t, srv, vaOwnerSecret, "Keyed")
	if len(acc.Models) == 0 {
		t.Fatal("the created account has no seeded models")
	}

	rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (fail-soft), body = %s", rec.Code, rec.Body.String())
	}
	body := vaDecodeRefresh(t, rec.Body.Bytes())
	if body.Refresh.Status != portal.VendorRefreshUnverifiable || body.Refresh.Discovered != 0 || body.Refresh.Detail == "" {
		t.Fatalf("refresh = %+v, want unverifiable / 0 / a reason", body.Refresh)
	}
	if !reflect.DeepEqual(body.Account.Models, acc.Models) {
		t.Fatalf("account models = %+v, want the seeded %+v kept", body.Account.Models, acc.Models)
	}
}

// Refreshing sends the owner's stored credential to the vendor, so it is
// owner-only: a stranger (and a system-scope principal) gets the same 404 as an
// unknown id, and the vendor is never asked on their behalf.
func TestVendorAccountModelsRefreshEndpointIsOwnerOnly(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "x"}}}
	srv, _ := newVendorRefreshTestServer(t, disc)
	acc := vaCreate(t, srv, vaOwnerSecret, "Keyed")

	for _, tc := range []struct{ name, id, secret string }{
		{"stranger", acc.ID, vaOtherSecret},
		{"system scope", acc.ID, vaSystemSecret},
		{"unknown id", "va_missing", vaOwnerSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(tc.id), tc.secret, "")
			if rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
				t.Fatalf("refresh = %d %s, want 404 %s", rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
			}
		})
	}
	if got := disc.credentials(); len(got) != 0 {
		t.Fatalf("discovery saw %v, want no vendor call for a refused principal", got)
	}
}

// While the vendor_accounts_enabled master flag is off the endpoint is the module's
// 409 (also in the shared table of TestVendorAccountEndpointsAre409WhileTheMasterFlagIsOff).
func TestVendorAccountModelsRefreshEndpointIs409WhileTheModuleIsDisabled(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "x"}}}
	srv, _ := newVendorRefreshTestServer(t, disc)
	acc := vaCreate(t, srv, vaOwnerSecret, "Keyed")
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable flag = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorModuleDisabledCode {
		t.Fatalf("refresh while disabled = %d %s, want 409 %s", rec.Code, rec.Body.String(), vendorModuleDisabledCode)
	}
	if got := disc.credentials(); len(got) != 0 {
		t.Fatalf("discovery saw %v, want no vendor call while the module is off", got)
	}
}

// A stored credential that cannot be OPENED is the same state the check reports: a
// 409 vendor_account.credential_unreadable with the fixed message, no credential in
// the body, never a 500, and the vendor is not asked.
func TestVendorAccountModelsRefreshEndpointUnreadableCredentialIs409(t *testing.T) {
	const wantMessage = "the stored credential could not be read; reconnect the account"
	cipher := newDispatchCipher(t)
	otherCipher, err := capture.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	sealedElsewhere, err := capture.SealSecret(otherCipher, false, vaTestAPIKey)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}

	for _, tc := range []struct {
		name   string
		stored string
	}{
		{"sealed under another key", sealedElsewhere},
		{"corrupt enc blob", "enc:!!!not-base64!!!"},
		{"value of no known shape", "garbage-without-a-prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "x"}}}
			srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, false, func(deps *portal.ServiceDeps) {
				deps.VendorDiscoverers = disc.discoverers()
				deps.Cipher = cipher
			})
			enableVendorAccountsFlag(t, srv)
			now := time.Now().UTC()
			acc := routing.VendorAccount{
				ID: "va_unreadable", OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
				Name: "Unreadable", Status: routing.VendorAccountStatusActive, APIKey: tc.stored, CreatedAt: now, UpdatedAt: now,
			}
			if err := routeStore.CreateVendorAccount(context.Background(), acc); err != nil {
				t.Fatalf("CreateVendorAccount: %v", err)
			}

			rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("refresh status = %d, want 409, body = %s", rec.Code, rec.Body.String())
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
			for _, leak := range []string{vaTestAPIKey, tc.stored} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("error body leaks %q: %s", leak, rec.Body.String())
				}
			}
			if got := disc.credentials(); len(got) != 0 {
				t.Fatalf("discovery saw %v, want no vendor call for an unreadable credential", got)
			}
		})
	}
}

// Only POST is allowed (the action calls the vendor), authentication comes first, a
// deeper path is the item's own 404 and a body is not needed.
func TestVendorAccountModelsRefreshEndpointRouting(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "x"}}}
	srv, _ := newVendorRefreshTestServer(t, disc)
	acc := vaCreate(t, srv, vaOwnerSecret, "Keyed")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, srv, method, vaRefreshPath(acc.ID), vaOwnerSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s refresh = %d (Allow %q), want 405 Allow: POST", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	for _, path := range []string{vaRefreshPath(acc.ID) + "/extra", "/api/portal/vendor-accounts/" + acc.ID + "/models", "/api/portal/vendor-accounts/" + acc.ID + "/models/other"} {
		if rec := vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, ""); rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
			t.Fatalf("POST %s = %d %s, want 404 %s", path, rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
		}
	}
	if rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous refresh = %d, want 401", rec.Code)
	}
	if got := disc.credentials(); len(got) != 0 {
		t.Fatalf("discovery saw %v, want no vendor call from a refused request", got)
	}
}

// model_prefix travels through the HTTP create and update path into the models the
// account SERVES: set on create it seeds the catalog under the prefix, changed by a
// PATCH it re-labels the stored rows (no vendor call), cleared by "" it serves the
// bare ids again, and a later refresh writes under whatever the prefix is then.
func TestVendorAccountEndpointsThreadTheModelPrefixIntoTheServedModels(t *testing.T) {
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "GPT-6 Luna"}}}
	srv, routeStore := newVendorRefreshTestServer(t, disc)

	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret,
		`{"vendor":"openai","auth_type":"api_key","name":"Prefixed","api_key":"`+vaTestAPIKey+`","model_prefix":"work/"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", rec.Code, rec.Body.String())
	}
	created := vaDecode(t, rec)
	if created.ModelPrefix != "work/" || len(created.Models) == 0 {
		t.Fatalf("created = %+v, want the prefix and the seeded models", created)
	}
	for _, m := range created.Models {
		if m.GatewayModel != "work/"+m.UpstreamModel {
			t.Fatalf("created model %+v, want the seed served as work/<upstream>", m)
		}
	}

	path := "/api/portal/vendor-accounts/" + created.ID
	rec = vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"model_prefix":"home-"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body = %s", rec.Code, rec.Body.String())
	}
	patched := vaDecode(t, rec)
	if patched.ModelPrefix != "home-" || len(patched.Models) != len(created.Models) {
		t.Fatalf("patched = %+v, want the new prefix over the same model set", patched)
	}
	for _, m := range patched.Models {
		if m.GatewayModel != "home-"+m.UpstreamModel {
			t.Fatalf("patched model %+v, want it re-labelled to home-<upstream>", m)
		}
	}
	if got := disc.credentials(); len(got) != 0 {
		t.Fatalf("discovery saw %v, want a prefix change to re-label without asking the vendor", got)
	}

	// A refresh now writes under the CURRENT prefix.
	rec = vaDo(t, srv, http.MethodPost, vaRefreshPath(created.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := vaDecodeRefresh(t, rec.Body.Bytes()); len(body.Account.Models) != 1 || body.Account.Models[0].GatewayModel != "home-gpt-6-luna" {
		t.Fatalf("refresh account = %+v, want the discovered model under home-", body.Account)
	}

	// Clearing the prefix serves the bare ids.
	rec = vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"model_prefix":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cleared := vaDecode(t, rec); cleared.ModelPrefix != "" || len(cleared.Models) != 1 || cleared.Models[0].GatewayModel != "gpt-6-luna" {
		t.Fatalf("cleared = %+v, want the bare discovered id", cleared)
	}
	rows, err := routeStore.VendorAccountModels(context.Background(), created.ID)
	if err != nil || len(rows) != 1 || rows[0].GatewayModel != "gpt-6-luna" || rows[0].UpstreamModel != "gpt-6-luna" {
		t.Fatalf("stored rows = %+v, %v, want the bare id", rows, err)
	}
}
