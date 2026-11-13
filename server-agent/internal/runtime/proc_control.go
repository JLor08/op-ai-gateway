// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// procControl is how the owner signals one generation's process: terminate is
// the graceful stop (terminateNow), kill the escalation once the kill grace has
// run out (handleKillGraceExpired). Every runningProc carries the control its
// launch calls for, so neither call site has to ask what kind of process it
// holds, and the generation-identity guards around both stay exactly as they
// are.
type procControl interface {
	terminate() error
	kill() error
}

// nativeControl signals the process group the agent exec'd itself, on the
// owner goroutine: exactly what terminateNow and handleKillGraceExpired did
// before a second kind of process existed.
type nativeControl struct{ cmd *exec.Cmd }

func (c nativeControl) terminate() error { return terminateGroup(c.cmd) }
func (c nativeControl) kill() error      { return killGroup(c.cmd) }

// wslHelperTimeout bounds every agent-owned helper call into a distro (the UID
// lookup, a scope signal, the orphan sweep's listing and kills). Each is one
// wsl.exe process, which can take seconds when the call has to start the VM,
// and none may hang a stop or the manager's Close.
const wslHelperTimeout = 10 * time.Second

// wslHelperDetailMax bounds the stderr text a failed helper call carries into
// its error, which ends up in a log line or a last_error.
const wslHelperDetailMax = 512

// wslRunner runs one helper call: wsl.exe with args and env, returning its
// stdout. A non-zero exit is an error carrying the call's own error text. A
// field (ManagerOptions.WSLRunner) so a test can see or fail individual calls.
type wslRunner func(ctx context.Context, exe string, args, env []string) ([]byte, error)

// execWSLRunner is the production wslRunner.
func execWSLRunner(ctx context.Context, exe string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is the operator's runtime_wsl_exe; args are built by the agent
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A wsl.exe killed at the deadline can leave its relay holding the pipes;
	// the call must not wait on them past the deadline it was given.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			// wsl.exe prints its own errors to stdout, not stderr.
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return stdout.Bytes(), err
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, clipText(detail, wslHelperDetailMax))
	}
	return stdout.Bytes(), nil
}

// clipText cuts s to at most n bytes without splitting a rune.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

// wslUserRefused reports whether user may not run anything in a distro: empty,
// root by name, or root by UID ("0", "00", ...), which wsl.exe -u accepts as
// well. The one rule permitWSL and every helper call share, so no
// gateway-chosen argv or env and no agent call ever reaches a root process.
func wslUserRefused(user string) bool {
	return user == "" || user == "root" || isZeroUID(user)
}

// wslHelperArgs is the wsl.exe argv of a helper call: the distro, always the
// configured user, and the command through --exec, never a shell.
func wslHelperArgs(user, distro string, command ...string) []string {
	return append([]string{"-d", distro, "-u", user, "--exec"}, command...)
}

// wslHelperEnv is the environment of a helper call: the agent's base variables
// (wsl.exe needs SYSTEMROOT and its siblings to run at all), WSL_UTF8 so the
// error text wsl.exe prints is readable in a log line, and -- for a call that
// talks to the user's systemd manager, uid >= 0 -- XDG_RUNTIME_DIR, the only
// variable WSLENV carries across, so systemctl --user finds the bus at
// /run/user/<uid>. The UID lookup itself (uid < 0) carries nothing across.
func wslHelperEnv(getenv func(string) string, uid int) []string {
	env := make([]string, 0, len(baseEnvNames)+3)
	for _, name := range baseEnvNames {
		if v := getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	env = append(env, "WSL_UTF8=1")
	if uid >= 0 {
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(uid), "WSLENV=XDG_RUNTIME_DIR/u")
	}
	return env
}

// runWSLHelper runs one helper call into distro, bounded by wslHelperTimeout.
// It refuses a root user itself rather than trusting every caller to have
// checked: this is the one way the agent's own calls reach a distro.
func runWSLHelper(run wslRunner, pol WSLPolicy, distro string, env []string, command ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wslHelperTimeout)
	defer cancel()
	return runWSLHelperCtx(ctx, run, pol, distro, env, command...)
}

// runWSLHelperCtx is runWSLHelper under a deadline the caller sets: the RAM
// sampler shares one, shorter, across a beat's calls (sampleWSL).
func runWSLHelperCtx(ctx context.Context, run wslRunner, pol WSLPolicy, distro string, env []string, command ...string) ([]byte, error) {
	if wslUserRefused(pol.User) {
		return nil, fmt.Errorf("runtime: no non-root WSL user is configured (runtime_wsl_user)")
	}
	return run(ctx, pol.Exe, wslHelperArgs(pol.User, distro, command...), env)
}

