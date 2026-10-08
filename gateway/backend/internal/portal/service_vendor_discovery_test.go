// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	discoveryTestAccess  = "tok-access-DO-NOT-ECHO-9f3a"
	discoveryTestAccount = "acct-disc-42"
)

var discoveryTestNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// --- the fake discovery seam -------------------------------------------------

// fakeDiscoveryCall is one recorded discoverer invocation.
type fakeDiscoveryCall struct {
	kind          string
	credential    string
	accountID     string
	clientVersion string
	// timeout is the Timeout of the *http.Client the service handed over;
	// hasClient is false when it handed over nil.
	timeout   time.Duration
	hasClient bool
}

type fakeDiscoveryResult struct {
	models []vendorauth.DiscoveredModel
	status vendorauth.DiscoveryStatus
}

// fakeVendorDiscoverers replaces the four vendorauth discovery fetchers so no
// service test reaches a vendor over the network. Each kind answers its
// configured result (Unverifiable until set) and every call is recorded.
type fakeVendorDiscoverers struct {
	mu      sync.Mutex
	results map[string]fakeDiscoveryResult
	calls   []fakeDiscoveryCall
}

func newFakeVendorDiscoverers() *fakeVendorDiscoverers {
	f := &fakeVendorDiscoverers{results: map[string]fakeDiscoveryResult{}}
	for _, kind := range []string{kindOpenAISubscription, kindAnthropicSubscription, kindOpenAIAPIKey, kindAnthropicAPIKey} {
		f.results[kind] = fakeDiscoveryResult{status: vendorauth.DiscoveryUnverifiable}
	}
	return f
}

// installFakeVendorDiscoverers wires a fresh fake into svc and returns it.
func installFakeVendorDiscoverers(svc *Service) *fakeVendorDiscoverers {
	f := newFakeVendorDiscoverers()
	svc.vendorDiscovery.discoverers = f.discoverers()
	return f
}

// ok makes kind answer a usable list.
func (f *fakeVendorDiscoverers) ok(kind string, models ...vendorauth.DiscoveredModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[kind] = fakeDiscoveryResult{models: models, status: vendorauth.DiscoveryOK}
}

// okSlugs is ok with the display name left to be the slug.
func (f *fakeVendorDiscoverers) okSlugs(kind string, slugs ...string) {
	models := make([]vendorauth.DiscoveredModel, 0, len(slugs))
	for _, slug := range slugs {
		models = append(models, vendorauth.DiscoveredModel{Slug: slug, DisplayName: slug})
	}
	f.ok(kind, models...)
}

func (f *fakeVendorDiscoverers) record(kind, credential, accountID, clientVersion string, client *http.Client) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := fakeDiscoveryCall{kind: kind, credential: credential, accountID: accountID, clientVersion: clientVersion, hasClient: client != nil}
	if client != nil {
		call.timeout = client.Timeout
	}
	f.calls = append(f.calls, call)
	r := f.results[kind]
	return append([]vendorauth.DiscoveredModel(nil), r.models...), r.status
}

func (f *fakeVendorDiscoverers) keyed(kind string) VendorCredentialDiscoverer {
	return func(_ context.Context, client *http.Client, credential string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		return f.record(kind, credential, "", "", client)
	}
}

func (f *fakeVendorDiscoverers) discoverers() VendorModelDiscoverers {
	return VendorModelDiscoverers{
		OpenAISubscription: func(_ context.Context, client *http.Client, accessToken, accountID, clientVersion string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
			return f.record(kindOpenAISubscription, accessToken, accountID, clientVersion, client)
		},
		AnthropicSubscription: f.keyed(kindAnthropicSubscription),
		OpenAIAPIKey:          f.keyed(kindOpenAIAPIKey),
		AnthropicAPIKey:       f.keyed(kindAnthropicAPIKey),
	}
}

func (f *fakeVendorDiscoverers) recorded() []fakeDiscoveryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeDiscoveryCall(nil), f.calls...)
}

// onlyCall fails the test unless exactly one discoverer call was made, and returns it.
func (f *fakeVendorDiscoverers) onlyCall(t *testing.T) fakeDiscoveryCall {
	t.Helper()
	calls := f.recorded()
	if len(calls) != 1 {
		t.Fatalf("discoverer calls = %+v, want exactly one", calls)
	}
	return calls[0]
}

func (f *fakeVendorDiscoverers) requireNoCalls(t *testing.T, why string) {
	t.Helper()
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("discoverer calls = %+v, want none %s", calls, why)
	}
}

// --- fixtures --------------------------------------------------------------

func discovered(slug, displayName string) vendorauth.DiscoveredModel {
	return vendorauth.DiscoveredModel{Slug: slug, DisplayName: displayName}
}

// newDiscoveryTestService is the vendor-account test service (flag ON, volatile
// "plain:" sealing, fake validators) with a fresh fake discovery seam installed.
func newDiscoveryTestService(t *testing.T) (*Service, *routing.MemoryStore, *fakeVendorDiscoverers) {
	t.Helper()
	svc, routeStore := newVendorAccountTestService(t, discoveryTestNow)
	return svc, routeStore, installFakeVendorDiscoverers(svc)
}

// setDiscoveryTokens seals ts into the subscription account id, bypassing the
// connect flow (which itself discovers).
func setDiscoveryTokens(t *testing.T, svc *Service, routeStore *routing.MemoryStore, id string, ts vendorauth.TokenSet) {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(svc.cipher, svc.settingsVolatile, ts)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	row, err := routeStore.VendorAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	row.OAuthTokens = sealed
	if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}
}

