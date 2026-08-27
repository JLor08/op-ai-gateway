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
	"sync/atomic"
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

	// Fix round 1 / I2: a failed start now enters backoff exactly like a
	// crash (rate-limits a non-pinned spec's retries and gives a pinned
	// one a retry at all), so start_failed is transient -- by the time
	// Status() is polled it has almost always already advanced to backoff
	// in the same synchronous transition, the same way a crash's
	// momentary "crashed" is never observable either.
	st := statusFor(m, "spec-a")
	if st == nil || (st.State != StateStartFailed && st.State != StateBackoff) {
		t.Fatalf("Status()[spec-a].State = %+v, want start_failed or backoff", st)
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

// TestManagerQueueWakesOnCompletion proves B waits for A's release and is
// woken by it, WITHOUT the M6-flagged time.Sleep(50ms) "peek and see
// nothing" heuristic: instead it compares monotonic timestamps. B's
// EnsureRunning call can only complete via the owner processing A's
// release -> wakeAllPendingWaiters -> admitAndStart(B) -> succeedPending,
// so if B ever resolved BEFORE release was even called (e.g. a bug that
// evicts a "busy" process), its recorded completion time would be provably
// earlier than the moment release was invoked. No polling, no sleep, no
// flaky race window.
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
		at       time.Time
	}
	resultCh := make(chan result, 1)
	go func() {
		ep, rel, err := m.EnsureRunning(ctx, "model-b")
		resultCh <- result{ep, rel, err, time.Now()}
	}()

	releasedAt := time.Now()
	releaseA()

	select {
	case r := <-resultCh:
		if r.at.Before(releasedAt) {
			t.Fatalf("EnsureRunning(model-b) completed at %v, before model-a's release was even called at %v -- it must have waited for the release, not evicted a busy process", r.at, releasedAt)
		}
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

// ---------------------------------------------------------------------------
// Fix round 1 (task-14-fix-round-1.md): C1, C2, C3, I1, I2, I3, I4 below are
// each written to FAIL against the pre-fix code -- confirmed by running them
// before applying the corresponding manager.go change; see
// task-14-report.md's fix-round-1 appendix for the captured "red" output.
// I5's two tests are new COVERAGE for already-correct behavior (not bugs),
// so they pass immediately, before and after this round's other changes.
// ---------------------------------------------------------------------------

// TestManagerEnsureRunningReturnsOnContextCancelWhileQueued is C1: a queued
// EnsureRunning call whose context is cancelled must return promptly, not
// hang forever. Before the fix, handleCancelEnsure removed the waiter from
// st.pending WITHOUT ever sending to its reply channel, so the caller never
// unblocked (only Manager.Close, at test cleanup, would have ended it).
func TestManagerEnsureRunningReturnsOnContextCancelWhileQueued(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	specA := baseSpec("spec-a", "model-a")
	specA.Pinned = true
	specB := baseSpec("spec-b", "model-b")
	specB.AdmissionWaitTimeoutSeconds = 0 // wait until ctx is done -- only cancellation can end this wait
	m.Apply(Config{Specs: []Spec{specA, specB}})

	waitUntil(t, 3*time.Second, "pinned spec-a running", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateRunning
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := m.EnsureRunning(ctx, "model-b")
		errCh <- err
	}()

	// Let model-b actually reach st.pending (past the first select in
	// EnsureRunning) before cancelling -- otherwise cancelling might only
	// exercise the already-correct "cancelled before the send" path instead
	// of the queued-waiter path this test targets.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("EnsureRunning returned a nil error after its context was cancelled while queued")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureRunning did not return within 2s of its context being cancelled while queued -- BUG C1: handleCancelEnsure removes the waiter without ever resolving its reply channel")
	}
}

// TestManagerEnsureRunningResolvesWhenSpecRemovedByApply is C2: a waiter
// queued on a spec that Apply then removes must be resolved, not dropped.
// Before the fix, applyConfig's delete(o.specs, id) (and onProcExited's,
// for a removed spec that was still draining) discarded st.pending
// entirely, and handleWaiterTimeout returned silently once st was nil --
// so even a configured AdmissionWaitTimeoutSeconds could not rescue the
// caller.
func TestManagerEnsureRunningResolvesWhenSpecRemovedByApply(t *testing.T) {
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

	errCh := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, _, err := m.EnsureRunning(ctx, "model-b")
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond) // let model-b actually queue before removing its spec
	m.Apply(Config{Specs: []Spec{specA}})

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrModelNotManaged) {
			t.Errorf("EnsureRunning(model-b) after its spec was removed = %v, want ErrModelNotManaged", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EnsureRunning(model-b) did not resolve within 3s of its spec being removed by Apply -- BUG C2: a waiter on a removed spec is dropped without being resolved, and even its own admission_wait_timeout cannot rescue it because handleWaiterTimeout returns silently once the spec is gone")
	}
}

