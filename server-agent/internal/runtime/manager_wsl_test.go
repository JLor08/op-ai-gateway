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
// to, the state directory that stands in for the distro's units, and the file
// whose contents the fake prints as the RAM sampling script's answer.
type fakeWSL struct {
	log, state, sample string
}

// fakeWSLIdleSample is the sampling answer newFakeWSL starts with: no scope
// figures, and a roomy VM, so a test that does not look at RAM gets a sample
// that parses and constrains nothing it starts.
const fakeWSLIdleSample = "meminfo 67108864 67108864\n"

func newFakeWSL(t *testing.T) *fakeWSL {
	t.Helper()
	dir := t.TempDir()
	f := &fakeWSL{log: filepath.Join(dir, "calls.jsonl"), state: filepath.Join(dir, "state"), sample: filepath.Join(dir, "sample")}
	if err := os.Mkdir(f.state, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	f.writeSample(t, fakeWSLIdleSample)
	return f
}

// writeSample replaces what the fake prints for the next sampling call. It
// renames a complete file into place, so a concurrent call reads the old
// answer or the new one, never half of one.
func (f *fakeWSL) writeSample(t *testing.T, body string) {
	t.Helper()
	tmp := f.sample + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	if err := os.Rename(tmp, f.sample); err != nil {
		t.Fatalf("rename sample: %v", err)
	}
}

// runner is execWSLRunner with the fake's locations added to every helper
// call's environment: the agent builds that environment itself and passes on
// nothing else of its own, so this is the one way to point the fake at them.
func (f *fakeWSL) runner() wslRunner {
	return func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		env = append(slices.Clone(env), "FAKEWSL_LOG="+f.log, "FAKEWSL_STATE="+f.state, "FAKEWSL_SAMPLE="+f.sample)
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

// TestWSLUIDIsPrewarmedAtFirstApply: the first document starts the UID lookup
// of every allowed distro by itself, with nothing asking for a start, and a
// later start finds the answer cached.
func TestWSLUIDIsPrewarmedAtFirstApply(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	pol := wslTestPolicy()
	pol.AllowedDistros = []string{wslTestDistro, "Debian"}
	m := newWSLTestManagerWith(t, pol, f.runner())
	// A wsl spec nobody has asked for: not pinned, no request.
	m.Apply(Config{ETag: "v1", Specs: []Spec{wslTestSpec(f, "S", "m")}})

	asked := func() map[string]int {
		n := map[string]int{}
		for _, c := range f.find(t, "id", "-u") {
			n[c.Argv[1]]++
		}
		return n
	}
	waitUntil(t, 5*time.Second, "the first document's UID lookups for both distros", func() bool {
		n := asked()
		return n[wslTestDistro] == 1 && n["Debian"] == 1
	})
	if st := statusFor(m, "S"); st == nil || st.State != StateStopped || st.LastError != nil {
		t.Errorf("S = %+v, want stopped and untouched: a lookup starts nothing", st)
	}
	if launches := f.find(t, "systemd-run"); len(launches) != 0 {
		t.Errorf("a workload was launched with no request: %+v", launches)
	}

	ensureWSL(t, m, "m")
	if n := asked(); n[wslTestDistro] != 1 {
		t.Errorf("id -u in %s = %d after a start, want 1: the start must find the warmed UID", wslTestDistro, n[wslTestDistro])
	}
}

// TestWSLBurstInstanceRefusedWhileUIDUnknown: a burst instance never waits, not
// even for the UID lookup -- it is refused at once with ErrInstanceNoCapacity
// and leaves no row behind, while the lookup it needs is started, so a later
// attempt finds the UID cached and starts.
func TestWSLBurstInstanceRefusedWhileUIDUnknown(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	second := make(chan struct{})
	release := sync.OnceFunc(func() { close(second) })
	var lookups atomic.Int32
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		if slices.Contains(args, "id") {
			switch lookups.Add(1) {
			case 1: // the first document's: fails, so nothing is cached
				return nil, errors.New("exit status 1: the distro is still booting")
			case 2: // the burst ensure's: held, so the UID stays unknown meanwhile
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
	spec.Replicable = true
	spec.RAMMB = 64
	m.Apply(Config{ETag: "v1", Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "the first document's lookup", func() bool { return lookups.Load() >= 1 })

	// Every ensure is refused at once; one made after the first lookup's
	// failure has been taken starts the second lookup.
	deadline := time.Now().Add(5 * time.Second)
	for lookups.Load() < 2 {
		took, err := ensureErr(t, m, "m#1")
		if !errors.Is(err, ErrInstanceNoCapacity) {
			t.Fatalf("EnsureRunning(m#1) = %v, want ErrInstanceNoCapacity while the uid is unknown", err)
		}
		if took > 2*time.Second {
			t.Fatalf("the refusal took %s; a burst refusal must be immediate", took)
		}
		if st := instanceStatus(m, "S", 1); st != nil {
			t.Fatalf("instance-1 row = %+v, want none: a refused burst instance leaves no state behind", st)
		}
		if time.Now().After(deadline) {
			t.Fatal("no burst ensure started a UID lookup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if launches := f.find(t, "systemd-run"); len(launches) != 0 {
		t.Fatalf("a workload was launched without a UID: %+v", launches)
	}

	// Once the lookup answers, the next attempt finds the UID and starts.
	release()
	waitUntil(t, 10*time.Second, "a burst ensure to start once the uid is cached", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, rel, err := m.EnsureRunning(ctx, "m#1")
		if err != nil {
			if !errors.Is(err, ErrInstanceNoCapacity) {
				t.Fatalf("EnsureRunning(m#1) = %v", err)
			}
			return false
		}
		rel()
		return true
	})
	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateRunning {
		t.Errorf("instance-1 row = %+v, want running", st)
	}
	if n := lookups.Load(); n != 2 {
		t.Errorf("UID lookups = %d, want 2", n)
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

// TestWSLTargetsNeverReachThePIDRAMMeasurer: a wsl instance's pid is wsl.exe, a
// thin client whose few MB would count as a real measurement and end the RAM
// bootstrap with a third of the workload uncharged, and on which no VRAM is
// ever found. Its RAM comes from its scope's cgroup through the sampling call
// instead, and its pid reaches neither native measurer -- neither the beat's
// nor the admission snapshot's synchronous VRAM read.
func TestWSLTargetsNeverReachThePIDRAMMeasurer(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())

	var mu sync.Mutex
	ramPIDs, vramPIDs := map[int]bool{}, map[int]bool{}
	m.SetRAMMeasurer(func(pids []int) map[int]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[int]int, len(pids))
		for _, p := range pids {
			ramPIDs[p] = true
			out[p] = 1500
		}
		return out
	})
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[int]map[int]int, len(pids))
		for _, p := range pids {
			vramPIDs[p] = true
			out[p] = map[int]int{0: 4242}
		}
		return out
	})

	native := baseSpec("N", "n")
	native.Pinned = true
	native.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}} // so the VRAM measurer runs at all
	wsl := wslTestSpec(f, "S", "m")
	wsl.Pinned = true
	wsl.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	m.Apply(Config{ETag: "v1", Specs: []Spec{native, wsl}, Coresident: coresidentAllPairs("N", "S")})
	waitForState(t, m, "N", StateRunning)
	sStatus := waitForState(t, m, "S", StateRunning)
	unit := f.onlyLaunchedUnit(t)

	f.writeSample(t, "unit "+unit+" 8419880960 4535152640\nmeminfo 28763116 21000000\n")
	waitUntil(t, 5*time.Second, "S reports its scope's RAM", func() bool {
		st := statusFor(m, "S")
		return st != nil && st.MeasuredRAMMB == 3704
	})
	// A further admission runs buildSnapshot's synchronous VRAM read.
	ensureWSL(t, m, "n")

	nStatus := statusFor(m, "N")
	mu.Lock()
	defer mu.Unlock()
	if ramPIDs[sStatus.PID] || vramPIDs[sStatus.PID] {
		t.Errorf("the wsl.exe pid %d reached a native measurer (RAM %v, VRAM %v)", sStatus.PID, ramPIDs, vramPIDs)
	}
	if !ramPIDs[nStatus.PID] || !vramPIDs[nStatus.PID] {
		t.Errorf("the native pid %d was not measured (RAM %v, VRAM %v)", nStatus.PID, ramPIDs, vramPIDs)
	}
	if nStatus.MeasuredRAMMB != 1500 {
		t.Errorf("N MeasuredRAMMB = %d, want the PID measurer's 1500", nStatus.MeasuredRAMMB)
	}
	if got := statusFor(m, "S").MeasuredVRAM; len(got) != 0 {
		t.Errorf("S MeasuredVRAM = %v, want none: no per-PID VRAM exists for a wsl instance", got)
	}

	samples := f.find(t, "sh", "-c")
	if len(samples) == 0 {
		t.Fatal("no sampling call was made")
	}
	for _, c := range samples {
		want := []string{"-d", wslTestDistro, "-u", "ai", "--exec", "sh", "-c", wslSampleScript, "sh", unit}
		if !slices.Equal(c.Argv, want) {
			t.Errorf("sampling argv %q\nwant %q", c.Argv, want)
		}
		if _, ok := c.Env["XDG_RUNTIME_DIR"]; ok {
			t.Errorf("the sampling call carries XDG_RUNTIME_DIR, which it does not need: %v", c.Env)
		}
	}
}

