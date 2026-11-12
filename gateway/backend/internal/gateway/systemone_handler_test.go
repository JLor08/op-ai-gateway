// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// systemOneGatewayModel is the name a client sends; systemOneUpstreamModel the
	// model id the decision server serves. They differ so the relay's model
	// rewrite is observable at the upstream.
	systemOneGatewayModel  = "sys-decider"
	systemOneUpstreamModel = "system_one_real"

	// systemOneBody carries state and questions with spacing, key order and
	// unicode a decode/re-encode would normalise, so a byte-exact relay is observable.
	systemOneBody = `{"model":"sys-decider",  "state":"Ein Text: über alles", "questions":{"zz":{"type":"noul","instructions":"Is it ok?"},"aa":{"type":"score","instructions":"How much?","criteria":[1,2,3]}}}`
)

const systemOneOKBody = `{"model":"system_one_real","answers":{"zz":"yes","aa":2},"usage":{"input_tokens":213,"output_tokens":0}}`

// systemOneUpstreamCall is one request a System One stub received.
type systemOneUpstreamCall struct {
	method string
	path   string
	body   []byte
}

// newSystemOneUpstream starts a System One-shaped upstream answering every
// request with status and body as JSON. calls receives each request it saw.
func newSystemOneUpstream(t *testing.T, status int, body string) (*httptest.Server, <-chan systemOneUpstreamCall) {
	t.Helper()
	calls := make(chan systemOneUpstreamCall, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- systemOneUpstreamCall{method: r.Method, path: r.URL.Path, body: b}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up, calls
}

func requireNoSystemOneUpstreamCall(t *testing.T, calls <-chan systemOneUpstreamCall) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the upstream received %s %s, want no call", c.method, c.path)
	default:
	}
}

func receivedSystemOneCall(t *testing.T, calls <-chan systemOneUpstreamCall) systemOneUpstreamCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream received no call")
		return systemOneUpstreamCall{}
	}
}