// TestManagerInFlightResetAfterCrash is C3: InFlight must return to zero
// once the process that was serving a request is gone, even if release()
// is never called (exactly what happens in production when the connection
// dies along with the crash). Before the fix, onProcExited never touched
// st.inFlight, so a spec that crashed once under load became permanently
// un-evictable: Admit's isEvictable requires InFlight==0, so the stuck
// counter made this spec block every incompatible model as though it were
// pinned, and it could never idle-unload either.
func TestManagerInFlightResetAfterCrash(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 100*time.Millisecond, 3, "")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Deliberately never call release(): the request was in flight when the
	// child crashed.

	waitUntil(t, 3*time.Second, "spec-a reaches backoff after the scripted crash", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateBackoff
	})

	st := statusFor(m, "spec-a")
	if st.InFlight != 0 {
		t.Fatalf("InFlight = %d after a crash with no release() call, want 0 -- BUG C3: onProcExited never resets InFlight, so this spec becomes permanently un-evictable/never idle-unloadable, as if pinned", st.InFlight)
	}
}

// TestBackoffDelayForEscalatesAndCaps is a fast, deterministic unit check
// on the pure backoff-math helper (no processes spawned) -- a cheap
// complement to TestManagerCrashBackoffEscalates below, which proves the
// Manager actually uses this escalation rather than resetting failures too
// early.
func TestBackoffDelayForEscalatesAndCaps(t *testing.T) {
	origBase, origCap := backoffBase, backoffCap
	backoffBase, backoffCap = 100*time.Millisecond, 1*time.Second
	defer func() { backoffBase, backoffCap = origBase, origCap }()

	d1 := backoffDelayFor(1)
	d2 := backoffDelayFor(2)
	d3 := backoffDelayFor(3)
	if d1 != 100*time.Millisecond {
		t.Errorf("backoffDelayFor(1) = %s, want 100ms", d1)
	}
	if d2 <= d1 {
		t.Errorf("backoffDelayFor(2) = %s, want > backoffDelayFor(1) = %s", d2, d1)
	}
	if d3 <= d2 {
		t.Errorf("backoffDelayFor(3) = %s, want > backoffDelayFor(2) = %s", d3, d2)
	}
	if d10 := backoffDelayFor(10); d10 != 1*time.Second {
		t.Errorf("backoffDelayFor(10) = %s, want capped at 1s", d10)
	}
}

// TestManagerCrashBackoffEscalates is I1: repeated crashes must produce
// escalating backoff delays, not the same base delay every time. Before
// the fix, handleStartResult's success branch reset st.failures to 0 on
// EVERY successful (re)start, so backoffDelayFor(1) -- the base delay --
// was recomputed after every single crash, and handleStableRun's
// stable-run reset (which already implements the behavior the brief asked
// for) was unreachable dead code.
func TestManagerCrashBackoffEscalates(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t) // this suite's backoffBase=60ms, backoffCap=500ms

	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true // so each backoff-fire automatically retries (wantUp requires pending>0 || Pinned || force_running)
	spec.Args = stubArgs(0, 20*time.Millisecond, 1, invocationLog)
	m.Apply(Config{Specs: []Spec{spec}})

	const window = 2 * time.Second
	time.Sleep(window)

	got := countInvocations(t, invocationLog)
	// Constant (buggy) backoff: ~20ms crash + ~60ms base delay per cycle =>
	// roughly 20-25 restarts in 2s. True escalation (60,120,240,480,500,500
	// ms, capped) => roughly 5 restarts in the same window. 15 cleanly
	// separates the two without being timing-fragile.
	const maxWithEscalation = 15
	if got > maxWithEscalation {
		t.Fatalf("invocation count = %d within %s, want <= %d -- BUG I1: st.failures is reset to 0 on every successful start, so every crash computes the same base backoff delay instead of escalating", got, window, maxWithEscalation)
	}
}

// TestManagerStartFailedRetriesWhenPinned is I2(a): a pinned spec whose
// start never becomes healthy must still be retried automatically, not go
// permanently dead after one failure. Before the fix, StateStartFailed had
// no backoff scheduled at all, so a pinned spec sat in start_failed forever
// until an unrelated config edit.
func TestManagerStartFailedRetriesWhenPinned(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true
	spec.Args = stubArgs(10*time.Second, 0, 0, "") // health never arrives
	spec.StartupTimeoutSeconds = 1
	m.Apply(Config{Specs: []Spec{spec}})

	waitUntil(t, 3*time.Second, "first start failure observed", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.LastError != nil && st.LastError.Failures >= 1
	})

	waitUntil(t, 6*time.Second, "a SECOND start failure observed after backoff", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.LastError != nil && st.LastError.Failures >= 2
	})
}

