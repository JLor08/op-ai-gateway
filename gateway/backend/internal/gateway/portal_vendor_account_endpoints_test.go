// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"testing"
	"time"
)

const (
	vaOwnerSecret  = "va-owner-secret"
	vaOtherSecret  = "va-other-secret"
	vaSystemSecret = "va-system-secret"
	vaTestAPIKey   = "sk-live-do-not-echo-123"
)

// newVendorAccountTestServer wires a portal Service over a memory route store
// with two plain-bearer users (usr_va_a / usr_va_b) and the vendor_accounts_enabled
// master flag switched ON (the endpoints answer 409 while it is off). volatile
// selects the RAM-mode seal path ("plain:", no cipher); false models a keyless
// disk store.
func newVendorAccountTestServer(t *testing.T, volatile bool) (*Server, *routing.MemoryStore) {
	t.Helper()
	srv, routeStore, _ := newVendorAccountSettingsTestServer(t, volatile)
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore
}

// enableVendorAccountsFlag turns the master flag on through the real system
// settings endpoint, the way an operator does.
func enableVendorAccountsFlag(t *testing.T, srv *Server) {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable flag status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// newVendorAccountSettingsTestServer is newVendorAccountTestServer with the
// master flag left at its default (OFF), plus a system-scoped bearer
// (vaSystemSecret) to flip it and the settings store it lives in.
func newVendorAccountSettingsTestServer(t *testing.T, volatile bool) (*Server, *routing.MemoryStore, *portal.MemorySystemSettings) {
	t.Helper()
	return newVendorAccountSettingsTestServerWithDeps(t, volatile, nil)
}

// vaNoNetworkValidators answers Unverifiable for every credential probe, so a
// vendor-account test can never reach a vendor over the network (the real probes
// have fixed vendor URLs). A test that needs a verdict overrides the probes it
// cares about through the adjust hook of newVendorAccountSettingsTestServerWithDeps.
func vaNoNetworkValidators() portal.VendorCredentialValidators {
	probe := func(context.Context, *http.Client, string) vendorauth.CredentialCheck {
		return vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, Detail: "fake: no network in tests"}
	}
	return portal.VendorCredentialValidators{OpenAISubscription: probe, AnthropicSubscription: probe, OpenAIAPIKey: probe, AnthropicAPIKey: probe}
}

// vaNoNetworkDiscoverers answers Unverifiable for every model-discovery fetch and
// for the OpenAI subscription usage fetch a refresh runs after it, so a
// vendor-account test (a connect runs a best-effort discovery) can never reach a
// vendor over the network. A test that needs discovered models overrides the
// fetchers it cares about through the adjust hook below.
func vaNoNetworkDiscoverers() portal.VendorModelDiscoverers {
	fetch := func(context.Context, *http.Client, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		return nil, vendorauth.DiscoveryUnverifiable
	}
	return portal.VendorModelDiscoverers{
		OpenAISubscription: func(context.Context, *http.Client, string, string, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
			return nil, vendorauth.DiscoveryUnverifiable
		},
		AnthropicSubscription: fetch,
		OpenAIAPIKey:          fetch,
		AnthropicAPIKey:       fetch,
		OpenAIUsage:           vaNoNetworkUsage,
	}
}

// vaNoNetworkUsage is the usage fetch of an OpenAI subscription that cannot be
// asked: Unverifiable and all-unknown, like the real fetcher's failure answer.
func vaNoNetworkUsage(context.Context, *http.Client, string, string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus) {
	return vendorauth.OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1}, vendorauth.DiscoveryUnverifiable
}

