// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tests for the WSL lifecycle (#381): a wsl spec launched through wsl.exe under
// a systemd user scope, stopped by signalling that scope from inside the
// distro, killed with wsl.exe as the backstop, its wsl.exe-level failure
// reported as such, and the units an earlier agent run left behind swept at
// start. The process tests launch through testdata/fakewsl (fakewslPath),
// which records every invocation and plays the distro's systemd user manager.

const wslTestDistro = "Ubuntu-24.04"

// fakeWSL is one test's fakewsl world: the log every invocation appends a line
// to, and the state directory that stands in for the distro's units.
type fakeWSL struct {
	log, state string
}

func newFakeWSL(t *testing.T) *fakeWSL {
	t.Helper()
	dir := t.TempDir()
	f := &fakeWSL{log: filepath.Join(dir, "calls.jsonl"), state: filepath.Join(dir, "state")}
	if err := os.Mkdir(f.state, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	return f
}

// runner is execWSLRunner with the fake's two locations added to every helper
// call's environment: the agent builds that environment itself and passes on
// nothing else of its own, so this is the one way to point the fake at them.
func (f *fakeWSL) runner() wslRunner {
	return func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		env = append(slices.Clone(env), "FAKEWSL_LOG="+f.log, "FAKEWSL_STATE="+f.state)
		return execWSLRunner(ctx, exe, args, env)
	}
}

// fakeWSLCall is one logged invocation.
type fakeWSLCall struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// command is the part of the argv after --exec: what ran inside the distro.
func (c fakeWSLCall) command() []string {
	if i := slices.Index(c.Argv, "--exec"); i >= 0 {
		return c.Argv[i+1:]
	}
	return nil
}

// unit is the --unit= value of a systemd-run call.
func (c fakeWSLCall) unit() string {
	for _, a := range c.command() {
		if u, ok := strings.CutPrefix(a, "--unit="); ok {
			return u
		}
	}
	return ""
}

func (f *fakeWSL) calls(t *testing.T) []fakeWSLCall {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read fakewsl log: %v", err)
	}
	// Only complete lines: a concurrent invocation may be mid-write.
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	} else {
		return nil
	}
	var out []fakeWSLCall
	for _, line := range bytes.Split(b, []byte("\n")) {
		var c fakeWSLCall
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("fakewsl log line %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// find returns the logged calls whose distro-side command starts with prefix.
func (f *fakeWSL) find(t *testing.T, prefix ...string) []fakeWSLCall {
	t.Helper()
	var out []fakeWSLCall
	for _, c := range f.calls(t) {
		if cmd := c.command(); len(cmd) >= len(prefix) && slices.Equal(cmd[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

// onlyLaunchedUnit is the unit of the one workload launch the log holds.
func (f *fakeWSL) onlyLaunchedUnit(t *testing.T) string {
	t.Helper()
	launches := f.find(t, "systemd-run")
	if len(launches) != 1 {
		t.Fatalf("workload launches = %d, want 1: %+v", len(launches), launches)
	}
	return launches[0].unit()
}

// unitPID is the pid the fake recorded for unit.
func (f *fakeWSL) unitPID(t *testing.T, unit string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, unit))
	if err != nil {
		t.Fatalf("unit %s was never recorded: %v", unit, err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatalf("unit %s pid %q: %v", unit, b, err)
	}
	return pid
}

// processAlive reports whether pid names a live process (signal 0).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func wslTestPolicy() WSLPolicy {
	return WSLPolicy{
		Exe:             fakewslPath,
		AllowedDistros:  []string{wslTestDistro},
		AllowedBinaries: []string{stubchildPath},
		User:            "ai",
	}
}

// wslTestSpec is baseSpec run in the test distro. Its env carries the fake's
// locations, which the launch forwards like any other spec variable.
func wslTestSpec(f *fakeWSL, id, model string) Spec {
	s := baseSpec(id, model)
	s.Execution = &Execution{Mode: ExecutionModeWSL, WSL: &WSLExecution{Distro: wslTestDistro}}
	s.Env = map[string]string{"FAKEWSL_LOG": f.log, "FAKEWSL_STATE": f.state}
	return s
}

func newWSLTestManager(t *testing.T, run wslRunner) *Manager {
	t.Helper()
	return newWSLTestManagerWith(t, wslTestPolicy(), run)
}

// newWSLTestManagerWith is newWSLTestManager with its own WSL policy. The
// native allowlist admits stubchild too, so a test can run a native spec
// beside the wsl one.
func newWSLTestManagerWith(t *testing.T, pol WSLPolicy, run wslRunner) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), WSL: pol, WSLRunner: run, Getenv: func(string) string { return "" }})
	t.Cleanup(m.Close)
	return m
}

