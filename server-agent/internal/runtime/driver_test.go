// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newFeaturesServer returns a *FeaturesClient backed by an httptest.Server
// that always answers with the given feature set (200, no ETag caching
// games needed for these tests -- every test either wants a stable
// active/inactive answer for the whole test, or exercises the transition by
// swapping the server's own answer via an atomic-guarded closure).
func newFeaturesServer(t *testing.T, features []string) *FeaturesClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(struct {
			Features []string `json:"features"`
		}{Features: features})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewFeaturesClient(srv.URL, "tok", nil)
}

// activeFeaturesClient is the common case most Sync tests need: a gateway
// that declares runtime_manager.
func activeFeaturesClient(t *testing.T) *FeaturesClient {
	return newFeaturesServer(t, []string{runtimeManagerFeature})
}

// fakeManager is a hand-written driverManager fake (this module's house
// pattern: no mocking framework exists or may be added) so driver_test.go
// can exercise Sync's control flow without a real process-supervision
// stack.
type fakeManager struct {
	mu       sync.Mutex
	applied  []Config
	statuses []Status
	trans    chan struct{}
}

func (f *fakeManager) Apply(cfg Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, cfg)
}

func (f *fakeManager) Status() []Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses
}

func (f *fakeManager) Transitions() <-chan struct{} { return f.trans }

func (f *fakeManager) EnsureRunning(context.Context, string) (string, func(), error) {
	return "", nil, ErrModelNotManaged
}

func (f *fakeManager) LoadedModels() []string { return nil }

func (f *fakeManager) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

func (f *fakeManager) lastApplied() Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied[len(f.applied)-1]
}

// fakeSource is a Source that is neither *GatewaySource nor *FileSource --
// it exercises Sync's generic "any other Source" fallback path, which
// always calls Load and never special-cases a pushed payload.
type fakeSource struct {
	mu      sync.Mutex
	cfg     Config
	changed bool
	loads   int
}

func (f *fakeSource) Load(context.Context) (Config, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	return f.cfg, f.changed, nil
}

func (f *fakeSource) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// fakeReporter records every posted report.
type fakeReporter struct {
	mu    sync.Mutex
	posts []json.RawMessage
}

func (f *fakeReporter) PostRuntimeReport(_ context.Context, raw json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, append(json.RawMessage(nil), raw...))
	return nil
}

func (f *fakeReporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeReporter) last() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts[len(f.posts)-1]
}

// TestDriverSyncAppliesOnlyOnChange proves Sync's step 3: manager.Apply is
// called only when the source reports changed=true, never on an unchanged
// Load.
func TestDriverSyncAppliesOnlyOnChange(t *testing.T) {
	mgr := &fakeManager{}
	// RouterListen 0: these tests exercise Apply-on-change, not StartRouter
	// (that has its own dedicated tests below) -- keeping it 0 means Sync's
	// own StartRouter(cfg.RouterListen) call stays a no-op, never binding a
	// real port.
	src := &fakeSource{cfg: Config{MaxProcesses: 3, Specs: []Spec{}}, changed: false}
	d := newDriver(mgr, src, activeFeaturesClient(t), nil)

	d.Sync(context.Background(), nil)
	if got := mgr.applyCount(); got != 0 {
		t.Fatalf("Apply calls after unchanged Load = %d, want 0", got)
	}
	if got := src.loadCount(); got != 1 {
		t.Fatalf("Load calls = %d, want 1", got)
	}

	src.mu.Lock()
	src.changed = true
	src.mu.Unlock()

	d.Sync(context.Background(), nil)
	if got := mgr.applyCount(); got != 1 {
		t.Fatalf("Apply calls after changed Load = %d, want 1", got)
	}
	if got := mgr.lastApplied(); got.MaxProcesses != 3 {
		t.Fatalf("applied config = %+v, want MaxProcesses=3", got)
	}
}

// TestDriverSyncFeatureInactiveStopsManagerAndSkipsLoad proves step 1: when
// the gateway no longer declares runtime_manager, Sync drains everything
// (an empty Apply) and returns WITHOUT ever calling source.Load -- a
// blocked driver must not keep polling a source it is not going to act on.
func TestDriverSyncFeatureInactiveStopsManagerAndSkipsLoad(t *testing.T) {
	mgr := &fakeManager{}
	src := &fakeSource{cfg: Config{Specs: []Spec{{ID: "s1"}}}, changed: true}
	inactive := newFeaturesServer(t, nil)
	d := newDriver(mgr, src, inactive, nil)

	d.Sync(context.Background(), nil)

	if got := src.loadCount(); got != 0 {
		t.Fatalf("Load calls with feature inactive = %d, want 0 (Sync must return before loading)", got)
	}
	if got := mgr.applyCount(); got != 1 {
		t.Fatalf("Apply calls = %d, want exactly 1 (the drain-everything call)", got)
	}
	applied := mgr.lastApplied()
	if len(applied.Specs) != 0 {
		t.Fatalf("applied config = %+v, want an EMPTY config (drain everything)", applied)
	}

	// A second Sync while still inactive must not log/transition again (no
	// externally observable assertion for the log itself, but it must still
	// behave identically: another drain-everything Apply, no Load).
	d.Sync(context.Background(), nil)
	if got := mgr.applyCount(); got != 2 {
		t.Fatalf("Apply calls after second inactive Sync = %d, want 2", got)
	}
}

