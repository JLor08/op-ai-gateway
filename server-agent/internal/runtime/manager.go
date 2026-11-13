// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Manager owns every managed model-server process for one agent: it starts,
// health-waits, drains, restarts (with crash backoff), and idle-unloads them
// (docs/architecture/cross-cutting/agent-runtime-manager.md §6 "Process
// lifecycle"; drain in §5.4), and answers the router's admission question
// (same file, §5 "Admission control").
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
// checks "is this still the CURRENT generation for this instance?" first.
//
// ONE PROCESS PER INSTANCE (#340). A replicable spec may run several
// instances of itself, so the owner's state is keyed by INSTANCE KEY (S for
// instance 0, S#k for k >= 1 -- InstanceKey), not by spec ID: each instance
// has its own specState, its own generation, its own lifecycle, backoff,
// drain and measurement, and everything above applies to it unchanged. A
// spec that never replicates has exactly one instance, instance 0, whose key
// is its spec ID -- so for every such spec the owner is exactly what it was
// before instances existed. The few places that treat instances of one spec
// differently from unrelated specs (siblings) say so where they do it:
// address resolution (resolveAddress), demand inheritance (demandSpec for a
// candidate, chargeSpec for an occupant), the burst/baseline split
// (admitAndStart), the protected floor (protected, ensureProtectedFloor), the
// order instances are asked in (instanceLess), the order the PROTECTED ones
// start in -- lowest index first, whatever asks (lowerSiblingWaiting); a burst
// instance is never held back for a lower sibling, it takes free room or is
// refused at once -- the "highest index leaves first" invariant (scanIdle,
// evictOneIdleForBootstrap), retirement (retireIfSpent), plain requests that
// never start a burst instance (EnsureServing) and output (one log buffer per
// spec, a further instance's lines prefixed "[#k] ").
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
	"reflect"
	"slices"
	"sort"
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
	// idleTickInterval is the owner's housekeeping beat: how often it scans
	// for idle-timeout unloads AND how often it dispatches a VRAM and a RAM
	// measurement of the live managed processes (dispatchMeasurement). One
	// beat rather than two because both are "sweep the current process set
	// and act on what has changed", and because neither does any real work
	// on the owner goroutine -- the measurement (a subprocess for VRAM,
	// in-process reads for RAM) runs elsewhere and posts its answer back.
	idleTickInterval = 15 * time.Second
	// notPermittedRetryInterval bounds how often StateNotPermitted /
	// StatePendingVRAMUnknown are re-evaluated (fix round 2 / R2-1). A
	// request arriving within this interval of the state's own onset gets
	// the cached verdict immediately, with no Permit/Admit/port-grab; once
	// it elapses, the next request re-evaluates fully and self-clears if
	// the underlying problem is fixed -- a rate limiter, not the
	// permanent stickiness I6 removed. 5s bounds retry-driven syscall
	// (grabEphemeralPort, for the PATH/HOME and ${AGENT_ENV:...} causes)
	// and measurer-invocation (for PendingVRAMUnknown, once a later task
	// wires a real one) cost to at most once per 5s per spec even under a
	// request storm, while keeping an operator's config-fix recovery time
	// short enough to need no gateway-side action -- the same order of
	// magnitude as backoffBase's own default.
	notPermittedRetryInterval = 5 * time.Second
)

// Cold-start RAM bootstrap failure-handling knobs (task 4.4). These bound the
// one escalation a stuck bootstrap is allowed, so that responding to memory
// pressure can NEVER become an unbounded evict-and-retry ("C3") storm. They are
// consts, not tunable vars: the whole point is a hard, non-configurable ceiling.
const (
	// ramBootstrapMaxRetries is the number of guarded evict-and-retry
	// escalations a single stuck bootstrap may trigger before it falls back to
	// the ordinary StateStartFailed crash-loop backoff. ONE: the first
	// memory-pressure death evicts an idle process and retries once; a SECOND
	// consecutive memory-pressure death does NOT escalate again (no further
	// eviction), it goes straight to StateStartFailed. The per-spec counter it
	// bounds (specState.ramBootstrapRetries) is reset only by a successful RAM
	// measurement, so a later, unrelated pressure event can earn the one shot
	// again -- but a bootstrap that keeps dying escalates at most once, and the
	// eviction count stays bounded however many times the child OOMs.
	ramBootstrapMaxRetries = 1
	// ramBootstrapMeasureBeats bounds the TERMINATION case: a bootstrap that
	// comes up healthy but is never measured would hold the RAMBootstrapInFlight
	// interlock (task 4.1) forever -- and that interlock makes every OTHER RAM
	// admission on the host Wait. After this many fruitless measurement beats
	// (idleTickInterval each, so ~10 beats on the production cadence) the owner
	// forces the spec to StateStartFailed so the interlock cannot hang the host
	// indefinitely. The counter (specState.ramBootstrapBeats) only advances while
	// a RAM measurer is installed and the spec is a healthy, still-unmeasured
	// bootstrap; a real measurement resets it.
	ramBootstrapMeasureBeats = 10
	// ramPressureBudgetNum / ramPressureBudgetDen define "the last-measured RSS
	// was near the host RAM budget at death" as a fraction (9/10 = 90%). A
	// bootstrap whose own final RSS reached at least this share of the whole
	// host-RAM pool is treated as having died of memory pressure even without a
	// readable kill signal: one process alone that large was crowding the pool.
	// Integer math (measuredRAM*Den >= budget*Num) avoids floating point. The
	// fraction is deliberately high: the signature must not fire for a process
	// that merely used a healthy slice of the pool, only for one that all but
	// filled it.
	ramPressureBudgetNum = 9
	ramPressureBudgetDen = 10
)

// bootstrapDiedOfMemoryPressure classifies a bootstrap process's death from its
// EXIT SIGNATURE and its LAST MEASUREMENT -- never free text (task 4.4). It
// returns true for the two memory-pressure signatures and false for everything
// else (a plain non-zero exit, a config/binary error), which is the genuine
// crash the existing crash-loop backoff already handles unchanged.
//
//   - oomSignaled: the kernel killed the process with SIGKILL (the Linux OOM
//     killer's signal; see killedBySIGKILL). The agent's OWN SIGKILL escalation
//     never reaches here -- it rides the intentional-stop branch of
//     onProcExited -- so a SIGKILL seen on the crash path is an external kill,
//     overwhelmingly an OOM for a model mid-load.
//   - the RSS-near-budget signature: the process's last measured resident RAM
//     reached ramPressureBudgetNum/Den of the host-RAM budget at death. Needs a
//     real budget (a hardware sample) and a real measurement; with either
//     missing only the signal can classify, which is the conservative direction
//     (fall through to a genuine crash rather than evict on a guess).
func bootstrapDiedOfMemoryPressure(oomSignaled bool, measuredRAMMB, hostRAMBudgetMB int) bool {
	if oomSignaled {
		return true
	}
	if hostRAMBudgetMB > 0 && measuredRAMMB > 0 {
		return measuredRAMMB*ramPressureBudgetDen >= hostRAMBudgetMB*ramPressureBudgetNum
	}
	return false
}

// Typed errors the router maps to HTTP
// (docs/architecture/cross-cutting/agent-runtime-manager.md §4.3 "Stable
// error codes"). Stable identity (errors.Is), stable wire code (Error()
// text) -- coding agents and the portal depend on both never silently
// changing shape.
var (
	ErrModelNotManaged  = errors.New("runtime.model_not_managed")
	ErrStartFailed      = errors.New("runtime.start_failed")
	ErrStartTimeout     = errors.New("runtime.start_timeout")
	ErrAdmissionBlocked = errors.New("runtime.admission_blocked")
	ErrNotPermitted     = errors.New("runtime.not_permitted")
	// ErrManagerClosed is returned by EnsureRunning (and queued waiters) once
	// Close has been called. Not one of §4.3's wire codes --
	// this can only happen during agent shutdown, a case the router does not
	// need a distinct HTTP mapping for (the connection is going away with the
	// whole process) -- but a stable sentinel is cheap and lets a caller
	// distinguish "shutting down" from "genuinely blocked" if it ever cares.
	ErrManagerClosed = errors.New("runtime.manager_closed")
	// ErrInstanceNoCapacity refuses a BURST instance (#340, IsBurst) that cannot
	// serve right now, at once and with no waiter queued -- the gateway then
	// selects another instance or another host:
	//   - an ensure (EnsureRunning) of one that does not fit into the room that
	//     is free right now: its admission is all-or-nothing
	//     (PolicySnapshot.NoEvict), so where a baseline start would evict an
	//     idle neighbour, queue behind a busy one or spend a bootstrap shot, a
	//     burst start is refused, and no state is left behind;
	//   - an ensure of one that is draining: burst capacity does not wait for a
	//     slot;
	//   - a plain request (EnsureServing) for one that is neither running nor
	//     starting: a request the router forwards never starts burst capacity
	//     (a row resting at a failure is retired by it, handleEnsure).
	// The router answers a request whose address moved while it was admitted
	// with it as well. Wire code runtime.instance_no_capacity (409).
	ErrInstanceNoCapacity = errors.New("runtime.instance_no_capacity")
	// ErrInstanceDemandUnknown refuses a BURST instance whose VRAM or RAM
	// demand is still unknown after inheriting the largest live measurement of
	// every running sibling (owner.demandSpec): an instance k >= 1 is never
	// admitted blind. It is the fast twin of the baseline instance's wait for a
	// measured sibling; the gateway retries shortly, by which time the first
	// instance has usually been measured (a process is measured as soon as its
	// first health probe passes). Wire code runtime.instance_demand_unknown
	// (409).
	ErrInstanceDemandUnknown = errors.New("runtime.instance_demand_unknown")
)

// measurerFunc mirrors SetMeasurer's function parameter, named so it can be
// held behind an atomic.Pointer.
type measurerFunc func(pids []int) map[int]map[int]int

// ramMeasurerFunc mirrors SetRAMMeasurer's function parameter, named so it can
// be held behind an atomic.Pointer. It is deliberately not measurerFunc: host
// RAM has no per-GPU axis, so its answer is one MB figure per pid.
type ramMeasurerFunc func(pids []int) map[int]int

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
	// GPUVendor is the GPU stack this HOST actually has, discovered locally
	// by main.go from collector.DetectGPUCollectors() -- never a
	// gateway-supplied value. It selects the environment variable a
	// SetVisibleDevices spec gets (VisibleDevicesVar); its zero value,
	// GPUVendorNone, is the correct answer for a CPU-only host and makes
	// that option a no-op rather than an error.
	GPUVendor GPUVendor

	// GPUVendors is the full detected vendor set (see GPUVendors). Optional;
	// empty falls back to GPUVendor alone.
	GPUVendors GPUVendors
	// LogBufferBytes / LogBufferTotalBytes size the per-spec managed-process
	// output retention and its fleet-wide ceiling (see LogStore). Both are
	// OPERATOR settings from the agent's own local config -- memory on an AI
	// server is the operator's tradeoff, and like every other agent-owned
	// runtime setting these can never be supplied by the gateway. Zero means
	// "use the documented default", so a Manager built without them (every
	// test) still gets a working store.
	LogBufferBytes      int
	LogBufferTotalBytes int
	// SpecsFromLocalFile states that this agent's launch specs come from a
	// LOCAL FILE (OP_AGENT_RUNTIME_SOURCE=file) rather than from the gateway.
	// It is fixed for the process lifetime -- the source is resolved once at
	// startup by main.go and cannot change without a restart -- which is why
	// it is a Manager option rather than something read off a Config.
	//
	// It has exactly ONE consumer: the masking of the resolved launch command
	// the log stream reports (command.go). In file mode a spec's env VALUES
	// are the one thing the agent deliberately withholds from the gateway
	// (report.go's redactConfigEnv), because a local document is the
	// operator's own and may hold a plaintext secret; the reported command
	// masks them for the same reason and by the same rule. Its zero value,
	// false, is the gateway-sourced case, where the gateway already holds
	// every literal in the document.
	SpecsFromLocalFile bool
	// ConfigFilesDir is the agent-owned, writable base directory under which
	// the config files a spec's config_files facet asks for are written, one
	// per-spec subdirectory (<ConfigFilesDir>/<spec-id>/<name>) -- per
	// instance for a replicable spec (<ConfigFilesDir>/<spec-id>#<k>, #340). It is
	// OPERATOR configuration from the agent's own local config (the
	// config_files_dir setting) -- the gateway names a file's relative name and
	// content, never where on the host it lands. Fixed for the process
	// lifetime, like the other options here. Empty (every test that does not
	// exercise config files) means no base directory was configured.
	ConfigFilesDir string
	// VRAMHeadroomMB is the amount of each GPU's DETECTED total memory, in MB,
	// that is held back from the auto-detected VRAM budget (see
	// owner.buildSnapshot): room for the driver, a display, other tenants. It
	// is OPERATOR configuration from the agent's own local settings
	// (OP_AGENT_VRAM_HEADROOM_MB) -- the gateway never supplies it. It applies
	// only to a detected budget; an operator-configured per-GPU budget is used
	// exactly as given. Fixed for the process lifetime, like the other options
	// here. Zero (the default) holds nothing back; a negative value is treated
	// as zero, since it would inflate the budget past the device.
	VRAMHeadroomMB int
	// HostRAMReserveMB is the amount of memory, in MB, that is held back from
	// the host-RAM pool admission budgets managed processes against (the pool
	// is the host's live available memory plus what the managed processes
	// already hold, minus this -- see hostRAMBudgetMB): room for the OS, the
	// agent itself and whatever else shares the machine. It is OPERATOR
	// configuration from the agent's own local settings
	// (OP_AGENT_HOST_RAM_RESERVE_MB) -- the gateway never supplies it. Fixed
	// for the process lifetime, like the other options here. The agent's config
	// defaults it to 2048; zero here (the default of a caller that sets
	// nothing, such as most tests) holds nothing back, and a negative value is
	// treated as zero, since it would inflate the pool past what the host has.
	HostRAMReserveMB int
	// RAMBootstrapGuessMB is the conservative host-RAM footprint, in MB, charged to
	// a spec of unknown RAM demand on its FIRST start so that cold start is admitted
	// against the host-RAM pool instead of starting blind -- replaced by the real
	// measurement once one lands. It is OPERATOR configuration from the agent's own
	// local settings (OP_AGENT_RAM_BOOTSTRAP_GUESS_MB) -- the gateway never supplies
	// it. Fixed for the process lifetime, like the other options here. The agent's
	// config defaults it to 4096 (erring high is the safe direction); zero here (the
	// default of a caller that sets nothing, such as most tests) turns the guess off
	// so an unknown-RAM spec starts unconstrained, and a negative value is treated
	// as zero.
	RAMBootstrapGuessMB int
	// EvictionGrace is how long after a process was last used it stays off the
	// eviction victim list: a model used within it counts as in use, never a
	// victim however hard the admission pressure, so a bursty pipeline (a voice
	// chat between turns) is not broken by a start that happens to need room. It
	// is OPERATOR configuration from the agent's own local settings
	// (OP_AGENT_EVICTION_GRACE_SECONDS) -- the gateway never supplies it. Fixed
	// for the process lifetime, like the other options here. The agent's config
	// defaults it to 30 seconds; zero here (the default of a caller that sets
	// nothing, such as most tests) turns the protection off, so an idle process is
	// evictable at once as before, and a negative value is treated as zero.
	EvictionGrace time.Duration
	// WSL is the agent's WSL configuration (#381): the wsl.exe, the distros and
	// binaries a wsl spec may name, and the distro user everything runs as.
	// OPERATOR configuration from the agent's own local settings
	// (runtime_wsl_*), never gateway-supplied, and fixed for the process
	// lifetime. When set it REPLACES Policy.WSL: the manager keeps one WSL
	// policy, so the block a spec is permitted against is the very block it is
	// launched, stopped and swept with -- above all the same user, which must
	// never be root. Left zero, Policy.WSL stands; both zero means no wsl spec
	// is permitted (permitWSL) and no distro is ever called.
	WSL WSLPolicy
	// WSLRAMReserveMB is the memory, in MB, held back from the WSL2 VM's RAM
	// pool that admission budgets wsl processes against: the WSL twin of
	// HostRAMReserveMB, operator configuration (OP_AGENT_WSL_RAM_RESERVE_MB,
	// default 1024). Fixed for the process lifetime; a negative value is treated
	// as zero.
	WSLRAMReserveMB int
	// WSLRunner runs the agent's helper calls into a distro (the UID lookup,
	// scope signals, the orphan sweep). nil, the production case, is
	// execWSLRunner; a test injects one to observe or fail single calls.
	WSLRunner wslRunner
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
	// state changed, or a row left the table, go re-read Status()" coalesces
	// naturally.
	transitions chan struct{}

	// measurer is read (never written) by the owner goroutine and written
	// (never read) by SetMeasurer's caller -- an atomic pointer swap needs no
	// owner round-trip and keeps the owner loop's per-admission read
	// lock-free, mirroring proxy.certHolder's atomic.Pointer pattern.
	measurer atomic.Pointer[measurerFunc]

	// ramMeasurer is the host-RAM twin of measurer above, under the same
	// discipline: the owner only reads it, SetRAMMeasurer's caller only writes
	// it, and an atomic swap needs no owner round-trip. nil (the default, and
	// every host whose collector.NewRAMMeasurer returns nil) means no RAM is
	// measured. It is read only by the off-owner dispatchMeasurement;
	// buildSnapshot, which feeds admission, never calls it -- it reads the last
	// result the beat stored (specState.measuredRAM), so the admission path pays
	// no measurement.
	ramMeasurer atomic.Pointer[ramMeasurerFunc]

	// relayGate is the agent's word on whether a relay-mode member's relay is
	// listening (SetMemberRelayGate). Written by its installer, read by the
	// member's readiness poll off the owner goroutine; nil means no gate.
	relayGate atomic.Pointer[func(specID string) error]

	// hostInfo is the latest hardware figures the agent's collect loop pushed
	// (SetHostInfo): device totals and live available RAM. Same discipline as
	// measurer above -- the owner only reads it, the collect loop only writes
	// it, and an atomic pointer swap needs no owner round-trip, so the
	// admission path reads a cached value and never shells out. nil until the
	// first sample has been pushed, which the owner reads as "nothing
	// detected" (owner.hostInfo).
	hostInfo atomic.Pointer[HostInfo]

	// hostWake is the collect loop's way of telling the owner that a fresh
	// hardware sample has landed (SetHostInfo), so it can re-ask a start whose
	// admission verdict depends on it. It is buffered(1) and posted with a
	// non-blocking send, which is the whole point of it being a channel of its
	// own rather than a command on cmds: cmds is unbuffered and postCmd blocks
	// until the owner reads, and the collect loop must never wait on the owner
	// (it samples hardware about once a second and does nothing else).
	// dispatchMeasurement solves the same problem with a goroutine per post, but
	// that goroutine would be started from a foreign goroutine, where a
	// WaitGroup.Add can race Close's Wait; a coalescing token needs neither. A
	// wake that finds one already pending is simply dropped -- "a sample landed,
	// re-read the latest" is all it says, and the owner re-reads hostInfo itself
	// -- so a burst of samples is one wake, and none is ever lost while the owner
	// is busy. The owner reads it in run() and does all the work there, on its
	// own goroutine, so the serialized-owner invariant holds. nil on a bare
	// Manager{} built by a test that never runs an owner; a send on it falls
	// through the select's default.
	hostWake chan struct{}

	// hostSeq counts the samples SetHostInfo has stored. The owner reads it to
	// tell how many samples have landed since a managed process exited (see
	// owner.noteRAMFreed): the sample that is current at an exit may predate it,
	// so what the host has freed shows up in the pool only a sample or two later.
	// Written by the collect loop, read by the owner; an atomic counter needs no
	// owner round-trip, like hostInfo beside it.
	hostSeq atomic.Uint64

	// wg tracks EVERY goroutine the owner spawns (health pollers, exit
	// waiters, the owner goroutine itself, every scheduleAfter timer
	// callback) so Close can block until none of them can touch shared state
	// (in particular the package-level timing vars above) any more --
	// required for tests to shrink/restore those vars race-free around
	// Close.
	wg sync.WaitGroup

	// logs is the per-spec managed-process output retention plus the
	// on-demand live fan-out (logs.go). It is a Manager field rather than
	// owner state because it is written from os/exec's copying goroutines
	// (never the owner) and read by the Driver's SetWatch/Drain on the
	// agent's run loop; it carries its own mutex for exactly that reason.
	logs *LogStore
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
		hostWake:    make(chan struct{}, 1),
		logs:        NewLogStore(opts.LogBufferBytes, opts.LogBufferTotalBytes),
	}
	policy := opts.Policy
	if !opts.WSL.isZero() {
		policy.WSL = opts.WSL
	}
	run := opts.WSLRunner
	if run == nil {
		run = execWSLRunner
	}
	o := &owner{
		m:                   m,
		policy:              policy,
		getenv:              getenv,
		gpuVendor:           opts.GPUVendor,
		gpuVendors:          opts.GPUVendors,
		specsFromLocalFile:  opts.SpecsFromLocalFile,
		configFilesDir:      opts.ConfigFilesDir,
		vramHeadroomMB:      max(0, opts.VRAMHeadroomMB),
		hostRAMReserveMB:    max(0, opts.HostRAMReserveMB),
		ramBootstrapGuessMB: max(0, opts.RAMBootstrapGuessMB),
		evictionGrace:       max(0, opts.EvictionGrace),
		wslRunner:           run,
		wslNonce:            newWSLNonce(),
		wslRAMReserveMB:     max(0, opts.WSLRAMReserveMB),
		specs:               make(map[string]*specState),
		allowedPairs:        map[[2]string]bool{},
		logs:                m.logs,
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		o.run()
	}()
	return m
}

// Logs exposes the managed-process output store so the Driver can apply the
// gateway's watch command to it and drain what it has queued. Never nil for a
// Manager built by NewManager.
func (m *Manager) Logs() *LogStore { return m.logs }

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
// currently-applied one is a no-op -- unless it opts a different set of specs
// into instances (Spec.Replicable), the one part of a document the Driver may
// withhold until the gateway declares runtime_instances (see applyConfig).
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
//
// upstreamModel may be an INSTANCE ADDRESS (#340): "m#k" names instance k of
// the spec whose upstream model is m, resolved by the same rule as
// ResolveAddress. An allowed instance that does not exist yet is created on
// demand, and a burst instance resting at a crash or a failed start is retired
// and created afresh: burst capacity is never retried on its own, so an ensure
// naming it again is the retry. (One resting at not_permitted is re-evaluated
// in place instead, behind the same rate limit as any refusal,
// notPermittedRetryInterval.) A burst instance answers at once -- it starts
// into free room or is refused with ErrInstanceNoCapacity /
// ErrInstanceDemandUnknown, a draining one is refused with
// ErrInstanceNoCapacity, and it is never queued behind anything but its own
// start. This is the ensure behind the router's POST /ensure/{model}, which is
// how the gateway adds capacity; a request the router forwards is admitted with
// EnsureServing instead, which never starts a burst instance.
func (m *Manager) EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error) {
	return m.ensure(ctx, cmdEnsure{model: upstreamModel})
}

// EnsureServing is EnsureRunning for a PLAIN request -- one the router forwards
// to the child (a proxied or live request), not an ensure (#340). It is
// EnsureRunning with one exception: a request naming a BURST instance (IsBurst)
// is served only by a process that is already there. A running one serves it,
// and a starting one -- admitted already -- has it wait for that start. In any
// other state (no state at all, stopped, draining, or resting at a failure) the
// request is refused at once with ErrInstanceNoCapacity, and nothing is created
// or started. A burst instance resting at a failure (crashed, start_failed,
// not_permitted) is retired by that refusal: its row is kept only until the
// instance is named again, and retiring it creates and starts nothing. A
// draining one is left to its exit, which retires it.
//
// Burst capacity is started on purpose: the gateway's scaler asks for it with an
// ensure (EnsureRunning, the router's /ensure/{model}) once it has decided that
// one more instance is worth a model load on this host. A plain request names
// m#k only because the gateway saw that instance up a moment ago; when it is not
// up any more (idle-unloaded or crashed since), starting it on the request path
// would add capacity nobody decided on and make the request wait out a cold
// start, while the refusal lets the gateway re-select a running instance at
// once. Instance 0 and the protected instances are baseline capacity -- instance
// 0 is what a request has always started, and a protected instance is wanted up
// anyway -- and behave exactly as in EnsureRunning.
//
// The decision is taken on the owner goroutine in the same turn that resolves
// the address and reads the instance's state (handleEnsure), so no start can
// slip in between the check and the request it lets through.
func (m *Manager) EnsureServing(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error) {
	return m.ensure(ctx, cmdEnsure{model: upstreamModel, serving: true})
}

