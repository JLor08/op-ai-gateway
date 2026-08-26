// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Manager owns every managed model-server process for one agent: it starts,
// health-waits, drains, restarts (with crash backoff), and idle-unloads
// them, and answers the router's admission question (design doc
// docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md §6.3).
//
// THE SERIALIZED OWNER (the one thing most likely to go wrong in a
// concurrency-sensitive component like this): every piece of mutable state --
// specs, running processes, per-spec state, the admission wait queue -- is
// owned EXCLUSIVELY by one goroutine (owner.run, below), reached only through
// the m.cmds command channel. Public methods never touch that state directly;
// EnsureRunning sends a request and blocks on a per-request reply channel.
// Admission decisions (Admit, from policy.go) are always followed, in the SAME
// owner-loop iteration, by marking the outcome (victims Draining, target
// Starting) BEFORE any goroutine that could race a second admission decision
// is spawned. This is what makes "two concurrent requests for the same cold
// spec must start exactly one process" true by construction rather than by
// luck -- see TestManagerConcurrentEnsureSingleStart.
//
// Generation identity: internal/proxy's Manager establishes the discipline
// this package reuses verbatim -- a *runningProc pointer, not a spec ID, is
// the generation identity, so a delayed exit-report for a process that has
// already been superseded (map entry replaced or removed) is a no-op. Every
// place below that reacts to an asynchronous report about a *runningProc
// checks "is this still the CURRENT generation for this spec?" first.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Package-level timing knobs -- the client.backoffBase / certinstall.hookTimeout
// pattern: VARS, not consts, so a test can shrink them (save the original,
// restore via defer) to run in milliseconds instead of the real seconds these
// defaults use. Read once per relevant timer/ticker construction, so a test
// must set its shrunk value BEFORE creating the Manager (drainGrace/killGrace/
// backoffBase/backoffCap/stableRunThreshold) or before the idle ticker fires
// for the first time (idleTickInterval); healthPollInterval is read on every
// poll tick, so it may be changed any time before the process starts.
var (
	// drainGrace bounds how long a drain-stop waits for InFlight to reach
	// zero before proceeding to SIGTERM anyway.
	drainGrace = 10 * time.Second
	// killGrace bounds how long a SIGTERM'd process group gets before
	// escalating to SIGKILL.
	killGrace = 5 * time.Second
	// backoffBase is the crash-backoff base delay (delay = backoffBase *
	// 2^(failures-1), capped at backoffCap) -- the WSSender discipline
	// (internal/client/ws.go's backoffDelay).
	backoffBase = 1 * time.Second
	// backoffCap is the crash-backoff maximum delay.
	backoffCap = 60 * time.Second
	// stableRunThreshold: a Running spell at least this long resets the
	// crash-failure streak back to zero.
	stableRunThreshold = 60 * time.Second
	// healthPollInterval is the cadence of health-endpoint probes while a
	// spec is Starting.
	healthPollInterval = 500 * time.Millisecond
	// idleTickInterval is how often the owner scans for idle-timeout
	// unloads.
	idleTickInterval = 15 * time.Second
)

// Typed errors the router maps to HTTP (design doc §6.5). Stable identity
// (errors.Is), stable wire code (Error() text) -- coding agents and the
// portal depend on both never silently changing shape.
var (
	ErrModelNotManaged  = errors.New("runtime.model_not_managed")
	ErrStartFailed      = errors.New("runtime.start_failed")
	ErrStartTimeout     = errors.New("runtime.start_timeout")
	ErrAdmissionBlocked = errors.New("runtime.admission_blocked")
	ErrNotPermitted     = errors.New("runtime.not_permitted")
	// ErrManagerClosed is returned by EnsureRunning (and queued waiters) once
	// Close has been called. Not one of the design doc's §6.5 wire codes --
	// this can only happen during agent shutdown, a case the router does not
	// need a distinct HTTP mapping for (the connection is going away with the
	// whole process) -- but a stable sentinel is cheap and lets a caller
	// distinguish "shutting down" from "genuinely blocked" if it ever cares.
	ErrManagerClosed = errors.New("runtime.manager_closed")
)

// measurerFunc mirrors SetMeasurer's function parameter, named so it can be
// held behind an atomic.Pointer.
type measurerFunc func(pids []int) map[int]map[int]int

// ManagerOptions configures a new Manager.
type ManagerOptions struct {
	// Policy is the agent-operator boundary (binary allowlist, permitted
	// directories) -- see policy_local.go. An empty Policy (its zero value)
	// permits nothing, by LocalPolicy.Permit's own design.
	Policy LocalPolicy
	// Getenv resolves ${AGENT_ENV:NAME} placeholders and the agent's own
	// PATH/HOME (see ExpandPlaceholders). os.Getenv in production; injected
	// in tests so no real environment variable is required.
	Getenv func(string) string
}

