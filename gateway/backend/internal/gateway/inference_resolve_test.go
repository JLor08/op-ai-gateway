// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
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

	if _, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "qwen3-32b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("unchanged model wrote %v, want no write", writes)
	}

	if _, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "llama-70b"}); err != nil {
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

	target, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "llama-70b"})
	if err != nil {
		t.Fatalf("resolveTarget returned %v, want nil (writer error must be swallowed)", err)
	}
	if target.ServerID == "" {
		t.Fatalf("target = %#v, want the live target resolved before the writer ran", target)
	}
	// A FAILED write must not refresh the in-memory snapshot (issue #96): the
	// refresh sits in the write's success branch precisely so that a same-request
	// second resolve still retries. Refreshing here would suppress that retry and
	// silently drop the update.
	if token.LastUsedModel != "qwen3-32b" {
		t.Fatalf("token.LastUsedModel = %q after a failed write, want %q unchanged (a failed write must not refresh the snapshot)", token.LastUsedModel, "qwen3-32b")
	}
}

// TestResolveTargetRetriesWriteAfterFailedWrite pins the retry-on-failure
// semantics of the #96 snapshot refresh: token.LastUsedModel is refreshed ONLY
// after a successful write (inference_resolve.go's else-branch), so when the
// FIRST resolve's marker write fails the snapshot stays stale and the SAME
// request's second resolve (the non-native passthrough -> translate fallthrough)
// still attempts the write. A refactor that refreshed the snapshot
// unconditionally would suppress that retry and silently drop the update — and
// every other last-used-model test would still pass, because they all use an
// always-succeeding writer that never exercises this branch.
func TestResolveTargetRetriesWriteAfterFailedWrite(t *testing.T) {
	s := newTestServer(t)
	var attempts []string
	failNext := true
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		attempts = append(attempts, tokenID+"="+model)
		if failNext {
			failNext = false
			return errors.New("transient write failure")
		}
		return nil
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}

	// First resolve: the write fails, so the snapshot must stay stale.
	if _, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "llama-70b"}); err != nil {
		t.Fatalf("resolve #1: %v", err)
	}
	if token.LastUsedModel != "qwen3-32b" {
		t.Fatalf("after the failed write token.LastUsedModel = %q, want %q unchanged", token.LastUsedModel, "qwen3-32b")
	}

	// Second resolve on the SAME token: the guard is still true (stale snapshot),
	// so the write is retried — this time it succeeds and refreshes the snapshot.
	if _, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "llama-70b"}); err != nil {
		t.Fatalf("resolve #2: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != "tok_1=llama-70b" || attempts[1] != "tok_1=llama-70b" {
		t.Fatalf("write attempts = %v, want two [tok_1=llama-70b] (the failed first write must be retried by the second resolve)", attempts)
	}
	if token.LastUsedModel != "llama-70b" {
		t.Fatalf("after the successful retry token.LastUsedModel = %q, want %q (refresh only on success)", token.LastUsedModel, "llama-70b")
	}
}

// TestResolveTargetSkipsLastUsedModelForTokenlessPrincipal pins issue #27: a
// token-less session principal — sessionPrincipal (auth.go) leaves
// auth.Token.ID == "" — resolving a target must NOT attempt a last-used-model
// write. `last_used_model` is an api_tokens column, and a session principal has
// no such row, so the write is not merely doomed ("store: not found") but has no
// addressee at all; it fired once per portal-chat turn. The guard sits on the
// id, NOT the model, deliberately: a populated id that no longer resolves (a
// token deleted or expired between auth and the write) is a real signal and must
// still be reported.
func TestResolveTargetSkipsLastUsedModelForTokenlessPrincipal(t *testing.T) {
	var writes []string
	s := newTestServer(t)
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}
	// The shape sessionPrincipal produces: a user, but no token id and no
	// last-used marker.
	token := auth.Token{UserID: "usr_1"}

	if _, err := s.resolveTarget(context.Background(), &token, inference.Request{Model: "qwen3-32b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("token-less principal wrote %v, want no write (issue #27)", writes)
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

	if _, err := s.resolveTarget(context.Background(), &auth.Token{ID: "tok_1"},
		inference.Request{Model: "nope"}); err == nil {
		t.Fatal("expected the resolver error to surface")
	}
	if len(writes) != 0 {
		t.Fatalf("failed resolve wrote %v, want no write", writes)
	}
}

// TestChatCompletionsRecordsLastUsedModel is the end-to-end regression guard
// for inference_complete.go's s.resolveTarget call site (the non-streaming
// /v1/chat/completions path): it drives a real HTTP request through
// ServeHTTP instead of calling resolveTarget directly, so a future revert of
// THIS call site back to the bare s.Resolver.Resolve (which every other
// gateway test would still pass, since LastUsedModelWriter is nil everywhere
// else) fails here. It reuses the existing NewTestServer + seedGatewayTestRoutes
// fixture (routable model "qwen-coder", token "tok_dev" / secret "dev-secret")
// verbatim.
//
// The other two call sites — native_passthrough.go's tryProxyNative and
// stream_session.go's beginStream — are NOT covered by this test (they are
// different functions on a different code path); see
// TestTryProxyNativeRecordsLastUsedModel and TestBeginStreamRecordsLastUsedModel
// below for their own dedicated guards.
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

// TestTryProxyNativeRecordsLastUsedModel is the regression guard for
// native_passthrough.go's tryProxyNative — it drives that function directly
// (rather than a full HTTP round-trip through ServeHTTP, which would need a
// native-flagged application wired end to end) so a future revert of its
// `s.resolveTarget(...)` call back to the bare `s.Resolver.Resolve(...)`
// fails here. newTestServer's seeded application declares no native flags,
// so tryProxyNative resolves successfully (writing the marker) and then
// correctly falls through to "not enabled" and returns false — this test
// only cares about the write, not the passthrough decision itself.
func TestTryProxyNativeRecordsLastUsedModel(t *testing.T) {
	s := newTestServer(t)
	var writes []string
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}
	pf := preflight{Req: inference.Request{Model: "llama-70b"}}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := httptest.NewRecorder()

	if handled := s.tryProxyNative(w, r, &token, []byte("{}"), "", pf); handled {
		t.Fatalf("tryProxyNative returned true, want false (seeded application has no native flags)")
	}
	if len(writes) != 1 || writes[0] != "tok_1=llama-70b" {
		t.Fatalf("writes = %v, want [tok_1=llama-70b]", writes)
	}
}

// TestBeginStreamRecordsLastUsedModel is the regression guard for
// stream_session.go's beginStream — driven directly (an httptest.ResponseRecorder
// satisfies http.Flusher, and provider.Mock satisfies provider.StreamingClient,
// so no full SSE round-trip is needed) so a future revert of its
// `s.resolveTarget(...)` call back to the bare `s.Resolver.Resolve(...)`
// fails here.
func TestBeginStreamRecordsLastUsedModel(t *testing.T) {
	s := newTestServer(t)
	var writes []string
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}
	req := inference.Request{Model: "llama-70b"}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	ss, ok := s.beginStream(w, r, token, req, nil, "req_test", func(provider.Response) any { return nil })
	if !ok {
		t.Fatalf("beginStream returned ok=false, body=%s", w.Body.String())
	}
	defer ss.close()

	if len(writes) != 1 || writes[0] != "tok_1=llama-70b" {
		t.Fatalf("writes = %v, want [tok_1=llama-70b]", writes)
	}
}