// newWSLPoolOwner is a hand-driven owner with one native instance N (pid 100)
// and one wsl instance S (pid 200) running, the VRAM pids buildSnapshot hands
// its synchronous measurer recorded in *vramPIDs, and a 1024 MB WSL reserve.
func newWSLPoolOwner(vramPIDs *[]int) (o *owner, native, wsl *specState) {
	m := &Manager{}
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		*vramPIDs = append(*vramPIDs, pids...)
		return nil
	})
	nSpec := baseSpec("N", "n")
	sSpec := baseSpec("S", "m")
	sSpec.Execution = &Execution{Mode: ExecutionModeWSL, WSL: &WSLExecution{Distro: wslTestDistro}}
	native = &specState{spec: nSpec, state: StateRunning, proc: &runningProc{specID: "N", pid: 100}}
	wsl = &specState{spec: sSpec, state: StateRunning, proc: &runningProc{specID: "S", pid: 200, wslUnit: "opagent-n-S-1", wslDistro: wslTestDistro}}
	o = &owner{
		m:               m,
		cfg:             Config{Specs: []Spec{nSpec, sSpec}},
		specs:           map[string]*specState{"N": native, "S": wsl},
		wslRAMReserveMB: 1024,
	}
	return o, native, wsl
}

func runningRow(t *testing.T, snap PolicySnapshot, id string) RunningProc {
	t.Helper()
	for _, r := range snap.Running {
		if r.SpecID == id {
			return r
		}
	}
	t.Fatalf("no running row for %s", id)
	return RunningProc{}
}