// Manager is the process supervisor described in the package doc above.
// Every exported method is safe for concurrent use.
type Manager struct {
	cmds chan command
	// done is closed by the owner goroutine as the very last thing it does
	// before returning from run(), i.e. the moment it stops reading cmds.
	// postCmd selects on it so a caller can never block forever sending to a
	// channel nobody is receiving from any more.
	done chan struct{}

	closeOnce sync.Once

	// transitions is buffered(1): a state-change notification a consumer has
	// not yet drained is never queued twice, it just stays pending -- "some
	// state changed, go re-read Status()" coalesces naturally.
	transitions chan struct{}

	// measurer is read (never written) by the owner goroutine and written
	// (never read) by SetMeasurer's caller -- an atomic pointer swap needs no
	// owner round-trip and keeps the owner loop's per-admission read
	// lock-free, mirroring proxy.certHolder's atomic.Pointer pattern.
	measurer atomic.Pointer[measurerFunc]

	// wg tracks EVERY goroutine the owner spawns (health pollers, exit
	// waiters, the owner goroutine itself, every scheduleAfter timer
	// callback) so Close can block until none of them can touch shared state
	// (in particular the package-level timing vars above) any more --
	// required for tests to shrink/restore those vars race-free around
	// Close.
	wg sync.WaitGroup
}

// NewManager starts the owner goroutine and returns immediately; Apply must
// be called at least once before anything is running.
func NewManager(opts ManagerOptions) *Manager {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	m := &Manager{
		cmds:        make(chan command),
		done:        make(chan struct{}),
		transitions: make(chan struct{}, 1),
	}
	o := &owner{
		m:            m,
		policy:       opts.Policy,
		getenv:       getenv,
		specs:        make(map[string]*specState),
		allowedPairs: map[[2]string]bool{},
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		o.run()
	}()
	return m
}

// postCmd sends c to the owner, or drops it silently once the owner has
// stopped (Close has fully completed) -- at that point there is no state
// left for any command to affect.
func (m *Manager) postCmd(c command) {
	select {
	case m.cmds <- c:
	case <-m.done:
	}
}

// scheduleAfter runs f in its own goroutine after d, tracked in m.wg so
// Close (and thus a test's package-var restore, which the house pattern
// defers AFTER Close) can wait for it. f must not block and must not touch
// owner state directly -- every use in this file has f post a command back
// to the owner instead.
func (m *Manager) scheduleAfter(d time.Duration, f func()) *time.Timer {
	m.wg.Add(1)
	return time.AfterFunc(d, func() {
		defer m.wg.Done()
		f()
	})
}

// cancelTimer stops *t if it has not already fired, releasing scheduleAfter's
// wg accounting for the callback that will now never run (a callback that
// already ran, or is genuinely racing this exact instant, already owns its
// own Done() call -- Stop() returning false means exactly "don't call Done
// yourself, the callback will").
func cancelTimer(m *Manager, t **time.Timer) {
	if *t == nil {
		return
	}
	if (*t).Stop() {
		m.wg.Done()
	}
	*t = nil
}

// Apply reconciles desired state: new/changed specs, removed specs (drained),
// pinned/force_running specs started, force_stopped specs drained. Blocks
// until the owner has applied it (which may itself only be the START of
// asynchronous work -- a newly pinned spec's process may still be starting
// when Apply returns). Idempotent: a Config whose ETag equals the
// currently-applied one is a no-op.
func (m *Manager) Apply(cfg Config) {
	done := make(chan struct{})
	m.postCmd(cmdApply{cfg: cfg, done: done})
	select {
	case <-done:
	case <-m.done:
	}
}

// EnsureRunning is the router's entry point: returns the child's loopback
// base URL ("http://127.0.0.1:<port>") once upstreamModel's spec is Running,
// starting/evicting/queueing per the admission policy. Blocks up to the
// spec's admission-wait timeout (0 = wait until ctx is done) plus its startup
// timeout. release must be called exactly once, when the caller is done
// proxying to endpoint; it decrements the in-flight counter, stamps
// LastUsed, and wakes any request queued behind this spec's resources.
func (m *Manager) EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error) {
	reply := make(chan ensureOutcome, 1)
	select {
	case m.cmds <- cmdEnsure{model: upstreamModel, reply: reply}:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	case <-m.done:
		return "", nil, ErrManagerClosed
	}

	// Watch ctx for cancellation while queued (e.g. the HTTP client
	// disconnected during a long admission wait). cancelDone stops this
	// goroutine promptly once the reply is in, rather than leaking it until
	// ctx eventually fires on its own.
	cancelDone := make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		select {
		case <-ctx.Done():
			m.postCmd(cmdCancelEnsure{reply: reply, err: ctx.Err()})
		case <-cancelDone:
		}
	}()

	var out ensureOutcome
	select {
	case out = <-reply:
	case <-m.done:
		close(cancelDone)
		return "", nil, ErrManagerClosed
	}
	close(cancelDone)

	if out.err != nil {
		return "", nil, out.err
	}

	var once sync.Once
	specID, proc := out.specID, out.proc
	release = func() {
		once.Do(func() {
			m.postCmd(cmdRelease{specID: specID, proc: proc})
		})
	}
	return out.endpoint, release, nil
}

// Status returns a snapshot of every managed spec's current status.
func (m *Manager) Status() []Status {
	reply := make(chan []Status, 1)
	m.postCmd(cmdStatus{reply: reply})
	select {
	case s := <-reply:
		return s
	case <-m.done:
		return nil
	}
}

// LoadedModels returns the upstream model names of every spec CURRENTLY in
// StateRunning -- Starting does not count (a cold/loading model must not be
// selected by prefer-loaded routing logic upstream).
func (m *Manager) LoadedModels() []string {
	reply := make(chan []string, 1)
	m.postCmd(cmdLoadedModels{reply: reply})
	select {
	case s := <-reply:
		return s
	case <-m.done:
		return nil
	}
}