// connectedSubscription creates a connected subscription account of vendor with
// prefix and a token set whose expiry is unknown (so it never needs a refresh).
func connectedSubscription(t *testing.T, svc *Service, routeStore *routing.MemoryStore, vendor, prefix string) VendorAccountDTO {
	t.Helper()
	acc := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: vendor, AuthType: routing.VendorAuthSubscription, Name: "Sub", ModelPrefix: prefix,
	})
	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{AccessToken: discoveryTestAccess, AccountID: discoveryTestAccount})
	return acc
}

func apiKeyAccount(t *testing.T, svc *Service, vendor, prefix string) VendorAccountDTO {
	t.Helper()
	return createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: vendor, AuthType: routing.VendorAuthAPIKey, Name: "Key", APIKey: vendorAccountTestKey, ModelPrefix: prefix,
	})
}

func storedModels(t *testing.T, routeStore *routing.MemoryStore, id string) []routing.VendorAccountModel {
	t.Helper()
	rows, err := routeStore.VendorAccountModels(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountModels: %v", err)
	}
	return rows
}

func modelRow(id, gateway, upstream, flavor, display string) routing.VendorAccountModel {
	return routing.VendorAccountModel{AccountID: id, GatewayModel: gateway, UpstreamModel: upstream, APIFlavor: flavor, DisplayName: display}
}

func gatewayModels(rows []routing.VendorAccountModel) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GatewayModel)
	}
	return out
}

func requireNoToken(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	for _, secret := range []string{discoveryTestAccess, vendorAccountTestKey} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaks a credential: %s", what, raw)
		}
	}
}

// --- RefreshVendorAccountModels: the replace path ----------------------------

// The operator's unblock: a connected ChatGPT-subscription account gets the real
// models the vendor reports instead of the static seed, each under prefix + slug
// with the slug as the upstream id and the vendor's display name.
func TestRefreshVendorAccountModelsReplacesTheSeedWithThePrefixedDiscovery(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.ok(kindOpenAISubscription,
		discovered("gpt-6-luna", "GPT-6 Luna"),
		discovered("gpt-5.6-pro", "GPT-5.6 Pro"),
		discovered("gpt-6.1-sol", "GPT-6.1 Sol"),
	)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "chatgpt/")
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"chatgpt/gpt-5", "chatgpt/gpt-5-mini"}) {
		t.Fatalf("seed rows = %v, want the static OpenAI subscription catalog under the prefix", got)
	}

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshOK || res.Discovered != 3 || res.Detail == "" {
		t.Fatalf("result = %+v, want ok / 3 / a detail", res)
	}

	want := []routing.VendorAccountModel{
		modelRow(acc.ID, "chatgpt/gpt-5.6-pro", "gpt-5.6-pro", routing.APIFlavorOpenAI, "GPT-5.6 Pro"),
		modelRow(acc.ID, "chatgpt/gpt-6-luna", "gpt-6-luna", routing.APIFlavorOpenAI, "GPT-6 Luna"),
		modelRow(acc.ID, "chatgpt/gpt-6.1-sol", "gpt-6.1-sol", routing.APIFlavorOpenAI, "GPT-6.1 Sol"),
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored rows = %+v, want %+v (the seeded gpt-5 / gpt-5-mini replaced)", got, want)
	}
	wantDTO := []VendorAccountModelDTO{
		{GatewayModel: "chatgpt/gpt-5.6-pro", UpstreamModel: "gpt-5.6-pro", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-5.6 Pro"},
		{GatewayModel: "chatgpt/gpt-6-luna", UpstreamModel: "gpt-6-luna", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6 Luna"},
		{GatewayModel: "chatgpt/gpt-6.1-sol", UpstreamModel: "gpt-6.1-sol", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6.1 Sol"},
	}
	if !reflect.DeepEqual(dto.Models, wantDTO) {
		t.Fatalf("dto models = %+v, want %+v", dto.Models, wantDTO)
	}
	if dto.ID != acc.ID || dto.ModelPrefix != "chatgpt/" {
		t.Fatalf("dto = %+v, want the same account with its prefix", dto)
	}

	// The subscription fetch got the opened access token, the stored ChatGPT
	// account id, the configured client_version and a 10s client.
	call := fake.onlyCall(t)
	if call.kind != kindOpenAISubscription || call.credential != discoveryTestAccess || call.accountID != discoveryTestAccount {
		t.Fatalf("call = %+v, want the OpenAI subscription fetcher with the access token and account id", call)
	}
	if call.clientVersion != DefaultVendorOpenAICodexClientVersion {
		t.Fatalf("client_version = %q, want the default %q", call.clientVersion, DefaultVendorOpenAICodexClientVersion)
	}
	if !call.hasClient || call.timeout != 10*time.Second {
		t.Fatalf("call = %+v, want a client with a 10s timeout", call)
	}
	requireNoToken(t, "result", res)
	requireNoToken(t, "dto", dto)
}

func TestRefreshVendorAccountModelsWithoutAPrefixServesTheBareSlug(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"))
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	want := []routing.VendorAccountModel{modelRow(acc.ID, "gpt-6-luna", "gpt-6-luna", routing.APIFlavorOpenAI, "GPT-6 Luna")}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored rows = %+v, want %+v", got, want)
	}
}

// The configured Codex client_version (a system setting) is what the subscription
// fetch sends, read at call time.
func TestRefreshVendorAccountModelsSendsTheConfiguredClientVersion(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	if err := svc.settings.SetSystemSetting(context.Background(), vendorOpenAICodexClientVersionKey, "27.101.40000", time.Time{}); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if call := fake.onlyCall(t); call.clientVersion != "27.101.40000" {
		t.Fatalf("client_version = %q, want the configured 27.101.40000", call.clientVersion)
	}
}

// A token set without a stored account id falls back to the claim baked into the
// access-token JWT, as the dispatch does.
func TestRefreshVendorAccountModelsFallsBackToTheJWTAccountID(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Sub")
	jwt := connectTestJWT(t, "acct-from-jwt", "plus")
	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{AccessToken: jwt})

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if call := fake.onlyCall(t); call.accountID != "acct-from-jwt" || call.credential != jwt {
		t.Fatalf("call = %+v, want the JWT's account id and the access token", call)
	}
}