// TestBuildSnapshotWSLRAMPool: the VM pool is the sample's MemAvailable plus
// what the agent's own scopes held in that same sample, minus the reserve; the
// ceiling is MemTotal minus the reserve; both are 0 (unconstrained) before the
// first sample. A wsl process is marked as such, takes its RAM from its unit's
// figure only, and its pid is not in the synchronous VRAM read.
func TestBuildSnapshotWSLRAMPool(t *testing.T) {
	var vramPIDs []int
	o, native, wsl := newWSLPoolOwner(&vramPIDs)
	o.m.SetHostInfo(HostInfo{MemAvailableMB: 40000})

	snap := o.buildSnapshot()
	if snap.WSLRAMBudgetMB != 0 || snap.WSLRAMCeilingMB != 0 {
		t.Errorf("before any sample: budget/ceiling = %d/%d, want 0/0", snap.WSLRAMBudgetMB, snap.WSLRAMCeilingMB)
	}
	if !runningRow(t, snap, "S").WSL || runningRow(t, snap, "N").WSL {
		t.Errorf("WSL flags: S=%v N=%v, want true/false", runningRow(t, snap, "S").WSL, runningRow(t, snap, "N").WSL)
	}
	if !slices.Equal(vramPIDs, []int{100}) {
		t.Errorf("synchronous VRAM read got pids %v, want only the native 100", vramPIDs)
	}

	o.applyMeasurement(cmdMeasured{
		targets: []measureTarget{
			{specID: "N", proc: native.proc, pid: 100, ready: true},
			{specID: "S", proc: wsl.proc, pid: 200, ready: true, wslUnit: "opagent-n-S-1", wslDistro: wslTestDistro},
		},
		// 200 is S's pid: a figure for it must never land on S.
		ramByPID:  map[int]int{100: 1500, 200: 99},
		ramByUnit: map[string]int{"opagent-n-S-1": 3704},
		wslVM:     &wslVMInfo{TotalMB: 28088, AvailableMB: 20507},
	})
	if native.measuredRAM != 1500 || wsl.measuredRAM != 3704 {
		t.Fatalf("measuredRAM N/S = %d/%d, want 1500/3704", native.measuredRAM, wsl.measuredRAM)
	}
	snap = o.buildSnapshot()
	if want := 20507 + 3704 - 1024; snap.WSLRAMBudgetMB != want {
		t.Errorf("WSLRAMBudgetMB = %d, want %d", snap.WSLRAMBudgetMB, want)
	}
	if want := 28088 - 1024; snap.WSLRAMCeilingMB != want {
		t.Errorf("WSLRAMCeilingMB = %d, want %d", snap.WSLRAMCeilingMB, want)
	}
	// The host pool is unchanged by the mode: every managed process's measured
	// RAM is added back, the wsl one's included (vmmem holds it on the host).
	if want := 40000 + 1500 + 3704; snap.HostRAMBudgetMB != want {
		t.Errorf("HostRAMBudgetMB = %d, want %d", snap.HostRAMBudgetMB, want)
	}
	if got := runningRow(t, snap, "S").RAMMB; got != 3704 {
		t.Errorf("S RAMMB = %d, want 3704", got)
	}

	// S exits. The sample still describes the instant it was taken, when S held
	// 3704 MB the VM has now got back: the pool must not shrink by S's share
	// until a later sample shows the VM's MemAvailable without it. Charging it
	// to nobody here is what would strand the very start S was evicted for.
	wsl.proc = nil
	wsl.measuredRAM = 0
	if got := o.buildSnapshot().WSLRAMBudgetMB; got != 20507+3704-1024 {
		t.Errorf("after S exits: WSLRAMBudgetMB = %d, want it unchanged at %d", got, 20507+3704-1024)
	}

	// A tight VM floors at 1, never at the unconstrained 0.
	o.applyMeasurement(cmdMeasured{wslVM: &wslVMInfo{TotalMB: 1000, AvailableMB: 10}})
	snap = o.buildSnapshot()
	if snap.WSLRAMBudgetMB != 1 || snap.WSLRAMCeilingMB != 1 {
		t.Errorf("tight VM: budget/ceiling = %d/%d, want 1/1", snap.WSLRAMBudgetMB, snap.WSLRAMCeilingMB)
	}
}