// ensure is EnsureRunning, EnsureServing and EnsureMember: it posts c, with a
// fresh reply channel, and waits for the answer (cmdEnsure says what each
// field asks for).
func (m *Manager) ensure(ctx context.Context, c cmdEnsure) (endpoint string, release func(), err error) {
	reply := make(chan ensureOutcome, 1)
	c.reply = reply
	select {
	case m.cmds <- c:
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

// Status returns a snapshot of every managed instance's current status: one
// row per instance, each carrying its BASE spec ID, its Instance index and its
// instance address as Model (so a spec that never replicates has its one row
// exactly as before instances existed).
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

// LoadedModels returns the instance address (InstanceAddress: the upstream
// model name for instance 0, "m#k" for instance k) of every instance
// CURRENTLY in StateRunning -- Starting does not count (a cold/loading model
// must not be selected by prefer-loaded routing logic upstream).
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

// ResolveAddress resolves a model name as the router receives it to the base
// upstream model and the instance index it addresses (#340): ok is false for
// anything that is not a managed address. It is the one resolution rule the
// router and the ensures (EnsureRunning, EnsureServing) share
// (owner.resolveAddress) -- one rule in one place, so the name the router hands
// the child and the instance the request is admitted to are never read off two
// rules that could disagree:
//
//   - an EXACT match on a managed upstream model wins first and is instance 0,
//     so a real model whose name merely contains '#' ("foo#2") keeps routing
//     literally, even when "foo" is a replicable spec;
//   - otherwise ParseInstanceAddress splits off an instance index, and the
//     address is managed only when the base is a managed upstream model whose
//     spec allows that instance (InstanceAllowed: replicable, within
//     max_instances and MaxInstanceIndex).
//
// It answers from the owner's current index and creates nothing; a later
// ensure re-resolves on its own, so an Apply landing in between decides that
// ensure's answer (a refusal when the push removed the address).
func (m *Manager) ResolveAddress(model string) (base string, k int, ok bool) {
	reply := make(chan resolvedAddress, 1)
	m.postCmd(cmdResolve{model: model, reply: reply})
	select {
	case r := <-reply:
		return r.base, r.k, r.ok
	case <-m.done:
		return "", 0, false
	}
}

// AppliedETag returns the ETag of the runtime-config document this manager
// has APPLIED -- the value that rides upward as the telemetry sample's
// runtime_config_applied_etag, so a gateway can wait for "you applied MY
// document" instead of guessing with a fixed delay. "" means nothing has
// been applied that carried an ETag, which includes a cold start and the
// driver's own drain (stopAll applies an ETag-less empty config).
//
// WHY THIS IS SERVED FROM THE OWNER LOOP, and not from an atomic field or
// from the Driver. There are three values in this module that could each be
// called "the ETag", and only one of them is true:
//
//   - GatewaySource.etag advances when a document is FETCHED and parsed,
//     strictly BEFORE the driver hands it to Apply. Reporting that one is
//     the exact lie this whole feature must not tell -- the gateway would
//     read "applied" off a document that has reconciled nothing.
//   - A field the Driver set around its Apply call would be honest about
//     ordering but would duplicate state the reconciler already owns, and
//     would need its own synchronization to be read from the sample-building
//     goroutine.
//   - o.cfg.ETag, returned here, is written by owner.applyConfig itself, on
//     the goroutine that owns every piece of state in this package, inside
//     the single event-loop turn that performs the reconciliation. Because
//     owner.run processes one command at a time and nothing re-enters the
//     loop, this command cannot be served midway through an apply: there is
//     no observable state in which this value names a document applyConfig
//     has not processed.
//
// What "applied" therefore means is precise and narrower than it looks: the
// document is the desired state and every reconciliation DECISION it
// implies is committed. Asynchronous consequences may still be in flight --
// a drained child is in StateDraining until its grace/kill sequence
// finishes, and a newly pinned spec may still be starting -- and Status()
// is what reports those. The one guarantee that makes this useful for
// isolation is separate: after the applied document force-stops a spec,
// NOTHING can start it again (both handleEnsure and admitAndStart refuse
// AdminState "force_stopped" outright), so an absence observed after this
// ETag names the document is durable in a way the same absence observed
// before it is not.
func (m *Manager) AppliedETag() string {
	reply := make(chan string, 1)
	m.postCmd(cmdAppliedETag{reply: reply})
	select {
	case s := <-reply:
		return s
	case <-m.done:
		return ""
	}
}

// Transitions returns a channel that receives a signal on any spec state
// change, and whenever a row leaves the state table (deleteState: a burst
// instance retired, a removed spec gone). Buffered(1): a burst of transitions
// between two reads of this channel coalesces into a single pending wake,
// exactly like client.WSSender's certUpdates/trustUpdates doorbells.
func (m *Manager) Transitions() <-chan struct{} {
	return m.transitions
}

// SetMeasurer installs f, which is called with every currently-live managed
// PID to get real per-GPU VRAM usage instead of a spec's static estimate
// (main.go wires this to collector.NewVRAMMeasurer, which picks a
// platform-appropriate one: PDH + D3DKMT on Windows, where nvidia-smi's
// per-process query returns [N/A] under WDDM, and nvidia-smi
// --query-compute-apps everywhere else). f may be nil to go back to estimates
// only, which is also NewManager's default -- measurement is a hardware
// capability, not a negotiated protocol feature, so every AMD, Apple and
// CPU-only host simply never installs one and is entirely unaffected by
// everything below.
//
// f is called from TWO places, and it must be safe for concurrent use because
// they can overlap:
//
//   - the recurring, OFF-owner measurement (dispatchMeasurement), which is
//     what keeps a merely-running spec's measurement current, and
//   - each admission snapshot (buildSnapshot), synchronously on the owner
//     goroutine, so an admission decides on the freshest numbers there are.
//
// f MUST BOUND ITS OWN RUNTIME. Nothing here interrupts it: on the owner it
// stalls every Status() and EnsureRunning for its duration, and off the owner
// it is tracked in the Manager's WaitGroup, so Close waits for it.
//
// How each shipped measurer meets that is worth knowing, because they meet it
// differently and only one of them meets it end to end. The compute-apps
// measurer is entirely subprocess work and puts a 2s context deadline
// (collector.nvidiaMeasureTimeout) over all of it. The Windows PDH measurer
// bounds its one subprocess with the same deadline -- and spawns none at all
// in the steady state -- but its PDH and D3DKMT syscalls, which are what runs
// on every admission, are in-process Win32 calls under NO deadline: there is
// nothing to cancel and no way to interrupt them, so a slow PDH provider
// stalls the owner for however long it takes. That is accepted (a wedged
// performance-counter provider is a broken host, and the alternative is a
// goroutine handing an abandoned buffer to the kernel), not overlooked -- but
// do not read "the shipped measurer is deadline-bounded" off this comment.
func (m *Manager) SetMeasurer(f func(pids []int) map[int]map[int]int) {
	if f == nil {
		m.measurer.Store(nil)
		return
	}
	fn := measurerFunc(f)
	m.measurer.Store(&fn)
}

// SetRAMMeasurer installs f, which is called with every currently-live managed
// PID to get its real resident host RAM in MB (main.go wires this to
// collector.NewRAMMeasurer: RSS off Windows, the private working set on
// Windows). f may be nil to go back to no RAM measurement, which is also
// NewManager's default.
//
// It is SetMeasurer's twin and carries the same obligations, with one
// difference in where it is called from. f is called ONLY from the recurring,
// OFF-owner measurement (dispatchMeasurement) -- never from buildSnapshot,
// which only READS the last stored result (specState.measuredRAM) and so never
// pays a measurement on the admission path -- so it never runs on the owner
// goroutine. It must still be safe for concurrent use (it is stateless in
// practice) and MUST BOUND ITS OWN RUNTIME: it is tracked in the Manager's
// WaitGroup, so Close waits for it.
//
// The answer maps pid -> MB for the pids it could read; a pid it could not
// read is simply absent. A value <= 0 is not a measurement (see realRAM).
func (m *Manager) SetRAMMeasurer(f func(pids []int) map[int]int) {
	if f == nil {
		m.ramMeasurer.Store(nil)
		return
	}
	fn := ramMeasurerFunc(f)
	m.ramMeasurer.Store(&fn)
}

// HostInfo is the latest hardware sample as far as admission is concerned: the
// figures the owner cannot get from a managed process's own measurement.
//
// GPUTotalsMB maps a GPU index (the same index spec.GPUs and GPUBudget use) to
// that device's total memory in MB, as the GPU collector last reported it. An
// index the collector did not report, or reported as 0, is simply absent --
// there is nothing detected for it, which is never the same as "a device with
// no memory".
//
// MemAvailableMB is the host RAM obtainable right now (the sample's
// MemAvailable, not total-minus-used), in MB. 0 means "no reading": either no
// sample has arrived yet or the read failed, and a consumer must treat it as
// unknown rather than as an exhausted host.
type HostInfo struct {
	GPUTotalsMB    map[int]int
	MemAvailableMB int
}

// SetHostInfo records the newest hardware figures for the owner's next
// admission. The agent's collect loop calls it once per sample (about once a
// second): it is a cheap atomic store, and PUSHING the value is the point --
// buildSnapshot runs on the owner on the admission path and must read a cached
// number, never poll hardware or shell out. Each call replaces the previous one
// wholesale.
//
// The manager keeps its own copy of the GPUTotalsMB map. The caller is a
// different goroutine from the owner that reads it, so retaining the caller's
// map would let a caller that goes on to reuse or edit it race that read.
//
// Storing is not the whole job. The host-RAM pool is built from the live
// MemAvailable, so the sample that follows a victim's exit is the one that
// finally shows its freed memory -- and admission is otherwise re-attempted only
// on a release, an exit, an Apply or a finished start, so a start that was
// refused on the PREVIOUS sample would keep waiting for one of those. Every call
// therefore also posts a coalescing, non-blocking wake the owner turns into a
// re-evaluation of exactly the starts a new sample can change (see
// owner.wakeOnHostSample). It never blocks this caller, and costs the owner
// nothing when no such start is waiting.
func (m *Manager) SetHostInfo(hi HostInfo) {
	cp := HostInfo{MemAvailableMB: hi.MemAvailableMB}
	if len(hi.GPUTotalsMB) > 0 {
		cp.GPUTotalsMB = make(map[int]int, len(hi.GPUTotalsMB))
		for i, mb := range hi.GPUTotalsMB {
			cp.GPUTotalsMB[i] = mb
		}
	}
	m.hostInfo.Store(&cp)
	m.hostSeq.Add(1)
	select {
	case m.hostWake <- struct{}{}:
	default: // a wake is already pending; it will see this value too
	}
}

// hostInfo returns the latest pushed hardware figures, or the zero HostInfo
// when nothing has been pushed yet: no detected GPU totals and MemAvailableMB
// 0 (unknown). It is the ONE place the "no sample yet" case is interpreted, so
// every consumer in buildSnapshot reads the same answer. The returned maps are
// shared and read-only -- SetHostInfo replaces the whole value, it never edits
// one in place.
func (o *owner) hostInfo() HostInfo {
	if hi := o.m.hostInfo.Load(); hi != nil {
		return *hi
	}
	return HostInfo{}
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
	// serving is set by EnsureServing: a plain request, which a burst instance
	// serves only while it is running or starting (plainRequestRefused).
	serving bool
	// member is set by EnsureMember: model is then an rpc_server spec ID, and
	// nothing else resolves (memberState). reserve is the reservation the
	// member is given once it is running (armReservation); 0 arms none.
	member  bool
	reserve time.Duration
	// clusterStart is set by EnsureClusterHead: the gateway's cluster start,
	// the one ensure that may start a cold cluster head (headRefusesUnmarked).
	clusterStart bool
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
	// rpcProtocol is the protocol version an rpc_server member announced in its
	// startup banner; deviceMismatch, when set, is the verdict that the banner
	// lists a different number of devices than the spec selects.
	rpcProtocol    string
	deviceMismatch string
	// relayDown, when set, is why a relay member's relay was still not
	// listening at the startup deadline although its child was up.
	relayDown string
}

func (cmdStartResult) isCommand() {}

type cmdBackoffFire struct{ specID string }

func (cmdBackoffFire) isCommand() {}

// cmdGraceExpiry is posted by the owner's single grace timer when the earliest
// within-grace victim's window expires. It carries no spec: the handler re-asks
// every spec blocked solely by the grace window (handleGraceExpiry), exactly as a
// host-sample wake re-asks every RAM waiter. See owner.rescheduleGraceWake.
type cmdGraceExpiry struct{}

func (cmdGraceExpiry) isCommand() {}

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

// cmdResolve asks the owner to resolve a model name (Manager.ResolveAddress).
// A command because byUpstream and the spec table are owner state.
type cmdResolve struct {
	model string
	reply chan resolvedAddress
}

func (cmdResolve) isCommand() {}

// resolvedAddress is cmdResolve's answer.
type resolvedAddress struct {
	base string
	k    int
	ok   bool
}

// cmdAppliedETag asks the owner for o.cfg.ETag. A COMMAND rather than an
// atomic field written by applyConfig, and that is the whole point: only a
// value served from the owner's own event loop is guaranteed to name a
// document the owner has finished reconciling. An atomic set at the top of
// applyConfig would be readable while the reconciliation it belongs to is
// still running -- see Manager.AppliedETag.
type cmdAppliedETag struct{ reply chan string }

func (cmdAppliedETag) isCommand() {}

type cmdClose struct{}

func (cmdClose) isCommand() {}

// measureTarget names one live generation a dispatched measurement was taken
// for. The *runningProc is the generation identity, exactly as everywhere
// else in this file (see the package doc): a pid alone is not enough, because
// the OS is free to hand the same number to something unrelated the moment
// the child exits, and the measurement is in flight for as long as a
// subprocess takes to answer.
type measureTarget struct {
	specID string
	proc   *runningProc
	pid    int
	// ready is whether proc had already passed a health probe when this
	// measurement was dispatched. Every reading the measurement takes happens
	// after the dispatch, so a ready target's reading is of the loaded model;
	// one dispatched while the process was still loading may have caught the
	// load at any point, even if the process is healthy by the time the answer
	// lands. Only a ready reading is one a sibling inherits (specState.readyVRAM).
	ready bool
	// wslUnit and wslDistro are proc's (runningProc.wslUnit/wslDistro): set
	// for a wsl generation, whose RAM is its scope's figure in the sampling
	// call (cmdMeasured.ramByUnit) and whose pid -- wsl.exe, a thin client --
	// reaches no native measurer. Both empty for a native process.
	wslUnit, wslDistro string
}

// cmdMeasured carries a completed off-owner measurement back to the owner.
// byPID is exactly what the installed VRAM measurer returned (pid -> gpu index
// -> MB) and ramByPID what the installed RAM measurer returned (pid -> MB);
// either is nil when that measurer is not installed, was not consulted this
// cycle, or answered nothing. ramByUnit and wslVM are the WSL sampler's answer
// (sampleWSL): each wsl target's scope figure by unit name (MB), and the VM's
// meminfo, nil when there was no wsl target or no distro answered.
type cmdMeasured struct {
	targets   []measureTarget
	byPID     map[int]map[int]int
	ramByPID  map[int]int
	ramByUnit map[string]int
	wslVM     *wslVMInfo
}

func (cmdMeasured) isCommand() {}

// cmdWSLUIDResolved carries the answer of one off-owner UID lookup
// (requestWSLUID) back to the owner: the configured user's UID inside distro,
// or why it could not be had.
type cmdWSLUIDResolved struct {
	distro string
	uid    int
	err    error
}

func (cmdWSLUIDResolved) isCommand() {}

// ensureOutcome is what an EnsureRunning caller eventually receives.
type ensureOutcome struct {
	endpoint string
	specID   string
	proc     *runningProc
	err      error
}

// ensureWaiter is one queued EnsureRunning request. timer is non-nil only
// when the spec's AdmissionWaitTimeoutSeconds is positive; 0 means "wait
// until the caller's own ctx is done"
// (docs/architecture/cross-cutting/agent-runtime-manager.md §5.4), so no
// timer is scheduled.
type ensureWaiter struct {
	reply chan ensureOutcome
	timer *time.Timer
	// queuedAt is when handleEnsure queued this waiter. It orders admission
	// retries oldest-first (wakeAdmissionCandidates), so a spec whose
	// request has been waiting longest gets the freed slot rather than
	// whichever spec Go's randomized map iteration happened to visit first.
	queuedAt time.Time
	// reserve is cmdEnsure.reserve, armed when this waiter is served.
	reserve time.Duration
	// clusterStart is cmdEnsure.clusterStart: this waiter wants a cold cluster
	// head started (specState.wantsStart).
	clusterStart bool
}

// runningProc is one live generation of a managed process. Its pointer
// identity IS the generation -- see the package doc's "Generation identity"
// section. Fields are set once at start (except the three timers, which are
// scheduled/cancelled as the generation's lifecycle progresses) and never
// touched by anything except the owner goroutine.
type runningProc struct {
	specID string
	cmd    *exec.Cmd
	pid    int
	port   int
	// log is THIS generation's writer into the spec-scoped output store
	// (logs.go). It is not the buffer: the buffer belongs to the SPEC and
	// deliberately outlives this process, so opening the log view on a
	// `crashed` row still shows what led to the crash.
	log       *procLog
	startedAt time.Time
	// exited is closed by waitForExit right before it reports the exit to
	// the owner, so pollHealth can select on it (a process that dies before
	// becoming healthy stops polling immediately instead of waiting out a
	// full healthPollInterval tick).
	exited chan struct{}

	// everHealthy is set true (by handleStartResult's success branch) the
	// first and only time this generation ever passes a health probe.
	// onProcExited (I4 fix) uses this -- not the spec's CURRENT display
	// state, which may already be Draining -- to classify an unexpected
	// exit as a crash (was genuinely healthy) vs. a start failure (never
	// became healthy at all).
	everHealthy bool

	drainTimer  *time.Timer // pending "drain grace exceeded, terminate anyway" callback
	killTimer   *time.Timer // pending "kill grace exceeded, SIGKILL" callback
	stableTimer *time.Timer // pending "this run has been stable long enough" callback

	// ctl signals this generation (terminate, then kill): nativeControl for a
	// process the agent exec'd directly, wslControl for a wsl spec, whose
	// workload is reached through its systemd scope. nil reads as nativeControl
	// (control), so a runningProc built without one behaves as before.
	ctl procControl
	// wslUnit and wslDistro name a wsl generation's scope (wslUnitName) and the
	// distro it runs in; both empty for a native process. wslUnit carries the
	// generation, so it identifies exactly this process inside the distro.
	wslUnit   string
	wslDistro string
}

// control is p.ctl, or the native control for a runningProc built without one.
func (p *runningProc) control() procControl {
	if p.ctl == nil {
		return nativeControl{cmd: p.cmd}
	}
	return p.ctl
}

// specState is one INSTANCE's complete owner-side bookkeeping (#340): a spec
// that never replicates has exactly one, keyed by its spec ID; a replicable
// spec has one per instance that exists, keyed by InstanceKey.
type specState struct {
	// spec is what this instance launches and is admitted as: the pushed spec
	// itself for instance 0, and for k >= 1 the per-instance copy
	// specForInstance derives from it (ID = instance key, InstanceOf = base ID,
	// Instance = k; every pushed field identical). applyConfig re-derives the
	// copy from each new push, so a copy never drifts from its base -- and two
	// copies of one unchanged base are DeepEqual, which is what keeps an
	// unchanged push from restarting instances.
	spec Spec

	// rpcProtocol is the RPC protocol version the running rpc_server member
	// announced in its startup banner; empty when none was seen.
	rpcProtocol string
	// bindMismatch: this member's bind address was not an address of this
	// host at its last start attempt, or its relay never listened. Cleared
	// by the next start that passes the check.
	bindMismatch bool

	// terminalSpec is a snapshot of the spec as it was when a cluster process
	// earned a terminal verdict (terminalCode): retrying the same spec cannot
	// end differently, so admission, wakes and ensures leave it resting at
	// not_permitted until the spec itself changes. Nil when there is none.
	terminalSpec *Spec
	terminalCode string

	// instance is this state's instance index k: 0 for the spec's own process
	// (and for every spec that never replicates), k >= 1 for a further
	// instance. Fixed for the state's lifetime; spec.Instance carries the same
	// number.
	instance int

	// startupMS is the wall time, in milliseconds, from this instance's most
	// recent SUCCESSFUL start (exec) to its first passing health probe -- the
	// measured cold-start figure Status.StartupMS reports. 0 = unknown:
	// never started successfully. It describes the last successful start, not
	// the live process, so unlike the measurements it survives the exit.
	startupMS int

	// waitingOnDemand is true while this BASELINE instance k >= 1 is held back
	// because its demand is still unknown after inheriting its siblings' ready
	// measurements: an instance k >= 1 is never admitted blind, so it waits for
	// a sibling to be measured once ready. A ready measurement landing is the
	// event that can end that wait and nothing else announces it, so
	// applyMeasurement re-asks exactly these instances (wantsDemandWake).
	// admitAndStart rewrites it on every verdict, like waitingOnRAM, so it never
	// outlives the wait.
	waitingOnDemand bool

	// waitingOnSibling is true while this PROTECTED instance k >= 1 is held
	// back because a lower-index sibling still waits to start (lowerSiblingWaiting):
	// the protected instances of a spec start lowest index first. It is what
	// wakeSiblingsWaitingFor selects when that sibling gets its process, so the
	// two load side by side rather than one after the other. admitAndStart
	// rewrites it on every verdict, like waitingOnDemand.
	waitingOnSibling bool

	// waitingOnWSLUID is true while this wsl instance is held back because the
	// configured user's UID inside its distro is not known yet: the lookup runs
	// off the owner (requestWSLUID), and its answer re-asks exactly these
	// instances (handleWSLUIDResolved) or, if it failed, fails their start.
	// admitAndStart rewrites it on every verdict, like waitingOnSibling.
	waitingOnWSLUID bool

	// decidedTurn is the owner turn (owner.turn) of this instance's latest FULL
	// admission decision -- one that went on to build a snapshot, whatever its
	// verdict -- and 0 once a later ask only held it back behind a lower
	// sibling (waitingOnSibling), which decides nothing. reask reads it to
	// skip a decision this very turn has already taken.
	decidedTurn uint64

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

	// backoffOnExit records an enterBackoff call that arrived while this
	// spec's process was still alive (the start-timeout path: terminateNow
	// has signalled it, but the exit is reported later). onProcExited
	// consumes it and enters the backoff then, so StateBackoff never
	// coexists with a live process and the delay is measured from the moment
	// the spec actually has nothing running -- see enterBackoff.
	backoffOnExit bool

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

	// measuredVRAM is the last real per-GPU measurement observed for THIS
	// SPEC'S CURRENT PROCESS, from whichever spec's admission snapshot was
	// built most recently (buildSnapshot). Reported via Status; nil when
	// nothing has been measured -- which is the permanent state of every
	// host with no measurer installed.
	//
	// CLEARED BY onProcExited, and the claim in the first sentence is why:
	// it describes one process, and it stops being true the instant that
	// process exits. Leaving it in place (the F2 defect) meant a
	// force-stopped spec kept reporting a dead process's measurement in
	// every Status(), the agent kept attaching `gpus` to every telemetry
	// sample, and the gateway kept writing the same value back once per
	// second per (spec, gpu) forever -- on a server doing nothing at all.
	measuredVRAM map[int]int

	// measuredRAM is the last real resident host-RAM measurement (MB) of THIS
	// SPEC'S CURRENT PROCESS, from the recurring measurement beat
	// (applyMeasurement). 0 means UNKNOWN -- never measured, no RAM measurer
	// installed, or only non-measurements (<= 0, see realRAM) seen -- and is
	// never "this process needs no RAM". Reported via Status.MeasuredRAMMB and
	// read by buildSnapshot, where it is floored at the spec's pushed demand
	// to become the process's RunningProc.RAMMB.
	//
	// ONE PROCESS PER INSTANCE, so this is a scalar: there is nothing to sum,
	// and unlike measuredVRAM there is no per-GPU key to re-key. Cleared by
	// onProcExited for exactly the reason measuredVRAM is: it describes a
	// process, and stops being true when that process exits.
	measuredRAM int

	// readyVRAM and readyRAM are the latest real measurements of THIS
	// instance's current process that were taken after it first passed a
	// health probe: what the loaded model holds, not a load in progress. They
	// are the only figures a sibling inherits (siblingPeak, #340), because
	// measuredVRAM/measuredRAM above may hold a reading caught mid-load --
	// buildSnapshot and the housekeeping beat measure a starting process too. That reading is right for charging the loading process itself
	// (max(live, demand) only ever raises its own charge), but it is a fraction
	// of what another copy of the model will hold: inherited, it would make a
	// further instance's unknown demand count as known and admit it against that
	// fraction. A reading is ready when the process had passed its probe before
	// the reading was taken: at dispatch for the off-owner beat
	// (measureTarget.ready), at the call for buildSnapshot's synchronous VRAM
	// read. Cleared by onProcExited with the measurements themselves, for the
	// same reason. readyVRAM may share its map with measuredVRAM; nothing
	// writes through either.
	readyVRAM map[int]int
	readyRAM  int

	// vramBootstrapDone is the DURABLE guard on the one-shot VRAM bootstrap
	// eviction (task 4.3). It means the spec's unknown-VRAM bootstrap is resolved
	// -- either a real VRAM measurement has landed for it at least once this
	// lifetime (so it has a number now and never needs the shot), or the owner has
	// already spent the one guarded eviction on it. While it is false AND the
	// spec's declared VRAM demand is unknown, vramBootstrapEligible offers the
	// shot; once it is true the spec is back to pure ADR-032 steady state (rule 4
	// blocks, never evicts a known-demand occupant).
	//
	// Set in exactly two places: wherever a real measurement is recorded
	// (applyMeasurement and buildSnapshot, beside st.measuredVRAM), and in
	// admitAndStart when Admit returns a bootstrap eviction (Decision.VRAMBootstrap).
	//
	// IT IS DELIBERATELY NOT CLEARED ON EXIT, and that is the whole anti-thrash
	// guarantee -- the opposite rule from measuredVRAM/measuredRAM above, which
	// describe the live process and so are cleared the moment it exits. This
	// marker describes the SPEC's lifetime, not its current process: a shot once
	// spent must stay spent across restarts, or every exit would re-arm the
	// one-shot and the ADR-032 bidirectional thrash would return one restart at a
	// time. It resets only when the spec itself is removed and re-added (a fresh
	// specState); an in-place config edit keeps the same specState and the marker.
	vramBootstrapDone bool

	// ramBootstrapRetries is the anti-storm counter for the cold-start RAM
	// bootstrap's memory-pressure escalation (task 4.4). It counts the guarded
	// evict-and-retry escalations THIS spec has spent on a stuck bootstrap, and is
	// bounded by ramBootstrapMaxRetries: the first memory-pressure death
	// increments it from 0 to 1 and escalates (evict an idle process, retry once);
	// a death while it is already at the ceiling does NOT escalate again (no
	// further eviction) and goes straight to StateStartFailed.
	//
	// It is RESET TO 0 ONLY BY A SUCCESSFUL RAM MEASUREMENT (applyMeasurement),
	// which is the signal that the bootstrap resolved -- the RAM twin of
	// vramBootstrapDone's resolve-on-measurement rule. That is what lets a later,
	// unrelated pressure event earn the one shot again while still guaranteeing a
	// bootstrap that keeps dying before it is ever measured escalates at most
	// once. It is owner-goroutine state (§5.5), never cleared on exit: a stuck
	// bootstrap's streak must survive its own restarts, exactly like
	// vramBootstrapDone and unlike the per-process measurement.
	ramBootstrapRetries int

	// ramBootstrapBeats is the anti-hang counter for the TERMINATION case (task
	// 4.4). A bootstrap that comes up HEALTHY but is never measured would hold the
	// RAMBootstrapInFlight interlock (task 4.1) forever, and that interlock makes
	// every other RAM admission on the host Wait. applyMeasurement advances this
	// once per fruitless measurement beat while a RAM measurer is installed and
	// the spec is still a healthy, unmeasured bootstrap; at ramBootstrapMeasureBeats
	// the owner forces the spec to StateStartFailed so the interlock is bounded. A
	// real measurement resets it to 0. Owner-goroutine state, like the counter above.
	ramBootstrapBeats int

	// waitingOnRAM is true while this spec's most recent admission verdict was a
	// Wait the host-RAM pool contributed to (Decision.WaitOnRAM). It is the one
	// kind of Wait a new hardware sample can end by itself, so it is what
	// wakeOnHostSample selects on: a start waiting on the matrix, the process
	// limit or a VRAM budget is never re-asked because a sample landed.
	// admitAndStart rewrites it on every verdict, so it never outlives the Wait
	// it describes.
	waitingOnRAM bool

	// waitingOnGrace is true while this spec's most recent admission verdict was a
	// Wait blocked SOLELY by a within-grace victim (Decision.WaitOnGrace). It is the
	// grace twin of waitingOnRAM: a Wait that resolves with nothing happening to any
	// process, this time by the mere passing of time as the earliest within-grace
	// victim's window expires. It is the set wantsGraceWake selects, so only such a
	// spec is re-asked by the grace-expiry wake (owner.rescheduleGraceWake /
	// handleGraceExpiry) -- a start waiting on the matrix, a busy neighbour, the
	// process limit or a VRAM/RAM budget is never woken by a grace expiry.
	// admitAndStart rewrites it on every verdict, like waitingOnRAM, so it never
	// outlives the Wait it describes.
	waitingOnGrace bool

	// notPermittedAt is stamped to now() every time admitAndStart (re)confirms
	// StateNotPermitted or StatePendingVRAMUnknown -- deliberately NOT the
	// same as `since`, because setState is a no-op (and so never touches
	// `since`) when re-entering a state the spec is already in, but a
	// rate-limit window needs to restart on every re-confirmation, not just
	// the first one. handleEnsure compares this against
	// notPermittedRetryInterval (fix round 2 / R2-1).
	notPermittedAt time.Time

	// drainedFor names the specs whose QUEUED REQUESTS this generation was
	// drain-stopped for -- set by admitAndStart when Admit picks this spec
	// as an eviction victim, consumed by onProcExited, and empty for every
	// other kind of stop (idle unload, config removal, force_stopped,
	// shutdown).
	//
	// It exists to give the beneficiary first refusal on the slot its own
	// eviction just freed. Without it the victim re-admits itself first
	// (onProcExited calls admitAndStart for the spec that just exited before
	// waking anyone else), so under sustained traffic for the victim's model
	// the spec that paid for the eviction can be starved indefinitely --
	// each cycle costing a full model load. Reordering alone would not do:
	// the wake loop would still reach the victim in randomized map order.
	drainedFor []string

	pending []*ensureWaiter

	// claim is an rpc_server member's hold and reservation (rpc_hold.go): while
	// either stands the member is never idle-unloaded nor an eviction victim.
	claim memberClaim
	// relaunchDeferred marks a claimed member whose launch shape changed under
	// its running process: it is relaunched once the claim ends (claimEnded),
	// or by the next ensure that finds no head holding it (handleEnsure),
	// since killing it while held would abort the head that uses it. Cleared
	// when the process exits.
	relaunchDeferred bool
	// admissionBlockedBy names the claimed member a waiting admission of this
	// spec could only proceed by evicting; empty otherwise. admitAndStart
	// decides it on every ask, and it is dropped once nothing waits any more.
	admissionBlockedBy string
}

// owner holds every piece of state the single serialized goroutine touches.
type owner struct {
	m      *Manager
	policy LocalPolicy
	getenv func(string) string
	// gpuVendor is fixed for the process lifetime (the host's hardware does
	// not change under a running agent), so it is a plain field read by the
	// owner goroutine and never written after construction.
	gpuVendor GPUVendor
	// gpuVendors is every vendor the host's GPU detection found, in precedence
	// order. Only the generic device tokens' "auto" backend reads it; the
	// single gpuVendor above keeps driving visibility and budgets.
	gpuVendors GPUVendors
	// specsFromLocalFile is ManagerOptions.SpecsFromLocalFile -- fixed for
	// the process lifetime, so a plain field like gpuVendor above. Its only
	// use is the masking rule for the reported launch command; see the option's
	// own doc.
	specsFromLocalFile bool
	// configFilesDir is ManagerOptions.ConfigFilesDir -- the base directory
	// the owner writes a spec's config files under. Fixed for the process
	// lifetime, so a plain field like gpuVendor above.
	configFilesDir string
	// lastCacheSweep is when the last sweep of the rpc cache root started
	// (maybeSweepMemberCaches); zero before the first document.
	lastCacheSweep time.Time
	// cacheSweeper runs those sweeps; created by the first.
	cacheSweeper *memberCacheSweeper
	// vramHeadroomMB is ManagerOptions.VRAMHeadroomMB, never negative. Fixed
	// for the process lifetime, so a plain field like gpuVendor above; read
	// only by buildSnapshot's auto-detected budgets.
	vramHeadroomMB int
	// hostRAMReserveMB is ManagerOptions.HostRAMReserveMB, never negative.
	// Fixed for the process lifetime, so a plain field like vramHeadroomMB
	// above; read only by buildSnapshot's host-RAM budget.
	hostRAMReserveMB int
	// ramBootstrapGuessMB is ManagerOptions.RAMBootstrapGuessMB, never negative.
	// Fixed for the process lifetime, so a plain field like hostRAMReserveMB
	// above; read only by buildSnapshot, which copies it into the snapshot's
	// RAMBootstrapGuessMB for Admit (the one place that interprets it).
	ramBootstrapGuessMB int
	// evictionGrace is ManagerOptions.EvictionGrace, never negative. Fixed for
	// the process lifetime, so a plain field like ramBootstrapGuessMB above; read
	// by buildSnapshot (copied into the snapshot's EvictionGrace for Admit, the
	// one place that interprets it) and by the bootstrap escalation's own victim
	// pick, so the two never disagree about who is protected.
	evictionGrace time.Duration
	// wslRunner is ManagerOptions.WSLRunner (execWSLRunner when unset): every
	// helper call into a distro goes through it. Fixed for the process
	// lifetime; copied into each wslControl and the orphan sweep.
	wslRunner wslRunner
	// wslNonce is this manager's run identity in WSL unit names (wslUnitName):
	// 8 hex characters from crypto/rand, drawn once in NewManager. The orphan
	// sweep kills only units whose nonce differs, which is what makes it safe to
	// run while this run's own units are starting.
	wslNonce string
	// wslRAMReserveMB is ManagerOptions.WSLRAMReserveMB, never negative. Fixed
	// for the process lifetime, like hostRAMReserveMB; read by the WSL VM RAM
	// pool (wslRAMBudgetMB, wslRAMCeilingMB).
	wslRAMReserveMB int
	// wslVM is the WSL VM's meminfo from the latest sampling call that
	// answered (applyMeasurement), nil until one has; wslVMHeldMB is what the
	// agent's own scopes held in that same sample, summed. Together they are
	// the VM RAM pool (wslRAMBudgetMB), and they change only together, when a
	// new sample lands -- never when a process starts or exits, see
	// wslRAMBudgetMB for why. Owner-goroutine state, like specs.
	wslVM       *wslVMInfo
	wslVMHeldMB int
	// wslUIDs is the configured user's UID per distro, owner-goroutine state
	// like specs: written only when an off-owner lookup's answer is handled
	// (handleWSLUIDResolved), read by admitAndStart and startProcess. Only
	// answers are kept -- a user's UID does not change under a running agent --
	// so a failed lookup is asked again by the next start. A reader off the
	// owner gets a copy of the value it needs, never the map.
	wslUIDs map[string]int
	// wslUIDInFlight names the distros whose lookup is running, so a burst of
	// starts asks each distro once (requestWSLUID).
	wslUIDInFlight map[string]bool
	// wslSweepDue names the distros the orphan sweep has yet to visit: set at
	// the first applyConfig, cleared as each distro's first UID answer starts
	// its sweep, so a distro that cannot be reached at first is swept once it
	// can.
	wslSweepDue map[string]bool
	// wslWarmed is set by the first applyConfig, which starts the UID lookups
	// and the orphan sweep of the manager's lifetime (maybeWarmWSL).
	wslWarmed bool
	// ramFreedMB is the host RAM that managed processes held when they exited
	// and that the hardware sample has not necessarily caught up with yet, valid
	// while hostSeq is below ramFreedUntilSeq (see noteRAMFreed). It only ever
	// loosens the terminal own-demand refusal's ceiling, never the admission
	// arithmetic. Owner-goroutine state, like specs.
	ramFreedMB       int
	ramFreedUntilSeq uint64
	// logs is m.logs, held here so the owner can open a generation's writer
	// and prune the retention of specs a new config removed. Carries its own
	// mutex; nothing about it is owner-goroutine-confined.
	logs *LogStore

	cfg          Config
	allowedPairs map[[2]string]bool
	specs        map[string]*specState
	byUpstream   map[string]string // upstream_model -> spec ID

	// graceTimer is a single owner-wide timer that fires when the EARLIEST
	// within-grace victim's window is about to expire, so a start blocked solely by
	// the grace window (specState.waitingOnGrace) is re-asked the moment its victim
	// becomes evictable -- the one wake that no release, exit, sample or Apply
	// provides (see Decision.WaitOnGrace). It is the grace analogue of the RAM
	// waiter's hostWake, except that grace has no external beat, so the owner
	// schedules the wake itself (rescheduleGraceWake). One timer, not one per spec:
	// every grace-blocked start's next chance is the same earliest expiry, so a
	// single timer wakes them all and cannot storm. graceTimerAt is the instant it
	// is currently set for (zero when none), used to avoid rescheduling to an
	// instant it is already armed for. Owner-goroutine state; cancelled by
	// handleClose so it never outlives the owner (m.wg / Close).
	graceTimer   *time.Timer
	graceTimerAt time.Time

	closing bool // true from the moment cmdClose is received
	stopped bool // true once every live process has been confirmed gone during close

	// measuring is true from the moment dispatchMeasurement hands a
	// measurement to its own goroutine until the matching cmdMeasured is
	// handled. At most one measurement is ever outstanding: a measurer
	// slower than the beat must not accumulate goroutines and subprocesses,
	// and a skipped beat costs nothing but freshness.
	measuring bool

	// remeasure records that a process became healthy while a measurement was
	// already out (measuring), so the measurement its first health probe asks
	// for (handleStartResult) could not be dispatched. applyMeasurement
	// dispatches it as soon as the outstanding one returns, instead of leaving
	// the new process unmeasured for a whole housekeeping beat: a burst
	// instance inherits its siblings' measured demand, and a fresh sibling's
	// figure is what lets the next one start. Only a start sets it -- a
	// skipped housekeeping beat stays skipped -- so a measurer slower than the
	// beat still never runs back to back.
	remeasure bool

	// turn numbers the owner's turns -- one per command handled, housekeeping
	// beat or host-sample wake -- from 1. A full admission decision records the
	// turn it was taken in (specState.decidedTurn), and a loop that asks a list
	// of candidates it collected before asking any of them skips one whose
	// decision from this very turn still stands (reask). Nothing an admission
	// reads moves within a turn except through the turn's own decisions -- an
	// exit, a measurement, a request and a host sample each arrive as a turn
	// of their own -- and those only take room (a start) or begin to free it
	// (a drain, which frees it at the exit), so asking again would repeat the
	// decision, and each one builds a snapshot, which calls the VRAM measurer
	// (a subprocess on a real host). 0 is an owner driven by hand rather than
	// by run (the package's owner-level tests): no decision is fresh then, and
	// every ask is a full one.
	turn uint64
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
			o.turn++
			if !o.closing {
				o.scanIdle()
				o.dispatchMeasurement()
				o.sweepTick()
			}
		case <-o.m.hostWake:
			o.turn++
			if !o.closing {
				o.wakeOnHostSample()
			}
		}
		if o.stopped {
			return
		}
	}
}