// The right fetcher is picked per vendor + auth type, with the credential that
// kind needs, and each row carries the vendor's API flavor.
func TestRefreshVendorAccountModelsPicksTheFetcherAndFlavorPerAccountKind(t *testing.T) {
	cases := []struct {
		vendor, authType string
		kind             string
		flavor           string
		credential       string
	}{
		{routing.VendorOpenAI, routing.VendorAuthSubscription, kindOpenAISubscription, routing.APIFlavorOpenAI, discoveryTestAccess},
		{routing.VendorOpenAI, routing.VendorAuthAPIKey, kindOpenAIAPIKey, routing.APIFlavorOpenAI, vendorAccountTestKey},
		{routing.VendorAnthropic, routing.VendorAuthSubscription, kindAnthropicSubscription, routing.APIFlavorAnthropic, discoveryTestAccess},
		{routing.VendorAnthropic, routing.VendorAuthAPIKey, kindAnthropicAPIKey, routing.APIFlavorAnthropic, vendorAccountTestKey},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			slug := "gpt-5.1"
			if tc.vendor == routing.VendorAnthropic {
				slug = "claude-opus-5"
			}
			fake.ok(tc.kind, discovered(slug, "Display"))
			var acc VendorAccountDTO
			if tc.authType == routing.VendorAuthSubscription {
				acc = connectedSubscription(t, svc, routeStore, tc.vendor, "p-")
			} else {
				acc = apiKeyAccount(t, svc, tc.vendor, "p-")
			}

			_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
			if err != nil {
				t.Fatalf("RefreshVendorAccountModels: %v", err)
			}
			if res.Status != VendorRefreshOK || res.Discovered != 1 {
				t.Fatalf("result = %+v, want ok / 1", res)
			}
			call := fake.onlyCall(t)
			if call.kind != tc.kind || call.credential != tc.credential {
				t.Fatalf("call = %+v, want kind %s with credential %q", call, tc.kind, tc.credential)
			}
			if !call.hasClient || call.timeout != 10*time.Second {
				t.Fatalf("call = %+v, want a client with a 10s timeout", call)
			}
			want := []routing.VendorAccountModel{modelRow(acc.ID, "p-"+slug, slug, tc.flavor, "Display")}
			if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, want) {
				t.Fatalf("stored rows = %+v, want %+v", got, want)
			}
		})
	}
}

// --- validation and caps ------------------------------------------------------

// A slug becomes a model id (it is in URLs, JSON bodies and headers) and a
// display name ends up in the UI: both are vendor input, so each is validated and
// capped. A bad slug drops its row; a bad display name only loses the name.
func TestRefreshVendorAccountModelsValidatesAndCapsWhatTheVendorSent(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	longName := strings.Repeat("N", 300)
	fake.ok(kindOpenAISubscription,
		discovered("ok-model", "OK Model"),
		discovered("bad slug", "has a space"),
		discovered("line\nbreak", "newline in slug"),
		discovered("tab\tslug", "tab in slug"),
		discovered("", "empty slug"),
		discovered(strings.Repeat("m", 129), "slug over the cap"),
		discovered("gpt-é", "non-ASCII slug"),
		discovered("q?x", "query char"),
		discovered("hash#x", "fragment char"),
		discovered("pct%2fx", "percent char"),
		discovered("quote\"x", "quote char"),
		discovered("../etc", "dot-dot lead"),
		discovered("a/../b", "dot-dot inside"),
		discovered("/abs", "slash lead"),
		discovered("-lead", "dash lead"),
		discovered("ok-model", "Duplicate of the first"),
		discovered("long-name", longName),
		discovered("ctrl-name", "Bad\x00Name"),
		discovered("newline-name", "Two\nLines"),
		discovered("uni-name", "Claude – Preview"),
		discovered("padded-name", "  Padded  "),
		discovered(strings.Repeat("s", 128), "slug at the cap"),
	)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "w/")

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	wantSlugs := map[string]string{ // slug -> expected display name
		"ok-model":               "OK Model",
		"long-name":              strings.Repeat("N", maxDiscoveredDisplayNameLen),
		"ctrl-name":              "",
		"newline-name":           "",
		"uni-name":               "",
		"padded-name":            "Padded",
		strings.Repeat("s", 128): "slug at the cap",
	}
	if res.Status != VendorRefreshOK || res.Discovered != len(wantSlugs) {
		t.Fatalf("result = %+v, want ok with %d kept rows", res, len(wantSlugs))
	}
	// 14 slugs fail (incl. the over-cap one) and 1 duplicate is folded: 15 dropped.
	if !strings.Contains(res.Detail, "15 unusable") {
		t.Fatalf("detail = %q, want it to say how many entries were dropped (15)", res.Detail)
	}
	if len(dto.Models) != len(wantSlugs) {
		t.Fatalf("models = %+v, want %d rows", dto.Models, len(wantSlugs))
	}
	for _, m := range dto.Models {
		wantName, ok := wantSlugs[m.UpstreamModel]
		if !ok {
			t.Fatalf("row %+v kept a slug that must have been dropped", m)
		}
		if m.GatewayModel != "w/"+m.UpstreamModel || m.DisplayName != wantName {
			t.Fatalf("row %+v, want gateway %q and display name %q", m, "w/"+m.UpstreamModel, wantName)
		}
		if len(m.UpstreamModel) > maxDiscoveredModelIDLen || len(m.DisplayName) > maxDiscoveredDisplayNameLen {
			t.Fatalf("row %+v exceeds the caps", m)
		}
	}
	// The first of two equal slugs wins.
	for _, m := range dto.Models {
		if m.UpstreamModel == "ok-model" && m.DisplayName != "OK Model" {
			t.Fatalf("duplicate slug kept the later display name: %+v", m)
		}
	}
}