// newSystemOneCapableTestServer builds a server over the shared chat seed plus
// one application declaring the decisions flavor, pointing at upstreamURL, with
// three mappings that are the verdict states the gate tells apart: sys-decider
// ((decision, yes), serves), no-decision ((decision, no)) and unprobed-decision
// (no row).
func newSystemOneCapableTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)

	ctx := context.Background()
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sysone", Name: "System One Upstream", Domain: up.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sysone", ServerID: "srv-sysone", Type: routing.ProviderAudioCpp, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorDecisions}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for _, m := range []struct{ id, gateway, upstream, verdict string }{
		{"route-sysone", systemOneGatewayModel, systemOneUpstreamModel, routing.CapabilityYes},
		{"route-sysone-no", "no-decision", "other_model", routing.CapabilityNo},
		{"route-sysone-unprobed", "unprobed-decision", "not_loaded_yet", ""},
	} {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-sysone", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
		if m.verdict == "" {
			continue
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, m.id, []routing.CapabilityRow{{Capability: routing.CapabilityDecision, Verdict: m.verdict, Source: routing.CapabilitySourceManual, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sysone", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	return New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:             routeStore,
		Portal:             portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		InternalAuthSecret: "s3cret",
		Users:              directory,
	})
}

func postSystemOne(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postImagesWithHeaders(t, srv, "/v1/systemone", body, map[string]string{"Authorization": "Bearer dev-secret"})
}

func TestApiFlavorSystemOneIsTheRoutingDecisionsFlavor(t *testing.T) {
	flavor, routingFlavor := apiFlavorSystemOne, routing.APIFlavorDecisions
	if flavor != routingFlavor {
		t.Fatalf("apiFlavorSystemOne = %q, want %q", flavor, routingFlavor)
	}
}

func TestSystemOneRelaysTheRawBodyAndRewritesTheModel(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postSystemOne(t, srv, systemOneBody)

	if rec.Code != http.StatusOK || rec.Body.String() != systemOneOKBody {
		t.Fatalf("status = %d body = %q, want 200 and the upstream's body", rec.Code, rec.Body.String())
	}
	call := receivedSystemOneCall(t, calls)
	if call.method != http.MethodPost || call.path != "/v1/systemone" {
		t.Fatalf("upstream saw %s %s, want POST /v1/systemone", call.method, call.path)
	}
	// Every other field is value-lossless: the shared native model rewrite
	// (rewriteModelField) re-serializes the body, so key order and spacing may
	// change, but state and questions decode to exactly what the client sent.
	var got, sent map[string]any
	if err := json.Unmarshal(call.body, &got); err != nil {
		t.Fatalf("upstream body is not JSON: %v; %s", err, call.body)
	}
	if err := json.Unmarshal([]byte(systemOneBody), &sent); err != nil {
		t.Fatalf("fixture body is not JSON: %v", err)
	}
	if got["model"] != systemOneUpstreamModel {
		t.Fatalf("upstream model = %v, want %q; body = %s", got["model"], systemOneUpstreamModel, call.body)
	}
	if !reflect.DeepEqual(got["state"], sent["state"]) || !reflect.DeepEqual(got["questions"], sent["questions"]) || len(got) != len(sent) {
		t.Fatalf("upstream body = %s, want state and questions (and no other field) as the client sent them", call.body)
	}
}

func TestSystemOneRecordsTokenUsage(t *testing.T) {
	up, _ := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postSystemOne(t, srv, systemOneBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	got := lastUsageEvent(t, srv)
	if got.InputTokens != 213 || got.OutputTokens != 0 || got.BillingUnit != "" || got.ProviderPath != "/v1/systemone" {
		t.Fatalf("row = in %d out %d unit %q path %q, want 213/0/\"\"/ /v1/systemone", got.InputTokens, got.OutputTokens, got.BillingUnit, got.ProviderPath)
	}
	if got.APIFlavor != apiFlavorSystemOne || got.ReqPath != "/v1/systemone" || got.Status != "success" {
		t.Fatalf("row = %+v, want a success row labelled decisions on /v1/systemone", got)
	}
}

// A 200 whose body carries no usage object still reaches the client untouched,
// and its row records zero tokens rather than failing or inventing a count.
func TestSystemOneWithoutUsageRecordsZeroTokens(t *testing.T) {
	const noUsage = `{"model":"system_one_real","answers":{"zz":"yes"}}`
	up, _ := newSystemOneUpstream(t, http.StatusOK, noUsage)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postSystemOne(t, srv, systemOneBody)

	if rec.Code != http.StatusOK || rec.Body.String() != noUsage {
		t.Fatalf("status = %d body = %q, want 200 and the upstream's body", rec.Code, rec.Body.String())
	}
	if events := srv.Usage.All(); len(events) != 1 {
		t.Fatalf("usage events = %+v, want exactly one row", events)
	}
	got := lastUsageEvent(t, srv)
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.Status != "success" {
		t.Fatalf("row = in %d out %d status %q, want 0/0/success", got.InputTokens, got.OutputTokens, got.Status)
	}
}

func TestSystemOneRelaysAnUpstreamErrorVerbatim(t *testing.T) {
	const upstreamErr = `{"detail":[{"loc":["body","questions"],"msg":"bad question","type":"value_error"}]}`
	up, _ := newSystemOneUpstream(t, http.StatusUnprocessableEntity, upstreamErr)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postSystemOne(t, srv, systemOneBody)

	if rec.Code != http.StatusUnprocessableEntity || rec.Body.String() != upstreamErr {
		t.Fatalf("status = %d body = %q, want 422 and the identical bytes", rec.Code, rec.Body.String())
	}
	got := lastUsageEvent(t, srv)
	if got.ErrorCode != "upstream.422" || got.Status != "error" || got.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("row = %q/%q/%d, want upstream.422/error/422", got.ErrorCode, got.Status, got.HTTPStatus)
	}
}

func TestValidateSystemOneRequest(t *testing.T) {
	const qs = `"questions":{"q":{"type":"noul","instructions":"x"}}`
	cases := []struct {
		name string
		body string
		want string // error code, "" for valid
	}{
		{"valid", `{"model":"m","state":"s",` + qs + `}`, ""},
		{"no model", `{"state":"s",` + qs + `}`, systemOneModelRequired},
		{"model not a string", `{"model":3,` + qs + `}`, systemOneModelRequired},
		{"blank model", `{"model":"  ",` + qs + `}`, systemOneModelRequired},
		{"no questions", `{"model":"m","state":"s"}`, systemOneQuestionsRequired},
		{"questions is an array", `{"model":"m","questions":[]}`, systemOneQuestionsRequired},
		{"questions is empty", `{"model":"m","questions":{}}`, systemOneQuestionsRequired},
		{"questions null", `{"model":"m","questions":null}`, systemOneQuestionsRequired},
		{"stream true", `{"model":"m","stream":true,` + qs + `}`, systemOneStreamUnsupported},
		{"stream is a string", `{"model":"m","stream":"false",` + qs + `}`, systemOneStreamUnsupported},
		{"stream false", `{"model":"m","stream":false,` + qs + `}`, ""},
		{"stream null", `{"model":"m","stream":null,` + qs + `}`, ""},
		{"model checked before questions", `{"questions":{}}`, systemOneModelRequired},
		{"not json", `nope`, systemOneModelRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSystemOneRequest([]byte(tc.body))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("error = %+v, want nil", err)
			case tc.want != "" && (err == nil || err.Code != tc.want):
				t.Fatalf("error = %+v, want code %s", err, tc.want)
			}
		})
	}
}