func (o *owner) handle(cmd command) {
	o.turn++ // every command is a turn of its own (owner.turn)
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
	case cmdGraceExpiry:
		o.handleGraceExpiry()
	case cmdStableRun:
		o.handleStableRun(c)
	case cmdDrainGraceExpired:
		o.handleDrainGraceExpired(c)
	case cmdKillGraceExpired:
		o.handleKillGraceExpired(c)
	case cmdMeasured:
		o.applyMeasurement(c)
	case cmdWSLUIDResolved:
		o.handleWSLUIDResolved(c)
	case cmdStatus:
		c.reply <- o.snapshotStatus()
	case cmdLoadedModels:
		c.reply <- o.loadedModels()
	case cmdStop:
		c.reply <- o.handleStop(c)
	case cmdStopMember:
		c.reply <- o.stopMember(c.specID, c.force)
	case cmdReleaseMember:
		c.reply <- o.releaseMember(c.specID)
	case cmdSetMemberHold:
		o.setMemberHold(c)
	case cmdReservationExpired:
		o.handleReservationExpired(c)
	case cmdPurgeMemberCache:
		c.reply <- o.purgeMemberCache(c.specID)
	case cmdClusterHeadInfo:
		c.reply <- o.clusterHeadInfo(c.model)
	case cmdResolve:
		_, base, k, ok := o.resolveAddress(c.model)
		c.reply <- resolvedAddress{base: base, k: k, ok: ok}
	case cmdAppliedETag:
		c.reply <- o.cfg.ETag
	case cmdClose:
		o.handleClose()
	}
	if o.closing {
		o.maybeFinishClose()
	}
}

// applyConfig reconciles owner state to cfg. See Manager.Apply's doc for the
// externally-visible contract.
//
// Instances (#340) are reconciled alongside their spec:
//
//   - every spec in cfg has its instance 0 (created here when the spec is new)
//     and its protected instances 1..P-1 (ProtectedCount), which are created
//     here and started below like a pinned spec;
//   - a further instance that already exists -- created earlier on demand --
//     is reconciled exactly like instance 0, against the per-instance copy
//     re-derived from the new push (so a launch-shape change drains and
//     relaunches every instance, and an unchanged push touches none);
//   - an instance the new push no longer allows (replicable turned off,
//     max_instances lowered) drains and is removed like a removed spec, its
//     in-flight requests finishing first; a removed spec takes all of its
//     instances with it;
//   - an instance that can no longer be reached because its address is now
//     another spec's literal upstream model (instanceAddressHolder) drains and
//     is removed the same way, and the protected floor never creates one;
//   - a BURST instance with no process left -- one resting at a failure,
//     kept for the operator only until the next Apply -- is retired here;
//   - a spec that is pushed again while a removal is still draining it is
//     desired again: none of its instances is deleted on exit any more (the
//     drain in progress completes, and each instance then follows the normal
//     rules -- stopped, restarted when protected or asked for, retired when
//     burst).
//
// Order matters in two places. The routing index (rebuildUpstreamIndex) is
// rebuilt once the removed specs are out and BEFORE the further instances and
// the floor are reconciled, because which addresses are another spec's literal
// model is read off that index. And the protected instances are asked in
// instance order (sortInstanceOrder), instance 0 of a spec first: when the room
// holds fewer instances than the floor asks for, the ones it holds are the
// lowest, never a further instance that instance 0 could not get the room back
// from (a sibling is never a victim: evicting a warm copy of a model to start a
// cold one frees nothing). Asked in that order, each further instance finds its
// lower siblings already started; admitAndStart's start-order rule
// (lowerSiblingWaiting) is what keeps the order on the paths that ask in event
// order instead, here included.
//
// A spec whose ID is reserved for instance keys (reservedSpecID: it contains
// '#') is ignored with a Warn, as ParseConfig already drops it from every
// document a Source parses; this covers a Config handed to Apply directly, so
// no spec ID in the owner's table ever collides with an instance key.
func (o *owner) applyConfig(cfg Config) {
	cfg.Specs = withoutReservedSpecIDs(cfg.Specs)
	// The ETag names the document, and the multi-instance opt-in is the one
	// part of it this agent may withhold on its own (Driver.Sync clears
	// Replicable until the gateway declares runtime_instances), so a document
	// can arrive a second time under the same ETag with only that opt-in
	// changed -- when the gateway's declaration changes. That second arrival is
	// a different desired state and must be applied, not short-circuited.
	if cfg.ETag != "" && cfg.ETag == o.cfg.ETag && sameReplicableOptIn(cfg.Specs, o.cfg.Specs) {
		return
	}
	o.cfg = cfg
	o.allowedPairs = cfg.AllowedPairs()

	// A member's cache belongs to the head it serves: remember who served which
	// head before this document, to drop the cache of a head a member no
	// longer serves. Caches of members that left the document are the sweep's.
	memberHeads := make(map[string]string)
	for id, st := range o.specs {
		if st.instance == 0 && st.spec.IsRPCServer() {
			memberHeads[id] = st.spec.RPC.HeadSpecID
		}
	}
	defer o.purgeChangedHeadCaches(memberHeads, cfg.Specs)
	defer o.maybeSweepMemberCaches(cfg.Specs)
	// The first document starts the UID lookups for every allowed distro, off
	// the owner, before anything in it is admitted -- so its wsl starts wait
	// for those lookups instead of asking again -- and each answer starts that
	// distro's sweep of the units an earlier agent run left behind. Starts this
	// document triggers run concurrently with the sweep; the nonce keeps their
	// units out of its reach.
	o.maybeWarmWSL()

	newIDs := make(map[string]bool, len(cfg.Specs))
	bases := make(map[string]Spec, len(cfg.Specs))
	for _, spec := range cfg.Specs {
		newIDs[spec.ID] = true
		bases[spec.ID] = spec
		st, exists := o.specs[spec.ID]
		if !exists {
			o.specs[spec.ID] = &specState{spec: spec, state: StateStopped, since: time.Now()}
			continue
		}
		// Pushed again while a removal was still draining it: desired again,
		// so it must not be deleted on exit -- left set, instance 0 would vanish
		// from the table (and the routing index with it) while its further
		// instances, cleared below, kept running unreachable. The drain in
		// progress completes; the exit then rests it at Stopped and re-admits
		// it like any other.
		st.removed = false
		o.reconcileSpec(st, spec)
	}

	// A spec gone from the config takes every one of its instances with it.
	// Keyed by the BASE spec, so a further instance of an absent spec leaves by
	// this same path: drained if it is serving, its in-flight requests finishing
	// first, and deleted on exit (removeState).
	for id, st := range o.specs {
		if newIDs[specBase(st.spec)] {
			continue
		}
		o.removeState(id, st)
	}

	// The literal model names as resolveAddress sees them from now on. Rebuilt
	// before any further instance is looked at, because an instance whose
	// address is one of them cannot be reached (instanceAddressHolder) -- and
	// it only indexes instance 0, so nothing below changes it.
	o.rebuildUpstreamIndex()

	// Further instances that already exist, against their spec's new push.
	// Deleting the entry being visited is safe in a Go range.
	for id, st := range o.specs {
		if st.instance == 0 {
			continue
		}
		base, desired := bases[st.spec.InstanceOf]
		if !desired {
			continue // the whole spec is gone: removed above, still draining
		}
		if !InstanceAllowed(base, st.instance) {
			// Not "drop it now": a disallowed instance may be serving, and a
			// push must never cut a request off mid-response. removeState
			// drains a live one (in-flight requests finish, bounded by
			// drainGrace) and deletes it on exit.
			o.removeState(id, st)
			continue
		}
		if holder, held := o.instanceAddressHolder(base, st.instance); held {
			// The push made this instance's address another spec's literal
			// model, so from now on every request for it reaches that spec (an
			// exact upstream match wins over an instance address,
			// resolveAddress): the instance is out of reach, and leaves the way
			// a disallowed one does, draining first.
			if !st.removed {
				slog.Warn("runtime: an instance address of a replicable spec is now another spec's upstream model, which it routes to; that instance drains and is removed",
					"spec", base.ID, "instance", st.instance, "address", InstanceAddress(base.UpstreamModel, st.instance), "conflicting_spec", holder)
			}
			o.removeState(id, st)
			continue
		}
		// Allowed again (a cap raised back while it was still draining): it
		// is no longer on its way out.
		st.removed = false
		if IsBurst(base, st.instance) && st.proc == nil {
			// Burst capacity is never retried, so a crash-loop wait is
			// meaningless for it -- reachable only for an instance that was
			// PROTECTED when it crashed and is burst under this push.
			cancelTimer(o.m, &st.backoffTimer)
			if st.state == StateBackoff {
				o.setState(st, StateStopped)
			}
			if len(st.pending) == 0 {
				o.retire(id, st) // see retireIfSpent
				continue
			}
			// A queued request is re-asked by the wake below, as the burst
			// instance this push makes it: started into free room, or refused.
		}
		o.reconcileSpec(st, specForInstance(base, st.instance))
	}

	// The protected floor: instances 1..P-1 always exist (the start loop below
	// brings them up).
	o.ensureProtectedFloor()

	// Retained output follows the SPEC, so a spec the operator deleted takes
	// its history with it: there is no row left in the portal to open a log
	// view on, and the buffer would otherwise be memory held for something
	// that no longer exists. Driven from newIDs (the DESIRED set), not from
	// o.specs, because a removed-but-still-draining spec is still in o.specs
	// and its output stops being reachable the moment the spec is gone. Spec
	// IDs only, for a replicable spec too: every instance writes into its
	// spec's one buffer (LogStore.newInstanceProc), so there is no
	// per-instance buffer to keep or release.
	retained := make([]string, 0, len(newIDs))
	for id := range newIDs {
		retained = append(retained, id)
	}
	o.logs.Retain(retained)

	var start []string
	for id, st := range o.specs {
		if st.removed || !newIDs[specBase(st.spec)] {
			continue
		}
		if st.spec.AdminState == "force_stopped" {
			if st.proc != nil {
				o.beginDrain(id)
			}
			continue
		}
		// protected() is the pre-instance "pinned || force_running" for
		// instance 0 of a spec that never replicates, and folds the
		// min_instances floor in for a replicable one. A protected cluster
		// head is asked too and stays down (wantsUp): only the gateway's
		// cluster start brings one up.
		if st.protected() && st.proc == nil && st.state != StateBackoff {
			start = append(start, id)
		}
	}
	// Never in map order: see the order note in this function's doc. An
	// instance a cascade from an earlier one has already decided is not asked
	// again (reask).
	o.sortInstanceOrder(start)
	for _, id := range start {
		o.reask(id)
	}
	o.wakeAdmissionCandidates()
}

// ensureProtectedFloor creates every protected instance 1..P-1 (ProtectedCount)
// of every desired spec that does not exist yet, Stopped -- the Apply start
// loop and the admission wake bring it up from there. Called from applyConfig,
// and when a removed state leaves the table (onProcExited), because that can
// free an address the floor had to skip.
//
// Bounded by InstanceAllowed, so a floor a document could set above
// max_instances or past MaxInstanceIndex (ParseConfig clamps both; Apply is
// also reachable without it) never loops past the addressable range. An
// instance that could never be reached is skipped rather than created: its
// address is another spec's literal upstream model (instanceAddressHolder),
// which every request for it reaches instead. The rest of the floor still
// comes up -- a gap in the index range is harmless: every instance is
// addressed by its own index, and none needs the ones below it. An existing
// state is always this spec's own instance: no spec ID contains '#'
// (reservedSpecID), so no other spec's state can hold the key.
func (o *owner) ensureProtectedFloor() {
	for _, spec := range o.cfg.Specs {
		for k := 1; k < ProtectedCount(spec) && InstanceAllowed(spec, k); k++ {
			key := InstanceKey(spec.ID, k)
			if _, exists := o.specs[key]; exists {
				continue
			}
			if holder, held := o.instanceAddressHolder(spec, k); held {
				slog.Warn("runtime: an instance address of a replicable spec is another spec's upstream model, which it routes to; that instance cannot exist",
					"spec", spec.ID, "instance", k, "address", InstanceAddress(spec.UpstreamModel, k), "conflicting_spec", holder)
				continue
			}
			o.specs[key] = &specState{spec: specForInstance(spec, k), instance: k, state: StateStopped, since: time.Now()}
		}
	}
}

// sameReplicableOptIn reports whether two spec lists opt the same spec IDs into
// instances (Spec.Replicable) -- the one field of an already-applied document
// that can change without its ETag changing (applyConfig's short circuit).
func sameReplicableOptIn(a, b []Spec) bool {
	replicable := make(map[string]bool, len(a))
	for _, spec := range a {
		if spec.Replicable {
			replicable[spec.ID] = true
		}
	}
	n := 0
	for _, spec := range b {
		if !spec.Replicable {
			continue
		}
		if !replicable[spec.ID] {
			return false
		}
		n++
	}
	return n == len(replicable)
}

// instanceLess is the owner's order over instances wherever the order decides
// which one is asked first: by base spec ID, then by instance index
// NUMERICALLY -- S, S#2, S#10, never the string order S, S#10, S#2 -- so
// instance 0 of a spec always comes before its further instances, and they
// come in index order: the highest index leaves first, so the lowest comes up
// first. For a spec that never replicates it is plain spec-ID order.
func instanceLess(aBase string, aK int, bBase string, bK int) bool {
	if aBase != bBase {
		return aBase < bBase
	}
	return aK < bK
}

// sortInstanceOrder sorts instance keys of states in o.specs into instanceLess
// order.
func (o *owner) sortInstanceOrder(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		a, b := o.specs[ids[i]], o.specs[ids[j]]
		return instanceLess(specBase(a.spec), a.instance, specBase(b.spec), b.instance)
	})
}

// reconcileSpec stores spec -- the newly pushed spec for instance 0, or the
// per-instance copy re-derived from it -- on st and acts on what changed. The
// comparison is between two values of the SAME kind (a pushed spec against the
// previously pushed one, a derived copy against the previously derived one), so
// an unchanged push compares equal for every instance and restarts none.
func (o *owner) reconcileSpec(st *specState, spec Spec) {
	// M1 fix: only reset a resting terminal/backoff state when THIS
	// spec's own definition actually changed, not merely because the
	// overall Config's ETag changed (which happens on ANY edit,
	// including one to a completely different spec). Before this, an
	// operator editing spec B could let spec A's active crash-backoff
	// skip its remaining wait for no reason connected to A at all.
	//
	// StateNotPermitted/StatePendingVRAMUnknown are deliberately NOT
	// reset here any more (I6): handleEnsure no longer short-circuits
	// them, so every fresh request already re-evaluates Permit/Admit
	// on its own; forcing them back to Stopped here without a request
	// to actually re-check would just be a cosmetic, potentially
	// misleading flip.
	changed := !reflect.DeepEqual(st.spec, spec)
	prevSpec := st.spec
	st.spec = spec
	if changed {
		st.terminalSpec, st.terminalCode = nil, ""
	}
	switch {
	case changed && st.proc == nil:
		switch st.state {
		case StateStartFailed, StateCrashed:
			o.setState(st, StateStopped)
		case StateBackoff:
			cancelTimer(o.m, &st.backoffTimer)
			o.setState(st, StateStopped)
		}
	case changed && st.proc != nil && spec.AdminState != "force_stopped" && spec.IsRPCServer() && st.claimed() &&
		(st.relaunchDeferred || !sameLaunchShape(prevSpec, spec)):
		// A member a head holds or has reserved is part of a running cluster:
		// a member has no requests in flight, so a drain would kill it at once
		// and the head with it. Its relaunch waits for the claim to end -- the
		// gateway relaunches the head for a change of the member's launch, and
		// releases the member when that head stops (claimEnded). Once deferred
		// it stays deferred: the running process has the old shape, whatever
		// a later document says.
		if !st.relaunchDeferred {
			slog.Info("runtime: the member's launch changed while a head uses it; it is relaunched once the head lets go", "spec", spec.ID)
		}
		st.relaunchDeferred = true
	case changed && st.proc != nil && spec.AdminState != "force_stopped" && !sameLaunchShape(prevSpec, spec):
		// I7 fix (issue #63): a LIVE child whose LAUNCH SHAPE changed must
		// be relaunched with it. Before this, applyConfig stored the new
		// spec (above) but acted only on a STOPPED child, so a running
		// child kept the old binary/args/env/GPUs/visible-devices/port
		// until it restarted for some unrelated reason -- the edit
		// silently had no effect. Drain it exactly like a removed spec,
		// but WITHOUT st.removed: onProcExited's clean-stop branch then
		// re-admits this same spec, so a pinned/force_running child comes
		// back immediately with the new shape and an on-demand one on its
		// next request; either way in-flight requests drain rather than
		// being killed. A metadata-only edit (idle timeout, pinned,
		// admin_state, health/probe paths, model name) leaves the launch
		// shape equal, so sameLaunchShape skips it and it takes effect in
		// place from the stored st.spec. force_stopped is excluded: its
		// own drain is handled by the force loop below, and it must not
		// relaunch.
		o.beginDrain(spec.ID)
	}
}

// removeState takes one instance out of the desired set: a spec removed from
// the config, or an instance its spec no longer allows (#340). A live one
// drains -- its in-flight requests finish first -- and is deleted on exit
// (specState.removed, onProcExited); one with no process is deleted now.
func (o *owner) removeState(id string, st *specState) {
	if st.proc != nil {
		st.removed = true
		o.beginDrain(id)
		return
	}
	// G2 fix (fix round 2, and PRE-EXISTING rather than a defect of
	// this batch): A BACKOFF TIMER MUST NEVER OUTLIVE ITS SPEC. It is
	// registered in m.wg, which Close() ends by waiting on, and
	// handleClose can only cancel the timers of specs still IN
	// o.specs -- so deleting a spec that is waiting out a crash
	// backoff (proc == nil, StateBackoff) orphaned its retry timer and
	// made the NEXT Close block for the whole remaining delay (up to
	// backoffCap, 60s with production defaults) with nothing left to
	// retry: delete a crash-looping spec in the portal, then stop the
	// agent, and shutdown hangs for a minute. Sibling of
	// enterBackoff's own closing guard -- same invariant, the other
	// way for a spec to stop existing.
	//
	// Only this deletion site needs it. onProcExited's removed branch
	// deletes a spec whose proc was live, and a spec with a live proc
	// never has a backoff timer (enterBackoff defers to backoffOnExit
	// while st.proc != nil, and handleBackoffFire clears the timer
	// before admitAndStart can start anything).
	cancelTimer(o.m, &st.backoffTimer)
	// C2 fix: resolve any queued waiter before dropping the spec --
	// otherwise it (and even its own admission_wait_timeout, since
	// handleWaiterTimeout is itself a no-op once the spec is gone)
	// would never be resolved at all.
	o.failPending(st, ErrModelNotManaged)
	o.deleteState(id)
}

