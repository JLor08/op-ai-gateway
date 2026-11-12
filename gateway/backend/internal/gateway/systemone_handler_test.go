// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	// unicode a decode/re-encode would normalise. The relay is value-lossless
	// (only the model is rewritten, through the shared JSON rewrite), not
	// byte-exact, so the tests compare values, never bytes.
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

// TestSystemOneGateHoldsUnderTheServerOverride: a token pinned to the decision
// server (server_override) still reaches only a mapping with a (decision, yes)
// verdict there. The override's own candidate pool runs the capability gate, so
// a mapping on the pinned server without that verdict is refused -- as the
// override refuses any model the server does not offer for the request, 404
// server_override.model_unavailable -- and its upstream is never called, while
// the decision model on it is served (the control). The user owns the server,
// so the override survives its write-time check and the request's
// re-authorization and really is applied.
func TestSystemOneGateHoldsUnderTheServerOverride(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)
	if err := srv.Routes.SetServerOwners(context.Background(), "srv-sysone", []string{"usr_dev"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	setTokenServerOverride(t, srv, "tok_dev", "srv-sysone", false)

	for _, model := range []string{"no-decision", "unprobed-decision"} {
		rec := postSystemOne(t, srv, `{"model":"`+model+`","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s under the override: status = %d, want 404; body = %s", model, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "server_override.model_unavailable")
		requireNoSystemOneUpstreamCall(t, calls)
	}

	rec := postSystemOne(t, srv, systemOneBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("the decision model under the override: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if call := receivedSystemOneCall(t, calls); call.path != "/v1/systemone" {
		t.Fatalf("the upstream received %s, want /v1/systemone", call.path)
	}
}

// TestSystemOneDoesNotHonourAnAffinityPinToANonDecisionMapping: a session pin to
// the decision server's application, under the System One request's own key
// (token, model, the decisions flavor, the explicit session header -- the only
// key the resolve looks up), is not honoured for a mapping without a (decision,
// yes) verdict: the request is refused as not capable and the upstream never
// called. The refusal is non-destructive: the pin survives.
func TestSystemOneDoesNotHonourAnAffinityPinToANonDecisionMapping(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, systemOneOKBody)
	srv := newSystemOneCapableTestServer(t, up.URL)
	// A pin is honoured only for an application with an affinity TTL, so the
	// decision application gets one: the test is about the gate, not about that
	// precondition.
	ctx := context.Background()
	app, err := srv.Routes.ApplicationByID(ctx, "app-sysone")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AffinityTTLSeconds = 1800
	if err := srv.Routes.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	now := time.Now()
	pin := routing.RouteAffinity{
		ID: "aff_systemone_pin", APITokenID: "tok_dev", Model: "no-decision", APIFlavor: routing.APIFlavorDecisions, SessionID: "sess-pin",
		ApplicationID: "app-sysone", ServerID: "srv-sysone", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := srv.Routes.UpsertAffinity(ctx, pin); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}

	rec := postImagesWithHeaders(t, srv, "/v1/systemone", `{"model":"no-decision","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`,
		map[string]string{"Authorization": "Bearer dev-secret", sessionHeaderName: "sess-pin"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (the pinned mapping has no decision verdict); body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	requireNoSystemOneUpstreamCall(t, calls)
	key := routing.AffinityKey{APITokenID: "tok_dev", Model: "no-decision", APIFlavor: routing.APIFlavorDecisions, SessionID: "sess-pin"}
	if _, ok, err := srv.Routes.Affinity(ctx, key); err != nil || !ok {
		t.Fatalf("the affinity row did not survive the refusal (ok=%v, err=%v)", ok, err)
	}
}

// The /v1/systemone translate route (ADR-100): a System One request for a
// mapping that answers only OpenAI Decisions is translated to /v1/decisions and
// its answer back. The fixture is the decisions one (newDecisionsTestServer):
// dec-both, dec-s1 and dec-oai on a vLLM application.

// systemOneTranslateRequestFor is a System One request for model whose
// translation is answered by decisionsOAIAnswer: a noul "ok" and a choice "q1"
// (the answer's unnamed second entry matches it by position).
func systemOneTranslateRequestFor(model string) string {
	return `{"model":"` + model + `","state":"Ein Text: über alles","questions":{` +
		`"ok":{"type":"noul","instructions":"Is it ok?"},` +
		`"q1":{"type":"choice","instructions":"Mood?","criteria":{"calm":null,"angry":"upset"}}}}`
}

// The OpenAI Decisions body the translate route must send for
// systemOneTranslateRequestFor(decisionsModelOAI).
const systemOneTranslatedOAIBody = `{"model":"dec_oai_real","input":"Ein Text: über alles","questions":[` +
	`{"type":"predicate","name":"ok","instructions":"Is it ok?"},` +
	`{"type":"choice","name":"q1","instructions":"Mood?","choices":[{"value":"calm"},{"value":"angry","description":"upset"}]}]}`

// systemOneTranslatedAnswer is what the client gets for decisionsOAIAnswer.
const systemOneTranslatedAnswer = `{"answers":{` +
	`"ok":{"type":"noul","noul":0.9,"probabilities":{"yes":0.9,"no":0.1},"confidence":0.9},` +
	`"q1":{"type":"choice","choice":"calm","probabilities":{"calm":0.6,"angry":0.2},"confidence":0.5}},` +
	`"model":"dec-oai","usage":{"input_tokens":100,"output_tokens":2}}`

// postSystemOneAs posts body to /v1/systemone with secret.
func postSystemOneAs(t *testing.T, srv *Server, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postImagesWithHeaders(t, srv, "/v1/systemone", body, map[string]string{"Authorization": "Bearer " + secret})
}

// postSystemOneTranslate posts systemOneTranslateRequestFor(dec-oai) with
// secret to an auto-mode fixture whose upstream answers replies.
func postSystemOneTranslate(t *testing.T, replies map[string]decisionsReply, secret string) (*Server, <-chan systemOneUpstreamCall, *httptest.ResponseRecorder) {
	t.Helper()
	up, calls := newDecisionsUpstream(t, replies)
	srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, routing.EndpointModeAuto, routing.ProviderVLLM)
	return srv, calls, postSystemOneAs(t, srv, secret, systemOneTranslateRequestFor(decisionsModelOAI))
}

func TestSystemOneAutoWithOnlyDecisionOpenAITranslates(t *testing.T) {
	srv, calls, rec := postSystemOneTranslate(t, defaultDecisionsReplies(), "dev-secret")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("Content-Length = %q, want %d", cl, rec.Body.Len())
	}
	requireJSONEqual(t, "client body", rec.Body.Bytes(), systemOneTranslatedAnswer)
	call := receivedSystemOneCall(t, calls)
	if call.method != http.MethodPost || call.path != "/v1/decisions" {
		t.Fatalf("upstream saw %s %s, want POST /v1/decisions", call.method, call.path)
	}
	requireJSONEqual(t, "upstream body", call.body, systemOneTranslatedOAIBody)

	got := lastUsageEvent(t, srv)
	if got.ProviderPath != "/v1/decisions" || got.ReqPath != "/v1/systemone" || got.APIFlavor != apiFlavorSystemOne || got.Status != "success" || got.HTTPStatus != http.StatusOK {
		t.Fatalf("row = %+v, want a success decisions row on /v1/systemone with provider_path /v1/decisions", got)
	}
	if got.InputTokens != 50 || got.CachedTokens != 40 || got.CacheWriteTokens != 10 || got.OutputTokens != 2 {
		t.Fatalf("row tokens = in %d cached %d write %d out %d, want 50/40/10/2", got.InputTokens, got.CachedTokens, got.CacheWriteTokens, got.OutputTokens)
	}
}

// A mapping that answers System One keeps the Phase 1 relay under auto: the
// streaming copier, byte for byte, even when it also answers /v1/decisions.
func TestSystemOneAutoWithDecisionPassesThroughByteIdentical(t *testing.T) {
	for _, model := range []string{decisionsModelS1, decisionsModelBoth} {
		t.Run(model, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, routing.EndpointModeAuto, routing.ProviderVLLM)
			body := systemOneTranslateRequestFor(model)

			rec := postSystemOneAs(t, srv, "dev-secret", body)

			if rec.Code != http.StatusOK || rec.Body.String() != decisionsS1Answer || rec.Header().Get("Content-Length") != "" {
				t.Fatalf("status = %d body = %q Content-Length = %q, want 200 and the upstream's bytes through the copier", rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
			}
			call := receivedSystemOneCall(t, calls)
			if call.path != "/v1/systemone" {
				t.Fatalf("upstream path = %q, want /v1/systemone", call.path)
			}
			var sent map[string]any
			_ = json.Unmarshal([]byte(body), &sent)
			sent["model"] = map[string]string{decisionsModelS1: decisionsUpstreamS1, decisionsModelBoth: decisionsUpstreamBoth}[model]
			want, _ := json.Marshal(sent)
			requireJSONEqual(t, "upstream body", call.body, string(want))
			if got := lastUsageEvent(t, srv); got.ProviderPath != "/v1/systemone" || got.Status != "success" {
				t.Fatalf("row = %+v, want a success row on /v1/systemone", got)
			}
		})
	}
}

func TestSystemOneForcedModesNeedTheirVerdict(t *testing.T) {
	cases := []struct {
		name  string
		mode  routing.EndpointMode
		model string
	}{
		{"translate without decision_openai", routing.EndpointModeTranslate, decisionsModelS1},
		{"passthrough without decision", routing.EndpointModePassthrough, decisionsModelOAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, tc.mode, routing.ProviderVLLM)

			rec := postSystemOneAs(t, srv, "dev-secret", systemOneTranslateRequestFor(tc.model))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
			requireNoSystemOneUpstreamCall(t, calls)
			got := lastUsageEvent(t, srv)
			if got.ErrorCode != "routing.model_not_capable" || got.Status != "error" || got.HTTPStatus != http.StatusNotFound || got.ProviderPath != "" || got.ReqPath != "/v1/systemone" {
				t.Fatalf("row = %+v, want an upstream-less model_not_capable 404 row", got)
			}
		})
	}
}

