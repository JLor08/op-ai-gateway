// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// Resolution side of the Claude model aliases (issue #225): a client that sends
// a generated anthropic-local-<hash> id as its model reaches the real gateway
// model behind it, through the one shared preflight, on the native passthrough
// path and on the translate path alike. These tests drive real requests through
// ServeHTTP and take the alias id from the discovery listing the client itself
// would read, so a resolvable alias is proven to be an offered one. The
// precedence/single-resolution rules in isolation are tested in
// inference_handlers_test.go, and the derivation in internal/portal.

const (
	aliasResSecret      = "alias-res-on"      // user token, generated aliases on
	aliasResPlainSecret = "alias-res-off"     // user token, generated aliases off
	aliasResSvcSecret   = "alias-res-svc-key" // service token (allowlist), set per test
)

// aliasResProvider is a provider that serves every endpoint kind and records what
// reached it: the bodies handed to ProxyNative (native passthrough) and the
// requests handed to Complete/CompleteStream (translate). Its replies carry a
// tool call whenever the request offered tools.
type aliasResProvider struct {
	nativePaths  []string
	nativeBodies [][]byte
	completeReqs []inference.Request
	streamReqs   []inference.Request
	targets      []routing.Target
}

func (p *aliasResProvider) calls() int {
	return len(p.nativeBodies) + len(p.completeReqs) + len(p.streamReqs)
}

func (p *aliasResProvider) Complete(_ context.Context, target routing.Target, req inference.Request) (provider.Response, error) {
	p.completeReqs = append(p.completeReqs, req)
	p.targets = append(p.targets, target)
	resp := provider.Response{Text: "translated", Usage: inference.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}, FinishReason: "stop"}
	if len(req.Tools) > 0 {
		resp.Text = ""
		resp.ToolCalls = []inference.ToolCall{{ID: "toolu_1", Name: req.Tools[0].Name, Arguments: `{"city":"Berlin"}`}}
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

func (p *aliasResProvider) CompleteStream(_ context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	p.streamReqs = append(p.streamReqs, req)
	p.targets = append(p.targets, target)
	finish := "stop"
	event := inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "translated"}
	if len(req.Tools) > 0 {
		finish = "tool_calls"
		event = inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &inference.ToolCall{ID: "toolu_1", Name: req.Tools[0].Name, Arguments: `{"city":"Berlin"}`}}
	}
	if err := emit(event); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, FinishReason: finish, Usage: &inference.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}})
}