// TestManagerStartFailedIsRateLimitedForNonPinned is I2(b): a non-pinned
// spec whose start always fails must not re-exec once per incoming
// request with no rate limit. Before the fix, every EnsureRunning call
// against a start_failed spec triggered a brand new exec attempt.
func TestManagerStartFailedIsRateLimitedForNonPinned(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origBase, origCap := backoffBase, backoffCap
	// Long enough that a second rapid request must queue, not re-exec.
	// backoffCap must be raised too -- shrinkTimings leaves it at 500ms,
	// which would otherwise silently clamp this back down.
	backoffBase = 5 * time.Second
	backoffCap = 10 * time.Second
	defer func() { backoffBase, backoffCap = origBase, origCap }()

	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(10*time.Second, 0, 0, invocationLog) // health never arrives -> always start_timeout
	spec.StartupTimeoutSeconds = 1
	// Deliberately larger than StartupTimeoutSeconds so this test's first
	// assertion isolates I2(b) even before I3 (the admission-wait timer
	// wrongly running through startup) is fixed -- otherwise the two timers
	// could race at ~1s and the first call could return ErrAdmissionBlocked
	// instead of ErrStartTimeout for an unrelated reason.
	spec.AdmissionWaitTimeoutSeconds = 3
	m.Apply(Config{Specs: []Spec{spec}})

	ctx1, cancel1 := context.WithTimeout(context.Background(), 4*time.Second)
	_, _, err1 := m.EnsureRunning(ctx1, "model-a")
	cancel1()
	if !errors.Is(err1, ErrStartTimeout) {
		t.Fatalf("first EnsureRunning error = %v, want ErrStartTimeout", err1)
	}
	if got := countInvocations(t, invocationLog); got != 1 {
		t.Fatalf("invocation count after the first failed attempt = %d, want 1", got)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err2 := m.EnsureRunning(ctx2, "model-a")
	cancel2()
	if !errors.Is(err2, ErrAdmissionBlocked) {
		t.Fatalf("second EnsureRunning error (issued during the still-active 5s backoff) = %v, want ErrAdmissionBlocked (queued, not re-executed)", err2)
	}
	if got := countInvocations(t, invocationLog); got != 1 {
		t.Fatalf("invocation count after a second request during an active backoff = %d, want still 1 -- BUG I2(b): a non-pinned start_failed spec re-execs once per request with no rate limit", got)
	}
}

// TestManagerAdmissionWaitDoesNotFireDuringStartup is I3: the brief
// specifies EnsureRunning may block for admission-wait PLUS startup, but
// before the fix the admission-wait timer was never cancelled once the
// target actually started -- so the first request to any cold model
// slower than its own admission_wait_timeout_seconds got a misleading
// ErrAdmissionBlocked instead of the endpoint it should have received a
// few seconds later.
func TestManagerAdmissionWaitDoesNotFireDuringStartup(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(2*time.Second, 0, 0, "") // healthy at ~2s
	spec.StartupTimeoutSeconds = 5
	spec.AdmissionWaitTimeoutSeconds = 1 // shorter than the time-to-healthy
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	endpoint, release, err := m.EnsureRunning(ctx, "model-a")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("EnsureRunning = %v after %s -- BUG I3: the admission-wait timer is never cancelled once the target enters Starting, so it wrongly fires mid-startup instead of letting the caller wait for health", err, elapsed)
	}
	defer release()
	if endpoint == "" {
		t.Fatal("expected a non-empty endpoint")
	}
	if elapsed < 1900*time.Millisecond {
		t.Errorf("EnsureRunning returned after only %s, want it to have actually waited for the ~2s health delay", elapsed)
	}
}

// TestManagerCrashDuringDrainIsClassifiedAsCrashNotStartFailure is I4: a
// child that exits on its own (not because the owner signalled it) while
// Draining must still be classified as a crash, since it had already
// passed its health check and was genuinely serving. Before the fix,
// onProcExited keyed the classification off the CURRENT state
// (st.state == StateRunning), which is false while Draining, so this
// exact scenario was misreported as start_failed with "process exited
// before becoming healthy" for a process that had been healthy all along.
func TestManagerCrashDuringDrainIsClassifiedAsCrashNotStartFailure(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 100*time.Millisecond, 7, "")
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Do not release: InFlight stays 1, so the drain below cannot terminate
	// the process immediately -- it must sit in StateDraining, waiting for
	// InFlight to reach zero, when the scripted crash fires on its own.

	forced := spec
	forced.AdminState = "force_stopped"
	m.Apply(Config{Specs: []Spec{forced}})

	waitUntil(t, 2*time.Second, "spec-a is Draining", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateDraining
	})

	waitUntil(t, 3*time.Second, "the crash is eventually reported", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.LastError != nil
	})

	st := statusFor(m, "spec-a")
	if st.State != StateCrashed && st.State != StateBackoff {
		t.Fatalf("State = %v, want crashed (or backoff, immediately after) -- BUG I4: a child that exits on its own while Draining is misclassified as start_failed because onProcExited keys off the CURRENT state instead of whether the process had ever passed a health check", st.State)
	}
	if st.LastError == nil || st.LastError.ExitCode != 7 {
		t.Fatalf("LastError = %+v, want ExitCode=7", st.LastError)
	}
	if !strings.Contains(st.LastError.Message, "unexpectedly") {
		t.Errorf("LastError.Message = %q, want it to say the process crashed unexpectedly, not that it failed to become healthy", st.LastError.Message)
	}
}

// TestManagerRejectsReservedEnvKeyPathOrHome is I5 (first of two): a
// manager-level test that a spec Env key of PATH or HOME -- one of the two
// refusals that each cost a review round to establish in ExpandPlaceholders
// -- actually surfaces as ErrNotPermitted/StateNotPermitted THROUGH the
// Manager, not just at the policy_local unit level.
func TestManagerRejectsReservedEnvKeyPathOrHome(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Env = map[string]string{"PATH": "/evil"}
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("EnsureRunning error = %v, want ErrNotPermitted (spec env key PATH is agent-owned)", err)
	}
	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateNotPermitted {
		t.Fatalf("Status()[spec-a].State = %+v, want not_permitted", st)
	}
}

