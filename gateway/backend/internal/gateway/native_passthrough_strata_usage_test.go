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
	"testing"
	"time"
)

// newStrataTestServer seeds one strata application at upstreamURL (Anthropic and OpenAI
// flavors, both modes passthrough as the portal defaults them) with one mapping,
// "strata-model", served by the real OpenAI-compatible client.
func newStrataTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
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
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-strata", Name: "Strata", Domain: up.Hostname(), Provider: routing.ProviderStrata, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-strata", ServerID: "srv-strata", Type: routing.ProviderStrata, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-strata", ApplicationID: "app-strata", GatewayModelName: "strata-model", AppModelName: "strata-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-strata", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

const strataAnthropicUsage = `{"input_tokens":1,"output_tokens":20,"cache_creation_input_tokens":4,"cache_read_input_tokens":53}`

// A /v1/messages passthrough to Strata reads Anthropic usage where input_tokens
// EXCLUDES the cache: the passthrough scan folds the cache buckets back in (1+4+53 =
// 58 prompt tokens) and recordUsage then records the disjoint split, so the row
// carries fresh input 1, cached 53, cache-write 4, output 20 and a total of 78
// (58+20). Pins the existing accounting for Strata's usage shape, buffered and
// streamed.
func TestStrataMessagesPassthroughUsageSumsCacheFields(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\n"+
						`data: {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n"+
						"event: content_block_delta\n"+
						`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`+"\n\n"+
						"event: message_delta\n"+
						`data: {"type":"message_delta","usage":`+strataAnthropicUsage+`}`+"\n\n"+
						"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"strata-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":`+strataAnthropicUsage+`}`)
			}))
			t.Cleanup(up.Close)
			srv := newStrataTestServer(t, up.URL)
			body := `{"model":"strata-model","max_tokens":16,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			ev := lastUsageEvent(t, srv)
			if ev.InputTokens != 1 || ev.CachedTokens != 53 || ev.CacheWriteTokens != 4 || ev.OutputTokens != 20 || ev.TotalTokens != 78 {
				t.Fatalf("recorded input/cached/write/output/total = %d/%d/%d/%d/%d, want 1/53/4/20/78", ev.InputTokens, ev.CachedTokens, ev.CacheWriteTokens, ev.OutputTokens, ev.TotalTokens)
			}
		})
	}
}

// Strata opens an image reference that is not a data: or http(s) URL on the model
// host (a bare path or file://), so a chat request to a strata application carries
// only data: and http(s) images, like an sglang target.
func TestChatToStrataRefusesHostPathImages(t *testing.T) {
	for ref, refused := range map[string]bool{
		"data:image/png;base64,iVBORw0=":    false,
		"https://images.example.test/x.png": false,
		"/etc/passwd":                       true,
		"file:///etc/passwd":                true,
		"secret.png":                        true,
	} {
		t.Run(ref, func(t *testing.T) {
			up, calls := newWireUpstream(t)
			srv := newStrataTestServer(t, up.URL)
			body := strings.Replace(chatWithImage(ref), "agent-omni", "strata-model", 1)
			rec := postImagesWithHeaders(t, srv, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer dev-secret"})
			if !refused {
				if rec.Code != http.StatusOK || len(calls.all()) != 1 {
					t.Fatalf("status = %d, upstream calls = %d, want a relayed 200; body = %s", rec.Code, len(calls.all()), rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "chat.media_url_unsupported")
			if got := calls.all(); len(got) != 0 {
				t.Fatalf("upstream calls = %+v, want none", got)
			}
		})
	}
}
