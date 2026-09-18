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
	if !strings.Contains(rec.Body.String(), "routing.model_not_capable") {
		t.Fatalf("body = %s, want the capability code, not an unknown-model code", rec.Body.String())
	}
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
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo", ServerID: "srv-sd-turbo", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
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
	if !strings.Contains(rec.Body.String(), imagesResponseFormatUnsupported) {
		t.Fatalf("body = %s, want %s", rec.Body.String(), imagesResponseFormatUnsupported)
	}
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
		if !strings.Contains(rec.Body.String(), imagesResponseFormatUnsupported) {
			t.Fatalf("%s: body = %s, want %s", body, rec.Body.String(), imagesResponseFormatUnsupported)
		}
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
	if !strings.Contains(rec.Body.String(), imagesStreamUnsupported) {
		t.Fatalf("body = %s, want %s", rec.Body.String(), imagesStreamUnsupported)
	}

	// stream:false and a non-boolean stream: the first must pass validation (it
	// then hits the capability refusal, 404, like every other request in this
	// file against the no-verdict qwen-coder), the second must not.
	if got := postImages(t, NewTestServer(), `{"model":"qwen-coder","prompt":"a cat","stream":false}`); got.Code != http.StatusNotFound {
		t.Fatalf("stream:false status = %d, want 404 (validation passed, routing refused); body = %s", got.Code, got.Body.String())
	}
	if got := postImages(t, NewTestServer(), `{"model":"qwen-coder","prompt":"a cat","stream":"true"}`); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), imagesStreamUnsupported) {
		t.Fatalf(`stream:"true" status = %d, body = %s, want 400 %s`, got.Code, got.Body.String(), imagesStreamUnsupported)
	}
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
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo-fake", ServerID: "srv-sd-turbo-fake", Type: routing.ProviderVLLM, Port: 80, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
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