// TestManagerRejectsAgentOwnEnvNamespaceReference is I5 (second of two):
// ${AGENT_ENV:OP_AGENT_*} must never reach a spec through the Manager --
// the exact exfiltration path a prior review round closed in
// ExpandPlaceholders.
func TestManagerRejectsAgentOwnEnvNamespaceReference(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Env = map[string]string{"TOKEN": "${AGENT_ENV:OP_AGENT_TOKEN}"}
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("EnsureRunning error = %v, want ErrNotPermitted (${AGENT_ENV:OP_AGENT_*} must never reach a spec)", err)
	}
	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateNotPermitted {
		t.Fatalf("Status()[spec-a].State = %+v, want not_permitted", st)
	}
}

// TestManagerNotPermittedRetriesOnNextRequestNotOnApply is the I6 decision
// recorded as a test: a spec that was StateNotPermitted because of a
// missing ${AGENT_ENV:...} variable must succeed on the very next
// EnsureRunning call once the operator sets that variable on the agent
// host -- WITHOUT requiring an unrelated Apply/ETag change to clear the
// stuck state. See task-14-report.md's I6 section for the full reasoning.
// TestManagerNotPermittedRetriesOnNextRequestNotOnApply is I6's property,
// updated for fix round 2 (R2-1) to account for notPermittedRetryInterval:
// a request INSIDE the interval must still get the cached verdict (the
// rate limit's whole point), but a request AFTER the interval elapses
// must re-evaluate fully and succeed once the underlying problem is
// fixed -- with no Apply/config change. This must not regress: R2-1 added
// a bound, not a return to I6's original stickiness.
func TestManagerNotPermittedRetriesOnNextRequestNotOnApply(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origInterval := notPermittedRetryInterval
	notPermittedRetryInterval = 200 * time.Millisecond
	defer func() { notPermittedRetryInterval = origInterval }()

	var tokenSet atomic.Bool
	getenv := func(name string) string {
		if name == "HF_TOKEN" && tokenSet.Load() {
			return "shh"
		}
		return ""
	}
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: getenv})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-a", "model-a")
	spec.Env = map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"}
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("first EnsureRunning error = %v, want ErrNotPermitted (HF_TOKEN not yet set)", err)
	}

	// The operator fixes it on the AGENT HOST -- no gateway config change,
	// no Apply call, no ETag change. A request made immediately (still
	// inside the interval) must still get the CACHED verdict: that is the
	// rate limit's whole point, not a regression back to stickiness.
	tokenSet.Store(true)
	ctxImmediate, cancelImmediate := context.WithTimeout(context.Background(), 2*time.Second)
	_, _, errImmediate := m.EnsureRunning(ctxImmediate, "model-a")
	cancelImmediate()
	if !errors.Is(errImmediate, ErrNotPermitted) {
		t.Fatalf("EnsureRunning immediately after the fix (still inside notPermittedRetryInterval) = %v, want the cached ErrNotPermitted -- a request inside the interval must not re-evaluate yet", errImmediate)
	}

	time.Sleep(notPermittedRetryInterval + 100*time.Millisecond)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	endpoint, release, err := m.EnsureRunning(ctx2, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning after the operator fixed the missing env var and the retry interval elapsed = %v, want success -- I6 must not regress: recovery needs no Apply/config change", err)
	}
	defer release()
	if endpoint == "" {
		t.Fatal("expected a non-empty endpoint")
	}
}

// TestManagerNotPermittedRateLimitsReEvaluation is R2-1: N rapid requests
// against a NotPermitted spec must perform only ONE full evaluation, not
// one per request. Counted from the FAKE's side (the injected Getenv
// closure's own call count), not from manager bookkeeping -- a missing
// (non-agent-owned) ${AGENT_ENV:NAME} is used as the failure cause because
// it is the one ExpandPlaceholders rejection that still calls getenv
// before erroring (the PATH/HOME and ${AGENT_ENV:OP_AGENT_*} causes
// deliberately never call getenv at all, by design -- see
// policy_local.go), giving a real external side effect to count.
func TestManagerNotPermittedRateLimitsReEvaluation(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origInterval := notPermittedRetryInterval
	notPermittedRetryInterval = 2 * time.Second
	defer func() { notPermittedRetryInterval = origInterval }()

	var getenvCalls atomic.Int64
	getenv := func(name string) string {
		if name == "MISSING_TOKEN" {
			getenvCalls.Add(1)
		}
		return ""
	}
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: getenv})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-a", "model-a")
	spec.Env = map[string]string{"HF_TOKEN": "${AGENT_ENV:MISSING_TOKEN}"}
	m.Apply(Config{Specs: []Spec{spec}})

	const n = 5
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, err := m.EnsureRunning(ctx, "model-a")
		cancel()
		if !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("attempt %d: EnsureRunning error = %v, want ErrNotPermitted", i, err)
		}
	}

	if got := getenvCalls.Load(); got != 1 {
		t.Fatalf("getenv(MISSING_TOKEN) called %d times across %d rapid requests within the retry interval, want exactly 1 -- BUG: NotPermitted must be rate-limited, not re-evaluated (ExpandPlaceholders re-run) on every request", got, n)
	}
}