const (
	aliasResNativeToolJSON = `{"id":"msg_up","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Berlin"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`
	aliasResNativeTextJSON = `{"id":"msg_up","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"native"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`
	aliasResNativeToolSSE  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Berlin\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	aliasResNativeTextSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"native\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	aliasResNativeResponsesJSON = `{"id":"resp_up","object":"response","output_text":"native","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`
)

func (p *aliasResProvider) ProxyNative(_ context.Context, target routing.Target, path string, body []byte, _ string) (*provider.ProxyResponse, error) {
	p.nativePaths = append(p.nativePaths, path)
	p.nativeBodies = append(p.nativeBodies, append([]byte(nil), body...))
	p.targets = append(p.targets, target)
	var probe struct {
		Stream bool            `json:"stream"`
		Tools  json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &probe)
	withTools := len(probe.Tools) > 0
	contentType, reply := "application/json", aliasResNativeTextJSON
	switch {
	case !strings.HasSuffix(path, "/messages"):
		reply = aliasResNativeResponsesJSON
	case probe.Stream && withTools:
		contentType, reply = "text/event-stream", aliasResNativeToolSSE
	case probe.Stream:
		contentType, reply = "text/event-stream", aliasResNativeTextSSE
	case withTools:
		reply = aliasResNativeToolJSON
	}
	return &provider.ProxyResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
}

// claudeAliasResolutionFixture is a gateway over three Messages-capable local
// models (gw-model, other-model and locked-model, the last one locked by
// model_settings) and the two user tokens aliasResSecret (generated aliases on)
// and aliasResPlainSecret (off).
type claudeAliasResolutionFixture struct {
	srv    *Server
	prov   *aliasResProvider
	routes *routing.MemoryStore
	// alias is gw-model's generated alias id, read from the discovery listing the
	// setting-on token gets; otherAlias is other-model's.
	alias, otherAlias string
}

// newClaudeAliasResolutionServer seeds the fixture with both the Responses and
// the Messages endpoint of its application in `mode` (passthrough => native
// passthrough, translate => the translate path). The application type is
// server_agent, like newNativeModeTestServerOn, so no per-model runtime spec is
// needed to reach either mode.
func newClaudeAliasResolutionServer(t *testing.T, mode routing.EndpointMode) *claudeAliasResolutionFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_alr", Email: "alr@example.test", DisplayName: "Alr", Role: "member", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	for _, tok := range []struct {
		id, secret string
		aliases    bool
	}{
		{"tok_alr_on", aliasResSecret, true},
		{"tok_alr_off", aliasResPlainSecret, false},
	} {
		tokens.AddPlainToken(auth.Token{ID: tok.id, UserID: "usr_alr", Name: tok.id, Active: true, Scopes: []string{"gateway:use"}, ClaudeModelAliases: tok.aliases}, tok.secret)
	}

	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-alr", Name: "Alias Upstream", Domain: "alr.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://alr.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-alr", ServerID: "srv-alr", Type: routing.ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: mode, MessagesMode: mode, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for _, m := range []struct{ gateway, upstream string }{
		{"gw-model", "upstream-model"}, {"other-model", "other-upstream"}, {"locked-model", "locked-upstream"},
	} {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map-" + m.gateway, ApplicationID: "app-alr", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.gateway, err)
		}
	}
	if err := routeStore.UpsertModelSetting(ctx, routing.ModelSetting{GatewayModelName: "locked-model", Visibility: "locked", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelSetting: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-alr", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	prov := &aliasResProvider{}
	recorder := usage.NewRecorder()
	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
	f := &claudeAliasResolutionFixture{srv: srv, prov: prov, routes: routeStore}
	byID := anthropicModelEntryByID(t, srv, aliasResSecret)
	f.alias, _ = aliasEntryFor(t, byID, "gw-model")
	f.otherAlias, _ = aliasEntryFor(t, byID, "other-model")
	return f
}

// sha256AliasID derives an alias id the way the contract documents it (prefix +
// the first 12 hex of the SHA-256 of the gateway id), for ids the listing
// withholds. TestClaudeAliasFixtureDerivationMatchesDiscovery pins it against
// the listing.
func sha256AliasID(gatewayID string) string {
	sum := sha256.Sum256([]byte(gatewayID))
	return claudeListingAliasPrefix + hex.EncodeToString(sum[:])[:12]
}

// TestClaudeAliasFixtureDerivationMatchesDiscovery keeps the helper above honest:
// it must reproduce the alias ids the listing offers, so an id it computes for a
// withheld model is the id a client would have used.
func TestClaudeAliasFixtureDerivationMatchesDiscovery(t *testing.T) {
	f := newClaudeAliasResolutionServer(t, routing.EndpointModeTranslate)
	if got := sha256AliasID("gw-model"); got != f.alias {
		t.Fatalf("sha256AliasID(gw-model) = %s, but discovery offers %s", got, f.alias)
	}
	if got := sha256AliasID("other-model"); got != f.otherAlias {
		t.Fatalf("sha256AliasID(other-model) = %s, but discovery offers %s", got, f.otherAlias)
	}
	byID := anthropicModelEntryByID(t, f.srv, aliasResSecret)
	if _, listed := byID[sha256AliasID("locked-model")]; listed {
		t.Fatal("the locked model is offered an alias")
	}
}

// aliasMessagesBody builds an Anthropic Messages request body for model.
func aliasMessagesBody(model string, stream, tools bool) string {
	body := `{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}]`
	if stream {
		body += `,"stream":true`
	}
	if tools {
		body += `,"tools":[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	}
	return body + `}`
}

// requireOneUsageEvent returns the single usage event the request recorded.
func requireOneUsageEvent(t *testing.T, srv *Server) usage.Event {
	t.Helper()
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %+v, want exactly one", events)
	}
	return events[0]
}

