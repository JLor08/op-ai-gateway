// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	vaUsageAccess  = "tok-usage-access-DO-NOT-ECHO-4b1c"
	vaUsageAccount = "acct-usage-77"
)

// vaUsage is a controllable usage fetch: it answers the usage and status set on it
// and records the access token of every call, so a test can assert both what is on
// the wire and whether (and with which credential) the vendor was asked at all.
type vaUsage struct {
	mu     sync.Mutex
	usage  vendorauth.OpenAISubscriptionUsage
	status vendorauth.DiscoveryStatus
	seen   []string
	// modelCalls counts the model-discovery fetches the usage refresh must never make.
	modelCalls int
}

func (u *vaUsage) fetch(_ context.Context, _ *http.Client, accessToken, _ string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen = append(u.seen, accessToken)
	return u.usage, u.status
}

func (u *vaUsage) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func (u *vaUsage) models() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.modelCalls
}

func (u *vaUsage) discoverers() portal.VendorModelDiscoverers {
	countModels := func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.modelCalls++
	}
	keyed := func(context.Context, *http.Client, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		countModels()
		return nil, vendorauth.DiscoveryUnverifiable
	}
	return portal.VendorModelDiscoverers{
		OpenAISubscription: func(context.Context, *http.Client, string, string, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
			countModels()
			return nil, vendorauth.DiscoveryUnverifiable
		},
		AnthropicSubscription: keyed,
		OpenAIAPIKey:          keyed,
		AnthropicAPIKey:       keyed,
		OpenAIUsage:           u.fetch,
	}
}

// vaUsageFailStore makes the usage snapshot read fail once armed (the refresh's
// store-failure path), and is transparent until then.
type vaUsageFailStore struct {
	routing.Store
	armed *atomic.Bool
}

func (f vaUsageFailStore) VendorAccountUsageByID(ctx context.Context, id string) (routing.VendorAccountUsage, bool, error) {
	if f.armed.Load() {
		return routing.VendorAccountUsage{}, false, errors.New("usage table unavailable")
	}
	return f.Store.VendorAccountUsageByID(ctx, id)
}

// newVendorUsageRefreshTestServer is the vendor-account test server (flag ON,
// volatile "plain:" sealing) whose discovery fetchers, usage fetch included, are
// replaced by usage; the returned flag makes the usage snapshot read fail.
func newVendorUsageRefreshTestServer(t *testing.T, usage *vaUsage) (*Server, *routing.MemoryStore, *atomic.Bool) {
	t.Helper()
	armed := &atomic.Bool{}
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(deps *portal.ServiceDeps) {
		deps.VendorDiscoverers = usage.discoverers()
		deps.Routes = vaUsageFailStore{Store: deps.Routes, armed: armed}
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore, armed
}

// vaCreateOpenAISubscription stores a connected OpenAI subscription account of the
// owner (usr_va_a) directly, with the token set sealed the RAM-mode way.
func vaCreateOpenAISubscription(t *testing.T, routeStore *routing.MemoryStore, id string) routing.VendorAccount {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(nil, true, vendorauth.TokenSet{AccessToken: vaUsageAccess, AccountID: vaUsageAccount})
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	now := time.Now().UTC()
	acc := routing.VendorAccount{
		ID: id, OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
		Name: "Subscription", Status: routing.VendorAccountStatusActive, OAuthTokens: sealed, CreatedAt: now, UpdatedAt: now,
	}
	if err := routeStore.CreateVendorAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	return acc
}

func vaUsagePath(id string) string {
	return "/api/portal/vendor-accounts/" + id + "/usage/refresh"
}

// vaUsageBody is the decoded POST .../usage/refresh response.
type vaUsageBody struct {
	Usage   *portal.VendorAccountUsageDTO   `json:"usage"`
	Refresh portal.VendorUsageRefreshResult `json:"refresh"`
}

func vaDecodeUsage(t *testing.T, raw []byte) vaUsageBody {
	t.Helper()
	var body vaUsageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal usage refresh response: %v (%s)", err, raw)
	}
	return body
}

