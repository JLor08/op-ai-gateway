// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	vendorDecisionAccountID = "acc_decisions_oai"
	vendorDecisionPlainKey  = "sk-test-decisions-key"
	vendorDecisionPrefix    = "oa/"
	vendorDecisionUpstream  = "gpt-6-luna"
	vendorDecisionModel     = vendorDecisionPrefix + vendorDecisionUpstream
)

// vendorDecisionsAnswer is api.openai.com's /v1/decisions answer to
// decisionsRequestFor: the shape of decisionsOAIAnswer, naming the vendor model.
const vendorDecisionsAnswer = `{"answers":[` +
	`{"type":"predicate","name":"ok","probability":0.9},` +
	`{"type":"choice","name":null,"choice":"calm","confidence":0.5,"probabilities":[{"value":"calm","probability":0.6},{"value":"angry","probability":0.2}]}` +
	`],"model":"gpt-6-luna","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":102}}`

// vendorDecisionsStub stands in for api.openai.com: it answers /v1/decisions
// with vendorDecisionsAnswer and anything else 404, and records each call.
type vendorDecisionsStub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []vendorDecisionsCall
}

type vendorDecisionsCall struct {
	host   string
	path   string
	header http.Header
	body   []byte
}

func newVendorDecisionsStub(t *testing.T) *vendorDecisionsStub {
	t.Helper()
	st := &vendorDecisionsStub{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		st.mu.Lock()
		st.calls = append(st.calls, vendorDecisionsCall{host: r.Header.Get("X-Original-Host"), path: r.URL.Path, header: r.Header.Clone(), body: b})
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/decisions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"not found","type":"invalid_request_error","code":null}}`)
			return
		}
		_, _ = io.WriteString(w, vendorDecisionsAnswer)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (st *vendorDecisionsStub) recorded() []vendorDecisionsCall {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]vendorDecisionsCall(nil), st.calls...)
}

// originalHostTransport sends every request to the stub, telling it which host
// the request named (X-Original-Host), so the production target (Endpoint
// https://api.openai.com) is exercised with no seam for its base URL.
type originalHostTransport struct{ stub *url.URL }

func (rt originalHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("X-Original-Host", r.URL.Scheme+"://"+r.URL.Host)
	r2.URL.Scheme = rt.stub.Scheme
	r2.URL.Host = rt.stub.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// newVendorDecisionsServer is the vendor-account test server (usr_va_a owns
// the accounts, vaOwnerSecret is its all-access token, vaOtherSecret another
// user's token without vendor access) with the master flag on, one OpenAI
// api-key account serving gpt-6-luna and gpt-5 under the "oa/" prefix, and
// the vendor_openai client pointed at the stub.
func newVendorDecisionsServer(t *testing.T) (*Server, *vendorDecisionsStub) {
	t.Helper()
	stub := newVendorDecisionsStub(t)
	stubURL, err := url.Parse(stub.srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorOpenAI: provider.NewOpenAICompatibleClient(&http.Client{Transport: originalHostTransport{stub: stubURL}}),
	}, provider.NewMock())
	sealed, err := capture.SealSecret(cipher, false, vendorDecisionPlainKey)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: vendorDecisionAccountID, OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: vendorDecisionAccountID, Status: routing.VendorAccountStatusActive, APIKey: sealed, ModelPrefix: vendorDecisionPrefix, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routes.SetVendorAccountModels(ctx, vendorDecisionAccountID, []routing.VendorAccountModel{
		{AccountID: vendorDecisionAccountID, GatewayModel: vendorDecisionModel, UpstreamModel: vendorDecisionUpstream, APIFlavor: routing.APIFlavorOpenAI},
		{AccountID: vendorDecisionAccountID, GatewayModel: vendorDecisionPrefix + "gpt-5", UpstreamModel: "gpt-5", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	enableVendorAccountsFlag(t, srv)
	return srv, stub
}

// POST /v1/decisions for the decision model of an OpenAI api-key account the
// token may use passes through to api.openai.com/v1/decisions: the opened key
// as Bearer, the client's body value-equal but for the upstream slug, the
// upstream's bytes back, and a usage row on the account with the cached tokens
// and no energy or cost.
func TestVendorDecisionsPassesThroughToTheOpenAIAPIKeyAccount(t *testing.T) {
	srv, stub := newVendorDecisionsServer(t)
	body := decisionsRequestFor(vendorDecisionModel)

	rec := postDecisions(t, srv, vaOwnerSecret, body)

	if rec.Code != http.StatusOK || rec.Body.String() != vendorDecisionsAnswer {
		t.Fatalf("status = %d body = %q, want 200 and the upstream's bytes", rec.Code, rec.Body.String())
	}
	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.host != "https://api.openai.com" || call.path != "/v1/decisions" {
		t.Fatalf("upstream = %s%s, want https://api.openai.com/v1/decisions", call.host, call.path)
	}
	if got := call.header.Get("Authorization"); got != "Bearer "+vendorDecisionPlainKey {
		t.Fatalf("Authorization = %q, want Bearer <the opened key>", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sent["model"] = vendorDecisionUpstream
	want, _ := json.Marshal(sent)
	requireJSONEqual(t, "upstream body", call.body, string(want))

	events := srv.Usage.ByUser("usr_va_a")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	got := events[0]
	if got.AccountID != vendorDecisionAccountID || got.Status != "success" || got.ProviderPath != "/v1/decisions" || got.ReqPath != "/v1/decisions" || got.APIFlavor != routing.APIFlavorOpenAIDecisions {
		t.Fatalf("row = %+v, want a success openai_decisions row on %s with provider_path /v1/decisions", got, vendorDecisionAccountID)
	}
	if got.InputTokens != 50 || got.CachedTokens != 40 || got.CacheWriteTokens != 10 || got.OutputTokens != 2 {
		t.Fatalf("row tokens = in %d cached %d write %d out %d, want 50/40/10/2", got.InputTokens, got.CachedTokens, got.CacheWriteTokens, got.OutputTokens)
	}
	if got.CostEUR != 0 || got.EnergyWh != 0 {
		t.Fatalf("row cost/energy = %v/%v, want 0/0 (vendor rows are cost-free)", got.CostEUR, got.EnergyWh)
	}
}

// POST /v1/systemone for the same model is translated to the vendor's
// /v1/decisions and the answer translated back.
func TestVendorSystemOneTranslatesToTheOpenAIAPIKeyAccount(t *testing.T) {
	srv, stub := newVendorDecisionsServer(t)

	rec := postSystemOneAs(t, srv, vaOwnerSecret, systemOneTranslateRequestFor(vendorDecisionModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(systemOneTranslatedAnswer), &answer); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	answer["model"] = vendorDecisionModel
	wantAnswer, _ := json.Marshal(answer)
	requireJSONEqual(t, "client body", rec.Body.Bytes(), string(wantAnswer))

	calls := stub.recorded()
	if len(calls) != 1 || calls[0].host != "https://api.openai.com" || calls[0].path != "/v1/decisions" {
		t.Fatalf("upstream calls = %+v, want one call to https://api.openai.com/v1/decisions", calls)
	}
	if got := calls[0].header.Get("Authorization"); got != "Bearer "+vendorDecisionPlainKey {
		t.Fatalf("Authorization = %q, want Bearer <the opened key>", got)
	}
	var upstream map[string]any
	if err := json.Unmarshal([]byte(systemOneTranslatedOAIBody), &upstream); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	upstream["model"] = vendorDecisionUpstream
	wantUpstream, _ := json.Marshal(upstream)
	requireJSONEqual(t, "upstream body", calls[0].body, string(wantUpstream))

	events := srv.Usage.ByUser("usr_va_a")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	got := events[0]
	if got.AccountID != vendorDecisionAccountID || got.Status != "success" || got.ProviderPath != "/v1/decisions" || got.ReqPath != "/v1/systemone" || got.APIFlavor != apiFlavorSystemOne {
		t.Fatalf("row = %+v, want a success decisions row on /v1/systemone with provider_path /v1/decisions", got)
	}
	if got.CachedTokens != 40 || got.CacheWriteTokens != 10 {
		t.Fatalf("row cached/write = %d/%d, want 40/10", got.CachedTokens, got.CacheWriteTokens)
	}
	if got.CostEUR != 0 || got.EnergyWh != 0 {
		t.Fatalf("row cost/energy = %v/%v, want 0/0 (vendor rows are cost-free)", got.CostEUR, got.EnergyWh)
	}
}

// A token without vendor access, and a model of the account that is not a
// decision model, are never sent to the vendor on either endpoint.
func TestVendorDecisionEndpointsRefuseWithoutAccessOrForAChatModel(t *testing.T) {
	srv, stub := newVendorDecisionsServer(t)
	cases := []struct {
		name, secret, model string
	}{
		{"no vendor access", vaOtherSecret, vendorDecisionModel},
		{"a chat model", vaOwnerSecret, vendorDecisionPrefix + "gpt-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postDecisions(t, srv, tc.secret, decisionsRequestFor(tc.model)); rec.Code != http.StatusNotFound {
				t.Fatalf("/v1/decisions status = %d body = %s, want 404", rec.Code, rec.Body.String())
			}
			if rec := postSystemOneAs(t, srv, tc.secret, systemOneTranslateRequestFor(tc.model)); rec.Code != http.StatusNotFound {
				t.Fatalf("/v1/systemone status = %d body = %s, want 404", rec.Code, rec.Body.String())
			}
		})
	}
	if calls := stub.recorded(); len(calls) != 0 {
		t.Fatalf("upstream calls = %+v, want none", calls)
	}
}

// The listings show the decision model to an entitled token only: GET
// /v1/systemone/models names it, /api/portal/models lists it with its chat
// flavors plus the decisions flavor and both decision flags.
func TestVendorDecisionModelIsListedForAnEntitledToken(t *testing.T) {
	srv, _ := newVendorDecisionsServer(t)

	systemOneModels := func(secret string) []string {
		rec := getSystemOneModels(t, srv, map[string]string{"Authorization": "Bearer " + secret})
		if rec.Code != http.StatusOK {
			t.Fatalf("/v1/systemone/models status = %d body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data []map[string]string `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := make([]string, 0, len(got.Data))
		for _, m := range got.Data {
			ids = append(ids, m["id"])
		}
		return ids
	}
	if got := systemOneModels(vaOwnerSecret); !reflect.DeepEqual(got, []string{vendorDecisionModel}) {
		t.Fatalf("/v1/systemone/models for the owner = %v, want [%s]", got, vendorDecisionModel)
	}
	if got := systemOneModels(vaOtherSecret); len(got) != 0 {
		t.Fatalf("/v1/systemone/models without vendor access = %v, want []", got)
	}

	rec := vaDo(t, srv, http.MethodGet, "/api/portal/models", vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/portal/models status = %d body = %s", rec.Code, rec.Body.String())
	}
	var listing portal.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *portal.ModelDTO
	for i := range listing.Data {
		if listing.Data[i].ID == vendorDecisionModel {
			found = &listing.Data[i]
		}
	}
	if found == nil {
		t.Fatalf("/api/portal/models = %s, want %s listed", rec.Body.String(), vendorDecisionModel)
	}
	if !reflect.DeepEqual(found.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorDecisions, routing.APIFlavorOpenAI}) || !found.Decision || !found.DecisionOpenAI {
		t.Fatalf("row = %+v, want flavors [anthropic decisions openai] with decision and decision_openai", *found)
	}
	if strings.Contains(rec.Body.String(), vendorDecisionPlainKey) {
		t.Fatal("the key leaked into the listing")
	}
}

