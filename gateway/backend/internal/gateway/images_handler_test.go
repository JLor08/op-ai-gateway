// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/logbuffer"
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
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	// The unit is endpoint identity, set on EVERY recordUsage call this path
	// makes -- this refusal is relayImages' own resolve-failure branch
	// (images_handler.go), which is otherwise untested for BillingUnit.
	if got := lastUsageEvent(t, srv).BillingUnit; got != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q on the refusal path, want %q", got, usage.BillingUnitImage)
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
	requireErrorCode(t, rec.Body.String(), "images.prompt_required")
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
	requireErrorCode(t, rec.Body.String(), "images.model_required")
}

// The usage row carries the endpoint's own path. This exercises the REFUSAL
// path (qwen-coder has no image verdict, see TestImagesRefusesIncapableModel):
// relayImages records a usage event for that terminal rejection too, exactly
// like the admission-queue and endpoint-disabled branches in
// native_passthrough.go already do. It pins ReqPath and the billing unit only:
// on a resolve failure the target is the zero value, so upstreamPath is never
// consulted at all and ProviderPath is "" -- there is nothing about
// upstreamPath's own apiFlavorImages branch to assert from here, which is why
// TestUpstreamPathImagesFlavorBypassesModeAndProviderFallbacks in
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
	// Same refusal path as TestImagesRefusesIncapableModel; pinning the unit
	// here too since this is one of only two of relayImages/proxyNative's
	// five images-reachable recordUsage call sites this file exercised.
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q on the refusal path, want %q", got.BillingUnit, usage.BillingUnitImage)
	}
	// This is the resolve-failure branch recordImagesRoutingFailure covers
	// (relayImages' own resolveTarget error, routing.ErrModelNotCapable here) --
	// pin HTTPStatus and Status too, so a later change to that shared helper
	// cannot silently alter what it records for a plain resolve failure
	// without a test noticing.
	if got.HTTPStatus != http.StatusNotFound {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, http.StatusNotFound)
	}
	if got.Status != "error" {
		t.Fatalf("Status = %q, want %q", got.Status, "error")
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
//
// APIFlavors carries APIFlavorOpenAIImages alongside APIFlavorOpenAI: since
// NormalizeAPIFlavor no longer folds openai_images into openai, an application
// must opt into the images flavor explicitly to be an image candidate at all.
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
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-images", ServerID: "srv-images", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
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

// newImageCapableTestServer returns a NewTestServer() (same base seed --
// seedGatewayTestRoutes' qwen-coder mapping, which carries no image verdict)
// whose route store ADDITIONALLY carries an application/mapping pointing at
// upstreamURL for the model "sd-turbo", with an image:yes capability row so
// the resolver's gate admits it. Mirrors seedGatewayTestRoutes' own seeding
// shape (CreateAIServer + CreateApplication + CreateMapping +
// UpsertMappingCapabilities + UpsertTelemetry) rather than inventing a second
// seeding style.
//
// Unlike NewTestServer, the Provider here is a REAL
// provider.NewOpenAICompatibleClient making a genuine HTTP round trip to
// upstreamURL. provider.NewMock's own ProxyNative (internal/provider/mock.go)
// returns a canned SSE stream and never looks at the target or path at all,
// so a server built with NewTestServer's own Provider could never reach the
// stub upstream these usage tests depend on -- the same reason
// newImagesHTTPTestServer above uses a real provider instead of the mock.
func newImageCapableTestServer(t *testing.T, upstreamURL string) *Server {
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
	seedGatewayTestRoutes(routeStore, now)

	ctx := context.Background()
	// ApplicationEndpoint (routing/store.go) builds the reachable origin from
	// the SERVER's Domain + the APPLICATION's Scheme/Port -- not from
	// AIServer.Endpoint, which is descriptive only here -- so upstreamURL (the
	// httptest server's real address) must be decomposed into those fields for
	// the resolved Target to actually reach it. Mirrors newImagesHTTPTestServer
	// above, which hit this exact requirement first.
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-turbo", Name: "SD Turbo Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo", ServerID: "srv-sd-turbo", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-turbo", ApplicationID: "app-sd-turbo", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-turbo", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

// newImageCapableLoopbackTestServer is newImageCapableTestServer's sibling for
// the loopback-auth tests: same image-capable route wiring, but ALSO wires
// InternalAuthSecret + Users so the internal trusted-loopback pair
// (X-OP-Internal-Auth + X-OP-Internal-User) authenticates -- the path the
// portal-chat run executor actually uses. Returns the directory so a caller
// can seed a run-as token owned by "usr_dev" (or another user) via
// CreatePlainToken.
func newImageCapableLoopbackTestServer(t *testing.T, upstreamURL string) (*Server, *portal.MemoryDirectory) {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
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
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-turbo", Name: "SD Turbo Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo", ServerID: "srv-sd-turbo", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-turbo", ApplicationID: "app-sd-turbo", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-turbo", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	srv := New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:             routeStore,
		Portal:             portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		InternalAuthSecret: "s3cret",
		Users:              directory,
	})
	return srv, directory
}

