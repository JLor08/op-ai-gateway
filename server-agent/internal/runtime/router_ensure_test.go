// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ensureBody is POST /ensure/{model}'s success body as the wire carries it,
// declared here rather than borrowed from router.go so these tests pin the
// wire shape itself.
type ensureBody struct {
	Status string `json:"status"`
}

// ensureStubManager is a managerPort fake for POST /ensure/{model}: it records
// the model EnsureRunning was asked for, runs onEnsure (when set) inside the
// call, and answers success with a release that counts its calls and runs
// onRelease (when set). Every test using it drives ServeHTTP synchronously on
// the test goroutine, so the fields need no locking.
type ensureStubManager struct {
	onEnsure  func()
	onRelease func()
	models    []string
	releases  int
}

func (m *ensureStubManager) EnsureRunning(_ context.Context, model string) (string, func(), error) {
	m.models = append(m.models, model)
	if m.onEnsure != nil {
		m.onEnsure()
	}
	return "http://127.0.0.1:1", func() {
		m.releases++
		if m.onRelease != nil {
			m.onRelease()
		}
	}, nil
}
func (m *ensureStubManager) LoadedModels() []string { return nil }
func (m *ensureStubManager) Status() []Status       { return nil }

// readRequestLog returns the lines of a stubchild -request-log file, or none
// when the child never wrote one.
func readRequestLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read request log: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// startEnsure sends a POST to url under ctx on its own goroutine and returns
// the channel its client-side error arrives on. An empty body sends none.
func startEnsure(t *testing.T, ctx context.Context, url, body string) <-chan error {
	t.Helper()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errCh <- err
	}()
	return errCh
}

// awaitCancelledEnsure waits for a request startEnsure sent to fail on the
// client side after its context was cancelled.
func awaitCancelledEnsure(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("the ensure request succeeded although its client cancelled it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled ensure request did not return within 3s")
	}
}

// closeServerWithin closes srv, which waits for every handler still running,
// and fails the test when that takes longer than 3s: the handler is then
// still inside EnsureRunning, whose context never saw its client leave. On
// that failure it closes m first, so the stuck EnsureRunning returns and the
// close, and the test's cleanups, can finish.
func closeServerWithin(t *testing.T, srv *httptest.Server, m *Manager) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		m.Close()
		<-closed
		t.Fatal("the ensure handler did not return within 3s of its client leaving: EnsureRunning's context never saw the client go")
	}
}

// TestRouterEnsureStartsAColdChildWithoutForwarding is the route's headline
// case: a bodiless POST /ensure/{model} for a cold spec answers 200
// {"status":"running"} once the child is healthy. The child's own request log
// proves that nothing but the manager's health probes reached it, release()
// ran exactly once, and nothing is left in flight.
func TestRouterEnsureStartsAColdChildWithoutForwarding(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	requestLog := filepath.Join(t.TempDir(), "requests.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Args = append(stubArgs(200*time.Millisecond, 0, 0, ""), "-request-log", requestLog)
	m.Apply(Config{Specs: []Spec{spec}})
	cm := &countingManager{inner: m}
	srv := httptest.NewServer(newRouter(cm))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ensure/model-a", "", nil)
	if err != nil {
		t.Fatalf("POST /ensure/model-a: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := decodeJSON[ensureBody](t, resp.Body); got.Status != "running" {
		t.Errorf("body status = %q, want running", got.Status)
	}

	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateRunning {
		t.Fatalf("spec-a status = %+v, want running", st)
	}
	if st.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0: the route proxies nothing, so it holds nothing in flight once it has answered", st.InFlight)
	}
	assertReleasedExactlyOnce(t, cm, 3*time.Second, 200*time.Millisecond)

	lines := readRequestLog(t, requestLog)
	if len(lines) == 0 {
		t.Fatal("the child's request log is empty; want at least the manager's health probe")
	}
	for _, line := range lines {
		if line != "GET /health" {
			t.Fatalf("the child received %q (full log %q); want only health probes -- the ensure route forwards nothing", line, lines)
		}
	}
}

// TestRouterEnsureReleasesBeforeAnswering pins the handler's own contract
// against a fake: the model is the decoded path after "/ensure/" (so "/" and
// escaped characters survive), release() runs exactly once and before anything
// is written, and the write deadline is armed for the answer.
func TestRouterEnsureReleasesBeforeAnswering(t *testing.T) {
	fw := &fakeDeadlineWriter{}
	codeAtRelease := -1
	m := &ensureStubManager{}
	m.onRelease = func() { codeAtRelease = fw.code }

	newRouter(m).ServeHTTP(fw, httptest.NewRequest(http.MethodPost, "/ensure/org/model%20v2", nil))

	if fw.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", fw.code, fw.body.String())
	}
	if ct := fw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := decodeJSON[ensureBody](t, &fw.body); got.Status != "running" {
		t.Errorf("body status = %q, want running", got.Status)
	}
	if len(m.models) != 1 || m.models[0] != "org/model v2" {
		t.Fatalf("EnsureRunning was asked for %q, want exactly [\"org/model v2\"] (the decoded path after /ensure/)", m.models)
	}
	if m.releases != 1 {
		t.Fatalf("release calls = %d, want exactly 1", m.releases)
	}
	if codeAtRelease != 0 {
		t.Errorf("status already written when release ran = %d, want 0: release runs before the answer, since nothing is proxied", codeAtRelease)
	}
	if n := fw.deadlineSets.Load(); n < 1 {
		t.Errorf("SetWriteDeadline called %d times, want at least 1 before the answer is written", n)
	}
}

