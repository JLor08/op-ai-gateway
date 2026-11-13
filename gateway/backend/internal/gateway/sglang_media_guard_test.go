// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// wireCall is one request the wire upstream received: its path and its body.
type wireCall struct {
	path string
	body string
}

// wireUpstream records every request an httptest upstream receives and answers a
// chat completion, so a test reads what the provider actually put on the wire.
type wireUpstream struct {
	mu    sync.Mutex
	calls []wireCall
}

func (u *wireUpstream) all() []wireCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]wireCall(nil), u.calls...)
}

const wireChatCompletion = `{"id":"c1","object":"chat.completion","created":1,"model":"agent-omni","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func newWireUpstream(t *testing.T) (*httptest.Server, *wireUpstream) {
	t.Helper()
	rec := &wireUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, wireCall{path: r.URL.Path, body: string(body)})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wireChatCompletion)
	}))
	t.Cleanup(up.Close)
	return up, rec
}

// newWireSpecTestServer seeds one server_agent application at upstreamURL (both modes
// passthrough, flavors openai, anthropic and openai_audio) whose one mapping,
// "agent-omni", carries spec, and serves it with the real OpenAI-compatible client, so
// the upstream sees the request the provider builds.
func newWireSpecTestServer(t *testing.T, upstreamURL string, spec routing.RuntimeSpec) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-wire", Name: "Wire", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-wire", ServerID: "srv-wire", Type: routing.ProviderServerAgent, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic, routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-wire", ApplicationID: "app-wire", GatewayModelName: "agent-omni", AppModelName: "agent-omni", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	spec.ID, spec.MappingID, spec.CreatedAt, spec.UpdatedAt = "spec-wire", "route-wire", now, now
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-wire", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// autoOmniPassthroughSpec is an sglang-omni spec as an operator who never picked a type
// leaves it: Auto (no type), an omni binary, and both coding-agent modes at the stock
// passthrough.
func autoOmniPassthroughSpec() routing.RuntimeSpec {
	return routing.RuntimeSpec{
		Binary:        "/opt/sglang-omni/bin/sgl-omni",
		APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic, routing.APIFlavorOpenAIAudio},
		ResponsesMode: routing.EndpointModePassthrough,
		MessagesMode:  routing.EndpointModePassthrough,
	}
}

// A /v1/messages request to an sglang_omni target whose messages_mode is passthrough
// is served as translate: the raw body would reach sglang-omni unexamined, so its image
// URLs would be fetched, or read from the model host, past the media guard. A URL image
// is refused like a chat one, and an inline image reaches the upstream's
// /v1/chat/completions as the translated request.
func TestMessagesPassthroughToSGLangOmniIsServedAsTranslate(t *testing.T) {
	messages := func(source string) string {
		return `{"model":"agent-omni","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image","source":` + source + `}]}]}`
	}
	t.Run("url image refused", func(t *testing.T) {
		up, calls := newWireUpstream(t)
		srv := newWireSpecTestServer(t, up.URL, autoOmniPassthroughSpec())
		rec := postImagesWithHeaders(t, srv, "/v1/messages", messages(`{"type":"url","url":"https://169.254.169.254/x.png"}`), map[string]string{"Authorization": "Bearer dev-secret"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "chat.media_url_unsupported")
		if got := calls.all(); len(got) != 0 {
			t.Fatalf("upstream calls = %+v, want none", got)
		}
		if strings.Contains(rec.Body.String(), "169.254") {
			t.Fatalf("refusal echoes the value: %s", rec.Body.String())
		}
	})
	t.Run("inline image translated", func(t *testing.T) {
		up, calls := newWireUpstream(t)
		srv := newWireSpecTestServer(t, up.URL, autoOmniPassthroughSpec())
		rec := postImagesWithHeaders(t, srv, "/v1/messages", messages(`{"type":"base64","media_type":"image/png","data":"iVBORw0="}`), map[string]string{"Authorization": "Bearer dev-secret"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		got := calls.all()
		if len(got) != 1 || got[0].path != "/v1/chat/completions" {
			t.Fatalf("upstream calls = %+v, want one /v1/chat/completions", got)
		}
		if !strings.Contains(got[0].body, `"url":"data:image/png;base64,iVBORw0="`) {
			t.Fatalf("upstream body = %s, want the inline image as an image_url", got[0].body)
		}
	})
}

// A /v1/responses request to an sglang_omni target whose responses_mode is passthrough
// is served as translate too. The Responses translation keeps text only (images inside
// message content are skipped, compat.ParseOpenAIResponses), so no image of any kind
// reaches sglang-omni from this endpoint: the request reaches the upstream's
// /v1/chat/completions without it.
func TestResponsesPassthroughToSGLangOmniIsServedAsTranslate(t *testing.T) {
	responses := func(imageURL string) string {
		return `{"model":"agent-omni","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"` + imageURL + `"}]}]}`
	}
	for name, imageURL := range map[string]string{
		"url image":    "https://169.254.169.254/x.png",
		"path image":   "/etc/passwd",
		"inline image": "data:image/png;base64,iVBORw0=",
	} {
		t.Run(name, func(t *testing.T) {
			up, calls := newWireUpstream(t)
			srv := newWireSpecTestServer(t, up.URL, autoOmniPassthroughSpec())
			rec := postImagesWithHeaders(t, srv, "/v1/responses", responses(imageURL), map[string]string{"Authorization": "Bearer dev-secret"})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			got := calls.all()
			if len(got) != 1 || got[0].path != "/v1/chat/completions" {
				t.Fatalf("upstream calls = %+v, want one /v1/chat/completions", got)
			}
			if strings.Contains(got[0].body, imageURL) || strings.Contains(got[0].body, "image_url") {
				t.Fatalf("upstream body = %s, want no image", got[0].body)
			}
			if ev := lastUsageEvent(t, srv); ev.ProviderPath != "/v1/chat/completions" {
				t.Fatalf("usage provider_path = %q, want /v1/chat/completions", ev.ProviderPath)
			}
		})
	}
}

// The coercion belongs to sglang_omni alone: an sglang target in passthrough mode keeps
// its raw relay.
func TestResponsesPassthroughToSGLangStaysPassthrough(t *testing.T) {
	up, calls := newWireUpstream(t)
	spec := autoOmniPassthroughSpec()
	spec.Binary = "/opt/sglang/bin/sglang"
	srv := newWireSpecTestServer(t, up.URL, spec)
	body := `{"model":"agent-omni","input":"hi"}`
	rec := postImagesWithHeaders(t, srv, "/v1/responses", body, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.all(); len(got) != 1 || got[0].path != "/v1/responses" {
		t.Fatalf("upstream calls = %+v, want one raw /v1/responses relay", got)
	}
}
