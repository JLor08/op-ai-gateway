// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
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

// The gateway model names of the decisions fixture, one per verdict state, and
// the upstream model ids they map to (different, so the model rewrite shows).
const (
	decisionsModelBoth = "dec-both" // decision: yes and decision_openai: yes
	decisionsModelS1   = "dec-s1"   // decision: yes only
	decisionsModelOAI  = "dec-oai"  // decision_openai: yes only

	decisionsUpstreamBoth = "dec_both_real"
	decisionsUpstreamS1   = "dec_s1_real"
	decisionsUpstreamOAI  = "dec_oai_real"

	// decisionsNormSecret is the bearer of the token that opted into confidence
	// normalization; dev-secret is the one that did not.
	decisionsNormSecret = "norm-secret"
)

// decisionsRequestFor is an OpenAI Decisions request for model: a named
// predicate and an unnamed choice, plus a prompt_cache_key the translator drops.
func decisionsRequestFor(model string) string {
	return `{"model":"` + model + `","input":"Ein Text: über alles","questions":[` +
		`{"type":"predicate","name":"ok","instructions":"Is it ok?"},` +
		`{"type":"choice","instructions":"Mood?","choices":[{"value":"calm"},{"value":"angry","description":"upset"}]}` +
		`],"prompt_cache_key":"k1"}`
}

// The System One body the translate route must send for decisionsRequestFor.
const decisionsTranslatedS1Body = `{"model":"dec_s1_real","state":"Ein Text: über alles","questions":{` +
	`"ok":{"type":"noul","instructions":"Is it ok?"},` +
	`"q1":{"type":"choice","instructions":"Mood?","criteria":{"calm":null,"angry":"upset"}}}}`

// decisionsS1Answer is a System One upstream answer to decisionsTranslatedS1Body.
const decisionsS1Answer = `{"id":"so-1","object":"systemone","answers":{` +
	`"ok":{"type":"noul","noul":0.9},` +
	`"q1":{"type":"choice","choice":"calm","confidence":0.5,"probabilities":{"calm":0.6,"angry":0.2}}},` +
	`"usage":{"input_tokens":120,"output_tokens":3}}`

// decisionsTranslatedAnswer is what the client gets for decisionsS1Answer.
const decisionsTranslatedAnswer = `{"answers":[` +
	`{"type":"predicate","name":"ok","probability":0.9},` +
	`{"type":"choice","name":null,"choice":"calm","confidence":0.5,"probabilities":[{"value":"calm","probability":0.6},{"value":"angry","probability":0.2}]}` +
	`],"model":"dec-s1","usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":123}}`

// decisionsOAIAnswer is a native /v1/decisions answer, with cache details.
const decisionsOAIAnswer = `{"answers":[` +
	`{"type":"predicate","name":"ok","probability":0.9},` +
	`{"type":"choice","name":null,"choice":"calm","confidence":0.5,"probabilities":[{"value":"calm","probability":0.6},{"value":"angry","probability":0.2}]}` +
	`],"model":"dec_oai_real","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":102}}`

// decisionsReply is one canned upstream answer.
type decisionsReply struct {
	status int
	body   string
}