// A vendor listing is untrusted in COUNT too: only the first maxDiscoveredModels
// usable entries are stored.
func TestRefreshVendorAccountModelsCapsTheNumberOfModels(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	slugs := make([]string, 0, maxDiscoveredModels+100)
	for i := 0; i < maxDiscoveredModels+100; i++ {
		slugs = append(slugs, fmt.Sprintf("m-%04d", i))
	}
	fake.okSlugs(kindAnthropicAPIKey, slugs...)
	acc := apiKeyAccount(t, svc, routing.VendorAnthropic, "")

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshOK || res.Discovered != maxDiscoveredModels {
		t.Fatalf("result = %+v, want ok capped at %d", res, maxDiscoveredModels)
	}
	rows := storedModels(t, routeStore, acc.ID)
	if len(rows) != maxDiscoveredModels {
		t.Fatalf("stored %d rows, want %d", len(rows), maxDiscoveredModels)
	}
	if rows[0].UpstreamModel != "m-0000" || rows[len(rows)-1].UpstreamModel != fmt.Sprintf("m-%04d", maxDiscoveredModels-1) {
		t.Fatalf("rows span %q..%q, want the vendor's first %d entries", rows[0].UpstreamModel, rows[len(rows)-1].UpstreamModel, maxDiscoveredModels)
	}
}

// --- fail-soft ----------------------------------------------------------------

// Unverifiable (a 401, a timeout, a changed schema, anything) keeps the account's
// models exactly as they were and says so, without an error.
func TestRefreshVendorAccountModelsKeepsTheRowsWhenTheVendorCannotBeVerified(t *testing.T) {
	cases := map[string]fakeDiscoveryResult{
		"unverifiable":           {status: vendorauth.DiscoveryUnverifiable},
		"unverifiable with data": {models: []vendorauth.DiscoveredModel{discovered("gpt-6-luna", "x")}, status: vendorauth.DiscoveryUnverifiable},
		"unknown status":         {models: []vendorauth.DiscoveredModel{discovered("gpt-6-luna", "x")}},
		"ok but empty":           {status: vendorauth.DiscoveryOK},
		"ok but nothing usable":  {models: []vendorauth.DiscoveredModel{discovered("bad slug", "x"), discovered("", "y")}, status: vendorauth.DiscoveryOK},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			fake.results[kindOpenAISubscription] = result
			acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "chatgpt/")
			before := storedModels(t, routeStore, acc.ID)
			if len(before) == 0 {
				t.Fatal("the seeded rows are missing")
			}

			dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
			if err != nil {
				t.Fatalf("RefreshVendorAccountModels: %v (fail-soft must not be an error)", err)
			}
			if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 || res.Detail == "" {
				t.Fatalf("result = %+v, want unverifiable / 0 / a detail", res)
			}
			if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
				t.Fatalf("stored rows = %+v, want the old rows %+v kept", got, before)
			}
			if len(dto.Models) != len(before) {
				t.Fatalf("dto models = %+v, want the old rows", dto.Models)
			}
		})
	}
}

// Nothing to test with (no key, an unconnected subscription) is unverifiable and
// never reaches the vendor.
func TestRefreshVendorAccountModelsWithoutACredentialIsUnverifiable(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	fake.okSlugs(kindOpenAIAPIKey, "gpt-5")
	unconnected := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Unconnected")
	keyed := apiKeyAccount(t, svc, routing.VendorOpenAI, "")
	if _, err := svc.UpdateVendorAccount(context.Background(), ownerToken(), keyed.ID, UpdateVendorAccountRequest{APIKey: strPtr("")}); err != nil {
		t.Fatalf("clear key: %v", err)
	}

	for name, id := range map[string]string{"unconnected subscription": unconnected.ID, "cleared api key": keyed.ID} {
		before := storedModels(t, routeStore, id)
		_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 || res.Detail == "" {
			t.Fatalf("%s: result = %+v, want unverifiable with a detail", name, res)
		}
		if got := storedModels(t, routeStore, id); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s: rows changed to %+v", name, got)
		}
	}
	fake.requireNoCalls(t, "without a credential to send")
}

// A stored credential that cannot be opened is ErrVendorAccountCredentialUnreadable
// (not a verdict), token-free, and nothing is fetched.
func TestRefreshVendorAccountModelsWithAnUnreadableCredential(t *testing.T) {
	for _, authType := range []string{routing.VendorAuthAPIKey, routing.VendorAuthSubscription} {
		t.Run(authType, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			fake.okSlugs(kindOpenAIAPIKey, "gpt-5")
			fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
			req := CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: authType, Name: "Broken"}
			if authType == routing.VendorAuthAPIKey {
				req.APIKey = vendorAccountTestKey
			}
			acc := createTestVendorAccount(t, svc, ownerToken(), req)
			row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
			if err != nil {
				t.Fatalf("VendorAccountByID: %v", err)
			}
			if authType == routing.VendorAuthAPIKey {
				row.APIKey = "enc:!!!not-base64"
			} else {
				row.OAuthTokens = "enc:!!!not-base64"
			}
			if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
				t.Fatalf("UpdateVendorAccount: %v", err)
			}
			cipher, err := capture.New(strings.Repeat("ab", 32))
			if err != nil {
				t.Fatalf("capture.New: %v", err)
			}
			svc.cipher = cipher
			before := storedModels(t, routeStore, acc.ID)

			dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
			if !errors.Is(err, ErrVendorAccountCredentialUnreadable) {
				t.Fatalf("err = %v, want ErrVendorAccountCredentialUnreadable", err)
			}
			if res != (RefreshResult{}) || dto.ID != "" {
				t.Fatalf("result = %+v, dto = %+v, want zero values with an error", res, dto)
			}
			if strings.Contains(err.Error(), vendorAccountTestKey) {
				t.Fatalf("err leaks the key: %v", err)
			}
			fake.requireNoCalls(t, "for an unreadable credential")
			if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
				t.Fatalf("rows changed to %+v", got)
			}
		})
	}
}