// A request failing validation is refused with 400 and its code, before any
// routing or upstream call, and records no usage.
func TestSystemOneAnswersAValidationErrorWith400(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postSystemOne(t, srv, `{"model":"sys-decider","questions":{}}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), systemOneQuestionsRequired)
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestSystemOneRefusesAMappingWithoutADecisionVerdict(t *testing.T) {
	for _, model := range []string{"no-decision", "unprobed-decision"} {
		t.Run(model, func(t *testing.T) {
			up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
			srv := newSystemOneCapableTestServer(t, up.URL)

			rec := postSystemOne(t, srv, `{"model":"`+model+`","questions":{"q":{"type":"noul","instructions":"x"}}}`)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
			requireNoSystemOneUpstreamCall(t, calls)
		})
	}
}

// newServerAgentSystemOneSpecTestServer seeds a server_agent application
// candidacy ADMITS for System One (app-level flavors include decisions, the
// mapping carries (decision, yes)) whose mapping's runtime spec carries
// specFlavors: the spec is the per-model authority, known only after the
// resolve, so the relay's own effective-flavor check decides.
func newServerAgentSystemOneSpecTestServer(t *testing.T, prov provider.Client, specFlavors []string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-agent-sys", Name: "Agent System One", Domain: "agent-sys.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://agent-sys.example.test:8081", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-agent-sys", ServerID: "srv-agent-sys", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}, Priority: 10, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-agent-sys", ApplicationID: "app-agent-sys", GatewayModelName: "agent-decider", AppModelName: "agent-decider", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-agent-sys", []routing.CapabilityRow{{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-agent-sys", MappingID: "route-agent-sys", APIFlavors: specFlavors, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-agent-sys", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

const systemOneAgentBody = `{"model":"agent-decider","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`

func TestSystemOneRefusesAServerAgentChildWhoseSpecExcludesDecisions(t *testing.T) {
	prov := &recordingProxyProvider{respBody: systemOneOKBody}
	srv := newServerAgentSystemOneSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI})

	rec := postSystemOne(t, srv, systemOneAgentBody)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].ErrorCode != "routing.no_model_route" {
		t.Fatalf("usage events = %+v, want one routing.no_model_route error", events)
	}
	if got := events[0]; got.Host != "" || got.RouteID != "" || got.ProviderPath != "" || got.ReqPath != "/v1/systemone" || got.HTTPStatus != http.StatusNotFound {
		t.Fatalf("usage row = %+v, want an upstream-less 404 row on /v1/systemone", got)
	}
}

// The positive control of the test above: the same fixture with a spec listing
// decisions is served, on the System One path.
func TestSystemOneServesAServerAgentChildWhoseSpecIncludesDecisions(t *testing.T) {
	prov := &recordingProxyProvider{respBody: systemOneOKBody}
	srv := newServerAgentSystemOneSpecTestServer(t, prov, []string{routing.APIFlavorDecisions})

	rec := postSystemOne(t, srv, systemOneAgentBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/systemone" {
		t.Fatalf("ProxyNative calls = %d on %q, want 1 on /v1/systemone", prov.proxyCalls, prov.gotPath)
	}
}