// A forced translate with its verdict takes that route even when auto would
// pass through.
func TestSystemOneForcedTranslateOnAMappingWithBothVerdicts(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, routing.EndpointModeTranslate, routing.ProviderVLLM)

	rec := postSystemOneAs(t, srv, "dev-secret", systemOneTranslateRequestFor(decisionsModelBoth))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if call := receivedSystemOneCall(t, calls); call.path != "/v1/decisions" {
		t.Fatalf("upstream path = %q, want /v1/decisions", call.path)
	}
}

// On a kind with no OpenAI /v1/decisions (llama.cpp) a decision_openai verdict
// is no route: auto is not capable rather than translating.
func TestSystemOneAutoIgnoresDecisionOpenAIOnKindsWithoutTheRoute(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, routing.EndpointModeAuto, routing.ProviderLlamaCPP)

	rec := postSystemOneAs(t, srv, "dev-secret", systemOneTranslateRequestFor(decisionsModelOAI))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	requireNoSystemOneUpstreamCall(t, calls)
}

// A server_agent application or its runtime spec can disable System One per
// model; the route answers it before the verdicts matter.
func TestSystemOneDisabledServerAgentIs404(t *testing.T) {
	cases := []struct {
		name          string
		appSystemOne  routing.EndpointMode
		withSpec      bool
		specSystemOne routing.EndpointMode
	}{
		{"server_agent application disabled, no spec", routing.EndpointModeDisabled, false, ""},
		{"spec disables it over an auto application", routing.EndpointModeAuto, true, routing.EndpointModeDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: decisionsS1Answer}
			srv := newServerAgentDecisionsTestServer(t, prov, routing.EndpointModeAuto, tc.appSystemOne, tc.withSpec, routing.EndpointModeAuto, tc.specSystemOne)

			rec := postSystemOne(t, srv, `{"model":"agent-decider","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), codeSystemOneEndpointDisabled)
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			got := lastUsageEvent(t, srv)
			if got.ErrorCode != codeSystemOneEndpointDisabled || got.HTTPStatus != http.StatusNotFound || got.ReqPath != "/v1/systemone" || got.ProviderPath != "" {
				t.Fatalf("row = %+v, want an upstream-less systemone.endpoint_disabled 404 row", got)
			}
		})
	}
}

func TestSystemOneTranslatorRefusalIs400WithoutAnUpstreamCall(t *testing.T) {
	cases := []struct {
		name, body, code string
	}{
		{"field without a counterpart", `{"model":"` + decisionsModelOAI + `","state":"s","instructions":"be brief","questions":{"q":{"type":"noul"}}}`, "systemone.field_unsupported"},
		{"question type without a counterpart", `{"model":"` + decisionsModelOAI + `","state":"s","questions":{"q":{"type":"rank","instructions":"x"}}}`, "systemone.question_type_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionsTestServerWithModes(t, up.URL, routing.EndpointModeAuto, routing.EndpointModeAuto, routing.ProviderVLLM)

			rec := postSystemOneAs(t, srv, "dev-secret", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.code)
			requireNoSystemOneUpstreamCall(t, calls)
			got := lastUsageEvent(t, srv)
			if got.ErrorCode != tc.code || got.Status != "error" || got.HTTPStatus != http.StatusBadRequest || got.ReqPath != "/v1/systemone" || got.ProviderPath != "" {
				t.Fatalf("row = %+v, want an upstream-less %s 400 error row", got, tc.code)
			}
		})
	}
}

func TestSystemOneTranslateUpstreamRefusalIs422(t *testing.T) {
	const refused = `{"answers":[{"type":"refusal","name":"ok"},{"type":"refusal","name":"q1"}],"model":"dec_oai_real",` +
		`"usage":{"input_tokens":30,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":0},"output_tokens":1}}`
	srv, _, rec := postSystemOneTranslate(t, map[string]decisionsReply{"/v1/decisions": {http.StatusOK, refused}}, "dev-secret")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != codeSystemOneRefused || !strings.Contains(env.Error.Message, "ok, q1") {
		t.Fatalf("body = %s, want a systemone.refused envelope naming ok and q1", rec.Body.String())
	}
	got := lastUsageEvent(t, srv)
	if got.ErrorCode != codeSystemOneRefused || got.Status != "error" || got.HTTPStatus != http.StatusUnprocessableEntity || got.ProviderPath != "/v1/decisions" {
		t.Fatalf("row = %+v, want a systemone.refused 422 row on /v1/decisions", got)
	}
	// The upstream answered and was paid for: its tokens are recorded.
	if got.InputTokens != 10 || got.CachedTokens != 20 || got.OutputTokens != 1 {
		t.Fatalf("row tokens = in %d cached %d out %d, want 10/20/1", got.InputTokens, got.CachedTokens, got.OutputTokens)
	}
}

func TestSystemOneTranslateUpstreamErrorsAreNormalized(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{"400 error object", http.StatusBadRequest, `{"error":{"message":"too many questions","type":"invalid_request_error","param":null,"code":null}}`, "too many questions"},
		{"422 detail string", http.StatusUnprocessableEntity, `{"detail":"bad input"}`, "bad input"},
		{"501 adds guidance", http.StatusNotImplemented, `{"error":{"message":"no read strategy"}}`, systemOneNotImplementedGuidance},
		{"500 unknown shape", http.StatusInternalServerError, `<html>oops</html>`, "the OpenAI Decisions upstream answered 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, rec := postSystemOneTranslate(t, map[string]decisionsReply{"/v1/decisions": {tc.status, tc.body}}, "dev-secret")

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			var env struct {
				Error struct{ Code, Message, Type string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an error envelope: %v; %s", err, rec.Body.String())
			}
			if env.Error.Code != codeSystemOneUpstreamError || env.Error.Type != "upstream_error" || !strings.Contains(env.Error.Message, tc.wantMessage) || !strings.Contains(env.Error.Message, "OpenAI Decisions upstream") {
				t.Fatalf("envelope = %+v, want systemone.upstream_error/upstream_error naming the OpenAI Decisions upstream and carrying %q", env.Error, tc.wantMessage)
			}
			got := lastUsageEvent(t, srv)
			if got.Status != "error" || got.HTTPStatus != tc.status || got.ErrorCode != "upstream."+strconv.Itoa(tc.status) || got.ProviderPath != "/v1/decisions" {
				t.Fatalf("row = %+v, want an upstream.%d error row on /v1/decisions", got, tc.status)
			}
		})
	}
}

func TestSystemOneTranslateIncompleteAnswerIs502(t *testing.T) {
	const missing = `{"answers":[{"type":"predicate","name":"ok","probability":0.9}],"usage":{"input_tokens":5,"output_tokens":1}}`
	srv, _, rec := postSystemOneTranslate(t, map[string]decisionsReply{"/v1/decisions": {http.StatusOK, missing}}, "dev-secret")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeSystemOneUpstreamError)
	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.ErrorCode != codeSystemOneUpstreamError || got.HTTPStatus != http.StatusBadGateway || got.ProviderPath != "/v1/decisions" {
		t.Fatalf("row = %+v, want a systemone.upstream_error 502 row on /v1/decisions", got)
	}
}

func TestSystemOneTranslateNormalization(t *testing.T) {
	cases := []struct {
		secret string
		want   float64
	}{
		{decisionsNormSecret, 0.75}, // 0.6 / 0.8
		{"dev-secret", 0.5},         // the upstream's own
	}
	for _, tc := range cases {
		t.Run(tc.secret, func(t *testing.T) {
			_, _, rec := postSystemOneTranslate(t, defaultDecisionsReplies(), tc.secret)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Answers map[string]struct {
					Confidence float64 `json:"confidence"`
				} `json:"answers"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || math.Abs(got.Answers["q1"].Confidence-tc.want) > 1e-9 {
				t.Fatalf("body = %s, want q1 confidence %v", rec.Body.String(), tc.want)
			}
		})
	}
}