// TestRouterEnsureReleasesOnceWhenTheClientLeavesAsItSucceeds covers the race
// between a successful EnsureRunning and the client disconnecting: the fake
// cancels the request context inside the call and then succeeds. release()
// must still run exactly once -- a lost release makes the spec un-evictable --
// and nothing is written to the departed client.
func TestRouterEnsureReleasesOnceWhenTheClientLeavesAsItSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &ensureStubManager{onEnsure: cancel}
	rec := httptest.NewRecorder()

	newRouter(m).ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/ensure/model-a", nil))

	if m.releases != 1 {
		t.Fatalf("release calls = %d, want exactly 1 although the client left", m.releases)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("wrote %q (Content-Type %q) to a client that had already left; want nothing", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

// TestRouterEnsureErrorCodes is the sentinel table for POST /ensure/{model}
// against a real manager: every failure is a genuine HTTP status carrying the
// same stable code the proxy paths use for it.
func TestRouterEnsureErrorCodes(t *testing.T) {
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
			name: "admission_blocked_force_stopped",
			setup: func(t *testing.T) (*Manager, string) {
				shrinkTimings(t)
				m := newTestManager(t, allowlistPolicy())
				spec := baseSpec("spec-a", "model-a")
				spec.AdminState = "force_stopped"
				m.Apply(Config{Specs: []Spec{spec}})
				return m, "model-a"
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "runtime.admission_blocked",
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
			name: "not_permitted",
			setup: func(t *testing.T) (*Manager, string) {
				m := newTestManager(t, LocalPolicy{AllowedBinaries: []string{"/not/the/stub"}})
				m.Apply(Config{Specs: []Spec{baseSpec("spec-a", "model-a")}})
				return m, "model-a"
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   "runtime.not_permitted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, model := tc.setup(t)
			rec := httptest.NewRecorder()
			NewRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/"+model, nil))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != tc.wantCode {
				t.Errorf("error code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// TestRouterEnsureNilManagerAndEmptyModel: both answer 404
// runtime.model_not_managed, and an empty model never reaches the manager.
func TestRouterEnsureNilManagerAndEmptyModel(t *testing.T) {
	t.Run("nil manager", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewRouter(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/model-a", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != "runtime.model_not_managed" {
			t.Fatalf("code = %q, want runtime.model_not_managed", code)
		}
	})
	t.Run("empty model", func(t *testing.T) {
		m := &statusManager{}
		rec := httptest.NewRecorder()
		newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != "runtime.model_not_managed" {
			t.Fatalf("code = %q, want runtime.model_not_managed", code)
		}
		if n := m.ensures.Load(); n != 0 {
			t.Fatalf("EnsureRunning called %d times, want 0 for an empty model", n)
		}
	})
}

// TestRouterEnsureCancelDuringStartingReleasesNothing: a client that leaves
// while its child is starting gets no release -- its waiter is dropped, so
// EnsureRunning returns an error -- and the child still comes up with nothing
// in flight, so the idle policy governs it as after any finished request.
func TestRouterEnsureCancelDuringStartingReleasesNothing(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(time.Second, 0, 0, "")
	m.Apply(Config{Specs: []Spec{spec}})
	cm := &countingManager{inner: m}
	srv := httptest.NewServer(newRouter(cm))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := startEnsure(t, ctx, srv.URL+"/ensure/model-a", "")
	waitUntil(t, 2*time.Second, "spec-a starting", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStarting
	})
	cancel()
	awaitCancelledEnsure(t, errCh)
	srv.Close() // blocks until the handler has returned

	waitUntil(t, 5*time.Second, "spec-a running although its only caller left", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateRunning
	})
	if st := statusFor(m, "spec-a"); st.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0 once the child is up", st.InFlight)
	}
	if n := cm.calls.Load(); n != 0 {
		t.Fatalf("release calls = %d, want 0: EnsureRunning failed for the departed client, so there is nothing to release", n)
	}
}

// TestRouterEnsureCancelWhileQueuedStartsNothing: a client that leaves while
// its request is still queued behind an occupant drops its waiter, so freeing
// the slot afterwards starts nothing. The candidate's own demand is unknown,
// so it may only start alone on gpu 0, and its admission wait of 0 means only
// the client leaving ends the wait. A caller that sends a body gets the same:
// the route reads the body to its end, which is what lets net/http notice
// the client leave.
func TestRouterEnsureCancelWhileQueuedStartsNothing(t *testing.T) {
	skipOnWindows(t)
	t.Run("bodiless", func(t *testing.T) { cancelQueuedEnsure(t, "") })
	t.Run("with a body", func(t *testing.T) { cancelQueuedEnsure(t, `{"model":"model-unknown"}`) })
}

// cancelQueuedEnsure is TestRouterEnsureCancelWhileQueuedStartsNothing for one
// request body ("" sends none).
func cancelQueuedEnsure(t *testing.T, body string) {
	t.Helper()
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	known := baseSpec("spec-known", "model-known")
	known.GPUs = []SpecGPU{{Index: 0, VRAMMB: 5000}}
	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	unknown := baseSpec("spec-unknown", "model-unknown")
	unknown.Args = stubArgs(0, 0, 0, invocationLog)
	unknown.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}}
	unknown.AdmissionWaitTimeoutSeconds = 0
	cfg := Config{
		Specs:      []Spec{known, unknown},
		Coresident: [][2]string{{"spec-known", "spec-unknown"}},
		GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 20000}},
	}
	m.Apply(cfg)
	occupyCtx, occupyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, release, err := m.EnsureRunning(occupyCtx, "model-known")
	occupyCancel()
	if err != nil {
		t.Fatalf("EnsureRunning(model-known) = %v, want the occupant to start", err)
	}
	release()

	cm := &countingManager{inner: m}
	srv := httptest.NewServer(newRouter(cm))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := startEnsure(t, ctx, srv.URL+"/ensure/model-unknown", body)
	// The precedence wait records why the candidate waits in the same owner
	// turn that queues it, so a LastError proves the waiter is queued.
	waitUntil(t, 3*time.Second, "spec-unknown queued behind spec-known", func() bool {
		st := statusFor(m, "spec-unknown")
		return st != nil && st.LastError != nil
	})
	cancel()
	awaitCancelledEnsure(t, errCh)
	// The handler returns only once EnsureRunning has, so from here on the
	// manager has answered the departed caller's waiter.
	closeServerWithin(t, srv, m)

	// Free the slot. A waiter still queued would start spec-unknown now.
	stopped := known
	stopped.AdminState = "force_stopped"
	cfg.Specs = []Spec{stopped, unknown}
	m.Apply(cfg)
	waitUntil(t, 3*time.Second, "spec-known stopped", func() bool {
		st := statusFor(m, "spec-known")
		return st != nil && st.State == StateStopped
	})
	// An absence has no event to wait for: the settle window covers the wake
	// the occupant's exit triggers.
	time.Sleep(300 * time.Millisecond)

	if n := countInvocations(t, invocationLog); n != 0 {
		t.Fatalf("spec-unknown was exec'd %d times, want 0: a cancelled waiter must start nothing", n)
	}
	if st := statusFor(m, "spec-unknown"); st == nil || st.State != StateStopped {
		t.Fatalf("spec-unknown status = %+v, want stopped", st)
	}
	if n := cm.calls.Load(); n != 0 {
		t.Fatalf("release calls = %d, want 0", n)
	}
}

