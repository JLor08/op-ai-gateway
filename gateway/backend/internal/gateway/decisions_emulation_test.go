// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"op-ai-gateway/internal/vendorauth"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	emulationAccountID = "acc_sub_oai_emulation"
	emulationPrefix    = "oai/"
	emulationUpstream  = "gpt-6-luna"
	emulationModel     = emulationPrefix + emulationUpstream
)

// emulationAnswerText is the chat model's structured answer to
// decisionsRequestFor: the predicate at 0.8, the choice weighted to "angry".
const emulationAnswerText = `{"answers":{"q0":{"yes":0.8},"q1":{"probabilities":{"o0":0.25,"o1":0.75}}}}`

// emulationUsage is what the fake Responses call reports.
var emulationUsage = inference.Usage{InputTokens: 100, CachedTokens: 40, OutputTokens: 5, TotalTokens: 105}

// emulationProvider stands in for the subscription Responses client: it records
// every Complete call and answers with text and usage, or err.
type emulationProvider struct {
	mu      sync.Mutex
	reqs    []inference.Request
	targets []routing.Target
	text    string
	usage   inference.Usage
	err     error
}

func (p *emulationProvider) Complete(_ context.Context, target routing.Target, req inference.Request) (provider.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	p.targets = append(p.targets, target)
	if p.err != nil {
		return provider.Response{}, p.err
	}
	return provider.Response{Text: p.text, Usage: p.usage}, nil
}

func (p *emulationProvider) calls() ([]inference.Request, []routing.Target) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]inference.Request(nil), p.reqs...), append([]routing.Target(nil), p.targets...)
}