// An access token already past its expiry while a refresh token can renew it would
// only be answered with a 401 (the dispatch refreshes lazily): nothing is fetched
// and the detail says why. Without a refresh token the token can never heal, so
// the fetch is tried.
func TestRefreshVendorAccountModelsSkipsAnExpiredTokenThatCanBeRefreshed(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Stale")
	expired := discoveryTestNow.Add(-time.Hour)

	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{AccessToken: discoveryTestAccess, RefreshToken: "refresh-DO-NOT-ECHO", ExpiresAt: expired})
	before := storedModels(t, routeStore, acc.ID)
	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshUnverifiable || !strings.Contains(res.Detail, "expired") {
		t.Fatalf("result = %+v, want unverifiable explaining the expired token", res)
	}
	fake.requireNoCalls(t, "for an expired token a refresh token can renew")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}

	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{AccessToken: discoveryTestAccess, ExpiresAt: expired})
	_, res, err = svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("no refresh token: result = %+v, err = %v, want the fetch to run and succeed", res, err)
	}
	fake.onlyCall(t)
}

// A store failure while writing the rows is a real error (the caller learns the
// refresh did not happen) and leaves the previous rows in place.
func TestRefreshVendorAccountModelsReportsAStoreFailure(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)
	svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("db down")}

	_, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the store failure", err)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
}

// failSetModelsStore is a routing.Store whose SetVendorAccountModels fails.
type failSetModelsStore struct {
	routing.Store
	err error
}

func (f failSetModelsStore) SetVendorAccountModels(context.Context, string, []routing.VendorAccountModel) error {
	return f.err
}

// A rename-and-reprefix that lands while the vendor call is in flight must not be
// overwritten by the older prefix: the rows are written under the prefix the
// account has at write time.
func TestRefreshVendorAccountModelsUsesThePrefixItHasWhenItWrites(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "old/")
	svc.vendorDiscovery.discoverers.OpenAISubscription = func(context.Context, *http.Client, string, string, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		// The prefix changes while the vendor call is "in flight".
		row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
		if err != nil {
			t.Errorf("VendorAccountByID: %v", err)
		}
		row.ModelPrefix = "new/"
		if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
			t.Errorf("UpdateVendorAccount: %v", err)
		}
		return []vendorauth.DiscoveredModel{discovered("gpt-6-luna", "GPT-6 Luna")}, vendorauth.DiscoveryOK
	}

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"new/gpt-6-luna"}) {
		t.Fatalf("gateway models = %v, want the prefix current at write time", got)
	}
}

// --- authorization ---------------------------------------------------------------

// Refreshing sends the owner's sealed credential to the vendor, so it is
// owner-only like the connection test: a stranger, a system-scope non-owner, a
// principal without identity and an unknown id are all 404 and never fetch.
func TestRefreshVendorAccountModelsIsOwnerOnly(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)

	for label, principal := range map[string]auth.Token{
		"another user":           otherToken(),
		"no user identity":       {},
		"another user (admin)":   {UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
		"system scope non-owner": systemToken(),
	} {
		if _, _, err := svc.RefreshVendorAccountModels(context.Background(), principal, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s: err = %v, want ErrVendorAccountNotFound", label, err)
		}
	}
	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
	fake.requireNoCalls(t, "for a refused principal")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("a refused refresh changed the rows to %+v", got)
	}
}

func TestRefreshVendorAccountModelsRefusesWhileTheMasterFlagIsOff(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	setVendorAccountsEnabled(t, svc, false)

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountsDisabled (a disabled area confirms nothing)", err)
	}
	fake.requireNoCalls(t, "while the module is off")
}

// --- OpenAI api-key chat filter ---------------------------------------------------

func TestIsChatCapableOpenAIModel(t *testing.T) {
	for _, id := range []string{
		"gpt-5", "gpt-5.1", "gpt-4.1", "gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo", "gpt-4-turbo-2024-04-09",
		"gpt-4o-search-preview", "gpt-4o-audio-preview", "gpt-5-codex",
		"o1", "o1-pro", "o3", "o3-mini", "o4-mini", "o4-mini-deep-research",
		"chatgpt-4o-latest",
		"ft:gpt-4o-mini-2024-07-18:acme::AbC123",
	} {
		if !isChatCapableOpenAIModel(id) {
			t.Errorf("%q must be kept as a chat model", id)
		}
	}
	for _, id := range []string{
		"text-embedding-3-small", "text-embedding-ada-002", "gpt-embedding-x",
		"whisper-1", "tts-1", "tts-1-hd", "gpt-4o-mini-tts",
		"gpt-4o-transcribe", "gpt-4o-mini-transcribe",
		"dall-e-2", "dall-e-3", "gpt-image-1",
		"omni-moderation-latest", "text-moderation-latest", "text-moderation-stable",
		"gpt-4o-realtime-preview", "gpt-realtime",
		"gpt-3.5-turbo-instruct", "davinci-002", "babbage-002", "sora-2", "computer-use-preview",
		"ft:davinci-002:acme::x", "ft:whisper-1:acme::x",
		"", "o", "omni", "ox1",
	} {
		if isChatCapableOpenAIModel(id) {
			t.Errorf("%q must be dropped as a non-chat model", id)
		}
	}
}