// Transitions returns a channel that receives a signal on any spec state
// change. Buffered(1): a burst of transitions between two reads of this
// channel coalesces into a single pending wake, exactly like
// client.WSSender's certUpdates/trustUpdates doorbells.
func (m *Manager) Transitions() <-chan struct{} {
	return m.transitions
}

// SetMeasurer installs f, which the owner calls (with every currently-live
// managed PID) while building each admission snapshot, to get real per-GPU
// VRAM usage instead of a spec's static estimate (Task 18 wires this to
// nvidia-smi). f may be nil to go back to estimates only, which is also
// NewManager's default -- measurement is a capability, not a protocol
// feature, so an agent without a measurement path simply never calls this.
func (m *Manager) SetMeasurer(f func(pids []int) map[int]map[int]int) {
	if f == nil {
		m.measurer.Store(nil)
		return
	}
	fn := measurerFunc(f)
	m.measurer.Store(&fn)
}

// Close drain-stops every managed process (bounded: every spec's drain runs
// concurrently, so the total bound is drainGrace+killGrace regardless of how
// many specs are running, not their sum) and blocks until the owner
// goroutine and every goroutine it spawned have fully exited. Idempotent.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.postCmd(cmdClose{})
	})
	<-m.done    // the owner has stopped reading cmds
	m.wg.Wait() // every side goroutine (timers, health polls, wait-for-exit, cancel watchers) has finished
}

