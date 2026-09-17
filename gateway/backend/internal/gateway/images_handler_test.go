// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/apierror"
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

// postImages sends an images request through the real mux, the way
// active_requests_test.go drives the server.
func postImages(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The endpoint runs the ONE existing gate. A model with no image verdict is
// refused with the capability code -- not served, and not mislabelled as an
// unknown model.
func TestImagesRefusesIncapableModel(t *testing.T) {
	srv := NewTestServer() // seedGatewayTestRoutes seeds qwen-coder with no image verdict

	rec := postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "routing.model_not_capable") {
		t.Fatalf("body = %s, want the capability code, not an unknown-model code", rec.Body.String())
	}
}

// An images request has no messages, so inference.Request.Validate would reject
// every one of them. The handler validates its own shape instead, and asserting
// the EXACT code (rather than only the 400 status, or only the absence of the
// chat validator's own code) is what proves validateImagesRequest itself ran --
// a 400 could otherwise come from any number of unrelated causes, and nothing
// on this path ever calls Validate to produce request.messages_required in the
// first place, so a negative check for it would never fail either way.
func TestImagesRejectsMissingPrompt(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"model":"qwen-coder","n":1}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "images.prompt_required") {
		t.Fatalf("body = %s, want images.prompt_required", rec.Body.String())
	}
}

// The routing model comes from our own mapping via the same tolerant JSON probe
// every native-passthrough endpoint uses. sd-server itself IGNORES the model
// field -- one process serves one model -- so a body with no model must not
// resolve to something arbitrary. Asserting the exact code (not just the 400
// status) is what pins this to validateImagesRequest's own model check rather
// than some other 400.
func TestImagesRejectsMissingModel(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"prompt":"a cat"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body with no model; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "images.model_required") {
		t.Fatalf("body = %s, want images.model_required", rec.Body.String())
	}
}

// The usage row carries the endpoint's own path. This exercises the REFUSAL
// path (qwen-coder has no image verdict, see TestImagesRefusesIncapableModel):
// relayImages records a usage event for that terminal rejection too, exactly
// like the admission-queue and endpoint-disabled branches in
// native_passthrough.go already do. On a resolve failure target is the zero
// value, so ProviderPath is "" here (upstreamPath returns "" immediately for
// an empty target.Provider) -- this test only pins ReqPath and confirms
// ProviderPath is NOT the chat default; it does NOT exercise upstreamPath's
// own apiFlavorImages branch (that never runs against a zero target), which
// is why TestUpstreamPathImagesFlavorBypassesModeAndProviderFallbacks in
// native_passthrough_test.go pins that branch directly instead.
func TestImagesUsageRowCarriesItsOwnPath(t *testing.T) {
	srv := NewTestServer()

	postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	events := srv.Usage.All()
	if len(events) == 0 {
		t.Fatal("the refusal recorded no usage event")
	}
	got := events[len(events)-1]
	if got.ReqPath != "/v1/images/generations" {
		t.Fatalf("ReqPath = %q, want /v1/images/generations", got.ReqPath)
	}
	if got.ProviderPath == "/v1/chat/completions" {
		t.Fatal("ProviderPath fell through to the chat default: upstreamPath needs its own case")
	}
}

// sd-server returns {"error": "<plain string>"}, not OpenAI's
// {"error":{message,type,code}}. The relay normalises it, and the mapping is
// pinned so a client contract change is a test failure rather than a surprise.
func TestNormalizeImagesUpstreamError(t *testing.T) {
	body, ok := normalizeImagesUpstreamError(500, []byte(`{"error":"out of memory"}`))
	if !ok {
		t.Fatal("a plain-string error body must normalise")
	}
	// The upstream string reaches the client verbatim as the message...
	if !strings.Contains(body.Error.Message, "out of memory") {
		t.Fatalf("message = %q, want the upstream string verbatim", body.Error.Message)
	}
	// ...and type/code are OURS. Pinned so a later change cannot quietly start
	// presenting a gateway-authored value as an upstream statement.
	if body.Error.Type == "" || body.Error.Code == "" {
		t.Fatalf("type/code = %q/%q, want gateway-authored values", body.Error.Type, body.Error.Code)
	}
}