// newVendorAccountSettingsTestServerWithDeps is newVendorAccountSettingsTestServer
// with a hook to adjust the portal.ServiceDeps before the Service is built (the
// connect tests inject httptest vendor OAuth endpoints this way). The credential
// probes and the model-discovery fetchers default to vaNoNetworkValidators /
// vaNoNetworkDiscoverers before the hook runs.
func newVendorAccountSettingsTestServerWithDeps(t *testing.T, volatile bool, adjust func(*portal.ServiceDeps)) (*Server, *routing.MemoryStore, *portal.MemorySystemSettings) {
	t.Helper()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_va_a", Email: "a@example.test", DisplayName: "Owner A", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_va_b", Email: "b@example.test", DisplayName: "Other B", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_va_a", UserID: "usr_va_a", Name: "Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, VendorProviderAccess: store.EncodeVendorAccess(auth.VendorAccess{All: true}), CreatedAt: now, UpdatedAt: now}, vaOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_va_b", UserID: "usr_va_b", Name: "Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, vaOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}
	dir.AddUser(store.User{ID: "usr_va_sys", Email: "sys@example.test", DisplayName: "System", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_va_sys", UserID: "usr_va_sys", Name: "System Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, vaSystemSecret); err != nil {
		t.Fatalf("CreatePlainToken system: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	recorder := usage.NewRecorder()
	settings := portal.NewMemorySystemSettings()
	deps := portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, SystemSettings: settings, SettingsVolatile: volatile, VendorValidators: vaNoNetworkValidators(), VendorDiscoverers: vaNoNetworkDiscoverers()}
	if adjust != nil {
		adjust(&deps)
	}
	svc := portal.NewService(deps)
	srv := New(ServerDeps{Tokens: tokens, Usage: recorder, Routes: routeStore, Portal: svc})
	return srv, routeStore, settings
}

func vaDo(t *testing.T, srv *Server, method, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func vaDecode(t *testing.T, rec *httptest.ResponseRecorder) portal.VendorAccountDTO {
	t.Helper()
	var dto portal.VendorAccountDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal dto: %v (%s)", err, rec.Body.String())
	}
	return dto
}

func vaCreateBody(name string) string {
	return `{"vendor":"openai","auth_type":"api_key","name":"` + name + `","api_key":"` + vaTestAPIKey + `"}`
}

func vaCreate(t *testing.T, srv *Server, secret, name string) portal.VendorAccountDTO {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", secret, vaCreateBody(name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	return vaDecode(t, rec)
}

func TestVendorAccountEndpointsCreateListGet(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)

	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaCreateBody("My OpenAI"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), vaTestAPIKey) {
		t.Fatalf("create response leaks the api key: %s", rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["api_key_set"] != true || raw["subscription_connected"] != false {
		t.Fatalf("api_key_set/subscription_connected = %v/%v, want true/false (%s)", raw["api_key_set"], raw["subscription_connected"], rec.Body.String())
	}
	models, ok := raw["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want the seeded OpenAI catalog", raw["models"])
	}
	for _, m := range models {
		if row, _ := m.(map[string]any); row["api_flavor"] != routing.APIFlavorOpenAI {
			t.Fatalf("model %#v, want api_flavor %q", m, routing.APIFlavorOpenAI)
		}
	}
	created := vaDecode(t, rec)
	if !strings.HasPrefix(created.ID, "va_") || created.Name != "My OpenAI" || created.Status != routing.VendorAccountStatusActive {
		t.Fatalf("created = %#v", created)
	}

	// List: the owner sees it; the other user's list is empty (own accounts only).
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list portal.VendorAccountListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != created.ID || !list.Data[0].APIKeySet {
		t.Fatalf("owner list = %#v, want exactly the created account", list.Data)
	}
	if strings.Contains(rec.Body.String(), vaTestAPIKey) {
		t.Fatalf("list response leaks the api key: %s", rec.Body.String())
	}
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOtherSecret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("other list = %d %s, want 200 with data:[]", rec.Code, rec.Body.String())
	}

	// Get by id as the owner.
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+created.ID, vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := vaDecode(t, rec); got.ID != created.ID {
		t.Fatalf("get = %#v", got)
	}
}