// ensureWSL starts model and releases the request at once.
func ensureWSL(t *testing.T, m *Manager, model string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint, release, err := m.EnsureRunning(ctx, model)
	if err != nil {
		t.Fatalf("EnsureRunning(%s): %v", model, err)
	}
	release()
	return endpoint
}

func waitForState(t *testing.T, m *Manager, specID string, want State) *Status {
	t.Helper()
	var st *Status
	waitUntil(t, 10*time.Second, specID+" reaches "+string(want), func() bool {
		st = statusFor(m, specID)
		return st != nil && st.State == want
	})
	return st
}

// assertHelperCall checks what every agent-owned helper call into the distro
// must look like: the configured user (never root), and the user manager's bus
// as the only variable carried across.
func assertHelperCall(t *testing.T, c fakeWSLCall) {
	t.Helper()
	if want := []string{"-d", wslTestDistro, "-u", "ai", "--exec"}; !slices.Equal(c.Argv[:5], want) {
		t.Errorf("helper call argv %q, want it to open with %q", c.Argv, want)
	}
	if got := c.Env["XDG_RUNTIME_DIR"]; got != "/run/user/1000" {
		t.Errorf("helper call XDG_RUNTIME_DIR = %q, want /run/user/1000", got)
	}
	if got := c.Env["WSLENV"]; got != "XDG_RUNTIME_DIR/u" {
		t.Errorf("helper call WSLENV = %q, want XDG_RUNTIME_DIR/u", got)
	}
}

func TestWSLSpecStartsAndServes(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.Apply(Config{ETag: "v1", Specs: []Spec{wslTestSpec(f, "S", "m")}})

	endpoint := ensureWSL(t, m, "m")
	if code, body := httpEcho(t, endpoint, "through the relay"); code != 200 || body != "through the relay" {
		t.Fatalf("echo = %d %q, want 200 and the body back", code, body)
	}
	waitForState(t, m, "S", StateRunning)

	launches := f.find(t, "systemd-run")
	if len(launches) != 1 {
		t.Fatalf("workload launches = %d, want 1", len(launches))
	}
	c := launches[0]
	if want := []string{"-d", wslTestDistro, "-u", "ai"}; !slices.Equal(c.Argv[:4], want) {
		t.Errorf("launch argv %q, want it to open with %q", c.Argv, want)
	}
	if !regexp.MustCompile(`^opagent-[0-9a-f]{8}-S-[0-9]+$`).MatchString(c.unit()) {
		t.Errorf("launch unit %q, want opagent-<8 hex nonce>-S-<generation>", c.unit())
	}
	if !slices.Contains(strings.Split(c.Env["WSLENV"], ":"), "XDG_RUNTIME_DIR/u") {
		t.Errorf("launch WSLENV %q does not forward XDG_RUNTIME_DIR", c.Env["WSLENV"])
	}
	if got := c.Env["XDG_RUNTIME_DIR"]; got != "/run/user/1000" {
		t.Errorf("launch XDG_RUNTIME_DIR = %q, want /run/user/1000 (the uid id -u printed)", got)
	}

	// The UID was asked of the distro once, as the configured user: the
	// first document's lookup, which the start waited for and the orphan sweep
	// reused.
	ids := f.find(t, "id", "-u")
	if len(ids) != 1 {
		t.Fatalf("id -u calls = %d, want 1", len(ids))
	}
	for _, c := range ids {
		if want := []string{"-d", wslTestDistro, "-u", "ai", "--exec"}; !slices.Equal(c.Argv[:5], want) {
			t.Errorf("id -u argv %q, want it to open with %q", c.Argv, want)
		}
		if _, ok := c.Env["XDG_RUNTIME_DIR"]; ok {
			t.Errorf("id -u carried XDG_RUNTIME_DIR %q before the UID was known", c.Env["XDG_RUNTIME_DIR"])
		}
	}

	// What the log view reports is what ran: wsl.exe, the full wrapper argv,
	// and the directory inside the distro.
	marker := waitForCommand(t, m, "S", "a started marker", func(e LogEntry) bool {
		return e.Command != nil && e.Event == logEventStarted
	})
	if marker.Command.Binary != fakewslPath {
		t.Errorf("reported binary %q, want wsl.exe (%q)", marker.Command.Binary, fakewslPath)
	}
	if !slices.Equal(marker.Command.Args, c.Argv) {
		t.Errorf("reported args %q, want the argv wsl.exe received %q", marker.Command.Args, c.Argv)
	}
	if want := path.Dir(stubchildPath); marker.Command.WorkDir != want {
		t.Errorf("reported work dir %q, want the directory inside the distro %q", marker.Command.WorkDir, want)
	}
}