// An upstream body that is ALREADY an OpenAI error object passes through
// untouched -- normalising it twice would restate our own type as the
// upstream's.
func TestNormalizeImagesLeavesOpenAIShapeAlone(t *testing.T) {
	raw := []byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_prompt"}}`)
	body, ok := normalizeImagesUpstreamError(400, raw)
	if !ok {
		t.Fatal("an OpenAI-shaped body must still be accepted")
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "bad_prompt" {
		t.Fatalf("type/code = %q/%q, want the upstream's own values preserved", body.Error.Type, body.Error.Code)
	}
}

// A body that is neither shape must not be silently swallowed.
func TestNormalizeImagesUnrecognisedBody(t *testing.T) {
	if _, ok := normalizeImagesUpstreamError(500, []byte(`<html>502 Bad Gateway</html>`)); ok {
		t.Fatal("an unrecognised body must report ok=false so the caller relays the upstream bytes unchanged")
	}
}

// newImagesHTTPTestServer wires a Server whose images route resolves to a REAL
// AIServer/Application/Mapping (with an image=yes capability verdict, so the
// resolver's gate admits it) and whose Provider is a REAL
// provider.OpenAICompatibleClient making a genuine HTTP round trip to
// upstreamURL -- unlike the recordingProxyProvider mocks elsewhere in this
// package, which fake ProxyNative in-process and so never exercise the actual
// HTTP transport (internal/provider's doNativeProxy) that carries a real
// sd-server's bytes back to proxyNative. That distinction is the whole point
// of this helper: it is what lets TestImagesRelayNormalisesUpstreamErrorOverHTTP
// prove the wiring end to end rather than only prove normalizeImagesUpstreamError
// as a pure function.
func newImagesHTTPTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	// ApplicationEndpoint (routing/store.go) builds the reachable origin from
	// the SERVER's Domain + the APPLICATION's Scheme/Port -- not from
	// AIServer.Endpoint, which is descriptive only here -- so upstreamURL
	// (the httptest server's real address) must be decomposed into those
	// fields for the resolved Target to actually reach it.
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-images", Name: "Image Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-images", ServerID: "srv-images", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-images", ApplicationID: "app-images", GatewayModelName: "gw-image-model", AppModelName: "upstream-image-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-images", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-images", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

// TestImagesRelayNormalisesUpstreamErrorOverHTTP is the reachability proof the
// unit tests above cannot give: it drives a REAL non-2xx HTTP response from a
// real sd-server-shaped upstream (httptest.NewServer, not a mocked
// provider.Client) through the actual mux and asserts the CLIENT sees the
// normalised OpenAI object -- proving normalizeImagesUpstreamError is not just
// correct in isolation but actually WIRED into the relay's non-2xx path.
func TestImagesRelayNormalisesUpstreamErrorOverHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"out of memory"}`))
	}))
	defer up.Close()

	srv := newImagesHTTPTestServer(t, up.URL)
	rec := postImages(t, srv, `{"model":"gw-image-model","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the upstream's own status, unchanged); body = %s", rec.Code, rec.Body.String())
	}
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("client body did not parse as an OpenAI error object: %v (body = %s)", err, rec.Body.String())
	}
	if !strings.Contains(body.Error.Message, "out of memory") {
		t.Fatalf("message = %q, want the upstream string verbatim", body.Error.Message)
	}
	if body.Error.Type == "" || body.Error.Code == "" {
		t.Fatalf("type/code = %q/%q, want gateway-authored values, not sd-server's (which states neither)", body.Error.Type, body.Error.Code)
	}
}