// TestDriverSyncPushedPayloadRoutesToApplyPushedOnGatewaySource proves the
// GatewaySource half of step 2: a pushed payload is consumed via
// ApplyPushed (never a fresh Load/HTTP round trip) when the source is a
// real *GatewaySource.
func TestDriverSyncPushedPayloadRoutesToApplyPushedOnGatewaySource(t *testing.T) {
	mgr := &fakeManager{}
	// A gateway base URL that would fail any real HTTP call, proving Load
	// is never reached on the pushed path -- ApplyPushed touches no
	// network at all.
	src := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")
	d := newDriver(mgr, src, activeFeaturesClient(t), nil)

	// router_listen 0: this test proves the pushed-payload routing, not
	// StartRouter's real bind (which has its own dedicated tests below).
	pushed := []byte(minimalConfigJSON(0, "push-e1"))
	d.Sync(context.Background(), pushed)

	if got := mgr.applyCount(); got != 1 {
		t.Fatalf("Apply calls = %d, want 1", got)
	}
	if got := mgr.lastApplied(); got.ETag != "push-e1" {
		t.Fatalf("applied config = %+v, want ETag=push-e1 (from the pushed payload)", got)
	}

	// The same document pushed again (same etag): ApplyPushed's own
	// changed=false, so Apply must not be called a second time.
	d.Sync(context.Background(), pushed)
	if got := mgr.applyCount(); got != 1 {
		t.Fatalf("Apply calls after re-pushing the identical document = %d, want still 1", got)
	}
}

// TestDriverSyncIgnoresPushedOnFileSource proves the FileSource half of
// step 2 (spec §10.2): a pushed payload is NEVER consumed when the source
// is a *FileSource -- Sync must fall back to reading the local file, and
// the applied config must reflect the FILE's content, not the pushed one.
func TestDriverSyncIgnoresPushedOnFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime-config.json")
	// router_listen 0 in the file: this test proves pushed-vs-file
	// precedence, not StartRouter's real bind (dedicated tests below).
	if err := os.WriteFile(path, []byte(minimalConfigJSON(0, "file-e1")), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	mgr := &fakeManager{}
	src := NewFileSource(path)
	d := newDriver(mgr, src, activeFeaturesClient(t), nil)

	pushed := []byte(minimalConfigJSON(9999, "pushed-should-be-ignored"))
	d.Sync(context.Background(), pushed)

	if got := mgr.applyCount(); got != 1 {
		t.Fatalf("Apply calls = %d, want 1", got)
	}
	if got := mgr.lastApplied(); got.ETag != "file-e1" {
		t.Fatalf("applied config = %+v, want the FILE's ETag=file-e1, not the pushed payload", got)
	}
}

// TestDriverSyncFileModeSendsReportOnChange proves step 3's file-mode
// report send: a *FileSource change posts a redacted report via the
// RuntimeReporter, and a GatewaySource change never does (the report is
// file-mode only).
func TestDriverSyncFileModeSendsReportOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime-config.json")
	// router_listen 0: this test proves the report-send behavior, not
	// StartRouter's real bind (dedicated tests below).
	const configJSON = `{"router_listen":0,"max_processes":1,"gpu_budgets":[],` +
		`"specs":[{"id":"s1","model":"m","upstream_model":"u","binary":"/bin/x",` +
		`"args":[],"env":{"SECRET":"hunter2"},"gpus":[],"listen_port":0,"health_path":"/health",` +
		`"health_timeout_seconds":5,"startup_timeout_seconds":60,"idle_timeout_seconds":0,` +
		`"admission_wait_timeout_seconds":0,"pinned":false,"admin_state":""}],"coresident":[],"etag":"file-e1"}`
	if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	mgr := &fakeManager{}
	src := NewFileSource(path)
	rep := &fakeReporter{}
	d := newDriver(mgr, src, activeFeaturesClient(t), rep)

	d.Sync(context.Background(), nil)

	if got := rep.count(); got != 1 {
		t.Fatalf("report posts = %d, want 1", got)
	}
	var report Report
	if err := json.Unmarshal(rep.last(), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.Source != "file" {
		t.Errorf("report.source = %q, want file", report.Source)
	}
	// The env value must be redacted -- the report is built via
	// BuildReport, which masks every env value before it ever reaches this
	// reporter.
	if bytes.Contains(report.Config, []byte("hunter2")) {
		t.Errorf("report config leaks the plaintext secret: %s", report.Config)
	}

	// A GatewaySource change must never send a report at all.
	mgr2 := &fakeManager{}
	gwSrc := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")
	rep2 := &fakeReporter{}
	d2 := newDriver(mgr2, gwSrc, activeFeaturesClient(t), rep2)
	d2.Sync(context.Background(), []byte(minimalConfigJSON(0, "gw-e1")))
	if got := rep2.count(); got != 0 {
		t.Fatalf("gateway-mode report posts = %d, want 0", got)
	}
}

