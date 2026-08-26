// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubchildPath is built once per test binary run (TestMain) and shared by
// every test below -- there is no re-exec-helper pattern in this repository
// (verified: no TestHelperProcess/GO_WANT_HELPER anywhere, and no other test
// builds/spawns a compiled binary), so this package adds its own tiny
// real-process stand-in under testdata/ instead (testdata is excluded from
// the module build and from internal/archtest's package graph by the go
// tool itself).
var stubchildPath string

func TestMain(m *testing.M) {
	os.Exit(buildStubchildAndRun(m))
}

func buildStubchildAndRun(m *testing.M) int {
	if runtime.GOOS == "windows" {
		// Process-management tests self-skip on Windows (see skipOnWindows
		// below); no need to build the helper at all.
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "runtime-stubchild-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "manager_test: mkdtemp:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	bin := filepath.Join(dir, "stubchild")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/stubchild")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "manager_test: build stubchild: %v\n%s", err, out)
		return 1
	}
	stubchildPath = bin
	return m.Run()
}

// skipOnWindows guards every test in this file that spawns a real process:
// process-group signaling (SIGTERM/SIGKILL to a negative PID) is unix-only
// (see proc_unix.go/proc_windows.go), and this repository's CI only runs
// ubuntu-latest, so there is no coverage loss in skipping here -- the same
// posture internal/certinstall's platform-specific files take (a
// //go:build split for the production code; this is the test-side
// counterpart for a suite that only makes sense on the platform where the
// production behavior it exercises is fully implemented).
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-management tests are unix-only (see proc_windows.go); CI does not run this suite on Windows")
	}
}

// waitUntil polls cond every 5ms until it reports true or timeout elapses,
// failing the test on timeout. Never a fixed time.Sleep: a deterministic
// deadline-bounded poll instead of a synchronization guess.
func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shrinkTimings sets every package-level timing var to a small,
// deterministic-for-tests value and returns a restore func. The house
// pattern (client.backoffBase / certinstall.hookTimeout): save the
// original, mutate, restore via defer -- and since these vars are read by
// the owner goroutine, the restore must run strictly AFTER that Manager's
// Close() has fully joined every goroutine it spawned (defer order: defer
// restore BEFORE defer m.Close() in caller code, so Close runs first).
func shrinkTimings(t *testing.T) {
	t.Helper()
	origDrain, origKill := drainGrace, killGrace
	origBackoffBase, origBackoffCap := backoffBase, backoffCap
	origStable := stableRunThreshold
	origHealthPoll := healthPollInterval
	origIdleTick := idleTickInterval

	drainGrace = 200 * time.Millisecond
	killGrace = 200 * time.Millisecond
	backoffBase = 60 * time.Millisecond
	backoffCap = 500 * time.Millisecond
	stableRunThreshold = 150 * time.Millisecond
	healthPollInterval = 20 * time.Millisecond
	idleTickInterval = 30 * time.Millisecond

	t.Cleanup(func() {
		drainGrace, killGrace = origDrain, origKill
		backoffBase, backoffCap = origBackoffBase, origBackoffCap
		stableRunThreshold = origStable
		healthPollInterval = origHealthPoll
		idleTickInterval = origIdleTick
	})
}

// stubArgs builds the standard argv for the stub child binary. invocationLog
// may be "" to skip writing one.
func stubArgs(healthDelay, crashAfter time.Duration, exitCode int, invocationLog string) []string {
	args := []string{
		"-port", "${PORT}",
		"-health-delay", healthDelay.String(),
	}
	if crashAfter > 0 {
		args = append(args, "-crash-after", crashAfter.String(), "-exit-code", fmt.Sprintf("%d", exitCode))
	}
	if invocationLog != "" {
		args = append(args, "-invocation-log", invocationLog)
	}
	return args
}

// baseSpec returns a minimal, valid launch spec for the stub child.
func baseSpec(id, upstreamModel string) Spec {
	return Spec{
		ID:                          id,
		Model:                       id,
		UpstreamModel:               upstreamModel,
		Binary:                      stubchildPath,
		Args:                        stubArgs(0, 0, 0, ""),
		Env:                         map[string]string{},
		HealthPath:                  "/health",
		HealthTimeoutSeconds:        2,
		StartupTimeoutSeconds:       5,
		IdleTimeoutSeconds:          0,
		AdmissionWaitTimeoutSeconds: 5,
		GPUs:                        []SpecGPU{},
	}
}

func allowlistPolicy() LocalPolicy {
	return LocalPolicy{AllowedBinaries: []string{stubchildPath}}
}

func newTestManager(t *testing.T, policy LocalPolicy) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{Policy: policy, Getenv: func(string) string { return "" }})
	t.Cleanup(m.Close)
	return m
}

