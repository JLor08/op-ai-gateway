// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
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
	"sync"
	"testing"
	"time"
)

// blockingProvider is a test provider whose Complete blocks on release until the
// test signals, closing started once it is actually in flight. It implements the
// StreamingClient interface too so completeStream reaches its flusher check
// (CompleteStream is never invoked on the no-flusher fallback path). All
// synchronization is via channels — no sleeps, no timing races.
type blockingProvider struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingProvider) enter() { p.once.Do(func() { close(p.started) }) }

func (p *blockingProvider) Complete(ctx context.Context, target routing.Target, req inference.Request) (provider.Response, error) {
	p.enter()
	select {
	case <-p.release:
		return provider.Response{Text: "ok", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
	case <-ctx.Done():
		return provider.Response{}, ctx.Err()
	}
}

func (p *blockingProvider) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	p.enter()
	select {
	case <-p.release:
		return emit(inference.StreamEvent{Type: inference.StreamEventCompleted})
	case <-ctx.Done():
		return ctx.Err()
	}
}

var (
	_ provider.Client          = (*blockingProvider)(nil)
	_ provider.StreamingClient = (*blockingProvider)(nil)
)

// nonFlushingWriter wraps a ResponseRecorder but deliberately does NOT expose a
// Flush method, so completeStream's `w.(http.Flusher)` assertion fails and takes
// the buffered-completion fallback.
type nonFlushingWriter struct{ rec *httptest.ResponseRecorder }

func (w *nonFlushingWriter) Header() http.Header         { return w.rec.Header() }
func (w *nonFlushingWriter) Write(b []byte) (int, error) { return w.rec.Write(b) }
func (w *nonFlushingWriter) WriteHeader(status int)      { w.rec.WriteHeader(status) }