// TestManagerPendingVRAMUnknownRateLimitsReEvaluation is R2-1's other
// half: N rapid requests against a PendingVRAMUnknown spec must invoke
// the installed measurer only ONCE, not once per request -- otherwise,
// once a later task wires a real nvidia-smi-backed measurer, a client
// retrying against a VRAM-blocked spec would drive an external process
// call at request rate on the single serialized owner goroutine.
func TestManagerPendingVRAMUnknownRateLimitsReEvaluation(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origInterval := notPermittedRetryInterval
	notPermittedRetryInterval = 2 * time.Second
	defer func() { notPermittedRetryInterval = origInterval }()

	m := newTestManager(t, allowlistPolicy())

	var measurerCalls atomic.Int64
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		measurerCalls.Add(1)
		return nil
	})

	pinned := baseSpec("spec-pinned", "model-pinned")
	pinned.Pinned = true
	pinned.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}

	unknown := baseSpec("spec-unknown", "model-unknown")
	unknown.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}} // unknown demand, touches the pinned spec's GPU

	m.Apply(Config{Specs: []Spec{pinned, unknown}})

	waitUntil(t, 3*time.Second, "pinned spec-pinned running", func() bool {
		st := statusFor(m, "spec-pinned")
		return st != nil && st.State == StateRunning
	})
	measurerCalls.Store(0) // the pinned spec's own start also builds a snapshot; isolate the count to what follows

	const n = 5
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, err := m.EnsureRunning(ctx, "model-unknown")
		cancel()
		if !errors.Is(err, ErrAdmissionBlocked) {
			t.Fatalf("attempt %d: EnsureRunning error = %v, want ErrAdmissionBlocked", i, err)
		}
	}

	if got := measurerCalls.Load(); got != 1 {
		t.Fatalf("measurer invoked %d times across %d rapid requests within the retry interval, want exactly 1 -- BUG: PendingVRAMUnknown must be rate-limited, not re-evaluated (and re-measured) on every request", got, n)
	}
}

// TestManagerApplyDoesNotResetBackoffOnUnrelatedSpecChange is M1: an
// Apply that changes spec B must not let spec A's crash backoff skip its
// current wait. Before the fix, applyConfig's terminal-state reset ran for
// every spec whenever the overall Config's ETag changed, regardless of
// whether THAT spec's own fields were touched.
func TestManagerApplyDoesNotResetBackoffOnUnrelatedSpecChange(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origBase, origCap := backoffBase, backoffCap
	// Long enough that an early Stopped would be observable as a bug;
	// backoffCap must be raised too, or shrinkTimings' 500ms cap would
	// silently clamp this back down.
	backoffBase = 3 * time.Second
	backoffCap = 10 * time.Second
	defer func() { backoffBase, backoffCap = origBase, origCap }()

	m := newTestManager(t, allowlistPolicy())

	specA := baseSpec("spec-a", "model-a")
	specA.Args = stubArgs(0, 50*time.Millisecond, 1, "")
	specB := baseSpec("spec-b", "model-b")
	m.Apply(Config{Specs: []Spec{specA, specB}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning(model-a): %v", err)
	}
	release()

	waitUntil(t, 2*time.Second, "spec-a reaches backoff", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateBackoff
	})

	// Touch only spec-b (spec-a's own fields are byte-identical).
	specBChanged := specB
	specBChanged.IdleTimeoutSeconds = 42
	m.Apply(Config{Specs: []Spec{specA, specBChanged}})

	st := statusFor(m, "spec-a")
	if st.State != StateBackoff {
		t.Fatalf("spec-a State = %v after an Apply that only touched spec-b, want it to still be backoff -- BUG M1: any config Apply resets every spec's backoff, letting a crash-looping spec skip its wait after an unrelated edit", st.State)
	}
}

// ---------------------------------------------------------------------------
// B6: Task 14's deferred concurrency residue, re-derived. Each test below
// fails against the pre-fix manager; the pasted failures are in
// task-22b-batch-b-report.md.
//
// stubArgsStubborn is stubArgs plus -ignore-sigterm: the child keeps serving
// /health after being signalled, so a test can actually look at manager state
// during the window between "this generation was told to die" and "this
// generation is gone". A cooperative child closes that window in
// microseconds, which is precisely why these three defects were only ever
// reasoned about rather than reproduced.
// ---------------------------------------------------------------------------

func stubArgsStubborn(healthDelay time.Duration) []string {
	return append(stubArgs(healthDelay, 0, 0, ""), "-ignore-sigterm")
}

// setKillGrace overrides killGrace AFTER shrinkTimings (whose t.Cleanup
// restores the original) and BEFORE the Manager is created, per
// shrinkTimings' documented ordering rule. Used to widen the
// SIGTERM->SIGKILL window so a stubborn child stays observably alive.
func setKillGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := killGrace
	killGrace = d
	t.Cleanup(func() { killGrace = orig })
}