func httpEcho(t *testing.T, endpoint, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(endpoint+"/v1/echo", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s/v1/echo: %v", endpoint, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

func httpGetOK(endpoint, path string) bool {
	resp, err := http.Get(endpoint + path)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func statusFor(m *Manager, specID string) *Status {
	for _, s := range m.Status() {
		s := s
		if s.SpecID == specID {
			return &s
		}
	}
	return nil
}

func countInvocations(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read invocation log: %v", err)
	}
	lines := bytes.Count(data, []byte("\n"))
	return lines
}

// ---------------------------------------------------------------------------

func TestManagerStartsAndServes(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	defer release()

	if endpoint == "" {
		t.Fatal("EnsureRunning returned an empty endpoint")
	}
	code, body := httpEcho(t, endpoint, "hello")
	if code != http.StatusOK || body != "hello" {
		t.Errorf("echo = (%d, %q), want (200, %q)", code, body, "hello")
	}

	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateRunning {
		t.Fatalf("Status()[spec-a].State = %+v, want running", st)
	}
	if st.Model != "model-a" {
		t.Errorf("Status()[spec-a].Model = %q, want %q", st.Model, "model-a")
	}

	loaded := m.LoadedModels()
	found := false
	for _, name := range loaded {
		if name == "model-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("LoadedModels() = %v, want it to contain %q", loaded, "model-a")
	}
}

func TestManagerRespectsAllowlist(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, LocalPolicy{AllowedBinaries: []string{"/not/the/stub"}})

	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("EnsureRunning error = %v, want ErrNotPermitted", err)
	}

	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateNotPermitted {
		t.Fatalf("Status()[spec-a].State = %+v, want not_permitted", st)
	}
}

func TestManagerStartTimeout(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(10*time.Second, 0, 0, "") // health never becomes ready in time
	spec.StartupTimeoutSeconds = 1

	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("EnsureRunning error = %v, want ErrStartTimeout", err)
	}

	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateStartFailed {
		t.Fatalf("Status()[spec-a].State = %+v, want start_failed", st)
	}
	if st.LastError == nil {
		t.Fatal("Status()[spec-a].LastError is nil, want it set")
	}
}

func TestManagerEvictsIdleForIncompatible(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	specA := baseSpec("spec-a", "model-a")
	specB := baseSpec("spec-b", "model-b")
	// No coresident pair -> A and B are matrix-incompatible.
	m.Apply(Config{Specs: []Spec{specA, specB}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseA, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning(model-a): %v", err)
	}
	releaseA() // idle now: InFlight 0, evictable

	endpointB, releaseB, err := m.EnsureRunning(ctx, "model-b")
	if err != nil {
		t.Fatalf("EnsureRunning(model-b): %v", err)
	}
	defer releaseB()
	if endpointB == "" {
		t.Fatal("expected a non-empty endpoint for model-b")
	}

	waitUntil(t, 3*time.Second, "spec-a stopped after eviction", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStopped
	})
	st := statusFor(m, "spec-b")
	if st == nil || st.State != StateRunning {
		t.Fatalf("Status()[spec-b].State = %+v, want running", st)
	}
}

func TestManagerNeverEvictsPinned(t *testing.T) {
	skipOnWindows(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := m.EnsureRunning(ctx, "model-b")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAdmissionBlocked) {
		t.Fatalf("EnsureRunning(model-b) error = %v, want ErrAdmissionBlocked", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("EnsureRunning(model-b) returned after %s, want it to have actually waited out the ~1s admission_wait_timeout", elapsed)
	}

	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateRunning {
		t.Fatalf("pinned spec-a must never be evicted, got state %+v", st)
	}
}

func TestManagerQueueWakesOnCompletion(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	specA := baseSpec("spec-a", "model-a")
	specB := baseSpec("spec-b", "model-b")
	m.Apply(Config{Specs: []Spec{specA, specB}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseA, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning(model-a): %v", err)
	}
	// Do NOT release yet: A is busy (InFlight=1), so B cannot evict it.

	type result struct {
		endpoint string
		release  func()
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		ep, rel, err := m.EnsureRunning(ctx, "model-b")
		resultCh <- result{ep, rel, err}
	}()

	// Give B a moment to actually queue (best-effort; correctness does not
	// depend on this, it only makes the "B is still blocked" assertion below
	// meaningful rather than trivially true).
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-resultCh:
		t.Fatalf("EnsureRunning(model-b) resolved before model-a was released: %+v", r)
	default:
	}

	releaseA()

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("EnsureRunning(model-b) after release = %v", r.err)
		}
		if r.endpoint == "" {
			t.Fatal("EnsureRunning(model-b) returned an empty endpoint")
		}
		r.release()
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunning(model-b) did not wake up after model-a was released")
	}
}