// sameLaunchShape reports whether two revisions of a spec would exec the
// IDENTICAL child process, and so whether a running child needs no relaunch
// when the spec changes (issue #63). It is defined as the COMPLEMENT of the
// metadata fields: every field the agent consumes live (re-read from the
// stored spec on the next relevant event) or only at the next start is
// zeroed, and whatever remains -- the fields ExpandPlaceholders + startProcess
// bake into the process (binary, args, env, work_dir, the GPU index set, the
// visible-devices toggle and mode, listen port, the ${API_TOKEN} material,
// upstream_model via a ${MODEL} placeholder, and config_files -- the files
// startProcess writes before exec and the child reads at startup, so a content
// edit must relaunch) -- is compared with
// reflect.DeepEqual. Anchoring on the metadata set is deliberate: a NEW Spec
// field therefore defaults to launch-affecting until it is added here, so a
// forgotten classification over-relaunches (harmless -- a graceful drain and
// restart) rather than silently ignoring a real launch change, which is the
// exact failure mode of issue #63.
func sameLaunchShape(a, b Spec) bool {
	strip := func(s Spec) Spec {
		s.Model = ""                      // router display/routing key, read live
		s.HealthPath = ""                 // startup health probe, next start only
		s.HealthTimeoutSeconds = 0        // "
		s.StartupTimeoutSeconds = 0       // "
		s.IdleTimeoutSeconds = 0          // scanIdle, read live
		s.AdmissionWaitTimeoutSeconds = 0 // per-request waiter, read live
		s.Pinned = false                  // admission / scanIdle, read live
		s.AdminState = ""                 // force loop below, read live
		s.Type = ""                       // reported for probes, read live
		s.MetricsPath = ""                // "
		s.ContextProbePath = ""           // "
		// RAMMB is the host-RAM admission DEMAND, the spec-level twin of
		// SpecGPU.VRAMMB below: consumed only by admission arithmetic, never
		// baked into the child. The gateway pushes max(measured, estimate), and
		// the measurement is written back from the agent's own reports, so
		// leaving it in the comparison would relaunch every freshly measured
		// healthy child.
		s.RAMMB = 0
		// Role is the eviction policy's input (which idle model to evict), never
		// baked into the child. The gateway derives it on every build from the
		// spec's capability verdicts, so an operator's verdict change moves it
		// with the process untouched, and the first document that carries it
		// would otherwise relaunch every healthy child.
		s.Role = ""
		// Replicable/MinInstances/MaxInstances (#340) decide HOW MANY processes
		// the spec may run and when they start -- the scaler's input, read live --
		// never what a process IS, so editing them must not bounce a healthy
		// child.
		s.Replicable = false
		s.MinInstances = 0
		s.MaxInstances = 0
		// Execution stays in the comparison (#381): the mode and the WSL distro
		// decide which process is exec'd and where. Only the spelling is
		// normalised, so an absent block and an explicit native one -- the same
		// launch -- do not bounce a healthy child when a gateway starts sending
		// the explicit form. An empty mode is native too (invalidExecutionReason
		// accepts it as such), so it is normalised the same way.
		if s.Execution != nil && (s.Execution.Mode == ExecutionModeNative || s.Execution.Mode == "") {
			s.Execution = nil
		}
		// Kind, DeviceBackend, Cluster and RPC stay in the comparison: they
		// decide what is exec'd and how (a member's bind and port, a head's
		// ordered endpoint list). The one exception is the connection
		// allowlist (RPC.HeadIPs/HeadDomain), which the agent applies to a
		// running member live, so an allowlist edit must not bounce it. The
		// RPC struct is copied before clearing: the Spec shares the pointer.
		if s.RPC != nil {
			rpc := *s.RPC
			rpc.HeadIPs = nil
			rpc.HeadDomain = ""
			s.RPC = &rpc
		}
		// GPUs' launch-relevant part is the ordered/deduped INDEX list
		// (gpuIndices -> hostGPUIDs/deviceList/the visibility variable);
		// SpecGPU.VRAMMB is the admission DEMAND, consumed only by the policy
		// arithmetic and the measurement snapshot, never baked into the child.
		// Zero it so a pure vram_mb edit is metadata (binds at the next start,
		// per the admission-not-retroactive rule) -- crucial because the
		// gateway writes measured VRAM back into vram_mb, which would otherwise
		// relaunch every freshly measured healthy child. Index (and any future
		// SpecGPU subfield) stays in the comparison, so the fail-safe bias
		// holds.
		if len(s.GPUs) > 0 {
			gpus := make([]SpecGPU, len(s.GPUs))
			for i, g := range s.GPUs {
				g.VRAMMB = 0
				gpus[i] = g
			}
			s.GPUs = gpus
		}
		return s
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// rebuildUpstreamIndex maps each spec's upstream model to its spec ID. Only
// instance 0 is indexed: a further instance shares its spec's upstream model
// (the child is always launched with the base name) and is reached through
// its instance address instead (resolveAddress), so indexing it would only
// collide with its own instance 0.
func (o *owner) rebuildUpstreamIndex() {
	o.byUpstream = make(map[string]string, len(o.specs))
	for id, st := range o.specs {
		if st.instance != 0 || st.spec.Kind == SpecKindRPCServer {
			// An rpc_server member serves no model: indexed, it would sit under
			// the empty upstream name and could shadow a real lookup.
			continue
		}
		// M4 fix: this used to collapse silently, with map iteration order
		// (unspecified, and different on every run) deciding which spec
		// wins EnsureRunning's upstream_model lookup -- an undiagnosable
		// config error without this log line. Still a collapse (the data
		// model's own invariant, one spec per mapping, means this
		// shouldn't happen from a well-formed config in the first place),
		// but now at least visible.
		if existing, dup := o.byUpstream[st.spec.UpstreamModel]; dup {
			slog.Warn("runtime: multiple specs share upstream_model; only one is reachable via EnsureRunning", "upstream_model", st.spec.UpstreamModel, "spec_a", existing, "spec_b", id)
		}
		o.byUpstream[st.spec.UpstreamModel] = id
	}
}

// handleEnsure resolves an EnsureRunning request immediately when possible
// (already Running, or a still-rate-limited terminal state), otherwise
// queues it and (re)attempts admission.
//
// I6 fix (round 1): earlier code short-circuited StateNotPermitted /
// StatePendingVRAMUnknown here, answering every subsequent request with
// the SAME cached verdict forever -- the only way to clear it was an
// unrelated Apply/ETag change (applyConfig's terminal-state reset). That
// made a transient cause (e.g. a missing ${AGENT_ENV:...} variable) look
// like a permanent one.
//
// R2-1 fix (round 2): removing that short-circuit outright made
// re-evaluation UNBOUNDED instead of merely un-sticky -- Permit is cheap,
// but the ExpandPlaceholders/env path (moved ahead of grabEphemeralPort in
// startProcess, see its own comment) and the PendingVRAMUnknown path
// (buildSnapshot invokes the installed measurer on every call) are not
// free once a spec is genuinely broken and something keeps retrying it.
// The two rate-limit checks below restore a bound: within
// notPermittedRetryInterval of the state's own onset (specState.notPermittedAt),
// a request gets the cached verdict with no re-evaluation at all; once
// the interval elapses, the next request falls through to the normal
// queue+admitAndStart path and re-evaluates fully, self-clearing if the
// underlying problem is fixed -- a rate limiter, not a return to
// stickiness. See docs/architecture/cross-cutting/agent-runtime-manager.md
// §6 (final paragraph) for the full reasoning, including the two endpoints
// that were tried and rejected.
//
// A plain request (EnsureServing, cmdEnsure.serving) differs in one place: it
// is refused with ErrInstanceNoCapacity, creating and starting nothing, when it
// names a burst instance that is neither running nor starting
// (plainRequestRefused) -- and when that instance rests at a failure
// (spentBurst), the refusal retires its row.
func (o *owner) handleEnsure(c cmdEnsure) {
	if o.closing {
		c.reply <- ensureOutcome{err: ErrManagerClosed}
		return
	}
	st, err := o.ensureTarget(c)
	if err != nil {
		c.reply <- ensureOutcome{err: err}
		return
	}
	specID := st.spec.ID
	if c.member && st.relaunchDeferred && st.state == StateRunning && st.proc != nil && st.claim.heldBy == "" {
		// A member whose relaunch waits for its claim, asked for again while
		// no head is connected to it (at most a reservation stands): this is
		// a cluster start, and its head must form against the current launch.
		// Nothing uses the old process, so it drains now and the ensure waits
		// for the relaunch like any request that finds its spec draining.
		st.relaunchDeferred = false
		o.beginDrain(specID)
	}
	if st.state == StateRunning && st.proc != nil {
		st.inFlight++
		st.lastUsed = time.Now()
		o.armReservation(st, c.reserve)
		c.reply <- ensureOutcome{endpoint: endpointFor(st.proc.port), specID: specID, proc: st.proc}
		return
	}
	if terminalHeld(st) {
		c.reply <- ensureOutcome{err: ErrNotPermitted}
		return
	}
	if st.state == StateNotPermitted && time.Since(st.notPermittedAt) < notPermittedRetryInterval {
		c.reply <- ensureOutcome{err: ErrNotPermitted}
		return
	}
	if st.state == StatePendingVRAMUnknown && time.Since(st.notPermittedAt) < notPermittedRetryInterval {
		c.reply <- ensureOutcome{err: ErrAdmissionBlocked}
		return
	}
	// A cold cluster head starts only for the gateway's cluster start (see
	// cluster_head.go); any other request is refused, so the gateway
	// orchestrates the cluster first. Decided after the verdicts above, so a
	// head that cannot run is reported as such rather than sending the gateway
	// off to bring its members up for nothing.
	if !c.clusterStart && headRefusesUnmarked(st) {
		c.reply <- ensureOutcome{err: fmt.Errorf("%w: cluster head %s is not running; the gateway starts its cluster", ErrClusterNotReady, specID)}
		return
	}
	// A burst instance is never queued behind anything but its own start: its
	// admission is all-or-nothing and immediate, so that the gateway can place
	// the request elsewhere at once. Starting: its admission already happened,
	// so the request waits for that start like any other. Draining: it is on its
	// way out, and burst capacity does not wait for a slot -- the gateway places
	// the request elsewhere. With no process at all, admitAndStart below answers
	// at once: it starts the instance or refuses it, never leaving the waiter
	// queued.
	if st.burst() && st.proc != nil && st.state != StateStarting {
		c.reply <- ensureOutcome{err: ErrInstanceNoCapacity}
		return
	}

	w := &ensureWaiter{reply: c.reply, queuedAt: time.Now(), reserve: c.reserve, clusterStart: c.clusterStart}
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

// ensureTarget resolves what an ensure names to the state it is admitted as,
// or answers it with a refusal. A member ensure names an rpc_server spec by ID
// and reaches nothing else (memberState); every other ensure names an instance
// address (resolveAddress), which never reaches a member: a member is not in
// the upstream index. force_stopped applies to every instance of a spec and is
// read off instance 0 -- the pushed spec itself -- before a further instance
// could be created for nothing.
func (o *owner) ensureTarget(c cmdEnsure) (*specState, error) {
	if c.member {
		st, ok := o.memberState(c.model)
		if !ok {
			return nil, ErrModelNotManaged
		}
		if st.spec.AdminState == "force_stopped" {
			return nil, ErrAdmissionBlocked
		}
		return st, nil
	}
	specID, _, k, ok := o.resolveAddress(c.model)
	if !ok {
		return nil, ErrModelNotManaged
	}
	st := o.specs[specID]
	if st.spec.AdminState == "force_stopped" {
		return nil, ErrAdmissionBlocked
	}
	if k == 0 {
		return st, nil
	}
	// Decided before instanceForRequest, which would create the instance, or
	// retire a crashed or start_failed one and start a fresh one -- the start is
	// exactly what a plain request must not cause for a burst instance
	// (EnsureServing).
	// The retirement it may cause: a burst instance resting at a failure stays
	// only until it is named again, and deleting it creates and starts nothing.
	// Left in place, a row the gateway keeps naming for a moment (its telemetry
	// lags) would read crashed until the next Apply.
	if c.serving && o.plainRequestRefused(st, k) {
		key := InstanceKey(st.spec.ID, k)
		if failed := o.specs[key]; failed != nil && failed.spentBurst() {
			o.retire(key, failed)
		}
		return nil, ErrInstanceNoCapacity
	}
	return o.instanceForRequest(st, k), nil
}

// resolveAddress is Manager.ResolveAddress on the owner, and the rule
// handleEnsure resolves a request with (#340). specID is the BASE
// spec's ID (instance 0's key); base is the base upstream model; k the
// instance index. An exact byUpstream match always wins and is instance 0 --
// even when its state has since gone, in which case the name is simply not
// managed rather than re-read as an instance address of some other spec.
// Otherwise the name must parse as an instance address of a managed spec that
// is still desired (not removed) and allows instance k. The instance key S#k is
// then this spec's own: no spec ID contains '#' (reservedSpecID -- ParseConfig
// and applyConfig both drop such a spec), so no other spec's state can hold it.
func (o *owner) resolveAddress(model string) (specID, base string, k int, ok bool) {
	if id, found := o.byUpstream[model]; found {
		if o.specs[id] == nil {
			return "", "", 0, false
		}
		return id, model, 0, true
	}
	b, n := ParseInstanceAddress(model)
	if n == 0 {
		return "", "", 0, false
	}
	id, found := o.byUpstream[b]
	st := o.specs[id]
	if !found || st == nil || st.removed || !InstanceAllowed(st.spec, n) {
		return "", "", 0, false
	}
	return id, b, n, true
}

// instanceAddressHolder reports whether the address of instance k >= 1 of spec
// is another spec's literal upstream model, and names that spec. resolveAddress
// routes an exact upstream match first -- a real model whose name merely contains
// '#' must keep reaching its own spec -- so every request for such an address
// reaches the other spec and the instance could never be reached: a
// protected one would hold its resources for nothing, and its status row would
// claim the other spec's model name -- in LoadedModels, and in the router's
// /upstream relay, which answers from the first running row of that name. It
// reads byUpstream, the routing index itself, so it agrees with resolveAddress
// by construction; instance 0's address is its spec's own upstream model, never
// another spec's.
func (o *owner) instanceAddressHolder(spec Spec, k int) (holder string, held bool) {
	holder, held = o.byUpstream[InstanceAddress(spec.UpstreamModel, k)]
	return holder, held && holder != spec.ID
}

// plainRequestRefused reports whether a plain request (EnsureServing) naming
// instance k >= 1 of base's spec is refused: k is a burst instance that is
// neither running nor starting. Burst is read off the pushed spec (IsBurst), so
// an instance with no state at all is classified too. A running burst instance
// serves the request, a starting one has been admitted already and the request
// waits for its start, and a protected instance is admitted as for any request.
// (handleEnsure retires a refused one that rests at a failure.)
func (o *owner) plainRequestRefused(base *specState, k int) bool {
	if !IsBurst(base.spec, k) {
		return false
	}
	st := o.specs[InstanceKey(base.spec.ID, k)]
	switch {
	case st == nil:
		return true
	case st.state == StateStarting:
		return false
	default:
		return st.state != StateRunning || st.proc == nil
	}
}

// instanceForRequest returns the state of instance k of base's spec for a
// request that names it, creating it on demand: an allowed instance that does
// not exist yet starts life Stopped and is admitted like any other. A burst
// instance resting at a crash or a failed start is "named again" here, which
// retires that row and starts over with a fresh one: burst capacity is never
// retried on its own, so an ensure asking for it again IS the retry. (One
// resting at not_permitted is re-evaluated in place instead, behind the same
// rate limit as any refusal.) Only an ensure (EnsureRunning) reaches this for a
// burst instance that is neither running nor starting: handleEnsure refuses a
// plain request (EnsureServing) for one first, and retires a failed row itself,
// starting nothing.
// A state found under the key is always an instance of this very spec: no spec
// ID contains '#' (reservedSpecID), so no other spec's state can hold it.
func (o *owner) instanceForRequest(base *specState, k int) *specState {
	key := InstanceKey(base.spec.ID, k)
	st := o.specs[key]
	if st != nil && st.burst() && st.proc == nil && (st.state == StateCrashed || st.state == StateStartFailed) {
		o.retire(key, st)
		st = nil
	}
	if st == nil {
		st = &specState{spec: specForInstance(base.spec, k), instance: k, state: StateStopped, since: time.Now()}
		o.specs[key] = st
	}
	return st
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
				o.noteWaitEnded(st)
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

// handleWaiterTimeout resolves the queued waiter whose admission-wait timer
// just fired with ErrAdmissionBlocked -- unless this spec is STARTING, in
// which case the wait is no longer an ADMISSION wait at all.
//
// B6 fix, and it closes a wider hole than the "nanosecond-scale Stop() race"
// it was deferred as. I3 established the rule (startProcess cancels the
// admission-wait timers of the waiters queued on a spec the moment its
// process starts: "an admission-wait timer exists to bound how long a
// request waits for a SLOT to free up; once the target has actually started,
// the request is waiting on startup, which has its own bound"). But it only
// ever covered the waiters that existed AT startProcess TIME:
//
//   - the narrow case originally recorded: cancelTimer's Stop() loses to a
//     callback already running, and the queued cmdWaiterTimeout then fails a
//     waiter whose model is seconds from healthy;
//   - the wide case, with no race at all: every request arriving AFTER the
//     spec entered StateStarting gets a FRESH timer from handleEnsure (the
//     state is Starting, not Running, so it queues) that nothing ever
//     cancels. So the second and later concurrent requests for a cold model
//     whose load outlasts admission_wait_timeout_seconds get exactly the
//     misleading ErrAdmissionBlocked I3 exists to prevent -- deterministic,
//     on ordinary traffic.
//
// THE GUARD IS StateStarting, NOT st.proc != nil (fix round 1, F1), and it is
// that ALONE -- no st.proc != nil conjunct (fix round 2, G6: the conjunct
// contradicted this paragraph and was redundant besides). StateStarting is set
// in exactly one place, startProcess, on the statement after st.proc = proc,
// and every path that clears st.proc leaves some other state behind, so
// StateStarting implies st.proc != nil.
//
// Trading the timer away is only defensible where something else takes over
// the bound, and startup_timeout_seconds does that in exactly one state:
// StateStarting, where pollHealth enforces it and handleStartResult reports
// it via failPending(ErrStartTimeout). st.proc != nil is TRUE IN
// StateDraining as well, and in that window there is no bound left at all --
// handleStartResult returns early for a draining generation (B6's own fix
// #1) so ErrStartTimeout can never be reported, pollHealth returns silently
// on <-proc.exited once the generation is killed, and scheduleAfter is
// one-shot so nothing re-arms the timer this callback has already spent
// (fix round 2, G6: "the timer discarded here" was loose -- reaching this
// function IS that timer firing; what is discarded is its ANSWER, and the
// timer cannot fire a second time). A request that queues while its spec is
// being torn down (idle-unload, force_stopped, an admission eviction) would
// then run to its caller's HTTP context instead of getting the bounded 503
// admission_wait_timeout_seconds promises. Draining is also the state where
// the timer is most obviously still MEANINGFUL: a dying generation has been
// DE-admitted, not admitted, so the request is once again waiting for a slot
// -- precisely what docs/architecture/cross-cutting/agent-runtime-manager.md
// §5.4's busy-or-pinned queueing bound is for.
//
// The timer is left alone rather than cleared: whichever of
// succeedPending/failPending later resolves this waiter calls cancelTimer,
// whose Stop() correctly returns false for an already-fired timer, so the wg
// accounting stays balanced.
//
// Known, accepted consequence: a waiter that survives a failed generation
// and re-queues behind a fresh admission decision no longer carries an
// admission-wait bound (its timer is spent). That hole is not new -- I3's own
// cancellation already produced it for the first-generation waiters -- and
// the caller's request context still bounds the wait, which is exactly what
// admission_wait_timeout_seconds == 0 means per
// docs/architecture/cross-cutting/agent-runtime-manager.md §5.4.
func (o *owner) handleWaiterTimeout(c cmdWaiterTimeout) {
	st := o.specs[c.specID]
	if st == nil {
		return
	}
	if st.state == StateStarting {
		slog.Debug("runtime: admission-wait timeout ignored; the spec is starting, which startup_timeout_seconds bounds", "spec", c.specID, "state", string(st.state))
		return
	}
	for i, w := range st.pending {
		if w.reply == c.reply {
			w.timer = nil // this IS that timer's own callback; nothing left to Stop()
			st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
			o.noteWaitEnded(st)
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
	o.wakeAdmissionCandidates()
}

// failPending resolves and clears every waiter queued on st with err.
func (o *owner) failPending(st *specState, err error) {
	for _, w := range st.pending {
		cancelTimer(o.m, &w.timer)
		w.reply <- ensureOutcome{err: err}
	}
	st.pending = nil
	o.noteWaitEnded(st)
}

// succeedPending resolves and clears every waiter queued on st with st's
// current (freshly Running) proc, incrementing InFlight once per resolved
// waiter -- each gets its own independent release().
func (o *owner) succeedPending(st *specState, specID string) {
	ep := endpointFor(st.proc.port)
	for _, w := range st.pending {
		cancelTimer(o.m, &w.timer)
		st.inFlight++
		o.armReservation(st, w.reserve)
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
	o.signalTransition()
}

// signalTransition rings the transitions doorbell (Manager.Transitions): a row
// of Status() changed, or left the table. It never blocks -- a signal nobody has
// read yet already stands for this one.
func (o *owner) signalTransition() {
	select {
	case o.m.transitions <- struct{}{}:
	default:
	}
}

// deleteState takes the state of instance id out of the table, and rings the
// transitions doorbell for it exactly as a state change does: the row leaves
// Status(), and the agent's next telemetry sample, with it. The gateway routes
// requests to the instance addresses it read off those rows, so it has to learn
// of a deletion as promptly as of a change -- not at the next periodic sample,
// while it keeps naming an address that is gone and every request sent there is
// refused and re-selected. Some deletions move no state at all on their way --
// a request retiring a burst row that rests at a failure, a push removing a
// state with no process, a removed state's exit -- so the doorbell is rung
// here, and every deletion of a state goes through here.
func (o *owner) deleteState(id string) {
	delete(o.specs, id)
	o.signalTransition()
}

// setNotPermitted records the not_permitted refusal and fails st's queued
// requests with it.
//
// A refused instance is not waiting to start (lowerSiblingWaiting), so a
// protected sibling above it that was held back for it is re-asked here, on the
// spot (wakeSiblingsWaitingFor): the paths that refuse an instance -- its
// crash-loop retry, a request naming it, the dry run of its start -- ask that one
// instance alone, and nothing else would ask the held sibling until an unrelated
// event, leaving a request queued on it to its admission timeout. Every refusal
// goes through here or through setPendingVRAMUnknown.
func (o *owner) setNotPermitted(st *specState, message string) {
	o.setNotPermittedCode(st, message, "")
}

// terminalHeld reports whether st rests under a terminal verdict that its
// current spec still matches.
func terminalHeld(st *specState) bool {
	return st.terminalSpec != nil && reflect.DeepEqual(*st.terminalSpec, st.spec)
}

// setTerminalVerdict is setNotPermittedCode for a verdict that no retry can
// change: the spec is remembered, and stays refused until it differs.
func (o *owner) setTerminalVerdict(st *specState, message, code string) {
	snapshot := st.spec
	st.terminalSpec, st.terminalCode = &snapshot, code
	o.setNotPermittedCode(st, message, code)
}

// setNotPermittedCode is setNotPermitted for a verdict that carries a stable
// error code.
func (o *owner) setNotPermittedCode(st *specState, message, code string) {
	o.setState(st, StateNotPermitted)
	st.notPermittedAt = time.Now() // (re)start the rate-limit window -- see specState.notPermittedAt
	st.lastError = &LastError{Message: message, At: time.Now(), Code: code}
	o.failPending(st, ErrNotPermitted)
	o.wakeSiblingsWaitingFor(st)
}

// setPendingVRAMUnknown records the terminal unknown-VRAM block, and records
// the policy's message as this spec's LastError when there IS one.
//
// The message is what names the spec an operator has to fix, and for rule 5 it
// is not this one: the spec entering this state is the one with a usable
// estimate, while the missing estimate belongs to the pinned spec beside it.
// Rule 4's block is the candidate's own demand, needs no name, and sends no
// message -- lastError is then left exactly as it was rather than being
// blanked, since it may still describe a real earlier failure of this spec.
//
// Like setNotPermitted, it re-asks a protected sibling held back for st, which a
// refused st no longer holds back.
func (o *owner) setPendingVRAMUnknown(st *specState, message string) {
	o.setState(st, StatePendingVRAMUnknown)
	st.notPermittedAt = time.Now() // (re)start the rate-limit window -- see specState.notPermittedAt
	if message != "" {
		st.lastError = &LastError{Message: message, At: time.Now()}
	}
	o.failPending(st, ErrAdmissionBlocked)
	o.wakeSiblingsWaitingFor(st)
}

// noteAdmissionWait records a queued spec's reason for waiting as its
// LastError, for the one Wait that carries one.
//
// WHY A WAIT WRITES ANYTHING AT ALL. Wait is the manager's silent branch by
// design: the spec stays queued, its state is untouched, and a future
// completion re-triggers the admission. That is right for a wait that resolves
// itself -- a busy neighbour finishes, a drain completes -- and it is wrong for
// the precedence Wait of ADR-032, which need not resolve at all: the
// known-demand occupant is spared BECAUSE it is not being evicted, and
// scanIdle skips a spec whose IdleTimeoutSeconds is 0, the documented "never
// unload". So an unknown-demand candidate can requeue to its
// admission_wait_timeout_seconds on every request indefinitely, and without
// this the operator saw a spec that never starts, in a state that says nothing,
// with no reason string anywhere. Admit decides which Wait that is (Message is
// set on that one alone); this function only refuses to invent a diagnostic
// where there is none.
//
// NEITHER THE STATE NOR failures MOVES. The spec has not failed to start --
// nothing was attempted -- so this is not recordFailure, and the pending
// waiters stay queued rather than being failed as setPendingVRAMUnknown fails
// them. A successful start clears LastError, which is the same lifecycle the
// terminal messages already have.
//
// It DOES replace an older message, including a crash's, exactly as
// setNotPermitted and setPendingVRAMUnknown do: LastError is "why this spec is
// not serving now", and an earlier generation's crash is no longer that once
// admission is what holds it back. The crash output itself is not lost with it
// -- the per-spec log view keeps that generation's tail (logs.go).
//
// The timestamp is kept at "since when", not "last asked": every retried
// request re-runs this with the identical message, and refreshing At on each
// of them would make LastError.At read as if the block had only just started.
func (o *owner) noteAdmissionWait(st *specState, message string) {
	if message == "" {
		return
	}
	if st.lastError != nil && st.lastError.Message == message {
		return
	}
	st.lastError = &LastError{Message: message, At: time.Now()}
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

// vramBootstrapEligible reports whether st may use its ONE guarded VRAM
// bootstrap eviction on this admission (task 4.3). Three conditions, all
// required:
//
//   - A VRAM measurer is installed. The exception exists only to CAPTURE a first
//     measurement, so where none can be taken it must not fire. Measurement is
//     NVIDIA-only (§5.3): on an AMD, Apple-silicon or GPU-less host "waiting for
//     the measurement to arrive is waiting for something that will not happen",
//     and there §5.3 is explicit that an unknown-demand spec simply WAITS behind
//     a busy/pinned holder. Evicting a working known-demand model on such a host
//     for an unknown spec that can never be measured is exactly the ADR-032
//     regression the order exists to prevent -- so the bootstrap is gated on the
//     measurer, the VRAM twin of P4a's ramMeasurerInstalled gate.
//   - The declared VRAM demand is unknown. A known-demand candidate never runs
//     rule 4 at all, so the grant would be meaningless.
//   - The bootstrap is still unresolved: no measurement captured and the one shot
//     unspent (specState.vramBootstrapDone). This is what keeps the exception
//     from repeating -- the thrash guard.
//
// It is the per-candidate grant admitAndStart copies into
// PolicySnapshot.VRAMBootstrapEligible, the one input that turns rule 4 from a
// block into a single eviction.
//
// "Unknown" is read off the instance's admission demand (demandSpec), so an
// instance 0 whose sibling has been measured is no longer a bootstrap: it has a
// number, inherited (#340).
func (o *owner) vramBootstrapEligible(st *specState) bool {
	return o.m.measurer.Load() != nil && !st.vramBootstrapDone && specHasUnknownVRAM(o.demandSpec(st))
}

// wantsUp reports whether anything wants st running right now: a queued
// request, or its being protected -- an operator pin / force_running on
// instance 0, or an instance inside the min_instances floor (protected) -- and
// for a cluster head only a queued cluster start (specState.wantsStart). It is
// admitAndStart's own admission precondition, factored out so the cold-start
// bootstrap memory-pressure escalation (onProcExited, task 4.4) can ask the
// identical question -- an eviction to free RAM for a retry is worth nothing if
// no retry is coming.
func (o *owner) wantsUp(st *specState) bool {
	return st.wantsStart()
}

// specForInstance returns the Spec instance k of base runs as (#340): base
// itself for instance 0, and for k >= 1 a copy that differs only in identity.
// ID becomes the instance key S#k -- which is what gives the instance its own
// admission identity and its own ${CONFIG_DIR} and config-file directory, all
// keyed by spec.ID -- InstanceOf names the base spec, and Instance is k. (Its
// output is the exception: it goes into the BASE spec's log buffer, each line
// prefixed "[#k] " -- see startProcess and LogStore.newInstanceProc.)
// Everything pushed is shared, UpstreamModel included, so
// ${MODEL} expands to the BASE model name: the child never sees an instance
// address (the router rewrites the request's model to match). The copy
// shares base's slices and maps; nothing in the owner writes through them
// (raisedDemand copies the GPU list before it raises a demand).
func specForInstance(base Spec, k int) Spec {
	if k == 0 {
		return base
	}
	s := base
	s.ID = InstanceKey(base.ID, k)
	s.InstanceOf = base.ID
	s.Instance = k
	return s
}

// protected reports whether this instance is inside its spec's protected
// floor (ProtectedCount): instances 0..P-1, where P folds
// min_instances together with the one instance a pinned or force_running spec
// has always kept. A protected instance is wanted up with no request at all
// (started on Apply, restarted after a crash with backoff) and is never
// idle-unloaded. For a spec that never replicates, P is 1 exactly when it is
// pinned or force_running, so this is the pre-instance "pinned ||
// force_running" rule unchanged. st.spec carries the base's own
// Pinned/AdminState/min_instances (copied for k >= 1), so no lookup of the
// base is needed.
func (st *specState) protected() bool {
	return st.instance < ProtectedCount(st.spec)
}

// pinnedForAdmission is the Pinned flag this instance carries into admission
// (RunningProc.Pinned), and the flag every other "never a victim" check the
// owner makes must agree with (evictOneIdleForBootstrap, earliestGraceExpiry,
// wantsWakeAfterStart). For a spec that never replicates it is spec.Pinned,
// exactly as before instances -- a force_running spec stays evictable as it
// always was. For a replicable spec it is protected(): the min_instances
// promise is worth nothing if an admission may evict a protected instance, and
// a further instance of a pinned spec must not read as pinned merely because
// its copy carries the field.
//
// A cluster head that is force_running reads pinned as well: for a head,
// force_running means the gateway keeps it up (headKeptUp).
//
// An rpc_server member under a hold or a reservation (claimed) reads pinned
// too: a head is using it, or is about to, and evicting it would abort that
// head's cluster.
func (st *specState) pinnedForAdmission() bool {
	return st.pinnedBySpec() || st.claimed()
}

// pinnedBySpec is pinnedForAdmission without a member's claim: what the
// pushed spec alone makes of the instance.
func (st *specState) pinnedBySpec() bool {
	if !st.spec.Replicable {
		return st.spec.Pinned || headKeptUp(st.spec)
	}
	return st.protected()
}

// burst reports whether this instance is burst capacity (IsBurst): above the
// protected floor, started on demand into free room only, never retried after
// a failure, and retired once it is gone. Instance 0 never is.
func (st *specState) burst() bool {
	return IsBurst(st.spec, st.instance)
}

// higherSiblingRuns reports whether an instance of st's spec with a HIGHER
// index still has a process (starting, running or draining). That is the
// ordering invariant of a spec's instances, "highest index leaves first", which
// keeps the instances that run the lowest ones: while one does, st may be
// neither idle-unloaded (scanIdle) nor picked by the owner's own victim choice
// (evictOneIdleForBootstrap), so instance 0 is always the last of a spec to
// go. Admit enforces the same rule for its victims
// (policy.go's higherSiblingStays); this is its owner-side twin. Siblings are
// matched by base spec ID, never by parsing a name.
func (o *owner) higherSiblingRuns(st *specState) bool {
	base := specBase(st.spec)
	for _, sib := range o.specs {
		if sib.proc != nil && sib.instance > st.instance && specBase(sib.spec) == base {
			return true
		}
	}
	return false
}

// demandSpec returns the Spec admission charges st as (#340): st.spec with each
// GPU's VRAM demand and the host-RAM demand raised to the largest LIVE
// measurement of any running sibling -- per GPU for VRAM, host-wide for RAM. Siblings run the same model with the same arguments, so a
// measured sibling is the best estimate there is of what another instance will
// hold, and the pushed demand stays a floor, exactly like the live-measurement
// rule in buildSnapshot. "Running" is meant strictly: only a reading taken
// after the sibling passed its health probe counts (siblingPeak), so a sibling
// still loading leaves the demand where it was -- unknown, if nothing was
// declared -- rather than lending it a fraction of the model.
//
// It applies to EVERY instance -- instance 0 included -- as a CANDIDATE: instance
// 0 restarting next to a protected (hence pinned), measured sibling would
// otherwise be charged its unknown declared demand and hit the pinned-unknown
// terminal (policy step 2). An OCCUPANT is charged by chargeSpec instead, which
// inherits only what the occupant has not measured of itself. A
// spec that never replicates is returned untouched: the Replicable gate makes
// that structural rather than a consequence of an empty sibling set.
//
// The result never aliases st.spec's GPU list: a raised demand is written into
// a fresh copy, because the list is shared with the pushed config.
func (o *owner) demandSpec(st *specState) Spec {
	if !st.spec.Replicable {
		return st.spec
	}
	vram, ram := o.siblingPeak(st)
	return raisedDemand(st.spec, vram, ram)
}

// chargeSpec is the demand buildSnapshot charges a LIVE instance st with as an
// occupant (#340). Siblings are ordinary occupants whose demand is summed, so
// each occupant counts for what it holds: an instance with its own READY measurement
// (readyVRAM per GPU, readyRAM -- taken after its first passing probe) is
// charged that figure, floored at its declared demand and never raised to a
// sibling's peak. Raising it would over-charge every measured sibling at once:
// two instances measured at 3000 and 3500 MB would count as 7000, and a third
// that fits next to their real 6500 would be refused. What the occupant has not
// measured of itself since it became ready -- a Starting instance has nothing
// yet, and a just-ready one may not have its RAM figure yet -- inherits the
// sibling peak exactly as demandSpec does, the conservative reading of a copy
// of the same model. A spec that never replicates is returned untouched.
//
// The result never aliases st.spec's GPU list, like demandSpec's.
func (o *owner) chargeSpec(st *specState) Spec {
	if !st.spec.Replicable {
		return st.spec
	}
	vram, ram := o.siblingPeak(st)
	for idx, mb := range st.readyVRAM {
		if vram == nil {
			vram = make(map[int]int, len(st.readyVRAM))
		}
		vram[idx] = mb // its own ready figure supersedes the inherited one
	}
	if own := realRAM(st.readyRAM); own > 0 {
		ram = own
	}
	return raisedDemand(st.spec, vram, ram)
}

// raisedDemand returns spec with each GPU's VRAM demand raised to vram[index]
// and the host-RAM demand raised to ram -- never lowered: the pushed demand is a
// floor. The GPU list is copied before it is written, because it is shared with
// the pushed config; vram is only read.
func raisedDemand(spec Spec, vram map[int]int, ram int) Spec {
	if len(vram) > 0 && len(spec.GPUs) > 0 {
		gpus := make([]SpecGPU, len(spec.GPUs))
		for i, g := range spec.GPUs {
			if v := vram[g.Index]; v > g.VRAMMB {
				g.VRAMMB = v
			}
			gpus[i] = g
		}
		spec.GPUs = gpus
	}
	spec.RAMMB = max(spec.RAMMB, ram)
	return spec
}

// siblingPeak returns the largest live measurement among st's running
// siblings: per GPU index for VRAM, one figure for host RAM. Only READY
// readings count (specState.readyVRAM/readyRAM: taken after the sibling's
// process first passed a health probe), never one caught while it was still
// loading -- that is a fraction of the model, and inheriting it would make an
// unknown demand count as known and admit a further instance against the
// fraction, where it is meant to wait for a measured running sibling. A ready
// reading describes its sibling's CURRENT process (cleared on exit), so only
// siblings with a process count, and st's own figures never do.
func (o *owner) siblingPeak(st *specState) (vram map[int]int, ram int) {
	base := specBase(st.spec)
	for _, sib := range o.specs {
		if sib == st || sib.proc == nil || specBase(sib.spec) != base {
			continue
		}
		for idx, mb := range sib.readyVRAM {
			if mb <= vram[idx] {
				continue
			}
			if vram == nil {
				vram = make(map[int]int, len(sib.readyVRAM))
			}
			vram[idx] = mb
		}
		ram = max(ram, realRAM(sib.readyRAM))
	}
	return vram, ram
}

// demandUnknown reports whether spec -- an instance's demandSpec -- still has
// an unknown demand on either axis: a declared GPU at VRAM 0, or host RAM 0 (0
// is "unknown", never "needs none", on both). An instance k >= 1 with such a
// demand is never admitted: a baseline one waits for a measured sibling, a
// burst one is refused with ErrInstanceDemandUnknown.
func demandUnknown(spec Spec) bool {
	return specHasUnknownVRAM(spec) || specHasUnknownRAM(spec)
}

// retireIfSpent deletes a burst instance's state once nothing is left of it
// (#340): no process, no queued request, and no failure to show. Burst capacity
// exists only while it serves -- left in place, the row would read "stopped"
// forever. A burst instance resting at a FAILURE (spentBurst: crashed,
// start_failed, not_permitted) is kept so an operator can read why, until it is
// named again or the next Apply retires it. The gateway's instance scaler
// treats such an index as free, so its next ensure for the spec is what names
// it again (routing sends no request to a row that is not running). Named by a
// plain request, the row
// is retired whichever of the three failures it shows, and nothing is started
// (handleEnsure). Named by an ensure, a crashed or start_failed row is retired
// and a fresh instance started in its place (instanceForRequest), while a
// not_permitted row is not retired: it is re-evaluated in place like any
// refusal -- within notPermittedRetryInterval of the refusal the ensure is
// answered ErrNotPermitted at once, after it the ensure runs admission again
// on that same row. Instance 0 and the protected instances are never retired,
// and neither is a removed state, which onProcExited deletes on its own path.
func (o *owner) retireIfSpent(id string) {
	st := o.specs[id]
	if st == nil || !st.burstLeftNothing() || restsAtFailure(st.state) {
		return
	}
	o.retire(id, st)
}

// burstLeftNothing reports whether st is a burst instance with nothing left of
// it: no process, no queued request, and not removed (a removed state is
// deleted on its own path).
func (st *specState) burstLeftNothing() bool {
	return st.burst() && !st.removed && st.proc == nil && len(st.pending) == 0
}

// restsAtFailure reports whether s is a state a burst instance rests at after
// a failure: crashed, start_failed or not_permitted.
func restsAtFailure(s State) bool {
	return s == StateCrashed || s == StateStartFailed || s == StateNotPermitted
}

// spentBurst reports whether st is a burst instance with nothing left of it but
// a failure to show -- a burst instance is never retried on its own, so that
// row only waits to be named again or the next Apply.
func (st *specState) spentBurst() bool {
	return st.burstLeftNothing() && restsAtFailure(st.state)
}

// retire deletes the state of a burst instance that is gone (deleteState, which
// rings the transitions doorbell), and cancels any crash-loop timer with it: a
// timer must never outlive its state (handleClose can only cancel the timers of
// the states still in the table).
func (o *owner) retire(id string, st *specState) {
	cancelTimer(o.m, &st.backoffTimer)
	o.deleteState(id)
}

// admitAndStart is the single place that decides whether spec should move
// toward Running right now, and if so starts it -- called for a freshly
// queued request, after a backoff/drain/idle completion frees something up,
// or from Apply for a pinned/force_running spec. Always a fast, synchronous,
// non-blocking call: any real work (exec, health polling) is handed to a
// goroutine before this returns.
//
// specID is an INSTANCE key (#340). Instance 0 and every protected instance --
// the baseline -- are admitted by the rules above unchanged, charged their
// sibling-inherited demand (demandSpec). A burst instance is admitted only into
// room that is free right now (PolicySnapshot.NoEvict) and is otherwise
// refused at once (ErrInstanceNoCapacity, or ErrInstanceDemandUnknown before
// Admit is even asked), never queued; an instance k >= 1 is never admitted
// with an unknown demand; and a protected instance k >= 1 is not admitted while
// a lower sibling still waits to start (lowerSiblingWaiting), whichever of them
// is asked first.
func (o *owner) admitAndStart(specID string) {
	if o.closing {
		// Never start (or restart) anything once shutdown has begun -- a
		// pinned/force_running spec resurrecting itself here would keep
		// maybeFinishClose from ever seeing every process gone, hanging
		// Close forever.
		return
	}
	// Reconcile the owner's single grace timer against the verdict reached below,
	// on every path out of here: whatever this admission does to its own
	// waitingOnGrace, the earliest within-grace expiry worth waking for may now be
	// different (or gone). rescheduleGraceWake is a cheap no-op whenever nothing is
	// grace-blocked (the common case), so the defer costs nothing on the hot path.
	defer o.rescheduleGraceWake()
	// A burst instance this admission leaves with nothing -- refused, or simply
	// no longer wanted (the exit of a drained one comes through here) -- is
	// retired on the way out. Deferred so every return below is covered, and
	// registered after the grace defer so it runs first: the grace scan then
	// sees the final spec table.
	defer o.retireIfSpent(specID)
	st := o.specs[specID]
	if st == nil || st.proc != nil || st.state == StateBackoff {
		return
	}
	if terminalHeld(st) {
		o.failPending(st, ErrNotPermitted)
		return
	}
	// Every verdict below rewrites these from scratch, and so does every return
	// before one: a spec nobody wants up, or one force-stopped, is neither a RAM
	// nor a grace waiter, and a stale true must never be able to select it for a
	// sample or grace wake.
	st.waitingOnRAM = false
	st.waitingOnGrace = false
	st.waitingOnDemand = false
	st.waitingOnSibling = false
	st.waitingOnWSLUID = false
	// The claimed member blocking this admission is decided afresh on every ask
	// and recorded once, on the way out, so a wait that stays blocked by the
	// same member does not ring the transitions doorbell on every re-ask.
	blockedBy := ""
	defer func() { o.setAdmissionBlockedBy(st, blockedBy) }()
	// A cold cluster head starts only for a cluster start. A request queued
	// without the mark -- one that waited for a start or a drain that has since
	// ended -- would otherwise start it here, so it is answered as handleEnsure
	// answers it now.
	if st.spec.IsClusterHead() && !st.clusterStartQueued() && len(st.pending) > 0 {
		o.failPending(st, fmt.Errorf("%w: cluster head %s is not running; the gateway starts its cluster", ErrClusterNotReady, specID))
	}
	if !o.wantsUp(st) {
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
	// Checked before admission, so nothing is evicted for a member that
	// cannot bind; refused like a Permit failure, so a retry is rate-limited
	// and nothing loops.
	if st.spec.IsRPCServer() {
		if err := memberBindLocal(st.spec); err != nil {
			st.bindMismatch = true
			o.setNotPermittedCode(st, err.Error(), codeBindMismatch)
			return
		}
		st.bindMismatch = false
	}
	// A wsl start cannot be built without the configured user's UID inside its
	// distro (the user manager's bus is /run/user/<uid>), and asking the distro
	// is a wsl.exe call that can take seconds -- far too long for the owner,
	// whose every other client (Status, every ensure, health results, kill
	// graces) would queue behind it. So the lookup runs off the owner, and the
	// instance waits for its answer here, before admission: queued, its state
	// untouched, no error recorded, and nothing evicted for a start that cannot
	// happen yet. The answer re-asks it (handleWSLUIDResolved). Like the
	// sibling wait, it is held back rather than decided.
	//
	// A BURST instance does not wait: its admission is all-or-nothing and
	// immediate (handleEnsure never leaves its waiter queued), so it is refused
	// at once with ErrInstanceNoCapacity, exactly like a burst start that finds
	// no free room, and the deferred retireIfSpent removes its row. The lookup
	// is started all the same, so the gateway's next attempt finds the UID
	// cached.
	if st.spec.IsWSL() {
		if _, ok := o.wslUIDs[st.spec.WSLDistro()]; !ok {
			o.requestWSLUID(st.spec.WSLDistro())
			if st.burst() {
				slog.Debug("runtime: burst instance refused: the WSL user's uid is not known yet", "spec", specID)
				o.failPending(st, ErrInstanceNoCapacity)
				return
			}
			st.waitingOnWSLUID = true
			st.decidedTurn = 0
			slog.Debug("runtime: wsl instance waits for the WSL user's uid", "spec", specID)
			return
		}
	}

	// The protected instances of a spec start lowest index first, on every path
	// that asks: Apply and the wakes ask in that order already, but a
	// crash-loop timer, a request naming m#k, and an instance's own re-admission
	// on exit ask in the order events happen to arrive. A protected instance
	// admitted ahead of a lower sibling that is still waiting would keep the
	// room it took -- a sibling is never a victim, and a protected instance is
	// never idle-unloaded -- so that sibling, instance 0 above all,
	// would stay down for as long as it runs. It waits instead: silently, with
	// nothing evicted for it, because the lower sibling's own row says why that
	// one is not up, and the wait ends as soon as it has a process
	// (wakeSiblingsWaitingFor). Ahead of the snapshot, so a held-back instance
	// costs no measurement.
	if lower, waits := o.lowerSiblingWaiting(st); waits {
		st.waitingOnSibling = true
		st.decidedTurn = 0 // held back, not decided: the next ask decides (reask)
		slog.Debug("runtime: protected instance waits for a lower instance of its spec to start first", "spec", specID, "lower_instance", lower)
		return
	}
	// Everything from here on is a full decision, whatever its verdict, and it
	// stands for the rest of this turn (owner.turn): a wake loop that reaches st
	// again in this turn does not repeat it (reask).
	st.decidedTurn = o.turn

	// The one-shot VRAM bootstrap grant is per-candidate owner state, so it is
	// set on the snapshot here (which buildSnapshot, being candidate-agnostic,
	// cannot know) rather than inside the builder -- the same shape every other
	// snapshot field keeps: the owner decides, Admit interprets.
	//
	// The snapshot is built FIRST: it refreshes the live measurements, and the
	// candidate's own demand (demandSpec) inherits its siblings' -- so the
	// demand Admit is asked about is as fresh as the occupants it is weighed
	// against.
	snap := o.buildSnapshot()
	demand := o.demandSpec(st)
	burst := st.burst()
	// An instance k >= 1 is never admitted blind. Instance 0 keeps today's
	// unknown-demand handling -- the bootstraps, the one-shot eviction --
	// because something has to run first to be measured; every further
	// instance can wait for that measurement instead, and does.
	if st.instance > 0 && demandUnknown(demand) {
		if burst {
			// Refused fast, with nothing queued and no state left behind (the
			// deferred retireIfSpent): the gateway retries shortly, by which time
			// the sibling started for the same model has usually been measured.
			slog.Debug("runtime: burst instance refused: its demand is unknown", "spec", specID)
			o.failPending(st, ErrInstanceDemandUnknown)
			return
		}
		// A baseline (protected) instance waits, the same silent-but-diagnosed
		// way as rule 4's precedence wait: queued, state untouched, the reason on
		// its row. A ready measurement of a sibling landing is what ends it
		// (applyMeasurement -> wantsDemandWake).
		st.waitingOnDemand = true
		o.noteAdmissionWait(st, fmt.Sprintf(
			"spec %s: its demand is unknown (no declared estimate, and no sibling measured since it became ready), so it waits for a ready sibling to be measured instead of starting blind",
			specID))
		return
	}
	// A burst instance's admission is all-or-nothing and immediate -- it is
	// extra capacity, worth a start only into room that is free right now:
	// NoEvict makes Admit answer OK or NoCapacity and nothing else.
	snap.NoEvict = burst
	snap.VRAMBootstrapEligible = o.vramBootstrapEligible(st)
	dec := Admit(snap, demand)
	st.waitingOnRAM = dec.Wait && dec.WaitOnRAM
	st.waitingOnGrace = dec.Wait && dec.WaitOnGrace
	switch {
	case dec.NoCapacity:
		// FIRST, and it must stay first: NoCapacity carries Reason "", Wait
		// false and an empty Evict, so every case below would read it as an OK
		// and START the process -- the one thing it refuses. A burst instance
		// that does not fit into free room is refused at once: every queued
		// waiter fails now, nothing waits, the state does not move (so no
		// not_permitted row, no pending_vram_unknown), no victim is drained and
		// no bootstrap shot is burned. The deferred retireIfSpent removes the
		// state a request created for it.
		slog.Debug("runtime: burst instance refused: no free capacity", "spec", specID, "reason", dec.Message)
		o.failPending(st, ErrInstanceNoCapacity)
	case dec.Reason == StateNotPermitted:
		o.setNotPermitted(st, dec.Message)
	case dec.Reason == StatePendingVRAMUnknown:
		// The unknown-demand refusal reads a pinned occupant as one that never
		// leaves, and a claimed member reads pinned. A claim does end, though:
		// when it is the claim alone that refuses the start, the start waits
		// instead -- queued, state untouched, the member named -- and the end of
		// the claim asks it again (claimEnded), when the ordinary rules,
		// the one-shot bootstrap among them, decide.
		if member := o.claimedMemberBlocking(snap, demand); member != "" {
			blockedBy = member
			return
		}
		o.setPendingVRAMUnknown(st, dec.Message)
	case dec.Wait:
		// Leave st queued; a later release, exit or Apply re-triggers this
		// via wakeAdmissionCandidates, and so does a start that finishes,
		// via wakeAfterStart, for a queued request or a pinned spec only (see
		// there). The state is deliberately unchanged -- the spec is waiting,
		// not failed -- but a Wait that carries a message records it, because
		// that is the one Wait nothing need ever resolve. See
		// noteAdmissionWait.
		o.noteAdmissionWait(st, dec.Message)
		blockedBy = o.claimedMemberBlocking(snap, demand)
	case len(dec.Evict) > 0:
		if dec.VRAMBootstrap {
			// This Evict spent the candidate's one-shot VRAM bootstrap (rule 4
			// evicted a known-demand occupant it would otherwise only have
			// blocked). Burn the shot NOW, before the victim even drains, so the
			// marker is durable against a re-entrant admission and the exception
			// fires at most once per spec -- the ADR-032 anti-thrash guarantee.
			st.vramBootstrapDone = true
		}
		for _, victimID := range dec.Evict {
			// Record WHO this eviction is for before asking for it: the
			// victim's exit path gives these specs their admission attempt
			// before the victim may re-take the slot (see
			// specState.drainedFor). Recorded even when beginDrain turns out
			// to be a no-op because the victim is already draining -- the
			// claim on the freed slot is what matters, not who started the
			// drain.
			if victim := o.specs[victimID]; victim != nil && victimID != specID {
				victim.drainedFor = appendUnique(victim.drainedFor, specID)
			}
			o.beginDrain(victimID)
		}
		// Retried once every victim is confirmed gone (onProcExited calls
		// wakeAdmissionCandidates).
	default: // dec.OK
		o.startProcess(st)
		if st.proc != nil {
			// The further instances that waited for this one to start may go
			// now, side by side with it.
			o.wakeSiblingsWaitingFor(st)
		}
	}
}

// lowerSiblingWaiting reports whether st, a PROTECTED instance k >= 1, has to
// wait for a lower-index sibling to start first (admitAndStart's start-order
// rule), and names the lowest such sibling's index. A sibling waits to start
// when it has no process and rests at Stopped -- waiting for room, for a
// sibling's measurement, or for its own turn -- or at Backoff, waiting out a
// crash-loop delay it will retry after. One resting at a refusal
// (not_permitted, pending_vram_unknown) is not waiting for room: the siblings
// of one spec are admitted against the same demand, budgets and verdicts, so
// st does not start where that sibling was refused either -- it is refused the
// same way, or waits for a measured sibling when its demand is unknown -- and
// holding it back would only hide its own verdict: a request naming it would
// wait out its admission timeout instead of being answered with that refusal. Every lower sibling of a protected instance is
// protected itself, so it always wants up; a missing one (a gap: the floor
// skipped an instance whose address another spec holds) holds nothing back.
//
// Instance 0 never waits here, and neither does a burst instance: burst
// capacity only ever takes room that is free right now and is refused at once
// otherwise, and it leaves again by idle unload, so a lower sibling it shares
// the spec with is kept down only transiently, never for as long as a
// protected instance would run.
func (o *owner) lowerSiblingWaiting(st *specState) (lower int, waits bool) {
	if st.instance == 0 || !st.protected() {
		return 0, false
	}
	base := specBase(st.spec)
	lower = st.instance
	for _, sib := range o.specs {
		if sib.instance >= lower || sib.proc != nil || specBase(sib.spec) != base {
			continue
		}
		if sib.state != StateStopped && sib.state != StateBackoff {
			continue
		}
		if !o.wantsUp(sib) {
			continue
		}
		lower, waits = sib.instance, true
	}
	return lower, waits
}

// wakeSiblingsWaitingFor re-asks, lowest index first, the further instances of
// st's spec that the start-order rule held back (specState.waitingOnSibling),
// now that st has just been given a process -- or has just been refused, which
// ends their wait as surely (setNotPermitted, setPendingVRAMUnknown). Each is
// admitted on its own merits again -- it may still find no room, or wait for
// another lower sibling -- and one that starts or is refused re-asks those above
// it in turn. Without this they would be asked again only at some later event
// (wakeAfterStart when st's start finishes, nothing at all when st is refused),
// so two instances whose crash-loop retries fell due in the wrong order would
// load one after the other instead of side by side, and a request queued on a
// held instance would wait for its admission timeout instead of getting its
// own verdict.
//
// An instance a deeper level of this cascade has already re-asked -- reached
// through a sibling between it and st that started or was refused meanwhile --
// is skipped when that ask settled it: it has a process now, or its verdict
// cleared waitingOnSibling (every verdict rewrites the flag). Asking it again
// would repeat the same admission decision once per level the cascade unwinds
// through, and each decision builds a snapshot, which calls the VRAM measurer
// (a subprocess on a real host). One held back again, behind a sibling that
// still waits, is asked again at no cost: the start-order rule answers before
// any snapshot is built.
func (o *owner) wakeSiblingsWaitingFor(st *specState) {
	base := specBase(st.spec)
	var ids []string
	for id, sib := range o.specs {
		if sib.waitingOnSibling && sib.instance > st.instance && specBase(sib.spec) == base {
			ids = append(ids, id)
		}
	}
	o.sortInstanceOrder(ids)
	for _, id := range ids {
		if sib := o.specs[id]; sib == nil || !sib.waitingOnSibling || sib.proc != nil {
			continue
		}
		o.admitAndStart(id)
	}
}

// dispatchMeasurement asks the installed measurers for the real per-GPU VRAM
// (SetMeasurer) and the real resident host RAM (SetRAMMeasurer) of every live
// managed process, OFF the owner goroutine, and has both answers posted back
// together as one cmdMeasured. The paragraphs below were written for the VRAM
// measurer and are about it unless they say otherwise.
//
// THIS IS THE ONLY PATH THAT EVER MEASURES A SPEC THAT IS MERELY RUNNING, and
// its absence was the F1 defect. buildSnapshot (below) also consults the
// measurer, but it is reached only from admitAndStart and its pid list is
// built from the specs that ALREADY have a live process -- so the spec being
// admitted is never in its own pid list, and a server whose specs are all up
// (the steady state) never reaches it again at all. On a server with exactly
// one managed spec the measurer was therefore invoked exactly once, with a
// zero-length pid list, and never again. Everything downstream then read an
// empty measurement forever: Status.MeasuredVRAM stayed nil, the telemetry
// sample omitted `gpus`, the gateway's write-back never ran,
// `vram_measured_mb` stayed 0, and the measured-wins-over-estimate rule never
// fired -- so an operator who left `vram_estimate_mb` at 0, exactly as the
// documentation invites, stayed in the unknown-demand class permanently and
// could never make the spec co-resident, which is the feature they configured
// the GPU row for.
//
// WHY OFF THE OWNER, AND WHY THAT IS NOT OPTIONAL HERE. The measurer spawns a
// subprocess (nvidia-smi). The owner goroutine is the single serialized owner
// of all state; it also answers Status() for the 1 s telemetry tick and every
// EnsureRunning, over an UNBUFFERED channel, so anything slow running ON it
// parks all of them behind it. buildSnapshot can afford that because an
// admission is occasional -- and an earlier round already had to mitigate
// even that, by caching the measurer's index/UUID map. A RECURRING
// measurement cannot: the cost would stop being occasional and become
// constant. So the owner does only the part that requires owner state --
// reading the live generation set -- and the subprocess goes to its own
// goroutine, whose result returns as just another command. Status() therefore
// never waits on a measurement, which is an existing deliberate property and
// is pinned by
// TestManagerStatusStaysResponsiveWhileAMeasurementIsInFlight.
//
// NO MEASURER INSTALLED IS THE FIRST CHECK, and it is the one that matters
// most: every AMD, Apple unified-memory and CPU-only deployment lands here
// (collector.NewVRAMMeasurer returns nil when the host cannot support the
// platform's measurer at all -- nvidia-smi off PATH everywhere, and on
// Windows also a missing pdh.dll/gdi32.dll export -- and nil is also
// NewManager's default). Those hosts take the first branch,
// spawn nothing, allocate nothing, and behave exactly as they did before this
// function existed. "No measurer" now means neither of the two: the host-RAM
// measurer (SetRAMMeasurer) rides this same beat, and a host that has only that
// one still measures RAM -- see the note on the two gates below.
//
// GATED ON A SPEC DECLARING A GPU, NOT ON BUDGETS EXISTING. Skipping the
// measurement when no GPU budget is configured was considered and rejected:
// the number is not only an input to the budget arithmetic. It is also what
// moves a spec out of the unknown-VRAM class, and §5.3's "must start alone on
// its GPUs" rule applies whether or not a budget exists -- so on a
// budget-less server the measurement is the difference between co-residency
// working and not. It is also what the portal shows as "Measured VRAM
// (agent-reported)", which is the operator's own feedback that leaving the
// estimate at 0 is safe. A GPU-declaring spec is the honest gate: with no
// GPUs declared anywhere there is no index the measurement could be attached
// to, no admission arithmetic it could inform, and no portal row it could
// fill. That gate belongs to the VRAM measurer alone: the RAM measurer has no
// GPU index to need, so it is gated only on a live process existing.
//
// A WSL GENERATION IS MEASURED BY ITS SCOPE, NEVER BY ITS PID (#381). Its pid
// is wsl.exe, a thin client inside which the workload does not run: its few MB
// of private working set would land as a real measurement -- ending the RAM
// bootstrap with most of the model uncharged (vLLM's API server alone is a
// third of its scope) -- and no VRAM is ever found on it. So a wsl target's pid
// goes to neither native measurer. Its RAM comes from the third source riding
// this beat, the WSL sampler (sampleWSL): one wsl.exe call per distro with live
// wsl targets, carrying the agent's fixed script and the units as arguments,
// which answers each scope's cgroup figure and the VM's meminfo. It is gated
// on wsl targets existing and on WSL being usable at all (wslSamplable), not on
// either native measurer: the scope is readable wherever the workload runs.
// The calls share the 2 s bound the native measurers keep (wslSampleTimeout),
// and their answer travels in the same one cmdMeasured.
func (o *owner) dispatchMeasurement() {
	if o.closing || o.measuring {
		return
	}
	mp := o.m.measurer.Load()
	rp := o.m.ramMeasurer.Load()
	if mp == nil && rp == nil && !o.wslSamplable() {
		return
	}
	targets, declaresGPU := o.measurementTargets()
	if len(targets) == 0 {
		return
	}
	// Native pids for the two native measurers, wsl units per distro for the
	// sampler. A wsl target is never in pids (see the paragraph above).
	pids := make([]int, 0, len(targets))
	var wslUnits map[string][]string
	for _, t := range targets {
		if t.wslUnit == "" {
			pids = append(pids, t.pid)
			continue
		}
		if !o.wslSamplable() {
			continue
		}
		if wslUnits == nil {
			wslUnits = make(map[string][]string)
		}
		wslUnits[t.wslDistro] = append(wslUnits[t.wslDistro], t.wslUnit)
	}
	// The two measurers are gated independently, and the asymmetry is the
	// point. The VRAM one keeps the declares-a-GPU gate described above, so a
	// host with nvidia-smi but no GPU-declaring spec still spawns nothing. The
	// RAM one is NOT gated on a GPU: host RAM has no GPU index to attach to,
	// costs one in-process read per pid rather than a subprocess, and the specs
	// whose RAM footprint matters most are exactly the CPU-only ones. Both
	// answers return in ONE cmdMeasured, so there is still at most one
	// measurement outstanding.
	var vramMeasure measurerFunc
	if mp != nil && declaresGPU {
		vramMeasure = *mp
	}
	var ramMeasure ramMeasurerFunc
	if rp != nil && len(pids) > 0 {
		ramMeasure = *rp
	}
	if vramMeasure == nil && ramMeasure == nil && wslUnits == nil {
		// Nothing any source would be asked: only a VRAM measurer and no
		// native spec declares a GPU, or only wsl targets on a host where WSL
		// is not usable.
		return
	}
	// The sampler's inputs, copied off owner state for the goroutine. Its env
	// carries no XDG_RUNTIME_DIR (uid -1): the script reads files, it does not
	// talk to the user's systemd manager.
	run, pol := o.wslRunner, o.policy.WSL
	var env []string
	if wslUnits != nil {
		env = wslHelperEnv(o.getenv, -1)
	}
	o.measuring = true
	// Tracked in m.wg like every other goroutine the owner spawns, so Close
	// cannot return while a measurer is still running (which would let a
	// test's package-var restore race it). The Add happens on the owner
	// goroutine, which Close provably joins before it Waits, so this can
	// never be an Add-after-Wait. It does mean Close waits out one measurer
	// invocation: an installed measurer must therefore bound itself, and the
	// shipped one does (collector.nvidiaMeasureTimeout, 2s, covering both of
	// its subprocess spawns).
	o.m.wg.Add(1)
	go func() {
		defer o.m.wg.Done()
		// Always posts, even for a nil result: the cmdMeasured is what
		// clears o.measuring, so a measurer that answers "nothing" must not
		// wedge the flag. (postCmd drops the command once the owner has
		// stopped, which only happens during Close, where the flag no longer
		// matters.)
		//
		// All three run here, one after the other, and their answers travel
		// together. Each bounds its own runtime (the RAM one is a few
		// in-process reads and returns at once, the WSL sampler and the VRAM
		// measurer 2 s each), and one cmdMeasured keeps the
		// one-measurement-outstanding invariant.
		c := cmdMeasured{targets: targets}
		if ramMeasure != nil {
			c.ramByPID = ramMeasure(pids)
		}
		if wslUnits != nil {
			c.ramByUnit, c.wslVM = sampleWSL(run, pol, env, wslUnits)
		}
		if vramMeasure != nil {
			c.byPID = vramMeasure(pids)
		}
		o.m.postCmd(c)
	}()
}

// wslSamplable reports whether the WSL sampler can run at all: a wsl.exe is
// configured and the user is one the agent may run anything as. Without
// either no wsl generation can have started, and the check keeps a host
// without WSL and without native measurers on dispatchMeasurement's
// allocate-nothing path.
func (o *owner) wslSamplable() bool {
	return o.policy.WSL.Exe != "" && !wslUserRefused(o.policy.WSL.User)
}

// measurementTargets lists the live generations worth measuring right now, or
// nil when there are none. Ordered by spec ID so the pid list handed to an
// external command does not depend on Go's randomized map iteration -- the
// measurer treats it as a set, but a stable order keeps logs and test
// expectations reproducible.
//
// declaresGPU reports whether any live NATIVE spec declares a GPU. It is NOT
// applied here: it is the VRAM measurer's gate (see dispatchMeasurement's
// gating paragraph), and the RAM measurer, which has no GPU axis, must still
// see every live process on a host where no spec declares one. A wsl
// generation does not count: its pid is never handed to the VRAM measurer, so
// it alone gives that measurer nothing to read.
func (o *owner) measurementTargets() (targets []measureTarget, declaresGPU bool) {
	targets = make([]measureTarget, 0, len(o.specs))
	for id, st := range o.specs {
		if st.proc == nil {
			continue
		}
		if len(st.spec.GPUs) > 0 && st.proc.wslUnit == "" {
			declaresGPU = true
		}
		targets = append(targets, measureTarget{
			specID: id, proc: st.proc, pid: st.proc.pid, ready: st.proc.everHealthy,
			wslUnit: st.proc.wslUnit, wslDistro: st.proc.wslDistro,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].specID < targets[j].specID })
	return targets, declaresGPU
}

// realMeasurement keeps only the entries of byGPU that are an actual
// measurement -- strictly positive MB -- and returns nil when none are. The
// result is always freshly allocated, so nothing downstream aliases the map
// the measurer handed back and may reuse.
//
// A measured value of 0 means "unknown", never "this process needs no VRAM" --
// the same meaning SpecGPU.VRAMMB carries for its own 0, and on Windows the
// only meaning available at all: the WDDM driver model puts the OS rather
// than the NVIDIA driver in charge of GPU memory, so
// `nvidia-smi --query-compute-apps=used_memory` answers `[N/A]` for every
// compute app and collector.naInt turns that into a 0. Taken literally, that
// 0 breaks BOTH consumers of a measurement:
//
//   - ADMISSION (buildSnapshot) charges the process 0 MB instead of the
//     operator's estimate, so it contributes nothing to Admit's per-GPU sum
//     (policy.go rule 3): the GPU looks entirely free however much is
//     actually resident on it, and co-residency admission admits without
//     limit -- which is exactly the OOM protection the VRAM budget exists
//     for, gone, on every managed process on every Windows host.
//   - REPORTING (st.measuredVRAM) carries the 0 into every Status(), hence
//     an all-zero `gpus` array on every telemetry sample, once per second
//     per spec, for a value the gateway's ingest discards on arrival
//     (`if g.VRAMMeasuredMB <= 0 { continue }` in agent_ingest.go). That is
//     the F2 cost paid for a number that cannot even reach the portal, where
//     a stored 0 is anyway indistinguishable from "never measured".
//
// That ingest check is where this `<= 0` rule already lives (and what
// agent-runtime-manager.md section 5 documents); this is the same rule at the
// near end of the same loop. It is applied PER GPU INDEX for the same reason
// the gateway's is: a multi-GPU spec can have one index answered and another
// not, and throwing away a real number because a sibling index came back
// unknown would be its own bug.
func realMeasurement(byGPU map[int]int) map[int]int {
	var out map[int]int
	for idx, mb := range byGPU {
		if mb <= 0 {
			continue
		}
		if out == nil {
			out = make(map[int]int, len(byGPU))
		}
		out[idx] = mb
	}
	return out
}

// realRAM is realMeasurement's scalar twin: a measured RAM figure is real only
// when strictly positive, and anything else comes back as 0, "unknown".
//
// The meaning of 0 is the same load-bearing one: a spec's RAM demand of 0 is
// UNKNOWN, never "needs none", so a measured 0 must not be allowed to win over
// an operator's estimate or to be reported as a measurement. A process that has
// not (yet) been read, a pid the measurer could not open, or a platform that
// answered 0 all land here, and the caller treats the result exactly like "not
// measured this cycle": the previous value stays.
func realRAM(v int) int {
	if v <= 0 {
		return 0
	}
	return v
}

// applyMeasurement records a completed measurement against the generations it
// was actually taken for, and re-arms the next dispatch.
//
// The generation check is the whole of the correctness here: a measurement is
// in flight for as long as a subprocess takes, and in that window a spec can
// have exited, restarted, or been replaced by a config change. Writing the
// result onto whatever happens to be running now would attribute one
// process's VRAM to a different one -- and pids are recycled, so the pid
// alone cannot tell them apart.
//
// A target the measurer said nothing about keeps its previous value rather
// than being cleared: nvidia-smi not listing a pid this cycle is a transient
// (a hiccup, a CPU-only child, a race with the process appearing), not
// evidence that the process stopped using VRAM. Only an actual exit clears
// the value, in onProcExited. A value that is not a real measurement counts
// as saying nothing -- see realMeasurement, which is what makes a cycle of
// zeros leave a previous real number in place instead of overwriting it with
// an "unknown" the portal cannot tell apart from a measured zero.
//
// A READY reading that lands -- one dispatched after its process passed a
// health probe (measureTarget.ready) -- is also the one event that can end a
// baseline instance's wait for a measured sibling (#340,
// specState.waitingOnDemand), so such instances are re-asked afterwards; a
// reading of a load in progress cannot end that wait and wakes nobody. And a
// measurement a start asked for while this one was out (remeasure) is
// dispatched now.
//
// A wsl target's RAM is its unit's figure from the WSL sampler, under the same
// generation guard and the same "a non-measurement says nothing" rule, and
// never a figure keyed by its pid (see dispatchMeasurement). A VM reading
// replaces the VM pool's sample together with what the agent's scopes held in
// it -- every wsl target's figure, superseded generations' included: they held
// it when the sample was taken, so it is the VM's to give back (wslRAMBudgetMB).
// A new pool can end a Wait on RAM, so the RAM waiters are re-asked, as a host
// sample re-asks them (wakeOnHostSample).
func (o *owner) applyMeasurement(c cmdMeasured) {
	o.measuring = false
	landed := false
	heldMB := 0
	for _, t := range c.targets {
		if t.wslUnit != "" {
			heldMB += realRAM(c.ramByUnit[t.wslUnit])
		}
		st := o.specs[t.specID]
		if st == nil || st.proc != t.proc {
			continue // superseded generation -- see measureTarget
		}
		// No VRAM is ever measured per pid for a wsl generation: its pid was not
		// asked, and a figure that happens to sit under it belongs to nobody.
		if byGPU := realMeasurement(c.byPID[t.pid]); byGPU != nil && t.wslUnit == "" {
			st.measuredVRAM = byGPU
			// A real measurement resolves the VRAM bootstrap durably: the spec
			// has a number now, so it must never spend (or keep holding) the
			// one-shot eviction. See specState.vramBootstrapDone.
			st.vramBootstrapDone = true
			if t.ready {
				st.readyVRAM = byGPU
				landed = true
			}
		}
		// ONE process per instance, so the pid's figure is the instance's figure:
		// nothing to sum and no key to re-key. The same generation guard
		// above is what keeps a recycled pid's reading off a different
		// process, and realRAM is the same "a non-measurement says nothing"
		// rule, so a cycle of zeros leaves a previous real number in place.
		// For a wsl generation the figure is its scope's, the whole workload
		// tree, keyed by the unit that names exactly this generation.
		ramMB := realRAM(c.ramByPID[t.pid])
		if t.wslUnit != "" {
			ramMB = realRAM(c.ramByUnit[t.wslUnit])
		}
		if mb := ramMB; mb > 0 {
			st.measuredRAM = mb
			// Task 4.4: a successful RAM measurement RESOLVES the cold-start
			// bootstrap -- the RAM twin of vramBootstrapDone above. It resets
			// BOTH bounds: the memory-pressure escalation counter (so a later,
			// unrelated pressure event can earn the one guarded retry again) and
			// the no-measurement termination counter (the interlock will clear on
			// its own now that a number has landed).
			st.ramBootstrapRetries = 0
			st.ramBootstrapBeats = 0
			if t.ready {
				st.readyRAM = mb
				landed = true
			}
		} else if o.healthyBootstrapAwaitingMeasurement(st) {
			// Task 4.4 TERMINATION: a bootstrap that is healthy but still
			// unmeasured holds the RAMBootstrapInFlight interlock (task 4.1), and
			// that interlock makes every OTHER RAM admission on the host Wait. If
			// the measurement never arrives the interlock would hold forever, so
			// this beat counter bounds it: after ramBootstrapMeasureBeats fruitless
			// beats the spec is forced to a bounded StateStartFailed, releasing the
			// interlock. It advances only while a RAM measurer is installed (else
			// there is no beat to resolve it and P4a's interlock is already bounded
			// by Starting), so a measurer-less host never reaches this.
			st.ramBootstrapBeats++
			if st.ramBootstrapBeats >= ramBootstrapMeasureBeats {
				o.failBootstrapUnmeasured(st)
			}
		}
	}
	if c.wslVM != nil {
		o.wslVM, o.wslVMHeldMB = c.wslVM, heldMB
	}
	if landed {
		o.wakeAdmission(wantsDemandWake)
	}
	if c.wslVM != nil {
		o.wakeAdmission(wantsHostSampleWake)
	}
	if o.remeasure {
		o.remeasure = false
		o.dispatchMeasurement()
	}
}

// measureSoon asks for a measurement of the live processes now, or -- when one
// is already out, and dispatchMeasurement allows only one -- the moment that one
// returns (owner.remeasure). It is what a process's first passing health probe
// calls (handleStartResult): see remeasure for why that measurement must not
// wait for the next housekeeping beat.
func (o *owner) measureSoon() {
	if o.measuring {
		o.remeasure = true
		return
	}
	o.dispatchMeasurement()
}

// healthyBootstrapAwaitingMeasurement reports whether st is a live, healthy
// cold-start bootstrap still waiting for its first RAM measurement WHILE a RAM
// measurer is installed -- the exact condition under which the
// RAMBootstrapInFlight interlock holds past Starting (buildSnapshot). It gates
// the no-measurement termination beat counter so that counter never advances for
// a spec that is still loading, has already been measured, has a known demand, or
// runs on a host with no RAM measurer to resolve it.
//
// The demand is read through chargeSpec, exactly as buildSnapshot's interlock
// reads it for a live process, so the beat counter and the interlock it bounds
// always agree.
func (o *owner) healthyBootstrapAwaitingMeasurement(st *specState) bool {
	return o.m.ramMeasurer.Load() != nil &&
		st.proc != nil && st.proc.everHealthy &&
		st.state == StateRunning &&
		needsRAMBootstrap(o.chargeSpec(st), st.measuredRAM)
}

// failBootstrapUnmeasured forces a healthy-but-never-measured bootstrap to a
// bounded StateStartFailed (task 4.4 termination), mirroring handleStartResult's
// start-timeout path exactly: record the failure, fail any queued waiter,
// terminate the live process, and enter backoff. terminateNow marks the stop
// intentional, so the eventual exit rests the spec in backoff via onProcExited's
// deferred-backoff branch rather than being re-read as a crash -- and it never
// reaches the memory-pressure classifier, which is correct: an unmeasured healthy
// process is a measurement failure, not a memory-pressure death.
func (o *owner) failBootstrapUnmeasured(st *specState) {
	proc := st.proc
	st.ramBootstrapBeats = 0
	o.setState(st, StateStartFailed)
	o.recordFailure(st, "runtime: RAM cold-start bootstrap never produced a measurement within the bootstrap deadline", -1, proc.log.Tail(2048))
	o.failPending(st, ErrStartFailed)
	o.terminateNow(st, proc)
	o.enterBackoff(st)
}

// buildSnapshot assembles the pure PolicySnapshot Admit needs from current
// owner state, consulting the installed measurer (if any) for real per-GPU
// VRAM that can raise each spec's pushed demand (never lower it: the demand
// is a floor, the charge is max(live, demand)).
//
// It measures SYNCHRONOUSLY, on the owner, and that is deliberate even though
// dispatchMeasurement (above) now keeps st.measuredVRAM continuously fresh.
// This is the safety-critical read: the arithmetic that decides whether one
// more process may share a GPU should use the newest numbers obtainable at
// the instant it decides, not ones up to one housekeeping beat old. The
// window is not academic -- a spec that started since the last beat would
// otherwise be charged its operator ESTIMATE, and an estimate that
// under-states reality is exactly how a co-resident pair reaches an OOM. An
// admission is occasional, so the cost stays occasional; the recurring cost
// is what had to move off the owner, and it did.
func (o *owner) buildSnapshot() PolicySnapshot {
	// The hardware sample is loaded ONCE and that one value feeds everything
	// below that derives from it -- the detected per-GPU budgets and the
	// host-RAM budget. The collect loop replaces the whole sample from another
	// goroutine about once a second, so two separate loads could straddle a
	// push and give one snapshot a VRAM budget from one sample and a RAM
	// budget from the next.
	hi := o.hostInfo()

	// A wsl generation's pid is wsl.exe, never measured (dispatchMeasurement):
	// it stays out of this read too.
	pids := make([]int, 0, len(o.specs))
	for _, st := range o.specs {
		if st.proc != nil && st.proc.wslUnit == "" {
			pids = append(pids, st.proc.pid)
		}
	}
	var measured map[int]map[int]int
	if mp := o.m.measurer.Load(); mp != nil {
		measured = (*mp)(pids)
	}
	// Whether host RAM is MEASURABLE at all decides how long the cold-start
	// interlock below can safely hold. With a RAM measurer a bootstrapping
	// process's first measurement will arrive, so the interlock can hold from
	// Starting until it does (the brief's "healthy+measured"). WITHOUT one no
	// measurement ever arrives, so holding past Starting would never clear and
	// would hang every other RAM admission on the host; there the interlock is
	// bounded by Starting, and once the process is healthy the conservative budget
	// (its RAM is resident, so MemAvailable has dropped and nothing was added back)
	// accounts for it without an interlock.
	ramMeasurerInstalled := o.m.ramMeasurer.Load() != nil

	running := make([]RunningProc, 0, len(o.specs))
	// residentRAMMB is the host RAM the managed processes ACTUALLY hold right
	// now -- their live measurements only, zero-guarded, never a pushed demand
	// -- summed for the host-RAM budget below.
	residentRAMMB := 0
	// ramBootstrapInFlight is the cold-start RAM interlock (task 4.1), true once
	// any managed process is still in its unaccounted bootstrap window (unknown
	// declared RAM and no measurement -- needsRAMBootstrap), bounded as above. It
	// is recomputed every call from owner state, so it is never a stale flag.
	ramBootstrapInFlight := false
	// ramBoundMB is the same sum read generously, for the ceiling the terminal
	// RAM refusal compares against (see below): a process with no live reading
	// is taken to hold its whole demand floor instead of nothing.
	ramBoundMB := 0
	// Two passes over the live processes. The first records this call's
	// measurements; the second charges every process. They are separate
	// because an instance's demand inherits its siblings' ready measurements
	// (chargeSpec, #340): charging in the same pass would let map order decide
	// whether a sibling's figure is this call's or the previous beat's.
	fresh := make(map[string]map[int]int, len(o.specs))
	for id, st := range o.specs {
		if st.proc == nil {
			continue
		}
		// realMeasurement is the zero-guard: only a strictly positive value
		// is a measurement, so an index the measurer answered with 0 (or
		// less) is absent here and charges the pushed demand below, exactly
		// like an index it never mentioned. Without it a measured 0 WON, and
		// "unknown" became "this model needs no VRAM" -- see realMeasurement
		// for what that costs admission.
		byGPU := realMeasurement(measured[st.proc.pid])
		fresh[id] = byGPU
		if byGPU != nil {
			st.measuredVRAM = byGPU
			// A real measurement resolves the VRAM bootstrap durably, the same as
			// the beat's applyMeasurement: the spec has a number now and must
			// never spend (or keep holding) the one-shot. See
			// specState.vramBootstrapDone.
			st.vramBootstrapDone = true
			// Taken now, on the owner: it is of the loaded model exactly when the
			// process has already passed a health probe, and only then may a
			// sibling inherit it (specState.readyVRAM). A starting process's
			// reading still charges the process itself, below.
			if st.proc.everHealthy {
				st.readyVRAM = byGPU
			}
		}
	}
	for id, st := range o.specs {
		if st.proc == nil {
			continue
		}
		// The charge is max(live, pushed demand). The pushed demand is
		// already max(persisted measurement, operator estimate) -- the
		// gateway builds it that way -- so it is a FLOOR here: a fresher live
		// reading raises it when the process has grown, but a reading below
		// it (a transient dip between allocations) must not lower the
		// reservation. Replacing the demand with the live value is how a
		// co-resident start got admitted into that gap.
		//
		// The demand is the occupant's chargeSpec: for an instance of a
		// replicable spec its own ready measurement where it has one (siblings
		// are summed, each at what it holds), and where it has none -- a
		// sibling still loading, or not yet measured since it became ready --
		// the pushed demand raised to its siblings' largest ready measurement,
		// rather than nothing. For a spec that never replicates it is the
		// pushed spec itself.
		demand := o.chargeSpec(st)
		byGPU := fresh[id]
		gpus := make(map[int]int, len(demand.GPUs))
		for _, g := range demand.GPUs {
			charge := g.VRAMMB
			if v, ok := byGPU[g.Index]; ok && v > charge {
				charge = v
			}
			gpus[g.Index] = charge
		}
		// Host RAM follows the same rule as the VRAM charge above, spec-level
		// instead of per GPU: max(live, pushed demand), the demand a FLOOR. The
		// live figure is st.measuredRAM, which the recurring off-owner beat
		// already stores zero-guarded; realRAM is applied again here because
		// this is where "0 is unknown, never needs-none" is relied on -- a
		// non-positive value must fall through to the demand floor rather than
		// win over it. With neither, RAMMB stays 0 = unknown, which Admit treats
		// as contributing nothing (see rule 6).
		//
		// The same live figure, WITHOUT the demand floor, is what this process
		// is really holding, and it is what the host-RAM budget adds back (see
		// hostRAMBudgetMB): the floor is a reservation, not residency.
		liveRAM := realRAM(st.measuredRAM)
		residentRAMMB += liveRAM
		if liveRAM > 0 {
			ramBoundMB += liveRAM
		} else {
			ramBoundMB += demand.RAMMB
		}
		// The cold-start interlock (task 4.1): this process is still unaccounted
		// if its RAM demand is unknown and nothing has measured it yet. It holds
		// until measured when a measurer will deliver that measurement, else only
		// while it is Starting -- see ramMeasurerInstalled above for why. A
		// sibling's inherited measurement accounts for it as surely as its own.
		if needsRAMBootstrap(demand, st.measuredRAM) && (st.state == StateStarting || ramMeasurerInstalled) {
			ramBootstrapInFlight = true
		}
		running = append(running, RunningProc{
			SpecID: id,
			// The instance fields (#340) make siblings siblings to admission:
			// they co-reside, are never one another's victims, and leave
			// highest index first. A running burst instance is Surplus, the
			// cheapest victim there is; a protected one is Pinned (see
			// pinnedForAdmission). For a spec that never replicates they read
			// exactly as a pre-instance process: its own base, instance 0, not
			// surplus, Pinned = spec.Pinned.
			Spec:     specBase(st.spec),
			Instance: st.instance,
			Surplus:  st.burst(),
			Member:   st.spec.IsRPCServer(),
			GPUs:     gpus,
			RAMMB:    max(liveRAM, demand.RAMMB),
			InFlight: st.inFlight,
			Pinned:   st.pinnedForAdmission(),
			Role:     roleOf(st.spec.Role),
			LastUsed: st.lastUsed,
			// A process that has been exec'd but never passed a health
			// probe. It occupies its resources exactly like a running one
			// (which is why it is in this list at all) but must never be
			// evicted -- see isEvictable for the fork-exec storm that
			// produced.
			Starting: st.state == StateStarting,
			// Read off the generation, not st.spec: what holds VM memory is
			// the process that runs, and a push may have changed the spec's
			// mode while the old generation drains.
			WSL: st.proc.wslUnit != "",
		})
	}

	// Every row is copied verbatim, zero budgets included. Do NOT add a
	// "skip BudgetMB == 0" filter here: Admit already treats a 0 budget and
	// an absent index identically (PolicySnapshot.Budgets), and it owns that
	// rule because it is the only code that interprets a budget, whereas a
	// snapshot can be built by any caller. A second copy of the rule here
	// would be one more place for the two to drift apart.
	//
	// Budgets the operator did not set are then filled in from the detected
	// device totals (fillDetectedBudgets), which is what makes VRAM accounting
	// default-on. That is a different thing from filtering: it adds a value,
	// it never removes or reinterprets a row.
	budgets := make(map[int]int, len(o.cfg.GPUBudgets))
	for _, b := range o.cfg.GPUBudgets {
		budgets[b.Index] = b.BudgetMB
	}
	o.fillDetectedBudgets(budgets, hi)

	return PolicySnapshot{
		Running:      running,
		MaxProcesses: o.cfg.MaxProcesses,
		// The clock and the grace window travel together: Admit never reads a
		// clock, so the snapshot is stamped here, once, and every LastUsed in
		// Running is measured against this one instant. The grace is copied
		// verbatim (0 = off); isEvictable is the one place that interprets it.
		Now:             time.Now(),
		EvictionGrace:   o.evictionGrace,
		Budgets:         budgets,
		HostRAMBudgetMB: hostRAMBudgetMB(hi, residentRAMMB, o.hostRAMReserveMB),
		// The ceiling is the budget's generous twin, for the terminal
		// own-demand refusal only: the same pool, except that a process with no
		// reading counts at its demand floor and the RAM of processes that have
		// exited since the sample was taken is counted as already back. Each
		// of those is a way the budget is understated for a moment without the
		// host being short of anything -- a model still loading, a victim that
		// just made room for this very candidate -- and a durable refusal must
		// not be issued on the strength of either.
		HostRAMCeilingMB:     hostRAMBudgetMB(hi, ramBoundMB+o.pendingRAMFreedMB(), o.hostRAMReserveMB),
		RAMBootstrapInFlight: ramBootstrapInFlight,
		// Copied verbatim; Admit is the one place that interprets it (0 = off),
		// like every budget. It applies only to an unknown-RAM candidate.
		RAMBootstrapGuessMB: o.ramBootstrapGuessMB,
		// The WSL VM pool and its ceiling, both 0 (unconstrained) until a
		// sampling call has answered; see wslRAMBudgetMB for why the pool's
		// add-back comes from the sample rather than the live set above.
		WSLRAMBudgetMB:  o.wslRAMBudget(),
		WSLRAMCeilingMB: wslRAMCeilingMB(o.wslVM, o.wslRAMReserveMB),
		Allowed:         o.allowedPairs,
	}
}

// wslRAMBudget is the WSL VM RAM pool as of the latest sample (wslRAMBudgetMB).
func (o *owner) wslRAMBudget() int {
	return wslRAMBudgetMB(o.wslVM, o.wslVMHeldMB, o.wslRAMReserveMB)
}

// hostRAMBudgetMB is the host-RAM pool admission budgets managed processes
// against: the capacity available to the MANAGED set, i.e. the host's LIVE
// available memory (the sample's MemAvailable) PLUS the RAM the managed
// processes already hold (residentMB, their summed live measurements), minus
// the operator's reserve.
//
// Why the add-back. MemAvailable is what the host can still hand out, so it
// already EXCLUDES every byte a managed process holds. Rule 6 then sums those
// same processes' RAM into its left-hand side, so a pool of plain
// MemAvailable-minus-reserve would charge them twice and cap the managed set
// at roughly half of what the host can really hold -- evicting, or Waiting, for
// starts that fit. Adding the held RAM back makes the pool a TOTAL for the
// managed set, the host-wide analogue of the per-GPU VRAM budget (a device
// total, not device-available); in steady state the add-back and the sum's
// running terms cancel, and a candidate is charged against genuinely free RAM.
// Only the live MEASUREMENT is added back, never a process's demand: the demand
// is a floor on what is reserved, not on what is resident, so a floored process
// that has not (yet) grown into it keeps charging the difference and a process
// with no reading adds back nothing (the conservative direction).
//
// Live-available rather than total-minus-reserve is still the self-correcting
// choice on a shared host: a co-located workload that grows lowers MemAvailable
// and so the pool on the next sample, with nothing to reconfigure.
//
// Two cases deliberately look alike and are not:
//
//   - No reading (MemAvailableMB <= 0: no sample yet, or the read failed) is
//     UNKNOWN, not an exhausted host, and yields 0 -- the UNCONSTRAINED
//     sentinel, same as an undetected GPU's budget -- so RAM admission is a
//     no-op until the host has actually been looked at. What the managed
//     processes hold cannot turn that into a constraint.
//   - A real reading whose pool is at or below zero is the tightest host there
//     is and yields 1, never 0. A plain max(0, ...) would hand the most
//     RAM-starved host the one value that means "admit anything", the opposite
//     of keeping everything back; fillDetectedBudgets floors its VRAM budget at
//     1 for the identical reason. 1 MB is a real ceiling, so Admit refuses
//     every spec with a known RAM demand until the host recovers.
//
// The same arithmetic builds PolicySnapshot.HostRAMCeilingMB, with a generous
// held figure in place of the measured one (see buildSnapshot): residentMB is
// then "what the managed processes could be holding", not "what they have been
// measured to hold".
func hostRAMBudgetMB(hi HostInfo, residentMB, reserveMB int) int {
	if hi.MemAvailableMB <= 0 {
		return 0
	}
	return max(1, hi.MemAvailableMB+residentMB-reserveMB)
}

// ramFreedSettlePushes is how many samples must land after a managed process
// exits before the pool is trusted to show the memory it freed: the one that is
// current at the exit may have been taken before it, and the next one may have
// been in flight; only the second is certain to have been taken afterwards,
// because the collect loop takes a sample only after pushing the previous one.
const ramFreedSettlePushes = 2

// noteRAMFreed records, as a managed process exits, the host RAM it held: its
// last live reading, or its demand floor when it was never measured (the
// generous reading is the point -- see below). Admission can only learn that the
// memory is back from the next hardware samples, and until then the pool is
// understated by exactly this much: the process has already left the running
// set, so its add-back is gone, while MemAvailable has not yet risen. That is
// the state in which the beneficiary of an eviction is re-admitted, and it is
// why the terminal own-demand refusal compares against a ceiling that counts
// this credit (HostRAMCeilingMB) rather than against the budget.
//
// The credit lives until ramFreedSettlePushes samples have landed (hostSeq), and
// further exits inside that window add to it and extend it. It is never part of
// the admission arithmetic -- the budget does not get it -- so a stale credit can
// make a durable refusal less eager, never admit something that does not fit.
// Nothing is recorded before the first sample: with no MemAvailable reading
// there is no pool to understate.
func (o *owner) noteRAMFreed(st *specState) {
	if o.m.hostInfo.Load() == nil {
		return
	}
	mb := realRAM(st.measuredRAM)
	if mb == 0 {
		// The instance's demand floor exactly as buildSnapshot charged it
		// (chargeSpec: sibling-inherited, #340); the pushed spec for one that
		// never replicates.
		mb = o.chargeSpec(st).RAMMB
	}
	if mb <= 0 {
		return
	}
	seq := o.m.hostSeq.Load()
	if seq >= o.ramFreedUntilSeq {
		o.ramFreedMB = 0 // the previous credit has already settled
	}
	o.ramFreedMB += mb
	o.ramFreedUntilSeq = seq + ramFreedSettlePushes
}

// pendingRAMFreedMB is the unsettled credit noteRAMFreed has recorded, 0 once
// enough samples have landed to have absorbed it.
func (o *owner) pendingRAMFreedMB() int {
	if o.ramFreedMB > 0 && o.m.hostSeq.Load() >= o.ramFreedUntilSeq {
		o.ramFreedMB = 0
	}
	return o.ramFreedMB
}

// fillDetectedBudgets makes VRAM accounting default-on: for every GPU the
// hardware sample hi reported, a budget the operator did not set is filled in from
// the detected device total, minus the optional headroom. It edits budgets in
// place, after the operator's rows have been copied verbatim.
//
// Per GPU index the rule is:
//
//   - an operator budget > 0 WINS and is left exactly as configured (no
//     headroom, no clamp to the hardware -- it is the operator's word);
//   - otherwise, a detected total > 0 gives max(1, total - headroom);
//   - otherwise the index is left as it is: absent, or a verbatim 0 row, i.e.
//     unconstrained -- the pre-feature behaviour, which is what a host with no
//     GPU, or one that has not produced its first sample, gets.
//
// A configured row of 0 (or less) is "no budget set", not an override -- the
// portal seeds a fresh row at 0 -- so the detected value replaces it. This is
// NOT a "skip BudgetMB == 0" filter on the operator's rows (see the caller):
// the row is still copied verbatim whenever nothing was detected, and Admit
// remains the only code that interprets a budget <= 0 as unconstrained.
//
// The floor of 1 on the detected value is not cosmetic. A headroom at or above
// the device total would otherwise yield 0, and 0 means UNCONSTRAINED -- the
// opposite of "keep all of it back". 1 MB is a real ceiling, so Admit refuses
// any spec with a known VRAM demand on that GPU, which is what reserving the
// whole device means.
//
// An Apple GPU is skipped entirely: it has no dedicated VRAM, and the
// "total" its collector reports is the system memory the GPU has allocated at
// that instant, which moves with load and is not a ceiling. Budgeting against
// it would refuse a spec larger than whatever is allocated right now.
func (o *owner) fillDetectedBudgets(budgets map[int]int, hi HostInfo) {
	if o.gpuVendor == GPUVendorApple {
		return
	}
	for idx, total := range hi.GPUTotalsMB {
		if total <= 0 || budgets[idx] > 0 {
			continue
		}
		budgets[idx] = max(1, total-o.vramHeadroomMB)
	}
}

// startProcess resolves the listen port, expands placeholders, writes the
// spec's config files, execs the child (never blocking the owner loop beyond
// the exec.Cmd.Start() call itself and the few small, size-bounded config-file
// writes just before it), and hands off health-waiting and exit-waiting to
// their own goroutines.
func (o *owner) startProcess(st *specState) {
	spec := st.spec

	// R2-1 fix, part 1: validate placeholder expansion and env BEFORE
	// acquiring any resource. A spec that can never launch (a spec.Env key
	// of PATH/HOME, ${AGENT_ENV:OP_AGENT_*}, a missing ${AGENT_ENV:NAME},
	// or a malformed placeholder) should fail before grabEphemeralPort's
	// real net.Listen/Close pair, not after -- previously EVERY retry of a
	// spec broken this way paid that syscall cost first. The port value
	// itself never affects whether ExpandPlaceholders errors (it only
	// substitutes a decimal string for ${PORT}), so this dry run with a
	// placeholder port is a faithful pre-check; the real port is
	// substituted in the second call below once one is available.
	if _, _, err := ExpandPlaceholders(spec, 0, o.gpuVendor, o.gpuVendors, o.getenv, o.configFilesDir); err != nil {
		o.setNotPermitted(st, err.Error())
		return
	}
	// The config_files half of the same dry run (#257): a secret or malformed
	// placeholder in a file's content, an OS-unsafe file name, or a spec ID that
	// would escape config_files_dir can never succeed on retry, so it is
	// reported exactly like a bad Args placeholder -- not_permitted,
	// rate-limited -- and before a port is acquired or anything is written. The
	// checks are port-independent for the same reason the one above is. Only the
	// filesystem writes, further down, are start failures.
	if _, err := planSpecConfigFiles(spec, 0, o.gpuVendor, o.gpuVendors, o.getenv, o.configFilesDir); err != nil {
		o.setNotPermitted(st, err.Error())
		return
	}

	port := spec.ListenPort
	if port == 0 && !spec.IsRPCServer() {
		p, err := grabEphemeralPort()
		if err != nil {
			o.setState(st, StateStartFailed)
			o.recordFailure(st, "runtime: grab ephemeral port: "+err.Error(), 0, "")
			o.failPending(st, ErrStartFailed)
			o.enterBackoff(st) // I2 fix: rate-limit / retry a persistently failing start, same as any other start_failed
			return
		}
		port = p
	}

	ex, err := expandSpec(spec, port, o.gpuVendor, o.gpuVendors, o.getenv, o.configFilesDir)
	if err != nil {
		// Already validated above (that dry run used port 0); reaching
		// here would mean ExpandPlaceholders' result depends on the port
		// value, which it does not. Defensive only.
		o.setNotPermitted(st, err.Error())
		return
	}

	// Provision the spec's config files BEFORE anything is exec'd: the child
	// reads them at startup, so they must already exist, complete, with the real
	// ${PORT} in them. Everything deterministic was settled by the dry run above,
	// so an error here is a genuine filesystem failure (unwritable directory,
	// full disk, failed rename) -- a start failure that backs off and retries,
	// the same shape as the ephemeral-port failure above, and NOT a
	// not_permitted verdict, which would claim the config can never work.
	if err := writeSpecConfigFiles(spec, port, o.gpuVendor, o.gpuVendors, o.getenv, o.configFilesDir); err != nil {
		o.setState(st, StateStartFailed)
		o.recordFailure(st, err.Error(), 0, "")
		o.failPending(st, ErrStartFailed)
		o.enterBackoff(st) // I2 fix: rate-limit / retry a persistently failing start, same as any other start_failed
		return
	}

	if spec.IsRPCServer() {
		// A member's command line is the operator's arguments plus the
		// agent-owned -d/-H/-p/-c, and its port is the one it binds. Everything
		// here is deterministic, so a failure is a verdict, not a start failure.
		// ex grows by those entries, so what is reported below is what runs.
		var err error
		if ex, port, err = o.prepareMember(spec, ex); err != nil {
			o.setNotPermitted(st, err.Error())
			return
		}
	}
	args, env := ex.args, ex.env
	// The reportable form of this exact launch, built from the SAME expansion
	// (never a second one that could disagree with it) and masked here, at the
	// point of substitution, so no plaintext resolved command is ever retained
	// anywhere. See command.go for the masking rule and what it does and does
	// not protect.
	resolved := ex.resolvedCommand(spec, o.specsFromLocalFile)

	// A wsl spec runs as the configured distro user under that user's own
	// systemd manager, whose bus is named after the user's UID inside the
	// distro. admitAndStart admits a wsl instance only once that UID is cached,
	// in this same turn, so it is there; should it ever not be, the instance
	// goes back to waiting for the off-owner lookup rather than block here.
	// Nothing is exec'd and no generation is opened.
	distroUID := -1
	if spec.IsWSL() {
		uid, ok := o.wslUIDs[spec.WSLDistro()]
		if !ok {
			st.waitingOnWSLUID = true
			o.requestWSLUID(spec.WSLDistro())
			return
		}
		distroUID = uid
	}

	// One writer for BOTH streams: the requirement is stdout AND stderr, and
	// sharing the writer means the interleaving an operator reads back is
	// the interleaving the process actually produced. procLog.Write is
	// concurrency-safe by construction, whatever goroutines os/exec copies on.
	//
	// Into the BASE spec's buffer for every instance (#340): the spec's log
	// view is where an operator looks, for a burst instance whose row is
	// already retired too, and a further instance's lines carry "[#k] " so the
	// instances can be told apart there. Instance 0 writes exactly as a spec
	// that never replicates always has.
	plog := o.logs.newInstanceProc(specBase(spec), st.instance)
	var cmd *exec.Cmd
	var wslUnit string
	execPath := spec.Binary
	if spec.IsWSL() {
		// wsl.exe -d <distro> -u <user> --cd <dir> --exec systemd-run --user
		// --scope --unit=<unit> -- <binary> <args...> (buildWSLLaunch). The unit
		// is named after plog's generation, which is unique for the agent's
		// lifetime, plus this run's nonce, so it addresses this generation and
		// nothing else: a late stop of a predecessor cannot reach a successor,
		// and the orphan sweep can tell this run's units from an earlier run's.
		// cmd.Dir stays empty: the directory that matters is inside the distro
		// (--cd), and wsl.exe needs none of its own. The reported command is
		// the wrapper as exec'd, with the operator's arguments masked at their
		// shifted positions (withWrapper).
		wslUnit = wslUnitName(o.wslNonce, spec.ID, plog.gen)
		l := buildWSLLaunch(o.policy.WSL, distroUID, spec.WSLDistro(), wslUnit, effectiveWSLWorkDir(spec), spec.Binary, args, env)
		resolved = ex.withWrapper(l).resolvedWSLCommand(spec, l.Exe, o.specsFromLocalFile)
		execPath = l.Exe
		cmd = exec.Command(l.Exe, l.Args...) //nolint:gosec // l.Exe is the operator's runtime_wsl_exe; the distro, binary and user were permitted by LocalPolicy.Permit above
		cmd.Env = l.Env
	} else {
		cmd = exec.Command(spec.Binary, args...) //nolint:gosec // spec.Binary is allowlisted by LocalPolicy.Permit above
		cmd.Env = env
		// R2: an empty work_dir runs beside the binary. effectiveWorkDir returns
		// spec.WorkDir verbatim when set, else filepath.Dir(spec.Binary) -- which,
		// for an allowlisted (absolute) binary, is never empty, so cmd.Dir is now
		// always set. The reported ResolvedCommand (command.go) reads the same
		// helper, so the log panel always names the directory the child really ran
		// in. Never write this back into st.spec.WorkDir: reconcile diffs the stored
		// spec with reflect.DeepEqual, and a rewrite would read as a changed spec.
		cmd.Dir = effectiveWorkDir(spec)
	}
	cmd.Stdout = plog
	cmd.Stderr = plog
	// For wsl.exe too: on unix (the test fake) the group is what killGroup, the
	// wsl kill's backstop, signals; on Windows it is a no-op either way.
	setProcGroup(cmd)

	if err := cmd.Start(); err != nil {
		// The command is retained even though nothing ran: a spec whose exec
		// fails prints nothing at all, so this is the only thing an operator
		// opening the log view can be shown -- and "the binary/work_dir it
		// actually tried" is usually the answer.
		plog.StartFailed(resolved)
		o.setState(st, StateStartFailed)
		o.recordFailure(st, fmt.Sprintf("runtime: exec %s: %s", execPath, err.Error()), 0, "")
		o.failPending(st, ErrStartFailed)
		o.enterBackoff(st) // I2 fix: rate-limit / retry a persistently failing start, same as any other start_failed
		return
	}

	// Records the generation boundary in the spec's own history, in place,
	// so a crash loop reads as a sequence of attempts rather than one
	// undifferentiated wall of output -- and, with it, the command THIS
	// generation was launched with, so the panel above the output can never
	// describe a different attempt than the one being read.
	plog.Started(cmd.Process.Pid, resolved)

	proc := &runningProc{
		specID:    spec.ID,
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		port:      port,
		log:       plog,
		startedAt: time.Now(),
		exited:    make(chan struct{}),
	}
	if wslUnit != "" {
		proc.wslUnit, proc.wslDistro = wslUnit, spec.WSLDistro()
		proc.ctl = wslControl{
			run:    o.wslRunner,
			pol:    o.policy.WSL,
			distro: proc.wslDistro,
			unit:   wslUnit,
			env:    wslHelperEnv(o.getenv, distroUID),
			cmd:    cmd,
			wg:     &o.m.wg,
			exited: proc.exited,
			specID: spec.ID,
		}
	} else {
		proc.ctl = nativeControl{cmd: cmd}
	}
	st.proc = proc
	o.setState(st, StateStarting)
	// I3 fix: an admission-wait timer exists to bound how long a request
	// waits for a SLOT to free up; once the target has actually started,
	// the request is no longer waiting on admission at all -- it is
	// waiting on startup, which has its own bound
	// (spec.StartupTimeoutSeconds, enforced by pollHealth). Without this,
	// the admission-wait timer kept running through startup and could fire
	// a misleading ErrAdmissionBlocked for a model that was seconds away
	// from becoming healthy.
	for _, w := range st.pending {
		cancelTimer(o.m, &w.timer)
	}
	if wslUnit != "" {
		slog.Info("runtime: process starting", "spec", spec.ID, "binary", spec.Binary, "port", port, "pid", proc.pid, "wsl_distro", proc.wslDistro, "wsl_unit", wslUnit)
	} else {
		slog.Info("runtime: process starting", "spec", spec.ID, "binary", spec.Binary, "port", port, "pid", proc.pid)
	}

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

	if spec.IsRPCServer() {
		o.pollMemberReady(specID, proc, spec, deadline)
		return
	}

	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}
	probeTimeout := time.Duration(spec.HealthTimeoutSeconds) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	url := endpointFor(proc.port) + healthPath
	// M5 fix: a private transport with keep-alives disabled, instead of
	// http.DefaultTransport's global connection pool -- the loopback port
	// this dials is a recyclable OS-assigned ephemeral port, so an idle
	// keep-alive connection left in the global pool after this generation
	// is killed could otherwise be reused against a completely different
	// process that is later assigned the same port number.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

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
	// B6 fix: a generation that is already being torn down is superseded just
	// as surely as one whose map entry was replaced -- the pointer identity
	// check above cannot see it, because a drain does not swap st.proc, it
	// signals the process and waits for the exit. Before this, BOTH branches
	// below overwrote beginDrain's deliberate StateDraining with an
	// unconditional setState:
	//
	//   - ok: StateRunning for a process that has already been SIGTERM'd,
	//     which puts the spec back into LoadedModels() -- i.e. into the
	//     AUTHORITATIVE loaded_models telemetry field and the router's
	//     /running -- so the gateway routes fresh requests at a process the
	//     agent is shutting down; succeedPending hands queued waiters that
	//     same endpoint; and it does not end with the process, because
	//     onProcExited's intentional branch only advances Draining->Stopped,
	//     leaving the spec stuck in StateRunning with proc == nil (observed:
	//     `State:running PID:0 Port:0`), advertising a model that is not
	//     loaded until some later request happens to start it again. Nothing
	//     self-heals it: scanIdle skips proc == nil and handleEnsure starts a
	//     fresh generation without correcting the state.
	//   - err: StateStartFailed + a spurious failures++ + backoff for a spec
	//     that was deliberately stopped (force_stopped, removed, or evicted
	//     as an admission victim) -- and, during Close, a backoff timer
	//     scheduled after handleClose already ran, which m.wg then makes
	//     Close wait out (see enterBackoff's own closing guard).
	//
	// Returning is safe and complete: the drain already owns this
	// generation's teardown (terminateNow's SIGTERM plus the killGrace
	// escalation), so the exit is reported and classified by onProcExited
	// exactly as the drain intends. Queued waiters stay queued and are woken
	// by wakeAdmissionCandidates once the process is gone -- a drain-then-start,
	// which is what a request for a spec being drained should get.
	if c.ok {
		// Recorded BEFORE the draining discard below (fix round 1, M6):
		// everHealthy is a fact about this PROCESS -- "this generation passed a
		// health probe" (see its own field comment; I4 turns on exactly that
		// distinction from the display state) -- so a result whose state
		// transitions are discarded must still leave the fact true. Discarding
		// it too would leave a generation that WAS healthy classified, on a
		// non-intentional exit, as "exited before becoming healthy".
		//
		// LOAD-BEARING, and this comment used to claim the exact opposite
		// (fix round 2, G1): that no reachable behavior depends on the line
		// and no test can therefore fail on it -- an invitation to delete a
		// guard a test now covers
		// (TestManagerHealthyGenerationDrainedAfterItsOwnExitIsStillACrash).
		//
		// The argument was that a discarded ok result implies st.inFlight == 0,
		// hence that beginDrain terminated the generation at once and set
		// intentionalStop, and that wasHealthy is only read on the
		// NON-intentional branch. It breaks at terminateNow: its
		// already-exited early return (M2, part 1) returns WITHOUT setting
		// intentionalStop, so a generation whose child is ALREADY GONE when
		// the drain runs lands on exactly that non-intentional branch.
		//
		// FIFO orders the command queue, not wall-clock events. m.cmds is
		// unbuffered, so anything that stalls the owner parks every event
		// behind it -- and buildSnapshot's measurer call sits inside
		// admitAndStart, ahead of the same call's Evict/beginDrain, so ONE
		// command can stall, let the child pass its health probe and then die,
		// and only then drain it. In production that stall is any slow
		// measurer (nvidia-smi), a concurrent exec, or a long applyConfig; the
		// child needs only to answer /health and then die, e.g. an OOM after
		// load. Without this line that child is reported as start_failed and
		// its queued request is failed with ErrStartFailed (a 503) instead of
		// surviving the backoff -- the exact inversion I4 exists to prevent.
		c.proc.everHealthy = true
	}
	if st.state == StateDraining {
		slog.Debug("runtime: discarding start result for a draining generation", "spec", c.specID, "ok", c.ok)
		return
	}
	if c.ok && c.deviceMismatch != "" {
		// The member is listening but exposes the wrong devices: serving
		// requests with it would silently shrink the cluster. Refuse it before
		// it is ever reported running; only a config change can fix it.
		o.setTerminalVerdict(st, c.deviceMismatch, codeRPCDeviceCountMismatch)
		o.terminateNow(st, c.proc)
		return
	}
	if c.ok {
		st.rpcProtocol = c.rpcProtocol
		st.terminalSpec, st.terminalCode = nil, ""
		if st.hasRunBefore {
			st.restarts++
		}
		st.hasRunBefore = true
		// The measured cold start: exec to the first passing probe of
		// THIS start, recorded before the transition so the status it wakes a
		// reader for already carries it. Floored at 1 so a start faster than a
		// millisecond still reads as measured -- 0 is "unknown".
		st.startupMS = max(1, int(time.Since(c.proc.startedAt).Milliseconds()))
		o.setState(st, StateRunning)
		st.lastError = nil
		// I1 fix: do NOT reset st.failures here. Resetting it on every
		// successful start meant the NEXT crash always recomputed
		// backoffDelayFor(1) -- the base delay -- so backoff never
		// escalated, and handleStableRun's stable-run reset (which already
		// implements the reset the brief actually asked for) was
		// unreachable dead code. failures now resets ONLY via
		// handleStableRun, after a run has been stable for
		// stableRunThreshold.
		proc := c.proc // everHealthy is already set above, ahead of the draining discard
		proc.stableTimer = o.m.scheduleAfter(stableRunThreshold, func() {
			o.m.postCmd(cmdStableRun{specID: c.specID, proc: proc})
		})
		o.succeedPending(st, c.specID)
		// A new live process exists, and this is the best moment there is to
		// measure it: a model server has finished mapping its weights by the
		// time it answers a health probe, so the first measurement is
		// already the meaningful one instead of catching the middle of a
		// load. The housekeeping beat then keeps it current as the KV cache
		// grows. Non-blocking -- see dispatchMeasurement -- and never skipped:
		// with a measurement already out it follows that one (measureSoon),
		// because a sibling's first figure is what lets the next instance of
		// its spec start (#340).
		o.measureSoon()
		// After succeedPending on purpose: a start that served waiters is
		// busy for this wake, so a request queued for another spec Waits for
		// their release instead of evicting the process just handed out.
		o.wakeAfterStart()
		return
	}

	if c.relayDown != "" {
		// The rpc-server came up but its relay never listened, so the head
		// could not have reached it.
		o.setState(st, StateStartFailed)
		o.recordFailure(st, c.relayDown, 0, "")
		st.lastError.Code = codeBindMismatch
		st.bindMismatch = true
		o.failPending(st, ErrStartFailed)
		o.terminateNow(st, c.proc)
		o.enterBackoff(st)
		return
	}

	// c.err is ErrStartTimeout: the child never became healthy in time.
	tail := c.proc.log.Tail(2048)
	o.setState(st, StateStartFailed)
	o.recordFailure(st, "runtime: startup timed out waiting for health", 0, tail)
	o.failPending(st, ErrStartTimeout)
	o.terminateNow(st, c.proc)
	// I2 fix: rate-limit retries of a spec that never becomes healthy
	// (non-pinned) and give a PINNED spec whose start always fails a
	// retry at all (handleBackoffFire already restarts pinned specs
	// correctly once the backoff elapses).
	o.enterBackoff(st)
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
	// Task 4.4: capture the cold-start RAM bootstrap's memory-pressure
	// classification inputs BEFORE the lines below clear them -- st.measuredRAM
	// (the last RSS) is zeroed a few lines down, and the host-RAM budget is read
	// off the LIVE process set, which still includes this one only while st.proc
	// is non-nil. Classification reads the EXIT SIGNATURE and the LAST
	// MEASUREMENT, never free text (bootstrapDiedOfMemoryPressure). The escalation
	// itself fires only on a non-intentional, non-removed exit, far below.
	//
	// Read off the instance's admission demand (demandSpec): an instance whose
	// running sibling has been measured was admitted with that figure and is no
	// bootstrap (#340). A burst instance never is one either -- it is never
	// admitted with an unknown demand, and it never evicts anything, which is
	// all the escalation does.
	bootstrapSpec := !st.burst() && specHasUnknownRAM(o.demandSpec(st))
	oomSignaled := killedBySIGKILL(exitErr)
	lastMeasuredRAM := st.measuredRAM
	ramBudgetAtDeath := 0
	if bootstrapSpec {
		// Only the bootstrap classifier reads this, and it loops the spec set, so
		// skip it for every ordinary (known-RAM) exit. A wsl generation is
		// weighed against the VM pool it lived in, not the host's (ramPoolAtDeath).
		ramBudgetAtDeath = o.ramPoolAtDeath(proc)
	}

	cancelTimer(o.m, &proc.killTimer)
	cancelTimer(o.m, &proc.drainTimer)
	cancelTimer(o.m, &proc.stableTimer)
	st.proc = nil
	st.rpcProtocol = ""
	st.relaunchDeferred = false
	// A hold and a reservation are claims on a running member: with its process
	// gone there is nothing left to hold, and the start a reservation covered has
	// ended. The admission wake that follows every exit re-asks whoever waited
	// for the room.
	o.dropClaim(st)
	// C3 fix: the generation's requests are dead by definition. Without
	// this, a request in flight when the child crashed left InFlight stuck
	// above zero forever, and Admit's isEvictable (which requires
	// InFlight==0) then treated this spec as permanently busy, as if
	// pinned -- never evictable again, and never idle-unloadable.
	st.inFlight = 0
	// F2 fix: a measurement describes A LIVE PROCESS, and this one is gone.
	// Keeping it made Status() report an exited process's VRAM indefinitely,
	// which the agent then republished on every 1 s telemetry sample and the
	// gateway rewrote to the store on every one of them -- an idle overnight
	// server with a handful of measured specs across two cards produced on
	// the order of a million identical UPDATEs a day for a table with a dozen
	// rows. Cleared for EVERY kind of exit (crash, drain, idle unload,
	// force_stopped, shutdown): none of them leaves the number true.
	st.measuredVRAM = nil
	// Same reasoning, and the same F2 cost, for the host-RAM figure: it
	// describes this generation's process and is untrue the moment it exits.
	// What it held is recorded first, though: the hardware sample has not seen
	// the memory come back yet (noteRAMFreed).
	o.noteRAMFreed(st)
	st.measuredRAM = 0
	// And for what a sibling inherits from this process (#340): an exited
	// process holds nothing, so its figures must not raise anyone's demand.
	st.readyVRAM = nil
	st.readyRAM = 0

	wasIntentional := st.intentionalStop
	st.intentionalStop = false
	// A backoff deferred by enterBackoff because this generation was still
	// alive. Cleared unconditionally: the non-intentional branch below
	// records its own failure and calls enterBackoff itself (with st.proc
	// now nil, so it takes effect immediately), and leaving the flag set
	// would schedule a second, redundant backoff timer for the same failure.
	deferredBackoff := st.backoffOnExit
	st.backoffOnExit = false
	removed := st.removed
	// Whoever this generation was evicted FOR gets its admission attempt
	// before this spec may re-admit itself -- see specState.drainedFor.
	// Consumed unconditionally: the claim belongs to THIS generation's exit,
	// and a later drain records its own beneficiaries.
	drainedFor := st.drainedFor
	st.drainedFor = nil
	// I4 fix: classify by whether THIS GENERATION ever passed a health
	// probe, not by the current display state. A healthy, serving process
	// that happens to exit on its own (for an unrelated reason) while
	// State is already Draining -- e.g. mid the in-flight drain wait,
	// before the owner ever signalled it -- must still be reported as a
	// crash, not "start_failed / never became healthy".
	wasHealthy := proc.everHealthy

	slog.Info("runtime: process exited", "spec", st.spec.ID, "intentional", wasIntentional, "exit_code", extractExitCode(exitErr))
	// Close this generation's section of the spec's retained output, for
	// EVERY kind of exit -- crash, drain, idle unload, force_stopped,
	// shutdown. cmd.Wait (waitForExit) has already waited for os/exec's two
	// copy goroutines, so every byte the process ever wrote is in the buffer
	// before this marker lands after it.
	proc.log.Exited(extractExitCode(exitErr))

	// A spec deleted from the config is dropped on this exit however the
	// process happened to die -- being removed is a property of the SPEC, not
	// of the exit's intent.
	//
	// M7 fix (fix round 1, and PRE-EXISTING rather than a defect of this
	// batch): this block used to sit inside the wasIntentional branch below,
	// so a removed spec whose child died on its own instead of being signalled
	// fell through to the crash classification and was never deleted -- it
	// kept appearing in Status() and telemetry, entered a crash-loop backoff,
	// and if pinned was RESTARTED by handleBackoffFire, for a spec the
	// operator had deleted. Reached without exotic timing: a removal while a
	// request is in flight drains via the drainGrace path, which does not set
	// intentionalStop (only terminateNow does, and it has not run yet), so a
	// child that crashes in that window is a non-intentional exit. The next
	// Apply cleaned it up (its removal loop deletes any absent spec once
	// st.proc is nil), which is why this only ever showed up as a spec that
	// lingered for one config-poll interval.
	//
	// No recordFailure and no state transition: the spec is about to cease to
	// exist, so there is nowhere for either to be read from. The row leaving
	// the table rings the transitions doorbell instead (deleteState).
	//
	// C2 fix: resolve any queued waiter before dropping the spec -- otherwise
	// it (and even its own admission_wait_timeout, since handleWaiterTimeout
	// is itself a no-op once the spec is gone) would never be resolved at all.
	//
	// Leaving the table can free a NAME (#340): a removed spec's upstream model
	// stays in the routing index for as long as its drain runs, so an instance
	// address equal to it -- another spec's instance -- stays unreachable, and
	// the protected floor skips that instance (instanceAddressHolder). The
	// index is therefore rebuilt here, and the floor re-run, so such an instance
	// comes up now instead of at the next Apply that changes the document; the
	// wake below starts it.
	if removed {
		o.failPending(st, ErrModelNotManaged)
		o.deleteState(st.spec.ID)
		o.rebuildUpstreamIndex()
		if !o.closing {
			o.ensureProtectedFloor()
		}
		o.wakeAdmissionCandidates()
		return
	}

	// A BURST instance (#340) leaves through the same branches as any other
	// process, with two differences that live elsewhere: it never enters a
	// crash-loop wait (enterBackoff), so a crash or failed start rests it at
	// crashed/start_failed for the operator; and once a clean stop leaves it
	// with nothing to do, admitAndStart's deferred retireIfSpent deletes it.

	if wasIntentional {
		if deferredBackoff && st.spec.AdminState != "force_stopped" {
			// The failure was already recorded (handleStartResult's
			// start-timeout path); this generation was merely still alive
			// then. Now that it is gone, start the crash-loop wait -- so the
			// delay bounds the interval between ATTEMPTS, which is what a
			// rate limit has to mean.
			//
			// ... except for a spec the OPERATOR stopped (fix round 1, M3).
			// This branch used to precede the Draining->Stopped normalization
			// unconditionally, so a force-stop landing on a spec whose start
			// had just timed out reported "backoff" --
			// docs/architecture/cross-cutting/agent-runtime-manager.md §6's
			// crash-loop WAIT -- for up to backoffCap (60s by default) on a
			// spec the operator had explicitly stopped, in the portal's live
			// status table. The wait was also a fiction: when the timer fired,
			// handleBackoffFire's admitAndStart refused to start a
			// force_stopped spec, so no retry was ever coming. The deferred
			// backoff is therefore DROPPED (never entered, no timer) and the
			// fall-through below rests the spec at Stopped. Deliberately not
			// hoisted into enterBackoff: a force_stopped spec whose child
			// CRASHES on its way out is still a crash (I4), and the guard
			// there would erase that from the status table.
			o.enterBackoff(st)
			o.wakeAdmissionCandidates()
			return
		}
		if st.state == StateDraining {
			o.setState(st, StateStopped)
		}
		if st.state == StateStopped {
			// FAIRNESS, and the reason a bare reorder of these two calls
			// would not have worked: the beneficiaries of this eviction are
			// admitted BY NAME, before the victim's own re-admission.
			// wakeAdmissionCandidates below would reach them too, but it
			// would also reach the victim, and only this explicit step
			// guarantees the ordering between the two.
			for _, id := range drainedFor {
				o.reask(id)
			}
			// For a burst instance with nobody waiting this is also where it
			// is retired (admitAndStart's deferred retireIfSpent).
			o.admitAndStart(st.spec.ID)
		}
		o.wakeAdmissionCandidates()
		return
	}

	exitCode := extractExitCode(exitErr)
	tail := proc.log.Tail(2048)

	// Task 4.4: a cold-start RAM bootstrap (unknown declared RAM) whose child
	// died of memory pressure gets the guarded, single evict-and-retry
	// escalation -- bounded so it can never become a fork-exec storm. Any other
	// bootstrap death, and every non-bootstrap death, falls through to the
	// unchanged crash / start-failure handling below. The classification is read
	// from the exit signature and the last measurement captured at the top.
	//
	// Gated on the spec WANTING to be up: evicting an idle process to free RAM is
	// worth something only when a retry is actually coming (a queued request, or a
	// pinned / force_running spec). A bootstrap that served its request and then
	// OOM'd with nothing waiting on it has no retry to help, so evicting an
	// innocent neighbour for it would be pure harm -- it falls through to the
	// ordinary crash path, which records the failure and does not restart it.
	if bootstrapSpec && o.wantsUp(st) && bootstrapDiedOfMemoryPressure(oomSignaled, lastMeasuredRAM, ramBudgetAtDeath) {
		o.handleBootstrapMemoryPressure(st, exitCode, tail)
		o.wakeAdmissionCandidates()
		return
	}

	// A cluster head or rpc_server member that died with a known llama.cpp RPC
	// message is classified. A terminal verdict on a START that never became
	// healthy cannot be fixed by retrying, so it rests as not_permitted without
	// backoff; every other classified exit keeps the ordinary handling below
	// and only gains its error code.
	rule, classified := matchExitRule(st.spec, tail)
	class := rule.class
	if classified && class.Terminal && !wasHealthy {
		o.setTerminalVerdict(st, exitClassMessage(rule, exitCode), class.Code)
		st.lastError.ExitCode, st.lastError.StderrTail = exitCode, tail
		o.wakeAdmissionCandidates()
		return
	}
	defer func() {
		if classified && st.lastError != nil {
			st.lastError.Code = class.Code
		}
	}()

	if wasHealthy {
		o.setState(st, StateCrashed)
		o.recordFailure(st, fmt.Sprintf("runtime: process exited unexpectedly (exit code %d)", exitCode), exitCode, tail)
		o.enterBackoff(st)
	} else {
		// I2 fix: route a failed START through backoff too, exactly like a
		// crash -- otherwise a non-pinned spec whose start always fails
		// re-execs once per incoming request with no rate limit, and a
		// PINNED spec whose start always fails never retries at all (stuck
		// in start_failed until an unrelated config edit).
		message := fmt.Sprintf("runtime: process exited before becoming healthy (exit code %d)", exitCode)
		// A wsl generation whose wsl.exe exited -1 never ran the workload at
		// all (no such distro, no systemd, a broken VM): say so, rather than
		// blame a process that never existed. Read off the generation, not
		// st.spec, which a push may have changed since this one started.
		if proc.wslUnit != "" && wslLaunchFailed(exitCode) {
			message = wslExitMessage(exitCode)
		}
		o.setState(st, StateStartFailed)
		o.recordFailure(st, message, exitCode, tail)
		o.failPending(st, ErrStartFailed)
		o.enterBackoff(st)
	}
	o.wakeAdmissionCandidates()
}

// handleBootstrapMemoryPressure is task 4.4's escalation, and its ENTIRE design
// goal is that it is BOUNDED -- it can never become a fork-exec / evict-and-retry
// storm (the "C3" anti-goal). It is reached only from onProcExited, only for a
// cold-start bootstrap (unknown declared RAM) whose death was classified as
// memory pressure, and only on a non-intentional exit.
//
// The bound is a per-spec counter (st.ramBootstrapRetries) capped at
// ramBootstrapMaxRetries == 1:
//
//   - FIRST memory-pressure death (counter below the cap): spend the one shot.
//     Evict a single idle process to free real host RAM -- more than the
//     guess-based admission arithmetic alone would free, since the OOM is
//     evidence the real footprint exceeds the guess -- and retry the bootstrap
//     ONCE. The retry itself is the ordinary crash-loop backoff (recordFailure +
//     StateStartFailed + enterBackoff), so it composes with the existing backoff
//     discipline and the queued request is kept for it. Exactly ONE idle process
//     is evicted, however many idle processes exist.
//
//   - SECOND consecutive memory-pressure death (counter at the cap): do NOT
//     escalate again. No eviction, no second shot. Fall back to the ordinary
//     bounded StateStartFailed crash-loop backoff and fail the queued request.
//     The counter is NOT reset here -- only a successful measurement resets it
//     (applyMeasurement) -- so every further memory-pressure death keeps taking
//     this branch, and no additional idle process is ever evicted for this stuck
//     bootstrap. That is what makes the escalation un-loopable.
//
// Because the counter survives restarts and is cleared only by a real
// measurement, a bootstrap that eventually comes up and is measured earns the
// one shot again for a genuinely new, later pressure event -- while one that
// keeps dying escalates at most once.
func (o *owner) handleBootstrapMemoryPressure(st *specState, exitCode int, tail string) {
	if st.ramBootstrapRetries >= ramBootstrapMaxRetries {
		// Shot already spent on this stuck bootstrap: the ordinary bounded path,
		// no eviction, no escalation.
		o.setState(st, StateStartFailed)
		o.recordFailure(st, fmt.Sprintf(
			"runtime: RAM cold-start bootstrap died of memory pressure again (exit code %d); escalation already spent, backing off",
			exitCode), exitCode, tail)
		o.failPending(st, ErrStartFailed)
		o.enterBackoff(st)
		return
	}

	st.ramBootstrapRetries++
	evicted := o.evictOneIdleForBootstrap(st.spec.ID)
	o.setState(st, StateStartFailed)
	msg := fmt.Sprintf(
		"runtime: RAM cold-start bootstrap died of memory pressure (exit code %d); evicting an idle process and retrying once",
		exitCode)
	if !evicted {
		msg = fmt.Sprintf(
			"runtime: RAM cold-start bootstrap died of memory pressure (exit code %d); no idle process to evict, retrying once",
			exitCode)
	}
	o.recordFailure(st, msg, exitCode, tail)
	// Deliberately NOT failPending here: the queued request is the reason to
	// retry, and the retry is exactly what this escalation grants it. The retry
	// rides the backoff below; if it, too, dies of memory pressure the second
	// death takes the branch above and fails the request then.
	o.enterBackoff(st)
}

// evictOneIdleForBootstrap drain-stops ONE idle process to free host RAM for a
// bootstrap retry, and returns whether it found one. It is the single eviction
// the memory-pressure escalation is allowed; the caller's counter is what keeps
// it to one per stuck bootstrap.
//
// It picks the oldest idle, unpinned, non-starting victim -- the same
// isEvictable contract Admit uses (never a busy, pinned or still-loading
// process, never the fork-exec-storm victim that clause exists to prevent, and
// never one used within the eviction grace window, so the escalation cannot take
// the model a pipeline is between requests on) and the same oldest-LastUsed,
// lowest-id tie-break Admit's sortOldestFirst uses, so the choice is
// deterministic over a map-ordered spec set. When every candidate is protected
// it finds no victim, which handleBootstrapMemoryPressure already reports as
// "no idle process to evict" and still retries once. It records the beneficiary
// (drainedFor) so the victim's exit offers the freed slot to the bootstrap
// first, exactly like an admission eviction.
//
// It is the owner's own victim choice, so it keeps Admit's instance rules too
// (#340): never a sibling of the beneficiary (evicting a warm copy of the very
// model being started frees nothing useful),
// never an instance while a higher-index sibling of it still runs (the ordering
// invariant, higherSiblingRuns), never a protected instance (pinnedForAdmission
// is what isEvictable sees as Pinned), and SURPLUS first -- a running burst
// instance is the cheapest thing on the host to give up -- ahead of the oldest
// LastUsed. A spec that never replicates has no siblings and is never surplus,
// so for it the choice is exactly the pre-instance one.
func (o *owner) evictOneIdleForBootstrap(beneficiaryID string) bool {
	beneficiaryBase := beneficiaryID
	if b := o.specs[beneficiaryID]; b != nil {
		beneficiaryBase = specBase(b.spec)
	}
	var victim *specState
	now := time.Now()
	for id, st := range o.specs {
		if id == beneficiaryID || st.proc == nil || st.state != StateRunning {
			continue
		}
		if specBase(st.spec) == beneficiaryBase || o.higherSiblingRuns(st) {
			continue
		}
		if !isEvictable(RunningProc{
			Pinned:   st.pinnedForAdmission(),
			Starting: st.state == StateStarting,
			InFlight: st.inFlight,
			LastUsed: st.lastUsed,
		}, now, o.evictionGrace) {
			continue
		}
		if victim == nil || betterBootstrapVictim(st, victim) {
			victim = st
		}
	}
	if victim == nil {
		return false
	}
	victim.drainedFor = appendUnique(victim.drainedFor, beneficiaryID)
	o.beginDrain(victim.spec.ID)
	return true
}

// betterBootstrapVictim is evictOneIdleForBootstrap's total order: a surplus
// (running burst) instance before anything else, then the oldest LastUsed, then
// the lowest instance key -- never left to map iteration.
func betterBootstrapVictim(a, b *specState) bool {
	if a.burst() != b.burst() {
		return a.burst()
	}
	if !a.lastUsed.Equal(b.lastUsed) {
		return a.lastUsed.Before(b.lastUsed)
	}
	return a.spec.ID < b.spec.ID
}

// ramPoolAtDeath is the RAM pool proc lived in, for the bootstrap
// memory-pressure classifier's "last measured RAM at 90 % of the pool"
// criterion: the WSL VM pool for a wsl generation, whose OOM killer is the
// VM's and strikes at the VM's ceiling while the host may have room to spare,
// and the host pool otherwise. Read off the generation, like the exit message.
// Both are what admission weighed the generation against, and both are 0
// (unconstrained) before their first sample, which makes the criterion a
// no-op there.
func (o *owner) ramPoolAtDeath(proc *runningProc) int {
	if proc.wslUnit != "" {
		return o.wslRAMBudget()
	}
	return o.hostRAMBudgetSnapshot()
}

// hostRAMBudgetSnapshot computes the current host-RAM pool exactly as
// buildSnapshot does -- the live MemAvailable plus the RAM the managed processes
// are MEASURED to hold, minus the operator reserve (hostRAMBudgetMB) -- so the
// memory-pressure classifier compares a dying bootstrap's last RSS against the
// same pool admission weighs demand against. It must be read while the dying
// process is still in the live set (st.proc non-nil), so onProcExited calls it
// before clearing that. Returns 0 (unconstrained) before the first hardware
// sample, which makes the RSS-near-budget signature a no-op there.
func (o *owner) hostRAMBudgetSnapshot() int {
	residentRAMMB := 0
	for _, st := range o.specs {
		if st.proc == nil {
			continue
		}
		residentRAMMB += realRAM(st.measuredRAM)
	}
	return hostRAMBudgetMB(o.hostInfo(), residentRAMMB, o.hostRAMReserveMB)
}

// enterBackoff puts st into the crash-loop wait and schedules the retry.
//
// Two preconditions it enforces rather than assumes, both B6 fixes:
//
// closing: nothing will ever be retried once shutdown has begun, and the
// timer scheduled here is registered in m.wg -- which Close() ends by
// waiting on. handleClose cancels the backoff timers that exist WHEN it
// runs, but a backoff entered afterwards made Close block for the whole
// delay (backoffCap, 60s by default) with nothing left to retry. Reachable
// without any exotic timing: a Running spec with a request still in flight
// drains via the drainGrace path, which does not set intentionalStop (only
// terminateNow does, and it has not run yet), so a child that crashes in
// that window lands as a NON-intentional exit -> StateCrashed -> here.
//
// The general rule both halves serve (see also applyConfig's removal loop,
// fix round 2's G2): a backoff timer must never outlive its spec. It is
// tracked in m.wg, and handleClose can only reach the timers of specs still
// in o.specs -- so whether the spec stops existing because the agent is
// shutting down or because the operator deleted it, the timer has to be
// refused or cancelled at that moment, not left to fire into nothing.
//
// proc != nil: StateBackoff means "nothing of this spec is running, waiting
// to retry". handleStartResult's start-timeout path used to enter backoff
// straight after terminateNow, which only SIGTERMs and schedules the SIGKILL
// escalation -- the process is still alive and still holding its VRAM. That
// produced a status contradicting itself (backoff, or Stopped once the delay
// elapsed, next to a live PID -- now visible in the portal's live status
// table) AND silently dropped the retry: handleBackoffFire clears the timer,
// sets StateStopped and calls admitAndStart, which returns immediately on
// st.proc != nil. Whenever the delay was shorter than the child's
// time-to-die (the first two failures against any child that does not die
// instantly on SIGTERM) the escalating backoff was defeated -- the restart
// happened whenever the process finally exited instead. Deferring to
// onProcExited keeps the spec in the honest StateStartFailed until the
// process is genuinely gone, then starts the delay from there.
//
// NOT a precondition here: st.spec.AdminState == "force_stopped". A
// force-stopped spec must not sit in a crash-loop wait either (fix round 1,
// M3), but that belongs to the ONE caller whose exit is a deliberate stop --
// onProcExited's deferred branch -- and not to this function: a force_stopped
// spec whose child CRASHES on its way out is still a crash (I4), and
// short-circuiting it here would report "stopped" for it and erase that from
// the status table.
//
// A BURST instance (#340) never enters it: burst capacity is opportunistic and
// is not retried on its own -- the gateway asks for it again when it wants it.
// Its caller has already set the failure state, which it keeps for the operator
// until it is named again (an ensure then retires the row and starts a fresh
// instance, instanceForRequest; a plain request only retires it, handleEnsure)
// or the next Apply; any waiter still queued on it would never be served, so it
// is failed here.
//
// A CLUSTER HEAD never enters it either, for the same reason: only the
// gateway's cluster start brings a head up (cluster_head.go), after bringing
// its members up, so a timer here could never start it. It rests at the
// failure until that start, which is the retry.
func (o *owner) enterBackoff(st *specState) {
	if o.closing {
		o.setState(st, StateStopped)
		return
	}
	if st.burst() || st.spec.IsClusterHead() {
		o.failPending(st, ErrStartFailed)
		return
	}
	if st.proc != nil {
		st.backoffOnExit = true
		return
	}
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
	// The process group for a native process; the scope, then wsl.exe as the
	// backstop, for a wsl one -- off the owner (wslControl).
	if err := c.proc.control().kill(); err != nil {
		slog.Debug("runtime: SIGKILL failed", "spec", c.specID, "error", err)
	}
	// State transitions to Stopped/Crashed once waitForExit actually reports
	// the exit -- not here, since the process may take a moment to die even
	// after SIGKILL is delivered.
}

// terminateNow sends SIGTERM to proc (its process group, or a wsl
// generation's scope -- runningProc.control) and schedules a
// SIGKILL escalation after killGrace if it has not exited by then. Marks
// st.intentionalStop so the eventual exit report is classified as a clean
// stop, not a crash.
func (o *owner) terminateNow(st *specState, proc *runningProc) {
	// M2 fix, part 1: if this generation has already exited, do nothing --
	// proc.cmd.Process.Pid could since have been recycled by the OS to an
	// unrelated process, and signaling it would be a hazard, not a no-op.
	select {
	case <-proc.exited:
		return
	default:
	}
	// M2 fix, part 2: if termination is already in progress (killTimer
	// already scheduled), don't re-signal or schedule a second escalation
	// timer -- this can otherwise be reached twice for the same
	// generation (e.g. the drain-grace timer expiring at the same moment
	// a release() brings InFlight to zero), producing a redundant
	// SIGTERM/SIGKILL and an orphaned killTimer whose wg accounting would
	// only be reclaimed when it eventually fires on its own.
	if proc.killTimer != nil {
		return
	}
	st.intentionalStop = true
	cancelTimer(o.m, &proc.drainTimer)
	// SIGTERM to the process group for a native process, to the systemd scope
	// for a wsl one (wslControl, off the owner); either way the killTimer below
	// escalates if the generation has not exited by then.
	if err := proc.control().terminate(); err != nil {
		slog.Debug("runtime: SIGTERM failed", "spec", st.spec.ID, "error", err)
	}
	specID := st.spec.ID
	proc.killTimer = o.m.scheduleAfter(killGrace, func() {
		o.m.postCmd(cmdKillGraceExpired{specID: specID, proc: proc})
	})
}

// wakeAdmissionCandidates re-attempts admission for every spec that is idle
// (no live process), not in an active crash-backoff wait, and WANTS to be up
// -- called after any event that could plausibly have freed a resource
// another spec's admission decision depends on (a release, a process actually
// exiting, a config Apply). A start that finishes changes an admission input
// too, without freeing anything, and has its own narrower wake:
// wakeAfterStart.
//
// "Wants to be up" is admitAndStart's own wantUp rule, not "has a queued
// request": a PINNED or force_running spec wants to be up with no waiter at
// all. Its predecessor (wakeAllPendingWaiters) required len(pending) > 0, so
// a pinned spec whose admission returned Wait -- blocked by a busy process,
// or, since the C3 fix, by one that is still loading -- was never retried by
// anything. It stayed Stopped until an unrelated config Apply happened to
// call admitAndStart for it again. That was a latent hole before C3 and a
// guaranteed one after it (the Starting clause makes Wait strictly more
// likely), so the wake loop has to cover every spec that wants to be up, not
// only the ones with someone waiting. The "still loading" case is retried
// when that start finishes, by wakeAfterStart, for a pinned spec and for one
// with a queued request; a force_running spec without a waiter waits for the
// next release, exit or Apply, because waking it there lets two such specs
// evict each other at load speed (see wakeAfterStart).
//
// Order is oldest-queued-request-first, not Go's randomized map order: when
// one freed slot cannot satisfy everyone, the request that has been waiting
// longest should get it. Specs with no queued request (pinned/force_running)
// sort last -- nobody is waiting on them, and they will be retried by the
// next event either way.
func (o *owner) wakeAdmissionCandidates() {
	o.wakeAdmission(wantsWake)
}

// wakeAfterStart is the admission wake for a start that has just finished.
//
// Leaving StateStarting changes an admission input without freeing anything:
// the process stays, but from this moment it is evictable when idle and
// unpinned (isEvictable's Starting clause no longer holds), and no exit,
// release or Apply follows to say so. A start that served waiters is covered
// later by their releases, because succeedPending has made the process busy.
// An unpinned start without one -- a force_running spec's, or an on-demand
// one whose every caller left while it loaded -- is covered by nothing else.
// Until 0.8.1 no wake ran here: a request queued for another spec behind
// such a start waited for an unrelated release, exit or Apply, and failed at
// its admission_wait_timeout_seconds if none came first (when that is 0, it
// waited until one came or its caller gave up), and a pinned spec behind it
// stayed stopped until one came.
//
// It retries only the candidates whose start the next such wake cannot undo
// while a caller still needs it: a spec with a queued request comes up busy
// unless every caller left while it loaded (succeedPending serves the
// requests still queued), and a pinned spec is never evicted. So every start
// this wake causes is for a queued request or a pinned spec, and it cannot
// loop without traffic. A force_running spec without a waiter is left to the
// next release, exit or Apply. Woken here, two force_running specs that
// cannot run together would evict each other with no event in between, one
// full model load per turn and no end: the storm isEvictable's Starting
// clause ends, at load speed.
func (o *owner) wakeAfterStart() {
	o.wakeAdmission(wantsWakeAfterStart)
}

// wantsWakeAfterStart is wakeAfterStart's selection rule: wantsWake, narrowed
// to a spec with a queued request or a pinned one. See wakeAfterStart.
// "Pinned" is pinnedForAdmission: a protected instance of a replicable spec is
// never a victim either, so it cannot take part in the eviction storm the
// narrowing guards against.
func wantsWakeAfterStart(st *specState) bool {
	return wantsWake(st) && (len(st.pending) > 0 || st.pinnedForAdmission())
}

// wantsDemandWake is the selection rule for the wake a landed ready measurement
// causes (applyMeasurement): a baseline instance waiting for a measured sibling
// (specState.waitingOnDemand), narrowed like the other event wakes. It is a
// cheap no-op whenever no such instance waits, which is nearly always.
func wantsDemandWake(st *specState) bool {
	return st.waitingOnDemand && wantsWakeAfterStart(st)
}

// wakeOnHostSample is the admission wake for a fresh hardware sample
// (Manager.SetHostInfo posts it, the owner's run loop calls it). Of every
// admission input, the host-RAM pool is the one that moves with the sample
// alone: it is built from the live MemAvailable, so a start refused for lack of
// RAM can fit on the very next sample with no release, exit or Apply in between
// -- most importantly the beneficiary of a RAM eviction, which is re-admitted on
// its victim's exit against the previous sample, before the freed memory shows.
// Until this wake existed it then waited for an unrelated event or its whole
// admission timeout.
//
// It re-asks only the specs wantsHostSampleWake selects, so it is a cheap no-op
// (one pass over the spec table, nothing evaluated) whenever nothing is waiting
// on RAM, which is nearly always. That is also what keeps it from being a storm:
// a sample lands about once a second for the life of the agent, and evaluating
// admission is not free -- buildSnapshot calls the installed VRAM measurer, a
// subprocess on a real host -- so a wake on every sample for every queued start
// would run it once a second for as long as anything queued behind a busy
// neighbour. Even a start that IS waiting on RAM costs one evaluation per
// sample, and no more.
func (o *owner) wakeOnHostSample() {
	o.wakeAdmission(wantsHostSampleWake)
}

// wantsHostSampleWake is wakeOnHostSample's selection rule: a spec whose last
// verdict was a Wait the RAM pool contributed to, narrowed to the same set
// wakeAfterStart may retry (a queued request, or pinned). That narrowing is the
// part that matters for safety: a force_running spec with no waiter is
// deliberately left to the next release, exit or Apply, because retrying it on
// anything but a real change lets two that cannot run together evict each other
// at load speed (see wakeAfterStart). A sample is such an event no more than a
// finished start is.
func wantsHostSampleWake(st *specState) bool {
	return st.waitingOnRAM && wantsWakeAfterStart(st)
}

// graceWakeSlack is how far PAST a within-grace victim's computed expiry the grace
// timer fires. The grace bound is strict -- a model is evictable only when
// now-LastUsed is GREATER than the window (isEvictable/withinGrace) -- so a wake
// at exactly the expiry would re-ask the admission one instant too early, find the
// victim still protected, and merely reschedule. A small slack puts now past the
// window on the first try; a timer firing a touch late only converges sooner, and
// a re-ask that is still too early simply reschedules, so correctness never
// depends on this value's precision.
const graceWakeSlack = 5 * time.Millisecond

// handleGraceExpiry is the owner's single grace timer's callback (cmdGraceExpiry):
// the earliest within-grace victim's window has expired, so re-ask every start
// blocked solely by the grace window. It is the grace analogue of wakeOnHostSample.
//
// It clears the handle with cancelTimer rather than a bare o.graceTimer = nil,
// and that is deliberate: the owner is single-threaded, but this command races
// other commands for the owner's attention, so an admitAndStart (via its deferred
// rescheduleGraceWake) could have REPLACED the fired timer with a fresh one
// between this timer firing and this handler running. cancelTimer clears the fired
// timer (Stop reports it already fired, so no wg.Done -- the callback owns that)
// AND cleanly cancels any such replacement (Stop reports it live, releasing its
// wg.Add), so the handle is never left pointing at an orphaned timer that
// handleClose could not reach -- the G2 invariant. rescheduleGraceWake then arms a
// fresh timer for the next earliest expiry if any grace-blocked start remains --
// reached here too via each re-asked admitAndStart's own deferred
// rescheduleGraceWake, and once more explicitly so the case where the wake frees
// everyone (or finds nobody) still reconciles the timer to "none".
func (o *owner) handleGraceExpiry() {
	cancelTimer(o.m, &o.graceTimer)
	o.graceTimerAt = time.Time{}
	if o.closing {
		return
	}
	o.wakeOnGraceExpiry()
	o.rescheduleGraceWake()
}

// wakeOnGraceExpiry re-asks exactly the starts a grace expiry can unblock -- those
// blocked solely by a within-grace victim (wantsGraceWake) -- oldest queued
// request first, exactly as wakeOnHostSample does for RAM waiters. A spec waiting
// on anything a grace expiry cannot change (the matrix, a busy neighbour, the
// process limit, a VRAM/RAM budget) is deliberately left alone.
func (o *owner) wakeOnGraceExpiry() {
	o.wakeAdmission(wantsGraceWake)
}

// wantsGraceWake is wakeOnGraceExpiry's selection rule: a spec whose last verdict
// was a Wait blocked solely by the grace window, narrowed to the same set
// wakeAfterStart may retry (a queued request, or pinned). The narrowing is the
// storm guard, exactly as for wantsHostSampleWake: a force_running spec with no
// waiter is left to a real event, never re-evaluated on a bare timer, so two that
// cannot run together cannot evict each other at load speed off this wake.
func wantsGraceWake(st *specState) bool {
	return st.waitingOnGrace && wantsWakeAfterStart(st)
}

// earliestGraceExpiry returns the soonest instant at which some currently
// grace-protected process becomes evictable -- the earliest (LastUsed + grace)
// among the running processes that are idle, unpinned, past Starting and used
// within the window -- and whether there is one. That set is exactly isEvictable's
// grace clause read off owner state (a never-used process has a zero LastUsed and
// is already evictable, not protected; a window that has already elapsed is past,
// not pending), so the timer and the policy can never disagree about who is
// protected. A draining process is included like any other idle one: counting it
// only risks a harmless early wake, never a missed one.
func (o *owner) earliestGraceExpiry(now time.Time) (time.Time, bool) {
	if o.evictionGrace <= 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	found := false
	for _, st := range o.specs {
		if st.proc == nil || st.pinnedForAdmission() || st.state == StateStarting || st.inFlight != 0 {
			continue
		}
		if st.lastUsed.IsZero() {
			continue // never used: already evictable, not grace-protected
		}
		expiry := st.lastUsed.Add(o.evictionGrace)
		if !expiry.After(now) {
			continue // the window has already elapsed: already evictable
		}
		if !found || expiry.Before(earliest) {
			earliest, found = expiry, true
		}
	}
	return earliest, found
}

// rescheduleGraceWake reconciles the owner's single grace timer with the current
// state. The invariant it keeps is simple and load-bearing: WHENEVER some start is
// blocked solely by the grace window (wantsGraceWake), a wake is pending -- or that
// start hangs, which is the very regression this machinery exists to close. With a
// waiter it arms the timer at the earliest within-grace victim's expiry (plus
// graceWakeSlack); with none it cancels any stray timer. Called via admitAndStart's
// defer on every admission path and from handleGraceExpiry, so the timer tracks the
// live set without any single call site having to get it right.
//
// The one subtlety is the fallback when a waiter exists but earliestGraceExpiry
// finds no pending window. A within-grace victim is what makes WaitOnGrace true in
// the first place, so this should not happen -- EXCEPT across the sub-instant skew
// between the admission's own snapshot clock and this call's clock: a victim whose
// window elapsed in that gap leaves a waiter whose last verdict was WaitOnGrace but
// whose victim is already evictable. Cancelling here would strand it, so instead we
// re-ask promptly (a graceWakeSlack catch-up wake); that re-admission resolves the
// waiter (it evicts the now-evictable victim) and clears the flag. It cannot spin:
// a fresh snapshot with no within-grace victim yields WaitOnGrace false.
//
// It is idempotent and cheap: with nothing grace-blocked (the common case) it
// cancels any stray timer and returns after one scan; while a timer is already
// armed for the same expiry it leaves it be, so a wake batch that re-asks several
// grace waiters arms the timer once, not once per spec.
func (o *owner) rescheduleGraceWake() {
	// With the grace window off there is nothing to wake for: no admission can be
	// WaitOnGrace, so there is no waiter and no expiry. Guard it up front so the
	// fallback below can never arm on a stale flag, matching earliestGraceExpiry.
	if o.evictionGrace <= 0 {
		cancelTimer(o.m, &o.graceTimer)
		o.graceTimerAt = time.Time{}
		return
	}
	hasWaiter := false
	for _, st := range o.specs {
		if wantsGraceWake(st) {
			hasWaiter = true
			break
		}
	}
	if !hasWaiter {
		cancelTimer(o.m, &o.graceTimer)
		o.graceTimerAt = time.Time{}
		return
	}
	expiry, ok := o.earliestGraceExpiry(time.Now())
	if ok && o.graceTimer != nil && o.graceTimerAt.Equal(expiry) {
		return // already armed for exactly this instant
	}
	cancelTimer(o.m, &o.graceTimer)
	var delay time.Duration
	if ok {
		o.graceTimerAt = expiry
		delay = time.Until(expiry) + graceWakeSlack
		if delay < 0 {
			delay = 0
		}
	} else {
		// The skew fallback above: a waiter with no pending window. Re-ask promptly
		// rather than strand it; graceTimerAt stays zero, since this wake is not tied
		// to a known expiry.
		o.graceTimerAt = time.Time{}
		delay = graceWakeSlack
	}
	o.graceTimer = o.m.scheduleAfter(delay, func() {
		o.m.postCmd(cmdGraceExpiry{})
	})
}

// wakeAdmission retries admission for every spec that want selects, oldest
// queued request first (see wakeAdmissionCandidates).
func (o *owner) wakeAdmission(want func(*specState) bool) {
	candidates := make([]wakeCandidate, 0, len(o.specs))
	for id, st := range o.specs {
		if !want(st) {
			continue
		}
		c := wakeCandidate{id: id, base: specBase(st.spec), instance: st.instance}
		for _, w := range st.pending {
			if c.queuedAt.IsZero() || w.queuedAt.Before(c.queuedAt) {
				c.queuedAt = w.queuedAt
			}
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return lessByQueueAge(candidates[i], candidates[j])
	})
	for _, c := range candidates {
		o.reask(c.id)
	}
}

// reask is admitAndStart for a loop that asks a list of candidates it collected
// before asking any of them -- the wakes (wakeAdmission), the Apply start loop,
// an exit's beneficiaries -- and skips a candidate whose full decision from
// this very turn still stands (decidedThisTurn). Such a candidate was decided
// by an earlier ask in the same turn, above all by a cascade: a sibling that
// started or was refused re-asks the instances held back behind it
// (wakeSiblingsWaitingFor), which a list collected before then still names.
// Asking it again would repeat the decision -- nothing it reads has moved in
// between (owner.turn) -- and every decision builds a snapshot, which calls
// the VRAM measurer: eight protected instances behind a refused instance 0
// would otherwise be decided almost twice over in one wake. An instance only
// held back since (waitingOnSibling) has no standing decision and is asked.
func (o *owner) reask(id string) {
	if st := o.specs[id]; st != nil && o.decidedThisTurn(st) {
		return
	}
	o.admitAndStart(id)
}

// decidedThisTurn reports whether st's latest full admission decision was taken
// in the current owner turn (owner.turn; never on an owner driven by hand,
// whose turn is 0).
func (o *owner) decidedThisTurn(st *specState) bool {
	return o.turn != 0 && st.decidedTurn == o.turn
}

// wakeCandidate is one spec wakeAdmission has selected for a retry,
// carried alongside the queue time of its OLDEST waiter -- the zero
// time when it has none, which is the pinned/force_running case. id is its
// instance key; base and instance are the spec it is an instance of and its
// index (#340), which order equal queue times (instanceLess).
type wakeCandidate struct {
	id       string
	base     string
	instance int
	queuedAt time.Time
}

// wantsWake is wakeAdmissionCandidates' selection rule, and it is exactly
// admitAndStart's own two early returns read as one predicate: idle (no live
// process, not in a crash-backoff wait) AND wantUp (a queued request, or
// protected: pinned, force_running, or inside min_instances).
//
// It is deliberately only a PRE-FILTER: admitAndStart re-checks both
// conditions itself and returns early, so a spec this predicate lets through
// is not thereby started. What the predicate decides is which specs take part
// in the ordering below -- so keeping it in step with admitAndStart's rule is
// what makes the wake fair, not what makes it correct.
func wantsWake(st *specState) bool {
	if st.proc != nil || st.state == StateBackoff || terminalHeld(st) {
		return false
	}
	return st.wantsStart()
}

// lessByQueueAge is wakeAdmission's wake order: oldest queued
// request first, specs with no waiter at all last, and the spec ID as a final
// tiebreak so the order is total. That last clause is load-bearing rather
// than tidiness -- leaving equal keys to fall back on Go's randomized map
// iteration is what the ordering exists to eliminate.
//
// For instances (#340) the tiebreak is instanceLess -- the base spec ID, then
// the instance index NUMERICALLY -- not the instance key as a string, which
// would put S#10 before S#2. Equal queue times are the protected instances
// with no waiter above all, and for them the order asks instance 0 ahead of
// every further instance of its spec, so a further one finds its lower siblings
// started rather than being held back for them (admitAndStart's start-order
// rule keeps the order whichever is asked first). The key itself breaks what
// is left, which for a spec that never replicates is the whole tiebreak: its
// base is its ID and its index 0.
func lessByQueueAge(a, b wakeCandidate) bool {
	if a.queuedAt.IsZero() != b.queuedAt.IsZero() {
		return b.queuedAt.IsZero() // a real queue time sorts before "no waiter"
	}
	if !a.queuedAt.Equal(b.queuedAt) {
		return a.queuedAt.Before(b.queuedAt)
	}
	if a.base != b.base || a.instance != b.instance {
		return instanceLess(a.base, a.instance, b.base, b.instance)
	}
	return a.id < b.id // total order: never leave a tie to map iteration
}

// appendUnique appends v to list unless it is already present. The lists it
// serves (specState.drainedFor) are bounded by the number of specs and are
// almost always length 0 or 1, so a linear scan is the right shape.
func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// scanIdle drain-stops every Running, unprotected instance whose idle timeout
// has elapsed with zero in-flight requests. Unprotected is the pre-instance
// "unpinned and not force_running" for a spec that never replicates, and keeps
// the min_instances floor for a replicable one.
//
// The ordering invariant (higherSiblingRuns) holds here too: an instance is not
// unloaded while a higher-index sibling still has a process, so a spec's idle
// instances leave one per scan, highest index first, and instance 0 last. A
// sibling this very scan has begun draining still has its process, so map
// order cannot let two of one spec's instances go in the same pass.
func (o *owner) scanIdle() {
	now := time.Now()
	for id, st := range o.specs {
		if st.proc == nil || st.state != StateRunning {
			continue
		}
		if st.protected() || st.claimed() {
			continue
		}
		isMember := st.spec.Kind == SpecKindRPCServer
		if (st.spec.IdleTimeoutSeconds <= 0 && !isMember) || st.inFlight != 0 {
			continue
		}
		if o.higherSiblingRuns(st) {
			continue
		}
		idle := time.Duration(st.spec.IdleTimeoutSeconds) * time.Second
		since := st.lastUsed
		if isMember {
			idle = memberIdleTimeout(st.spec)
			since = st.claim.idleSince(st.lastUsed)
		}
		if now.Sub(since) >= idle {
			o.beginDrain(id)
		}
	}
}

func (o *owner) snapshotStatus() []Status {
	out := make([]Status, 0, len(o.specs))
	for _, st := range o.specs {
		s := Status{
			// One row per instance (#340): the BASE spec ID, so an older
			// gateway sees exactly the rows it always did for a spec that never
			// replicates and a newer one folds instance rows by spec; the
			// instance address as the model; and the instance index.
			SpecID:      specBase(st.spec),
			Instance:    st.instance,
			Model:       InstanceAddress(st.spec.UpstreamModel, st.instance),
			StartupMS:   st.startupMS,
			RPCProtocol: st.rpcProtocol,
			State:       st.state,
			Since:       st.since,
			InFlight:    st.inFlight,
			Restarts:    st.restarts,
			LastError:   st.lastError,
			// Probe config (Task 9): pure struct copies of the spec's own
			// resolved fields, no I/O -- this runs on the owner's single
			// command goroutine, which must never block on a network probe.
			Type:             st.spec.Type,
			MetricsPath:      st.spec.MetricsPath,
			ContextProbePath: st.spec.ContextProbePath,
			Kind:             st.spec.Kind,
		}
		if st.measuredVRAM != nil {
			// M3 fix: hand out a COPY, not the same map the measurer
			// returned -- that map is shared with every Status() caller
			// and, once a real measurer exists (Task 18), may be reused
			// by its next call.
			cp := make(map[int]int, len(st.measuredVRAM))
			for k, v := range st.measuredVRAM {
				cp[k] = v
			}
			s.MeasuredVRAM = cp
		}
		// A scalar: copied by value, so nothing aliases owner state.
		s.MeasuredRAMMB = st.measuredRAM
		if st.proc != nil {
			s.PID = st.proc.pid
			s.Port = st.proc.port
		}
		if st.spec.IsRPCServer() {
			member := *st.spec.RPC
			member.HeadIPs = slices.Clone(member.HeadIPs)
			s.RPCMember = &member
			s.RPCReleasedAt = st.claim.releasedAt
			s.RPC = &RPCStatus{
				HeldBy:        st.claim.heldBy,
				HeldSince:     st.claim.heldSince,
				ReservedUntil: st.claim.reservedUntil,
				BindMismatch:  st.bindMismatch,
				// A start that verified the banner proved the device count equal
				// to the expected one, so the expected count is what the member
				// exposes; a member that is not running exposes none.
				DeviceCount: st.memberDeviceCount(),
			}
		}
		// A head's launch facts: its endpoints cannot change under a live
		// process (a different set relaunches it), so while the process exists
		// the spec's endpoints are the ones it was started against.
		if st.spec.IsClusterHead() && st.proc != nil {
			launched := make([]string, len(st.spec.Cluster.Endpoints))
			for i, ep := range st.spec.Cluster.Endpoints {
				launched[i] = ep.Address
			}
			s.Cluster = &ClusterStatus{Launched: launched}
		}
		s.AdmissionBlockedBy = st.admissionBlockedBy
		out = append(out, s)
	}
	return out
}

// memberDeviceCount is the number of devices a running rpc_server member
// exposes, 0 while it is not running.
func (st *specState) memberDeviceCount() int {
	if st.state != StateRunning || st.proc == nil {
		return 0
	}
	return expectedMemberDevices(st.spec)
}

func (o *owner) loadedModels() []string {
	var out []string
	for _, st := range o.specs {
		if st.state == StateRunning && st.spec.Kind != SpecKindRPCServer {
			out = append(out, InstanceAddress(st.spec.UpstreamModel, st.instance))
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
	// The grace timer is registered in m.wg, which Close() ends by waiting on, so
	// it must never outlive the owner -- the same invariant the backoff timers keep
	// below (G2). Nothing will be admitted again, so there is nothing left for it to
	// wake.
	cancelTimer(o.m, &o.graceTimer)
	o.graceTimerAt = time.Time{}
	for id, st := range o.specs {
		if st.state == StateBackoff {
			cancelTimer(o.m, &st.backoffTimer)
			o.setState(st, StateStopped)
		}
		// Registered in m.wg like the backoff timer; nothing is idle-scanned or
		// admitted again, so the reservation has nothing left to guard.
		cancelTimer(o.m, &st.claim.reservationTimer)
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