// TestApplyMeasurementWSLGenerationAndSilence: a scope's figure lands only on
// the generation it was taken for, a unit the sample said nothing about keeps
// its previous figure, and a cycle with no VM reading keeps the previous pool.
func TestApplyMeasurementWSLGenerationAndSilence(t *testing.T) {
	var vramPIDs []int
	o, _, wsl := newWSLPoolOwner(&vramPIDs)
	old := wsl.proc
	wsl.proc = &runningProc{specID: "S", pid: 201, wslUnit: "opagent-n-S-2", wslDistro: wslTestDistro}
	wsl.measuredRAM = 777

	o.applyMeasurement(cmdMeasured{
		targets:   []measureTarget{{specID: "S", proc: old, pid: 200, wslUnit: "opagent-n-S-1", wslDistro: wslTestDistro}},
		ramByUnit: map[string]int{"opagent-n-S-1": 5000},
		wslVM:     &wslVMInfo{TotalMB: 28088, AvailableMB: 20000},
	})
	if wsl.measuredRAM != 777 {
		t.Errorf("a superseded generation's figure landed: measuredRAM = %d, want 777", wsl.measuredRAM)
	}
	// What the superseded scope held at the sample is the VM's to give back.
	if got := o.buildSnapshot().WSLRAMBudgetMB; got != 20000+5000-1024 {
		t.Errorf("WSLRAMBudgetMB = %d, want %d", got, 20000+5000-1024)
	}

	cur := measureTarget{specID: "S", proc: wsl.proc, pid: 201, wslUnit: "opagent-n-S-2", wslDistro: wslTestDistro}
	o.applyMeasurement(cmdMeasured{targets: []measureTarget{cur}, ramByUnit: map[string]int{}})
	if wsl.measuredRAM != 777 {
		t.Errorf("a unit the sample said nothing about lost its figure: %d", wsl.measuredRAM)
	}
	if got := o.buildSnapshot().WSLRAMBudgetMB; got != 20000+5000-1024 {
		t.Errorf("a cycle without a VM reading moved the pool to %d", got)
	}
}