// endpointFor formats a managed process's loopback base URL.
func endpointFor(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

// grabEphemeralPort binds 127.0.0.1:0, reads back the OS-assigned port, and
// releases it immediately -- "grab and release": the child is expected to
// bind it within the startup-timeout window. A narrow TOCTOU race (something
// else grabbing the same port first) is an accepted, documented trade-off,
// not a defect.
func grabEphemeralPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// extractExitCode returns a Wait() error's process exit code, or -1 when it
// is not an *exec.ExitError (e.g. the process could not be waited on at
// all).
func extractExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ---------------------------------------------------------------------------
// Owner-private state. Every type and function below this point is touched
// EXCLUSIVELY by the single owner goroutine (owner.run) -- there is no lock
// because there is no concurrent access to guard against.
// ---------------------------------------------------------------------------

// command is anything the owner goroutine processes. Implementations are
// unexported structs constructed only within this file.
type command interface{ isCommand() }

type cmdApply struct {
	cfg  Config
	done chan struct{}
}

func (cmdApply) isCommand() {}

type cmdEnsure struct {
	model string
	reply chan ensureOutcome
}

func (cmdEnsure) isCommand() {}

type cmdCancelEnsure struct {
	reply chan ensureOutcome
	err   error // the caller's ctx.Err(); what the waiter is resolved with
}

func (cmdCancelEnsure) isCommand() {}

type cmdWaiterTimeout struct {
	specID string
	reply  chan ensureOutcome
}

func (cmdWaiterTimeout) isCommand() {}

type cmdRelease struct {
	specID string
	proc   *runningProc
}

func (cmdRelease) isCommand() {}

type cmdChildExited struct {
	proc    *runningProc
	exitErr error
}

func (cmdChildExited) isCommand() {}

type cmdStartResult struct {
	specID string
	proc   *runningProc
	ok     bool
	err    error
}

func (cmdStartResult) isCommand() {}

type cmdBackoffFire struct{ specID string }

func (cmdBackoffFire) isCommand() {}

type cmdStableRun struct {
	specID string
	proc   *runningProc
}

func (cmdStableRun) isCommand() {}

type cmdDrainGraceExpired struct {
	specID string
	proc   *runningProc
}

func (cmdDrainGraceExpired) isCommand() {}

type cmdKillGraceExpired struct {
	specID string
	proc   *runningProc
}

func (cmdKillGraceExpired) isCommand() {}

type cmdStatus struct{ reply chan []Status }

func (cmdStatus) isCommand() {}

type cmdLoadedModels struct{ reply chan []string }

func (cmdLoadedModels) isCommand() {}

type cmdClose struct{}

func (cmdClose) isCommand() {}

// ensureOutcome is what an EnsureRunning caller eventually receives.
type ensureOutcome struct {
	endpoint string
	specID   string
	proc     *runningProc
	err      error
}

// ensureWaiter is one queued EnsureRunning request. timer is non-nil only
// when the spec's AdmissionWaitTimeoutSeconds is positive; 0 means "wait
// until the caller's own ctx is done" (design doc §6.2), so no timer is
// scheduled.
type ensureWaiter struct {
	reply chan ensureOutcome
	timer *time.Timer
}

// runningProc is one live generation of a managed process. Its pointer
// identity IS the generation -- see the package doc's "Generation identity"
// section. Fields are set once at start (except the three timers, which are
// scheduled/cancelled as the generation's lifecycle progresses) and never
// touched by anything except the owner goroutine.
type runningProc struct {
	specID    string
	cmd       *exec.Cmd
	pid       int
	port      int
	ring      *ringBuffer
	startedAt time.Time
	// exited is closed by waitForExit right before it reports the exit to
	// the owner, so pollHealth can select on it (a process that dies before
	// becoming healthy stops polling immediately instead of waiting out a
	// full healthPollInterval tick).
	exited chan struct{}

	drainTimer  *time.Timer // pending "drain grace exceeded, terminate anyway" callback
	killTimer   *time.Timer // pending "kill grace exceeded, SIGKILL" callback
	stableTimer *time.Timer // pending "this run has been stable long enough" callback
}

// specState is one spec's complete owner-side bookkeeping.
type specState struct {
	spec Spec

	state State
	since time.Time

	proc *runningProc // non-nil while Starting, Running, or Draining

	inFlight  int
	lastUsed  time.Time
	lastError *LastError

	failures     int  // consecutive crash/start-failure streak; drives the NEXT backoff delay
	hasRunBefore bool // false until the first successful start, so restarts starts counting from the second
	restarts     int

	backoffTimer *time.Timer // pending "backoff elapsed, try again" callback

	// intentionalStop is set immediately before this generation's process is
	// signalled to exit for a reason the owner already knows about (drain,
	// idle-unload, force_stopped, startup timeout) so the eventual exit
	// report is classified as a clean stop rather than a fresh crash.
	// Cleared as soon as that exit is processed.
	intentionalStop bool

	// removed is set when this spec was dropped from a newly-applied Config
	// while its process was still live: the drain proceeds exactly as usual,
	// but on completion the specState is deleted instead of returning to
	// StateStopped.
	removed bool

	// measuredVRAM is the last real per-GPU measurement observed for this
	// spec's current process, cached opportunistically whenever ANY spec's
	// admission snapshot is built (buildSnapshot). Reported via Status; falls
	// back to nil (never measured) when no measurer is installed.
	measuredVRAM map[int]int

	pending []*ensureWaiter
}

// owner holds every piece of state the single serialized goroutine touches.
type owner struct {
	m      *Manager
	policy LocalPolicy
	getenv func(string) string

	cfg          Config
	allowedPairs map[[2]string]bool
	specs        map[string]*specState
	byUpstream   map[string]string // upstream_model -> spec ID

	closing bool // true from the moment cmdClose is received
	stopped bool // true once every live process has been confirmed gone during close
}

// run is the owner's single event loop. Every state mutation in this
// package happens on this goroutine, reached only via m.cmds (and the idle
// ticker, which is itself just another event source feeding decisions made
// right here).
func (o *owner) run() {
	defer close(o.m.done)

	idleTick := time.NewTicker(idleTickInterval)
	defer idleTick.Stop()

	for {
		select {
		case cmd := <-o.m.cmds:
			o.handle(cmd)
		case <-idleTick.C:
			if !o.closing {
				o.scanIdle()
			}
		}
		if o.stopped {
			return
		}
	}
}

func (o *owner) handle(cmd command) {
	switch c := cmd.(type) {
	case cmdApply:
		o.applyConfig(c.cfg)
		close(c.done)
	case cmdEnsure:
		o.handleEnsure(c)
	case cmdCancelEnsure:
		o.handleCancelEnsure(c)
	case cmdWaiterTimeout:
		o.handleWaiterTimeout(c)
	case cmdRelease:
		o.handleRelease(c)
	case cmdChildExited:
		o.handleChildExited(c)
	case cmdStartResult:
		o.handleStartResult(c)
	case cmdBackoffFire:
		o.handleBackoffFire(c.specID)
	case cmdStableRun:
		o.handleStableRun(c)
	case cmdDrainGraceExpired:
		o.handleDrainGraceExpired(c)
	case cmdKillGraceExpired:
		o.handleKillGraceExpired(c)
	case cmdStatus:
		c.reply <- o.snapshotStatus()
	case cmdLoadedModels:
		c.reply <- o.loadedModels()
	case cmdClose:
		o.handleClose()
	}
	if o.closing {
		o.maybeFinishClose()
	}
}

// applyConfig reconciles owner state to cfg. See Manager.Apply's doc for the
// externally-visible contract.
func (o *owner) applyConfig(cfg Config) {
	if cfg.ETag != "" && cfg.ETag == o.cfg.ETag {
		return
	}
	o.cfg = cfg
	o.allowedPairs = cfg.AllowedPairs()

	newIDs := make(map[string]bool, len(cfg.Specs))
	for _, spec := range cfg.Specs {
		newIDs[spec.ID] = true
		st, exists := o.specs[spec.ID]
		if !exists {
			o.specs[spec.ID] = &specState{spec: spec, state: StateStopped, since: time.Now()}
			continue
		}
		st.spec = spec
		if st.proc == nil {
			switch st.state {
			case StateNotPermitted, StatePendingVRAMUnknown, StateStartFailed, StateCrashed:
				o.setState(st, StateStopped)
			case StateBackoff:
				cancelTimer(o.m, &st.backoffTimer)
				o.setState(st, StateStopped)
			}
		}
	}

	for id, st := range o.specs {
		if newIDs[id] {
			continue
		}
		if st.proc != nil {
			st.removed = true
			o.beginDrain(id)
		} else {
			// C2 fix: resolve any queued waiter before dropping the spec --
			// otherwise it (and even its own admission_wait_timeout, since
			// handleWaiterTimeout is itself a no-op once the spec is gone)
			// would never be resolved at all.
			o.failPending(st, ErrModelNotManaged)
			delete(o.specs, id)
		}
	}

	o.rebuildUpstreamIndex()

	for id, st := range o.specs {
		if !newIDs[id] {
			continue
		}
		if st.spec.AdminState == "force_stopped" {
			if st.proc != nil {
				o.beginDrain(id)
			}
			continue
		}
		if (st.spec.Pinned || st.spec.AdminState == "force_running") && st.proc == nil && st.state != StateBackoff {
			o.admitAndStart(id)
		}
	}
	o.wakeAllPendingWaiters()
}

func (o *owner) rebuildUpstreamIndex() {
	o.byUpstream = make(map[string]string, len(o.specs))
	for id, st := range o.specs {
		o.byUpstream[st.spec.UpstreamModel] = id
	}
}

// handleEnsure resolves an EnsureRunning request immediately when possible
// (already Running, or a terminal not-retryable state), otherwise queues it
// and (re)attempts admission.
func (o *owner) handleEnsure(c cmdEnsure) {
	if o.closing {
		c.reply <- ensureOutcome{err: ErrManagerClosed}
		return
	}
	specID, ok := o.byUpstream[c.model]
	st := o.specs[specID]
	if !ok || st == nil {
		c.reply <- ensureOutcome{err: ErrModelNotManaged}
		return
	}
	if st.spec.AdminState == "force_stopped" {
		c.reply <- ensureOutcome{err: ErrAdmissionBlocked}
		return
	}
	if st.state == StateRunning && st.proc != nil {
		st.inFlight++
		st.lastUsed = time.Now()
		c.reply <- ensureOutcome{endpoint: endpointFor(st.proc.port), specID: specID, proc: st.proc}
		return
	}
	if st.state == StateNotPermitted {
		c.reply <- ensureOutcome{err: ErrNotPermitted}
		return
	}
	if st.state == StatePendingVRAMUnknown {
		c.reply <- ensureOutcome{err: ErrAdmissionBlocked}
		return
	}

	w := &ensureWaiter{reply: c.reply}
	if st.spec.AdmissionWaitTimeoutSeconds > 0 {
		d := time.Duration(st.spec.AdmissionWaitTimeoutSeconds) * time.Second
		reply := c.reply
		w.timer = o.m.scheduleAfter(d, func() {
			o.m.postCmd(cmdWaiterTimeout{specID: specID, reply: reply})
		})
	}
	st.pending = append(st.pending, w)
	o.admitAndStart(specID)
}

// handleCancelEnsure resolves and removes the queued waiter matching
// c.reply -- fixes C1: previously this removed the waiter WITHOUT ever
// sending to its reply channel, so a caller whose context was cancelled
// while queued (the routine case of a client disconnecting during a cold
// start) hung forever. c.reply is buffered(1), so this send is always
// safe and can never block the owner.
func (o *owner) handleCancelEnsure(c cmdCancelEnsure) {
	for _, st := range o.specs {
		for i, w := range st.pending {
			if w.reply == c.reply {
				cancelTimer(o.m, &w.timer)
				st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
				err := c.err
				if err == nil {
					err = context.Canceled
				}
				c.reply <- ensureOutcome{err: err}
				return
			}
		}
	}
}

func (o *owner) handleWaiterTimeout(c cmdWaiterTimeout) {
	st := o.specs[c.specID]
	if st == nil {
		return
	}
	for i, w := range st.pending {
		if w.reply == c.reply {
			w.timer = nil // this IS that timer's own callback; nothing left to Stop()
			st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
			c.reply <- ensureOutcome{err: ErrAdmissionBlocked}
			return
		}
	}
}

func (o *owner) handleRelease(c cmdRelease) {
	st := o.specs[c.specID]
	if st == nil || st.proc != c.proc {
		return // stale/superseded generation: nothing to decrement
	}
	if st.inFlight > 0 {
		st.inFlight--
	}
	st.lastUsed = time.Now()
	if st.state == StateDraining && st.inFlight == 0 {
		o.terminateNow(st, c.proc)
	}
	o.wakeAllPendingWaiters()
}

// failPending resolves and clears every waiter queued on st with err.
func (o *owner) failPending(st *specState, err error) {
	for _, w := range st.pending {
		cancelTimer(o.m, &w.timer)
		w.reply <- ensureOutcome{err: err}
	}
	st.pending = nil
}

// succeedPending resolves and clears every waiter queued on st with st's
// current (freshly Running) proc, incrementing InFlight once per resolved
// waiter -- each gets its own independent release().
func (o *owner) succeedPending(st *specState, specID string) {
	ep := endpointFor(st.proc.port)
	for _, w := range st.pending {
		cancelTimer(o.m, &w.timer)
		st.inFlight++
		w.reply <- ensureOutcome{endpoint: ep, specID: specID, proc: st.proc}
	}
	st.pending = nil
	st.lastUsed = time.Now()
}

func (o *owner) setState(st *specState, s State) {
	if st.state == s {
		return
	}
	st.state = s
	st.since = time.Now()
	select {
	case o.m.transitions <- struct{}{}:
	default:
	}
}

func (o *owner) setNotPermitted(st *specState, message string) {
	o.setState(st, StateNotPermitted)
	st.lastError = &LastError{Message: message, At: time.Now()}
	o.failPending(st, ErrNotPermitted)
}

func (o *owner) recordFailure(st *specState, message string, exitCode int, stderrTail string) {
	st.failures++
	st.lastError = &LastError{
		Message:    message,
		At:         time.Now(),
		ExitCode:   exitCode,
		Failures:   st.failures,
		StderrTail: stderrTail,
	}
}

// admitAndStart is the single place that decides whether spec should move
// toward Running right now, and if so starts it -- called for a freshly
// queued request, after a backoff/drain/idle completion frees something up,
// or from Apply for a pinned/force_running spec. Always a fast, synchronous,
// non-blocking call: any real work (exec, health polling) is handed to a
// goroutine before this returns.
func (o *owner) admitAndStart(specID string) {
	if o.closing {
		// Never start (or restart) anything once shutdown has begun -- a
		// pinned/force_running spec resurrecting itself here would keep
		// maybeFinishClose from ever seeing every process gone, hanging
		// Close forever.
		return
	}
	st := o.specs[specID]
	if st == nil || st.proc != nil || st.state == StateBackoff {
		return
	}
	wantUp := len(st.pending) > 0 || st.spec.Pinned || st.spec.AdminState == "force_running"
	if !wantUp {
		return
	}
	if st.spec.AdminState == "force_stopped" {
		o.failPending(st, ErrAdmissionBlocked)
		return
	}

	if err := o.policy.Permit(st.spec); err != nil {
		o.setNotPermitted(st, err.Error())
		return
	}

	dec := Admit(o.buildSnapshot(), st.spec)
	switch {
	case dec.Reason == StateNotPermitted:
		o.setNotPermitted(st, dec.Message)
	case dec.Reason == StatePendingVRAMUnknown:
		o.setState(st, StatePendingVRAMUnknown)
		o.failPending(st, ErrAdmissionBlocked)
	case dec.Wait:
		// Leave st queued; a future completion event elsewhere re-triggers
		// this via wakeAllPendingWaiters.
	case len(dec.Evict) > 0:
		for _, victimID := range dec.Evict {
			o.beginDrain(victimID)
		}
		// Retried once every victim is confirmed gone (onProcExited calls
		// wakeAllPendingWaiters).
	default: // dec.OK
		o.startProcess(st)
	}
}

// buildSnapshot assembles the pure PolicySnapshot Admit needs from current
// owner state, consulting the installed measurer (if any) for real per-GPU
// VRAM instead of each spec's static estimate.
func (o *owner) buildSnapshot() PolicySnapshot {
	pids := make([]int, 0, len(o.specs))
	for _, st := range o.specs {
		if st.proc != nil {
			pids = append(pids, st.proc.pid)
		}
	}
	var measured map[int]map[int]int
	if mp := o.m.measurer.Load(); mp != nil {
		measured = (*mp)(pids)
	}

	running := make([]RunningProc, 0, len(o.specs))
	for id, st := range o.specs {
		if st.proc == nil {
			continue
		}
		byGPU := measured[st.proc.pid]
		gpus := make(map[int]int, len(st.spec.GPUs))
		for _, g := range st.spec.GPUs {
			if byGPU != nil {
				if v, ok := byGPU[g.Index]; ok {
					gpus[g.Index] = v
					continue
				}
			}
			gpus[g.Index] = g.VRAMMB
		}
		if byGPU != nil {
			st.measuredVRAM = byGPU
		}
		running = append(running, RunningProc{
			SpecID:   id,
			GPUs:     gpus,
			InFlight: st.inFlight,
			Pinned:   st.spec.Pinned,
			LastUsed: st.lastUsed,
		})
	}

	budgets := make(map[int]int, len(o.cfg.GPUBudgets))
	for _, b := range o.cfg.GPUBudgets {
		budgets[b.Index] = b.BudgetMB
	}

	return PolicySnapshot{
		Running:      running,
		MaxProcesses: o.cfg.MaxProcesses,
		Budgets:      budgets,
		Allowed:      o.allowedPairs,
	}
}

// startProcess resolves the listen port, expands placeholders, execs the
// child (never blocking the owner loop beyond the exec.Cmd.Start() call
// itself), and hands off health-waiting and exit-waiting to their own
// goroutines.
func (o *owner) startProcess(st *specState) {
	spec := st.spec

	port := spec.ListenPort
	if port == 0 {
		p, err := grabEphemeralPort()
		if err != nil {
			o.setState(st, StateStartFailed)
			o.recordFailure(st, "runtime: grab ephemeral port: "+err.Error(), 0, "")
			o.failPending(st, ErrStartFailed)
			return
		}
		port = p
	}

	args, env, err := ExpandPlaceholders(spec, port, o.getenv)
	if err != nil {
		o.setNotPermitted(st, err.Error())
		return
	}

	ring := &ringBuffer{}
	cmd := exec.Command(spec.Binary, args...) //nolint:gosec // spec.Binary is allowlisted by LocalPolicy.Permit above
	cmd.Env = env
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	cmd.Stdout = ring
	cmd.Stderr = ring
	setProcGroup(cmd)

	if err := cmd.Start(); err != nil {
		o.setState(st, StateStartFailed)
		o.recordFailure(st, fmt.Sprintf("runtime: exec %s: %s", spec.Binary, err.Error()), 0, "")
		o.failPending(st, ErrStartFailed)
		return
	}

	proc := &runningProc{
		specID:    spec.ID,
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		port:      port,
		ring:      ring,
		startedAt: time.Now(),
		exited:    make(chan struct{}),
	}
	st.proc = proc
	o.setState(st, StateStarting)
	slog.Info("runtime: process starting", "spec", spec.ID, "binary", spec.Binary, "port", port, "pid", proc.pid)

	o.m.wg.Add(1)
	go o.waitForExit(proc)

	o.m.wg.Add(1)
	go o.pollHealth(spec.ID, proc, spec)
}

// waitForExit blocks until proc's process exits, then reports it to the
// owner. cmd.Wait() (with a non-*os.File Stdout/Stderr, as set in
// startProcess) itself waits for the stdout/stderr copy goroutines to
// finish first, so proc.ring already holds every byte the process ever
// wrote by the time this reports the exit.
func (o *owner) waitForExit(proc *runningProc) {
	defer o.m.wg.Done()
	err := proc.cmd.Wait()
	close(proc.exited)
	o.m.postCmd(cmdChildExited{proc: proc, exitErr: err})
}

// pollHealth probes proc's health endpoint immediately, then every
// healthPollInterval, until a 2xx response, the process exits (in which case
// waitForExit already owns reporting the failure -- this just stops
// silently), or the spec's startup timeout elapses.
func (o *owner) pollHealth(specID string, proc *runningProc, spec Spec) {
	defer o.m.wg.Done()

	startupTimeout := time.Duration(spec.StartupTimeoutSeconds) * time.Second
	if startupTimeout <= 0 {
		startupTimeout = 30 * time.Second
	}
	deadline := proc.startedAt.Add(startupTimeout)

	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}
	probeTimeout := time.Duration(spec.HealthTimeoutSeconds) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	url := endpointFor(proc.port) + healthPath
	client := &http.Client{}

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		if probeOnce(client, url, probeTimeout) {
			o.m.postCmd(cmdStartResult{specID: specID, proc: proc, ok: true})
			return
		}
		if time.Now().After(deadline) {
			o.m.postCmd(cmdStartResult{specID: specID, proc: proc, err: ErrStartTimeout})
			return
		}
		select {
		case <-proc.exited:
			return
		case <-ticker.C:
		}
	}
}