// TestActiveRegistryTracksLiveRequest is the integration test: it drives a real
// inference request through the HTTP layer against a provider that blocks
// mid-Complete, asserts the in-flight request appears in the registry AND at the
// /api/portal/usage/active endpoint, then unblocks and asserts it is gone.
func TestActiveRegistryTracksLiveRequest(t *testing.T) {
	prov := newBlockingProvider()
	srv := newStreamTestServerWithProvider(prov)

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		close(reqDone)
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider Complete was never entered")
	}

	// In flight: the registry has exactly the one request, with the right metadata.
	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	row := snap[0]
	if row.UserID != "usr_dev" || row.Model != "qwen-coder" || row.Stream || row.ReqPath != "/openai/v1/chat/completions" {
		t.Fatalf("in-flight row wrong: %#v", row)
	}
	// No token override here, so the requested model equals the effective one.
	if row.RequestedModel != "qwen-coder" {
		t.Fatalf("in-flight RequestedModel = %q, want qwen-coder (no override in play): %#v", row.RequestedModel, row)
	}
	if row.TokenID != "tok_dev" || row.TokenName != "Dev Token" {
		t.Fatalf("in-flight row token metadata wrong: %#v", row)
	}

	// And it is visible through the HTTP endpoint.
	getReq := httptest.NewRequest(http.MethodGet, "/api/portal/usage/active", nil)
	getReq.Header.Set("Authorization", "Bearer dev-secret")
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("active endpoint status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var body struct {
		Data []activeRequestDTO `json:"data"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, getRec.Body.String())
	}
	if len(body.Data) != 1 || body.Data[0].Model != "qwen-coder" || body.Data[0].ID != row.ID {
		t.Fatalf("active endpoint during flight = %#v", body.Data)
	}

	// Unblock: the request finishes and the entry is removed.
	close(prov.release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after release")
	}
	if snap := srv.Active.Snapshot(); len(snap) != 0 {
		t.Fatalf("post-completion snapshot = %d rows, want 0 (%#v)", len(snap), snap)
	}
}

// TestNoDoubleRegisterOnNoFlusherFallback drives a stream request through a
// non-flushing ResponseWriter. completeStream must NOT register at its head; only
// the delegated complete registers, so exactly one active entry exists in flight
// and it is a non-stream entry.
func TestNoDoubleRegisterOnNoFlusherFallback(t *testing.T) {
	prov := newBlockingProvider()
	srv := newStreamTestServerWithProvider(prov)

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer dev-secret")
		w := &nonFlushingWriter{rec: httptest.NewRecorder()}
		srv.ServeHTTP(w, req)
		close(reqDone)
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider Complete was never entered (fallback path not taken?)")
	}

	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("no-flusher fallback registered %d active entries, want exactly 1 (%#v)", len(snap), snap)
	}
	if snap[0].Stream {
		t.Fatalf("fallback path must register a non-stream entry, got Stream=true: %#v", snap[0])
	}

	close(prov.release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after release")
	}
	if snap := srv.Active.Snapshot(); len(snap) != 0 {
		t.Fatalf("post-completion snapshot = %d rows, want 0 (%#v)", len(snap), snap)
	}
}

// TestActiveRegistryTracksLiveStreamRequest exercises the real streaming path
// (flusher present): the entry is registered after the flusher check with
// Stream=true, and removed when the stream ends.
func TestActiveRegistryTracksLiveStreamRequest(t *testing.T) {
	prov := newBlockingProvider()
	srv := newStreamTestServerWithProvider(prov)

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder() // ResponseRecorder IS an http.Flusher
		srv.ServeHTTP(rec, req)
		close(reqDone)
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider CompleteStream was never entered")
	}

	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight stream snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	if !snap[0].Stream {
		t.Fatalf("streaming path must register Stream=true: %#v", snap[0])
	}
	// The streaming path builds its own ActiveRequest literal, so it needs its
	// own guard that the requested model is carried (no override here, so it
	// equals the effective model).
	if snap[0].RequestedModel != "qwen-coder" {
		t.Fatalf("streaming path RequestedModel = %q, want qwen-coder: %#v", snap[0].RequestedModel, snap[0])
	}

	close(prov.release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request did not finish after release")
	}
	if snap := srv.Active.Snapshot(); len(snap) != 0 {
		t.Fatalf("post-stream snapshot = %d rows, want 0 (%#v)", len(snap), snap)
	}
}

// newOverrideTestServerWithProvider mirrors newStreamTestServerWithProvider but its
// token carries a per-model override map, so a request for "gpt-oss-20b" is
// served as "qwen-coder" (the only routable model in seedGatewayTestRoutes).
// That divergence is what makes the pre-override assertion below meaningful.
func newOverrideTestServerWithProvider(t *testing.T, prov provider.Client) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ovr", UserID: "usr_dev", Name: "Override Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now, ModelOverrideMap: `{"gpt-oss-20b":"qwen-coder"}`}, "ovr-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// TestActiveRegistryTracksPreOverrideRequestedModel is the load-bearing case for
// the running-connections view: with a token override rewriting the model, the
// in-flight row must carry BOTH names -- the effective model it routed to and the
// pre-override name the client actually asked for -- and the /usage/active DTO
// must expose them, so an operator can tell the two apart while the request is
// still running rather than only afterwards in the activity list.
func TestActiveRegistryTracksPreOverrideRequestedModel(t *testing.T) {
	prov := newBlockingProvider()
	srv := newOverrideTestServerWithProvider(t, prov)

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer ovr-secret")
		srv.ServeHTTP(httptest.NewRecorder(), req)
		close(reqDone)
	}()
	// Release from a defer so a failing assertion below cannot leave the
	// provider (and this goroutine) blocked for the rest of the package run.
	// It also asserts the completion path: the entry must be gone afterwards.
	defer func() {
		close(prov.release)
		select {
		case <-reqDone:
		case <-time.After(2 * time.Second):
			t.Error("request did not finish after release")
			return
		}
		if snap := srv.Active.Snapshot(); len(snap) != 0 {
			t.Errorf("post-completion snapshot = %d rows, want 0 (%#v)", len(snap), snap)
		}
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider Complete was never entered")
	}

	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	if got := snap[0].Model; got != "qwen-coder" {
		t.Fatalf("in-flight Model = %q, want qwen-coder (the override target)", got)
	}
	if got := snap[0].RequestedModel; got != "gpt-oss-20b" {
		t.Fatalf("in-flight RequestedModel = %q, want gpt-oss-20b (what the client sent)", got)
	}

	// The same divergence must survive into the endpoint's DTO.
	getReq := httptest.NewRequest(http.MethodGet, "/api/portal/usage/active", nil)
	getReq.Header.Set("Authorization", "Bearer ovr-secret")
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("active endpoint status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var body struct {
		Data []activeRequestDTO `json:"data"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, getRec.Body.String())
	}
	if len(body.Data) != 1 {
		t.Fatalf("active endpoint rows = %d, want 1 (%#v)", len(body.Data), body.Data)
	}
	if body.Data[0].Model != "qwen-coder" || body.Data[0].RequestedModel != "gpt-oss-20b" {
		t.Fatalf("DTO model pair = (%q, %q), want (qwen-coder, gpt-oss-20b)", body.Data[0].Model, body.Data[0].RequestedModel)
	}
}