// TestRouterEnsureDiscardsABody: a body is read and discarded, never
// forwarded and never a model name -- the path names the model -- and a body
// over maxBodyBytes is refused with serveProxy's 413 before EnsureRunning.
func TestRouterEnsureDiscardsABody(t *testing.T) {
	t.Run("within the bound", func(t *testing.T) {
		m := &ensureStubManager{}
		rec := httptest.NewRecorder()
		newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/model-a", strings.NewReader(`{"model":"model-b"}`)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(m.models) != 1 || m.models[0] != "model-a" {
			t.Fatalf("EnsureRunning was asked for %q, want exactly [\"model-a\"]: the path names the model, the body nothing", m.models)
		}
	})
	t.Run("over the bound", func(t *testing.T) {
		m := &statusManager{}
		rec := httptest.NewRecorder()
		big := strings.Repeat("a", maxBodyBytes+1)
		newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/model-a", strings.NewReader(big)))

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
		}
		got := decodeJSON[errorEnvelope](t, rec.Body)
		if want := fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes); got.Error.Code != "runtime.request_too_large" || got.Error.Message != want {
			t.Fatalf("error = %+v, want runtime.request_too_large %q", got.Error, want)
		}
		if n := m.ensures.Load(); n != 0 {
			t.Fatalf("EnsureRunning called %d times, want 0 for a refused body", n)
		}
	})
}