// lastUsageEvent returns the most recently recorded usage event on srv,
// failing the test when none was recorded.
func lastUsageEvent(t *testing.T, srv *Server) usage.Event {
	t.Helper()
	events := srv.Usage.All()
	if len(events) == 0 {
		t.Fatal("no usage event recorded")
	}
	return events[len(events)-1]
}

// The quantity comes from the RESPONSE, not the request. n states what was asked
// for; data[] states what was produced, and a partial failure makes those
// differ. Metering the ask would be the same class of error as counting a
// measured zero as a measurement.
func TestImagesUsageQuantityComesFromTheResponse(t *testing.T) {
	// Stub upstream: two images back for a request that asked for four.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="},{"b64_json":"BB=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat","n":4}`)

	got := lastUsageEvent(t, srv)
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitImage)
	}
	if got.BillingQuantity != 2 {
		t.Fatalf("BillingQuantity = %v, want 2 (what the response produced, not the n=4 that was asked for)", got.BillingQuantity)
	}
}

// The XOR: all seven token-denominated columns must be zero on a non-token row.
func TestImagesUsageRowSatisfiesTheBillingXOR(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	if err := usage.ValidateBillingXOR(lastUsageEvent(t, srv)); err != nil {
		t.Fatalf("the recorded image row violates the billing XOR: %v", err)
	}
}

// A failed image request is still a NON-TOKEN row: the unit is endpoint
// identity, never response-derived, so a 500 must not be recorded as
// token-metered with a zero measure -- the exact lie the pair exists to prevent.
func TestImagesUsageOnFailureIsStillImageUnit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"out of memory"}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	got := lastUsageEvent(t, srv)
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q on a FAILED image request, want %q", got.BillingUnit, usage.BillingUnitImage)
	}
	if got.BillingQuantity != 0 {
		t.Fatalf("BillingQuantity = %v, want 0: nothing was produced", got.BillingQuantity)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Fatalf("the failed image row violates the billing XOR: %v", err)
	}
}

// response_format:"url" is REJECTED at the request boundary rather than
// relayed and billed afterwards -- see validateImagesRequest's own doc
// comment for the reasoning (sd-server cannot host a URL for its own
// in-process output, and imagesDataCounter is built against the b64_json
// KEY, so a "url" response would relay successfully while counting 0
// produced images for every one of them).
func TestImagesRejectsUnsupportedResponseFormat(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","response_format":"url"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "images.response_format_unsupported")
}

// A NON-STRING response_format must be rejected too. It used to slip through:
// validateImagesRequest decoded the field straight into a string and discarded
// the unmarshal error, so ["url"] and 123 both left the field at "" and walked
// past the one check the function makes -- the exact values a client sends when
// it means something this relay cannot honor. Both are rejected by the raw
// decode now; the "url" case above is the same rule on a well-typed value.
func TestImagesRejectsNonStringResponseFormat(t *testing.T) {
	for _, body := range []string{
		`{"model":"qwen-coder","prompt":"a cat","response_format":["url"]}`,
		`{"model":"qwen-coder","prompt":"a cat","response_format":123}`,
	} {
		srv := NewTestServer()

		rec := postImages(t, srv, body)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "images.response_format_unsupported")
	}
}