// TestClaudeAliasResolvesOnTheMessagesPaths: the alias a setting-on token is
// offered reaches the real model behind it on /anthropic/v1/messages, native
// passthrough and translate, buffered and streaming, with and without tools:
// the upstream is dispatched to the TARGET's mapping, the request keeps the alias
// the client sent, and the usage event pairs the two.
func TestClaudeAliasResolvesOnTheMessagesPaths(t *testing.T) {
	for _, mode := range []routing.EndpointMode{routing.EndpointModePassthrough, routing.EndpointModeTranslate} {
		for _, stream := range []bool{false, true} {
			for _, tools := range []bool{false, true} {
				name := string(mode) + map[bool]string{false: "/buffered", true: "/stream"}[stream] + map[bool]string{false: "/no-tools", true: "/tools"}[tools]
				t.Run(name, func(t *testing.T) {
					f := newClaudeAliasResolutionServer(t, mode)
					rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, stream, tools))
					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
					}
					if f.prov.calls() != 1 {
						t.Fatalf("provider calls = %d, want 1", f.prov.calls())
					}
					if mode == routing.EndpointModePassthrough {
						requireNativeAliasDispatch(t, f, rec, stream, tools)
					} else {
						requireTranslateAliasDispatch(t, f, rec, stream, tools)
					}
					ev := requireOneUsageEvent(t, f.srv)
					if ev.Model != "gw-model" || ev.RequestedModel != f.alias || ev.Stream != stream || ev.Status != "success" {
						t.Fatalf("usage event = (model %q, requested %q, stream %v, status %q), want (gw-model, %s, %v, success)", ev.Model, ev.RequestedModel, ev.Stream, ev.Status, f.alias, stream)
					}
					if ev.RouteID != "map-gw-model" {
						t.Fatalf("usage RouteID = %q, want the target's mapping map-gw-model", ev.RouteID)
					}
				})
			}
		}
	}
}