// waitUntilChildIsServing blocks until specID's child answers its health
// endpoint with ANY HTTP status (503 included: the stub answers 503 until its
// -health-delay elapses). Reaching that point proves the child got as far as
// http.ListenAndServe, which in stubchild's main() is strictly AFTER
// signal.Ignore(SIGTERM).
//
// This is load-bearing, not defensive. Without it these tests were vacuous:
// a spec is observably StateStarting within microseconds of cmd.Start(), long
// before the freshly exec'd child has finished Go runtime init and installed
// its SIGTERM disposition -- so a drain triggered "as soon as it is Starting"
// killed the child outright with SIGTERM's DEFAULT action, pollHealth
// returned on proc.exited without ever posting a result, and the very race
// the test names could not occur. It passed against the unfixed manager.
func waitUntilChildIsServing(t *testing.T, m *Manager, specID string) {
	t.Helper()
	waitUntil(t, 5*time.Second, "spec "+specID+"'s child is listening (so it has installed its SIGTERM disposition)", func() bool {
		st := statusFor(m, specID)
		if st == nil || st.Port == 0 {
			return false
		}
		resp, err := http.Get(endpointFor(st.Port) + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
}

// TestManagerLateHealthResultDoesNotResurrectADrainingSpec is B6's third
// item, the PRE-EXISTING one: handleStartResult's setState was unconditional,
// so a health probe that lands after beginDrain already moved the spec to
// StateDraining overwrites that deliberate transition.
//
// The ok=true branch is the damaging one, and it is worse than a transient
// wrong status:
//
//   - StateRunning puts the spec back into LoadedModels(), which the router
//     serves as /running and the agent reports as the AUTHORITATIVE
//     loaded_models telemetry field (design spec §7: "the flat loaded_models
//     list carries only truly loaded models"). The gateway then routes fresh
//     requests to a process it has just been told to shut down.
//   - succeedPending hands queued waiters that same dying process's endpoint.
//   - and it does not end when the process does: onProcExited's intentional
//     branch only advances Draining->Stopped, so a spec left in StateRunning
//     stays StateRunning with proc == nil after the exit. Nothing self-heals
//     it -- scanIdle skips proc == nil, and handleEnsure starts a fresh
//     generation without correcting the state -- so /running and
//     loaded_models keep advertising a model that is not loaded until the
//     next request for it happens to arrive.
//
// force_stopped (not removal) is the vehicle: a removed spec gets deleted on
// exit, which would mask the persistent half of the bug.
func TestManagerLateHealthResultDoesNotResurrectADrainingSpec(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setKillGrace(t, 2*time.Second) // wide enough that the child is still serving when health goes green
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true // starts on Apply, no request needed
	spec.Args = stubArgsStubborn(400 * time.Millisecond)
	spec.StartupTimeoutSeconds = 5
	m.Apply(Config{Specs: []Spec{spec}})

	waitUntil(t, 3*time.Second, "spec-a is Starting", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStarting
	})
	waitUntilChildIsServing(t, m, "spec-a")

	forced := spec
	forced.AdminState = "force_stopped"
	m.Apply(Config{Specs: []Spec{forced}})

	waitUntil(t, 2*time.Second, "spec-a is Draining", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateDraining
	})

	// The child ignores SIGTERM and answers /health at ~400ms, so the late
	// cmdStartResult{ok:true} arrives here, well inside the drain window.
	// Poll across it: the spec must never be advertised as loaded, and must
	// never report StateRunning.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, name := range m.LoadedModels() {
			if name == "model-a" {
				t.Fatalf("LoadedModels() advertised model-a while its spec was force_stopped and draining -- BUG: handleStartResult's unconditional setState(StateRunning) overwrites beginDrain's StateDraining, so the gateway routes to a process the agent is shutting down")
			}
		}
		if st := statusFor(m, "spec-a"); st != nil && st.State == StateRunning {
			t.Fatalf("Status()[spec-a].State = running while force_stopped and draining -- BUG: a late health result resurrects a deliberately drained generation")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// And after SIGKILL actually ends it: a force_stopped spec must rest in a
	// stopped-shaped state with no process, never in StateRunning.
	waitUntil(t, 4*time.Second, "spec-a's process is gone", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.PID == 0
	})
	st := statusFor(m, "spec-a")
	if st.State == StateRunning {
		t.Fatalf("Status()[spec-a] = %+v after the process exited -- BUG: the spec is stuck in StateRunning with no process, so /running and the authoritative loaded_models telemetry keep advertising a model that is not loaded", st)
	}
	for _, name := range m.LoadedModels() {
		if name == "model-a" {
			t.Fatalf("LoadedModels() still advertises model-a after its process exited (state = %v)", st.State)
		}
	}
}