func TestWSLSpecGracefulStopSignalsScope(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	// A kill grace no test waits out: the stop must end the child through the
	// scope's SIGTERM alone. shrinkTimings restores it.
	killGrace = 30 * time.Second
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.Apply(Config{ETag: "v1", Specs: []Spec{wslTestSpec(f, "S", "m")}})
	ensureWSL(t, m, "m")
	unit := f.onlyLaunchedUnit(t)

	if _, err := m.Stop(context.Background(), "m"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := waitForState(t, m, "S", StateStopped)
	if st.LastError != nil {
		t.Errorf("a graceful stop left last_error %+v, want none", st.LastError)
	}

	terms := f.find(t, "systemctl", "--user", "kill", "--signal=SIGTERM", unit+".scope")
	if len(terms) != 1 {
		t.Fatalf("SIGTERM calls for %s = %d, want 1; log: %+v", unit, len(terms), f.calls(t))
	}
	assertHelperCall(t, terms[0])
	if kills := f.find(t, "systemctl", "--user", "kill", "--signal=SIGKILL"); len(kills) != 0 {
		t.Errorf("a child that honours SIGTERM was SIGKILLed: %+v", kills)
	}
}

func TestWSLSpecKillEscalation(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	spec := wslTestSpec(f, "S", "m")
	spec.Args = append(spec.Args, "-ignore-sigterm")
	m.Apply(Config{ETag: "v1", Specs: []Spec{spec}})
	ensureWSL(t, m, "m")
	unit := f.onlyLaunchedUnit(t)
	pid := f.unitPID(t, unit)

	if _, err := m.Stop(context.Background(), "m"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, m, "S", StateStopped)

	if n := len(f.find(t, "systemctl", "--user", "kill", "--signal=SIGTERM", unit+".scope")); n != 1 {
		t.Errorf("SIGTERM calls = %d, want 1", n)
	}
	kills := f.find(t, "systemctl", "--user", "kill", "--signal=SIGKILL", unit+".scope")
	if len(kills) != 1 {
		t.Fatalf("SIGKILL calls for %s = %d, want 1 after the kill grace; log: %+v", unit, len(kills), f.calls(t))
	}
	assertHelperCall(t, kills[0])
	if processAlive(pid) {
		t.Errorf("the workload (pid %d) is still alive after the escalation", pid)
	}
}

// TestWSLKillFallsBackToWSLExe: when the scope cannot be signalled at all (no
// user bus, say), the kill grace still ends the generation, by killing wsl.exe.
func TestWSLKillFallsBackToWSLExe(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if slices.Contains(args, "kill") {
			return nil, errors.New("exit status 1: Failed to connect to bus")
		}
		return f.runner()(ctx, exe, args, env)
	}
	m := newWSLTestManager(t, run)
	spec := wslTestSpec(f, "S", "m")
	spec.Args = append(spec.Args, "-ignore-sigterm")
	m.Apply(Config{ETag: "v1", Specs: []Spec{spec}})
	ensureWSL(t, m, "m")
	pid := f.unitPID(t, f.onlyLaunchedUnit(t))

	if _, err := m.Stop(context.Background(), "m"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, m, "S", StateStopped)
	if processAlive(pid) {
		t.Errorf("the workload (pid %d) outlived the backstop", pid)
	}
}