// wslUIDFailureMessage is the last_error of a wsl start whose UID lookup
// failed. It names the setting, not the distro or the user: the detail (which
// distro, what id -u or wsl.exe printed) goes to the agent's own log
// (handleWSLUIDResolved), where the operator who owns both reads it.
const wslUIDFailureMessage = "runtime: could not resolve the uid of runtime_wsl_user in the WSL distro; see the agent log"

// lookupWSLUID asks distro for the configured user's UID (id -u), which names
// the user manager's bus (/run/user/<uid>) that the launch and every scope
// call need. It is a wsl.exe call bounded by wslHelperTimeout and runs off the
// owner (requestWSLUID). A user whose UID is 0 is refused like root by name:
// wsl.exe -u would run the workload, and the gateway-chosen environment it
// carries, as root.
func lookupWSLUID(run wslRunner, pol WSLPolicy, getenv func(string) string, distro string) (int, error) {
	out, err := runWSLHelper(run, pol, distro, wslHelperEnv(getenv, -1), "id", "-u")
	if err != nil {
		return 0, fmt.Errorf("runtime: could not look up the UID of the WSL user (runtime_wsl_user) in distro %q: %w", distro, err)
	}
	text := strings.TrimSpace(string(out))
	uid, err := strconv.Atoi(text)
	if err != nil || uid < 0 {
		return 0, fmt.Errorf("runtime: the WSL user (runtime_wsl_user) has no numeric UID in distro %q (id -u printed %q)", distro, clipText(text, 64))
	}
	if uid == 0 {
		return 0, fmt.Errorf("runtime: the WSL user (runtime_wsl_user) is UID 0 in distro %q; refusing to run anything as root there", distro)
	}
	return uid, nil
}

// requestWSLUID starts the UID lookup for distro off the owner, unless one is
// already running: however many instances wait for one distro, it is asked
// once. The answer comes back as cmdWSLUIDResolved. Owner goroutine only; the
// goroutine is tracked in the manager's WaitGroup, so Close waits for it (at
// most wslHelperTimeout). Once the owner has stopped, the answer is dropped
// (postCmd).
func (o *owner) requestWSLUID(distro string) {
	if o.closing || o.wslUIDInFlight[distro] {
		return
	}
	if o.wslUIDInFlight == nil {
		o.wslUIDInFlight = make(map[string]bool)
	}
	o.wslUIDInFlight[distro] = true
	run, pol, getenv, m := o.wslRunner, o.policy.WSL, o.getenv, o.m
	m.wg.Go(func() {
		uid, err := lookupWSLUID(run, pol, getenv, distro)
		m.postCmd(cmdWSLUIDResolved{distro: distro, uid: uid, err: err})
	})
}

// handleWSLUIDResolved takes a lookup's answer on the owner. An answer is
// cached, starts the distro's orphan sweep if it is still due (with the UID it
// needs, so the sweep asks nothing itself), and re-asks the instances waiting
// for it, through the ordinary admission wake. A failure is logged with its
// detail and fails the start of every instance waiting for that distro, with
// the generic wslUIDFailureMessage: start_failed, then the ordinary crash-loop
// backoff, whose retry asks the distro again (requestWSLUID; failures are
// never cached). A lookup nobody waits for (the first applyConfig's) fails
// silently but for the log.
func (o *owner) handleWSLUIDResolved(c cmdWSLUIDResolved) {
	delete(o.wslUIDInFlight, c.distro)
	if o.closing {
		return
	}
	if c.err != nil {
		slog.Warn("runtime: could not resolve the WSL user's uid", "distro", c.distro, "error", c.err)
		for _, st := range o.specs {
			if !st.waitingOnWSLUID || st.proc != nil || st.spec.WSLDistro() != c.distro {
				continue
			}
			st.waitingOnWSLUID = false
			if !o.wantsUp(st) {
				continue
			}
			o.setState(st, StateStartFailed)
			o.recordFailure(st, wslUIDFailureMessage, 0, "")
			o.failPending(st, ErrStartFailed)
			o.enterBackoff(st)
		}
		return
	}
	if o.wslUIDs == nil {
		o.wslUIDs = make(map[string]int)
	}
	o.wslUIDs[c.distro] = c.uid
	if o.wslSweepDue[c.distro] {
		delete(o.wslSweepDue, c.distro)
		sw := wslSweep{run: o.wslRunner, pol: o.policy.WSL, env: wslHelperEnv(o.getenv, c.uid), nonce: o.wslNonce, done: o.m.done}
		distro := c.distro
		o.m.wg.Go(func() { sw.sweepDistro(distro) })
	}
	o.wakeAdmission(func(st *specState) bool {
		return st.waitingOnWSLUID && st.spec.WSLDistro() == c.distro && wantsWake(st)
	})
}