// TestManagerNeverReportsBackoffWhileItsProcessIsStillAlive is B6's first
// item: handleStartResult's start-timeout path called enterBackoff while
// st.proc was still non-nil (terminateNow only SIGTERMs and schedules the
// SIGKILL escalation; the exit is reported later).
//
// Two consequences, neither cosmetic now that the portal has a live status
// table:
//
//   - the reported state contradicts the reported PID: "backoff" (design spec
//     §7: crash-loop WAIT) or, once the backoff elapses, "stopped", for a spec
//     whose process is still running and still holding its VRAM;
//   - the retry is silently dropped. handleBackoffFire clears the timer, sets
//     StateStopped and calls admitAndStart, which returns immediately on
//     st.proc != nil. Whenever the backoff delay is shorter than the child's
//     time-to-die (the first two failures, at backoffBase and 2*backoffBase,
//     against any child that does not die instantly on SIGTERM), the
//     escalating backoff is defeated: the actual restart happens whenever the
//     process finally exits instead.
//
// The invariant asserted: StateBackoff and StateStopped both mean "nothing of
// this spec is running", so neither may ever be reported together with a live
// PID.
func TestManagerNeverReportsBackoffWhileItsProcessIsStillAlive(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setKillGrace(t, 1500*time.Millisecond) // the observation window
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	// Deliberately NOT pinned: a pinned spec is restarted after its backoff
	// with a fresh pid, so "the timed-out generation is gone" would never be
	// observable as PID == 0 and this test could not tell the window it is
	// about from a restart.
	spec.Args = stubArgsStubborn(30 * time.Second) // health never arrives -> start timeout
	spec.StartupTimeoutSeconds = 1
	spec.AdmissionWaitTimeoutSeconds = 0 // wait on ctx only; the start timeout is what must resolve this
	m.Apply(Config{Specs: []Spec{spec}})

	// One request triggers the start and then reports the start timeout.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, release, _ := m.EnsureRunning(ctx, "model-a")
		if release != nil {
			release()
		}
	}()
	waitUntilChildIsServing(t, m, "spec-a")

	var violations []string
	sawFailure, sawProcGone := false, false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := statusFor(m, "spec-a")
		if st != nil {
			if st.LastError != nil {
				sawFailure = true
			}
			if st.PID != 0 && (st.State == StateBackoff || st.State == StateStopped) {
				violations = append(violations, fmt.Sprintf("state=%s pid=%d", st.State, st.PID))
			}
			if sawFailure && st.PID == 0 {
				sawProcGone = true
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawFailure {
		t.Fatal("never observed the start timeout being recorded; the test did not reach the window it is about")
	}
	if !sawProcGone {
		t.Fatal("the timed-out generation never went away; the test did not complete the window it is about")
	}
	if len(violations) > 0 {
		t.Fatalf("observed %d snapshot(s) reporting a resting state together with a LIVE pid (first: %s) -- BUG: handleStartResult's start-timeout path enters backoff while st.proc is still non-nil, so the portal shows %q for a process that is still running and the backoff retry is dropped by admitAndStart's st.proc != nil guard", len(violations), violations[0], violations[0])
	}
}

// TestManagerAdmissionWaitDoesNotFireForARequestQueuedDuringStartup is B6's
// second item, re-derived. It was recorded as a "nanosecond-scale Stop()
// race" in I3's timer cancellation -- cancelTimer losing to a timer callback
// that then fails the waiter with ErrAdmissionBlocked despite the process
// having started. That race is real but astronomically narrow.
//
// Re-deriving it exposed a MUCH wider instance of the identical defect, with
// no race at all: I3 cancels the admission-wait timers of the waiters that
// exist AT startProcess TIME. Every request that arrives AFTER the spec
// entered StateStarting gets a fresh admission-wait timer from handleEnsure
// (state is Starting, not Running, so it queues), and nothing ever cancels
// that one. So the SECOND and later concurrent requests for a cold model
// whose load outlasts admission_wait_timeout_seconds get exactly the
// misleading ErrAdmissionBlocked that I3 exists to prevent -- deterministic,
// on ordinary traffic, not a nanosecond window.
//
// The design-spec meaning is the same in both cases: admission_wait_timeout
// bounds "how long a request may queue when blocked by busy/pinned
// processes" (§4.1). Once THIS spec's own process exists, the request is not
// blocked by anything -- it is waiting on startup, which
// startup_timeout_seconds already bounds.
func TestManagerAdmissionWaitDoesNotFireForARequestQueuedDuringStartup(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(2*time.Second, 0, 0, "") // healthy at ~2s
	spec.StartupTimeoutSeconds = 5
	spec.AdmissionWaitTimeoutSeconds = 1 // shorter than the time-to-healthy
	m.Apply(Config{Specs: []Spec{spec}})

	// Request A triggers the start; I3 cancels ITS timer.
	firstDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, release, err := m.EnsureRunning(ctx, "model-a")
		if release != nil {
			release()
		}
		firstDone <- err
	}()

	waitUntil(t, 3*time.Second, "spec-a is Starting", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStarting
	})

	// Request B arrives with the process ALREADY starting: handleEnsure gives
	// it a brand new admission-wait timer that startProcess has long since
	// stopped being able to cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	endpoint, release, err := m.EnsureRunning(ctx, "model-a")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second EnsureRunning (issued while spec-a was already Starting) = %v after %s -- BUG: the admission-wait timer of a request queued DURING startup is never cancelled, so it fires ErrAdmissionBlocked for a model that was about to become healthy. I3 only covers the waiters that existed when startProcess ran.", err, elapsed)
	}
	release()
	if endpoint == "" {
		t.Fatal("second EnsureRunning returned an empty endpoint")
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first EnsureRunning = %v, want nil", err)
	}
}