func TestWSLOrphanSweepKillsOnlyForeignNonce(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)

	// A unit an earlier agent run left behind, its process still alive.
	const foreign = "opagent-deadbeef-x-1"
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	gone := make(chan struct{})
	go func() {
		_ = sleeper.Wait()
		close(gone)
	}()
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		<-gone
	})
	if err := os.WriteFile(filepath.Join(f.state, foreign), []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatalf("record foreign unit: %v", err)
	}

	// The listing waits until this run's own unit is recorded, so the sweep
	// sees both and has to tell them apart: a spec starting while the sweep
	// runs is exactly the case that must never be touched.
	ownRecorded := func() bool {
		entries, _ := os.ReadDir(f.state)
		for _, e := range entries {
			if e.Name() != foreign {
				return true
			}
		}
		return false
	}
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if slices.Contains(args, "list-units") {
			deadline := time.Now().Add(5 * time.Second)
			for !ownRecorded() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		return f.runner()(ctx, exe, args, env)
	}
	m := newWSLTestManager(t, run)
	spec := wslTestSpec(f, "S", "m")
	spec.Pinned = true
	m.Apply(Config{ETag: "v1", Specs: []Spec{spec}})

	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatalf("the foreign unit's process survived the sweep; log: %+v", f.calls(t))
	}
	waitForState(t, m, "S", StateRunning)
	own := f.onlyLaunchedUnit(t)

	lists := f.find(t, "systemctl", "--user", "list-units")
	if len(lists) != 1 {
		t.Fatalf("list-units calls = %d, want 1 (the sweep runs once)", len(lists))
	}
	assertHelperCall(t, lists[0])
	kills := f.find(t, "systemctl", "--user", "kill")
	if len(kills) != 1 || !slices.Equal(kills[0].command(), []string{"systemctl", "--user", "kill", "--signal=SIGKILL", foreign + ".scope"}) {
		t.Fatalf("sweep kills = %+v, want exactly one SIGKILL of %s.scope", kills, foreign)
	}
	assertHelperCall(t, kills[0])
	if !processAlive(f.unitPID(t, own)) {
		t.Errorf("the sweep killed this run's own unit %s", own)
	}
}

// TestWSLUIDLookupFailureIsStartFailure: a lookup that fails, answers no
// number, or answers UID 0 fails the waiting start with a message that names
// the setting but neither the distro nor the user (the detail is the agent
// log's), and nothing is launched.
func TestWSLUIDLookupFailureIsStartFailure(t *testing.T) {
	skipOnWindows(t)
	const user = "modelsvc"
	for name, answer := range map[string]func() ([]byte, error){
		"lookup fails":  func() ([]byte, error) { return nil, errors.New("exit status 1: id: '" + user + "': no such user") },
		"not a number":  func() ([]byte, error) { return []byte(user + "\n"), nil },
		"user is uid 0": func() ([]byte, error) { return []byte("0\n"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			shrinkTimings(t)
			f := newFakeWSL(t)
			var lookups atomic.Int32
			run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
				if slices.Contains(args, "id") {
					lookups.Add(1)
					return answer()
				}
				return f.runner()(ctx, exe, args, env)
			}
			pol := wslTestPolicy()
			pol.User = user
			m := newWSLTestManagerWith(t, pol, run)
			m.Apply(Config{ETag: "v1", Specs: []Spec{wslTestSpec(f, "S", "m")}})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, _, err := m.EnsureRunning(ctx, "m"); !errors.Is(err, ErrStartFailed) {
				t.Fatalf("EnsureRunning err = %v, want ErrStartFailed", err)
			}
			st := statusFor(m, "S")
			if st == nil || st.LastError == nil || st.LastError.Message != wslUIDFailureMessage {
				t.Fatalf("status %+v, want last_error %q", st, wslUIDFailureMessage)
			}
			for _, leak := range []string{user, wslTestDistro, "Ubuntu"} {
				if strings.Contains(st.LastError.Message, leak) {
					t.Errorf("last_error %q names %q", st.LastError.Message, leak)
				}
			}
			if lookups.Load() == 0 {
				t.Error("no UID lookup ran")
			}
			if launches := f.find(t, "systemd-run"); len(launches) != 0 {
				t.Errorf("the workload was launched without a UID: %+v", launches)
			}
		})
	}
}

