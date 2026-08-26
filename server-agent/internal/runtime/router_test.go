// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkHeartbeat sets heartbeatInterval to d for the duration of the test,
// restoring the original value afterward -- the shrinkTimings convention
// (manager_test.go), applied to router.go's own package-level timing var.
func shrinkHeartbeat(t *testing.T, d time.Duration) {
	t.Helper()
	orig := heartbeatInterval
	heartbeatInterval = d
	t.Cleanup(func() { heartbeatInterval = orig })
}

// decodeJSON decodes r's contents as T, failing the test on any error.
func decodeJSON[T any](t *testing.T, r io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return v
}

// readUntilContains reads from r, accumulating bytes, until the buffer
// contains want or timeout elapses (failing the test in the latter case). It
// exists because a streaming response can carry an unpredictable number of
// `: keepalive` comments before the real data a test cares about, so reading
// a fixed byte count (as TestRouterNoResponseBuffering's plain-path test
// safely does, since that path never heartbeats) is not reliable here.
func readUntilContains(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var buf []byte
	tmp := make([]byte, 256)
	for {
		if strings.Contains(string(buf), want) {
			return string(buf)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting to read %q; got so far: %q", timeout, want, buf)
		}
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			t.Fatalf("read (looking for %q): %v; got so far: %q", want, err, buf)
		}
	}
}

// countingManager wraps a managerPort (in every test below, a real
// *Manager), counting how many times ANY release() func EnsureRunning ever
// returned was actually invoked. This is deliberately independent of the
// real Manager's own internal sync.Once idempotency guard (EnsureRunning's
// doc comment): that guard would silently absorb a double-release bug
// introduced by the ROUTER's own code, which is exactly the class of bug
// TestRouterReleaseCalledExactlyOnce targets -- counted from the fake's
// side, not the real Manager's, per the brief's explicit instruction.
type countingManager struct {
	inner managerPort
	calls atomic.Int64
}

func (c *countingManager) EnsureRunning(ctx context.Context, upstreamModel string) (string, func(), error) {
	endpoint, release, err := c.inner.EnsureRunning(ctx, upstreamModel)
	if release == nil {
		return endpoint, nil, err
	}
	return endpoint, func() {
		c.calls.Add(1)
		release()
	}, err
}

func (c *countingManager) LoadedModels() []string { return c.inner.LoadedModels() }
func (c *countingManager) Status() []Status       { return c.inner.Status() }

// ---------------------------------------------------------------------------

// TestRouterHealthAlwaysAnswers proves /health, /v1/health and /running
// answer promptly even while a model-routed request for a DIFFERENT,
// slow-to-start model is blocked inside EnsureRunning -- the property the
// gateway's 3s/one-cycle application health probe depends on (package doc,
// router.go).
func TestRouterHealthAlwaysAnswers(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(300*time.Millisecond, 0, 0, "")
	m.Apply(Config{Specs: []Spec{spec}})

	rt := NewRouter(m)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, req)
	}()

	waitUntil(t, 2*time.Second, "spec-a starting", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStarting
	})

	for _, path := range []string{"/health", "/v1/health", "/running"} {
		start := time.Now()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, req)
		elapsed := time.Since(start)
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, w.Code)
		}
		if elapsed > 200*time.Millisecond {
			t.Errorf("%s took %s while spec-a was still starting (300ms health-delay); want it to answer promptly regardless", path, elapsed)
		}
	}
}

func TestRouterRunningLlamaSwapShape(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	defer release()

	rt := NewRouter(m)
	req := httptest.NewRequest(http.MethodGet, "/running", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /running status = %d, want 200", w.Code)
	}
	got := decodeJSON[runningResponse](t, w.Body)
	if len(got.Running) != 1 || got.Running[0].Model != "model-a" || got.Running[0].State != "ready" {
		t.Fatalf("GET /running body = %+v, want exactly one {model-a, ready} entry", got)
	}
}

// TestRouterModelsListsAllManagedSpecs proves /v1/models lists every managed
// spec, including ones that were never started (cold) -- they are servable
// on demand, and the gateway's model-sync health mode and model listing both
// depend on the full set appearing here.
func TestRouterModelsListsAllManagedSpecs(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	specA := baseSpec("spec-a", "model-a")
	specB := baseSpec("spec-b", "model-b")
	m.Apply(Config{Specs: []Spec{specA, specB}}) // neither Pinned nor force_running: both stay cold

	rt := NewRouter(m)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200", w.Code)
	}
	got := decodeJSON[modelsResponse](t, w.Body)
	if got.Object != "list" {
		t.Errorf("object = %q, want %q", got.Object, "list")
	}
	ids := map[string]bool{}
	for _, e := range got.Data {
		if e.Object != "model" {
			t.Errorf("entry %+v: object = %q, want %q", e, e.Object, "model")
		}
		ids[e.ID] = true
	}
	if !ids["model-a"] || !ids["model-b"] {
		t.Fatalf("GET /v1/models data = %+v, want it to include cold model-a and model-b", got.Data)
	}
	for _, st := range m.Status() {
		if st.State != StateStopped {
			t.Fatalf("spec %s state = %s, want stopped -- this test's whole point is that /v1/models lists COLD specs", st.SpecID, st.State)
		}
	}
}