// requireNativeAliasDispatch checks a native-passthrough dispatch: the raw body
// reached ProxyNative with its model rewritten to the TARGET's upstream name
// (never the alias), everything else untouched, and the reply came back verbatim.
func requireNativeAliasDispatch(t *testing.T, f *claudeAliasResolutionFixture, rec *httptest.ResponseRecorder, stream, tools bool) {
	t.Helper()
	if len(f.prov.nativeBodies) != 1 || f.prov.nativePaths[0] != "/v1/messages" {
		t.Fatalf("native calls = %d (paths %v), want one on /v1/messages", len(f.prov.nativeBodies), f.prov.nativePaths)
	}
	var sent struct {
		Model  string          `json:"model"`
		Stream bool            `json:"stream"`
		Tools  json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(f.prov.nativeBodies[0], &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent.Model != "upstream-model" {
		t.Fatalf("upstream model = %q, want upstream-model (gw-model's mapping, not the alias)", sent.Model)
	}
	if sent.Stream != stream || (len(sent.Tools) > 0) != tools {
		t.Fatalf("upstream body lost stream/tools: %s", f.prov.nativeBodies[0])
	}
	want := aliasResNativeTextJSON
	switch {
	case stream && tools:
		want = aliasResNativeToolSSE
	case stream:
		want = aliasResNativeTextSSE
	case tools:
		want = aliasResNativeToolJSON
	}
	if rec.Body.String() != want {
		t.Fatalf("client body = %q, want the upstream reply verbatim", rec.Body.String())
	}
}

// requireTranslateAliasDispatch checks a translate dispatch: the provider got the
// parsed request for the TARGET's mapping (its upstream model name, on the
// target's route, with the alias kept as the requested name) and the client got
// the Anthropic envelope of its reply.
func requireTranslateAliasDispatch(t *testing.T, f *claudeAliasResolutionFixture, rec *httptest.ResponseRecorder, stream, tools bool) {
	t.Helper()
	var got inference.Request
	switch {
	case stream && len(f.prov.streamReqs) == 1:
		got = f.prov.streamReqs[0]
	case !stream && len(f.prov.completeReqs) == 1:
		got = f.prov.completeReqs[0]
	default:
		t.Fatalf("provider calls: complete %d, stream %d, native %d; want exactly one of the requested kind", len(f.prov.completeReqs), len(f.prov.streamReqs), len(f.prov.nativeBodies))
	}
	if got.Model != "upstream-model" || got.RequestedModel != f.alias {
		t.Fatalf("provider request (Model, RequestedModel) = (%q, %q), want (upstream-model, %s): gw-model's mapping, not the alias", got.Model, got.RequestedModel, f.alias)
	}
	if f.prov.targets[0].RouteID != "map-gw-model" {
		t.Fatalf("dispatched to route %q, want the target's mapping map-gw-model", f.prov.targets[0].RouteID)
	}
	if (len(got.Tools) > 0) != tools {
		t.Fatalf("provider request tools = %v, want tools=%v", got.Tools, tools)
	}
	body := rec.Body.String()
	if tools && !strings.Contains(body, `"tool_use"`) {
		t.Fatalf("client reply carries no tool_use block: %s", body)
	}
	if !tools && strings.Contains(body, `"tool_use"`) {
		t.Fatalf("client reply carries an unexpected tool_use block: %s", body)
	}
	if stream && ((!tools && !strings.Contains(body, "event: content_block_delta")) || !strings.Contains(body, "event: message_stop")) {
		t.Fatalf("client reply is not a complete Anthropic event stream: %s", body)
	}
}

// TestClaudeAliasResolvesOnTheOtherInferenceEndpoints: resolution lives in the
// shared preflight, not in one handler, so the alias works the same on the
// endpoints a Claude-compatible client may also use: OpenAI chat completions
// (translate) and the Responses endpoint (native passthrough).
func TestClaudeAliasResolvesOnTheOtherInferenceEndpoints(t *testing.T) {
	t.Run("chat completions translate", func(t *testing.T) {
		f := newClaudeAliasResolutionServer(t, routing.EndpointModeTranslate)
		rec := postBearer(t, f.srv, aliasResSecret, "/v1/chat/completions", `{"model":"`+f.alias+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if len(f.prov.completeReqs) != 1 || f.prov.completeReqs[0].Model != "upstream-model" || f.prov.completeReqs[0].RequestedModel != f.alias {
			t.Fatalf("provider requests = %+v, want one for gw-model's mapping (upstream-model, requested %s)", f.prov.completeReqs, f.alias)
		}
		if ev := requireOneUsageEvent(t, f.srv); ev.Model != "gw-model" || ev.RequestedModel != f.alias {
			t.Fatalf("usage event model pair = (%q, %q), want (gw-model, %s)", ev.Model, ev.RequestedModel, f.alias)
		}
	})
	t.Run("responses native passthrough", func(t *testing.T) {
		f := newClaudeAliasResolutionServer(t, routing.EndpointModePassthrough)
		rec := postBearer(t, f.srv, aliasResSecret, "/v1/responses", `{"model":"`+f.alias+`","input":"hi"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if len(f.prov.nativeBodies) != 1 || !strings.Contains(string(f.prov.nativeBodies[0]), `"model":"upstream-model"`) {
			t.Fatalf("native bodies = %q, want one with model rewritten to upstream-model", f.prov.nativeBodies)
		}
		if ev := requireOneUsageEvent(t, f.srv); ev.Model != "gw-model" || ev.RequestedModel != f.alias {
			t.Fatalf("usage event model pair = (%q, %q), want (gw-model, %s)", ev.Model, ev.RequestedModel, f.alias)
		}
	})
}

// TestClaudeAliasPrecedenceOnTheWire: through real requests, an explicit
// model_override_map entry for the alias id wins over the generated alias, the
// generated alias wins over the catch-all and is resolved once (the catch-all is
// not applied to its target), and an ordinary unknown name still gets the
// catch-all exactly as before.
func TestClaudeAliasPrecedenceOnTheWire(t *testing.T) {
	post := func(f *claudeAliasResolutionFixture, secret, model string) usage.Event {
		t.Helper()
		rec := postBearer(t, f.srv, secret, "/anthropic/v1/messages", aliasMessagesBody(model, false, false))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST model %q: status = %d, body = %s", model, rec.Code, rec.Body.String())
		}
		return requireOneUsageEvent(t, f.srv)
	}
	for _, mode := range []routing.EndpointMode{routing.EndpointModePassthrough, routing.EndpointModeTranslate} {
		t.Run(string(mode)+"/explicit rule wins over the generated alias", func(t *testing.T) {
			f := newClaudeAliasResolutionServer(t, mode)
			tokens := f.srv.Tokens.(*auth.TokenStore)
			tokens.AddPlainToken(auth.Token{
				ID: "tok_alr_rule", UserID: "usr_alr", Name: "rule", Active: true, Scopes: []string{"gateway:use"}, ClaudeModelAliases: true,
				ModelOverrideRules: map[string]auth.ModelOverrideRule{f.alias: {To: "other-model"}},
			}, "alias-res-rule")
			ev := post(f, "alias-res-rule", f.alias)
			if ev.Model != "other-model" || ev.RequestedModel != f.alias {
				t.Fatalf("usage model pair = (%q, %q), want (other-model, %s): the manual rule owns the id", ev.Model, ev.RequestedModel, f.alias)
			}
		})
		t.Run(string(mode)+"/generated alias beats the catch-all and is resolved once", func(t *testing.T) {
			f := newClaudeAliasResolutionServer(t, mode)
			tokens := f.srv.Tokens.(*auth.TokenStore)
			tokens.AddPlainToken(auth.Token{
				ID: "tok_alr_catch", UserID: "usr_alr", Name: "catch", Active: true, Scopes: []string{"gateway:use"}, ClaudeModelAliases: true,
				ModelOverride: "other-model",
			}, "alias-res-catch")
			ev := post(f, "alias-res-catch", f.alias)
			if ev.Model != "gw-model" || ev.RequestedModel != f.alias {
				t.Fatalf("usage model pair = (%q, %q), want (gw-model, %s): the alias resolves to its own target, not through the catch-all", ev.Model, ev.RequestedModel, f.alias)
			}
		})
		t.Run(string(mode)+"/an ordinary name still gets the catch-all", func(t *testing.T) {
			f := newClaudeAliasResolutionServer(t, mode)
			tokens := f.srv.Tokens.(*auth.TokenStore)
			tokens.AddPlainToken(auth.Token{
				ID: "tok_alr_catch", UserID: "usr_alr", Name: "catch", Active: true, Scopes: []string{"gateway:use"}, ClaudeModelAliases: true,
				ModelOverride: "other-model",
			}, "alias-res-catch")
			ev := post(f, "alias-res-catch", "claude-sonnet-4-5")
			if ev.Model != "other-model" || ev.RequestedModel != "claude-sonnet-4-5" {
				t.Fatalf("usage model pair = (%q, %q), want (other-model, claude-sonnet-4-5)", ev.Model, ev.RequestedModel)
			}
		})
	}
}

// requireUnknownModelFailure asserts a request failed exactly like a request for
// a name nobody serves: the same status and error code the model "no-such-model"
// gets, and nothing was dispatched upstream.
func requireUnknownModelFailure(t *testing.T, f *claudeAliasResolutionFixture, secret, model string, rec *httptest.ResponseRecorder, dispatchedBefore int) {
	t.Helper()
	control := postBearer(t, f.srv, secret, "/anthropic/v1/messages", aliasMessagesBody("no-such-model", false, false))
	if control.Code < 400 {
		t.Fatalf("control request for an unknown model succeeded: %d %s", control.Code, control.Body.String())
	}
	if rec.Code != control.Code || errorBodyOf(t, rec) != errorBodyOf(t, control) {
		t.Fatalf("model %q: status %d code %q, want the unknown-model outcome %d %q", model, rec.Code, errorBodyOf(t, rec), control.Code, errorBodyOf(t, control))
	}
	if f.prov.calls() != dispatchedBefore {
		t.Fatalf("model %q reached an upstream (%d provider calls, was %d): an unresolvable alias must not fall back to any model", model, f.prov.calls(), dispatchedBefore)
	}
}

// TestClaudeAliasDoesNotResolveWithoutTheSetting: a token without the setting
// cannot use an alias another token was offered, and gets the plain
// unknown-model outcome: no silent fallback to the target or any other model.
func TestClaudeAliasDoesNotResolveWithoutTheSetting(t *testing.T) {
	for _, mode := range []routing.EndpointMode{routing.EndpointModePassthrough, routing.EndpointModeTranslate} {
		t.Run(string(mode), func(t *testing.T) {
			f := newClaudeAliasResolutionServer(t, mode)
			rec := postBearer(t, f.srv, aliasResPlainSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false))
			requireUnknownModelFailure(t, f, aliasResPlainSecret, f.alias, rec, 0)
		})
	}
}

// TestClaudeAliasStopsResolvingWhenDisabledOrWithdrawn: aliases are not stored,
// so the lifecycle is the live state. After the token's setting is switched off,
// or after the target is hidden by an operator, the alias a client persisted
// earlier stops resolving at once and follows the plain unknown-model outcome.
func TestClaudeAliasStopsResolvingWhenDisabledOrWithdrawn(t *testing.T) {
	t.Run("setting switched off", func(t *testing.T) {
		f := newClaudeAliasResolutionServer(t, routing.EndpointModeTranslate)
		if rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false)); rec.Code != http.StatusOK {
			t.Fatalf("before: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		dispatched := f.prov.calls()
		tokens := f.srv.Tokens.(*auth.TokenStore)
		tok, ok := tokens.LookupBearer("Bearer " + aliasResSecret)
		if !ok {
			t.Fatal("token not found")
		}
		tok.ClaudeModelAliases = false
		tokens.UpdateToken(tok)

		rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false))
		requireUnknownModelFailure(t, f, aliasResSecret, f.alias, rec, dispatched)
	})
	t.Run("target hidden by an operator", func(t *testing.T) {
		f := newClaudeAliasResolutionServer(t, routing.EndpointModeTranslate)
		if rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false)); rec.Code != http.StatusOK {
			t.Fatalf("before: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		dispatched := f.prov.calls()
		now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
		if err := f.routes.UpsertModelSetting(context.Background(), routing.ModelSetting{GatewayModelName: "gw-model", Visibility: "hidden", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertModelSetting: %v", err)
		}
		rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false))
		requireUnknownModelFailure(t, f, aliasResSecret, f.alias, rec, dispatched)
		// The other model's alias is untouched.
		if rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(f.otherAlias, false, false)); rec.Code != http.StatusOK {
			t.Fatalf("other alias: status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

// TestClaudeAliasNeverReachesAModelTheTokenIsNotOffered: the alias id of a model
// discovery withholds (here one model_settings locks) is a plain unknown name,
// not a way in.
func TestClaudeAliasNeverReachesAModelTheTokenIsNotOffered(t *testing.T) {
	f := newClaudeAliasResolutionServer(t, routing.EndpointModeTranslate)
	locked := sha256AliasID("locked-model")
	rec := postBearer(t, f.srv, aliasResSecret, "/anthropic/v1/messages", aliasMessagesBody(locked, false, false))
	requireUnknownModelFailure(t, f, aliasResSecret, locked, rec, 0)
}

// TestClaudeAliasAccessChecksRunOnTheEffectiveTarget: the service-account
// allowlist is judged against the model the alias resolves to. A service token
// that may use gw-model reaches it through the alias although the alias id is not
// on its list, and a token whose list names only the alias, or only another
// model, is refused with model.not_allowed before anything is dispatched.
func TestClaudeAliasAccessChecksRunOnTheEffectiveTarget(t *testing.T) {
	for _, mode := range []routing.EndpointMode{routing.EndpointModePassthrough, routing.EndpointModeTranslate} {
		t.Run(string(mode), func(t *testing.T) {
			f := newClaudeAliasResolutionServer(t, mode)
			tokens := f.srv.Tokens.(*auth.TokenStore)
			addSvc := func(id string, allowed []string) string {
				secret := aliasResSvcSecret + "-" + id
				addServiceToken(tokens, secret, auth.Token{ID: "tok_" + id, ServiceID: "svc_" + id, ServiceName: id, ClaudeModelAliases: true, AllowedModels: allowed})
				return secret
			}
			// The target is on the allowlist: served, with the model pair recorded.
			rec := postBearer(t, f.srv, addSvc("target", []string{"gw-model"}), "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false))
			if rec.Code != http.StatusOK {
				t.Fatalf("allowlist [gw-model]: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if ev := requireOneUsageEvent(t, f.srv); ev.Model != "gw-model" || ev.RequestedModel != f.alias {
				t.Fatalf("usage model pair = (%q, %q), want (gw-model, %s)", ev.Model, ev.RequestedModel, f.alias)
			}
			dispatched := f.prov.calls()

			// The alias id on the list grants nothing, and neither does another model.
			for name, allowed := range map[string][]string{"alias only": {f.alias}, "other model only": {"other-model"}} {
				rec := postBearer(t, f.srv, addSvc(strings.ReplaceAll(name, " ", "-"), allowed), "/anthropic/v1/messages", aliasMessagesBody(f.alias, false, false))
				if rec.Code != http.StatusForbidden || errorBodyOf(t, rec) != "model.not_allowed" {
					t.Fatalf("allowlist %q: status = %d code %q, want 403 model.not_allowed (the target is not allowed)", name, rec.Code, errorBodyOf(t, rec))
				}
				if f.prov.calls() != dispatched {
					t.Fatalf("allowlist %q: a refused request reached an upstream", name)
				}
			}
		})
	}
}