func vaOKUsage() vendorauth.OpenAISubscriptionUsage {
	return vendorauth.OpenAISubscriptionUsage{FiveHourPct: 23, WeeklyPct: 61, CreditBalance: "12.34", SpendUsedPct: -1}
}

// POST .../{id}/usage/refresh pulls the usage on its own (never the model discovery)
// and answers 200 with exactly {usage, refresh:{status, detail}}; a second plain
// call inside the lazy TTL is "fresh" and does not ask the vendor, while ?force=1
// (the button) always does.
func TestVendorAccountUsageRefreshEndpointPullsUsageAndHonoursTheLazyTTL(t *testing.T) {
	usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
	srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")

	rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage refresh status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{vaUsageAccess, vaUsageAccount} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("usage refresh response leaks %q: %s", secret, rec.Body.String())
		}
	}

	// The wire contract: exactly usage + refresh, the latter exactly status / detail.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("unmarshal fields: %v (%s)", err, rec.Body.String())
	}
	if len(fields) != 2 || fields["usage"] == nil || fields["refresh"] == nil {
		t.Fatalf("response fields = %s, want exactly usage and refresh", rec.Body.String())
	}
	var refreshFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["refresh"], &refreshFields); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	if len(refreshFields) != 2 || refreshFields["status"] == nil || refreshFields["detail"] == nil {
		t.Fatalf("refresh = %s, want exactly status and detail", fields["refresh"])
	}

	body := vaDecodeUsage(t, rec.Body.Bytes())
	if body.Refresh.Status != portal.VendorUsageRefreshOK || body.Refresh.Detail == "" {
		t.Fatalf("refresh = %+v, want ok with a detail", body.Refresh)
	}
	if body.Usage == nil || body.Usage.FiveHourPct != 23 || body.Usage.WeeklyPct != 61 || body.Usage.CreditBalance != "12.34" {
		t.Fatalf("usage = %+v, want the pulled snapshot", body.Usage)
	}
	if got := usage.calls(); !reflect.DeepEqual(got, []string{vaUsageAccess}) {
		t.Fatalf("usage fetch saw %v, want exactly the account's stored access token", got)
	}
	if n := usage.models(); n != 0 {
		t.Fatalf("the model discovery ran %d times, want never for a usage-only refresh", n)
	}
	stored, found, err := routeStore.VendorAccountUsageByID(context.Background(), acc.ID)
	if err != nil || !found || stored.FiveHourPct != 23 {
		t.Fatalf("stored usage = %+v (found %v, err %v), want the pulled snapshot", stored, found, err)
	}
	// The detail GET serves the very snapshot the refresh answered.
	if got := vaDecode(t, vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, "")); got.Usage == nil || got.Usage.FiveHourPct != 23 {
		t.Fatalf("GET after refresh usage = %+v, want the pulled snapshot", got.Usage)
	}

	// A plain second call is the lazy path: inside the TTL it is fresh and silent.
	rec = vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("lazy status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := vaDecodeUsage(t, rec.Body.Bytes()); body.Refresh.Status != portal.VendorUsageRefreshFresh || body.Usage == nil || body.Usage.FiveHourPct != 23 {
		t.Fatalf("lazy = %+v, want fresh with the stored snapshot", body)
	}
	if n := len(usage.calls()); n != 1 {
		t.Fatalf("usage fetch calls = %d after the lazy call, want still 1", n)
	}

	// ?force=1 is the manual button: it bypasses the TTL.
	for _, query := range []string{"?force=1", "?force=true"} {
		rec = vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID)+query, vaOwnerSecret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("forced status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if body := vaDecodeUsage(t, rec.Body.Bytes()); body.Refresh.Status != portal.VendorUsageRefreshOK {
			t.Fatalf("%s = %+v, want ok (a forced refresh asks the vendor)", query, body.Refresh)
		}
	}
	if n := len(usage.calls()); n != 3 {
		t.Fatalf("usage fetch calls = %d after two forced calls, want 3", n)
	}

	// Any other force value is the lazy default (the vendor is protected by default).
	for _, query := range []string{"?force=0", "?force=false", "?force=", "?force=maybe", "?other=1"} {
		rec = vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID)+query, vaOwnerSecret, "")
		if body := vaDecodeUsage(t, rec.Body.Bytes()); rec.Code != http.StatusOK || body.Refresh.Status != portal.VendorUsageRefreshFresh {
			t.Fatalf("%s = %d %+v, want 200 fresh", query, rec.Code, body.Refresh)
		}
	}
	if n := len(usage.calls()); n != 3 {
		t.Fatalf("usage fetch calls = %d after the lazy variants, want still 3", n)
	}
}