// TestRAMPoolAtDeathIsTheVMForWSL: the bootstrap memory-pressure classifier
// compares a wsl instance's last RAM with the VM pool it lived in, a native
// one's with the host pool.
func TestRAMPoolAtDeathIsTheVMForWSL(t *testing.T) {
	var vramPIDs []int
	o, native, wsl := newWSLPoolOwner(&vramPIDs)
	o.m.SetHostInfo(HostInfo{MemAvailableMB: 40000})
	native.measuredRAM, wsl.measuredRAM = 1500, 3704
	if got, want := o.ramPoolAtDeath(wsl.proc), 0; got != want {
		t.Errorf("wsl before any VM sample = %d, want %d (unknown)", got, want)
	}
	o.wslVM, o.wslVMHeldMB = &wslVMInfo{TotalMB: 28088, AvailableMB: 6000}, 3704
	if got, want := o.ramPoolAtDeath(wsl.proc), 6000+3704-1024; got != want {
		t.Errorf("wsl = %d, want the VM pool %d", got, want)
	}
	if got, want := o.ramPoolAtDeath(native.proc), 40000+1500+3704; got != want {
		t.Errorf("native = %d, want the host pool %d", got, want)
	}
	// 8000 MB is 92 % of the 8680 MB VM pool, but 18 % of the host pool:
	// memory pressure in the VM, which the host's figure would hide.
	if !bootstrapDiedOfMemoryPressure(false, 8000, o.ramPoolAtDeath(wsl.proc)) {
		t.Error("8000 MB of a 8680 MB VM pool must read as memory pressure")
	}
	if bootstrapDiedOfMemoryPressure(false, 8000, o.ramPoolAtDeath(native.proc)) {
		t.Error("8000 MB of the 45204 MB host pool must not")
	}
}

// TestWSLEvictionForVMRoomStartsTheCandidate: a wsl start that does not fit the
// VM pool evicts the idle wsl workload, and then starts. After the eviction no
// wsl process is left to sample, so the pool cannot be refreshed: the start
// must fit the pool the last sample described, in which the victim's share was
// already the VM's to give back. Charging the victim's share to nobody instead
// would leave the candidate waiting for a sample that never comes.
func TestWSLEvictionForVMRoomStartsTheCandidate(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	a := wslTestSpec(f, "A", "a")
	a.RAMMB = 6000
	b := wslTestSpec(f, "B", "b")
	b.RAMMB = 5000
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, Coresident: coresidentAllPairs("A", "B")})

	ensureWSL(t, m, "a")
	unitA := f.onlyLaunchedUnit(t)
	// A's scope holds 6000 MB of a VM with 4000 MB available: a pool of 10000,
	// and 6000 + 5000 does not fit it.
	f.writeSample(t, "unit "+unitA+" "+strconv.Itoa(6000<<20)+" 0\nmeminfo "+strconv.Itoa(12000<<10)+" "+strconv.Itoa(4000<<10)+"\n")
	waitUntil(t, 5*time.Second, "A's sample lands", func() bool {
		st := statusFor(m, "A")
		return st != nil && st.MeasuredRAMMB == 6000
	})

	ensureWSL(t, m, "b")
	if st := statusFor(m, "A"); st.State == StateRunning {
		t.Errorf("A is still running beside B: the VM pool was not applied")
	}
	waitForState(t, m, "B", StateRunning)
}