// stream:true is refused for the same reason response_format:"url" is: the
// gateway has pinned this endpoint to the buffered path, so relaying the flag
// unexamined sent the upstream a "stream":true the gateway then ignored and
// handed the client a single application/json body where it asked for a stream.
// stream:false and an absent stream describe what the endpoint already does and
// are accepted -- the acceptance is what makes this a refusal of the value
// rather than of the field.
func TestImagesRejectsStreamTrue(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","stream":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "images.stream_unsupported")

	// stream:false and a non-boolean stream: the first must pass validation (it
	// then hits the capability refusal, 404, like every other request in this
	// file against the no-verdict qwen-coder), the second must not.
	if got := postImages(t, NewTestServer(), `{"model":"qwen-coder","prompt":"a cat","stream":false}`); got.Code != http.StatusNotFound {
		t.Fatalf("stream:false status = %d, want 404 (validation passed, routing refused); body = %s", got.Code, got.Body.String())
	}
	// Hoisted out of the if so requireErrorCode can still see `got`; the
	// preceding `got` is scoped to its own if, so this declaration is new.
	got := postImages(t, NewTestServer(), `{"model":"qwen-coder","prompt":"a cat","stream":"true"}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf(`stream:"true" status = %d, body = %s, want 400`, got.Code, got.Body.String())
	}
	requireErrorCode(t, got.Body.String(), "images.stream_unsupported")
}

// imagesDataCounter must count "b64_json" only when it is used as a JSON
// KEY, never when it appears as a matching STRING VALUE -- the reviewer's
// three concrete over-count repros, reproduced directly against the real
// counter (no HTTP, no server): an echoed response_format, an echoed
// parameters object, and a prompt/revised_prompt whose text happens to be
// exactly "b64_json". Each body carries exactly ONE real image key.
func TestImagesDataCounterCountsKeysNotValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "echoed response_format",
			body: `{"created":1,"response_format":"b64_json","data":[{"b64_json":"AA=="}]}`,
		},
		{
			name: "echoed parameters object",
			body: `{"created":1,"parameters":{"n":1,"response_format":"b64_json"},"data":[{"b64_json":"AA=="}]}`,
		},
		{
			name: "revised_prompt text happens to be the literal string",
			body: `{"created":1,"data":[{"b64_json":"AA==","revised_prompt":"b64_json"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &imagesDataCounter{}
			c.feed([]byte(tc.body))
			if got := c.total(); got != 1 {
				t.Fatalf("total() = %d, want 1 (one real image key; the rest are string values) -- body: %s", got, tc.body)
			}
		})
	}
}

// A successful (2xx) images relay that counts zero produced images must not
// silently record it as a plain zero -- indistinguishable from a genuine
// empty data[] -- it must say so loudly. See imagesDataCounter's own doc
// comment and the log call beside BillingQuantity's assignment in
// proxyNative (native_passthrough.go).
func TestImagesUsageLogsLoudlyWhenASuccessfulRelayCountsZeroImages(t *testing.T) {
	logs := logbuffer.NewBuffer(50, logbuffer.LevelTrace)
	setDefaultSlogForTest(t, logs)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 2xx, but a shape this relay's counter cannot recognise any b64_json
		// KEY in -- e.g. an upstream whose response shape drifted unannounced.
		_, _ = w.Write([]byte(`{"created":1,"data":[{"unexpected_field":"oops"}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	got := lastUsageEvent(t, srv)
	if got.BillingQuantity != 0 {
		t.Fatalf("BillingQuantity = %v, want 0 (nothing this relay could count)", got.BillingQuantity)
	}
	var found bool
	for _, rec := range logs.Snapshot() {
		if rec.Level == "ERROR" && strings.Contains(rec.Msg, "zero produced images") {
			found = true
		}
	}
	if !found {
		t.Fatal("a successful images relay that counted zero images must log loudly, not silently record a bare zero")
	}
}

// newImageCapableTestServerWithProvider mirrors newImageCapableTestServer's
// route seeding (sd-turbo, image:yes) but wires prov directly instead of a
// real HTTP client -- for exercising proxyNative's PRE-RESPONSE branches
// ("provider is not a NativeProxyClient", a ProxyNative transport error),
// which never reach an actual upstream at all, so no httptest server or URL
// decomposition is needed here.
func newImageCapableTestServerWithProvider(t *testing.T, prov provider.Client) *Server {
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
	seedGatewayTestRoutes(routeStore, now)
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-turbo-fake", Name: "SD Turbo Fake", Domain: "sd-turbo.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://sd-turbo.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo-fake", ServerID: "srv-sd-turbo-fake", Type: routing.ProviderVLLM, Port: 80, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-turbo-fake", ApplicationID: "app-sd-turbo-fake", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-sd-turbo-fake", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-turbo-fake", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

// TestImagesProviderUnavailableStillRecordsImageUnit pins the third of the
// five images-reachable recordUsage call sites: proxyNative's own
// "provider is not a NativeProxyClient" branch (native_passthrough.go),
// shared with every other native-passthrough flavor, which must still read
// the unit from pfReq.APIFlavor (billingUnitFor) for an images request.
func TestImagesProviderUnavailableStillRecordsImageUnit(t *testing.T) {
	srv := newImageCapableTestServerWithProvider(t, nonProxyCapableProvider{})

	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	got := lastUsageEvent(t, srv)
	if got.ErrorCode != "provider.unavailable" {
		t.Fatalf("ErrorCode = %q, want provider.unavailable", got.ErrorCode)
	}
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q, want %q (endpoint identity, set even when the provider itself can't proxy)", got.BillingUnit, usage.BillingUnitImage)
	}
}

// TestImagesUpstreamCallFailureStillRecordsImageUnit pins the fourth of the
// five images-reachable recordUsage call sites: proxyNative's pre-response
// ProxyNative-error branch (native_passthrough.go, upstream unreachable),
// also shared with every other native-passthrough flavor.
func TestImagesUpstreamCallFailureStillRecordsImageUnit(t *testing.T) {
	srv := newImageCapableTestServerWithProvider(t, erroringProxyProvider{err: errors.New("dial tcp: connection refused")})

	postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	got := lastUsageEvent(t, srv)
	if got.Status != "error" {
		t.Fatalf("Status = %q, want error", got.Status)
	}
	if got.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q, want %q (endpoint identity, set even on a pre-response transport failure)", got.BillingUnit, usage.BillingUnitImage)
	}
}

// postImagesWithHeaders is postImages' sibling for the auth tests: same mux,
// same path, but the caller owns the headers. postImages hardcodes a bearer and
// every existing images test depends on that, so it is left alone.
func postImagesWithHeaders(t *testing.T, srv *Server, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The bearer leg must keep working: every existing API client uses it.
func TestImagesStillAcceptsABearerToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv := newImageCapableTestServer(t, upstream.URL)
	rec := postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The alias route shares the handler and therefore the widened auth. No test
// covered it before this change. Posts the LOOPBACK pair, not a bearer: a
// bearer already worked on this alias before this task widened anything (the
// alias has shared handleOpenAIImages, and therefore requireAnyScope, all
// along) -- so a bearer-only assertion here would pin route registration, not
// the widening. The loopback pair is the leg this task actually added.
func TestImagesAliasRouteSharesTheAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv, _ := newImageCapableLoopbackTestServer(t, upstream.URL)
	rec := postImagesWithHeaders(t, srv, "/openai/v1/images/generations",
		`{"model":"sd-turbo","prompt":"a cat"}`,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alias route status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// THE BOUNDARY THIS TASK EXISTS TO PRESERVE: a browser session cookie must NOT
// reach this endpoint. newChatTestServer wires a real Account and loginCookie
// mints a real session, so this is the genuine article rather than a stub. A
// 401 proves auth refused; had the cookie been accepted the request would have
// failed LATER and differently (the capability gate, a 404), never with a 401.
func TestImagesRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_img", "img@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "img@example.test", "password-1")

	rec := postImagesWithHeaders(t, srv, "/v1/images/generations",
		`{"model":"sd-turbo","prompt":"a cat"}`,
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a real session cookie: %s", rec.Code, rec.Body.String())
	}
}

// TestImagesRunAsTokenAttributesUsage verifies that an image request carrying
// the loopback pair plus X-OP-Run-As-Token swaps the principal to the named
// token (owned by the loopback-resolved user) before routing, and that usage
// is attributed to that token rather than to the bare loopback/session
// principal -- the sibling of TestChatRunAsTokenAppliesOverrideAndUsage
// (server_test.go) for the images path, mirroring its own assertion style
// (lastUsageEvent's TokenID).
func TestImagesRunAsTokenAttributesUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv, dir := newImageCapableLoopbackTestServer(t, upstream.URL)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ra_img", UserID: "usr_dev", Name: "RA Img", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "ra-secret"); err != nil {
		t.Fatalf("seed run-as token: %v", err)
	}

	rec := postImagesWithHeaders(t, srv, "/v1/images/generations",
		`{"model":"sd-turbo","prompt":"a cat"}`,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_ra_img"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := lastUsageEvent(t, srv).TokenID; got != "tok_ra_img" {
		t.Fatalf("TokenID = %q, want tok_ra_img (the run-as token, not the bare loopback principal)", got)
	}
}

// TestImagesRunAsTokenForbiddenForUnownedToken verifies that naming a run-as
// token the loopback-resolved user does not own is rejected with 403
// portal.token_forbidden -- refused rather than silently downgraded to the
// bare loopback principal -- and that no image request reaches the upstream
// or records usage. Sibling of TestChatRunAsTokenForbiddenForUnownedToken.
func TestImagesRunAsTokenForbiddenForUnownedToken(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	defer upstream.Close()

	srv, dir := newImageCapableLoopbackTestServer(t, upstream.URL)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other_img", UserID: "usr_other", Name: "Other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "other-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rec := postImagesWithHeaders(t, srv, "/v1/images/generations",
		`{"model":"sd-turbo","prompt":"a cat"}`,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_other_img"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("run-as of unowned token should be 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "portal.token_forbidden")
	if upstreamCalled {
		t.Fatal("upstream must never be called for a forbidden run-as token")
	}
	if events := srv.Usage.All(); len(events) != 0 {
		t.Fatalf("forbidden run-as should record no usage, got %#v", events)
	}
}

// TestImagesRunAsWithoutPortalIsRefused guards the nil-Portal edge case a
// bare &Server{} (no Portal wired) would otherwise hit: handleOpenAIChat's
// AuthorizeRunAsToken call is unguarded against s.Portal == nil, and copying
// that block verbatim into handleOpenAIImages would inherit the same nil
// dereference. Refused with 403 rather than panicking, and rather than
// silently proceeding as the bare loopback principal.
func TestImagesRunAsWithoutPortalIsRefused(t *testing.T) {
	s := &Server{internalAuthSecret: "s3cret", users: fakeUserLookup{
		"usr_1": {ID: "usr_1", Role: "user"},
	}} // Portal, Routes, Tokens all nil
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"sd-turbo","prompt":"a cat"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	r.Header.Set(runAsHeaderName, "tok_whatever")
	w := httptest.NewRecorder()

	s.handleOpenAIImages(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s, want 403", w.Code, w.Body.String())
	}
	requireErrorCode(t, w.Body.String(), "portal.token_forbidden")
}

// newServerAgentImagesSpecTestServer seeds a server_agent application that
// candidacy ADMITS for images (app-level flavors include openai_images, and
// the mapping carries image:yes), whose mapping's runtime spec carries
// specFlavors. The spec is the per-model authority for a server_agent mapping
// (routing.Resolver.targetFrom), known only post-resolve -- so whether the
// request is served is decided by the relay's effective-flavor check alone.
func newServerAgentImagesSpecTestServer(t *testing.T, prov provider.Client, specFlavors []string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-agent-images", Name: "Agent Images", Domain: "agent-images.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://agent-images.example.test:8081", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-agent-images", ServerID: "srv-agent-images", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-agent-images", ApplicationID: "app-agent-images", GatewayModelName: "flux1-dev", AppModelName: "flux1-dev", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-agent-images", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-agent-images", MappingID: "route-agent-images", APIFlavors: specFlavors, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-agent-images", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
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

// TestImagesRefusesServerAgentChildWhoseSpecExcludesImages covers the
// agent-managed case. Candidacy gates a server_agent application on its own
// app-level flavors, so a spec that narrows its model to text only is honoured
// nowhere on the images path unless relayImages checks it -- tryProxyNative's
// equivalent check is gated on the coding-agent endpoints, and relayImages
// does not go through it. The refusal looks like candidacy's own no-route
// answer when this was the ONLY route for the model, as it is here (same
// code, same status) -- it is NOT retried against a sibling application that
// might still serve the request, the same limitation tryProxyNative's own
// equivalent check carries for the coding-agent endpoints; see relayImages'
// comment at the flavor check for that follow-up.
func TestImagesRefusesServerAgentChildWhoseSpecExcludesImages(t *testing.T) {
	prov := &recordingProxyProvider{respBody: `{"created":1,"data":[{"b64_json":"AA=="}]}`}
	srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI})

	rec := postImages(t, srv, `{"model":"flux1-dev","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0 -- a text-only spec must never be sent an image request", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].ErrorCode != "routing.no_model_route" || events[0].BillingUnit != usage.BillingUnitImage {
		t.Fatalf("usage events = %+v, want one routing.no_model_route error billed as image", events)
	}
	// This refusal never reaches proxyNative (proxyCalls == 0 above), so no
	// upstream call was ever made -- the row must record that, the same way
	// every other resolve-failure row on this path does. upstreamPath's own
	// doc comment reserves "" for exactly this case, and a populated
	// Host/RouteID/ProviderPath would both misreport a call that never
	// happened and make the energy reconciler price + count this row against
	// a server it never touched.
	got := events[0]
	if got.Host != "" || got.RouteID != "" || got.ProviderPath != "" {
		t.Fatalf("usage row = %+v, want Host/RouteID/ProviderPath all empty -- no upstream call was ever made", got)
	}
	if got.HTTPStatus != http.StatusNotFound {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, http.StatusNotFound)
	}
}

// TestImagesServesServerAgentChildWhoseSpecIncludesImages is the positive
// control for the test above: the same fixture with a spec that lists
// openai_images is served. Without it, the refusal above could come from
// anything in the fixture rather than from the flavor check.
func TestImagesServesServerAgentChildWhoseSpecIncludesImages(t *testing.T) {
	prov := &recordingProxyProvider{respBody: `{"created":1,"data":[{"b64_json":"AA=="}]}`}
	srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIImages})

	rec := postImages(t, srv, `{"model":"flux1-dev","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
	}
}

// newRedirectingTestServer builds an images server whose dev token is opted in
// to the unknown-model redirect with the given LastUsedModel and fallback.
// Three models are routable for an images request, one per shape the redirect
// has to tell apart:
//
//   - qwen-coder (seedGatewayTestRoutes): its application declares
//     openai_images beside the two text flavors, and the mapping carries NO
//     image verdict -- callable for the images flavor, refused by the
//     capability gate.
//   - sd-turbo: the same mixed flavors, and an image=yes verdict.
//   - flux1-dev: an images-only application ([openai_images]), image=yes --
//     the stable-diffusion.cpp shape.
func newRedirectingTestServer(t *testing.T, prov provider.Client, lastUsed, fallback string) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive,
		Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now,
		UnknownModelRedirect: true, LastUsedModel: lastUsed, UnknownModelFallback: fallback,
	}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	ctx := context.Background()
	seedImageModel := func(id, gatewayModel string, flavors []string) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-" + id, Name: id, Domain: id + ".example.test", Provider: routing.ProviderVLLM, Endpoint: "http://" + id + ".example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-" + id, ServerID: "srv-" + id, Type: routing.ProviderVLLM, Port: 80, Scheme: "http", APIFlavors: flavors, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-" + id, ApplicationID: "app-" + id, GatewayModelName: gatewayModel, AppModelName: gatewayModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, "route-"+id, []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
		}
		if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-" + id, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", id, err)
		}
	}
	seedImageModel("sd-turbo", "sd-turbo", []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages})
	seedImageModel("flux", "flux1-dev", []string{routing.APIFlavorOpenAIImages})
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// imagesRelayResponse is a minimal successful sd-server answer.
const imagesRelayResponse = `{"created":1,"data":[{"b64_json":"AA=="}]}`

// requireRedirectedImagesRequest asserts that an images request for an unknown
// model was redirected to want and served there: 200, one upstream call, and a
// usage row whose effective Model is want while RequestedModel keeps the
// client's own name.
func requireRedirectedImagesRequest(t *testing.T, srv *Server, prov *recordingProxyProvider, rec *httptest.ResponseRecorder, requested, want string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (redirected to %s); body = %s", rec.Code, want, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
	}
	got := lastUsageEvent(t, srv)
	if got.Model != want || got.RequestedModel != requested {
		t.Fatalf("usage Model/RequestedModel = %q/%q, want %q/%q", got.Model, got.RequestedModel, want, requested)
	}
}

// An images request for a name that does not exist is redirected when the
// token's fallback is a model that carries the image capability: a client that
// hardcodes dall-e-3 against a token dedicated to images is served by the
// fallback, exactly as for a text request.
func TestImagesRedirectsAnUnknownModelToAnImageCapableFallback(t *testing.T) {
	prov := &recordingProxyProvider{respBody: imagesRelayResponse}
	srv := newRedirectingTestServer(t, prov, "", "sd-turbo")

	rec := postImages(t, srv, `{"model":"dall-e-3","prompt":"a cat","n":1}`)

	requireRedirectedImagesRequest(t, srv, prov, rec, "dall-e-3", "sd-turbo")
}

// The token's last-used model is the first candidate, and an images-only model
// (whose application declares openai_images alone) qualifies like any other
// image-capable model.
func TestImagesRedirectsAnUnknownModelToAnImagesOnlyLastUsedModel(t *testing.T) {
	prov := &recordingProxyProvider{respBody: imagesRelayResponse}
	srv := newRedirectingTestServer(t, prov, "flux1-dev", "sd-turbo")

	rec := postImages(t, srv, `{"model":"dall-e-3","prompt":"a cat","n":1}`)

	requireRedirectedImagesRequest(t, srv, prov, rec, "dall-e-3", "flux1-dev")
}

// A candidate that lacks the image capability is skipped, not taken: the chain
// moves on to the next candidate. A token used for chat as well as images has a
// text model as its last-used model most of the time; taking it would answer
// 404 model_not_capable about a model the client never named.
func TestImagesRedirectSkipsALastUsedModelWithoutTheImageCapability(t *testing.T) {
	prov := &recordingProxyProvider{respBody: imagesRelayResponse}
	srv := newRedirectingTestServer(t, prov, "qwen-coder", "flux1-dev")

	rec := postImages(t, srv, `{"model":"dall-e-3","prompt":"a cat","n":1}`)

	requireRedirectedImagesRequest(t, srv, prov, rec, "dall-e-3", "flux1-dev")
}

// With no candidate that carries the image capability, the redirect declines
// and the client gets the ordinary unknown-model answer for the name it sent.
// The fallback here, qwen-coder, is callable for the images flavor (its
// application declares openai_images) but has no image verdict, so the
// capability gate would refuse it -- a 404 model_not_capable about a model the
// client never named, which is the outcome callableFor's contract calls a
// defect.
func TestImagesDoesNotRedirectAnUnknownModelToAFallbackWithoutTheImageCapability(t *testing.T) {
	prov := &recordingProxyProvider{respBody: imagesRelayResponse}
	srv := newRedirectingTestServer(t, prov, "", "qwen-coder")

	rec := postImages(t, srv, `{"model":"no-such-model","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	if got := lastUsageEvent(t, srv).Model; got != "no-such-model" {
		t.Fatalf("usage Model = %q, want the client's own no-such-model (no redirect)", got)
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
	}
}