// wslControl signals a WSL generation from inside its distro, through the
// systemd scope it was launched in (wsl.exe ... --exec systemd-run --user
// --scope --unit=<unit>). Signalling the process the agent exec'd would not do:
// killing wsl.exe sends the workload no signal, it only closes the relay, so
// the workload dies of a SIGHUP it has no handler for -- no clean shutdown --
// and a child that called setsid survives as an orphan.
// `systemctl --user kill --signal=... <unit>.scope` reaches every process in
// the scope's cgroup, setsid'd ones included.
//
// terminate sends SIGTERM to the scope. kill sends SIGKILL to the scope and
// then kills wsl.exe itself as the backstop: should the scope be unreachable
// (no user bus, a unit that never came up), killing wsl.exe still ends the
// generation, and for vLLM it tears the whole tree down (measured in Phase 0).
//
// Each call is a wsl.exe process of its own and may take seconds, so it runs
// on a goroutine tracked in the manager's WaitGroup (wg), never on the owner,
// and Close waits for it. Both methods therefore return nil at once: what a
// signal achieved shows as the generation's exit, exactly as for a native
// process, and a failed call is logged and left to the kill grace's
// escalation (terminate) or to the backstop (kill).
//
// Generation identity: unit names this generation alone (wslUnitName carries
// the agent run's nonce and the generation), so a call that lands late can
// never signal a successor, and cmd is this generation's own wsl.exe. The
// backstop is skipped once the generation has exited (exited is closed by
// waitForExit), so it never signals a pid that may already be recycled -- up
// to the same instant between reaping and close that the native kill accepts.
type wslControl struct {
	run    wslRunner
	pol    WSLPolicy
	distro string
	unit   string
	// env is the helper calls' environment (wslHelperEnv with the UID).
	env []string
	// cmd is the wsl.exe running this generation: the kill's backstop.
	cmd    *exec.Cmd
	wg     *sync.WaitGroup
	exited <-chan struct{}
	specID string
}

func (c wslControl) terminate() error {
	c.wg.Go(func() { c.signalScope("SIGTERM") })
	return nil
}

func (c wslControl) kill() error {
	c.wg.Go(func() {
		c.signalScope("SIGKILL")
		select {
		case <-c.exited:
			return
		default:
		}
		if err := killGroup(c.cmd); err != nil {
			slog.Debug("runtime: killing wsl.exe failed", "spec", c.specID, "error", err)
		}
	})
	return nil
}

func (c wslControl) signalScope(sig string) {
	if _, err := runWSLHelper(c.run, c.pol, c.distro, c.env, "systemctl", "--user", "kill", "--signal="+sig, c.unit+".scope"); err != nil {
		slog.Warn("runtime: signalling the WSL scope failed", "spec", c.specID, "distro", c.distro, "unit", c.unit, "signal", sig, "error", err)
	}
}