// The detail GET and the list carry the scraped rate-limit snapshot under
// "usage"; a snapshot-less account leaves the key out of both.
func TestVendorAccountEndpointsGetCarriesTheUsageSnapshot(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, true)
	created := vaCreate(t, srv, vaOwnerSecret, "Owner's")
	path := "/api/portal/vendor-accounts/" + created.ID

	usageOf := func(rec *httptest.ResponseRecorder) (map[string]any, bool) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var raw struct {
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return raw.Usage, raw.Usage != nil
	}

	if _, has := usageOf(vaDo(t, srv, http.MethodGet, path, vaOwnerSecret, "")); has {
		t.Fatal("detail GET carried usage before any snapshot exists")
	}

	resetAt := time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC)
	if err := routeStore.UpsertVendorAccountUsage(context.Background(), routing.VendorAccountUsage{
		AccountID:       created.ID,
		FiveHourPct:     64,
		FiveHourResetAt: &resetAt,
		WeeklyPct:       -1,
		UpdatedAt:       resetAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	usage, has := usageOf(vaDo(t, srv, http.MethodGet, path, vaOwnerSecret, ""))
	if !has {
		t.Fatal("detail GET lacks usage although a snapshot exists")
	}
	if usage["five_hour_pct"] != float64(64) || usage["weekly_pct"] != float64(-1) || usage["five_hour_reset_at"] != "2026-10-07T17:00:00Z" {
		t.Fatalf("usage = %v", usage)
	}

	// The list carries the same snapshot for the account that has one.
	rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			ID    string         `json:"id"`
			Usage map[string]any `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != created.ID {
		t.Fatalf("list = %s, want the one account", rec.Body.String())
	}
	if u := list.Data[0].Usage; u["five_hour_pct"] != float64(64) || u["weekly_pct"] != float64(-1) || u["five_hour_reset_at"] != "2026-10-07T17:00:00Z" {
		t.Fatalf("list usage = %v, want the stored snapshot", u)
	}
	// ... and omits the key (omitempty) for one with no snapshot.
	bare := vaCreate(t, srv, vaOwnerSecret, "Owner's second")
	rec = vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts", vaOwnerSecret, "")
	var rows struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	for _, row := range rows.Data {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("id: %v", err)
		}
		if _, has := row["usage"]; has != (id == created.ID) {
			t.Fatalf("list row %s (bare=%v): usage key present = %v: %s", id, id == bare.ID, has, rec.Body.String())
		}
	}
	if rec := vaDo(t, srv, http.MethodGet, path, vaOtherSecret, ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("non-owner detail = %d %s, want the plain 404", rec.Code, rec.Body.String())
	}
}

// A non-owner gets the SAME 404 as for an unknown id on every item verb, so an
// account's existence never leaks across users -- and nothing is changed.
func TestVendorAccountEndpointsCrossUserIs404(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, true)
	created := vaCreate(t, srv, vaOwnerSecret, "Owner's")
	path := "/api/portal/vendor-accounts/" + created.ID

	for _, tc := range []struct {
		method, body string
	}{
		{http.MethodGet, ""},
		{http.MethodPatch, `{"name":"Hijacked"}`},
		{http.MethodDelete, ""},
	} {
		rec := vaDo(t, srv, tc.method, path, vaOtherSecret, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s as non-owner status = %d, want 404, body = %s", tc.method, rec.Code, rec.Body.String())
		}
		if code := perfErrorCode(t, rec.Body.Bytes()); code != portal.CodeVendorAccountNotFound {
			t.Fatalf("%s as non-owner code = %q, want %s", tc.method, code, portal.CodeVendorAccountNotFound)
		}
	}
	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/va_missing", vaOwnerSecret, ""); rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
		t.Fatalf("unknown id = %d %s, want the same 404", rec.Code, rec.Body.String())
	}
	row, err := routeStore.VendorAccountByID(context.Background(), created.ID)
	if err != nil || row.Name != "Owner's" {
		t.Fatalf("row after cross-user attempts = %#v, %v, want untouched", row, err)
	}
}

func TestVendorAccountEndpointsPatchAndDelete(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, true)
	created := vaCreate(t, srv, vaOwnerSecret, "Original")
	path := "/api/portal/vendor-accounts/" + created.ID

	// Rename + disable; the omitted api_key keeps the stored key.
	rec := vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"name":"Renamed","status":"disabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := vaDecode(t, rec); got.Name != "Renamed" || got.Status != routing.VendorAccountStatusDisabled || !got.APIKeySet {
		t.Fatalf("patched = %#v", got)
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), created.ID); row.APIKey != "plain:"+vaTestAPIKey {
		t.Fatalf("stored key after rename = %q, want kept", row.APIKey)
	}

	// An explicit empty api_key clears it.
	rec = vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"api_key":""}`)
	if rec.Code != http.StatusOK || vaDecode(t, rec).APIKeySet {
		t.Fatalf("clear key = %d %s, want 200 with api_key_set=false", rec.Code, rec.Body.String())
	}

	rec = vaDo(t, srv, http.MethodDelete, path, vaOwnerSecret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("delete = %d %s, want 200 {ok:true}", rec.Code, rec.Body.String())
	}
	if rec := vaDo(t, srv, http.MethodGet, path, vaOwnerSecret, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", rec.Code)
	}
	if rec := vaDo(t, srv, http.MethodDelete, path, vaOwnerSecret, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}
}