// TestManagerCloseDoesNotWaitOutACrashBackoff is NOT one of B6's three
// items; it surfaced while re-deriving them and is the same shape.
//
// enterBackoff schedules its retry timer through Manager.scheduleAfter, which
// registers it in m.wg -- and Close() ends with m.wg.Wait(). handleClose
// cancels the backoff timers that exist when it runs, but nothing stops a NEW
// one from being scheduled afterwards, so a backoff entered during shutdown
// makes Close block for the whole backoff delay (up to backoffCap, 60s by
// default) with nothing left to retry.
//
// Reached deterministically: a Running spec with a request still in flight
// drains on Close via the drainGrace path, which does NOT set intentionalStop
// (only terminateNow does, and that has not run yet). The scripted crash
// then lands as a NON-intentional exit -> StateCrashed -> enterBackoff, after
// closing is already true.
func TestManagerCloseDoesNotWaitOutACrashBackoff(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	origDrain, origBase, origCap := drainGrace, backoffBase, backoffCap
	drainGrace = 3 * time.Second  // long enough that the scripted crash lands first
	backoffBase = 4 * time.Second // an observable hang if Close waits it out
	backoffCap = 8 * time.Second
	t.Cleanup(func() { drainGrace, backoffBase, backoffCap = origDrain, origBase, origCap })

	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }})
	closed := false
	defer func() {
		if !closed {
			m.Close()
		}
	}()

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgs(0, 700*time.Millisecond, 9, "") // healthy at once, crashes at ~700ms
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := m.EnsureRunning(ctx, "model-a"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Deliberately NOT released: InFlight stays 1, so Close's beginDrain waits
	// out drainGrace instead of terminating immediately -- and therefore never
	// marks intentionalStop before the scripted crash arrives.

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		m.Close()
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		closed = true
		if took > 3*time.Second {
			t.Fatalf("Close() took %s -- BUG: a crash-backoff entered while closing schedules a timer through m.wg, and Close's wg.Wait() then blocks for the whole backoff delay (backoffCap is 60s by default) with nothing left to retry", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close() has blocked for over 3s -- BUG: a crash-backoff entered while closing schedules a retry timer through m.wg, and Close's wg.Wait() waits it out")
	}
}

// ---------------------------------------------------------------------------
// Fix round 1. The Critical (F1) and one minor (M3) below are REGRESSIONS this
// branch's own B6 batch introduced: in both cases the fix reached past the
// window its rationale actually covered.
// ---------------------------------------------------------------------------

// TestManagerAdmissionWaitStillBoundsARequestQueuedWhileDraining is fix round
// 1's Critical (F1). handleWaiterTimeout's B6 guard was st.proc != nil, but
// the rule it expresses -- "the request is no longer waiting for a SLOT, it is
// waiting on a startup that has its own bound" -- only holds while the state
// is StateStarting. StateDraining ALSO has st.proc != nil, and there no bound
// applies at all:
//
//   - the admission-wait timer is discarded here, and scheduleAfter is
//     one-shot: nothing re-arms it, so that request never gets an
//     admission-wait answer again;
//   - startup_timeout_seconds cannot substitute, because handleStartResult
//     returns early for a draining generation (its own B6 fix #1) and
//     pollHealth returns silently on <-proc.exited for a killed one -- so
//     failPending(ErrStartTimeout) can never fire in this window either.
//
// The request therefore hangs to its caller's HTTP context instead of getting
// a bounded 503, which is exactly what admission_wait_timeout_seconds is for
// (design spec §4.1: it bounds "how long a request may queue when blocked by
// busy/pinned processes" -- a dying process is the busiest kind).
//
// Vehicle: an idle-unload drain of a child that ignores SIGTERM, so the
// SIGTERM->SIGKILL window (killGrace) is a real, controllable observation
// window rather than the microseconds a cooperative child leaves. Eviction
// produces the identical state via a second spec; the idle path needs only
// one, so nothing about the second spec's admission can be mistaken for the
// bound under test.
func TestManagerAdmissionWaitStillBoundsARequestQueuedWhileDraining(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setKillGrace(t, 3*time.Second) // the drain window this test observes
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Args = stubArgsStubborn(0) // healthy at once, then ignores SIGTERM
	spec.IdleTimeoutSeconds = 1     // the drain vehicle
	spec.AdmissionWaitTimeoutSeconds = 1
	m.Apply(Config{Specs: []Spec{spec}})

	// This request both starts the child and proves it is serving: the manager
	// only reports success once a health probe returned 2xx, which the child's
	// own HTTP server answers strictly AFTER stubchild's main() has installed
	// its SIGTERM disposition. So the drain below cannot degenerate into
	// "SIGTERM's default action killed it instantly", which is what would make
	// this test vacuous (see waitUntilChildIsServing's doc comment).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	release() // idle from here: scanIdle drains it one second later

	waitUntil(t, 4*time.Second, "spec-a is Draining with its child still alive", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateDraining && st.PID != 0
	})

	// The request under test. handleEnsure sees StateDraining (not the
	// StateRunning fast path), so it queues with a FRESH admission-wait timer
	// -- the one whose expiry must still be honoured.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer reqCancel()
	start := time.Now()
	_, lateRelease, err := m.EnsureRunning(reqCtx, "model-a")
	elapsed := time.Since(start)
	if lateRelease != nil {
		lateRelease()
	}
	if !errors.Is(err, ErrAdmissionBlocked) {
		t.Fatalf("EnsureRunning issued while spec-a was Draining = %v after %s, want ErrAdmissionBlocked at ~1s (admission_wait_timeout_seconds) -- BUG: handleWaiterTimeout's st.proc != nil guard also covers StateDraining, where NEITHER bound applies (handleStartResult returns early for a draining generation, so failPending(ErrStartTimeout) can never fire), so the request runs to its HTTP context instead of getting a bounded 503", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("EnsureRunning returned ErrAdmissionBlocked after %s, want ~1s (the configured admission_wait_timeout_seconds) -- the bound fired for some other reason than its own timer", elapsed)
	}
}