// newWSLNonce draws a manager's run identity for its unit names (owner.wslNonce):
// 8 hex characters, so two agent runs sharing a distro user can tell their units
// apart. crypto/rand.Read does not fail (it aborts the program instead).
func newWSLNonce() string {
	var b [wslNonceLen / 2]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// wslLaunchFailed reports whether exitCode is wsl.exe's own failure rather than
// the workload's: wsl.exe exits -1 when it could not run the command at all,
// which Windows reports as the unsigned 4294967295. A workload's own exit is
// its WEXITSTATUS, a death by signal the raw signal number -- never either of
// these.
func wslLaunchFailed(exitCode int) bool {
	c := int64(exitCode)
	return c == -1 || c == math.MaxUint32
}

// wslExitMessage is the last_error message of a WSL start that wsl.exe itself
// failed (wslLaunchFailed). wsl.exe's own error text went to its stdout, which
// is the runtime log, so the message points there instead of guessing.
func wslExitMessage(exitCode int) string {
	return fmt.Sprintf("wsl.exe could not run the workload (exit %d); see the runtime log", exitCode)
}

// maybeWarmWSL runs once per manager lifetime, from the first applyConfig,
// when WSL is configured: it starts the UID lookup for every allowed distro
// off the owner (requestWSLUID) and marks every distro as due for its orphan
// sweep, which the distro's first UID answer starts (handleWSLUIDResolved).
//
// The lookups are started before the document's specs are admitted, so a wsl
// start in it waits for them rather than asking again, and a later start
// usually finds its UID cached. The sweep exists because a unit outlives its
// agent when the agent dies without stopping it (a crash, a kill, a host-side
// update): its workload still holds the GPU, the RAM and the port inside the
// VM, invisible to this run. Tying it to the UID answer gives it the bus
// address it needs without a lookup of its own, and sweeps a distro that was
// unreachable at first as soon as a later start reaches it. A manager that is
// never configured never calls into a distro.
func (o *owner) maybeWarmWSL() {
	if o.wslWarmed || o.closing {
		return
	}
	o.wslWarmed = true
	pol := o.policy.WSL
	if pol.Exe == "" || wslUserRefused(pol.User) {
		return
	}
	o.wslSweepDue = make(map[string]bool)
	for _, distro := range pol.AllowedDistros {
		// A file-sourced allowlist entry is not validated like a spec's distro;
		// only a name a spec could use is passed to wsl.exe -d.
		if !wslDistroPattern.MatchString(distro) {
			continue
		}
		o.wslSweepDue[distro] = true
		o.requestWSLUID(distro)
	}
}

// wslSweep is one distro's orphan sweep: its inputs, copied off the owner so
// the sweep's goroutine reads nothing the owner owns. env is the helper
// environment for the distro's UID. done is the manager's: once the owner has
// stopped, the sweep issues no further call, so Close waits for at most the
// one in flight.
//
// It never touches a unit of this run, even one whose spec starts while the
// sweep is listing: every unit this run launches carries this manager's nonce
// (wslUnitName), and the sweep kills only units whose nonce differs.
type wslSweep struct {
	run   wslRunner
	pol   WSLPolicy
	env   []string
	nonce string
	done  <-chan struct{}
}

func (s wslSweep) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// sweepDistro lists the agent's scopes in distro and SIGKILLs those of another
// run. SIGKILL rather than a graceful stop: an orphan has nobody waiting for
// its answers and no grace to honour, and the point is to give its resources
// back before this run admits anything against them. Every failure is a Warn;
// the sweep never blocks a start.
func (s wslSweep) sweepDistro(distro string) {
	if s.stopped() {
		return
	}
	out, err := runWSLHelper(s.run, s.pol, distro, s.env, "systemctl", "--user", "list-units", "--plain", "--no-legend", "--all", "--full", wslUnitPrefix+"*.scope")
	if err != nil {
		slog.Warn("runtime: WSL orphan sweep could not list the agent's units", "distro", distro, "error", err)
		return
	}
	for _, unit := range wslOrphanUnits(out, s.nonce) {
		if s.stopped() {
			return
		}
		if _, err := runWSLHelper(s.run, s.pol, distro, s.env, "systemctl", "--user", "kill", "--signal=SIGKILL", unit); err != nil {
			slog.Warn("runtime: WSL orphan sweep could not stop a unit", "distro", distro, "unit", unit, "error", err)
			continue
		}
		slog.Info("runtime: stopped a WSL unit an earlier agent run left behind", "distro", distro, "unit", unit)
	}
}

// wslOrphanUnits picks, from a `systemctl --user list-units --plain
// --no-legend --all --full` listing (UNIT LOAD ACTIVE SUB DESCRIPTION per line;
// --full so no long name is ellipsized), the agent's scopes that belong to
// another run: the name is one wslUnitName could have built
// (parseWSLUnitName), and its nonce differs from nonce. Any other
// opagent-prefixed unit is not the agent's and is never touched. A scope that
// is inactive or failed holds no process any more -- a scope is active for as
// long as one remains -- so it is left alone rather than signalled into an
// error on every agent start.
func wslOrphanUnits(listing []byte, nonce string) []string {
	var out []string
	for _, line := range strings.Split(string(listing), "\n") {
		f := strings.Fields(line)
		// Some systemd versions mark a unit with a leading bullet even in plain
		// output.
		if len(f) > 0 && (f[0] == "●" || f[0] == "*") {
			f = f[1:]
		}
		if len(f) < 3 {
			continue
		}
		unit, active := f[0], f[2]
		name, ok := strings.CutSuffix(unit, ".scope")
		if !ok {
			continue
		}
		unitNonce, _, _, ok := parseWSLUnitName(name)
		if !ok || unitNonce == nonce {
			continue
		}
		if active == "inactive" || active == "failed" {
			continue
		}
		out = append(out, unit)
	}
	return out
}