// model_prefix is accepted on create and patch, returned on every read, and a
// model's display_name is always present on the wire (the dashboard keys off it).
func TestVendorAccountEndpointsModelPrefixAndDisplayName(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, true)

	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret,
		`{"vendor":"openai","auth_type":"api_key","name":"Prefixed","model_prefix":" work/ "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	created := vaDecode(t, rec)
	if created.ModelPrefix != "work/" {
		t.Fatalf("created model_prefix = %q, want the trimmed work/", created.ModelPrefix)
	}
	var raw struct {
		ModelPrefix *string          `json:"model_prefix"`
		Models      []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.ModelPrefix == nil || len(raw.Models) == 0 {
		t.Fatalf("response %s must carry model_prefix and the seeded models", rec.Body.String())
	}
	for _, m := range raw.Models {
		if _, ok := m["display_name"]; !ok {
			t.Fatalf("model %#v lacks display_name", m)
		}
	}

	path := "/api/portal/vendor-accounts/" + created.ID
	rec = vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"model_prefix":"home-"}`)
	if rec.Code != http.StatusOK || vaDecode(t, rec).ModelPrefix != "home-" {
		t.Fatalf("patch = %d %s, want 200 with model_prefix home-", rec.Code, rec.Body.String())
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), created.ID); row.ModelPrefix != "home-" {
		t.Fatalf("stored prefix = %q, want home-", row.ModelPrefix)
	}
	// An unrelated patch keeps it.
	rec = vaDo(t, srv, http.MethodPatch, path, vaOwnerSecret, `{"name":"Renamed"}`)
	if rec.Code != http.StatusOK || vaDecode(t, rec).ModelPrefix != "home-" {
		t.Fatalf("rename = %d %s, want the prefix kept", rec.Code, rec.Body.String())
	}
}

