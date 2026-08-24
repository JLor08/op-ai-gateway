// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	"testing"
	"time"
)

// fakeFlushWriter is a minimal http.ResponseWriter + http.Flusher double for
// unit-testing nativeCopier without a real httptest.ResponseRecorder — it lets
// a single Write call be forced to fail (writeErr), which httptest's recorder
// cannot do, so the write-error-takes-precedence-over-read-error branch in
// nativeCopier.run (native_passthrough.go) can be exercised directly.
type fakeFlushWriter struct {
	header     http.Header
	buf        bytes.Buffer
	flushCount int
	writeErr   error
}

func (f *fakeFlushWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}

func (f *fakeFlushWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *fakeFlushWriter) WriteHeader(int) {}

func (f *fakeFlushWriter) Flush() { f.flushCount++ }

// twoChunkReader replays a fixed sequence of (chunk, error) reads, letting a
// test construct precisely the read-error/EOF/short-read shapes nativeCopier.run
// must handle — including a single Read call that returns BOTH n>0 bytes AND a
// non-nil, non-EOF error simultaneously (legal per io.Reader's contract), which
// is exactly the shape needed to prove write-error precedence over read-error.
type scriptedRead struct {
	chunk []byte
	err   error
}

type scriptedReader struct {
	reads []scriptedRead
	i     int
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.reads) {
		return 0, io.EOF
	}
	sr := r.reads[r.i]
	r.i++
	n := copy(p, sr.chunk)
	return n, sr.err
}

func newNativeCopier(w http.ResponseWriter, capBytes int) *nativeCopier {
	flusher, _ := w.(http.Flusher)
	return &nativeCopier{
		w:        w,
		rc:       http.NewResponseController(w),
		flusher:  flusher,
		respBuf:  &bytes.Buffer{},
		capBytes: capBytes,
	}
}

// TestNativeCopierRunHappyPathFlushesTeesAndCaps proves the normal multi-chunk
// copy path: every chunk reaches the client writer, the flusher is invoked once
// per chunk (so SSE frames are pushed live), the tee buffer accumulates the
// bytes for usage parsing, and a response body larger than capBytes stops
// growing the tee once the cap is crossed (the soft respBuf.Len() <= capBytes
// gate) without truncating what is written to the client.
func TestNativeCopierRunHappyPathFlushesTeesAndCaps(t *testing.T) {
	w := &fakeFlushWriter{}
	c := newNativeCopier(w, 5) // tiny cap to observe the tee stop mid-stream
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: []byte("hello"), err: nil},
		{chunk: []byte("world!"), err: nil},
		{chunk: nil, err: io.EOF},
	}}

	if err := c.run(body); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if got := w.buf.String(); got != "helloworld!" {
		t.Fatalf("client body = %q, want %q (every chunk written)", got, "helloworld!")
	}
	if w.flushCount != 2 {
		t.Fatalf("flushCount = %d, want 2 (one per non-empty chunk)", w.flushCount)
	}
	// respBuf.Len() was 0 (<=5) before "hello" -> written (len 5); before "world!"
	// respBuf.Len() is 5 (<=5) -> ALSO written once more (soft cap), then no
	// further chunks arrive. Net: the tee holds exactly "helloworld!" here since
	// there were only two data chunks, but the mechanism is the <= check at
	// write-time, not a hard truncation.
	if got := c.respBuf.String(); got != "helloworld!" {
		t.Fatalf("respBuf = %q, want %q", got, "helloworld!")
	}
}

// TestNativeCopierRunCapStopsTeeingOnceExceeded proves a THIRD chunk, once
// respBuf.Len() has already exceeded capBytes, is written to the client but no
// longer teed — the cap bounds the buffer usage/capture parsing sees, not what
// reaches the client.
func TestNativeCopierRunCapStopsTeeingOnceExceeded(t *testing.T) {
	w := &fakeFlushWriter{}
	c := newNativeCopier(w, 4)
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: []byte("12345"), err: nil}, // respBuf.Len()==0<=4 -> teed, now len=5
		{chunk: []byte("6789"), err: nil},  // respBuf.Len()==5>4 -> NOT teed
		{chunk: nil, err: io.EOF},
	}}

	if err := c.run(body); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if got := w.buf.String(); got != "123456789" {
		t.Fatalf("client body = %q, want %q (cap never affects the client copy)", got, "123456789")
	}
	if got := c.respBuf.String(); got != "12345" {
		t.Fatalf("respBuf = %q, want %q (second chunk skipped once over cap)", got, "12345")
	}
}