// newDecisionEmulationServer is the vendor-account test server with one ACTIVE
// OpenAI subscription account of usr_va_a serving gpt-6-luna and gpt-5 under
// "oai/", the account's decision-emulation opt-in on, the owner token's
// (vaOwnerSecret) opt-in set to tokenOptIn, and the fake Responses client.
func newDecisionEmulationServer(t *testing.T, tokenOptIn bool) (*Server, *emulationProvider) {
	t.Helper()
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	fake := &emulationProvider{text: emulationAnswerText, usage: emulationUsage}
	srv.Provider = fake
	sealed, err := vendorauth.SealTokenSet(cipher, false, vendorauth.TokenSet{AccessToken: "emu-access", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct"})
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: emulationAccountID, OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
		Name: emulationAccountID, Status: routing.VendorAccountStatusActive, OAuthTokens: sealed, ModelPrefix: emulationPrefix, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routes.UpdateVendorAccountDecisionEmulation(ctx, emulationAccountID, true); err != nil {
		t.Fatalf("UpdateVendorAccountDecisionEmulation: %v", err)
	}
	if err := routes.SetVendorAccountModels(ctx, emulationAccountID, []routing.VendorAccountModel{
		{AccountID: emulationAccountID, GatewayModel: emulationModel, UpstreamModel: emulationUpstream, APIFlavor: routing.APIFlavorOpenAI},
		{AccountID: emulationAccountID, GatewayModel: emulationPrefix + "gpt-5", UpstreamModel: "gpt-5", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	enableVendorAccountsFlag(t, srv)
	setTokenDecisionsEmulation(t, srv, vaOwnerSecret, tokenOptIn)
	return srv, fake
}

// setTokenDecisionsEmulation sets the decision-emulation opt-in of the token
// behind secret.
func setTokenDecisionsEmulation(t *testing.T, srv *Server, secret string, on bool) {
	t.Helper()
	store, ok := srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("bearer store is %T, want *auth.TokenStore", srv.Tokens)
	}
	tok, ok := store.LookupBearer("Bearer " + secret)
	if !ok {
		t.Fatal("token not found")
	}
	tok.DecisionsEmulation = on
	store.UpdateToken(tok)
}

func requireEmulatedHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("X-OP-Decision-Emulated"); got != "chat" {
		t.Fatalf("X-OP-Decision-Emulated = %q, want chat (status %d body %s)", got, rec.Code, rec.Body.String())
	}
}

func onlyEmulationRow(t *testing.T, srv *Server) usage.Event {
	t.Helper()
	events := srv.Usage.ByUser("usr_va_a")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1: %+v", len(events), events)
	}
	return events[0]
}

func requireEmulationTokens(t *testing.T, row usage.Event) {
	t.Helper()
	if row.InputTokens != 60 || row.CachedTokens != 40 || row.OutputTokens != 5 || row.TotalTokens != 105 {
		t.Fatalf("row tokens = in %d cached %d out %d total %d, want 60/40/5/105", row.InputTokens, row.CachedTokens, row.OutputTokens, row.TotalTokens)
	}
}

// POST /v1/decisions for gpt-6-luna on an opted-in subscription account, asked
// by an opted-in token, is answered by the chat model: one buffered Responses
// call with the strict schema and low effort, the OpenAI Decisions answer
// derived from its numbers, the emulation header, and a usage row marked
// decision_emulated on the account with provider_path /responses.
func TestDecisionEmulationDecisionsHappyPath(t *testing.T) {
	srv, fake := newDecisionEmulationServer(t, true)

	rec := postDecisions(t, srv, vaOwnerSecret, decisionsRequestFor(emulationModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	requireEmulatedHeader(t, rec)
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	requireJSONEqual(t, "answer", rec.Body.Bytes(), `{"answers":[`+
		`{"type":"predicate","name":"ok","probability":0.8},`+
		`{"type":"choice","name":null,"choice":"angry","confidence":0.75,"probabilities":[{"value":"calm","probability":0.25},{"value":"angry","probability":0.75}]}`+
		`],"model":"`+emulationModel+`","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":105}}`)

	reqs, targets := fake.calls()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if reqs[0].OutputSchema == nil || !reqs[0].OutputSchema.Strict || reqs[0].ReasoningEffort != "low" || reqs[0].Model != emulationUpstream || reqs[0].Stream {
		t.Fatalf("provider request = model %q effort %q schema %+v stream %v, want the upstream model, low effort, a strict schema, no stream", reqs[0].Model, reqs[0].ReasoningEffort, reqs[0].OutputSchema, reqs[0].Stream)
	}
	if targets[0].VendorAccountID != emulationAccountID || targets[0].Provider != routing.ProviderVendorOpenAISubscription || !targets[0].DecisionEmulated {
		t.Fatalf("provider target = %+v, want the emulated subscription target of %s", targets[0], emulationAccountID)
	}

	row := onlyEmulationRow(t, srv)
	if !row.DecisionEmulated || row.Status != "success" || row.AccountID != emulationAccountID || row.ProviderPath != "/responses" ||
		row.ReqPath != "/v1/decisions" || row.APIFlavor != routing.APIFlavorOpenAIDecisions || row.HTTPStatus != http.StatusOK || row.Model != emulationModel {
		t.Fatalf("row = %+v, want a decision_emulated success row on %s with provider_path /responses", row, emulationAccountID)
	}
	requireEmulationTokens(t, row)
	if row.CostEUR != 0 {
		t.Fatalf("row cost = %v, want 0", row.CostEUR)
	}
}

// POST /v1/systemone for the same model is translated to OpenAI Decisions,
// emulated, and its answer translated back to System One keyed by the client's
// own question keys.
func TestDecisionEmulationSystemOneHappyPath(t *testing.T) {
	srv, fake := newDecisionEmulationServer(t, true)

	rec := postSystemOneAs(t, srv, vaOwnerSecret, systemOneTranslateRequestFor(emulationModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	requireEmulatedHeader(t, rec)
	var got struct {
		Answers map[string]struct {
			Type   string  `json:"type"`
			Noul   float64 `json:"noul"`
			Choice string  `json:"choice"`
		} `json:"answers"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, rec.Body.String())
	}
	keys := make([]string, 0, len(got.Answers))
	for k := range got.Answers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"ok", "q1"}) || got.Answers["ok"].Type != "noul" || got.Answers["ok"].Noul != 0.8 ||
		got.Answers["q1"].Type != "choice" || got.Answers["q1"].Choice != "angry" || got.Model != emulationModel {
		t.Fatalf("answer = %s, want System One answers ok (noul 0.8) and q1 (choice angry) for %s", rec.Body.String(), emulationModel)
	}
	if reqs, _ := fake.calls(); len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	row := onlyEmulationRow(t, srv)
	if !row.DecisionEmulated || row.Status != "success" || row.ProviderPath != "/responses" || row.ReqPath != "/v1/systemone" || row.APIFlavor != routing.APIFlavorDecisions {
		t.Fatalf("row = %+v, want a decision_emulated success /v1/systemone row with provider_path /responses", row)
	}
	requireEmulationTokens(t, row)
}

// A model answer that is not the schema's JSON is the endpoint's 502
// upstream_error, with the header and the tokens the vendor reported (it
// answered, and is paid).
func TestDecisionEmulationUnparseableAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, code string
	}{
		{"decisions", "/v1/decisions", decisionsRequestFor(emulationModel), codeDecisionsUpstreamError},
		{"systemone", "/v1/systemone", systemOneTranslateRequestFor(emulationModel), codeSystemOneUpstreamError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := newDecisionEmulationServer(t, true)
			fake.text = "I think the answer is yes."

			rec := postImagesWithHeaders(t, srv, tc.path, tc.body, map[string]string{"Authorization": "Bearer " + vaOwnerSecret})

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d body = %s, want 502", rec.Code, rec.Body.String())
			}
			requireEmulatedHeader(t, rec)
			requireErrorCode(t, rec.Body.String(), tc.code)
			row := onlyEmulationRow(t, srv)
			if !row.DecisionEmulated || row.Status != "error" || row.ErrorCode != tc.code || row.HTTPStatus != http.StatusBadGateway || row.ProviderPath != "/responses" {
				t.Fatalf("row = %+v, want a decision_emulated error row with %s", row, tc.code)
			}
			requireEmulationTokens(t, row)
		})
	}
}

// A provider error is answered with the status the chat path gives it; the
// generic provider code becomes the endpoint's upstream_error, and the row
// keeps the chat path's classification and carries the mark.
func TestDecisionEmulationProviderAuthRejected(t *testing.T) {
	srv, fake := newDecisionEmulationServer(t, true)
	authErr := fmt.Errorf("%w: upstream status 401", provider.ErrAuthRejected)
	fake.err = authErr

	rec := postDecisions(t, srv, vaOwnerSecret, decisionsRequestFor(emulationModel))

	if rec.Code != completionHTTPStatus(authErr) {
		t.Fatalf("status = %d body = %s, want the chat path's %d", rec.Code, rec.Body.String(), completionHTTPStatus(authErr))
	}
	requireEmulatedHeader(t, rec)
	requireErrorCode(t, rec.Body.String(), codeDecisionsUpstreamError)
	row := onlyEmulationRow(t, srv)
	if !row.DecisionEmulated || row.Status != "error" || row.ErrorCode != completionErrorCode(authErr) || row.AccountID != emulationAccountID || row.HTTPStatus != rec.Code {
		t.Fatalf("row = %+v, want a decision_emulated error row with %s on %s", row, completionErrorCode(authErr), emulationAccountID)
	}
}

// A provider timeout keeps the chat path's specific code.
func TestDecisionEmulationProviderTimeoutKeepsItsCode(t *testing.T) {
	srv, fake := newDecisionEmulationServer(t, true)
	fake.err = provider.ErrTimeout

	rec := postSystemOneAs(t, srv, vaOwnerSecret, systemOneTranslateRequestFor(emulationModel))

	if rec.Code != completionHTTPStatus(provider.ErrTimeout) {
		t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), completionHTTPStatus(provider.ErrTimeout))
	}
	requireEmulatedHeader(t, rec)
	requireErrorCode(t, rec.Body.String(), provider.ErrTimeout.Error())
}

// A request the emulator cannot express is the translator's 400 with the
// header, a marked row and no provider call.
func TestDecisionEmulationTranslatorRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"decisions", "/v1/decisions", `{"model":"` + emulationModel + `","input":"x","questions":[{"type":"bogus","instructions":"?"}]}`},
		{"systemone", "/v1/systemone", `{"model":"` + emulationModel + `","state":"x","questions":{"a":{"type":"bogus","instructions":"?"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := newDecisionEmulationServer(t, true)

			rec := postImagesWithHeaders(t, srv, tc.path, tc.body, map[string]string{"Authorization": "Bearer " + vaOwnerSecret})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", rec.Code, rec.Body.String())
			}
			requireEmulatedHeader(t, rec)
			if reqs, _ := fake.calls(); len(reqs) != 0 {
				t.Fatalf("provider calls = %d, want 0", len(reqs))
			}
			row := onlyEmulationRow(t, srv)
			if !row.DecisionEmulated || row.Status != "error" || row.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("row = %+v, want a decision_emulated 400 row", row)
			}
		})
	}
}

// A token without the opt-in is not routed to the emulated model: no provider
// call, no header, today's 404.
func TestDecisionEmulationNeedsTheTokenOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"decisions", "/v1/decisions", decisionsRequestFor(emulationModel)},
		{"systemone", "/v1/systemone", systemOneTranslateRequestFor(emulationModel)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := newDecisionEmulationServer(t, false)

			rec := postImagesWithHeaders(t, srv, tc.path, tc.body, map[string]string{"Authorization": "Bearer " + vaOwnerSecret})

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d body = %s, want 404", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-OP-Decision-Emulated"); got != "" {
				t.Fatalf("X-OP-Decision-Emulated = %q, want none", got)
			}
			if reqs, _ := fake.calls(); len(reqs) != 0 {
				t.Fatalf("provider calls = %d, want 0", len(reqs))
			}
		})
	}
}