// Chat behavior for the decision model is unchanged: the chat offering still
// holds it, so a redirect-enabled chat request naming it is not redirected,
// while an unknown name is (the control proves the redirect is armed).
func TestVendorDecisionModelIsNotRedirectedOnChat(t *testing.T) {
	srv, _ := newVendorDecisionsServer(t)
	// The owner's all-access token (tok_va_a), with the redirect switched on.
	token := auth.Token{ID: "tok_va_a", UserID: "usr_va_a", Active: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{All: true}}
	token.UnknownModelRedirect = true
	token.UnknownModelRedirectBlocked = true
	token.UnknownModelFallback = vendorDecisionPrefix + "gpt-5"

	for _, flavor := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		off := srv.Portal.ModelOfferingFor(context.Background(), token, flavor, nil)
		if _, ok := off.Callable[vendorDecisionModel]; !ok {
			t.Fatalf("%s offering = %+v, want %s callable", flavor, off, vendorDecisionModel)
		}
		if _, ok := off.Existing[vendorDecisionModel]; !ok {
			t.Fatalf("%s offering = %+v, want %s existing", flavor, off, vendorDecisionModel)
		}
		if to := redirectUnknownModel(token, vendorDecisionModel, off); to != "" {
			t.Fatalf("%s: %s redirected to %q, want no redirect", flavor, vendorDecisionModel, to)
		}
		if to := redirectUnknownModel(token, "no-such-model", off); to != vendorDecisionPrefix+"gpt-5" {
			t.Fatalf("%s: control redirect = %q, want the fallback %s", flavor, to, vendorDecisionPrefix+"gpt-5")
		}
	}
}