// TestNativeCopierRunWriteErrorTakesPrecedenceOverReadError proves that when a
// single Read call returns BOTH data and a non-EOF error, and writing that data
// to the client fails, run() returns the WRITE error — the read error is never
// even inspected. This is the exact precedence native_passthrough.go documents
// ("same error precedence — a write error terminates before the read error is
// inspected").
func TestNativeCopierRunWriteErrorTakesPrecedenceOverReadError(t *testing.T) {
	writeErr := errors.New("boom: write failed")
	readErr := errors.New("boom: read also failed")
	w := &fakeFlushWriter{writeErr: writeErr}
	c := newNativeCopier(w, 1<<20)
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: []byte("partial"), err: readErr}, // n>0 AND err!=nil in the same Read
	}}

	err := c.run(body)
	if !errors.Is(err, writeErr) {
		t.Fatalf("run() = %v, want the write error %v", err, writeErr)
	}
	if errors.Is(err, readErr) {
		t.Fatalf("run() returned the read error %v; the write error must win", err)
	}
}

// TestNativeCopierRunNonEOFReadErrorPropagates proves a read error with NO data
// (n==0) and not io.EOF is returned as-is.
func TestNativeCopierRunNonEOFReadErrorPropagates(t *testing.T) {
	readErr := errors.New("upstream connection reset")
	w := &fakeFlushWriter{}
	c := newNativeCopier(w, 1<<20)
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: nil, err: readErr},
	}}

	err := c.run(body)
	if !errors.Is(err, readErr) {
		t.Fatalf("run() = %v, want %v", err, readErr)
	}
	if w.buf.Len() != 0 {
		t.Fatalf("client body = %q, want empty (no data was ever read)", w.buf.String())
	}
}

// TestNativeCopierRunCleanEOFReturnsNil proves a plain io.EOF (after data or on
// an empty body) is the clean-exit case: run() returns nil, not io.EOF itself.
func TestNativeCopierRunCleanEOFReturnsNil(t *testing.T) {
	w := &fakeFlushWriter{}
	c := newNativeCopier(w, 1<<20)
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: nil, err: io.EOF},
	}}

	if err := c.run(body); err != nil {
		t.Fatalf("run() = %v, want nil on clean EOF", err)
	}
}

// TestNativeCopierWriteChunkWatchdogNilVsSet proves writeChunk's watchdog
// re-arm is correctly guarded: with watchdog==nil (a buffered, non-streaming
// exchange) it must not panic and simply skip the reset; with a real *time.Timer
// set it resets it (and best-effort sets a write deadline, ignored here since
// fakeFlushWriter doesn't implement SetWriteDeadline — http.ResponseController
// silently no-ops via ErrNotSupported, which writeChunk discards).
func TestNativeCopierWriteChunkWatchdogNilVsSet(t *testing.T) {
	t.Run("nil watchdog", func(t *testing.T) {
		w := &fakeFlushWriter{}
		c := newNativeCopier(w, 1<<20)
		c.watchdog = nil
		c.idle = 0
		if err := c.writeChunk([]byte("x")); err != nil {
			t.Fatalf("writeChunk() = %v, want nil", err)
		}
		if w.buf.String() != "x" {
			t.Fatalf("client body = %q, want %q", w.buf.String(), "x")
		}
	})
	t.Run("set watchdog", func(t *testing.T) {
		w := &fakeFlushWriter{}
		c := newNativeCopier(w, 1<<20)
		timer := time.NewTimer(time.Hour) // long enough it never fires in-test
		defer timer.Stop()
		c.watchdog = timer
		c.idle = time.Hour
		if err := c.writeChunk([]byte("y")); err != nil {
			t.Fatalf("writeChunk() = %v, want nil", err)
		}
		if w.buf.String() != "y" {
			t.Fatalf("client body = %q, want %q", w.buf.String(), "y")
		}
	})
}