// TestWSLUIDLookupRunsOffTheOwner: while the UID lookup hangs, the owner keeps
// answering -- Status, and a native spec's whole start -- and the wsl instance
// waits without an error; it is started once the answer is posted back.
func TestWSLUIDLookupRunsOffTheOwner(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	answer := make(chan struct{})
	release := sync.OnceFunc(func() { close(answer) })
	var lookups atomic.Int32
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if slices.Contains(args, "id") {
			lookups.Add(1)
			select {
			case <-answer:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.runner()(ctx, exe, args, env)
	}
	m := newWSLTestManager(t, run)
	t.Cleanup(release) // registered after the manager, so it runs before Close
	wsl := wslTestSpec(f, "S", "m")
	wsl.Pinned = true
	// Each owner round trip must come back while the lookup still hangs.
	promptly := func(what string, call func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			call()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked while the UID lookup was running", what)
		}
	}
	// The pinned wsl spec is admitted in this very Apply.
	promptly("Apply", func() { m.Apply(Config{ETag: "v1", Specs: []Spec{wsl, baseSpec("N", "n")}}) })
	waitUntil(t, 5*time.Second, "the UID lookup to start", func() bool { return lookups.Load() > 0 })
	promptly("Status", func() { m.Status() })
	ensureWSL(t, m, "n") // a native start goes through the owner untouched
	waitForState(t, m, "N", StateRunning)

	st := statusFor(m, "S")
	if st == nil || st.State != StateStopped || st.LastError != nil {
		t.Fatalf("waiting wsl instance = %+v, want stopped with no last_error", st)
	}
	if launches := f.find(t, "systemd-run"); len(launches) != 0 {
		t.Fatalf("the workload was launched before its UID was known: %+v", launches)
	}

	release()
	waitForState(t, m, "S", StateRunning)
	if n := lookups.Load(); n != 1 {
		t.Errorf("UID lookups = %d, want 1 (the waiting start reuses the first document's)", n)
	}
	// The lookup the start waited for ran before the launch, in the log's order.
	calls := f.calls(t)
	idAt := slices.IndexFunc(calls, func(c fakeWSLCall) bool { return slices.Equal(c.command(), []string{"id", "-u"}) })
	launchAt := slices.IndexFunc(calls, func(c fakeWSLCall) bool { return c.unit() != "" })
	if idAt < 0 || launchAt < 0 || idAt > launchAt {
		t.Errorf("id -u at %d, launch at %d: want the lookup first", idAt, launchAt)
	}
}