// TestWSLVMSampleWakesARAMWaiter: a wsl start that waits on the VM pool -- the
// only wsl holder is pinned, so nothing can be evicted -- is re-asked when the
// next VM sample lands, and starts once that sample shows the room.
func TestWSLVMSampleWakesARAMWaiter(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	a := wslTestSpec(f, "A", "a")
	a.RAMMB = 1000
	a.Pinned = true
	b := wslTestSpec(f, "B", "b")
	b.RAMMB = 5000
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, Coresident: coresidentAllPairs("A", "B")})
	waitForState(t, m, "A", StateRunning)
	unitA := f.onlyLaunchedUnit(t)
	sample := func(availMB int) string {
		return "unit " + unitA + " " + strconv.Itoa(1000<<20) + " 0\nmeminfo " + strconv.Itoa(12000<<10) + " " + strconv.Itoa(availMB<<10) + "\n"
	}
	// A pool of 2000 + 1000 = 3000: B's 5000 does not fit beside A.
	f.writeSample(t, sample(2000))
	waitUntil(t, 5*time.Second, "A's sample lands", func() bool {
		st := statusFor(m, "A")
		return st != nil && st.MeasuredRAMMB == 1000
	})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, release, err := m.EnsureRunning(ctx, "b")
		if err == nil {
			release()
		}
		done <- err
	}()
	// Several beats, each landing the same tight sample: B keeps waiting.
	time.Sleep(300 * time.Millisecond)
	if st := statusFor(m, "B"); st.State == StateRunning || st.State == StateStarting {
		t.Fatalf("B is %s with the VM pool too small for it", st.State)
	}
	select {
	case err := <-done:
		t.Fatalf("EnsureRunning(b) returned %v while it should wait", err)
	default:
	}

	f.writeSample(t, sample(8000)) // a pool of 9000 now
	if err := <-done; err != nil {
		t.Fatalf("EnsureRunning(b) = %v, want the next VM sample to start it", err)
	}
	if st := statusFor(m, "A"); st.State != StateRunning {
		t.Errorf("A is %s, want it untouched (pinned)", st.State)
	}
}

// wslSampleCalls is the number of sampling calls the fake has logged.
func (f *fakeWSL) wslSampleCalls(t *testing.T) []fakeWSLCall {
	t.Helper()
	return f.find(t, "sh", "-c")
}

// stopWSLAfterSample starts the wsl spec model, has its scope report heldMB of
// a VM with availMB available, waits for that sample to land, and stops it
// again: the VM pool is then frozen at availMB + heldMB, with no wsl process
// left to refresh it.
func stopWSLAfterSample(t *testing.T, m *Manager, f *fakeWSL, specID, model string, heldMB, availMB int) {
	t.Helper()
	ensureWSL(t, m, model)
	unit := f.onlyLaunchedUnit(t)
	f.writeSample(t, "unit "+unit+" "+strconv.Itoa(heldMB<<20)+" 0\nmeminfo "+strconv.Itoa(12000<<10)+" "+strconv.Itoa(availMB<<10)+"\n")
	waitUntil(t, 5*time.Second, specID+"'s sample lands", func() bool {
		st := statusFor(m, specID)
		return st != nil && st.MeasuredRAMMB == heldMB
	})
	if _, err := m.Stop(context.Background(), model); err != nil {
		t.Fatalf("Stop(%s): %v", model, err)
	}
	waitForState(t, m, specID, StateStopped)
}

