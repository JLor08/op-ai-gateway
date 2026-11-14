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
// chat completion (an event stream when the request asks for one), so a test reads
// what the provider actually put on the wire.
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

const wireChatStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"agent-omni\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"agent-omni\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

func newWireUpstream(t *testing.T) (*httptest.Server, *wireUpstream) {
	t.Helper()
	rec := &wireUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, wireCall{path: r.URL.Path, body: string(body)})
		rec.mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wireChatStream)
			return
		}
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

// chatWithImage is a chat completions body whose one user message carries an image.
func chatWithImage(imageURL string) string {
	return `{"model":"agent-omni","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`
}

// `sglang serve --model-type omni` is sglang-omni behind SGLang's console script: an
// Auto-typed spec with those args is guarded as sglang_omni (inline images only, and
// its passthrough served as translate), not as plain sglang.
func TestSGLangServeModelTypeOmniIsGuardedAsSGLangOmni(t *testing.T) {
	spec := autoOmniPassthroughSpec()
	spec.Binary, spec.Args = "/home/ai/sglang-env/bin/sglang", `["serve","--model-type","omni","--model-path","inclusionAI/Ming-3.0-tts-tiny"]`
	up, calls := newWireUpstream(t)
	srv := newWireSpecTestServer(t, up.URL, spec)
	rec := postImagesWithHeaders(t, srv, "/v1/chat/completions", chatWithImage("https://169.254.169.254/x.png"), map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "chat.media_url_unsupported")
	rec = postImagesWithHeaders(t, srv, "/v1/responses", `{"model":"agent-omni","input":"hi"}`, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("responses status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.all(); len(got) != 1 || got[0].path != "/v1/chat/completions" {
		t.Fatalf("upstream calls = %+v, want only the translated /v1/chat/completions", got)
	}
}

// A chat request routed to an sglang target may carry an image as a data: URL or an
// http(s) URL (the scheme case-insensitive). SGLang reads any reference starting with
// "/" or "file://" from the model host (python/sglang/srt/utils/common.py,
// get_image_bytes), and no gateway client has a reason to name a model-host path, so
// every other reference is refused. sglang_omni stays data: only.
func TestChatToSGLangRefusesHostPathImages(t *testing.T) {
	cases := []struct {
		url      string
		specType routing.RuntimeSpecType
		refused  bool
	}{
		{"data:image/png;base64,iVBORw0=", routing.RuntimeSpecTypeSGLang, false},
		{"DATA:image/png;base64,iVBORw0=", routing.RuntimeSpecTypeSGLang, false},
		{"http://images.example.test/x.png", routing.RuntimeSpecTypeSGLang, false},
		{"https://images.example.test/x.png", routing.RuntimeSpecTypeSGLang, false},
		{"HTTPS://images.example.test/x.png", routing.RuntimeSpecTypeSGLang, false},
		{"/etc/passwd", routing.RuntimeSpecTypeSGLang, true},
		{"file:///etc/passwd", routing.RuntimeSpecTypeSGLang, true},
		{"FILE:///etc/passwd", routing.RuntimeSpecTypeSGLang, true},
		{"secret.png", routing.RuntimeSpecTypeSGLang, true},
		{" /etc/passwd", routing.RuntimeSpecTypeSGLang, true},
		{"ftp://images.example.test/x.png", routing.RuntimeSpecTypeSGLang, true},
		{"iVBORw0KGgo=", routing.RuntimeSpecTypeSGLang, true},
		{"https://images.example.test/x.png", routing.RuntimeSpecTypeSGLangOmni, true},
		{"/etc/passwd", routing.RuntimeSpecTypeVLLM, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.specType)+" "+tc.url, func(t *testing.T) {
			prov := &captureCompleteProvider{recordingProxyProvider: recordingProxyProvider{respBody: "x"}}
			srv := newTypedServerAgentSpeechSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}, tc.specType)
			body := strings.Replace(chatWithImage(tc.url), "agent-omni", "agent-voice", 1)
			rec := postImagesWithHeaders(t, srv, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer dev-secret"})
			if !tc.refused {
				if rec.Code != http.StatusOK || prov.completes != 1 {
					t.Fatalf("status = %d, provider calls = %d, want a relayed 200; body = %s", rec.Code, prov.completes, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "chat.media_url_unsupported")
			if prov.completes != 0 {
				t.Fatalf("provider calls = %d, want 0", prov.completes)
			}
			if !strings.Contains(rec.Body.String(), "image_url.url") {
				t.Fatalf("refusal does not name the field: %s", rec.Body.String())
			}
			for _, leak := range []string{"passwd", "secret.png", "images.example.test", "iVBOR"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("refusal echoes the value (%q): %s", leak, rec.Body.String())
				}
			}
			ev := lastUsageEvent(t, srv)
			if ev.ErrorCode != "chat.media_url_unsupported" || ev.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("usage row = %+v, want a 400 chat.media_url_unsupported row", ev)
			}
		})
	}
}

// The sglang refusal says what the sglang target accepts, which is not the sglang_omni
// sentence.
func TestChatMediaRefusalMessageFollowsTheTargetType(t *testing.T) {
	refusal := func(specType routing.RuntimeSpecType) string {
		prov := &captureCompleteProvider{recordingProxyProvider: recordingProxyProvider{respBody: "x"}}
		srv := newTypedServerAgentSpeechSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI}, specType)
		body := strings.Replace(chatWithImage("/etc/passwd"), "agent-omni", "agent-voice", 1)
		return postImagesWithHeaders(t, srv, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer dev-secret"}).Body.String()
	}
	sglang, omni := refusal(routing.RuntimeSpecTypeSGLang), refusal(routing.RuntimeSpecTypeSGLangOmni)
	if !strings.Contains(sglang, "http") || sglang == omni {
		t.Fatalf("sglang refusal = %s, omni refusal = %s; want an sglang sentence that names http(s)", sglang, omni)
	}
}

// The speech guard fails closed: a body that does not decode as one JSON object says
// nothing about what the upstream would read from it, so for an sglang_omni target it
// is refused under the field name "body" (validateSpeechRequest refuses every such body
// first today; the guard does not rely on that).
func TestSpeechReferenceGuardFailsClosedOnANonObjectBody(t *testing.T) {
	omni := routing.Target{LiveProgressSpecType: string(routing.RuntimeSpecTypeSGLangOmni)}
	for _, raw := range []string{`["/etc/passwd"]`, `"/etc/passwd"`, `null`, `1`, `{"ref_audio":"/etc/passwd"`, ``, `{"model":"m"} {"ref_audio":"/etc/passwd"}`} {
		if got := nonInlineVoiceReferenceField([]byte(raw), omni); got != "body" {
			t.Errorf("nonInlineVoiceReferenceField(%q) = %q, want body", raw, got)
		}
	}
	if got := nonInlineVoiceReferenceField([]byte(`{}`), omni); got != "" {
		t.Errorf("an empty object = %q, want accepted", got)
	}
	if got := nonInlineVoiceReferenceField([]byte(`["/etc/passwd"]`), routing.Target{}); got != "" {
		t.Errorf("another target = %q, want unchecked", got)
	}
	if msg := speechReferenceError("body").Message; !strings.Contains(msg, "JSON object") {
		t.Errorf("body refusal message = %q, want it to say the body must be a JSON object", msg)
	}
}