// newDecisionsUpstream starts an upstream answering each path with its reply
// (404 for a path without one). calls receives every request it saw.
func newDecisionsUpstream(t *testing.T, replies map[string]decisionsReply) (*httptest.Server, <-chan systemOneUpstreamCall) {
	t.Helper()
	calls := make(chan systemOneUpstreamCall, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- systemOneUpstreamCall{method: r.Method, path: r.URL.Path, body: b}
		reply, ok := replies[r.URL.Path]
		if !ok {
			reply = decisionsReply{status: http.StatusNotFound, body: `{"detail":"Not Found"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(up.Close)
	return up, calls
}

// defaultDecisionsReplies answers both decision endpoints successfully.
func defaultDecisionsReplies() map[string]decisionsReply {
	return map[string]decisionsReply{
		"/v1/systemone": {http.StatusOK, decisionsS1Answer},
		"/v1/decisions": {http.StatusOK, decisionsOAIAnswer},
	}
}

// newDecisionsTestServer builds a server with one vLLM application declaring the
// decisions flavor with the given decisions mode, pointing at upstreamURL, and
// three mappings (decisionsModelBoth/S1/OAI). Two tokens: dev-secret (no
// normalization) and decisionsNormSecret (normalization on).
func newDecisionsTestServer(t *testing.T, upstreamURL string, mode routing.EndpointMode) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	for _, tk := range []struct {
		id, secret string
		normalize  bool
	}{{"tok_dev", "dev-secret", false}, {"tok_norm", decisionsNormSecret, true}} {
		if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: tk.id, UserID: "usr_dev", Name: tk.id, Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, DecisionsConfidenceNormalize: tk.normalize, CreatedAt: now, UpdatedAt: now}, tk.secret); err != nil {
			t.Fatalf("CreatePlainToken %s: %v", tk.id, err)
		}
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-dec", Name: "Decisions Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-dec", ServerID: "srv-dec", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorDecisions}, DecisionsMode: mode, SystemOneMode: routing.EndpointModePassthrough, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for _, m := range []struct {
		id, gateway, upstream string
		caps                  []string
	}{
		{"route-dec-both", decisionsModelBoth, decisionsUpstreamBoth, []string{routing.CapabilityDecision, routing.CapabilityDecisionOpenAI}},
		{"route-dec-s1", decisionsModelS1, decisionsUpstreamS1, []string{routing.CapabilityDecision}},
		{"route-dec-oai", decisionsModelOAI, decisionsUpstreamOAI, []string{routing.CapabilityDecisionOpenAI}},
	} {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-dec", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
		rows := make([]routing.CapabilityRow, 0, len(m.caps))
		for _, c := range m.caps {
			rows = append(rows, routing.CapabilityRow{Capability: c, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now})
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, m.id, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-dec", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

func postDecisions(t *testing.T, srv *Server, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postImagesWithHeaders(t, srv, "/v1/decisions", body, map[string]string{"Authorization": "Bearer " + secret})
}

// requireJSONEqual fails unless got and want decode to the same JSON value.
func requireJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s is not JSON: %v; %s", what, err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: the expected value is not JSON: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %s\nwant %s", what, got, want)
	}
}

// choiceConfidence reads the confidence of the second (choice) answer of an
// OpenAI-dialect body.
func choiceConfidence(t *testing.T, body []byte) float64 {
	t.Helper()
	var got struct {
		Answers []struct {
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &got); err != nil || len(got.Answers) < 2 {
		t.Fatalf("body = %s, want an answers array of two", body)
	}
	return got.Answers[1].Confidence
}

func TestValidateDecisionsRequest(t *testing.T) {
	const qs = `"questions":[{"type":"predicate","instructions":"x"}]`
	cases := []struct {
		name string
		body string
		want string // error code, "" for valid
	}{
		{"valid", `{"model":"m","input":"s",` + qs + `}`, ""},
		{"no model", `{"input":"s",` + qs + `}`, decisionsModelRequired},
		{"model not a string", `{"model":3,` + qs + `}`, decisionsModelRequired},
		{"blank model", `{"model":"  ",` + qs + `}`, decisionsModelRequired},
		{"no questions", `{"model":"m","input":"s"}`, decisionsQuestionsRequired},
		{"questions is an object", `{"model":"m","questions":{}}`, decisionsQuestionsRequired},
		{"questions is a filled object", `{"model":"m","questions":{"q":{"type":"noul"}}}`, decisionsQuestionsRequired},
		{"questions is empty", `{"model":"m","questions":[]}`, decisionsQuestionsRequired},
		{"questions null", `{"model":"m","questions":null}`, decisionsQuestionsRequired},
		{"stream true", `{"model":"m","stream":true,` + qs + `}`, decisionsStreamUnsupported},
		{"stream is a string", `{"model":"m","stream":"false",` + qs + `}`, decisionsStreamUnsupported},
		{"stream false", `{"model":"m","stream":false,` + qs + `}`, ""},
		{"stream null", `{"model":"m","stream":null,` + qs + `}`, ""},
		{"model checked before questions", `{"questions":[]}`, decisionsModelRequired},
		{"not json", `nope`, decisionsModelRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDecisionsRequest([]byte(tc.body))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("error = %+v, want nil", err)
			case tc.want != "" && (err == nil || err.Code != tc.want):
				t.Fatalf("error = %+v, want code %s", err, tc.want)
			}
		})
	}
}

// The codes are API surface: clients branch on them.
func TestDecisionsErrorCodesAreStable(t *testing.T) {
	got := []string{decisionsModelRequired, decisionsQuestionsRequired, decisionsStreamUnsupported, codeDecisionsEndpointDisabled, codeSystemOneEndpointDisabled, codeDecisionsUpstreamError, codeSystemOneUpstreamError}
	want := []string{"decisions.model_required", "decisions.questions_required", "decisions.stream_unsupported", "decisions.endpoint_disabled", "systemone.endpoint_disabled", "decisions.upstream_error", "systemone.upstream_error"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v", got, want)
	}
}

func TestDecisionsInferenceShape(t *testing.T) {
	shape := decisionsInferenceShape("m")
	if shape.apiFlavor != routing.APIFlavorOpenAIDecisions || shape.endpoint != endpointDecisions || shape.model != "m" || shape.stream ||
		!reflect.DeepEqual(shape.requiredCapabilities, []string{routing.CapabilityAnyDecision}) {
		t.Fatalf("shape = %+v, want openai_decisions on endpointDecisions requiring [any_decision]", shape)
	}
}

func TestDecisionsAutoWithDecisionOpenAIPassesThrough(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelBoth))

	if rec.Code != http.StatusOK || rec.Body.String() != decisionsOAIAnswer {
		t.Fatalf("status = %d body = %q, want 200 and the upstream's bytes", rec.Code, rec.Body.String())
	}
	call := receivedSystemOneCall(t, calls)
	if call.path != "/v1/decisions" {
		t.Fatalf("upstream path = %q, want /v1/decisions", call.path)
	}
	// Value-equal to the client's body with the model rewritten.
	var sent map[string]any
	_ = json.Unmarshal([]byte(decisionsRequestFor(decisionsModelBoth)), &sent)
	sent["model"] = decisionsUpstreamBoth
	want, _ := json.Marshal(sent)
	requireJSONEqual(t, "upstream body", call.body, string(want))

	got := lastUsageEvent(t, srv)
	if got.ProviderPath != "/v1/decisions" || got.ReqPath != "/v1/decisions" || got.APIFlavor != routing.APIFlavorOpenAIDecisions || got.Status != "success" {
		t.Fatalf("row = %+v, want a success openai_decisions row with provider_path /v1/decisions", got)
	}
	if got.InputTokens != 50 || got.CachedTokens != 40 || got.CacheWriteTokens != 10 || got.OutputTokens != 2 {
		t.Fatalf("row tokens = in %d cached %d write %d out %d, want 50/40/10/2", got.InputTokens, got.CachedTokens, got.CacheWriteTokens, got.OutputTokens)
	}
}

func TestDecisionsAutoWithOnlyDecisionTranslates(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelS1))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("Content-Length = %q, want %d", cl, rec.Body.Len())
	}
	requireJSONEqual(t, "client body", rec.Body.Bytes(), decisionsTranslatedAnswer)
	call := receivedSystemOneCall(t, calls)
	if call.path != "/v1/systemone" {
		t.Fatalf("upstream path = %q, want /v1/systemone", call.path)
	}
	requireJSONEqual(t, "upstream body", call.body, decisionsTranslatedS1Body)

	got := lastUsageEvent(t, srv)
	if got.ProviderPath != "/v1/systemone" || got.ReqPath != "/v1/decisions" || got.Status != "success" || got.InputTokens != 120 || got.OutputTokens != 3 {
		t.Fatalf("row = %+v, want a success row on /v1/systemone with 120/3 tokens", got)
	}
}

func TestDecisionsAliasUnderOpenAIPrefix(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postImagesWithHeaders(t, srv, "/openai/v1/decisions", decisionsRequestFor(decisionsModelBoth), map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if call := receivedSystemOneCall(t, calls); call.path != "/v1/decisions" {
		t.Fatalf("upstream path = %q, want /v1/decisions", call.path)
	}
}

func TestDecisionsForcedModesNeedTheirVerdict(t *testing.T) {
	cases := []struct {
		name  string
		mode  routing.EndpointMode
		model string
	}{
		{"translate without decision", routing.EndpointModeTranslate, decisionsModelOAI},
		{"passthrough without decision_openai", routing.EndpointModePassthrough, decisionsModelS1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionsTestServer(t, up.URL, tc.mode)

			rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(tc.model))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
			requireNoSystemOneUpstreamCall(t, calls)
			got := lastUsageEvent(t, srv)
			if got.ErrorCode != "routing.model_not_capable" || got.Status != "error" || got.HTTPStatus != http.StatusNotFound || got.ProviderPath != "" {
				t.Fatalf("row = %+v, want an upstream-less model_not_capable 404 row", got)
			}
		})
	}
}

// A forced mode with its verdict takes that route even when auto would pick
// the other one.
func TestDecisionsForcedTranslateOnAMappingWithBothVerdicts(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeTranslate)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelBoth))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if call := receivedSystemOneCall(t, calls); call.path != "/v1/systemone" {
		t.Fatalf("upstream path = %q, want /v1/systemone", call.path)
	}
}

// An ordinary application whose decisions mode is disabled is no candidate
// (ADR-098 candidacy), so a model it alone serves has no route.
func TestDecisionsDisabledOrdinaryApplicationIsNoCandidate(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeDisabled)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelBoth))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	requireNoSystemOneUpstreamCall(t, calls)

	// System One on the same application still serves.
	rec = postSystemOne(t, srv, `{"model":"`+decisionsModelBoth+`","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("System One status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// newServerAgentDecisionsTestServer seeds a server_agent application (decisions
// flavor, appDecisions/appSystemOne modes) whose one mapping "agent-decider"
// carries both verdicts; withSpec adds a runtime spec with the spec modes.
func newServerAgentDecisionsTestServer(t *testing.T, prov provider.Client, appDecisions, appSystemOne routing.EndpointMode, withSpec bool, specDecisions, specSystemOne routing.EndpointMode) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-agent-dec", Name: "Agent Decisions", Domain: "agent-dec.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://agent-dec.example.test:8081", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-agent-dec", ServerID: "srv-agent-dec", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorDecisions}, DecisionsMode: appDecisions, SystemOneMode: appSystemOne, Priority: 10, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-agent-dec", ApplicationID: "app-agent-dec", GatewayModelName: "agent-decider", AppModelName: "agent-decider", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-agent-dec", []routing.CapabilityRow{
		{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if withSpec {
		if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-agent-dec", MappingID: "route-agent-dec", APIFlavors: []string{routing.APIFlavorDecisions}, DecisionsMode: specDecisions, SystemOneMode: specSystemOne, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-agent-dec", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

const agentDecisionsBody = `{"model":"agent-decider","input":"s","questions":[{"type":"predicate","instructions":"x"}]}`

func TestDecisionsDisabledEndpointIs404(t *testing.T) {
	cases := []struct {
		name          string
		appDecisions  routing.EndpointMode
		withSpec      bool
		specDecisions routing.EndpointMode
	}{
		{"server_agent application disabled, no spec", routing.EndpointModeDisabled, false, ""},
		{"spec disables it over an auto application", routing.EndpointModeAuto, true, routing.EndpointModeDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: decisionsOAIAnswer}
			srv := newServerAgentDecisionsTestServer(t, prov, tc.appDecisions, routing.EndpointModePassthrough, tc.withSpec, tc.specDecisions, routing.EndpointModePassthrough)

			rec := postDecisions(t, srv, "dev-secret", agentDecisionsBody)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), codeDecisionsEndpointDisabled)
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			got := lastUsageEvent(t, srv)
			if got.ErrorCode != codeDecisionsEndpointDisabled || got.HTTPStatus != http.StatusNotFound || got.ReqPath != "/v1/decisions" || got.ProviderPath != "" {
				t.Fatalf("row = %+v, want an upstream-less decisions.endpoint_disabled 404 row", got)
			}
		})
	}
}

// The positive control: the same server_agent fixture with a spec that keeps
// the endpoint passes through to /v1/decisions.
func TestDecisionsServerAgentSpecPassthroughIsServed(t *testing.T) {
	prov := &recordingProxyProvider{respBody: decisionsOAIAnswer}
	srv := newServerAgentDecisionsTestServer(t, prov, routing.EndpointModeDisabled, routing.EndpointModePassthrough, true, routing.EndpointModePassthrough, routing.EndpointModePassthrough)

	rec := postDecisions(t, srv, "dev-secret", agentDecisionsBody)

	if rec.Code != http.StatusOK || prov.proxyCalls != 1 || prov.gotPath != "/v1/decisions" {
		t.Fatalf("status = %d, calls = %d on %q, want 200 and one call on /v1/decisions; body = %s", rec.Code, prov.proxyCalls, prov.gotPath, rec.Body.String())
	}
}

func TestSystemOneDisabledBySpecIs404(t *testing.T) {
	prov := &recordingProxyProvider{respBody: decisionsS1Answer}
	srv := newServerAgentDecisionsTestServer(t, prov, routing.EndpointModeAuto, routing.EndpointModePassthrough, true, routing.EndpointModeAuto, routing.EndpointModeDisabled)

	rec := postSystemOne(t, srv, `{"model":"agent-decider","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeSystemOneEndpointDisabled)
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
	}
	got := lastUsageEvent(t, srv)
	if got.ErrorCode != codeSystemOneEndpointDisabled || got.HTTPStatus != http.StatusNotFound || got.ReqPath != "/v1/systemone" {
		t.Fatalf("row = %+v, want a systemone.endpoint_disabled 404 row", got)
	}
}

func TestDecisionsTranslatorRefusalIs400WithoutAnUpstreamCall(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)
	body := `{"model":"` + decisionsModelS1 + `","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/cat.png"}]}],"questions":[{"type":"predicate","instructions":"cat?"}]}`

	rec := postDecisions(t, srv, "dev-secret", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "decisions.input_unsupported")
	requireNoSystemOneUpstreamCall(t, calls)
	got := lastUsageEvent(t, srv)
	if got.ErrorCode != "decisions.input_unsupported" || got.Status != "error" || got.HTTPStatus != http.StatusBadRequest || got.ReqPath != "/v1/decisions" {
		t.Fatalf("row = %+v, want a decisions.input_unsupported 400 error row", got)
	}
}

func TestDecisionsTranslateUpstreamErrorsAreNormalized(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{"501 adds guidance", http.StatusNotImplemented, `{"error":{"message":"The model does not support the structured decision API","type":"NotImplementedError","code":501}}`, "The model does not support the structured decision API"},
		{"400 error object", http.StatusBadRequest, `{"error":{"message":"too many questions","type":"BadRequestError","param":null,"code":400}}`, "too many questions"},
		{"422 detail list", http.StatusUnprocessableEntity, `{"detail":[{"loc":["body","questions"],"msg":"field required","type":"missing"}]}`, "field required"},
		{"404 detail string", http.StatusNotFound, `{"detail":"Not Found"}`, "Not Found"},
		{"500 unknown shape", http.StatusInternalServerError, `<html>oops</html>`, "the System One upstream answered 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireTranslatedUpstreamError(t, tc.status, tc.body, tc.wantMessage)
		})
	}
}

// requireTranslatedUpstreamError sends a translated request to an upstream
// answering status and body, and requires the gateway envelope under that
// status carrying wantMessage, and its error row.
func requireTranslatedUpstreamError(t *testing.T, status int, body, wantMessage string) {
	t.Helper()
	up, _ := newDecisionsUpstream(t, map[string]decisionsReply{"/v1/systemone": {status, body}})
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelS1))

	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, status, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code, Message, Type string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an error envelope: %v; %s", err, rec.Body.String())
	}
	if env.Error.Code != codeDecisionsUpstreamError || env.Error.Type != "upstream_error" || !strings.Contains(env.Error.Message, wantMessage) {
		t.Fatalf("envelope = %+v, want decisions.upstream_error/upstream_error carrying %q", env.Error, wantMessage)
	}
	if status == http.StatusNotImplemented && !strings.Contains(env.Error.Message, decisionsNotImplementedGuidance) {
		t.Fatalf("message = %q, want the 501 guidance", env.Error.Message)
	}
	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.HTTPStatus != status || got.ErrorCode != "upstream."+strconv.Itoa(status) || got.ProviderPath != "/v1/systemone" {
		t.Fatalf("row = %+v, want an upstream.%d error row on /v1/systemone", got, status)
	}
}

func TestDecisionsPassthroughUpstreamErrorIsVerbatim(t *testing.T) {
	const upstreamErr = `{"error":{"message":"name must be a string","type":"BadRequestError","param":null,"code":400}}`
	for _, secret := range []string{"dev-secret", decisionsNormSecret} {
		t.Run(secret, func(t *testing.T) {
			up, _ := newDecisionsUpstream(t, map[string]decisionsReply{"/v1/decisions": {http.StatusBadRequest, upstreamErr}})
			srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

			rec := postDecisions(t, srv, secret, decisionsRequestFor(decisionsModelBoth))

			if rec.Code != http.StatusBadRequest || rec.Body.String() != upstreamErr {
				t.Fatalf("status = %d body = %q, want 400 and the identical bytes", rec.Code, rec.Body.String())
			}
			if got := lastUsageEvent(t, srv); got.ErrorCode != "upstream.400" || got.ProviderPath != "/v1/decisions" {
				t.Fatalf("row = %+v, want an upstream.400 row on /v1/decisions", got)
			}
		})
	}
}

func TestDecisionsTranslateIncompleteAnswerIs502(t *testing.T) {
	const missing = `{"answers":{"ok":{"type":"noul","noul":0.9}},"usage":{"input_tokens":5,"output_tokens":1}}`
	up, _ := newDecisionsUpstream(t, map[string]decisionsReply{"/v1/systemone": {http.StatusOK, missing}})
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelS1))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeDecisionsUpstreamError)
	got := lastUsageEvent(t, srv)
	if got.Status != "error" || got.ErrorCode != codeDecisionsUpstreamError || got.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("row = %+v, want a decisions.upstream_error 502 row", got)
	}
}

func TestDecisionsOversizeUpstreamBodyIs502(t *testing.T) {
	cases := []struct {
		name, secret, model, path string
	}{
		{"translate", "dev-secret", decisionsModelS1, "/v1/systemone"},
		{"normalized passthrough", decisionsNormSecret, decisionsModelBoth, "/v1/decisions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			big := `{"answers":{},"pad":"` + strings.Repeat("x", 4096) + `"}`
			up, _ := newDecisionsUpstream(t, map[string]decisionsReply{tc.path: {http.StatusOK, big}})
			srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)
			srv.maxDecisionsBodyBytes = 1024 // the request (~400 bytes) fits; the answer does not

			rec := postDecisions(t, srv, tc.secret, decisionsRequestFor(tc.model))

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), codeDecisionsUpstreamError)
			if strings.Contains(rec.Body.String(), "xxxx") {
				t.Fatalf("body = %s, want nothing of the upstream's answer", rec.Body.String())
			}
		})
	}
}

// postDecisionsOn posts decisionsRequestFor(model) with secret to an auto-mode
// fixture whose upstream answers replies.
func postDecisionsOn(t *testing.T, replies map[string]decisionsReply, secret, model string) (*Server, *httptest.ResponseRecorder) {
	t.Helper()
	up, _ := newDecisionsUpstream(t, replies)
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)
	return srv, postDecisions(t, srv, secret, decisionsRequestFor(model))
}

func requireChoiceConfidence(t *testing.T, rec *httptest.ResponseRecorder, want float64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := choiceConfidence(t, rec.Body.Bytes()); math.Abs(got-want) > 1e-9 {
		t.Fatalf("confidence = %v, want %v", got, want)
	}
}

func TestDecisionsNormalizationOnTranslate(t *testing.T) {
	_, rec := postDecisionsOn(t, defaultDecisionsReplies(), decisionsNormSecret, decisionsModelS1)
	requireChoiceConfidence(t, rec, 0.75) // 0.6 / 0.8
}

func TestDecisionsNormalizationOffTranslateKeepsTheUpstreamConfidence(t *testing.T) {
	_, rec := postDecisionsOn(t, defaultDecisionsReplies(), "dev-secret", decisionsModelS1)
	requireChoiceConfidence(t, rec, 0.5)
}

func TestDecisionsNormalizationOnPassthrough(t *testing.T) {
	srv, rec := postDecisionsOn(t, defaultDecisionsReplies(), decisionsNormSecret, decisionsModelBoth)
	requireChoiceConfidence(t, rec, 0.75)
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("Content-Length = %q, want %d (the buffered path)", cl, rec.Body.Len())
	}
	got := lastUsageEvent(t, srv)
	if got.InputTokens != 50 || got.CachedTokens != 40 || got.CacheWriteTokens != 10 || got.OutputTokens != 2 || got.ProviderPath != "/v1/decisions" {
		t.Fatalf("row = %+v, want 50/40/10/2 on /v1/decisions", got)
	}
}

func TestDecisionsNormalizationOffPassthroughIsTheStreamingCopier(t *testing.T) {
	_, rec := postDecisionsOn(t, defaultDecisionsReplies(), "dev-secret", decisionsModelBoth)
	if rec.Body.String() != decisionsOAIAnswer || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("body = %q, Content-Length = %q, want the upstream bytes with no Content-Length", rec.Body.String(), rec.Header().Get("Content-Length"))
	}
}

func TestDecisionsNormalizationRelaysANonJSONSuccessUnchanged(t *testing.T) {
	_, rec := postDecisionsOn(t, map[string]decisionsReply{"/v1/decisions": {http.StatusOK, "not json"}}, decisionsNormSecret, decisionsModelBoth)
	if rec.Code != http.StatusOK || rec.Body.String() != "not json" {
		t.Fatalf("status = %d body = %q, want 200 and the bytes unchanged", rec.Code, rec.Body.String())
	}
}

const (
	systemOneChoiceAnswer  = `{"model":"dec_both_real","answers":{"c":{"type":"choice","choice":"a","confidence":0.1,"probabilities":{"a":0.3,"b":0.1}}},"usage":{"input_tokens":7,"output_tokens":1}}`
	systemOneChoiceRequest = `{"model":"` + decisionsModelBoth + `","state":"s","questions":{"c":{"type":"choice","instructions":"x","criteria":{"a":null,"b":null}}}}`
)

// postSystemOneOn posts systemOneChoiceRequest with secret to an auto-mode
// fixture whose System One upstream answers status and body; capBytes > 0
// lowers the decisions body cap.
func postSystemOneOn(t *testing.T, status int, body, secret string, capBytes int64) (*Server, *httptest.ResponseRecorder) {
	t.Helper()
	up, _ := newDecisionsUpstream(t, map[string]decisionsReply{"/v1/systemone": {status, body}})
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)
	if capBytes > 0 {
		srv.maxDecisionsBodyBytes = capBytes
	}
	return srv, postImagesWithHeaders(t, srv, "/v1/systemone", systemOneChoiceRequest, map[string]string{"Authorization": "Bearer " + secret})
}

func TestSystemOneNormalizationOn(t *testing.T) {
	srv, rec := postSystemOneOn(t, http.StatusOK, systemOneChoiceAnswer, decisionsNormSecret, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Answers map[string]struct {
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || math.Abs(got.Answers["c"].Confidence-0.75) > 1e-9 {
		t.Fatalf("body = %s, want confidence 0.75 (0.3 / 0.4)", rec.Body.String())
	}
	if row := lastUsageEvent(t, srv); row.InputTokens != 7 || row.OutputTokens != 1 || row.Status != "success" || row.ProviderPath != "/v1/systemone" {
		t.Fatalf("row = %+v, want 7/1 success on /v1/systemone", row)
	}
}

func TestSystemOneNormalizationOffIsByteIdenticalThroughTheCopier(t *testing.T) {
	_, rec := postSystemOneOn(t, http.StatusOK, systemOneChoiceAnswer, "dev-secret", 0)
	if rec.Code != http.StatusOK || rec.Body.String() != systemOneChoiceAnswer || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("status = %d body = %q Content-Length = %q, want the upstream bytes with no Content-Length", rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
	}
}

func TestSystemOneNormalizationRelaysAnUpstreamErrorVerbatim(t *testing.T) {
	const upstreamErr = `{"detail":[{"msg":"bad question"}]}`
	_, rec := postSystemOneOn(t, http.StatusUnprocessableEntity, upstreamErr, decisionsNormSecret, 0)
	if rec.Code != http.StatusUnprocessableEntity || rec.Body.String() != upstreamErr {
		t.Fatalf("status = %d body = %q, want 422 and the identical bytes", rec.Code, rec.Body.String())
	}
}

func TestSystemOneNormalizationOversizeAnswerIsSystemOneUpstreamError(t *testing.T) {
	big := `{"answers":{},"pad":"` + strings.Repeat("x", 4096) + `"}`
	_, rec := postSystemOneOn(t, http.StatusOK, big, decisionsNormSecret, 1024)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeSystemOneUpstreamError)
}

func TestDecisionsAnswersAValidationErrorWith400(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postDecisions(t, srv, "dev-secret", `{"model":"dec-both","input":"s","questions":[],"stream":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), decisionsQuestionsRequired)
	rec = postDecisions(t, srv, "dev-secret", `{"model":"dec-both","input":"s","questions":[{"type":"predicate","instructions":"x"}],"stream":true}`)
	requireErrorCode(t, rec.Body.String(), decisionsStreamUnsupported)
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestDecisionsBodyOverTheCapIs413(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)
	srv.maxDecisionsBodyBytes = 64

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor(decisionsModelBoth))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "request.body_too_large")
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestDecisionsRequiresABearer(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postImagesWithHeaders(t, srv, "/v1/decisions", decisionsRequestFor(decisionsModelBoth), nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestDecisionsRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_dec", "dec@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "dec@example.test", "password-1")

	for _, path := range []string{"/v1/decisions", "/openai/v1/decisions"} {
		rec := postImagesWithHeaders(t, srv, path, decisionsRequestFor(decisionsModelBoth),
			map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 from a real session cookie: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestDecisionsRefusesTheInternalLoopback(t *testing.T) {
	up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionsTestServer(t, up.URL, routing.EndpointModeAuto)

	rec := postImagesWithHeaders(t, srv, "/v1/decisions", decisionsRequestFor(decisionsModelBoth),
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoSystemOneUpstreamCall(t, calls)
}

func TestDecisionsRejectsAGet(t *testing.T) {
	srv := newDecisionsTestServer(t, "http://127.0.0.1:1", routing.EndpointModeAuto)
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d, Allow = %q, want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
}

// A model with neither verdict never passes the pre-resolve gate.
func TestDecisionsRefusesAModelWithoutADecisionVerdict(t *testing.T) {
	up, calls := newSystemOneUpstream(t, http.StatusOK, decisionsOAIAnswer)
	srv := newSystemOneCapableTestServer(t, up.URL)

	rec := postDecisions(t, srv, "dev-secret", decisionsRequestFor("no-decision"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	requireNoSystemOneUpstreamCall(t, calls)
}
