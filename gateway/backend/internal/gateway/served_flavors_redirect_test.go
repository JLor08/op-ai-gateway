// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// seedServedFlavorsApplication creates app on a server of its own (id
// "srv-"+app.ID, healthy, with a telemetry sample) and one active mapping
// "route-"+model per model on it.
func seedServedFlavorsApplication(t *testing.T, routes routing.Store, app routing.Application, models ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	app.ServerID = "srv-" + app.ID
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: app.ServerID, Name: app.ID, Domain: app.ID + ".example.test", Provider: routing.ProviderVLLM, Endpoint: "http://" + app.ID + ".example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", app.ServerID, err)
	}
	app.Port, app.Scheme, app.Priority, app.Weight, app.TimeoutMS = 8000, "http", 10, 50, 30000
	app.Status, app.CreatedAt, app.UpdatedAt = routing.ServerStatusActive, now, now
	if err := routes.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication %s: %v", app.ID, err)
	}
	for _, model := range models {
		if err := routes.CreateMapping(ctx, routing.ModelMapping{ID: "route-" + model, ApplicationID: app.ID, GatewayModelName: model, AppModelName: model, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", model, err)
		}
	}
	if err := routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: app.ServerID, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry %s: %v", app.ServerID, err)
	}
}

// seedServedFlavorsSpec stores the runtime spec of mapping "route-"+model.
func seedServedFlavorsSpec(t *testing.T, routes routing.Store, model string, flavors []string, responsesMode, messagesMode routing.EndpointMode) {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err := routes.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{ID: "spec-" + model, MappingID: "route-" + model, APIFlavors: flavors, ResponsesMode: responsesMode, MessagesMode: messagesMode, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec %s: %v", model, err)
	}
}