func TestVendorAccountEndpointsErrorMapping(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	created := vaCreate(t, srv, vaOwnerSecret, "Mapped")
	itemPath := "/api/portal/vendor-accounts/" + created.ID

	cases := []struct {
		name         string
		method, path string
		body         string
		status       int
		code         string
	}{
		{"blank name", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"openai","auth_type":"api_key","name":" "}`, http.StatusBadRequest, "vendor_account.name_required"},
		{"bad vendor", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"mistral","auth_type":"api_key","name":"x"}`, http.StatusBadRequest, "vendor_account.vendor_invalid"},
		{"bad auth type", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"openai","auth_type":"password","name":"x"}`, http.StatusBadRequest, "vendor_account.auth_type_invalid"},
		{"bad status", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"openai","auth_type":"api_key","name":"x","status":"paused"}`, http.StatusBadRequest, "vendor_account.status_invalid"},
		{"api key on subscription", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"anthropic","auth_type":"subscription","name":"x","api_key":"sk-x"}`, http.StatusBadRequest, "vendor_account.api_key_not_allowed"},
		{"bad model prefix", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":"openai","auth_type":"api_key","name":"x","model_prefix":"has space"}`, http.StatusBadRequest, "vendor_account.model_prefix_invalid"},
		{"malformed json", http.MethodPost, "/api/portal/vendor-accounts", `{"vendor":`, http.StatusBadRequest, codeRequestInvalidJSON},
		{"patch blank name", http.MethodPatch, itemPath, `{"name":""}`, http.StatusBadRequest, "vendor_account.name_required"},
		{"patch bad status", http.MethodPatch, itemPath, `{"status":"needs_reconnect"}`, http.StatusBadRequest, "vendor_account.status_invalid"},
		{"patch whitespace-only key", http.MethodPatch, itemPath, `{"api_key":"   "}`, http.StatusBadRequest, "vendor_account.api_key_invalid"},
		{"patch bad model prefix", http.MethodPatch, itemPath, `{"model_prefix":"w\u00f6rk"}`, http.StatusBadRequest, "vendor_account.model_prefix_invalid"},
		{"collection rejects PUT", http.MethodPut, "/api/portal/vendor-accounts", `{}`, http.StatusMethodNotAllowed, codeRequestMethodNotAllowed},
		{"item rejects POST", http.MethodPost, itemPath, `{}`, http.StatusMethodNotAllowed, codeRequestMethodNotAllowed},
		{"nested path is not an item", http.MethodGet, itemPath + "/connect", "", http.StatusNotFound, portal.CodeVendorAccountNotFound},
		{"empty id", http.MethodGet, "/api/portal/vendor-accounts/", "", http.StatusNotFound, portal.CodeVendorAccountNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, tc.method, tc.path, vaOwnerSecret, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

func TestVendorAccountEndpointsRequireSession(t *testing.T) {
	srv, _ := newVendorAccountTestServer(t, true)
	for _, path := range []string{"/api/portal/vendor-accounts", "/api/portal/vendor-accounts/va_x"} {
		if rec := vaDo(t, srv, http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without credentials = %d, want 401, body = %s", path, rec.Code, rec.Body.String())
		}
	}
}

// A disk-backed store with no cipher must refuse to persist a plaintext key:
// the operator's own keyless misconfiguration is a 400, not a 500, and no row
// is written.
func TestVendorAccountEndpointsKeylessDiskStoreRefusesAPIKey(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, false)

	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, vaCreateBody("Keyless"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "vendor_account.api_key_key_required" {
		t.Fatalf("code = %q, want vendor_account.api_key_key_required", code)
	}
	if rows, _ := routeStore.VendorAccounts(context.Background()); len(rows) != 0 {
		t.Fatalf("rows = %#v, want none persisted", rows)
	}
	// A key-less account (no secret to seal) is still fine on such a store.
	rec = vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, `{"vendor":"anthropic","auth_type":"subscription","name":"Sub"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscription create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if got := vaDecode(t, rec); got.SubscriptionConnected || got.APIKeySet {
		t.Fatalf("subscription dto = %#v, want unconnected", got)
	}
}

// Authorization precedes body validation: a non-owner sending a SEMANTICALLY
// invalid PATCH gets the 404, never a 400 that would confirm the account exists.
// (A syntactically malformed body is rejected by the JSON decoder before any
// account lookup, for every caller alike, so it reveals nothing.)
func TestVendorAccountEndpointsStrangerWithInvalidPatchGets404(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, true)
	created := vaCreate(t, srv, vaOwnerSecret, "Owner's")
	path := "/api/portal/vendor-accounts/" + created.ID

	for _, body := range []string{`{"name":""}`, `{"status":"paused"}`, `{"status":"needs_reconnect"}`, `{"api_key":"   "}`} {
		rec := vaDo(t, srv, http.MethodPatch, path, vaOtherSecret, body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("stranger PATCH %s status = %d, want 404, body = %s", body, rec.Code, rec.Body.String())
		}
		if code := perfErrorCode(t, rec.Body.Bytes()); code != portal.CodeVendorAccountNotFound {
			t.Fatalf("stranger PATCH %s code = %q, want %s", body, code, portal.CodeVendorAccountNotFound)
		}
	}
	row, err := routeStore.VendorAccountByID(context.Background(), created.ID)
	if err != nil || row.Name != "Owner's" || row.APIKey != "plain:"+vaTestAPIKey {
		t.Fatalf("row after stranger PATCHes = %#v, %v, want untouched", row, err)
	}
}

// The seal also runs on PATCH: on a keyless disk store a replacement key is a
// 400, not a 500, and the account is left as it was.
func TestVendorAccountEndpointsKeylessDiskStoreRefusesReplacementKey(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, false)
	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret, `{"vendor":"openai","auth_type":"api_key","name":"No key yet"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	created := vaDecode(t, rec)

	rec = vaDo(t, srv, http.MethodPatch, "/api/portal/vendor-accounts/"+created.ID, vaOwnerSecret, `{"api_key":"`+vaTestAPIKey+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "vendor_account.api_key_key_required" {
		t.Fatalf("code = %q, want vendor_account.api_key_key_required", code)
	}
	if row, err := routeStore.VendorAccountByID(context.Background(), created.ID); err != nil || row.APIKey != "" {
		t.Fatalf("row after refused PATCH = %#v, %v, want no key stored", row, err)
	}
}