// OpenAI's /v1/models lists everything the key can reach (embeddings, speech,
// image, moderation): an api_key account is seeded with the chat-capable ones only.
func TestRefreshVendorAccountModelsFiltersAnOpenAIAPIKeyListingToChatModels(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAIAPIKey,
		"gpt-4.1", "text-embedding-3-small", "whisper-1", "o4-mini", "tts-1", "dall-e-3",
		"omni-moderation-latest", "chatgpt-4o-latest", "gpt-5", "gpt-4o-mini-tts", "davinci-002",
	)
	acc := apiKeyAccount(t, svc, routing.VendorOpenAI, "")

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshOK || res.Discovered != 4 {
		t.Fatalf("result = %+v, want ok with the 4 chat models", res)
	}
	want := []string{"chatgpt-4o-latest", "gpt-4.1", "gpt-5", "o4-mini"}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway models = %v, want only the chat models %v", got, want)
	}
}

// A listing with nothing chat-capable is no usable list: the rows are kept.
func TestRefreshVendorAccountModelsKeepsTheRowsWhenAnAPIKeyListsNoChatModel(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAIAPIKey, "text-embedding-3-small", "whisper-1", "dall-e-3")
	acc := apiKeyAccount(t, svc, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 {
		t.Fatalf("result = %+v, want unverifiable", res)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
}

// The chat filter is for the unfiltered OpenAI api-key listing only: a
// subscription catalog (already narrowed by visibility) and Anthropic (all chat)
// are taken as sent, whatever the slug looks like.
func TestRefreshVendorAccountModelsFiltersOnlyTheOpenAIAPIKeyListing(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "codex-auto-review", "gpt-6-luna")
	sub := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	fake.okSlugs(kindAnthropicAPIKey, "claude-opus-5", "my-embedding-claude")
	anth := apiKeyAccount(t, svc, routing.VendorAnthropic, "")

	if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), sub.ID); err != nil || res.Discovered != 2 {
		t.Fatalf("subscription: result = %+v, err = %v, want both slugs kept", res, err)
	}
	if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), anth.ID); err != nil || res.Discovered != 2 {
		t.Fatalf("anthropic: result = %+v, err = %v, want both slugs kept", res, err)
	}
}

// --- connect-time discovery ---------------------------------------------------------

// A freshly imported subscription account gets its real models at once, and the
// returned DTO already shows them.
func TestConnectVendorAccountImportDiscoversTheRealModels(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"), discovered("gpt-6.1-sol", "GPT-6.1 Sol"))
	acc := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: "ChatGPT Plus", ModelPrefix: "chatgpt/",
	})
	access := connectTestJWT(t, "acct-77", "plus")

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: access})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if !dto.SubscriptionConnected {
		t.Fatalf("dto = %+v, want connected", dto)
	}
	wantGateway := []string{"chatgpt/gpt-6-luna", "chatgpt/gpt-6.1-sol"}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, wantGateway) {
		t.Fatalf("stored gateway models = %v, want %v", got, wantGateway)
	}
	if len(dto.Models) != 2 || dto.Models[0].GatewayModel != "chatgpt/gpt-6-luna" || dto.Models[0].DisplayName != "GPT-6 Luna" {
		t.Fatalf("dto models = %+v, want the discovered models in the connect response", dto.Models)
	}
	call := fake.onlyCall(t)
	if call.kind != kindOpenAISubscription || call.credential != access || call.accountID != "acct-77" {
		t.Fatalf("call = %+v, want discovery with the imported token and its account id", call)
	}
}

func TestCompleteVendorAccountConnectDiscoversTheRealModels(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"))
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	idToken := connectTestJWT(t, "acct-123", "plus")
	stub.respond(http.StatusOK, `{"access_token":"`+connectTestAccess+`","refresh_token":"`+connectTestRefresh+`","id_token":"`+idToken+`","expires_in":3600}`)

	dto, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "openai-code")
	if err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"gpt-6-luna"}) {
		t.Fatalf("stored gateway models = %v, want the discovered model", got)
	}
	if len(dto.Models) != 1 || dto.Models[0].GatewayModel != "gpt-6-luna" {
		t.Fatalf("dto models = %+v, want the discovered model in the connect response", dto.Models)
	}
	call := fake.onlyCall(t)
	if call.credential != connectTestAccess || call.accountID != "acct-123" {
		t.Fatalf("call = %+v, want discovery with the exchanged access token and account id", call)
	}
}

func TestPollVendorAccountDeviceConnectDiscoversTheRealModels(t *testing.T) {
	svc, routeStore, stub := newVendorDeviceConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"))
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// While the user has not approved yet the poll must not discover anything.
	if connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil || connected {
		t.Fatalf("pending poll = (%v, %v), want (false, nil)", connected, err)
	}
	fake.requireNoCalls(t, "before the account is connected")

	idToken := connectTestJWT(t, "acct-dev", "pro")
	stub.setToken(http.StatusOK, `{"authorization_code":"auth-code","code_challenge":"chal","code_verifier":"ver"}`)
	stub.setExchange(http.StatusOK, `{"access_token":"`+connectTestAccess+`","refresh_token":"`+connectTestRefresh+`","id_token":"`+idToken+`","expires_in":3600}`)
	if connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil || !connected {
		t.Fatalf("poll = (%v, %v), want (true, nil)", connected, err)
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"gpt-6-luna"}) {
		t.Fatalf("stored gateway models = %v, want the discovered model", got)
	}
	call := fake.onlyCall(t)
	if call.credential != connectTestAccess || call.accountID != "acct-dev" {
		t.Fatalf("call = %+v, want discovery with the approved account's token and id", call)
	}
}