func TestManagerDrainWaitsForInFlight(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Do not release: simulate an in-flight request being served.

	// force_stopped triggers a drain even though InFlight > 0.
	forced := spec
	forced.AdminState = "force_stopped"
	m.Apply(Config{Specs: []Spec{forced}})

	waitUntil(t, 2*time.Second, "spec-a Draining", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateDraining
	})

	if !httpGetOK(endpoint, "/health") {
		t.Fatal("process was killed while a request was still in flight -- drain must wait for InFlight to reach zero")
	}

	release()

	waitUntil(t, 3*time.Second, "process actually terminated after release", func() bool {
		return !httpGetOK(endpoint, "/health")
	})
}

func TestManagerCrashBackoffAndLastError(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 100*time.Millisecond, 3, "")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	release()

	waitUntil(t, 3*time.Second, "spec-a reaches backoff after the scripted crash", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateBackoff
	})

	st := statusFor(m, "spec-a")
	if st.LastError == nil {
		t.Fatal("LastError is nil after a crash, want it set")
	}
	if st.LastError.ExitCode != 3 {
		t.Errorf("LastError.ExitCode = %d, want 3", st.LastError.ExitCode)
	}
	if st.LastError.StderrTail == "" || !strings.Contains(st.LastError.StderrTail, "scripted crash") {
		t.Errorf("LastError.StderrTail = %q, want it to contain the crash line", st.LastError.StderrTail)
	}

	// A request arriving during backoff waits for the retry rather than
	// bypassing it: this call must block roughly until backoffBase elapses.
	start := time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	endpoint, release2, err := m.EnsureRunning(ctx2, "model-a")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("EnsureRunning during/after backoff: %v", err)
	}
	defer release2()
	if elapsed < backoffBase/2 {
		t.Errorf("EnsureRunning resolved after only %s, want it to have waited out the backoff (base=%s)", elapsed, backoffBase)
	}
	if endpoint == "" {
		t.Fatal("expected a non-empty endpoint after the backoff retry succeeded")
	}

	stAfter := statusFor(m, "spec-a")
	if stAfter.State != StateRunning {
		t.Fatalf("Status()[spec-a].State = %v, want running after the successful restart", stAfter.State)
	}
	if stAfter.LastError != nil {
		t.Errorf("LastError = %+v, want nil: it must be cleared by the next SUCCESSFUL start", stAfter.LastError)
	}
}

func TestManagerIdleTimeoutUnloads(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.IdleTimeoutSeconds = 1

	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	release()

	waitUntil(t, 4*time.Second, "spec-a idle-unloaded to stopped", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStopped
	})
}

func TestManagerForceStoppedBlocksEnsure(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 0, 0, invocationLog)
	spec.AdminState = "force_stopped"

	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrAdmissionBlocked) {
		t.Fatalf("EnsureRunning error = %v, want ErrAdmissionBlocked", err)
	}

	if n := countInvocations(t, invocationLog); n != 0 {
		t.Errorf("invocation count = %d, want 0: force_stopped must never start a process", n)
	}
}

func TestManagerPinnedStartsOnApply(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 0, 0, invocationLog)
	spec.Pinned = true

	m.Apply(Config{Specs: []Spec{spec}})

	waitUntil(t, 3*time.Second, "pinned spec-a running without any EnsureRunning call", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateRunning
	})

	if n := countInvocations(t, invocationLog); n != 1 {
		t.Errorf("invocation count = %d, want exactly 1", n)
	}
}

// TestManagerConcurrentEnsureSingleStart is the load-bearing proof that the
// serialized owner works: many concurrent EnsureRunning calls for one cold
// spec must start EXACTLY ONE process. Counted from the CHILD side (the
// stub's own invocation log, appended to once per real exec), never from
// the manager's own bookkeeping -- the manager's view is exactly what this
// test exists to validate, so it cannot also be the source of truth.
func TestManagerConcurrentEnsureSingleStart(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 0, 0, invocationLog)
	m.Apply(Config{Specs: []Spec{spec}})

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	endpoints := make([]string, n)
	releases := make([]func(), n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ep, rel, err := m.EnsureRunning(ctx, "model-a")
			endpoints[i], releases[i], errs[i] = ep, rel, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: EnsureRunning error = %v", i, err)
		}
	}
	for i, rel := range releases {
		if rel != nil {
			rel()
		} else {
			_ = endpoints[i]
		}
	}

	if got := countInvocations(t, invocationLog); got != 1 {
		t.Fatalf("child-side invocation count = %d, want exactly 1 (single-start property violated)", got)
	}
}

func TestManagerTransitionsCoalesce(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	m.Apply(Config{Specs: []Spec{spec}})

	// Drain any signal from Apply/registration before the real assertion.
	select {
	case <-m.Transitions():
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	defer release()
	// This single EnsureRunning call alone drives at least two transitions
	// (Stopped->Starting, Starting->Running); buffered(1) must coalesce them.

	select {
	case <-m.Transitions():
	default:
		t.Fatal("expected a pending transition signal after at least two state changes")
	}
	select {
	case <-m.Transitions():
		t.Fatal("Transitions() must be buffered(1): a second receive right after the first must find nothing pending")
	default:
	}
}