// TestDriverStatusAndTransitionsNilSafe pins the NewDriver(nil, ...)
// defensive case: a Driver built over a literal nil *Manager (never
// expected in production, but never assigned as a typed-nil interface
// either) must answer Status/Transitions with nil rather than panicking.
func TestDriverStatusAndTransitionsNilSafe(t *testing.T) {
	d := NewDriver(nil, &fakeSource{}, activeFeaturesClient(t), nil)
	if got := d.Status(); got != nil {
		t.Fatalf("Status() = %v, want nil", got)
	}
	if got := d.Transitions(); got != nil {
		t.Fatalf("Transitions() = %v, want nil", got)
	}
}

// TestDriverStatusDelegatesToManager proves the ordinary (non-nil) path.
func TestDriverStatusDelegatesToManager(t *testing.T) {
	mgr := &fakeManager{statuses: []Status{{SpecID: "s1", State: StateRunning}}}
	d := newDriver(mgr, &fakeSource{}, activeFeaturesClient(t), nil)
	got := d.Status()
	if len(got) != 1 || got[0].SpecID != "s1" {
		t.Fatalf("Status() = %+v, want [{SpecID: s1}]", got)
	}
}

// grabFreePort picks an OS-assigned TCP port and releases it immediately,
// mirroring manager.go's own grabEphemeralPort trick -- accepted TOCTOU
// trade-off, not a defect, matching the rest of this module's tests.
func grabFreePort(t *testing.T) int {
	t.Helper()
	port, err := grabEphemeralPort()
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	return port
}

// waitForHTTP200 polls url until it answers 200 or the deadline elapses --
// this module's poll-not-sleep house pattern (never a fixed time.Sleep to
// synchronize with an async listener coming up).
func waitForHTTP200(t *testing.T, url string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not answer 200 within %s", url, d)
}

// TestDriverStartRouterBindsServesAndRebinds proves StartRouter's contract:
// listen<=0 is a no-op, a positive listen actually serves the router
// (proven via a real GET /health), the SAME port again is idempotent (no
// error, keeps serving), and switching to a DIFFERENT port tears down the
// old listener and binds the new one.
func TestDriverStartRouterBindsServesAndRebinds(t *testing.T) {
	mgr := &fakeManager{}
	d := newDriver(mgr, &fakeSource{}, nil, nil)

	if err := d.StartRouter(0); err != nil {
		t.Fatalf("StartRouter(0) = %v, want nil (no-op)", err)
	}

	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d) = %v", port, err)
	}
	t.Cleanup(func() { _ = d.StartRouter(0) })

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	waitForHTTP200(t, healthURL, 2*time.Second)

	// Idempotent: the SAME port again must not error and must not disrupt
	// serving.
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter(%d) second call = %v, want nil", port, err)
	}
	waitForHTTP200(t, healthURL, time.Second)

	// A DIFFERENT port: the old one must stop answering, the new one must
	// come up.
	port2 := grabFreePort(t)
	if err := d.StartRouter(port2); err != nil {
		t.Fatalf("StartRouter(%d) = %v", port2, err)
	}
	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/health", port2), 2*time.Second)

	oldClient := &http.Client{Timeout: 200 * time.Millisecond}
	if resp, err := oldClient.Get(healthURL); err == nil {
		resp.Body.Close()
		t.Fatalf("old port %d still answers after rebinding to %d", port, port2)
	}
}

// TestDriverCloseStopsRouter proves Close tears down the router listener
// without touching the manager (main.go owns the manager's own Close
// separately).
func TestDriverCloseStopsRouter(t *testing.T) {
	mgr := &fakeManager{}
	d := newDriver(mgr, &fakeSource{}, nil, nil)
	port := grabFreePort(t)
	if err := d.StartRouter(port); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	waitForHTTP200(t, healthURL, 2*time.Second)

	d.Close()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Get(healthURL); err != nil {
			return // stopped serving, as expected
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("router on port %d still answers after Close", port)
}