// Discovery is best effort at connect time: whatever goes wrong with it, the
// connect still succeeds, the tokens are stored and the account keeps its seeded
// models.
func TestConnectSucceedsWhateverDiscoveryDoes(t *testing.T) {
	failures := map[string]func(svc *Service, routeStore *routing.MemoryStore, fake *fakeVendorDiscoverers){
		"unverifiable": func(*Service, *routing.MemoryStore, *fakeVendorDiscoverers) {},
		"empty ok list": func(_ *Service, _ *routing.MemoryStore, fake *fakeVendorDiscoverers) {
			fake.ok(kindOpenAISubscription)
		},
		"only junk slugs": func(_ *Service, _ *routing.MemoryStore, fake *fakeVendorDiscoverers) {
			fake.okSlugs(kindOpenAISubscription, "bad slug", "../x")
		},
		"the rows cannot be stored": func(svc *Service, routeStore *routing.MemoryStore, fake *fakeVendorDiscoverers) {
			fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
			svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("db down")}
		},
	}
	for name, arrange := range failures {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			fake := installFakeVendorDiscoverers(svc)
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
			seed := storedModels(t, routeStore, acc.ID)
			arrange(svc, routeStore, fake)

			dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
			if err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v (a failing discovery must not fail the connect)", err)
			}
			if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive {
				t.Fatalf("dto = %+v, want a connected active account", dto)
			}
			_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
			if ts.AccessToken != connectTestAccess {
				t.Fatalf("stored token set = %v, want the pasted tokens", ts)
			}
			if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, seed) {
				t.Fatalf("stored rows = %+v, want the seeded rows kept", got)
			}
			if len(dto.Models) != len(seed) {
				t.Fatalf("dto models = %+v, want the seeded rows", dto.Models)
			}
			requireNoToken(t, "dto", dto)
		})
	}
}

// A connect that is refused (stranger, bad token, invalid credentials) never
// discovers anything.
func TestRefusedConnectDoesNotDiscover(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	validators := installFakeVendorValidators(svc)
	validators.setAll(vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "rejected"})
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Sub")

	if _, err := svc.ConnectVendorAccountImport(context.Background(), otherToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("stranger: err = %v", err)
	}
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: ""}); !errors.Is(err, ErrVendorAccountConnectTokenRequired) {
		t.Fatalf("blank: err = %v", err)
	}
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountConnectInvalidCredentials) {
		t.Fatalf("invalid: err = %v", err)
	}
	fake.requireNoCalls(t, "for a refused connect")
}

// --- prefix application ---------------------------------------------------------------

// An account created WITH a prefix is seeded under it, so the static fallback
// follows the same GatewayModel = prefix + UpstreamModel rule as everything after.
func TestCreateVendorAccountSeedsTheCatalogUnderThePrefix(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	plain := apiKeyAccount(t, svc, routing.VendorOpenAI, "")
	prefixed := apiKeyAccount(t, svc, routing.VendorOpenAI, "work/")

	plainRows, prefixedRows := storedModels(t, routeStore, plain.ID), storedModels(t, routeStore, prefixed.ID)
	if len(plainRows) == 0 || len(prefixedRows) != len(plainRows) {
		t.Fatalf("rows = %d / %d, want the same non-empty catalog", len(plainRows), len(prefixedRows))
	}
	for i, row := range prefixedRows {
		if row.UpstreamModel != plainRows[i].UpstreamModel || row.GatewayModel != "work/"+row.UpstreamModel {
			t.Fatalf("row %+v, want the catalog model %q served as work/%s", row, plainRows[i].UpstreamModel, plainRows[i].UpstreamModel)
		}
		if row.APIFlavor != plainRows[i].APIFlavor {
			t.Fatalf("row %+v lost its flavor", row)
		}
	}
	for i, row := range plainRows {
		if row.GatewayModel != row.UpstreamModel {
			t.Fatalf("plain row %d = %+v, want the bare id", i, row)
		}
	}
}

// --- prefix application on update -------------------------------------------------

func seedDiscoveredRows(t *testing.T, routeStore *routing.MemoryStore, id, prefix string) {
	t.Helper()
	rows := []routing.VendorAccountModel{
		{GatewayModel: prefix + "gpt-6-luna", UpstreamModel: "gpt-6-luna", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6 Luna"},
		{GatewayModel: prefix + "gpt-6.1-sol", UpstreamModel: "gpt-6.1-sol", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-6.1 Sol"},
	}
	if err := routeStore.SetVendorAccountModels(context.Background(), id, rows); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

// Changing the prefix re-labels the account's existing rows without asking the
// vendor again: GatewayModel = new prefix + UpstreamModel, everything else kept.
func TestUpdateVendorAccountModelPrefixRelabelsTheRowsWithoutRediscovery(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	seedDiscoveredRows(t, routeStore, acc.ID, "")
	ctx := context.Background()

	wantRows := func(prefix string) []routing.VendorAccountModel {
		return []routing.VendorAccountModel{
			modelRow(acc.ID, prefix+"gpt-6-luna", "gpt-6-luna", routing.APIFlavorOpenAI, "GPT-6 Luna"),
			modelRow(acc.ID, prefix+"gpt-6.1-sol", "gpt-6.1-sol", routing.APIFlavorOpenAI, "GPT-6.1 Sol"),
		}
	}
	for _, prefix := range []string{"chatgpt/", "work-", "", "a:b/"} {
		dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr(prefix)})
		if err != nil {
			t.Fatalf("prefix %q: %v", prefix, err)
		}
		if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, wantRows(prefix)) {
			t.Fatalf("prefix %q: stored rows = %+v, want %+v", prefix, got, wantRows(prefix))
		}
		if dto.ModelPrefix != prefix || len(dto.Models) != 2 || dto.Models[0].GatewayModel != prefix+"gpt-6-luna" {
			t.Fatalf("prefix %q: dto = %+v, want the re-labelled models in the response", prefix, dto)
		}
	}
	fake.requireNoCalls(t, "a prefix change re-labels the rows, it never re-discovers")
}