// seedServedFlavorsGroup stores an active model group called name (id
// "grp-"+name) whose members are the given model names, in that priority
// order.
func seedServedFlavorsGroup(t *testing.T, routes routing.Store, name string, members ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err := routes.CreateModelGroup(ctx, routing.ModelGroup{ID: "grp-" + name, GatewayModelName: name, DisplayName: name, Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModelGroup %s: %v", name, err)
	}
	rows := make([]routing.GroupMember, 0, len(members))
	for i, member := range members {
		rows = append(rows, routing.GroupMember{MemberGatewayName: member, Priority: i})
	}
	if err := routes.SetGroupMembers(ctx, "grp-"+name, rows); err != nil {
		t.Fatalf("SetGroupMembers %s: %v", name, err)
	}
}

// newServedFlavorsRedirectTestServer seeds one model per shape the redirect's
// two sets tell apart, and the dev token ("dev-secret") with the caller's
// redirect and override settings in tok. Two ordinary applications
// [openai, anthropic]:
//
//   - text-model: both coding-agent endpoints in translate mode, so it is
//     served on every text endpoint -- the redirect target;
//   - msg-off: the messages endpoint disabled.
//
// And one server_agent application that declares every flavor, with both
// application modes passthrough, whose three children each carry a spec:
//
//   - flux-child: [openai_images], both modes disabled (the
//     stable-diffusion.cpp child);
//   - text-child: [openai], both modes translate (a spec without anthropic);
//   - agent-msg-off: [openai, anthropic], responses translate, messages
//     disabled.
func newServedFlavorsRedirectTestServer(t *testing.T, prov provider.Client, tok store.TokenRecord) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	tok.ID, tok.UserID, tok.Name, tok.Status = "tok_dev", "usr_dev", "Dev Token", store.TokenStatusActive
	tok.Scopes, tok.CreatedAt, tok.UpdatedAt = `["gateway:use"]`, now, now
	if err := directory.CreatePlainToken(ctx, tok, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	textFlavors := []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}
	seedServedFlavorsApplication(t, routeStore, routing.Application{ID: "app-text", Type: routing.ProviderVLLM, APIFlavors: textFlavors, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: routing.EndpointModeTranslate}, "text-model")
	seedServedFlavorsApplication(t, routeStore, routing.Application{ID: "app-msg-off", Type: routing.ProviderVLLM, APIFlavors: textFlavors, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: routing.EndpointModeDisabled}, "msg-off")
	seedServedFlavorsApplication(t, routeStore, routing.Application{ID: "app-agent", Type: routing.ProviderServerAgent, APIFlavors: serverAgentAllFlavors, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough}, "flux-child", "text-child", "agent-msg-off")
	seedServedFlavorsSpec(t, routeStore, "flux-child", []string{routing.APIFlavorOpenAIImages}, routing.EndpointModeDisabled, routing.EndpointModeDisabled)
	seedServedFlavorsSpec(t, routeStore, "text-child", []string{routing.APIFlavorOpenAI}, routing.EndpointModeTranslate, routing.EndpointModeTranslate)
	seedServedFlavorsSpec(t, routeStore, "agent-msg-off", textFlavors, routing.EndpointModeTranslate, routing.EndpointModeDisabled)
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// chatBodyFor is a chat completions body for model.
func chatBodyFor(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

// messagesBodyFor is a /v1/messages body whose model the routing probe reads.
func messagesBodyFor(model string) string {
	return `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
}

// requireRedirectedTo asserts that the request was redirected to want and
// served there by translation: 200, one translate call and no native one,
// and a usage row whose effective Model is want while RequestedModel keeps
// the client's own name.
func requireRedirectedTo(t *testing.T, srv *Server, prov *countingTranslateProvider, rec *httptest.ResponseRecorder, requested, want string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (redirected to %s); body = %s", rec.Code, want, rec.Body.String())
	}
	if prov.completeCalls+prov.streamCalls != 1 || prov.proxyCalls != 0 {
		t.Fatalf("calls: complete %d, stream %d, proxy %d; want one translate call and no proxy call",
			prov.completeCalls, prov.streamCalls, prov.proxyCalls)
	}
	got := lastUsageEvent(t, srv)
	if got.Model != want || got.RequestedModel != requested {
		t.Fatalf("usage Model/RequestedModel = %q/%q, want %q/%q", got.Model, got.RequestedModel, want, requested)
	}
}

// TestRedirectTakesATextRequestForAnImagesOnlyChildElsewhere: an images-only
// child fails the openai flavor half, so for a text request it does not exist,
// the same as a model whose application lacks openai -- and the narrow default
// redirects it. That holds for /v1/chat/completions and /v1/responses alike,
// and when the child is the effective model only because an override row or
// the catch-all targets it.
func TestRedirectTakesATextRequestForAnImagesOnlyChildElsewhere(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		requested string
		rules     string
		catchAll  string
	}{
		{"chat completions", "/v1/chat/completions", chatBodyFor("flux-child"), "flux-child", "", ""},
		{"responses", "/v1/responses", `{"model":"flux-child","input":"hi"}`, "flux-child", "", ""},
		{"chat completions through an override row onto the child", "/v1/chat/completions", chatBodyFor("my-images"), "my-images", `{"my-images":{"to":"flux-child"}}`, ""},
		{"chat completions through the catch-all onto the child", "/v1/chat/completions", chatBodyFor("gpt-4o"), "gpt-4o", "", "flux-child"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &countingTranslateProvider{}
			srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{
				UnknownModelRedirect: true, UnknownModelFallback: "text-model",
				ModelOverrideMap: tc.rules, ModelOverride: tc.catchAll,
			})

			rec := postBearer(t, srv, "dev-secret", tc.path, tc.body)

			requireRedirectedTo(t, srv, prov, rec, tc.requested, "text-model")
		})
	}
}

// TestRedirectSkipsAnImagesOnlyLastUsedModel: a token that generated images
// last carries the images-only child as its last-used model. For a text
// request that candidate is not callable, so the chain moves on to the
// fallback instead of answering 404 about a model the client never named.
func TestRedirectSkipsAnImagesOnlyLastUsedModel(t *testing.T) {
	prov := &countingTranslateProvider{}
	srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, LastUsedModel: "flux-child", UnknownModelFallback: "text-model"})

	rec := postBearer(t, srv, "dev-secret", "/v1/chat/completions", chatBodyFor("no-such-model"))

	requireRedirectedTo(t, srv, prov, rec, "no-such-model", "text-model")
}

// TestRedirectTakesAMessagesRequestForAChildWhoseSpecLacksAnthropicElsewhere:
// a child whose spec lacks anthropic fails the anthropic flavor half, so it
// does not exist for /v1/messages, and the narrow default redirects it.
func TestRedirectTakesAMessagesRequestForAChildWhoseSpecLacksAnthropicElsewhere(t *testing.T) {
	prov := &countingTranslateProvider{}
	srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, UnknownModelFallback: "text-model"})

	rec := postBearer(t, srv, "dev-secret", "/v1/messages", messagesBodyFor("text-child"))

	requireRedirectedTo(t, srv, prov, rec, "text-child", "text-model")
}

// TestMessagesDisabledModelIsRedirectedOnlyUnderBlocked: a model whose
// effective messages mode is disabled passes the anthropic flavor half but
// not the served rule, so it exists under anthropic without being callable --
// the "exists but you cannot call it" shape. The narrow default leaves it
// alone and the client keeps its 404, while UnknownModelRedirectBlocked
// redirects it. Both shapes count: an ordinary application's messages mode,
// and an agent child's spec.
func TestMessagesDisabledModelIsRedirectedOnlyUnderBlocked(t *testing.T) {
	models := []struct {
		name    string
		refusal string
	}{
		{"msg-off", "routing.no_model_route"},
		{"agent-msg-off", "messages.endpoint_disabled"},
	}
	for _, m := range models {
		t.Run(m.name+"/narrow default keeps the model", func(t *testing.T) {
			prov := &countingTranslateProvider{}
			srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, UnknownModelFallback: "text-model"})

			rec := postBearer(t, srv, "dev-secret", "/v1/messages", messagesBodyFor(m.name))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), m.refusal)
			if n := prov.calls(); n != 0 {
				t.Fatalf("provider calls = %d, want 0", n)
			}
			if got := lastUsageEvent(t, srv).Model; got != m.name {
				t.Fatalf("usage Model = %q, want the client's own %q (no redirect)", got, m.name)
			}
		})
		t.Run(m.name+"/blocked redirects it", func(t *testing.T) {
			prov := &countingTranslateProvider{}
			srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "text-model"})

			rec := postBearer(t, srv, "dev-secret", "/v1/messages", messagesBodyFor(m.name))

			requireRedirectedTo(t, srv, prov, rec, m.name, "text-model")
		})
	}
}

// TestRedirectSkipsAMessagesDisabledLastUsedModel: a messages-disabled model
// is not callable under anthropic, so as the last-used candidate of a
// /v1/messages request it is skipped for the fallback -- an ordinary
// application's and an agent child's alike. As the fallback itself, with no
// last-used model, it is skipped too: the chain runs out and declines, and the
// client keeps its own 404 for the name it sent.
func TestRedirectSkipsAMessagesDisabledLastUsedModel(t *testing.T) {
	for _, model := range []string{"msg-off", "agent-msg-off"} {
		t.Run(model+"/as the last-used model", func(t *testing.T) {
			prov := &countingTranslateProvider{}
			srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, LastUsedModel: model, UnknownModelFallback: "text-model"})

			rec := postBearer(t, srv, "dev-secret", "/v1/messages", messagesBodyFor("no-such-model"))

			requireRedirectedTo(t, srv, prov, rec, "no-such-model", "text-model")
		})
		t.Run(model+"/as the fallback", func(t *testing.T) {
			prov := &countingTranslateProvider{}
			srv := newServedFlavorsRedirectTestServer(t, prov, store.TokenRecord{UnknownModelRedirect: true, UnknownModelFallback: model})

			rec := postBearer(t, srv, "dev-secret", "/v1/messages", messagesBodyFor("no-such-model"))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
			if n := prov.calls(); n != 0 {
				t.Fatalf("provider calls = %d, want 0", n)
			}
			if got := lastUsageEvent(t, srv).Model; got != "no-such-model" {
				t.Fatalf("usage Model = %q, want the client's own %q (no redirect)", got, "no-such-model")
			}
		})
	}
}

// TestImagesRedirectsATextChildOfAMixedApplication: the text child of an
// application that declares openai_images has a spec without it, so it fails
// the images flavor half and does not exist for an images request. The narrow
// default then redirects the request to the token's image-capable fallback
// instead of letting the relay refuse the child. A group whose only member is
// that child has no member that passes the images flavor half, so it does not
// exist for an images request either, and is redirected the same way.
func TestImagesRedirectsATextChildOfAMixedApplication(t *testing.T) {
	for _, requested := range []string{"flux1-dev", "text-group"} {
		t.Run(requested, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: imagesRelayResponse}
			srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI})
			seedServedFlavorsApplication(t, srv.Routes, routing.Application{ID: "app-sd-turbo", Type: routing.ProviderVLLM, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}}, "sd-turbo")
			seedCapabilityRow(t, srv, "route-sd-turbo", routing.CapabilityImage, routing.CapabilityYes, "manual")
			seedServedFlavorsGroup(t, srv.Routes, "text-group", "flux1-dev")
			srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_redirect", UserID: "usr_dev", Name: "Redirect", Active: true, Scopes: []string{"gateway:use"}, UnknownModelRedirect: true, UnknownModelFallback: "sd-turbo"}, "redirect-secret")

			rec := postImagesWithHeaders(t, srv, "/v1/images/generations", `{"model":"`+requested+`","prompt":"a cat","n":1}`, map[string]string{"Authorization": "Bearer redirect-secret"})

			requireRedirectedImagesRequest(t, srv, prov, rec, requested, "sd-turbo")
		})
	}
}