// TestRouterProxiesByModel proves a request is routed to the child selected
// by its `model` field, with method/path/body preserved: the stub's /v1/echo
// endpoint only ever answers 200 on that exact path, and the response body
// must equal the request body byte for byte.
func TestRouterProxiesByModel(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	rt := NewRouter(m)
	body := `{"model":"model-a","hello":"world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != body {
		t.Errorf("body = %q, want the exact request body %q echoed back verbatim", w.Body.String(), body)
	}
}

func TestRouterUnknownModel404(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{}) // nothing managed

	rt := NewRouter(m)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ghost-model"}`))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	got := decodeJSON[errorEnvelope](t, w.Body)
	if got.Error.Code != "runtime.model_not_managed" {
		t.Errorf("error code = %q, want %q", got.Error.Code, "runtime.model_not_managed")
	}
}

// TestRouterNonStreamErrorCodes is the table over the sentinel->status map
// (design doc §6.5) for a non-streaming request, where every failure is
// still reportable as a real HTTP status (no heartbeats have begun to
// commit anything).
func TestRouterNonStreamErrorCodes(t *testing.T) {
	skipOnWindows(t)

	cases := []struct {
		name       string
		setup      func(t *testing.T) (m *Manager, model string)
		wantStatus int
		wantCode   string
	}{
		{
			name: "model_not_managed",
			setup: func(t *testing.T) (*Manager, string) {
				m := newTestManager(t, allowlistPolicy())
				m.Apply(Config{})
				return m, "ghost-model"
			},
			wantStatus: http.StatusNotFound,
			wantCode:   "runtime.model_not_managed",
		},
		{
			name: "not_permitted",
			setup: func(t *testing.T) (*Manager, string) {
				m := newTestManager(t, LocalPolicy{AllowedBinaries: []string{"/not/the/stub"}})
				spec := baseSpec("spec-a", "model-a")
				m.Apply(Config{Specs: []Spec{spec}})
				return m, "model-a"
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   "runtime.not_permitted",
		},
		{
			name: "start_failed",
			setup: func(t *testing.T) (*Manager, string) {
				badBinary := filepath.Join(t.TempDir(), "does-not-exist")
				m := newTestManager(t, LocalPolicy{AllowedBinaries: []string{badBinary}})
				spec := baseSpec("spec-a", "model-a")
				spec.Binary = badBinary
				m.Apply(Config{Specs: []Spec{spec}})
				return m, "model-a"
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   "runtime.start_failed",
		},
		{
			name: "start_timeout",
			setup: func(t *testing.T) (*Manager, string) {
				shrinkTimings(t)
				m := newTestManager(t, allowlistPolicy())
				spec := baseSpec("spec-a", "model-a")
				spec.Args = stubArgs(10*time.Second, 0, 0, "") // health never becomes ready in time
				spec.StartupTimeoutSeconds = 1
				m.Apply(Config{Specs: []Spec{spec}})
				return m, "model-a"
			},
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "runtime.start_timeout",
		},
		{
			name: "admission_blocked",
			setup: func(t *testing.T) (*Manager, string) {
				shrinkTimings(t)
				m := newTestManager(t, allowlistPolicy())
				specA := baseSpec("spec-a", "model-a")
				specA.Pinned = true
				specB := baseSpec("spec-b", "model-b")
				specB.AdmissionWaitTimeoutSeconds = 1
				m.Apply(Config{Specs: []Spec{specA, specB}})
				waitUntil(t, 3*time.Second, "pinned spec-a running", func() bool {
					st := statusFor(m, "spec-a")
					return st != nil && st.State == StateRunning
				})
				return m, "model-b"
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "runtime.admission_blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, model := tc.setup(t)
			rt := NewRouter(m)
			body := fmt.Sprintf(`{"model":%q}`, model)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			got := decodeJSON[errorEnvelope](t, w.Body)
			if got.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q", got.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestRouterNoResponseBuffering proves the plain (non-streaming) proxy path
// never buffers a response: the upstream writes and flushes one chunk,
// pauses, then writes a second one, and the client must observe the first
// chunk well before the pause elapses -- a fully-buffered implementation
// would instead deliver both chunks together only after the full pause.
func TestRouterNoResponseBuffering(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	const gap = 300 * time.Millisecond
	body := `{"model":"model-a"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chunked?gap="+gap.String(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("chunk1"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	firstAt := time.Since(start)
	if string(buf) != "chunk1" {
		t.Fatalf("first chunk = %q, want %q", buf, "chunk1")
	}
	if firstAt > gap/2 {
		t.Fatalf("first chunk arrived after %s (of a %s gap) -- response looks buffered", firstAt, gap)
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	secondAt := time.Since(start)
	if string(rest) != "chunk2" {
		t.Fatalf("second chunk = %q, want %q", rest, "chunk2")
	}
	if secondAt-firstAt < gap/2 {
		t.Fatalf("second chunk arrived only %s after the first (of a %s gap) -- response looks buffered", secondAt-firstAt, gap)
	}
}

// TestRouterStreamHeartbeats proves a streaming request against a
// slow-to-become-healthy model sees `: keepalive` comment(s) before any real
// data, and that 200 + SSE headers are committed immediately (before the
// outcome of the cold start is known at all).
func TestRouterStreamHeartbeats(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	shrinkHeartbeat(t, 20*time.Millisecond)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(150*time.Millisecond, 0, 0, "") // several heartbeat ticks' worth of cold start
	m.Apply(Config{Specs: []Spec{spec}})

	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	body := `{"model":"model-a","stream":true}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/echo", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed before the cold-start outcome is known)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	all, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(all)
	if !strings.Contains(text, ": keepalive\n\n") {
		t.Fatalf("body = %q, want at least one keepalive comment", text)
	}
	if !strings.HasSuffix(text, body) {
		t.Fatalf("body = %q, want it to end with the verbatim echoed request body %q", text, body)
	}
	if strings.Index(text, ": keepalive") > strings.Index(text, body) {
		t.Fatalf("body = %q, want keepalive(s) to precede the echoed data", text)
	}
}

// TestRouterStreamSplicesUpstream proves the upstream's own SSE-shaped lines
// arrive at the client byte-for-byte, not re-framed by the router, after any
// cold-start heartbeats.
func TestRouterStreamSplicesUpstream(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	shrinkHeartbeat(t, 20*time.Millisecond)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(80*time.Millisecond, 0, 0, "")
	m.Apply(Config{Specs: []Spec{spec}})

	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	sseChunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	sseChunk2 := "data: [DONE]\n\n"
	target := srv.URL + "/v1/chunked?c1=" + url.QueryEscape(sseChunk1) + "&c2=" + url.QueryEscape(sseChunk2)
	body := `{"model":"model-a","stream":true}`
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	all, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(all)
	wantTail := sseChunk1 + sseChunk2
	if !strings.HasSuffix(text, wantTail) {
		t.Fatalf("body = %q, want it to end with the exact upstream SSE bytes %q verbatim (not re-framed)", text, wantTail)
	}
	if !strings.Contains(text, ": keepalive") {
		t.Errorf("body = %q, want at least one keepalive before the upstream data", text)
	}
}

// TestRouterStreamStartFailureTerminalFrame proves that once a streaming
// request's 200 is committed, a start failure (here: a binary that is
// allowlisted but does not exist on disk, so exec fails synchronously) is
// reported as a terminal SSE error frame carrying runtime.start_failed --
// never as an HTTP status, since none is available any more.
func TestRouterStreamStartFailureTerminalFrame(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	shrinkHeartbeat(t, 20*time.Millisecond)
	badBinary := filepath.Join(t.TempDir(), "does-not-exist")
	m := newTestManager(t, LocalPolicy{AllowedBinaries: []string{badBinary}})
	spec := baseSpec("spec-a", "model-a")
	spec.Binary = badBinary
	m.Apply(Config{Specs: []Spec{spec}})

	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	body := `{"model":"model-a","stream":true}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/echo", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (already committed before the start failure)", resp.StatusCode)
	}
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(all)
	if !strings.HasPrefix(text, "data: ") {
		t.Fatalf("body = %q, want the terminal frame written as an SSE `data:` line", text)
	}
	if !strings.Contains(text, `"code":"runtime.start_failed"`) {
		t.Fatalf("body = %q, want a terminal frame carrying runtime.start_failed", text)
	}
}

// TestRouterRequestBodyTooLarge proves the router rejects an oversized
// request body with 413 before any admission decision is attempted (the
// maxBodyBytes bound named in the brief, not itself one of the ten
// enumerated Step-1 tests but an explicitly required behavior).
func TestRouterRequestBodyTooLarge(t *testing.T) {
	skipOnWindows(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{})

	rt := NewRouter(m)
	big := strings.Repeat("a", maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

// TestRouterReleaseCalledExactlyOnce proves release() is invoked exactly
// once on every exit path of the streaming handler -- success, a client
// disconnecting mid-stream, a non-2xx upstream status, and the child dying
// mid-response -- counted from a fake (countingManager) wrapping the real
// Manager, since the real EnsureRunning's own sync.Once would otherwise mask
// a double-release bug in the router's own code. A leaked release is the bug
// that makes a spec permanently un-evictable (EnsureRunning's doc comment).
func TestRouterReleaseCalledExactlyOnce(t *testing.T) {
	skipOnWindows(t)

	t.Run("success", func(t *testing.T) {
		shrinkTimings(t)
		m := newTestManager(t, allowlistPolicy())
		spec := baseSpec("spec-a", "model-a")
		m.Apply(Config{Specs: []Spec{spec}})
		cm := &countingManager{inner: m}
		srv := httptest.NewServer(newRouter(cm))
		defer srv.Close()

		resp, err := http.Post(srv.URL+"/v1/echo", "application/json", strings.NewReader(`{"model":"model-a"}`))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining only
		resp.Body.Close()

		waitUntil(t, 3*time.Second, "release called exactly once", func() bool {
			return cm.calls.Load() == 1
		})
	})

	t.Run("client_disconnect_midstream", func(t *testing.T) {
		shrinkTimings(t)
		shrinkHeartbeat(t, 20*time.Millisecond)
		m := newTestManager(t, allowlistPolicy())
		spec := baseSpec("spec-a", "model-a")
		m.Apply(Config{Specs: []Spec{spec}})
		cm := &countingManager{inner: m}
		srv := httptest.NewServer(newRouter(cm))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		body := `{"model":"model-a","stream":true}`
		target := srv.URL + "/v1/chunked?gap=2s&c1=first-chunk"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		// Read until the real chunk has actually arrived, tolerating any
		// number of `: keepalive` comments first -- EnsureRunning is not
		// instantaneous even for a fast-starting model, so a fixed-size
		// read here could otherwise consume bytes from a keepalive comment
		// instead of "first-chunk" and cancel while EnsureRunning is still
		// pending, which is a different (and already-covered) case, not the
		// "disconnect mid-stream after real data flowed" case this
		// subtest targets.
		readUntilContains(t, resp.Body, "first-chunk", 3*time.Second)
		cancel() // disconnect mid-stream, before the child's 2s gap elapses
		resp.Body.Close()

		waitUntil(t, 3*time.Second, "release called exactly once despite client disconnect", func() bool {
			return cm.calls.Load() == 1
		})
	})

	t.Run("upstream_non2xx", func(t *testing.T) {
		shrinkTimings(t)
		shrinkHeartbeat(t, 20*time.Millisecond)
		m := newTestManager(t, allowlistPolicy())
		spec := baseSpec("spec-a", "model-a")
		m.Apply(Config{Specs: []Spec{spec}})
		cm := &countingManager{inner: m}
		srv := httptest.NewServer(newRouter(cm))
		defer srv.Close()

		body := `{"model":"model-a","stream":true}`
		resp, err := http.Post(srv.URL+"/v1/fail?status=500", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining only
		resp.Body.Close()

		waitUntil(t, 3*time.Second, "release called exactly once on a non-2xx upstream status", func() bool {
			return cm.calls.Load() == 1
		})
	})

	t.Run("upstream_crash_midstream", func(t *testing.T) {
		shrinkTimings(t)
		shrinkHeartbeat(t, 20*time.Millisecond)
		m := newTestManager(t, allowlistPolicy())
		spec := baseSpec("spec-a", "model-a")
		spec.Args = stubArgs(0, 500*time.Millisecond, 1, "") // dies mid-gap, well after the first chunk is sent
		m.Apply(Config{Specs: []Spec{spec}})
		cm := &countingManager{inner: m}
		srv := httptest.NewServer(newRouter(cm))
		defer srv.Close()

		body := `{"model":"model-a","stream":true}`
		resp, err := http.Post(srv.URL+"/v1/chunked?gap=5s", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining only
		resp.Body.Close()

		waitUntil(t, 5*time.Second, "release called exactly once when the child dies mid-response", func() bool {
			return cm.calls.Load() == 1
		})
	})
}