// The static seed is re-labelled the same way (an account that was never
// discovered still follows its prefix).
func TestUpdateVendorAccountModelPrefixRelabelsTheSeedToo(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	acc := apiKeyAccount(t, svc, routing.VendorOpenAI, "")
	seed := storedModels(t, routeStore, acc.ID)

	if _, err := svc.UpdateVendorAccount(context.Background(), ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("p/")}); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}
	got := storedModels(t, routeStore, acc.ID)
	if len(got) != len(seed) {
		t.Fatalf("rows = %+v, want %d", got, len(seed))
	}
	for _, row := range got {
		if row.GatewayModel != "p/"+row.UpstreamModel {
			t.Fatalf("row %+v is not prefixed", row)
		}
	}
	// Upstream ids are untouched.
	upstream := func(rows []routing.VendorAccountModel) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.UpstreamModel)
		}
		return out
	}
	if !reflect.DeepEqual(upstream(got), upstream(seed)) {
		t.Fatalf("upstream ids changed from %v to %v", upstream(seed), upstream(got))
	}
}

// An update that does not carry a prefix (a rename, a status change) leaves the
// rows alone, and re-sending the same prefix is a no-op.
func TestUpdateVendorAccountWithoutAPrefixChangeLeavesTheRowsAlone(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "p/")
	seedDiscoveredRows(t, routeStore, acc.ID, "p/")
	before := storedModels(t, routeStore, acc.ID)
	// A store that would reject any rows write proves nothing is written.
	svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("must not write")}
	ctx := context.Background()

	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{Name: strPtr("Renamed")}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{Status: strPtr(routing.VendorAccountStatusDisabled)}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("p/")}); err != nil {
		t.Fatalf("same prefix: %v (the rows already match, so nothing is written)", err)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
}

// If re-labelling the rows fails after the prefix was stored, the error is
// reported and re-sending the same prefix heals the mismatch.
func TestUpdateVendorAccountModelPrefixRelabelFailureIsReportedAndRetryHeals(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	seedDiscoveredRows(t, routeStore, acc.ID, "")
	ctx := context.Background()

	svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("db down")}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("w/")}); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the store failure", err)
	}
	svc.routes = routeStore
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("w/")}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"w/gpt-6-luna", "w/gpt-6.1-sol"}) {
		t.Fatalf("gateway models after the retry = %v, want them re-labelled", got)
	}
}

// A stranger's update with a prefix is a 404 and re-labels nothing; an invalid
// prefix is refused before any row is touched.
func TestUpdateVendorAccountModelPrefixAuthorizationAndValidationRunBeforeRelabel(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "p/")
	seedDiscoveredRows(t, routeStore, acc.ID, "p/")
	before := storedModels(t, routeStore, acc.ID)
	ctx := context.Background()

	if _, err := svc.UpdateVendorAccount(ctx, otherToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("x/")}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("stranger: err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("bad prefix")}); !errors.Is(err, ErrVendorAccountModelPrefixInvalid) {
		t.Fatalf("invalid: err = %v, want ErrVendorAccountModelPrefixInvalid", err)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
}

// --- the seam -------------------------------------------------------------------------

func TestNewServiceDefaultsAndInjectsTheVendorDiscoverers(t *testing.T) {
	defaults := NewService(ServiceDeps{})
	d := defaults.vendorDiscovery.discoverers
	if d.OpenAISubscription == nil || d.AnthropicSubscription == nil || d.OpenAIAPIKey == nil || d.AnthropicAPIKey == nil {
		t.Fatalf("defaults = %+v, want every discoverer filled with the vendorauth function", d)
	}
	if c := defaults.vendorDiscovery.client; c == nil || c.Timeout != 10*time.Second {
		t.Fatalf("default discovery client = %+v, want a 10s timeout", c)
	}

	var openAIKeyCalled bool
	injected := NewService(ServiceDeps{VendorDiscoverers: VendorModelDiscoverers{
		OpenAIAPIKey: func(context.Context, *http.Client, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
			openAIKeyCalled = true
			return nil, vendorauth.DiscoveryUnverifiable
		},
	}})
	id := injected.vendorDiscovery.discoverers
	if id.OpenAISubscription == nil || id.AnthropicSubscription == nil || id.AnthropicAPIKey == nil {
		t.Fatalf("injected = %+v, want the un-injected discoverers defaulted", id)
	}
	id.OpenAIAPIKey(context.Background(), nil, "k")
	if !openAIKeyCalled {
		t.Fatal("the injected OpenAI api-key discoverer was replaced by the default")
	}
}

// The portal test constructor must never reach a vendor: the default installed by
// newVendorAccountTestService answers Unverifiable.
func TestVendorAccountTestServiceInstallsNoNetworkDiscoverers(t *testing.T) {
	svc, routeStore := newVendorAccountTestService(t, discoveryTestNow)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil || res.Status != VendorRefreshUnverifiable {
		t.Fatalf("result = %+v, err = %v, want the installed fake's Unverifiable (never a real fetch)", res, err)
	}
}

func TestRefreshResultWireShape(t *testing.T) {
	raw, err := json.Marshal(RefreshResult{Status: VendorRefreshOK, Discovered: 3, Detail: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"status":"ok","discovered":3,"detail":"d"}` {
		t.Fatalf("JSON = %s", raw)
	}
	if VendorRefreshOK != "ok" || VendorRefreshUnverifiable != "unverifiable" {
		t.Fatalf("status constants = %q / %q", VendorRefreshOK, VendorRefreshUnverifiable)
	}
}