// TestWSLStaleVMSampleIsRefreshedForAWaitingStart: the last sample was taken
// while something outside the agent's scopes held most of the VM (500 MB
// available beside A's 1000), and A has since stopped, so no beat samples the
// VM any more. That consumer has left too. A wsl start that does not fit the
// stale pool must not wait on it for good: with no wsl process live, its wait
// on the VM pool asks for a VM-only sample (the script with no units), and the
// fresh figure lets it start.
//
// Only a VM-only call is answered with the fresh figure; a call that carries
// units -- one dispatched while A was live may still be out after the stop --
// keeps answering the stale one. So B can start through a VM-only sample and
// nothing else.
func TestWSLStaleVMSampleIsRefreshedForAWaitingStart(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	fresh := filepath.Join(t.TempDir(), "fresh")
	if err := os.WriteFile(fresh, []byte("meminfo "+strconv.Itoa(12000<<10)+" "+strconv.Itoa(9000<<10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, exe string, args, env []string) ([]byte, error) {
		sample := f.sample
		if n := len(args); n > 0 && args[n-1] == "sh" && slices.Contains(args, wslSampleScript) {
			sample = fresh
		}
		env = append(slices.Clone(env), "FAKEWSL_LOG="+f.log, "FAKEWSL_STATE="+f.state, "FAKEWSL_SAMPLE="+sample)
		return execWSLRunner(ctx, exe, args, env)
	}
	m := newWSLTestManager(t, run)
	a := wslTestSpec(f, "A", "a")
	a.RAMMB = 1000
	b := wslTestSpec(f, "B", "b")
	b.RAMMB = 5000
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, Coresident: coresidentAllPairs("A", "B")})
	stopWSLAfterSample(t, m, f, "A", "a", 1000, 500) // a pool of 1500 MB

	before := len(f.wslSampleCalls(t))
	ensureWSL(t, m, "b")
	waitForState(t, m, "B", StateRunning)

	// A sample dispatched while A was still live may be logged after the
	// stop, so the refresh is looked for, not assumed to be the next call.
	want := []string{"-d", wslTestDistro, "-u", "ai", "--exec", "sh", "-c", wslSampleScript, "sh"}
	if !slices.ContainsFunc(f.wslSampleCalls(t)[before:], func(c fakeWSLCall) bool { return slices.Equal(c.Argv, want) }) {
		t.Errorf("no VM-only sampling call (%q) after the start began to wait: %+v", want, f.wslSampleCalls(t)[before:])
	}
}

// TestWSLNoVMSampleWhileNoWSLStartWaits: with no wsl process live and no wsl
// start waiting, nothing samples the VM -- a sample would start an idle VM --
// even while the beat keeps measuring a live native process.
func TestWSLNoVMSampleWhileNoWSLStartWaits(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	var beats atomic.Int32
	m.SetRAMMeasurer(func(pids []int) map[int]int {
		beats.Add(1)
		return nil
	})
	native := baseSpec("N", "n")
	native.Pinned = true
	a := wslTestSpec(f, "A", "a")
	a.RAMMB = 1000
	m.Apply(Config{ETag: "v1", Specs: []Spec{native, a}, Coresident: coresidentAllPairs("N", "A")})
	waitForState(t, m, "N", StateRunning)
	stopWSLAfterSample(t, m, f, "A", "a", 1000, 500)
	// Two native beats after the stop, any measurement dispatched while A was
	// live has finished (one is outstanding at a time), its sample logged.
	settled := beats.Load()
	waitUntil(t, 5*time.Second, "two native measurement beats", func() bool { return beats.Load() >= settled+2 })

	before := len(f.wslSampleCalls(t))
	start := beats.Load()
	waitUntil(t, 5*time.Second, "ten native measurement beats", func() bool { return beats.Load() >= start+10 })
	if got := len(f.wslSampleCalls(t)); got != before {
		t.Errorf("sampling calls went from %d to %d with nothing wsl live or waiting", before, got)
	}
}