// TestNativeCopierWriteChunkWriteErrorSkipsFlushAndTee proves a write failure
// short-circuits BEFORE the flush and the tee (neither must run once the bytes
// never reached the client).
func TestNativeCopierWriteChunkWriteErrorSkipsFlushAndTee(t *testing.T) {
	writeErr := errors.New("write failed")
	w := &fakeFlushWriter{writeErr: writeErr}
	c := newNativeCopier(w, 1<<20)

	err := c.writeChunk([]byte("z"))
	if !errors.Is(err, writeErr) {
		t.Fatalf("writeChunk() = %v, want %v", err, writeErr)
	}
	if w.flushCount != 0 {
		t.Fatalf("flushCount = %d, want 0 (must not flush after a write error)", w.flushCount)
	}
	if c.respBuf.Len() != 0 {
		t.Fatalf("respBuf.Len() = %d, want 0 (must not tee after a write error)", c.respBuf.Len())
	}
}

// canceledContextRequest returns an *http.Request whose Context() is already
// canceled, so r.Context().Err() != nil without needing a real client
// disconnect — exactly what nativeTerminalStatus's clientGone branch checks.
func canceledContextRequest() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
}

// TestNativeTerminalStatusClassification exercises every classification arm of
// nativeTerminalStatus in the documented precedence order (idle timeout >
// client disconnect > copy error > non-2xx upstream > success), proving each
// one in isolation returns its own (status, errorCode) pair.
func TestNativeTerminalStatusClassification(t *testing.T) {
	srv := NewTestServer()
	pfReq := inference.Request{Model: "m", APIFlavor: "openai_responses"}

	cases := []struct {
		name           string
		req            *http.Request
		upstreamStatus int
		idledOut       bool
		copyErr        error
		wantStatus     string
		wantCode       string
	}{
		{
			name:           "idle timeout wins over everything else",
			req:            canceledContextRequest(), // also client-gone-shaped, but idledOut must win
			upstreamStatus: 200,
			idledOut:       true,
			copyErr:        errors.New("irrelevant"),
			wantStatus:     "error",
			wantCode:       "provider.stream_idle_timeout",
		},
		{
			name:           "client disconnected",
			req:            canceledContextRequest(),
			upstreamStatus: 200,
			idledOut:       false,
			copyErr:        nil,
			wantStatus:     "error",
			wantCode:       "provider.client_disconnected",
		},
		{
			name:           "copy error",
			req:            httptest.NewRequest(http.MethodPost, "/v1/responses", nil),
			upstreamStatus: 200,
			idledOut:       false,
			copyErr:        errors.New("stream broke"),
			wantStatus:     "error",
			wantCode:       "provider.stream_copy_error",
		},
		{
			name:           "non-2xx upstream",
			req:            httptest.NewRequest(http.MethodPost, "/v1/responses", nil),
			upstreamStatus: 502,
			idledOut:       false,
			copyErr:        nil,
			wantStatus:     "error",
			wantCode:       "upstream.502",
		},
		{
			name:           "success",
			req:            httptest.NewRequest(http.MethodPost, "/v1/responses", nil),
			upstreamStatus: 200,
			idledOut:       false,
			copyErr:        nil,
			wantStatus:     "success",
			wantCode:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := srv.nativeTerminalStatus(tc.req, tc.upstreamStatus, pfReq, "srv-test", tc.idledOut, tc.copyErr, time.Now())
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("nativeTerminalStatus() = (%q, %q), want (%q, %q)", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// nonProxyCapableProvider implements provider.Client + provider.StreamingClient
// but deliberately NOT provider.NativeProxyClient, so tryProxyNative's type
// assertion fails even though the resolved application has native passthrough
// enabled — the "provider.unavailable" 502 branch in proxyNative.
type nonProxyCapableProvider struct{}

func (nonProxyCapableProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{Text: "translated"}, nil
}

func (nonProxyCapableProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{}})
}

// TestProxyNativeProviderUnavailableReturns502 proves that when the resolved
// application has native passthrough enabled but the configured provider does
// not implement provider.NativeProxyClient, proxyNative writes a 502
// provider.unavailable response and records the failure as usage — instead of
// panicking on the failed type assertion.
func TestProxyNativeProviderUnavailableReturns502(t *testing.T) {
	srv := newNativeProxyTestServer(nonProxyCapableProvider{}, true, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "provider.unavailable" {
		t.Fatalf("error code = %q, want provider.unavailable", code)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Status != "error" || events[0].ErrorCode != "provider.unavailable" {
		t.Fatalf("usage event = %+v, want status=error error_code=provider.unavailable", events[0])
	}
}

// erroringProxyProvider implements NativeProxyClient but always fails the
// upstream call itself, exercising proxyNative's pre-response failure branch
// (nothing written to the client yet -> a JSON error is returned and recorded).
type erroringProxyProvider struct {
	err error
}

func (erroringProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (erroringProxyProvider) CompleteStream(context.Context, routing.Target, inference.Request, provider.StreamEmit) error {
	return nil
}

func (p erroringProxyProvider) ProxyNative(context.Context, routing.Target, string, []byte) (*provider.ProxyResponse, error) {
	return nil, p.err
}

// TestProxyNativeUpstreamCallFailureReturnsJSONError proves a ProxyNative
// transport failure (upstream unreachable) maps through completionErrorCode /
// completionHTTPStatus and writeCompletionErrorCaptured, and is recorded as a
// failed usage event with the JSON content type (no partial stream was ever
// started).
func TestProxyNativeUpstreamCallFailureReturnsJSONError(t *testing.T) {
	srv := newNativeProxyTestServer(erroringProxyProvider{err: errors.New("dial tcp: connection refused")}, true, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code < 500 {
		t.Fatalf("status = %d, want a 5xx upstream failure, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json (pre-response failure)", ct)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Status != "error" {
		t.Fatalf("usage status = %q, want error", events[0].Status)
	}
	if events[0].ContentType != jsonContentType {
		t.Fatalf("usage content_type = %q, want %q", events[0].ContentType, jsonContentType)
	}
}

// noContentTypeProxyProvider returns an upstream response with NO Content-Type
// header set at all, exercising proxyNative's fallback-to-jsonContentType branch.
type noContentTypeProxyProvider struct {
	body string
}

func (noContentTypeProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (noContentTypeProxyProvider) CompleteStream(context.Context, routing.Target, inference.Request, provider.StreamEmit) error {
	return nil
}

func (p noContentTypeProxyProvider) ProxyNative(context.Context, routing.Target, string, []byte) (*provider.ProxyResponse, error) {
	return &provider.ProxyResponse{
		StatusCode: 200,
		Header:     http.Header{}, // deliberately no Content-Type
		Body:       io.NopCloser(strings.NewReader(p.body)),
	}, nil
}

// TestProxyNativeDefaultsContentTypeWhenUpstreamOmitsIt proves that when the
// upstream response carries no Content-Type header, the client response and
// the recorded usage event both fall back to jsonContentType rather than
// leaving it blank.
func TestProxyNativeDefaultsContentTypeWhenUpstreamOmitsIt(t *testing.T) {
	srv := newNativeProxyTestServer(noContentTypeProxyProvider{body: `{"output_text":"hi"}`}, true, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Fatalf("content-type = %q, want %q (fallback)", ct, jsonContentType)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].ContentType != jsonContentType {
		t.Fatalf("usage content_type = %q, want %q", events[0].ContentType, jsonContentType)
	}
}

// TestProxyNativeRecordsPreOverrideRequestedModel proves the native-passthrough
// path records the model the CLIENT sent (issue #7's requested_model), not only
// the effective post-override one. Unlike the translate path — where the field
// rides along through mergeInto — proxyNative builds its own inference.Request
// literal by hand-copying fields off the preflight request (see
// native_passthrough.go), so nothing structural keeps RequestedModel populated:
// dropping that single line from the literal has to fail a test. A token whose
// ModelOverride rewrites the client's "client-model" into the routable gateway
// model "gw-model" makes requested and effective differ, so this pins the
// PRE-override value rather than merely "some model name got recorded".
func TestProxyNativeRecordsPreOverrideRequestedModel(t *testing.T) {
	prov := &recordingProxyProvider{respBody: `{"output_text":"hi"}`}
	srv := newNativeProxyTestServer(prov, true, false)
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{
		ID: "tok_ovr", UserID: "usr_dev", Name: "Override Token", Active: true,
		Scopes: []string{"gateway:use", "admin"}, ModelOverride: "gw-model",
	}, "ovr-secret")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"client-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ovr-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1 (the request must take the native path, not translate)", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Model != "gw-model" {
		t.Fatalf("Model = %q, want gw-model (the token override drove routing)", events[0].Model)
	}
	if events[0].RequestedModel != "client-model" {
		t.Fatalf("RequestedModel = %q, want client-model (the pre-override name the client sent)", events[0].RequestedModel)
	}
}

// newCapAdmissionTestServer builds a *Server with a single native-passthrough
// application whose mapping caps concurrency at 1 (ModelMapping.MaxConcurrency)
// and whose AdmissionQueueTimeoutSeconds is a short, real 1 second — enough for
// tryProxyNative's admission-queue-timeout branch (native_passthrough.go's
// "errors.Is(err, routing.ErrAdmissionQueueTimeout)" case) to genuinely fire
// within the test's real wall-clock budget once the sole slot is occupied.
func newCapAdmissionTestServer(t *testing.T, prov provider.Client) (*Server, string) {
	t.Helper()
	const serverID = "srv-native-cap"
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
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: serverID, Name: "Native Cap Upstream", Domain: "native-cap.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://native-cap.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{
		ID: "app-native-cap", ServerID: serverID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000,
		Status: routing.ServerStatusActive, NativeResponses: true,
		AdmissionQueueTimeoutSeconds: 1, // real, short: the test genuinely waits out one timeout
	}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map-native-cap", ApplicationID: "app-native-cap", GatewayModelName: "gw-model-cap", AppModelName: "gw-model-cap", Status: routing.ServerStatusActive, MaxConcurrency: 1}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: serverID, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	}), serverID
}

// TestTryProxyNativeAdmissionQueueTimeoutRecordsFailure proves that when
// Resolve's admission queue times out waiting for a free concurrency slot
// (CP4), tryProxyNative treats it as terminal — a 503 is written to the
// client and the failure is recorded as usage — rather than falling through
// to the translate path (which would re-resolve and wait on the queue a
// second time). The sole concurrency slot is occupied by a synthetic
// ActiveRequest for the target server, and AdmissionQueueTimeoutSeconds=1 is
// genuinely real (this test waits out one real ~1s timeout rather than
// faking the resolver's clock).
func TestTryProxyNativeAdmissionQueueTimeoutRecordsFailure(t *testing.T) {
	srv, serverID := newCapAdmissionTestServer(t, &recordingProxyProvider{respBody: "unused"})
	srv.Active.Add(ActiveRequest{ID: "occupier", ServerID: serverID, UserID: "usr_other", StartedAt: time.Now()})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model-cap","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", rec.Code, rec.Body.String())
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %s, want >= ~1s (the request must actually wait out the admission-queue timeout, not fail some earlier/unrelated way)", elapsed)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].Status != "error" || events[0].ErrorCode != "routing.admission_queue_timeout" {
		t.Fatalf("usage event = %+v, want status=error error_code=routing.admission_queue_timeout", events[0])
	}
	if events[0].ContentType != jsonContentType {
		t.Fatalf("usage content_type = %q, want %q", events[0].ContentType, jsonContentType)
	}
}
