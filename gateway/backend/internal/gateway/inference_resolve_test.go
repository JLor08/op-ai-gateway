// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
)

// newTestServer builds a working *Server whose resolver can successfully
// route both "qwen3-32b" and "llama-70b" — the two models
// TestResolveTargetRecordsLastUsedModelOnlyOnChange switches between. The
// seeded application declares APIFlavors: []string{""} because the brief's
// test requests carry no APIFlavor (matching routing.NormalizeAPIFlavor's
// pass-through of an empty string).
func newTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()

	if err := routeStore.CreateAIServer(ctx, routing.AIServer{
		ID:           "srv_resolve_test",
		Name:         "Resolve Test Server",
		Domain:       "resolve.example.test",
		Provider:     routing.ProviderMock,
		Endpoint:     "mock://resolve",
		Status:       routing.ServerStatusActive,
		HealthStatus: routing.HealthHealthy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: "srv_resolve_test", ReportedAt: now, LatencyMS: 100, ErrorRate: 0,
		ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{
		ID:                 "app_resolve_test",
		ServerID:           "srv_resolve_test",
		Type:               routing.ProviderMock,
		Port:               8100,
		Scheme:             "http",
		APIFlavors:         []string{""},
		Priority:           10,
		Weight:             50,
		TimeoutMS:          30000,
		AffinityTTLSeconds: 1800,
		Status:             routing.ServerStatusActive,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for _, model := range []string{"qwen3-32b", "llama-70b"} {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{
			ID:               "route_" + model,
			ApplicationID:    "app_resolve_test",
			GatewayModelName: model,
			AppModelName:     model,
			Status:           routing.ServerStatusActive,
			CreatedAt:        now,
			UpdatedAt:        now,
		}); err != nil {
			t.Fatalf("CreateMapping(%s): %v", model, err)
		}
	}

	return New(ServerDeps{Routes: routeStore, Provider: provider.NewMock()})
}

// failingResolver returns a *routing.Resolver whose Resolve call always fails
// with err. Only routing.ErrNoModelRoute is supported: a zero-value Resolver
// has a nil store, which Resolve's first guard maps to exactly that error —
// sufficient for this file's failure-path test without standing up a second
// fake routing.Store.
func failingResolver(err error) *routing.Resolver {
	if err != routing.ErrNoModelRoute {
		panic(fmt.Sprintf("failingResolver: unsupported error %v (only routing.ErrNoModelRoute)", err))
	}
	return &routing.Resolver{}
}

func TestResolveTargetRecordsLastUsedModelOnlyOnChange(t *testing.T) {
	// A write per request would double the token table's write load on the hot
	// path; repeated requests for the same model must not write at all.
	var writes []string
	s := newTestServer(t)
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}

	if _, err := s.resolveTarget(context.Background(), token, inference.Request{Model: "qwen3-32b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("unchanged model wrote %v, want no write", writes)
	}

	if _, err := s.resolveTarget(context.Background(), token, inference.Request{Model: "llama-70b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 1 || writes[0] != "tok_1=llama-70b" {
		t.Fatalf("changed model wrote %v, want [tok_1=llama-70b]", writes)
	}
}

// TestResolveTargetSwallowsWriterErrorAndKeepsTarget covers the "a write
// error is logged and swallowed" constraint from resolveTarget's doc comment
// (inference_resolve.go): the marker is a convenience, never a reason to fail
// a request that already has a live target. A writer that itself errors must
// not surface that error to the caller, and the already-resolved target must
// still come back intact.
func TestResolveTargetSwallowsWriterErrorAndKeepsTarget(t *testing.T) {
	s := newTestServer(t)
	writerErr := errors.New("write failed")
	s.LastUsedModelWriter = func(_ context.Context, _, _ string) error {
		return writerErr
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}

	target, err := s.resolveTarget(context.Background(), token, inference.Request{Model: "llama-70b"})
	if err != nil {
		t.Fatalf("resolveTarget returned %v, want nil (writer error must be swallowed)", err)
	}
	if target.ServerID == "" {
		t.Fatalf("target = %#v, want the live target resolved before the writer ran", target)
	}
}

func TestResolveTargetDoesNotRecordOnFailure(t *testing.T) {
	// "Last used" means last SUCCESSFULLY routed — a typo or a dead model must
	// never become the redirect target for every later request.
	var writes []string
	s := newTestServer(t)
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, model)
		return nil
	}
	s.Resolver = failingResolver(routing.ErrNoModelRoute)

	if _, err := s.resolveTarget(context.Background(), auth.Token{ID: "tok_1"},
		inference.Request{Model: "nope"}); err == nil {
		t.Fatal("expected the resolver error to surface")
	}
	if len(writes) != 0 {
		t.Fatalf("failed resolve wrote %v, want no write", writes)
	}
}

// TestChatCompletionsRecordsLastUsedModel is the end-to-end regression guard
// for all three s.resolveTarget call sites (complete, tryProxyNative,
// beginStream): it drives a real HTTP request through ServeHTTP instead of
// calling resolveTarget directly, so a future revert of ANY of those three
// call sites back to the bare s.Resolver.Resolve (which every other gateway
// test would still pass, since LastUsedModelWriter is nil everywhere else)
// fails here. Exercises the non-streaming /v1/chat/completions path
// (inference_complete.go's complete) specifically because it reuses the
// existing NewTestServer + seedGatewayTestRoutes fixture (routable model
// "qwen-coder", token "tok_dev" / secret "dev-secret") verbatim — the
// cheapest of the three paths to stand up.
func TestChatCompletionsRecordsLastUsedModel(t *testing.T) {
	srv := NewTestServer()
	var writes []string
	srv.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, newJSONRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if len(writes) != 1 || writes[0] != "tok_dev=qwen-coder" {
		t.Fatalf("writes = %v, want [tok_dev=qwen-coder]", writes)
	}
}