// TestRouterEnsureOtherMethodsFallThrough: the route answers POST only. GET
// and PUT on the same path land in serveProxy's body dispatch, where a
// bodiless request names no model -- 404 runtime.model_not_managed with
// serveProxy's own message and no EnsureRunning call, so neither method can
// start a child through this path.
func TestRouterEnsureOtherMethodsFallThrough(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			m := &statusManager{}
			rec := httptest.NewRecorder()
			newRouter(m).ServeHTTP(rec, httptest.NewRequest(method, "/ensure/model-a", nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			got := decodeJSON[errorEnvelope](t, rec.Body)
			if got.Error.Code != "runtime.model_not_managed" || got.Error.Message != "request body does not name a managed model" {
				t.Fatalf("error = %+v, want serveProxy's runtime.model_not_managed for a body that names no model", got.Error)
			}
			if n := m.ensures.Load(); n != 0 {
				t.Fatalf("EnsureRunning called %d times, want 0: only POST starts a child through /ensure/", n)
			}
		})
	}
}

// TestRouterEnsureModelIDWithSlash: an HF-style upstream model id carries "/",
// and the route takes everything after "/ensure/" as the model, so such a spec
// starts through it like any other.
func TestRouterEnsureModelIDWithSlash(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{baseSpec("spec-a", "org/model-a")}})
	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ensure/org/model-a", "", nil)
	if err != nil {
		t.Fatalf("POST /ensure/org/model-a: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := decodeJSON[ensureBody](t, resp.Body); got.Status != "running" {
		t.Errorf("body status = %q, want running", got.Status)
	}
	if st := statusFor(m, "spec-a"); st == nil || st.State != StateRunning {
		t.Fatalf("spec-a status = %+v, want running", st)
	}
}