// TestWSLUIDLookupFailureIsRetried: a failed lookup is not cached. The start
// that waited for it fails into the ordinary backoff, whose retry asks the
// distro again, and the instance starts once that answer comes.
func TestWSLUIDLookupFailureIsRetried(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	second := make(chan struct{})
	release := sync.OnceFunc(func() { close(second) })
	var lookups atomic.Int32
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if slices.Contains(args, "id") {
			switch lookups.Add(1) {
			case 1:
				return nil, errors.New("exit status 1: the distro is still booting")
			case 2:
				select {
				case <-second:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return f.runner()(ctx, exe, args, env)
	}
	m := newWSLTestManager(t, run)
	t.Cleanup(release) // registered after the manager, so it runs before Close
	spec := wslTestSpec(f, "S", "m")
	spec.Pinned = true
	m.Apply(Config{ETag: "v1", Specs: []Spec{spec}})

	waitUntil(t, 10*time.Second, "the backoff retry to ask the distro again", func() bool { return lookups.Load() >= 2 })
	st := statusFor(m, "S")
	if st == nil || st.LastError == nil || st.LastError.Message != wslUIDFailureMessage {
		t.Fatalf("status %+v, want the first lookup's failure, %q", st, wslUIDFailureMessage)
	}
	if launches := f.find(t, "systemd-run"); len(launches) != 0 {
		t.Fatalf("the workload was launched without a UID: %+v", launches)
	}

	release()
	waitForState(t, m, "S", StateRunning)
	if n := lookups.Load(); n != 2 {
		t.Errorf("UID lookups = %d, want 2 (the failed one and its retry)", n)
	}
}

// TestWSLUIDIsLookedUpOncePerDistro: an answer is kept for the manager's
// lifetime, so a second start of the instance asks nothing.
func TestWSLUIDIsLookedUpOncePerDistro(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.Apply(Config{ETag: "v1", Specs: []Spec{wslTestSpec(f, "S", "m")}})
	ensureWSL(t, m, "m")
	if _, err := m.Stop(context.Background(), "m"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, m, "S", StateStopped)
	ensureWSL(t, m, "m")

	if n := len(f.find(t, "systemd-run")); n != 2 {
		t.Fatalf("workload launches = %d, want 2", n)
	}
	if n := len(f.find(t, "id", "-u")); n != 1 {
		t.Errorf("id -u calls = %d, want 1 for two starts", n)
	}
}

func TestWSLLaunchFailedExitCode(t *testing.T) {
	u32max := int64(4294967295) // a variable: the constant does not fit a 32-bit int
	for _, code := range []int{-1, int(u32max)} {
		if !wslLaunchFailed(code) {
			t.Errorf("wslLaunchFailed(%d) = false, want true", code)
		}
		if msg := wslExitMessage(code); !strings.Contains(msg, "wsl.exe could not run the workload") {
			t.Errorf("wslExitMessage(%d) = %q", code, msg)
		}
	}
	for _, code := range []int{0, 1, 9, 15} {
		if wslLaunchFailed(code) {
			t.Errorf("wslLaunchFailed(%d) = true, want false: a workload's own exit or signal", code)
		}
	}

	// The wiring in onProcExited, on an owner driven by hand: a unix child
	// cannot exit -1, but a wait error that is not an exit status reads as -1.
	for name, tc := range map[string]struct {
		unit string
		want string
	}{
		"wsl":    {unit: "opagent-cafe0001-S-1", want: "wsl.exe could not run the workload (exit -1); see the runtime log"},
		"native": {unit: "", want: "runtime: process exited before becoming healthy (exit code -1)"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec("S", "m")
			spec.RAMMB = 1000
			logs := NewLogStore(0, 0)
			o := &owner{m: &Manager{}, cfg: Config{Specs: []Spec{spec}}, specs: map[string]*specState{}}
			st := &specState{spec: spec, state: StateStarting, proc: &runningProc{
				specID: spec.ID, pid: 4242, log: logs.newProc(spec.ID), exited: make(chan struct{}),
				wslUnit: tc.unit, wslDistro: wslTestDistro,
			}}
			o.specs[spec.ID] = st

			o.onProcExited(st, errors.New("wait: the relay vanished"))
			cancelTimer(o.m, &st.backoffTimer)

			if st.lastError == nil || st.lastError.Message != tc.want {
				t.Errorf("last_error %+v, want message %q", st.lastError, tc.want)
			}
		})
	}
}

func TestWSLOrphanUnits(t *testing.T) {
	listing := strings.Join([]string{
		"opagent-deadbeef-S-1.scope loaded active running S",
		"opagent-cafe0001-S-3.scope loaded active running this run's own",
		"● opagent-0badf00d-T_1-2.scope loaded active running marked",
		"opagent-deadbeef-U-4.scope loaded failed failed nothing left to kill",
		"opagent-deadbeef-V-5.scope loaded inactive dead nothing left to kill",
		"user.slice loaded active active not ours",
		"opagent-dead;beef-W-6.scope loaded active running not a name the agent makes",
		"opagent-deadbeef.scope loaded active running no nonce segment end",
		"",
	}, "\n")
	got := wslOrphanUnits([]byte(listing), "cafe0001")
	want := []string{"opagent-deadbeef-S-1.scope", "opagent-0badf00d-T_1-2.scope", "opagent-deadbeef.scope"}
	if !slices.Equal(got, want) {
		t.Errorf("wslOrphanUnits = %q, want %q", got, want)
	}
}

func TestExecWSLRunnerReportsStderr(t *testing.T) {
	skipOnWindows(t)
	out, err := execWSLRunner(context.Background(), fakewslPath, []string{"-d", wslTestDistro, "-u", "ai", "--exec", "id", "-u"}, nil)
	if err != nil || strings.TrimSpace(string(out)) != "1000" {
		t.Fatalf("id -u = %q, %v, want 1000", out, err)
	}
	_, err = execWSLRunner(context.Background(), fakewslPath, []string{"-d", wslTestDistro, "-u", "ai", "--exec", "frobnicate"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported command") {
		t.Fatalf("a failing call returned %v, want an error carrying its stderr", err)
	}
}