// A token that opted into confidence normalization still gets it on the
// emulated answer (harmless: the confidences already are the largest
// probabilities).
func TestDecisionEmulationAppliesConfidenceNormalization(t *testing.T) {
	srv, _ := newDecisionEmulationServer(t, true)
	store := srv.Tokens.(*auth.TokenStore)
	tok, _ := store.LookupBearer("Bearer " + vaOwnerSecret)
	tok.DecisionsConfidenceNormalize = true
	store.UpdateToken(tok)

	rec := postDecisions(t, srv, vaOwnerSecret, decisionsRequestFor(emulationModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	requireEmulatedHeader(t, rec)
	if got := choiceConfidence(t, rec.Body.Bytes()); got != 0.75 {
		t.Fatalf("choice confidence = %v, want 0.75", got)
	}
}

// GET /v1/systemone/models lists the emulated model for the opted-in token
// only.
func TestDecisionEmulationSystemOneModelsListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		optIn  bool
		listed bool
	}{
		{"opted in", true, true},
		{"not opted in", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newDecisionEmulationServer(t, tc.optIn)

			rec := vaDo(t, srv, http.MethodGet, "/v1/systemone/models", vaOwnerSecret, "")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			var list struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("listing is not JSON: %v", err)
			}
			ids := make([]string, 0, len(list.Data))
			for _, m := range list.Data {
				ids = append(ids, m.ID)
			}
			if slices.Contains(ids, emulationModel) != tc.listed {
				t.Fatalf("listing = %v, want %s listed = %v", ids, emulationModel, tc.listed)
			}
			if slices.Contains(ids, emulationPrefix+"gpt-5") {
				t.Fatalf("listing = %v, want no chat-only model", ids)
			}
		})
	}
}