// A vendor model of the same name is never chosen for a System One request: the
// shape's capability list keeps it off the vendor-account path, whose model match
// reads the name and never the flavor. The control proves the same account does
// serve the same model name on a plain chat request.
func TestSystemOneIsNeverVendorRoutable(t *testing.T) {
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	routes := routing.NewMemoryStore()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	seedAPIKeyAnthropicAccount(t, routes, cipher, apiKeyAnthropicOwnerID, now)
	resolver := routing.NewResolver(routes, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, nil, nil)
	tok := auth.Token{ID: "tok_vendor", UserID: apiKeyAnthropicOwnerID, Active: true, VendorAccess: auth.VendorAccess{All: true}}

	control, err := resolver.Resolve(context.Background(), tok, inference.Request{Model: apiKeyAnthropicModel, APIFlavor: routing.APIFlavorAnthropic})
	if err != nil || control.Provider != routing.ProviderVendorAnthropic {
		t.Fatalf("control Resolve = %+v, %v; want the vendor target (the fixture must be vendor-routable)", control, err)
	}

	shape := systemOneInferenceShape(apiKeyAnthropicModel)
	_, err = resolver.Resolve(context.Background(), tok, inference.Request{Model: shape.model, APIFlavor: shape.apiFlavor, RequiredCapabilities: shape.requiredCapabilities})
	if !errors.Is(err, routing.ErrNoModelRoute) {
		t.Fatalf("Resolve(System One shape) = %v, want ErrNoModelRoute (never a vendor target)", err)
	}
}

func TestSystemOneBodyOverTheCapIs413(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)
	srv.maxDecisionsBodyBytes = 64

	rec := postSystemOne(t, srv, systemOneBody)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "request.body_too_large")
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestSystemOneRequiresABearer(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postImagesWithHeaders(t, srv, "/v1/systemone", systemOneBody, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoSystemOneUpstreamCall(t, calls)
}

// THE BOUNDARY: a browser session cookie must NOT reach this endpoint. A 401
// proves auth refused: had the cookie been accepted the request would have
// failed later and differently, never with a 401.
func TestSystemOneRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_sysone", "sysone@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "sysone@example.test", "password-1")

	rec := postImagesWithHeaders(t, srv, "/v1/systemone", systemOneBody,
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a real session cookie: %s", rec.Code, rec.Body.String())
	}
}

// Unlike speech, the internal trusted-loopback pair is no way in: bearer only.
func TestSystemOneRefusesTheInternalLoopback(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postImagesWithHeaders(t, srv, "/v1/systemone", systemOneBody,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestSystemOneRejectsAGet(t *testing.T) {
	srv := newSystemOneCapableTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/systemone", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d, Allow = %q, want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
}

// There is no /openai alias: the endpoint is not an OpenAI one.
func TestSystemOneHasNoOpenAIAlias(t *testing.T) {
	srv := newSystemOneCapableTestServer(t, "http://127.0.0.1:1")
	rec := postImagesWithHeaders(t, srv, "/openai/v1/systemone", systemOneBody, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func getSystemOneModels(t *testing.T, srv *Server, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/systemone/models", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestSystemOneModelsListsOnlyCallableDecisionModels(t *testing.T) {
	up, _ := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := getSystemOneModels(t, srv, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Object string              `json:"object"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}
	want := []map[string]string{{"id": systemOneGatewayModel, "object": "model", "api": "systemone"}}
	if got.Object != "list" || !reflect.DeepEqual(got.Data, want) {
		t.Fatalf("listing = %+v, want only %v (decision-no, unprobed and openai-only models must be absent)", got, want)
	}
}

func TestSystemOneModelsIsAnEmptyArrayNotNull(t *testing.T) {
	srv := NewTestServer()

	rec := getSystemOneModels(t, srv, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("body = %s, want \"data\":[]", rec.Body.String())
	}
}

func TestSystemOneModelsIsBearerOnly(t *testing.T) {
	up, _ := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)

	if rec := getSystemOneModels(t, srv, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if rec := getSystemOneModels(t, srv, map[string]string{"Cookie": "op_session=x"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie: status = %d, want 401", rec.Code)
	}
	rec := postImagesWithHeaders(t, srv, "/v1/systemone/models", "{}", map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status = %d, want 405; body = %s", rec.Code, rec.Body.String())
	}
}