// A vendor that cannot be asked is NOT an error: 200 with status unverifiable, the
// stored snapshot answered untouched.
func TestVendorAccountUsageRefreshEndpointIsFailSoft(t *testing.T) {
	usage := &vaUsage{usage: vendorauth.OpenAISubscriptionUsage{FiveHourPct: 99, WeeklyPct: 99, SpendUsedPct: -1}, status: vendorauth.DiscoveryUnverifiable}
	srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")
	now := time.Now().UTC()
	if err := routeStore.UpsertVendorAccountUsage(context.Background(), routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: 40, WeeklyPct: 70, SpendUsedPct: -1, UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID)+"?force=1", vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage refresh status = %d, want 200 (fail-soft), body = %s", rec.Code, rec.Body.String())
	}
	body := vaDecodeUsage(t, rec.Body.Bytes())
	if body.Refresh.Status != portal.VendorUsageRefreshUnverifiable || body.Refresh.Detail == "" {
		t.Fatalf("refresh = %+v, want unverifiable with a detail", body.Refresh)
	}
	if body.Usage == nil || body.Usage.FiveHourPct != 40 || body.Usage.WeeklyPct != 70 {
		t.Fatalf("usage = %+v, want the stored snapshot kept", body.Usage)
	}
}

// An account with no active pull (here an api-key account) is a 200 "unsupported"
// with no usage, and the vendor is never asked.
func TestVendorAccountUsageRefreshEndpointIsUnsupportedForANonSubscriptionAccount(t *testing.T) {
	usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
	srv, _, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreate(t, srv, vaOwnerSecret, "Keyed")

	rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if string(fields["usage"]) != "null" {
		t.Fatalf("usage = %s, want an explicit null (the key is always on the wire)", fields["usage"])
	}
	if body := vaDecodeUsage(t, rec.Body.Bytes()); body.Refresh.Status != portal.VendorUsageRefreshUnsupported || body.Refresh.Detail == "" {
		t.Fatalf("refresh = %+v, want unsupported with a detail", body.Refresh)
	}
	if got := usage.calls(); len(got) != 0 {
		t.Fatalf("usage fetch saw %v, want no vendor call for an api-key account", got)
	}
	if strings.Contains(rec.Body.String(), vaTestAPIKey) {
		t.Fatalf("response leaks the api key: %s", rec.Body.String())
	}
}

// Refreshing sends the owner's stored credential to the vendor, so it is owner-only:
// a stranger (and a system-scope principal) gets the same 404 as an unknown id, and
// the vendor is never asked on their behalf.
func TestVendorAccountUsageRefreshEndpointIsOwnerOnly(t *testing.T) {
	usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
	srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")

	for _, tc := range []struct{ name, id, secret string }{
		{"stranger", acc.ID, vaOtherSecret},
		{"system scope", acc.ID, vaSystemSecret},
		{"unknown id", "va_missing", vaOwnerSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, http.MethodPost, vaUsagePath(tc.id)+"?force=1", tc.secret, "")
			if rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
				t.Fatalf("usage refresh = %d %s, want 404 %s", rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
			}
		})
	}
	if got := usage.calls(); len(got) != 0 {
		t.Fatalf("usage fetch saw %v, want no vendor call for a refused principal", got)
	}
}