// TestWSLVMWaitAsksForASampleAtOnce: the first verdict that makes a wsl start
// wait on the VM pool, with no wsl process live, dispatches a VM-only sample at
// once -- the owner here has no beat at all -- and that sample's answer travels
// back as the ordinary measurement. A second verdict of the same wait asks for
// nothing more (the beat refreshes it from then on), and a start that waits on
// the VM pool beside a live wsl process asks for nothing either.
func TestWSLVMWaitAsksForASampleAtOnce(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	run := func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, slices.Clone(args))
		mu.Unlock()
		return []byte("meminfo " + strconv.Itoa(12000<<10) + " " + strconv.Itoa(9000<<10) + "\n"), nil
	}
	pol := allowlistPolicy()
	pol.WSL = wslTestPolicy()
	b := wslCandidate("B", 5000)
	b.Binary, b.UpstreamModel, b.Pinned = stubchildPath, "b", true
	m := &Manager{cmds: make(chan command, 4), done: make(chan struct{})}
	o := &owner{
		m:         m,
		policy:    pol,
		cfg:       Config{Specs: []Spec{b}},
		specs:     map[string]*specState{"B": {spec: b, state: StateStopped}},
		wslRunner: run,
		getenv:    func(string) string { return "" },
		wslUIDs:   map[string]int{wslTestDistro: 1000},
		// A stale pool of 500 MB, with nothing wsl live to refresh it.
		wslVM: &wslVMInfo{TotalMB: 12000, AvailableMB: 500},
	}
	st := o.specs["B"]

	o.admitAndStart("B")
	if !st.waitingOnWSLVM || !st.waitingOnRAM || st.proc != nil {
		t.Fatalf("B: waitingOnWSLVM=%v waitingOnRAM=%v proc=%v, want a wait on the VM pool", st.waitingOnWSLVM, st.waitingOnRAM, st.proc)
	}
	var c cmdMeasured
	select {
	case cmd := <-m.cmds:
		c = cmd.(cmdMeasured)
	case <-time.After(5 * time.Second):
		t.Fatal("the wait on the VM pool dispatched no sample")
	}
	m.wg.Wait()
	want := []string{"-d", wslTestDistro, "-u", "ai", "--exec", "sh", "-c", wslSampleScript, "sh"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("sampling calls %q, want one VM-only call %q", calls, want)
	}
	if c.wslVM == nil || c.wslVM.AvailableMB != 9000 || len(c.targets) != 0 {
		t.Errorf("measurement = %+v, want the VM-only answer with no targets", c)
	}

	// The same wait, asked again while that measurement is still unapplied:
	// no second sample, now or deferred to the outstanding one's return.
	o.admitAndStart("B")
	if !st.waitingOnWSLVM {
		t.Fatal("B no longer waits on the VM pool")
	}
	if o.remeasure {
		t.Error("a repeated verdict of the same wait asked for another measurement")
	}

	// Beside a live wsl process the beat samples the VM anyway: a new wait
	// asks for nothing.
	o.applyMeasurement(cmdMeasured{}) // clears measuring, without a VM reading
	st.waitingOnWSLVM = false
	a := wslCandidate("A", 1000)
	o.specs["A"] = &specState{spec: a, state: StateRunning, inFlight: 1, proc: &runningProc{specID: "A", pid: 300, wslUnit: "opagent-n-A-1", wslDistro: wslTestDistro}}
	o.allowedPairs = allowPairs("B", "A")
	o.wslVM = &wslVMInfo{TotalMB: 12000, AvailableMB: 500}
	o.admitAndStart("B")
	if !st.waitingOnWSLVM {
		t.Fatal("B no longer waits on the VM pool beside A")
	}
	if o.measuring {
		t.Error("a wait beside a live wsl process dispatched a measurement of its own")
	}
	close(m.done)
	m.wg.Wait()
}