func probeOnce(client *http.Client, url string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain for keep-alive reuse
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (o *owner) handleStartResult(c cmdStartResult) {
	st := o.specs[c.specID]
	if st == nil || st.proc != c.proc {
		return // superseded generation
	}
	if c.ok {
		if st.hasRunBefore {
			st.restarts++
		}
		st.hasRunBefore = true
		o.setState(st, StateRunning)
		st.lastError = nil
		st.failures = 0
		proc := c.proc
		proc.stableTimer = o.m.scheduleAfter(stableRunThreshold, func() {
			o.m.postCmd(cmdStableRun{specID: c.specID, proc: proc})
		})
		o.succeedPending(st, c.specID)
		return
	}

	// c.err is ErrStartTimeout: the child never became healthy in time.
	tail := c.proc.ring.Tail(2048)
	o.setState(st, StateStartFailed)
	o.recordFailure(st, "runtime: startup timed out waiting for health", 0, tail)
	o.failPending(st, ErrStartTimeout)
	o.terminateNow(st, c.proc)
}

func (o *owner) handleStableRun(c cmdStableRun) {
	st := o.specs[c.specID]
	if st == nil || st.proc != c.proc {
		return
	}
	c.proc.stableTimer = nil
	if st.state == StateRunning {
		st.failures = 0
	}
}

func (o *owner) handleChildExited(c cmdChildExited) {
	st := o.specs[c.proc.specID]
	if st == nil || st.proc != c.proc {
		return // superseded generation: this exit no longer matters
	}
	o.onProcExited(st, c.exitErr)
}

func (o *owner) onProcExited(st *specState, exitErr error) {
	proc := st.proc
	cancelTimer(o.m, &proc.killTimer)
	cancelTimer(o.m, &proc.drainTimer)
	cancelTimer(o.m, &proc.stableTimer)
	st.proc = nil
	// C3 fix: the generation's requests are dead by definition. Without
	// this, a request in flight when the child crashed left InFlight stuck
	// above zero forever, and Admit's isEvictable (which requires
	// InFlight==0) then treated this spec as permanently busy, as if
	// pinned -- never evictable again, and never idle-unloadable.
	st.inFlight = 0

	wasIntentional := st.intentionalStop
	st.intentionalStop = false
	removed := st.removed
	wasRunning := st.state == StateRunning

	slog.Info("runtime: process exited", "spec", st.spec.ID, "intentional", wasIntentional, "exit_code", extractExitCode(exitErr))

	if wasIntentional {
		if removed {
			// C2 fix: resolve any queued waiter before dropping the spec --
			// otherwise it (and even its own admission_wait_timeout, since
			// handleWaiterTimeout is itself a no-op once the spec is gone)
			// would never be resolved at all.
			o.failPending(st, ErrModelNotManaged)
			delete(o.specs, st.spec.ID)
			o.wakeAllPendingWaiters()
			return
		}
		if st.state == StateDraining {
			o.setState(st, StateStopped)
		}
		if st.state == StateStopped {
			o.admitAndStart(st.spec.ID)
		}
		o.wakeAllPendingWaiters()
		return
	}

	exitCode := extractExitCode(exitErr)
	tail := proc.ring.Tail(2048)
	if wasRunning {
		o.setState(st, StateCrashed)
		o.recordFailure(st, fmt.Sprintf("runtime: process exited unexpectedly (exit code %d)", exitCode), exitCode, tail)
		o.enterBackoff(st)
	} else {
		o.setState(st, StateStartFailed)
		o.recordFailure(st, fmt.Sprintf("runtime: process exited before becoming healthy (exit code %d)", exitCode), exitCode, tail)
		o.failPending(st, ErrStartFailed)
	}
	o.wakeAllPendingWaiters()
}

func (o *owner) enterBackoff(st *specState) {
	o.setState(st, StateBackoff)
	delay := backoffDelayFor(st.failures)
	specID := st.spec.ID
	st.backoffTimer = o.m.scheduleAfter(delay, func() {
		o.m.postCmd(cmdBackoffFire{specID: specID})
	})
}

func backoffDelayFor(failures int) time.Duration {
	d := backoffBase
	for i := 1; i < failures && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return d
}

func (o *owner) handleBackoffFire(specID string) {
	st := o.specs[specID]
	if st == nil {
		return
	}
	st.backoffTimer = nil // this IS that timer's own callback
	if st.state == StateBackoff {
		o.setState(st, StateStopped)
	}
	o.admitAndStart(specID)
}

// beginDrain transitions a running spec to Draining and either terminates it
// immediately (no in-flight requests) or waits up to drainGrace for
// in-flight to reach zero first. Idempotent: a no-op if already draining or
// not running at all.
func (o *owner) beginDrain(specID string) {
	st := o.specs[specID]
	if st == nil || st.proc == nil || st.state == StateDraining {
		return
	}
	o.setState(st, StateDraining)
	proc := st.proc
	if st.inFlight == 0 {
		o.terminateNow(st, proc)
		return
	}
	proc.drainTimer = o.m.scheduleAfter(drainGrace, func() {
		o.m.postCmd(cmdDrainGraceExpired{specID: specID, proc: proc})
	})
}

func (o *owner) handleDrainGraceExpired(c cmdDrainGraceExpired) {
	st := o.specs[c.specID]
	if st == nil || st.proc != c.proc {
		return
	}
	c.proc.drainTimer = nil
	o.terminateNow(st, c.proc) // grace exceeded: proceed regardless of remaining in-flight
}

func (o *owner) handleKillGraceExpired(c cmdKillGraceExpired) {
	st := o.specs[c.specID]
	if st == nil || st.proc != c.proc {
		return
	}
	c.proc.killTimer = nil
	if err := killGroup(c.proc.cmd); err != nil {
		slog.Debug("runtime: SIGKILL failed", "spec", c.specID, "error", err)
	}
	// State transitions to Stopped/Crashed once waitForExit actually reports
	// the exit -- not here, since the process may take a moment to die even
	// after SIGKILL is delivered.
}

// terminateNow sends SIGTERM to proc's process group and schedules a
// SIGKILL escalation after killGrace if it has not exited by then. Marks
// st.intentionalStop so the eventual exit report is classified as a clean
// stop, not a crash.
func (o *owner) terminateNow(st *specState, proc *runningProc) {
	st.intentionalStop = true
	cancelTimer(o.m, &proc.drainTimer)
	if err := terminateGroup(proc.cmd); err != nil {
		slog.Debug("runtime: SIGTERM failed", "spec", st.spec.ID, "error", err)
	}
	specID := st.spec.ID
	proc.killTimer = o.m.scheduleAfter(killGrace, func() {
		o.m.postCmd(cmdKillGraceExpired{specID: specID, proc: proc})
	})
}

// wakeAllPendingWaiters re-attempts admission for every spec that is idle
// (no live process), not in an active crash-backoff wait, and has at least
// one request queued -- called after any event that could plausibly have
// freed a resource another spec's admission decision depends on (a release,
// a process actually exiting, a config Apply).
func (o *owner) wakeAllPendingWaiters() {
	for id, st := range o.specs {
		if st.proc == nil && len(st.pending) > 0 && st.state != StateBackoff {
			o.admitAndStart(id)
		}
	}
}

// scanIdle drain-stops every Running, unpinned, non-force_running spec whose
// idle timeout has elapsed with zero in-flight requests.
func (o *owner) scanIdle() {
	now := time.Now()
	for id, st := range o.specs {
		if st.proc == nil || st.state != StateRunning {
			continue
		}
		if st.spec.Pinned || st.spec.AdminState == "force_running" {
			continue
		}
		if st.spec.IdleTimeoutSeconds <= 0 || st.inFlight != 0 {
			continue
		}
		idle := time.Duration(st.spec.IdleTimeoutSeconds) * time.Second
		if now.Sub(st.lastUsed) >= idle {
			o.beginDrain(id)
		}
	}
}

func (o *owner) snapshotStatus() []Status {
	out := make([]Status, 0, len(o.specs))
	for id, st := range o.specs {
		s := Status{
			SpecID:       id,
			Model:        st.spec.UpstreamModel,
			State:        st.state,
			Since:        st.since,
			InFlight:     st.inFlight,
			Restarts:     st.restarts,
			LastError:    st.lastError,
			MeasuredVRAM: st.measuredVRAM,
		}
		if st.proc != nil {
			s.PID = st.proc.pid
			s.Port = st.proc.port
		}
		out = append(out, s)
	}
	return out
}

func (o *owner) loadedModels() []string {
	var out []string
	for _, st := range o.specs {
		if st.state == StateRunning {
			out = append(out, st.spec.UpstreamModel)
		}
	}
	return out
}

// handleClose begins a bounded shutdown: every live process starts draining
// concurrently (so the total wait is bounded by drainGrace+killGrace, not
// their sum across specs), every pending backoff is cancelled immediately
// (nothing should wait out a crash-backoff delay just because the agent is
// shutting down), and every queued waiter is failed now since nothing will
// ever start again.
func (o *owner) handleClose() {
	o.closing = true
	for id, st := range o.specs {
		if st.state == StateBackoff {
			cancelTimer(o.m, &st.backoffTimer)
			o.setState(st, StateStopped)
		}
		o.failPending(st, ErrManagerClosed)
		if st.proc != nil {
			o.beginDrain(id)
		}
	}
}

func (o *owner) maybeFinishClose() {
	if !o.closing || o.stopped {
		return
	}
	for _, st := range o.specs {
		if st.proc != nil {
			return
		}
	}
	o.stopped = true
}