// While the vendor_accounts_enabled master flag is off the endpoint is the module's
// 409 (also in the shared table of TestVendorAccountEndpointsAre409WhileTheMasterFlagIsOff).
func TestVendorAccountUsageRefreshEndpointIs409WhileTheModuleIsDisabled(t *testing.T) {
	usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
	srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")
	if rec := vaDo(t, srv, http.MethodPut, "/api/system/settings", vaSystemSecret, `{"vendor_accounts_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable flag = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != vendorModuleDisabledCode {
		t.Fatalf("usage refresh while disabled = %d %s, want 409 %s", rec.Code, rec.Body.String(), vendorModuleDisabledCode)
	}
	if got := usage.calls(); len(got) != 0 {
		t.Fatalf("usage fetch saw %v, want no vendor call while the module is off", got)
	}
}

// A stored credential that cannot be OPENED is the 409 vendor_account.credential_unreadable
// (no credential in the body, never a 500, no vendor call); a store failure outside
// the known states is the 500 vendor_account.usage_refresh_failed.
func TestVendorAccountUsageRefreshEndpointErrorMapping(t *testing.T) {
	t.Run("unreadable credential is 409", func(t *testing.T) {
		usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
		srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
		acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")
		acc.OAuthTokens = "garbage-without-a-prefix"
		if err := routeStore.UpdateVendorAccount(context.Background(), acc); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}

		rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
		if rec.Code != http.StatusConflict || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.credential_unreadable" {
			t.Fatalf("usage refresh = %d %s, want 409 vendor_account.credential_unreadable", rec.Code, rec.Body.String())
		}
		for _, leak := range []string{vaUsageAccess, acc.OAuthTokens} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatalf("error body leaks %q: %s", leak, rec.Body.String())
			}
		}
		if got := usage.calls(); len(got) != 0 {
			t.Fatalf("usage fetch saw %v, want no vendor call for an unreadable credential", got)
		}
	})
	t.Run("store failure is 500 usage_refresh_failed", func(t *testing.T) {
		usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
		srv, routeStore, armed := newVendorUsageRefreshTestServer(t, usage)
		acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")
		armed.Store(true)

		rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), vaOwnerSecret, "")
		if rec.Code != http.StatusInternalServerError || perfErrorCode(t, rec.Body.Bytes()) != codeVendorAccountUsageRefreshFailed {
			t.Fatalf("usage refresh = %d %s, want 500 %s", rec.Code, rec.Body.String(), codeVendorAccountUsageRefreshFailed)
		}
		if codeVendorAccountUsageRefreshFailed != "vendor_account.usage_refresh_failed" {
			t.Fatalf("code = %q, want the stable vendor_account.usage_refresh_failed", codeVendorAccountUsageRefreshFailed)
		}
		if strings.Contains(rec.Body.String(), "usage table unavailable") {
			t.Fatalf("error body echoes the store error: %s", rec.Body.String())
		}
	})
}

// Only POST is allowed (the action calls the vendor), authentication comes first, a
// deeper path or a sibling is the item's own 404 and a body is not needed.
func TestVendorAccountUsageRefreshEndpointRouting(t *testing.T) {
	usage := &vaUsage{usage: vaOKUsage(), status: vendorauth.DiscoveryOK}
	srv, routeStore, _ := newVendorUsageRefreshTestServer(t, usage)
	acc := vaCreateOpenAISubscription(t, routeStore, "va_usage_sub")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := vaDo(t, srv, method, vaUsagePath(acc.ID), vaOwnerSecret, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s usage refresh = %d (Allow %q), want 405 Allow: POST", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	for _, path := range []string{vaUsagePath(acc.ID) + "/extra", "/api/portal/vendor-accounts/" + acc.ID + "/usage", "/api/portal/vendor-accounts/" + acc.ID + "/usage/other"} {
		if rec := vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, ""); rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
			t.Fatalf("POST %s = %d %s, want 404 %s", path, rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
		}
	}
	if rec := vaDo(t, srv, http.MethodPost, vaUsagePath(acc.ID), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous usage refresh = %d, want 401", rec.Code)
	}
	if got := usage.calls(); len(got) != 0 {
		t.Fatalf("usage fetch saw %v, want no vendor call from a refused request", got)
	}
}
