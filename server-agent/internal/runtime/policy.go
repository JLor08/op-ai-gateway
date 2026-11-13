// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RunningProc is one currently-running managed process, as the admission
// policy needs to see it. GPUs holds the EFFECTIVE VRAM the process is
// occupying per GPU index -- the measured value when the agent has one,
// else the spec's estimate. Admit trusts this number exactly as given; it
// never re-derives or looks up an estimate elsewhere.
type RunningProc struct {
	// SpecID is the process's unique key: the spec ID for instance 0 (and for
	// every spec that never replicates), the instance key S#k for instance k
	// >= 1 (#340). The self-filter, the victim set and Decision.Evict all speak
	// in it, so two instances of one spec are two distinct occupants.
	SpecID string
	// Spec is the BASE spec ID the process is an instance of, and Instance its
	// index k. They are what makes instances of one spec SIBLINGS to admission
	// (baseOf): siblings always co-reside, a sibling is never a victim for a
	// candidate of its own spec, a pair is looked up by base so an instance
	// inherits its spec's matrix verdicts, and an instance is not evictable
	// while a higher-index sibling stays running (the "highest index leaves
	// first" ordering invariant). Spec == "" reads as SpecID, so a process built
	// without it -- any snapshot that predates instances, every table test --
	// is its own base at instance 0, exactly as before instances existed.
	Spec     string
	Instance int
	// Surplus marks a running BURST instance (IsBurst): capacity above the
	// spec's protected floor, started on demand. Evicting one costs the least
	// of any victim, since its spec keeps serving from a lower sibling, so
	// orderVictims draws surplus instances before every other idle victim. It
	// changes only WHICH victim goes first, never WHETHER one may go.
	Surplus bool
	// Member marks an rpc_server cluster member. The kind is exempt from the
	// co-residency matrix (pairAllowed): a member runs next to anything, and
	// anything next to it, while VRAM, RAM and the process limit still apply.
	Member bool
	GPUs   map[int]int // gpu index -> effective VRAM MB
	// RAMMB is the EFFECTIVE host RAM the process occupies, in MB: the live
	// measurement when the agent has one, floored at the spec's pushed demand.
	// It is host-scoped -- one number per process, not per GPU -- and Admit
	// sums it against PolicySnapshot.HostRAMBudgetMB exactly as given. 0 means
	// UNKNOWN (neither a demand nor a measurement), never "uses none": such a
	// process contributes nothing to the sum and is not drawn as a victim for
	// it, since evicting it would free nothing the arithmetic can count.
	RAMMB    int
	InFlight int
	Pinned   bool
	// Role is the process's model role, normalized by roleOf (never empty): the
	// spec's pushed role, or RoleOther when it has none. Admit compares it with
	// the candidate's own role (roleOf(spec.Role)) to order the eviction victims:
	// an idle model of the candidate's role is drawn before one of another role
	// (orderVictims, task 3.3). It changes only WHICH idle victim is chosen first,
	// never WHETHER one is evictable -- the grace gate (isEvictable) is unchanged.
	Role string
	// LastUsed is when the process last served, started serving or finished a
	// request. It does two jobs: the idle-victim ordering (oldest evicted first)
	// and, against PolicySnapshot.Now and EvictionGrace, the grace window that
	// keeps a recently used process out of the victim set altogether
	// (isEvictable). Zero means never used.
	LastUsed time.Time
	// Starting is true while this process is still loading -- it has been
	// exec'd but has never passed a health probe, so it is serving nothing
	// and its InFlight is necessarily 0. Such a process is NEVER an eviction
	// victim (isEvictable), which is the C3 fix. See that function.
	Starting bool
	// WSL marks a process that runs inside the WSL VM (execution mode wsl,
	// #381). Its RAMMB is held by the VM as well as by the host, so it is the
	// one kind of process charged against PolicySnapshot.WSLRAMBudgetMB and
	// drawn as a victim for it (ruleWSLRAM); every other rule reads it like
	// any process.
	WSL bool
}

// PolicySnapshot is the pure input to Admit: the currently running set plus
// the configured constraints, all as of one instant. No clock, no store, no
// live process handles -- a caller assembles a fresh snapshot for every
// admission decision.
type PolicySnapshot struct {
	Running      []RunningProc
	MaxProcesses int // 0 = unlimited
	// Now is the instant this snapshot was assembled at, the clock isEvictable
	// reads the grace window against. Admit is pure -- it never reads the wall
	// clock itself -- so the one caller that owns a clock (owner.buildSnapshot)
	// stamps it here, exactly as it hands over every other time-dependent input.
	Now time.Time
	// EvictionGrace is the hard protection window after a process was last used:
	// a process whose LastUsed is within it of Now is never an eviction victim,
	// whatever the pressure (OP_AGENT_EVICTION_GRACE_SECONDS,
	// owner.evictionGrace, copied through verbatim). 0 is the sentinel for OFF --
	// the pre-grace behaviour, idle means evictable -- and is what a snapshot
	// built without it (every table test, a caller that sets nothing) gets.
	// isEvictable is the one place that interprets it. A snapshot that sets a
	// grace but no Now is a caller bug and fails CLOSED: nothing is evictable,
	// rather than everything.
	EvictionGrace time.Duration
	// Budgets maps a GPU index to its VRAM budget in MB. An index that is
	// ABSENT and an index whose budget is 0 are the SAME thing:
	// unconstrained. That is the gateway data model's own definition --
	// see the doc comment on routing.ServerGPUBudget.BudgetMB in
	// gateway/backend/internal/routing/store.go, which spells out that
	// "no budget for this GPU" is expressed either way; if the meaning of
	// 0 ever changes there, it must change here in the same commit.
	//
	// A 0 is real operator input, not a defensive hypothetical: the
	// portal's SetServerGPUBudgets rejects only NEGATIVE values, and the
	// runtime screen seeds a fresh budget row at 0 MB. Admit therefore
	// skips any index whose budget is <= 0 (see rule 3) rather than
	// treating it as a ceiling of zero, which would refuse every spec on
	// that GPU terminally. This also keeps the budget consistent with
	// every other zero-value in this feature -- MaxProcesses above,
	// Spec.IdleTimeoutSeconds, AdmissionWaitTimeoutSeconds, ListenPort,
	// SpecGPU.VRAMMB -- where 0 means unbounded, automatic, or unknown,
	// never "off".
	//
	// That <= 0 rule is unchanged, but owner.buildSnapshot rarely feeds it a
	// 0 for a GPU the agent can read: its budget auto-detect (ADR-083,
	// fillDetectedBudgets) replaces a 0 or absent index with the detected
	// device total (minus any configured VRAM headroom) before Admit runs, so a
	// 0 stays truly unconstrained only for a GPU-less host, an Apple GPU, or
	// before the first hardware sample (a positive operator budget always wins
	// verbatim). The gateway-side comment on routing.ServerGPUBudget.BudgetMB
	// states the same.
	Budgets map[int]int
	// HostRAMBudgetMB is the host-RAM pool in MB that every running process's
	// RAMMB plus the candidate's own demand must fit under: the capacity
	// available to the MANAGED set, a total like a per-GPU budget rather than a
	// free-memory figure. It is ONE host-wide figure -- RAM is not per-GPU -- and
	// 0 is UNCONSTRAINED, the same sentinel a per-GPU budget uses: Admit skips
	// the RAM rule when it is <= 0, and that check lives there rather than in the
	// builder for the same reason (see Budgets). owner.buildSnapshot sets it to
	// the host's LIVE available memory plus the RAM the managed processes
	// already hold, minus the operator's reserve (see hostRAMBudgetMB for why
	// they are added back), and leaves it 0 before the first hardware sample.
	HostRAMBudgetMB int
	// HostRAMCeilingMB is the MOST the host-RAM pool could possibly offer, and it
	// exists for one reader: the terminal own-demand refusal (ramOwnDemandRefusal),
	// which must never call a candidate hopeless on the strength of a budget that
	// is merely UNDERSTATED. HostRAMBudgetMB deliberately is conservative -- it
	// adds back only what the managed processes have been MEASURED to hold, and
	// follows a sample that lags the running set -- which is right for the
	// admission arithmetic (it can only refuse or evict, never over-admit) but
	// wrong for a durable verdict, because the budget is transiently too low while
	// a process is still loading or unmeasured, and for a sample or two after one
	// has exited (the victim of the eviction that was just done for this very
	// candidate). The ceiling is the generous reading of the same quantity; the
	// builder documents exactly how (owner.buildSnapshot). 0 means no ceiling was
	// computed: Admit then falls back to HostRAMBudgetMB, and a ceiling BELOW the
	// budget is ignored, so a caller that sets only the budget gets the plain rule.
	HostRAMCeilingMB int
	// WSLRAMBudgetMB is the WSL VM's own RAM pool in MB (#381), which a wsl
	// candidate must fit IN ADDITION to the host pool: the VM has a ceiling of
	// its own (.wslconfig memory=, half the host's RAM by default), so wsl
	// workloads can exhaust it, and meet the VM's OOM killer, while the host
	// still has memory free. Only wsl processes (RunningProc.WSL) are charged
	// against it. It is a total for the managed wsl set like HostRAMBudgetMB,
	// built the same way from the VM's own MemAvailable (owner.buildSnapshot,
	// wslRAMBudgetMB), and 0 is the same UNCONSTRAINED sentinel: no sampling
	// call has answered yet, so the rule is skipped.
	WSLRAMBudgetMB int
	// WSLRAMCeilingMB is the most the VM pool could offer, the VM's MemTotal
	// minus the reserve: what wslRAMOwnDemandRefusal compares a wsl candidate's
	// own demand against, for the reason HostRAMCeilingMB exists. 0 falls back
	// to WSLRAMBudgetMB, and a ceiling below the budget is ignored.
	WSLRAMCeilingMB int
	// UnattributedVRAM is, per GPU index, the VRAM the WSL VM holds beyond what
	// the wsl processes' attributed figures account for (#381): growth after a
	// workload's start window closed, a start whose window was void, GPU use in
	// the VM the agent did not start, a figure that came out low -- and, since
	// every Hyper-V VM worker is summed, another VM's GPU memory. Windows
	// charges all of it to the VM's worker, so no process can carry it, and
	// rule 3 charges it to the GPU instead: it is added to the sum of every
	// GPU the candidate has a known demand on. It is never a victim (evicting
	// nothing frees it) and never part of the own-demand refusal, which reads
	// only the candidate and its budget. owner.buildSnapshot reads it fresh from
	// the VM at every admission (wslRemainder); nil -- every native-only host,
	// every table test -- charges nothing.
	UnattributedVRAM map[int]int
	// WSLStartInFlight is true while a wsl start's VRAM window is open: its
	// figure is the VM's growth across the start (owner.wslStartWindow), which
	// holds only if no other wsl start grows the VM meanwhile. So every wsl
	// candidate that declares a GPU Waits while it is set (wslStartWait), right
	// after the own-demand refusals and before anything that could evict. A
	// native candidate is not held back, and neither is a wsl one with no GPU:
	// it is taken to hold no VRAM, so it grows no window. Stops are never held
	// back by it.
	WSLStartInFlight bool
	// RAMBootstrapInFlight is the cold-start RAM interlock (task 4.1): true while
	// some managed process is still in its UNACCOUNTED bootstrap window -- unknown
	// declared RAM and no measurement yet, so rule 6 charges it nothing and the
	// host sample has not caught up with what it is in the middle of allocating.
	// Its true cost being unknown, a second RAM admission that takes the
	// apparently-free RAM mid-window is exactly how the bootstrap causes the OOM it
	// exists to prevent, so Admit makes EVERY other RAM admission Wait (WaitOnRAM)
	// while it is set, evicting nothing -- see rule 6 in Admit.
	//
	// It is computed fresh by owner.buildSnapshot from authoritative owner state
	// (the bootstrapping spec's state, its declared demand, its measurement, and
	// whether a RAM measurer is installed), never stored, so it cannot go stale or
	// race, and it always clears: with a measurer when the process is measured
	// (the brief's "healthy+measured"), otherwise when it leaves Starting, and in
	// both cases when it exits or an operator fills its estimate in. Admit only
	// acts on it when RAM is actually being accounted (HostRAMBudgetMB > 0).
	RAMBootstrapInFlight bool
	// RAMBootstrapGuessMB is the conservative host-RAM footprint Admit charges an
	// UNKNOWN-RAM candidate (spec.RAMMB == 0) on its first start, in place of the
	// nothing it would otherwise contribute to rule 6 (task 4.2). It is operator
	// configuration (OP_AGENT_RAM_BOOTSTRAP_GUESS_MB, owner.ramBootstrapGuessMB)
	// copied through verbatim; 0 is the sentinel for OFF -- the pre-bootstrap
	// behaviour, an unknown-RAM spec starting unconstrained -- so Admit substitutes
	// the guess only when it is > 0 (and only when HostRAMBudgetMB > 0, i.e. RAM is
	// being accounted at all).
	//
	// It drives ONLY rule 6's eviction/wait arithmetic, never a terminal refusal:
	// the guess is an inflated stand-in, not a measured demand, so a guess the pool
	// cannot hold waits (resolvable by a sample, an estimate, or room opening up),
	// where a KNOWN demand that large is refused (ramOwnDemandRefusal). It applies
	// to the CANDIDATE only; an occupant's own RAM is always its RunningProc.RAMMB.
	RAMBootstrapGuessMB int
	// VRAMBootstrapEligible grants the candidate its ONE guarded VRAM bootstrap
	// eviction (task 4.3). ADR-032 makes rule 4 -- a candidate whose own VRAM
	// demand is unknown -- BLOCK rather than evict a known-demand occupant, which
	// is the total order that stops a mixed pair cold-loading each other forever.
	// That order is correct in steady state but strands a spec that has NEVER been
	// measured: while any known-demand occupant holds one of its cards it can
	// never run alone, so it can never capture the first measurement that would
	// move it out of the unknown class. When this is true, rule 4 instead evicts
	// the idle known-demand touchers of the candidate's cards (blocking on any
	// that cannot be evicted), exactly as rule 5 treats unknown occupants, so the
	// candidate gets a card to itself and is measured.
	//
	// It is strictly a per-candidate grant the OWNER computes and sets on the
	// snapshot before each admission (owner.vramBootstrapEligible), true only
	// while a VRAM measurer is installed (the exception exists to capture a
	// measurement, so it never fires where none can be taken -- §5.3), the
	// candidate's spec has an unknown VRAM demand, AND its bootstrap is unresolved
	// -- no measurement captured and the one shot unspent
	// (specState.vramBootstrapDone). The owner withdraws it the instant the
	// bootstrap resolves, and a bootstrap eviction sets Decision.VRAMBootstrap so
	// the owner knows to burn the shot. That withdrawal is what makes the exception fire at
	// most once per spec: it CANNOT repeat, so it cannot reintroduce the ADR-032
	// thrash. It never affects a known-demand candidate (rule 4 does not run for
	// one) and never touches rule 5's eviction right (the winning side of the
	// order), so false -- the default every table test and every already-measured
	// spec gets -- is pure ADR-032.
	VRAMBootstrapEligible bool
	// NoEvict makes this admission all-or-nothing and immediate: the candidate
	// may start only into room that is free RIGHT NOW. It is a per-candidate
	// setting like VRAMBootstrapEligible, which the owner sets for a BURST
	// instance (#340, IsBurst): burst capacity is opportunistic, so it must
	// never drain-stop a neighbour, queue behind one, or spend a bootstrap shot
	// to get in -- the gateway would rather place it on another host. Under it
	// Admit answers OK or Decision.NoCapacity and nothing else (step 8 in
	// Admit). false -- every baseline instance, every spec that never
	// replicates, every table test -- leaves Admit exactly as it was.
	NoEvict bool
	// Allowed is the canonical (a<=b) spec-ID pair set: true = the pair
	// may run together. Build it with Config.AllowedPairs(), never by
	// hand-rolling a map from Config.Coresident's raw wire-order pairs --
	// AllowedPairs canonicalizes each pair via PairKey so a lookup never
	// depends on which side is the candidate and which is the
	// already-running process.
	Allowed map[[2]string]bool
}

// Decision is Admit's answer. Exactly one of four shapes holds, plus a fifth,
// NoCapacity (see that field), that only a NoEvict admission answers:
//   - OK: spec may start now. Evict is empty, Wait is false, Reason is "".
//   - Evict-then-retry: OK is false, Evict names the idle victims to
//     drain-stop first, oldest LastUsed first (the instances of one spec
//     highest index first, see siblingsHighestFirst). Once they are gone,
//     Admit applied again to the reduced running set is expected to return OK.
//   - Wait: OK is false, Wait is true, Evict is empty. Nothing MAY be evicted
//     to help right now, so the caller queues the request and re-asks on the
//     next completion. Two different situations reach that, and only the
//     first is "nothing CAN be evicted": every remaining blocker is busy,
//     still loading, used within the eviction grace window, or (outside the
//     unknown-VRAM case below) pinned. In the
//     second, a blocker plainly could be evicted and deliberately is not --
//     rule 4's known-demand occupant, which an unknown-demand candidate is no
//     longer allowed to drain-stop (see rule 4 in Admit). Wait is still the
//     right shape for it: an idle occupant is resolvable, just by its own
//     idle timeout rather than by an eviction this decision asks for. What
//     Wait promises the caller is therefore only that re-asking later may
//     succeed -- never that it exhausted the eviction candidates. ONLY THAT
//     SECOND SITUATION SETS Message, because only it can fail to resolve on
//     its own: an occupant with idle_timeout_seconds 0 (never unload) holds
//     the card indefinitely, so the wait is diagnosed from the message rather
//     than from the absence of a start. The first keeps Message empty -- a
//     spec queued behind a busy neighbour is ordinary operation. A wsl
//     candidate held back by the WSL start interlock (WaitOnWSLStart) also
//     waits without evicting what could be evicted, but it keeps Message
//     empty too: the window it waits for always closes, at the latest when
//     that start settles, fails or times out.
//   - Reason: OK is false, Wait is false, Evict is empty, and Reason names
//     a durable, non-transient block -- one that cannot resolve by
//     eviction OR by waiting, so reporting it as either of the other two
//     outcomes would be actively misleading (Wait would hang the caller
//     until its admission-wait timeout on every single request; Evict
//     would ask for a drain-stop that can never restore a fit). Two
//     States share this shape, carrying five distinct causes between
//     them, and Message is what tells those causes apart:
//   - StatePendingVRAMUnknown: a VRAM demand on one of the contested
//     GPUs is unknown -- the CANDIDATE's own (rule 4) or a running
//     OCCUPANT's (rule 5) -- and a PINNED process sits on the other
//     side of that contention: pinned processes are never evicted,
//     and a pinned process never finishes, so waiting cannot help
//     either. Message NAMES THE OCCUPANT when the unknown demand is
//     the occupant's, because the estimate an operator has to fill in
//     then lives on a DIFFERENT spec than the one displaying this
//     state; it is empty for the candidate's own unknown demand,
//     where the state is already attached to the only spec involved.
//     Terminal is a report, not a permanent verdict: the manager
//     re-evaluates after notPermittedRetryInterval. What can clear it
//     is HOST-DEPENDENT -- where a measurer is installed, the
//     housekeeping beat measures the pinned occupant and the block
//     clears with nothing evicted, but measurement is NVIDIA-only
//     (§5.3), so on an AMD, Apple-silicon or GPU-less host nothing
//     clears it except an operator filling the estimate in.
//   - StateNotPermitted: a durable operator-configuration block. Four
//     causes reach it from here. (1) Spec's own declared demand on
//     some GPU exceeds that GPU's budget all by itself, whatever else
//     is running -- an over-estimated VRAM demand, or a budget shrunk
//     below it (rule 3's own-demand half, which is why that half is
//     evaluated FIRST: no running set can change its verdict).
//     (2) A pinned occupant of unknown demand holds one of spec's
//     GPUs AND the co-residency pair is not permitted, where the
//     closed matrix cell -- not the missing VRAM number -- is what
//     actually blocks. (3) Spec's own known RAM demand exceeds the
//     whole host-RAM budget all by itself -- the host-wide twin of (1),
//     evaluated just after it for the same reason (ramOwnDemandRefusal).
//     (4) A wsl spec's own known RAM demand exceeds the WSL VM's whole
//     RAM ceiling all by itself -- (3) against the VM's pool, evaluated
//     right after it (wslRAMOwnDemandRefusal).
//     None resolves by eviction or by waiting. All carry the same
//     "configuration error, visible in the portal, not transient"
//     meaning the design doc already assigns StateNotPermitted for a
//     refused binary/directory, so Message is the only thing separating
//     all five.
type Decision struct {
	OK     bool
	Reason State // "" unless a terminal case above applies
	// Message is the operator-facing explanation, set on exactly two shapes:
	// alongside a terminal Reason that needs disambiguating context, and
	// alongside the precedence-induced Wait (rule 4), which is the one Wait
	// that need never resolve by itself. "" otherwise -- and in particular on
	// every ordinary Wait.
	Message string
	Evict   []string // spec IDs (instance keys) to drain-stop first, oldest LastUsed first, siblings highest index first; never nil
	Wait    bool
	// WaitOnRAM is set only together with Wait, and only when rule 6 (the
	// host-RAM pool) was among what blocked: the idle processes could not free
	// enough. It tells the caller this particular Wait can end by itself the
	// moment the hardware sample refreshes -- the pool is built from the host's
	// live MemAvailable, and the sample that follows a victim's exit is the one
	// that finally shows its freed RAM -- unlike a Wait on the matrix, the
	// process limit or a VRAM budget, which no new sample can change. The
	// manager re-asks a WaitOnRAM spec when a sample lands, and nothing else
	// (see owner.wakeOnHostSample). The WSL VM pool (ruleWSLRAM) sets it too:
	// its sample lands with the measurement beat, which re-asks the same
	// specs (owner.applyMeasurement).
	WaitOnRAM bool
	// WaitOnWSLVM is set only together with WaitOnRAM, and only when the WSL
	// VM pool's arithmetic (ruleWSLRAM) was among what blocked -- not its
	// bootstrap interlock, which no sample resolves. It tells the caller that a
	// fresh VM sample may end this Wait: the VM's figure is refreshed only
	// while a wsl process is live to be measured, so with none live the owner
	// has to ask for one itself (owner.wslVMWaitDistros).
	WaitOnWSLVM bool
	// WaitOnWSLStart is set only together with Wait, and only when a wsl
	// candidate waits behind another wsl start's open VRAM window
	// (PolicySnapshot.WSLStartInFlight). The window's close ends it, and the
	// owner re-asks such a spec when it does (owner.wakeOnWSLSettle).
	WaitOnWSLStart bool
	// WaitOnGrace is set only together with Wait, and only when this Wait would
	// have been a non-Wait (an OK, or an Evict) had NOTHING been grace-protected:
	// the admission is blocked SOLELY by one or more within-grace victims, each of
	// which would otherwise be drawn. It is the grace twin of WaitOnRAM and the one
	// other kind of Wait that resolves with nothing happening to any process -- not
	// a sample this time but the mere passing of time, as the earliest within-grace
	// victim's window expires and it becomes evictable. Nothing else wakes it: the
	// release that stamped its victim's LastUsed fired before the window opened, the
	// housekeeping beat only scans and measures, and wakeOnHostSample wakes only RAM
	// waiters -- so at the production admission wait of 0 a start blocked purely by
	// grace would hang until its caller gave up. The manager therefore schedules a
	// wake at that expiry for a WaitOnGrace spec, and only for such a spec (see
	// owner.rescheduleGraceWake / wantsGraceWake). It is computed by a counterfactual
	// re-run of this admission with the grace window off (decide); when BOTH this and
	// WaitOnRAM are set, either wake can end the Wait, and each is harmless if it
	// fires first (the re-ask simply blocks again).
	WaitOnGrace bool
	// VRAMBootstrap is set only on an Evict decision, and only when that decision
	// spent the candidate's one-shot VRAM bootstrap -- i.e. rule 4 evicted a
	// known-demand occupant it would otherwise only have blocked (ADR-032),
	// because snap.VRAMBootstrapEligible granted the shot. It is the signal the
	// owner uses to BURN the shot (set specState.vramBootstrapDone), so the
	// exception fires at most once per spec. It is never set on an OK, a Wait or a
	// terminal, and never for rule 5's eviction (that is the order's winning side,
	// not a bootstrap): an eviction driven purely by the matrix, a budget, rule 5
	// or the process limit leaves it false, so the owner burns the shot only when
	// the bootstrap actually used it.
	VRAMBootstrap bool
	// NoCapacity is the fifth shape, and the only non-OK one a NoEvict
	// admission answers (PolicySnapshot.NoEvict): OK, Wait, WaitOnRAM,
	// WaitOnGrace and VRAMBootstrap are false, Reason is "", Evict is empty, and
	// Message names what the pipeline would otherwise have answered -- the
	// victims it would have evicted, what it would have waited for, or the
	// terminal cause. It replaces every one of those, terminals included,
	// because a burst instance is never queued, never evicts and never parks in
	// a durable state: the caller refuses the start at once and the gateway
	// tries another host. Never set without NoEvict.
	NoCapacity bool
}

// baseOf returns the base spec ID r is an instance of: r.Spec, or r.SpecID
// when Spec is unset -- a process built without instance fields is its own
// base, which keeps every non-replicable spec (and every snapshot that never
// heard of instances) exactly as it was. See RunningProc.Spec.
func baseOf(r RunningProc) string {
	if r.Spec == "" {
		return r.SpecID
	}
	return r.Spec
}

// specBase is baseOf's candidate-side twin: the base spec ID spec is an
// instance of -- spec.InstanceOf for an instance copy k >= 1, else spec.ID
// itself (instance 0, and every spec that never replicates).
func specBase(spec Spec) string {
	if spec.InstanceOf == "" {
		return spec.ID
	}
	return spec.InstanceOf
}

// PairKey canonicalizes an (a, b) spec-ID pair to lexical a<=b order, so an
// Allowed lookup never depends on which side is the candidate and which is
// the already-running process.
func PairKey(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// isEvictable reports whether r may ever appear in a Decision.Evict list:
// never a pinned process, never one that is still loading, only when it is
// serving no request right now, and only once it has also been idle for longer
// than the grace window:
//
//	!Pinned && !Starting && InFlight == 0 && now - LastUsed > grace
//
// THE GRACE CLAUSE is the hard protection for a bursty pipeline. InFlight == 0
// says a process is idle THIS instant, not that it is finished: a voice chat
// leaves its speech-to-text and text-to-speech models idle between turns, and a
// new start must not take one of them in the gap. A process used within grace
// of now therefore counts as in use -- never a victim, regardless of pressure --
// and when the only way to make room is such a process the admission Waits
// (decide), exactly as it does behind a busy one. The comparison is strict:
// evictable only when now-LastUsed is GREATER than grace, so a process used
// exactly one grace ago is still protected, and one that was never used (zero
// LastUsed) is the oldest thing there is.
//
// grace <= 0 turns the clause off -- the pre-grace behaviour, which every
// snapshot built without PolicySnapshot.EvictionGrace keeps. A positive grace
// with a zero now fails CLOSED (now.Sub(LastUsed) is then hugely negative, so
// everything reads as within grace): a caller that configured protection and
// forgot the clock gets no evictions, never unprotected ones.
//
// THE Starting CLAUSE IS THE C3 FIX, and it is load-bearing rather than a
// refinement. InFlight == 0 was meant to mean "idle, so nothing is lost by
// stopping it", but a process that is still LOADING also has InFlight == 0
// while being the opposite of idle: it is the most expensive thing on the
// host, and the request that asked for it is still queued on it. Evicting
// one produced an unbounded fork-exec loop with no delay anywhere in it:
//
//	request for A -> A execs, Starting, A's waiter queued
//	request for B -> A is "idle" -> Evict[A] -> A has nothing in flight, so
//	                it is SIGTERMed immediately
//	A exits        -> intentional stop -> Stopped -> A's own waiter is still
//	                queued and nothing is running, so A execs AGAIN
//	                -> ... and the same event wakes B, which evicts A again.
//
// Measured at 86a287b: 1762 execs in 1.5s (two 700MB specs against a 1000MB
// budget) and 2267 in 2.3s (max_processes: 1). Neither request ever
// progressed; on a real host each iteration is a model server beginning to
// map a model file and then being killed. It is unbounded rather than
// self-limiting because admission_wait_timeout_seconds defaults to 0 ("wait
// until the client disconnects"), and a positive timeout only bounds the
// DURATION -- 1s still measured 1284 execs.
//
// With this clause the same collision terminates: B's admission finds a
// blocker it may not evict, so it Waits (bounded by StartupTimeoutSeconds,
// which is what actually bounds StateStarting), A finishes loading and
// serves the request it was started for, and B evicts A once it is genuinely
// idle. Every exec now buys at least one served request.
//
// Note this is NOT the same guard as Pinned: a pinned process is
// permanently unevictable, whereas Starting is a state every process passes
// through exactly once per generation, and always leaves (healthy, start
// timeout, or exit).
func isEvictable(r RunningProc, now time.Time, grace time.Duration) bool {
	return !r.Pinned && !r.Starting && r.InFlight == 0 && !withinGrace(r.LastUsed, now, grace)
}

// withinGrace reports whether a process last used at lastUsed is still inside
// the grace window at now: grace is on (> 0) and now-lastUsed has not exceeded
// it. See isEvictable for why the bound is inclusive and a zero now reads as
// inside.
func withinGrace(lastUsed, now time.Time, grace time.Duration) bool {
	return grace > 0 && now.Sub(lastUsed) <= grace
}

// firstSharedGPU returns the first index in idx that r occupies, and whether
// there is one. idx is spec.GPUs order (see specGPUIndexes), so the answer is
// deterministic and names the same card rule 3's messages name.
func firstSharedGPU(r RunningProc, idx []int) (int, bool) {
	for _, i := range idx {
		if _, ok := r.GPUs[i]; ok {
			return i, true
		}
	}
	return 0, false
}

// touchesAnyGPU reports whether r occupies any GPU index named in idx.
func touchesAnyGPU(r RunningProc, idx []int) bool {
	_, ok := firstSharedGPU(r, idx)
	return ok
}

// specGPUIndexes returns every GPU index spec.GPUs names, known-VRAM and
// unknown-VRAM alike. It is the set of cards the candidate CONTESTS, and both
// unknown-VRAM rules are keyed on it: rule 4 requires a spec of unknown
// demand to start alone on ALL of its GPUs, not only the one(s) whose demand
// happens to be unknown, and rule 5 asks the mirror question of an occupant
// -- does it hold any of these cards -- so that the two sides agree on what
// "the same card" means.
func specGPUIndexes(spec Spec) []int {
	idx := make([]int, len(spec.GPUs))
	for i, g := range spec.GPUs {
		idx[i] = g.Index
	}
	return idx
}

// specHasUnknownVRAM reports whether spec has any GPU whose declared demand
// is unknown (VRAMMB == 0).
func specHasUnknownVRAM(spec Spec) bool {
	for _, g := range spec.GPUs {
		if g.VRAMMB == 0 {
			return true
		}
	}
	return false
}

// specHasUnknownRAM reports whether spec's host-RAM demand is unknown
// (RAMMB == 0), the host-wide twin of specHasUnknownVRAM. Like that one it reads
// only the spec's own declared demand, and 0 is UNKNOWN, never "needs none" (see
// Spec.RAMMB). It is the first half of the cold-start bootstrap condition: the
// owner pairs it with "no measurement captured yet" (needsRAMBootstrap) to decide
// a first start, and Admit uses it to recognise the unknown-RAM candidate a
// bootstrap guess stands in for (rule 6).
func specHasUnknownRAM(spec Spec) bool {
	return spec.RAMMB == 0
}

// needsRAMBootstrap reports whether a spec's NEXT start is a RAM cold-start
// bootstrap: its declared demand is unknown (specHasUnknownRAM) AND no real
// host-RAM measurement has been captured for its current process yet. A
// non-positive measuredRAMMB is NOT a measurement (realRAM's rule: 0 or less is
// "unknown", never "measured zero"), so it still counts as a first start.
//
// It is the predicate the owner evaluates against a RUNNING process to decide
// whether that process is still in its unaccounted bootstrap window -- the
// interlock in buildSnapshot. For the CANDIDATE being admitted the measurement is
// always absent (admitAndStart runs only with no live process, which onProcExited
// has cleared the measurement for), so there the condition reduces to
// specHasUnknownRAM.
func needsRAMBootstrap(spec Spec, measuredRAMMB int) bool {
	return specHasUnknownRAM(spec) && measuredRAMMB <= 0
}

// firstUnknownGPU returns the LOWEST GPU index whose demand r reports as
// unknown (a key PRESENT with a value of 0), and whether there is one.
//
// The lowest, not the first one found: RunningProc.GPUs is a map and Go
// randomizes map iteration, so "the first hit" would name a different card on
// every admission for a process with two unknown cards -- and this index goes
// into an operator-facing Message.
func firstUnknownGPU(r RunningProc) (int, bool) {
	lowest, found := 0, false
	for i, v := range r.GPUs {
		if v != 0 {
			continue
		}
		if !found || i < lowest {
			lowest, found = i, true
		}
	}
	return lowest, found
}

// procHasUnknownVRAM reports whether r occupies ANY GPU whose demand is
// UNKNOWN. It is the occupant-side mirror of specHasUnknownVRAM, and rule 5
// in Admit is the rule it feeds.
//
// WHY THERE IS A RULE 5 AT ALL. Rule 4 admits a spec of unknown demand only
// if it runs ALONE on its GPUs -- and without this predicate that guarantee
// expired the moment it was granted. Both unknown-VRAM rules keyed on the
// CANDIDATE, while rule 3's arithmetic charges an occupant whatever
// r.GPUs[g.Index] says, which for an occupant of unknown demand is 0. So
// the spec that had just been given a card to itself was charged nothing,
// and the very next admission put a second process on that card no matter
// how large the first really was, and no matter whether it was idle, busy
// or PINNED. Measured before the fix: an occupant of unknown demand
// produced OK with an empty Evict list in all three of those cases, where
// the same occupant declaring 6000 MB produced Evict. Both processes could
// then OOM, which is the one outcome the whole budget feature exists to
// prevent. It was never a deliberate asymmetry -- §5.3 accepts that an
// unknown-demand spec WAITS behind a pinned or busy holder, which is only
// coherent if the reverse also holds.
//
// THE SIGNAL IS A KEY THAT IS PRESENT WITH A VALUE OF 0, and both halves
// are load-bearing. RunningProc.GPUs holds the EFFECTIVE figure -- the
// measured value when the agent has one, else the spec's estimate --
// and SpecGPU.VRAMMB defines 0 as "unknown demand, never a real zero-cost
// claim" (owner.buildSnapshot copies that 0 through verbatim, and
// realMeasurement is what keeps a measured 0 from arriving here dressed up
// as a measurement). An ABSENT key means something else entirely: the
// process is not on that card at all, which says nothing about its demand.
//
// THE SCOPE IS THE WHOLE PROCESS, AND THAT IS WHAT MAKES RULE 5 RULE 4's
// MIRROR -- in SUBJECT. Which of the two sides may evict the other is a
// separate question, settled the other way (known demand beats unknown
// demand: rule 4 blocks, rule 5 evicts; see rule 4 in Admit).
// This predicate asks "does r have an unknown demand anywhere",
// exactly as specHasUnknownVRAM asks it of the candidate; Admit then pairs it
// with touchesAnyGPU, exactly as rule 4 does. It was narrower once -- "is r
// unknown at one of the CANDIDATE's indexes" -- which made rule 5 a mirror in
// name only, because rule 4's promise is aloneness on ALL of the unknown
// spec's GPUs (see specGPUIndexes), not just its unknown ones. Measured on
// the narrow version: an occupant of {gpu 0: unknown, gpu 1: 5000} was
// admitted by rule 4 only on the promise of running alone on BOTH cards, yet
// a candidate asking for gpu 1 alone found nothing unknown there and got
// OK -- with that occupant idle, busy AND pinned -- silently revoking on
// gpu 1 the aloneness rule 4 had just granted. That is the same expiring
// promise rule 5 exists to close, one card over.
//
// The CONTENTION half stays per-GPU: an occupant whose cards the candidate
// never touches competes for nothing and is ignored (see
// TestAdmitUnknownOccupantOnOtherGPUIsIgnored). Widening THAT to "any
// unknown-demand process anywhere" would evict half the host on every start.
//
// The test is `== 0`, not `<= 0`, so that it stays literally the test
// specHasUnknownVRAM and rule 3 apply to a spec's declared demand. A
// negative figure is not reachable (the portal rejects negative VRAM, and
// realMeasurement admits only strictly positive measurements), and a
// second, differently-shaped notion of "unknown" is exactly how the two
// sides of a symmetric rule drift apart.
//
// WHY AN EXPLICIT RULE AND NOT AN ARITHMETIC FIX. Charging an unknown
// occupant some large number in rule 3 instead does not work: that loop
// releases an evicted toucher with `sum -= r.GPUs[g.Index]`, which
// subtracts the same 0, so the eviction never reduces the sum. Rule 3 would
// evict every idle toucher on the card, still find itself over budget, and
// answer Wait -- destroying running work AND blocking the candidate. The
// charge and the release would both have to be made consistent, inventing a
// VRAM figure for a process nobody has measured; naming the contention
// instead needs no number.
func procHasUnknownVRAM(r RunningProc) bool {
	_, ok := firstUnknownGPU(r)
	return ok
}

// ownDemandRefusal returns rule 3's own-demand terminal -- spec's declared
// demand on one GPU exceeds that GPU's budget all by itself -- or ok == false
// when every declared demand fits its own budget.
//
// IT READS ONLY spec AND snap.Budgets, WHICH IS WHY IT MAY RUN FIRST, ahead
// of every rule that looks at the running set at all. Nothing about it
// depends on what is running, what is pinned or what could be evicted, so no
// ordering can change its verdict -- while its verdict is the most durable
// one Admit has: the candidate cannot start until an operator edits a number,
// whatever else happens on the host.
//
// Running it first is a FIX, not a tidy-up. It used to sit inside rule 3's
// loop, behind the pinned unknown-VRAM short-circuit, and a snapshot
// satisfying both reported the wrong one: candidate demand 9000 MB against a
// budget of 8000 MB, plus a pinned occupant of unknown demand on the same
// card, answered pending_vram_unknown -- inviting the operator to go fill in
// a DIFFERENT spec's estimate, which cannot make 9000 MB fit under 8000 MB.
// The shadowed cause is permanent and the reported one was not, which is the
// wrong way round; §5.2 is explicit that Message is the only thing telling a
// budget refusal apart from a local-policy refusal, and an empty one told
// nothing.
func ownDemandRefusal(snap PolicySnapshot, spec Spec) (Decision, bool) {
	for _, g := range spec.GPUs {
		if g.VRAMMB == 0 {
			continue // unknown demand: rules 4 and 5, never the arithmetic
		}
		// Absent from Budgets and a budget of 0 both mean unconstrained and
		// must behave identically -- see PolicySnapshot.Budgets, and the
		// longer note at rule 3, which is the other reader of this rule.
		budget := snap.Budgets[g.Index]
		if budget <= 0 {
			continue
		}
		if g.VRAMMB > budget {
			return Decision{
				Reason: StateNotPermitted,
				Message: fmt.Sprintf(
					"spec %s: gpu %d demand %d MB exceeds budget %d MB on its own",
					spec.ID, g.Index, g.VRAMMB, budget,
				),
				Evict: []string{},
			}, true
		}
	}
	return Decision{}, false
}

// ramOwnDemandRefusal is rule 6's own-demand terminal, the host-wide twin of
// ownDemandRefusal: spec's KNOWN RAM demand exceeds the whole host-RAM pool all
// by itself. It returns the refusal, or ok == false when the rule does not
// apply or the demand fits.
//
// The pool is a TOTAL for the managed set (see PolicySnapshot.HostRAMBudgetMB),
// so the smallest sum rule 6 can ever compute is the candidate alone -- every
// other process evicted. A demand above the pool therefore cannot be made to
// fit by any eviction, and waiting cannot help either (nothing the managed set
// does changes the pool; only a non-managed workload releasing memory, or the
// operator lowering the estimate or the reserve, can). Answering Wait would
// hang the caller to its admission timeout on every request with nothing in the
// portal to explain it, so it is the same durable "configuration error, not
// transient" block as the per-GPU case. Like that one it is a report, not a
// permanent verdict: the manager re-evaluates after notPermittedRetryInterval
// and the spec starts on its own once the pool has grown back.
//
// WHAT IT COMPARES AGAINST IS THE CEILING, NOT THE BUDGET, and that is the
// difference from the per-GPU rule it otherwise mirrors. A GPU budget is a fixed
// device total; this pool is rebuilt from a live sample, so the budget can be
// transiently too LOW -- a process still loading or not yet measured has not
// been added back, and for a sample or two after a process exits the pool has
// already lost it while MemAvailable has not yet gained it. That last case is
// not an edge: it is exactly the state the beneficiary of an eviction is
// re-admitted in, and refusing it here turned a legitimate eviction into a
// durable ErrNotPermitted (its pending request failed, with a message about a
// budget it did not actually exceed) that no later sample could take back,
// because a refused spec is not a waiter. Against the ceiling -- the most the
// pool could be offering -- a candidate is refused only when it could not fit
// even then; anything between the budget and the ceiling falls through to the
// arithmetic, which Waits (and so is woken by the next sample) instead of
// refusing or evicting for a start that cannot yet be shown to fit.
//
// IT READS ONLY spec AND the snapshot's two RAM figures, which is why it may run
// before every rule that looks at the running set, for the reason
// ownDemandRefusal does: no ordering can change its verdict.
//
// It is skipped for both sentinels. A budget <= 0 is unconstrained, and
// spec.RAMMB == 0 is an UNKNOWN demand, never "needs none" -- not gated here,
// since routing it to a first measurement is the cold-start bootstrap's job.
func ramOwnDemandRefusal(snap PolicySnapshot, spec Spec) (Decision, bool) {
	if snap.HostRAMBudgetMB <= 0 || spec.RAMMB <= 0 {
		return Decision{}, false
	}
	limit := max(snap.HostRAMCeilingMB, snap.HostRAMBudgetMB)
	if spec.RAMMB <= limit {
		return Decision{}, false
	}
	return Decision{
		Reason: StateNotPermitted,
		Message: fmt.Sprintf(
			"spec %s: RAM demand %d MB exceeds the host RAM budget %d MB on its own",
			spec.ID, spec.RAMMB, limit,
		),
		Evict: []string{},
	}, true
}

// wslRAMOwnDemandRefusal is ramOwnDemandRefusal against the WSL VM's pool
// (#381): a wsl spec's KNOWN RAM demand exceeds the VM's whole RAM ceiling all
// by itself, so no eviction and no wait can make it fit. Everything that
// function says carries over unchanged -- the pool is a total for the managed
// wsl set, the comparison is against the CEILING (the VM's MemTotal minus the
// reserve, WSLRAMCeilingMB) so a transiently low budget never turns into a
// durable refusal, and both sentinels skip it -- with one more gate: only a
// wsl candidate lives in the VM, so a native spec is never measured against
// it. What resolves it is the operator: a larger .wslconfig memory=, a smaller
// reserve, or a smaller estimate.
func wslRAMOwnDemandRefusal(snap PolicySnapshot, spec Spec) (Decision, bool) {
	if !spec.IsWSL() || snap.WSLRAMBudgetMB <= 0 || spec.RAMMB <= 0 {
		return Decision{}, false
	}
	limit := max(snap.WSLRAMCeilingMB, snap.WSLRAMBudgetMB)
	if spec.RAMMB <= limit {
		return Decision{}, false
	}
	return Decision{
		Reason: StateNotPermitted,
		Message: fmt.Sprintf(
			"spec %s: RAM demand %d MB exceeds the WSL VM RAM budget %d MB on its own",
			spec.ID, spec.RAMMB, limit,
		),
		Evict: []string{},
	}, true
}

// pinnedUnknownOccupantRefusal is step 2's occupant half -- rule 5 against a
// PINNED occupant -- returning the terminal refusal to report, or ok == false
// when no pinned occupant of unknown demand holds any card spec wants.
//
// IT PICKS ITS SUBJECT DETERMINISTICALLY, and that is not cosmetic:
// PolicySnapshot.Running arrives in Go map order (owner.buildSnapshot ranges
// over o.specs), so "the first qualifying one" would name a different
// occupant -- and, with the matrix rule below, report a different Reason --
// on every admission against an unchanged host. A closed co-residency cell
// wins over an open one; equal-precedence occupants break on the lowest spec
// id, the same total order sortOldestFirst gives victims.
//
// A CLOSED MATRIX CELL IS REPORTED AS ITSELF, not as pending_vram_unknown.
// Both blocks are durable here -- a pinned process is never evicted and never
// drains -- but only one of them is resolvable by a VRAM number, and §5.3
// reserves pending_vram_unknown for a block that a measurement or a filled-in
// estimate DOES resolve. Filling in the occupant's estimate while the pair is
// closed resolves nothing: rule 1 refuses the pair whatever the numbers say.
// So the cause named is the one an operator can act on (open the cell, or
// unpin the occupant), under StateNotPermitted, which already carries exactly
// that meaning for the other two causes that reach it.
//
// THE ASYMMETRY WITH THE CANDIDATE SIDE IS DELIBERATE AND NARROW. Rule 4's
// pinned short-circuit gets no such carve-out: a candidate whose OWN demand
// is unknown, facing a pinned matrix-incompatible occupant, still reports
// pending_vram_unknown. That outcome long predates rule 5 -- it is the
// original §5.3 behaviour, and a manager-level test asserts it -- so revising
// it is a separate decision about the candidate side. What is corrected here
// is only what rule 5 itself introduced: before rule 5 existed, this exact
// snapshot answered Wait, which queued the request to its admission timeout;
// rule 5 turned it into a terminal naming a cause that resolves nothing.
//
// The pair is read exactly as rule 1 reads it (pairAllowed): by base spec, with
// siblings always open, so a pinned sibling of unknown demand is reported as
// the missing number it is, never as a closed cell the matrix cannot even hold.
func pinnedUnknownOccupantRefusal(snap PolicySnapshot, spec Spec, running []RunningProc, gpuIdx []int) (Decision, bool) {
	var (
		blocker    RunningProc
		sharedGPU  int
		pairClosed bool
		found      bool
	)
	base := specBase(spec)
	for _, r := range running {
		if !r.Pinned || !procHasUnknownVRAM(r) {
			continue
		}
		gpu, shares := firstSharedGPU(r, gpuIdx)
		if !shares {
			continue
		}
		closed := !pairAllowed(snap.Allowed, base, spec.IsRPCServer(), r)
		if found && !outranksAsBlocker(closed, r.SpecID, pairClosed, blocker.SpecID) {
			continue
		}
		blocker, sharedGPU, pairClosed, found = r, gpu, closed, true
	}
	if !found {
		return Decision{}, false
	}
	if pairClosed {
		return Decision{
			Reason: StateNotPermitted,
			Message: fmt.Sprintf(
				"spec %s: gpu %d is held by pinned spec %s, and that pair is not permitted by the co-residency matrix",
				spec.ID, sharedGPU, blocker.SpecID,
			),
			Evict: []string{},
		}, true
	}
	unknownGPU, _ := firstUnknownGPU(blocker) // procHasUnknownVRAM guarantees one
	return Decision{
		Reason: StatePendingVRAMUnknown,
		Message: fmt.Sprintf(
			"spec %s: gpu %d is held by pinned spec %s, whose own demand on gpu %d is unknown",
			spec.ID, sharedGPU, blocker.SpecID, unknownGPU,
		),
		Evict: []string{},
	}, true
}

// outranksAsBlocker is pinnedUnknownOccupantRefusal's precedence: a closed
// co-residency cell first (it is the cause no VRAM number resolves), then the
// lowest spec id.
func outranksAsBlocker(closed bool, id string, bestClosed bool, bestID string) bool {
	if closed != bestClosed {
		return closed
	}
	return id < bestID
}

// pairAllowed reports whether a candidate of base spec candidateBase may run
// next to r. Siblings (r is an instance of the same spec) always may: the
// gateway's matrix never holds a self-pair, so reading one would make every
// spec matrix-incompatible with its own instances. So may every pair with an
// rpc_server member on either side (candidateMember, r.Member): a member lends
// its devices to a head on another host and serves no model of its own, so the
// matrix, which records which models may share a host, has nothing to say
// about it -- its resources are still charged like any process's. Any other
// pair is looked up by BASE spec IDs, so an instance inherits its spec's
// verdicts on either side of the pair and the matrix needs no entries per
// instance (#340).
func pairAllowed(allowed map[[2]string]bool, candidateBase string, candidateMember bool, r RunningProc) bool {
	if candidateMember || r.Member {
		return true
	}
	occupantBase := baseOf(r)
	return occupantBase == candidateBase || allowed[PairKey(candidateBase, occupantBase)]
}

// sortOldestFirst orders procs by LastUsed ascending (ties on SpecID): the order
// decide reports the final Decision.Evict list in -- oldest idle victim first.
// That is only the DRAIN order of a set every member of which is already going;
// WHICH victims are in the set is decided by orderVictims below, the role-aware
// order the resource rules draw them in. Ties in LastUsed break on SpecID so the
// output is fully deterministic -- sort.Slice is not stable, and map iteration
// order (toEvict, in decide) is randomized per run, so without a total order a
// tie would otherwise pick a different victim on every run.
func sortOldestFirst(procs []RunningProc) {
	sort.Slice(procs, func(i, j int) bool {
		if !procs[i].LastUsed.Equal(procs[j].LastUsed) {
			return procs[i].LastUsed.Before(procs[j].LastUsed)
		}
		return procs[i].SpecID < procs[j].SpecID
	})
}

// orderVictims sorts an admission's evictable candidates into the order they are
// DRAWN in when a resource rule needs room (task 3.3): a victim of the
// candidate's OWN role before one of any other role, and within a role band the
// least-recently-used first (ties on SpecID for a total, deterministic order,
// exactly as sortOldestFirst). Same-role-first is the role swap of design doc
// §4.3 -- a new ASR prefers to evict the idle ASR, not the TTS a voice chat still
// needs between turns, keeping one model of each role alive -- and it crosses
// into another role only when its own-role peers do not free enough, so cross-
// role eviction stays available under real pressure.
//
// It changes only WHICH idle victim is chosen first, never WHETHER one may be
// evicted: isEvictable (incl. the grace window) is the gate every candidate has
// already passed through before it reaches here (evictableOf), and rules 1-6 and
// the resource arithmetic are otherwise untouched. The three victim-drawing
// loops (chargeGPUBudget, ruleHostRAM, ruleProcessCount) all call this in place
// of the plain oldest-first sort; decide then reports the resulting set oldest-
// first, which is only the drain order (see sortOldestFirst).
//
// The candidate's role is a.candidateRole (roleOf(spec.Role)), normalized once in
// newAdmission so this comparison matches the already-normalized
// RunningProc.Role every occupant carries (buildSnapshot sets it through roleOf).
//
// SURPLUS INSTANCES COME FIRST, ahead of the role band (#340): a running burst
// instance is the cheapest victim there is, because its spec keeps serving
// from a lower sibling -- so it also never strands a role, which is what the
// role band protects. Within the surplus band the existing
// keys apply. Within ONE spec the instances then go highest index first
// (siblingsHighestFirst): the draw loops ask evictable as they go, and a lower
// instance only qualifies once every higher running sibling is queued.
func (a *admission) orderVictims(procs []RunningProc) {
	sort.Slice(procs, func(i, j int) bool {
		if procs[i].Surplus != procs[j].Surplus {
			return procs[i].Surplus // a burst instance is drawn first
		}
		iSame := procs[i].Role == a.candidateRole
		jSame := procs[j].Role == a.candidateRole
		if iSame != jSame {
			return iSame // the candidate's own role is drawn first
		}
		if !procs[i].LastUsed.Equal(procs[j].LastUsed) {
			return procs[i].LastUsed.Before(procs[j].LastUsed)
		}
		return procs[i].SpecID < procs[j].SpecID
	})
	siblingsHighestFirst(procs)
}

// siblingsHighestFirst reorders the instances of each spec within the
// positions they already hold in procs, highest index first, leaving every
// other entry where it is. The order procs was sorted into decides WHEN a
// spec's turn comes (its best-placed instance sets it); the ordering invariant
// decides WHICH instance takes that turn -- always the highest one still there.
//
// It is a permutation applied after the sort rather than an instance key inside
// the comparator because the latter is not a strict weak order: "higher index
// first within a spec" next to "least recently used first across specs" can
// cycle (A#2 before A#1 by index, A#1 before B by age, B before A#2 by age), and
// sort.Slice's result is undefined for such a comparator. A no-op for a list
// with no two siblings, i.e. for every spec that never replicates.
func siblingsHighestFirst(procs []RunningProc) {
	slots := make(map[string][]int)
	for i, r := range procs {
		base := baseOf(r)
		slots[base] = append(slots[base], i)
	}
	for _, idx := range slots {
		if len(idx) < 2 {
			continue
		}
		members := make([]RunningProc, len(idx))
		for j, i := range idx {
			members[j] = procs[i]
		}
		sort.Slice(members, func(x, y int) bool {
			if members[x].Instance != members[y].Instance {
				return members[x].Instance > members[y].Instance
			}
			return members[x].SpecID < members[y].SpecID
		})
		for j, i := range idx {
			procs[i] = members[j]
		}
	}
}

// admission carries the mutable state Admit accumulates as it walks its
// deliberately linear, ordered rule pipeline. Admit builds one of these, then
// calls each rule in turn as a method on it; the function body is the sequence
// of those calls, in the single order the shared-state chaining makes correct.
//
// THE ORDER AND THE SHARED-STATE CHAINING ARE CORRECTNESS INVARIANTS, not a
// stylistic convenience. toEvict is chained: a process one rule queues there is
// seen by every LATER rule as notionally already gone -- its VRAM is dropped
// from rule 3's per-GPU sum, its RAM from rule 6's host sum, and it is never
// drawn a second time, so one eviction is never joined by a redundant innocent
// one. Reordering the rules would over-evict or double-evict. The methods below
// therefore read and WRITE this shared state in place and must stay in the
// order Admit calls them; each one's doc comment carries the reasoning that was
// inline in the single function before the extraction.
type admission struct {
	snap PolicySnapshot
	spec Spec
	// running is self-filtered -- never a RunningProc sharing spec.ID (the
	// instance key) -- and ordered siblings-highest-index-first (newAdmission),
	// so a rule that walks it and queues every qualifying occupant meets S#2
	// before S#1 before S, and each finds its higher siblings already queued.
	running     []RunningProc
	unknownVRAM bool
	gpuIdx      []int
	// base is specBase(spec): the spec the candidate is an instance of, the side
	// every sibling test and every matrix lookup uses (#340).
	base string
	// candidateRole is roleOf(spec.Role): the candidate's model role, normalized
	// once here so orderVictims can compare it against each occupant's already-
	// normalized RunningProc.Role (buildSnapshot sets that through the same roleOf).
	// Normalizing once, up front, is what keeps the comparison from reading a
	// cased or absent role as a role of its own. See orderVictims.
	candidateRole string

	toEvict map[string]RunningProc
	blocked bool
	// ramBlocked records that rule 6 was among the causes of blocked, which is
	// what Decision.WaitOnRAM reports. It is deliberately separate from blocked:
	// a start waiting on a busy matrix neighbour must not be woken by every
	// hardware sample, and only the RAM verdict depends on one.
	ramBlocked bool
	// vmBlocked records that the WSL VM pool's arithmetic was among the causes
	// of blocked (Decision.WaitOnWSLVM).
	vmBlocked bool
	// waitMessage is set by rule 4 alone, and only ever reaches a Wait: rule 4
	// setting it also sets blocked, and blocked always answers Wait. See
	// blockOnKnownTouchers for why that one Wait is the only one that explains
	// itself.
	waitMessage string
	// bootstrapEvicted records that rule 4 spent the candidate's one-shot VRAM
	// bootstrap -- it queued a known-demand toucher for eviction under the
	// snap.VRAMBootstrapEligible grant, the one case ADR-032 otherwise only
	// blocks. It reaches the caller as Decision.VRAMBootstrap, and ONLY on an
	// Evict decision: a bootstrap that could not free the card (a non-evictable
	// toucher) falls through to a plain Wait with an empty Evict list, so no shot
	// was actually spent and the owner must not burn it.
	bootstrapEvicted bool
}

// evictable is isEvictable against this admission's snapshot: the one gate every
// rule asks, so the grace window (snap.Now, snap.EvictionGrace) is applied to
// each of them identically and cannot be forgotten by a rule added later.
//
// Two instance clauses (#340) live here for the same reason.
// A SIBLING is never a victim for a candidate of its own spec: evicting a warm
// instance to start a cold one of the same model is never useful. And an
// instance is not evictable while a HIGHER-INDEX sibling runs -- the "highest
// index leaves first" ordering invariant, which keeps instance 0 the last of a
// spec to go and lets the burst/baseline split be decided by index alone. Both
// only shrink the victim set, so a blocker they spare reads exactly like a busy
// one: the start Waits.
//
// The invariant clause reads toEvict, so evictable is a question about NOW, in
// the middle of the pipeline: a sibling already queued to go no longer holds a
// lower one back (higherSiblingStays). That is what lets one decision evict a
// whole run of idle siblings, highest first, when the start needs all of them
// gone. Callers therefore ask it at the moment they would queue a victim, and
// walk siblings highest index first (newAdmission, orderVictims) so the higher
// ones are queued by the time the lower ones are asked about.
func (a *admission) evictable(r RunningProc) bool {
	return a.mayEvict(r) && !a.higherSiblingStays(r)
}

// mayEvict is evictable without the ordering-invariant clause: the part that
// does not change as victims are queued -- isEvictable, and never a sibling of
// the candidate. It is the prefilter for victim lists (evictableOf and rule 2's
// candidates); whether a listed victim may go yet is evictable's call, asked
// when it is drawn.
func (a *admission) mayEvict(r RunningProc) bool {
	return isEvictable(r, a.snap.Now, a.snap.EvictionGrace) && baseOf(r) != a.base
}

// higherSiblingStays reports whether a sibling of r with a higher index is
// running and NOT already queued in toEvict -- i.e. whether evicting r now would
// break "highest index leaves first".
//
// Discounting the queued ones is a liveness requirement, not an optimisation.
// Without it one admission could evict at most the highest instance of each
// spec, so a start that needs S#2 AND S#1 gone found S#1 unevictable, blocked,
// and answered Wait -- and a Wait evicts nothing, so S#2 was never drained either
// and the start sat until an idle unload happened to run.
func (a *admission) higherSiblingStays(r RunningProc) bool {
	base := baseOf(r)
	for _, o := range a.running {
		if o.Instance <= r.Instance || baseOf(o) != base {
			continue
		}
		if _, going := a.toEvict[o.SpecID]; !going {
			return true
		}
	}
	return false
}

// evictableOf returns the subset of procs that may be victims of this admission
// at all (mayEvict), preserving input order. It is the idle-victim prefilter
// rule 3 (chargeGPUBudget) and rule 6 (ruleHostRAM) share before ordering
// victims with orderVictims; each draw then asks evictable for the ordering
// invariant against what is already queued.
func (a *admission) evictableOf(procs []RunningProc) []RunningProc {
	var out []RunningProc
	for _, r := range procs {
		if a.mayEvict(r) {
			out = append(out, r)
		}
	}
	return out
}

// newAdmission builds the pipeline state for one admission decision. Step 0, the
// self-filter: a RunningProc sharing spec.ID -- e.g. a double-start race, or a
// snapshot assembled a moment too early or late -- is never a blocker or a
// victim for its own candidate (see Admit's step 0), so it is dropped once, up
// front, and none of the rules below can see it. The filter is by SpecID, the
// instance key, so a SIBLING stays in: it is an ordinary occupant whose VRAM and
// RAM are summed and which counts toward max_processes (#340).
//
// The kept set is then put in a total order -- by base spec, highest instance
// first, then SpecID -- so that the rules which queue EVERY qualifying occupant
// (1, 4's bootstrap arm, 5) meet a spec's instances highest index first and each
// lower one finds its higher siblings already queued (see evictable). Snapshot
// order carried no meaning to lose: it is Go map order (owner.buildSnapshot
// ranges over o.specs), and every rule's verdict was already independent of it.
func newAdmission(snap PolicySnapshot, spec Spec) *admission {
	running := make([]RunningProc, 0, len(snap.Running))
	for _, r := range snap.Running {
		if r.SpecID != spec.ID {
			running = append(running, r)
		}
	}
	sort.Slice(running, func(i, j int) bool {
		bi, bj := baseOf(running[i]), baseOf(running[j])
		if bi != bj {
			return bi < bj
		}
		if running[i].Instance != running[j].Instance {
			return running[i].Instance > running[j].Instance
		}
		return running[i].SpecID < running[j].SpecID
	})
	return &admission{
		snap:          snap,
		spec:          spec,
		running:       running,
		unknownVRAM:   specHasUnknownVRAM(spec),
		gpuIdx:        specGPUIndexes(spec),
		base:          specBase(spec),
		candidateRole: roleOf(spec.Role),
		toEvict:       make(map[string]RunningProc),
	}
}

// Admit answers: may spec start next to the processes already running in
// snap? It applies the design doc's §5 admission rule -- matrix
// compatibility, the process-count limit, per-GPU VRAM arithmetic and the
// host-wide RAM pool (rule 6) -- plus the unknown-VRAM-alone rule, which is
// applied to BOTH sides of the pair (rule 4 for the candidate's own unknown
// demand, rule 5 for a running occupant's), and computes the idle-victim set
// that would unblock the start if one exists.
//
// The two unknown-VRAM rules are symmetric in SUBJECT and asymmetric in
// OUTCOME, and the asymmetry is the point: an unknown demand makes a card
// unshareable from whichever side it sits on, but only the known-demand side
// may evict to get it. Known demand beats unknown demand -- a total order,
// adopted because the outcome-symmetric version had a mixed pair evicting in
// both directions and thrashing forever. Rule 4 (blockOnKnownTouchers) carries
// the measurement and the reasoning; the one case the order does not decide
// (both sides unknown) is a recorded acceptance in
// 11-risks-and-technical-debt.md §11.4.
//
// Kept as a small, deliberately linear pipeline rather than a clever one,
// since it will be read far more often than it will be changed. Each numbered
// step is a method on admission (step 0 is newAdmission), run by admitSteps in
// the one order the shared-state chaining makes correct; step 8 is applied here
// to whatever they answer:
//
//  0. Self-filter. A RunningProc sharing spec.ID -- e.g. a double-start
//     race, or a snapshot assembled a moment too early or late -- is never
//     a blocker or a victim for its own candidate: it is not a foreign
//     occupant competing with spec for anything (the gateway's coresident
//     matrix never contains a self-pair, so leaving it in would read as
//     matrix-incompatible with itself). Filtered out once, up front, so
//     none of the rules below can propose evicting the very process being
//     started or wait on it forever. spec.ID is the INSTANCE key (#340), so
//     the candidate's siblings stay in as ordinary occupants -- summed into
//     every resource rule, counted toward the process limit, but always
//     matrix-compatible (pairAllowed) and never victims (evictable).
//  1. Rule 3's own-demand half, hoisted ahead of everything that reads
//     the running set: a declared demand that exceeds its own GPU's
//     budget by itself refuses terminally, because no running set, no
//     eviction and no wait can change that verdict. It reads only spec
//     and snap.Budgets, so running it first cannot disturb another rule's
//     inputs -- and running it LAST let the resolvable cause in step 2
//     shadow this permanent one (see ownDemandRefusal). Its host-wide
//     twin follows it: a KNOWN RAM demand that exceeds the whole
//     host-RAM budget by itself refuses terminally for the same reason
//     (see ramOwnDemandRefusal), and so, for a wsl spec, does one that
//     exceeds the WSL VM's whole RAM ceiling (wslRAMOwnDemandRefusal).
//     Right after them, a wsl candidate that declares a GPU Waits, evicting
//     nothing, while another wsl start's VRAM window is open (wslStartWait):
//     its figure is the VM's growth across that start, so those starts go one
//     at a time.
//  2. Unknown-VRAM-vs-pinned short-circuit, for an unknown demand on
//     EITHER side of the pair. Neither eviction nor waiting can resolve
//     it, so it is checked, and returned, before any rule that proposes
//     either. The occupant half additionally reports a closed
//     co-residency cell as itself rather than as a missing VRAM number
//     (see pinnedUnknownOccupantRefusal).
//  3. Collect the matrix (rule 1) and unknown-VRAM (rules 4 and 5)
//     blockers against the self-filtered running set: every disallowed
//     pair; -- if spec has any unknown-VRAM GPU -- every process touching
//     any of spec's GPUs; and every process that has an unknown demand of
//     its OWN and holds any of spec's GPUs. For rules 1 and 5 an evictable
//     blocker is queued for eviction and a non-evictable (busy or
//     still-loading) one marks the whole decision blocked. RULE 4 NORMALLY
//     QUEUES NO VICTIM: known demand beats unknown demand, so a candidate
//     of unknown demand blocks on the occupants of its cards instead of
//     evicting them -- which is also why rules 4 and 5 no longer name the
//     same occupant when both sides are unknown. Rule 5 alone claims that
//     one. THE ONE EXCEPTION is the guarded bootstrap: when the owner has
//     granted the candidate its one-shot (snap.VRAMBootstrapEligible), rule
//     4 instead evicts idle known-demand touchers (blocking on non-evictable
//     ones) and flags Decision.VRAMBootstrap so the owner burns the shot --
//     fired at most once per spec, so it cannot thrash. See rule 4 in the
//     body for the convergence defect this closes, the bootstrap that
//     threads through it, and Decision's Wait shape for what the block costs.
//  4. Per-GPU budget (rule 3), evaluated with step 3's already-queued
//     evictions notionally already gone: for every GPU spec has a KNOWN
//     demand for, sum spec's own demand plus every remaining toucher's
//     demand, plus the WSL VM's unattributed VRAM on that GPU
//     (PolicySnapshot.UnattributedVRAM), which is occupied but no victim.
//     A GPU absent from Budgets, and one whose budget is 0, are
//     both unconstrained and are skipped identically (see
//     PolicySnapshot.Budgets). Overflow evicts idle touchers of THAT gpu
//     only, oldest LastUsed first, until it fits or idle candidates run
//     out. Spec's own demand is already known to fit each budget by
//     itself: step 1 refused it otherwise.
//  5. RAM pools (rule 6). For a wsl candidate only, the WSL VM's own pool
//     comes first (PolicySnapshot.WSLRAMBudgetMB): only wsl processes are
//     summed and drawn, narrow before broad as step 4 runs before this one, so
//     a wsl victim it needs anyway is already gone for the host pool and is
//     never joined by a native one the VM did not need (see ruleWSLRAM).
//     Then the host-RAM pool, the host-scoped twin of step 4, evaluated with
//     steps 3-5's queued evictions notionally already gone: the candidate's RAM
//     charge plus every remaining process's RAM, against the one
//     PolicySnapshot.HostRAMBudgetMB. A budget <= 0 (no sample yet) skips it. The
//     candidate's charge is its KNOWN demand, or -- when that demand is unknown
//     (RAMMB 0) -- the conservative cold-start guess RAMBootstrapGuessMB in its
//     place (task 4.2; 0 guess = off, skip). While another spec's RAM is
//     mid-bootstrap (RAMBootstrapInFlight, task 4.1) EVERY RAM admission Waits
//     here instead, evicting nothing. A known demand that exceeds the whole budget
//     by itself was already refused in step 1; the guess never refuses. Overflow
//     evicts idle processes HOST-WIDE -- any of them, not one GPU's occupants --
//     oldest LastUsed first, until it fits; if the idle ones cannot make it fit,
//     step 7 answers Wait. A still-loading process is never a victim, so it
//     reserves its RAM.
//  6. Process-count limit (rule 2), evaluated last so it only asks for as
//     many ADDITIONAL evictions as steps 3-5 have not already supplied.
//  7. Build the Decision (decide): any non-evictable blocker anywhere makes the
//     answer Wait -- never a partial Evict list, since evicting only some
//     of the blockers would not actually unblock a retry. Otherwise an
//     empty evict set means OK; a non-empty one means Evict-then-retry,
//     ordered oldest LastUsed first.
//  8. NoEvict gate (snap.NoEvict -- a burst instance, #340). That admission
//     is all-or-nothing and immediate, so anything but a plain OK -- an
//     Evict, any Wait, a VRAM bootstrap, or a terminal from step 1 or 2 --
//     becomes Decision.NoCapacity, with a Message naming what steps 0-7
//     answered (noEvictGate). It only rewrites the finished answer, so no
//     rule's verdict depends on it, and without NoEvict it is a no-op.
func Admit(snap PolicySnapshot, spec Spec) Decision {
	dec := admitSteps(snap, spec)
	if snap.NoEvict {
		return noEvictGate(spec, dec)
	}
	return dec
}

// noEvictGate is Admit's step 8: under PolicySnapshot.NoEvict, an OK passes
// and every other answer becomes Decision.NoCapacity. Terminals are mapped too
// (the own-demand refusals of step 1 included): a burst instance never parks in
// a durable state, and another host may well have the room this one lacks, so
// the one useful answer is "not here, now". The Message keeps the cause an
// operator needs -- the victims, the wait, or the terminal's own explanation.
// Evict is set to an empty slice, never nil, as on every other shape.
func noEvictGate(spec Spec, dec Decision) Decision {
	if dec.OK {
		return dec
	}
	return Decision{
		NoCapacity: true,
		Message: fmt.Sprintf(
			"spec %s: no free capacity to start without evicting or waiting: %s",
			spec.ID, noCapacityCause(dec),
		),
		Evict: []string{},
	}
}

// noCapacityCause names what a non-OK Decision would have done, for
// noEvictGate's Message.
func noCapacityCause(dec Decision) string {
	switch {
	case dec.Reason != "" && dec.Message != "":
		return fmt.Sprintf("%s (%s)", dec.Reason, dec.Message)
	case dec.Reason != "":
		return string(dec.Reason)
	case len(dec.Evict) > 0:
		return "it would evict " + strings.Join(dec.Evict, ", ")
	case dec.Message != "":
		return "it would wait: " + dec.Message
	case dec.WaitOnWSLStart:
		return "it would wait for another WSL start to settle"
	case dec.WaitOnWSLVM:
		// Checked before WaitOnRAM, which is always set with it: the pool the
		// candidate waits on is the WSL VM's, not the host's.
		return "it would wait for RAM in the WSL VM RAM pool"
	case dec.WaitOnRAM:
		return "it would wait for host RAM"
	case dec.WaitOnGrace:
		return "it would wait for an occupant's eviction grace to expire"
	default:
		return "it would wait for an occupant that cannot be evicted now"
	}
}

// admitSteps runs Admit's steps 0-7 -- everything but the NoEvict gate -- in
// the order Admit documents.
func admitSteps(snap PolicySnapshot, spec Spec) Decision {
	a := newAdmission(snap, spec)

	// Step 1. Spec's own declared demand against its own budgets, which is
	// the one verdict in this function that nothing in the running set can
	// influence -- and therefore the one that must be reached before any
	// rule that inspects it. See ownDemandRefusal for the misreport that
	// came of evaluating it last.
	if dec, refuse := ownDemandRefusal(snap, spec); refuse {
		return dec
	}
	// Its host-wide twin, for the same reason and in the same place: a known
	// RAM demand above the whole host-RAM budget cannot be fixed by anything in
	// the running set. Behind the per-GPU one so that a spec over BOTH budgets
	// keeps reporting the VRAM cause it always has.
	if dec, refuse := ramOwnDemandRefusal(snap, spec); refuse {
		return dec
	}
	// And its WSL VM twin (#381), for the same reason and in the same place:
	// it reads only spec and the VM pool's two figures. Behind the host one,
	// so that a wsl spec over both pools reports the host cause, the one an
	// operator of a native spec of the same size would also see.
	if dec, refuse := wslRAMOwnDemandRefusal(snap, spec); refuse {
		return dec
	}
	// The WSL start interlock (#381): a wsl candidate waits while another wsl
	// start's VRAM window is open. After the refusals, so a durable verdict is
	// not hidden behind a transient wait; before everything that could evict,
	// so nothing is drained for a start that cannot happen yet.
	if dec, wait := wslStartWait(snap, spec); wait {
		return dec
	}

	// Step 2. A pinned process is never an eviction victim and never
	// finishes, so an unknown demand contested with one resolves by neither
	// eviction nor waiting: returned ahead of every rule that proposes
	// either, so nothing below can offer a victim or a Wait that could not
	// possibly help. Both directions land here because both are the same
	// contention seen from opposite ends -- one card, two processes, and no
	// number for at least one of them.
	if a.unknownVRAM {
		if dec, refuse := pinnedUnknownCandidateRefusal(a.running, a.gpuIdx); refuse {
			return dec
		}
	}
	// Rule 5, occupant side: a pinned process with an unknown demand of its
	// own holds one of spec's GPUs, so that card cannot be shared with it
	// either -- whatever spec declares for itself. This half DOES name the
	// occupant it refuses on, so it picks one deterministically and reports
	// a closed co-residency pair as the closed pair it is.
	if dec, refuse := pinnedUnknownOccupantRefusal(snap, spec, a.running, a.gpuIdx); refuse {
		return dec
	}

	// Steps 3-6: the accumulating rule pipeline. The order below and the
	// toEvict chaining are load-bearing (see the admission type): each rule
	// sees the victims the earlier ones queued as already gone.
	a.ruleMatrix()               // rule 1: matrix compatibility
	a.ruleUnknownVRAMCandidate() // rule 4: candidate's own unknown VRAM
	a.ruleUnknownVRAMOccupant()  // rule 5: an occupant's unknown VRAM
	a.rulePerGPUVRAM()           // rule 3: per-GPU VRAM arithmetic
	a.ruleWSLRAM()               // rule 6 against the WSL VM's pool, wsl candidates only
	a.ruleHostRAM()              // rule 6: host-wide RAM pool
	a.ruleProcessCount()         // rule 2: process-count limit, last

	return a.decide()
}

// wslStartWait is Admit's WSL start interlock: a wsl candidate that declares a
// GPU Waits, evicting nothing, while another wsl start's VRAM window is open
// (PolicySnapshot.WSLStartInFlight), or ok == false when it does not apply. A
// wsl candidate with no GPU is taken to hold no VRAM: it opens no window and
// grows none, so it is not held back.
//
// WHY. A wsl workload's VRAM is measured as the WSL VM's growth across its
// start: Windows charges all of the VM's GPU memory to the VM's worker, and
// nothing inside the VM reports it per process. A second start growing the VM
// inside the first one's window would be added to the first one's figure, and
// its own window would catch the rest of the first one's load. Starts take
// minutes anyway, so they go one at a time. It is a Wait with no eviction,
// because evicting for a start that cannot proceed until the window closes
// would drain a workload for nothing; the window's close re-asks it
// (Decision.WaitOnWSLStart). Only starts are serialized: a stop never waits for
// a window, and the window corrects for it instead.
func wslStartWait(snap PolicySnapshot, spec Spec) (Decision, bool) {
	if !spec.IsWSL() || !wslDeclaresGPU(spec) || !snap.WSLStartInFlight {
		return Decision{}, false
	}
	return Decision{Wait: true, WaitOnWSLStart: true, Evict: []string{}}, true
}

// pinnedUnknownCandidateRefusal is step 2's candidate half -- rule 4 against a
// PINNED occupant. The candidate's own demand is unknown (its caller gates on
// that), so it may only run alone on its GPUs, and a pinned process holds one of
// them. Every qualifying occupant yields the identical Decision, so this half
// needs no tie-break to stay deterministic over a map-ordered Running.
func pinnedUnknownCandidateRefusal(running []RunningProc, gpuIdx []int) (Decision, bool) {
	for _, r := range running {
		if r.Pinned && touchesAnyGPU(r, gpuIdx) {
			return Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}}, true
		}
	}
	return Decision{}, false
}

// ruleMatrix is rule 1: matrix compatibility. Every running process whose pair
// with spec is not allowed must go -- queued if evictable, otherwise it blocks
// the whole decision. Siblings are always allowed and every other pair is read
// by base spec (pairAllowed), so an instance carries its spec's verdicts.
func (a *admission) ruleMatrix() {
	for _, r := range a.running {
		if pairAllowed(a.snap.Allowed, a.base, a.spec.IsRPCServer(), r) {
			continue
		}
		if a.evictable(r) {
			a.toEvict[r.SpecID] = r
		} else {
			a.blocked = true
		}
	}
}

// ruleUnknownVRAMCandidate is rule 4: the candidate's own VRAM demand is
// unknown, so it may only run alone on its GPUs. It has two arms. The steady
// state (blockOnKnownTouchers) only BLOCKS -- known demand beats unknown demand
// (ADR-032), so an unknown-demand candidate does not drain-stop a correctly
// configured occupant. The ONE exception is the guarded one-shot bootstrap
// (bootstrapEvictTouchers), granted by snap.VRAMBootstrapEligible, which evicts
// idle known-demand touchers so a never-measured spec can run alone once and be
// measured. A toucher of UNKNOWN demand is rule 5's subject in both arms and is
// left to it.
func (a *admission) ruleUnknownVRAMCandidate() {
	if !a.unknownVRAM {
		return
	}
	if a.snap.VRAMBootstrapEligible {
		a.bootstrapEvictTouchers()
		return
	}
	a.blockOnKnownTouchers()
}

// bootstrapEvictTouchers is rule 4's guarded one-shot arm (task 4.3), the sole,
// deliberate exception to the block below, and it is why rule 4 has two arms.
// ADR-032's price is a Wait that need never resolve, and for a spec whose
// operator left its estimate at 0 on a host where no known-demand occupant ever
// becomes idle, that Wait is permanent: the spec can never run alone, so it can
// never be measured, so it stays unknown forever. The owner breaks that deadlock
// by granting the spec exactly ONE eviction -- the first time it is admitted
// while still never-measured and still holding its shot
// (snap.VRAMBootstrapEligible). Under the grant, rule 4 treats a known-demand
// toucher exactly as rule 5 treats an unknown one: evict it if idle, block on it
// otherwise, so the candidate gets its card(s) to itself and is measured.
//
// The aloneness demand is on ALL of its cards (specGPUIndexes), so every
// known-demand toucher must go -- idle ones are queued, a non-evictable one
// (busy or still loading) blocks the whole start rather than yielding a partial,
// unmeasured co-residency (the same "any non-evictable blocker -> Wait"
// invariant rules 1 and 5 keep). A toucher of UNKNOWN demand is still rule 5's
// subject and is left to it. The grant is withdrawn the instant the shot is
// spent or a measurement lands (owner.vramBootstrapEligible /
// specState.vramBootstrapDone), so the exception fires AT MOST ONCE per spec and
// cannot reintroduce the bidirectional thrash: a host alternating requests
// between a mixed pair cold-loads at most one of them, once, not forever. The
// Wait this can produce carries no message: it is an ordinary wait a busy
// neighbour's completion resolves, not the ADR-032 precedence wait that need
// never resolve.
func (a *admission) bootstrapEvictTouchers() {
	for _, r := range a.running {
		if !touchesAnyGPU(r, a.gpuIdx) || procHasUnknownVRAM(r) {
			continue
		}
		if a.evictable(r) {
			a.toEvict[r.SpecID] = r
			a.bootstrapEvicted = true
		} else {
			a.blocked = true
		}
	}
}

// blockOnKnownTouchers is rule 4's steady-state arm: every remaining toucher of
// spec's GPUs. Any pinned toucher already short-circuited in step 2, so only
// idle, busy and still-loading ones remain. None of them is evicted for this
// candidate: this arm only BLOCKS.
//
// KNOWN DEMAND BEATS UNKNOWN DEMAND (ADR-032). Rules 4 and 5 were symmetric in
// outcome as well as in subject, and that was a convergence defect rather than a
// nicety: rule 5 evicts an unknown-demand occupant for a known-demand candidate,
// while rule 4 evicted a known-demand occupant for an unknown-demand candidate
// -- so a mixed pair contesting one card evicted in BOTH directions. Measured on
// one card, both idle, the pair open and no budget at all: candidate a (unknown
// demand) answered Evict [b] and candidate b (known demand) answered Evict [a],
// so a host alternating requests between the two paid a cold load for every
// single request, forever, with no state in between that either request could be
// served from.
//
// The operator's resolution is a TOTAL ORDER, and this arm is the half that
// gives something up: a candidate whose own demand is unknown still insists on
// having its cards to itself -- that is rule 4's entire point, §5.3's "may start
// only alone on that GPU" -- but it may no longer drain-stop a
// correctly-configured occupant to get there. It loses only the privilege of
// killing a spec whose estimate someone filled in; it keeps the refusal to
// share. So the answer is Wait -- which the occupant's own idle timeout resolves
// while destroying nothing, WHEN IT HAS ONE: Spec.IdleTimeoutSeconds == 0 means
// never unload, and then the wait lasts until a measurement or an operator
// estimate arrives (§5.3 states that price). For an occupant that can never
// leave the answer is the terminal step 2 already returned. A total order
// converges, and the spec that loses is always the misconfigured one.
//
// THE BLOCK IS UNCONDITIONAL, not "unless some other rule would have evicted
// that occupant anyway". A closed matrix cell (rule 1) or an overflowing sibling
// budget (rule 3) does supply an independent reason to drain-stop the same
// occupant, and honouring the order only where no such reason exists would leave
// those pairs evicting in both directions -- the same defect, one rule over. The
// order is a property of the two SPECS, not of this loop.
//
// A TOUCHER OF UNKNOWN DEMAND IS RULE 5's SUBJECT AND IS LEFT TO IT.
// ruleUnknownVRAMOccupant scans exactly procHasUnknownVRAM(r) &&
// touchesAnyGPU(r, gpuIdx) over this same running set, ungated by the
// candidate's own demand, so every occupant this loop would still have evicted
// after the change -- an equally-unknown one, the tie the order does not decide
// -- gets the identical evict-if-idle / block-otherwise treatment there.
// Repeating it here would be a branch whose deletion changes no decision. That
// tie keeps its pre-existing mutual eviction and is a recorded acceptance
// (11-risks-and-technical-debt.md §11.4) pinned by
// TestAdmitUnknownTieStillEvictsBothWays -- which is also the test that fails if
// rule 5's scan is ever narrowed out from under this comment.
//
// IT IS THE ONE WAIT THAT REPORTS ITSELF. Every other Wait in this function
// resolves without anyone doing anything -- a busy neighbour finishes, a queued
// victim drains -- but this one need not resolve at all: the occupant is spared
// here precisely because it is not being evicted, and owner.scanIdle skips a
// spec whose IdleTimeoutSeconds is 0, which is the documented "never unload". An
// idle, unpinned, never-again-requested occupant therefore keeps the card
// indefinitely while this candidate requeues to its admission timeout on every
// request. So this branch fills in waitMessage (Decision.Message), which the
// manager records as the candidate's last_error (the portal renders it in an
// always-visible column); the ordinary Waits deliberately keep saying nothing,
// since an error on every contested start is how a diagnostic stops being read.
//
// The PINNED variant of this same block, returned by step 2, needs no message
// and still has none: it reports the terminal StatePendingVRAMUnknown, and that
// state on this spec's own row already names the missing number as this spec's
// (§5.3). A message is added here precisely because a Wait has no state to speak
// for it.
func (a *admission) blockOnKnownTouchers() {
	var (
		precedenceBlocker string
		precedenceGPU     int
		foundBlocker      bool
	)
	for _, r := range a.running {
		gpu, shares := firstSharedGPU(r, a.gpuIdx)
		if !shares || procHasUnknownVRAM(r) {
			continue
		}
		a.blocked = true
		// Lowest spec id wins, as in pinnedUnknownOccupantRefusal and
		// sortOldestFirst: PolicySnapshot.Running arrives in map order
		// (owner.buildSnapshot ranges over o.specs), so "the first one seen"
		// would name a different spec on every admission against an unchanged
		// host -- in an operator-facing message.
		if !foundBlocker || r.SpecID < precedenceBlocker {
			precedenceBlocker, precedenceGPU, foundBlocker = r.SpecID, gpu, true
		}
	}
	if !foundBlocker {
		return
	}
	// Both cards are named because they need not be the same one: rule 4
	// demands aloneness on ALL of the candidate's cards, so the card being held
	// and the card whose number is missing can differ (see specGPUIndexes).
	// firstSpecUnknownGPU is the first in spec.GPUs order, which is the order
	// rule 3's messages already name cards in.
	a.waitMessage = fmt.Sprintf(
		"spec %s: its own demand on gpu %d is unknown, so it waits for spec %s to leave gpu %d rather than evicting it",
		a.spec.ID, firstSpecUnknownGPU(a.spec), precedenceBlocker, precedenceGPU,
	)
}

// firstSpecUnknownGPU returns the index of the first GPU in spec.GPUs order
// whose declared demand is unknown (VRAMMB == 0), or 0 when spec has none. It is
// the card blockOnKnownTouchers names as the candidate's own missing number, in
// the same order rule 3's messages name cards.
func firstSpecUnknownGPU(spec Spec) int {
	for _, g := range spec.GPUs {
		if g.VRAMMB == 0 {
			return g.Index
		}
	}
	return 0
}

// ruleUnknownVRAMOccupant is rule 5: rule 4's mirror in SUBJECT -- every
// remaining occupant that has an unknown demand of its OWN and holds one of
// spec's GPUs, making that card unshareable exactly as an unknown candidate
// does. It is the winning half of the order in blockOnKnownTouchers, so unlike
// rule 4 it still EVICTS: idle occupants are queued as victims, and it is the
// only rule that queues an occupant of unknown demand at all. Any pinned such
// occupant already short-circuited in step 2, so only idle/busy/starting remain
// here.
//
// It belongs in step 3, next to rules 1 and 4, for two reasons: it is evaluated
// against the ORIGINAL running set, so its verdict never depends on what another
// rule happened to queue first, and rule 3 below must already see its victims as
// gone -- an occupant queued here is skipped by rule 3's sum, which is the only
// consistent reading (charging it a number there cannot work; see
// procHasUnknownVRAM's own doc comment). It must not move after rule 2 either:
// the process-count limit runs last precisely so it asks only for victims the
// earlier rules have not already supplied.
func (a *admission) ruleUnknownVRAMOccupant() {
	for _, r := range a.running {
		if !procHasUnknownVRAM(r) || !touchesAnyGPU(r, a.gpuIdx) {
			continue
		}
		if a.evictable(r) {
			a.toEvict[r.SpecID] = r
		} else {
			a.blocked = true
		}
	}
}

// rulePerGPUVRAM is rule 3: per-GPU VRAM arithmetic, only over GPUs with a KNOWN
// demand and a REAL (positive) budget. Each GPU is charged independently
// (chargeGPUBudget), with step 3's already-queued evictions notionally gone.
func (a *admission) rulePerGPUVRAM() {
	for _, g := range a.spec.GPUs {
		a.chargeGPUBudget(g)
	}
}

// chargeGPUBudget charges one GPU's budget: spec's own demand plus every
// remaining toucher's, and if that overflows, it evicts idle touchers of THAT
// gpu only, oldest first, until it fits or the idle ones run out (then it
// blocks).
//
// Absent from Budgets and a budget of 0 both mean "no budget for this GPU" =
// unconstrained, and must behave identically -- see PolicySnapshot.Budgets, and
// routing.ServerGPUBudget.BudgetMB in the gateway
// (gateway/backend/internal/routing/store.go), which is where that meaning is
// defined. The check lives HERE, at the one place that interprets a budget,
// rather than in owner.buildSnapshot where the map happens to be built: Budgets
// is an exported field on an exported struct that any caller can populate, so
// filtering at a single producer would leave every other producer free to
// reintroduce the divergence.
//
// spec's own demand is already known to fit this budget by itself: step 1
// (ownDemandRefusal) returned the terminal refusal otherwise, before anything
// here or in step 2 ran.
//
// The WSL VM's unattributed VRAM on this GPU (PolicySnapshot.UnattributedVRAM)
// is part of the sum as well: memory the VM holds that no process's charge
// accounts for, so it is occupied whatever is evicted. It is never drawn as a
// victim, and with it the sum can stay over budget once every idle toucher is
// queued: the start Waits, and a fresh reading at the next admission sees
// whatever the VM has given back.
func (a *admission) chargeGPUBudget(g SpecGPU) {
	if g.VRAMMB == 0 {
		return
	}
	budget := a.snap.Budgets[g.Index]
	if budget <= 0 {
		return
	}
	sum := g.VRAMMB + a.snap.UnattributedVRAM[g.Index]
	var touchers []RunningProc
	for _, r := range a.running {
		if _, already := a.toEvict[r.SpecID]; already {
			continue // already guaranteed to be gone
		}
		v, ok := r.GPUs[g.Index]
		if !ok {
			continue
		}
		sum += v
		touchers = append(touchers, r)
	}
	if sum <= budget {
		return
	}

	idleTouchers := a.evictableOf(touchers)
	a.orderVictims(idleTouchers)
	for _, r := range idleTouchers {
		if sum <= budget {
			break
		}
		if !a.evictable(r) {
			continue // a higher sibling stays running (higherSiblingStays)
		}
		a.toEvict[r.SpecID] = r
		sum -= r.GPUs[g.Index]
	}
	if sum > budget {
		a.blocked = true
	}
}

// ruleHostRAM is rule 6: the host-wide RAM pool. RAM is a host resource, so
// unlike rule 3 there is ONE sum, over every running process, and victims are
// drawn from ALL of them rather than from one GPU's occupants.
//
// It runs after rules 1, 3, 4 and 5 for the reason rule 3 runs after 1, 4 and 5:
// a process one of them already queued is guaranteed to be gone, so its RAM is
// skipped here rather than charged twice, and it is not drawn again -- a VRAM
// victim that also holds the RAM we need must not be joined by a second,
// innocent one. For the same reason it runs after the WSL VM pool
// (ruleWSLRAM), the narrower of the two RAM pools. It runs before rule 2 for
// the reason rules 1-5 do: the process-count limit asks only for the victims
// still missing.
//
// Both ends of the pool follow the 0 sentinels used everywhere else:
//
//   - HostRAMBudgetMB <= 0 is unconstrained (no hardware sample yet), skipped
//     outright, and the check lives HERE rather than in buildSnapshot for the
//     reason given on PolicySnapshot.Budgets.
//   - spec.RAMMB == 0 is an UNKNOWN demand, never "needs none". Sending an
//     unknown demand to a first measurement is the cold-start bootstrap's job
//     (task 4.2): the candidate is charged snap.RAMBootstrapGuessMB in its
//     place -- a conservative stand-in weighed against the pool like a known
//     demand, so a first start fits beside room, pre-evicts idle oldest-first
//     when tight, or Waits rather than blind-starting. A guess of 0 (the knob
//     off) charges nothing and the spec starts unconstrained, as before the
//     bootstrap existed. An OCCUPANT's unknown RAMMB is the same 0 and still
//     contributes nothing; it is not a RAM victim either, since draining it
//     frees nothing this arithmetic can count (the guess is the CANDIDATE's
//     alone, never charged to a running process).
//
// A process that is still loading is in running with its RAMMB and is never
// evictable (isEvictable), so it reserves what it occupies: a second admission
// sees the first one's headroom already spoken for and cannot double-commit it.
// That is the interlock for a KNOWN demand. The interlock for an UNKNOWN one is
// snap.RAMBootstrapInFlight, checked first below: a bootstrapping process
// reserves nothing in the arithmetic (its RAMMB is the unknown 0), so while one
// is mid-window EVERY other RAM admission Waits rather than racing it for RAM
// whose true owner has not finished claiming it.
//
// The candidate's own demand against the whole pool is NOT decided here: it is
// rule 6's own-demand half, hoisted to step 1 like rule 3's (see
// ramOwnDemandRefusal), so by this point spec.RAMMB fits the budget alone and
// every Wait below is behind holders that cannot be evicted right now: a busy or
// still-loading one resolves by itself when it finishes, a pinned one is the
// operator's own choice.
func (a *admission) ruleHostRAM() {
	if a.snap.HostRAMBudgetMB <= 0 {
		return
	}
	if a.snap.RAMBootstrapInFlight {
		// The cold-start interlock (task 4.1). Another spec's RAM is mid-
		// bootstrap and unaccounted, so no new RAM admission -- known demand or
		// a second bootstrap alike -- may proceed until it settles or leaves.
		// It is a Wait that evicts nothing (blocked always returns an empty
		// Evict list), and WaitOnRAM so the next hardware sample re-asks it.
		a.blocked = true
		a.ramBlocked = true
		return
	}
	// The candidate's own charge. A KNOWN demand is itself; an UNKNOWN one
	// (spec.RAMMB == 0, specHasUnknownRAM) is charged the conservative
	// cold-start guess instead of nothing (task 4.2), so its first start is
	// weighed against the pool like any other -- fitting with no eviction when
	// there is headroom, pre-evicting idle oldest-first only when the host is
	// genuinely too tight, and Waiting (below) rather than blind-starting when
	// nothing idle can make room. A guess of 0 (the knob off) leaves
	// candidateRAM 0, so this whole rule is skipped and the spec starts
	// unconstrained, exactly as before the bootstrap existed.
	candidateRAM := a.spec.RAMMB
	if specHasUnknownRAM(a.spec) {
		candidateRAM = a.snap.RAMBootstrapGuessMB
	}
	if candidateRAM <= 0 {
		return
	}
	sum := candidateRAM
	var holders []RunningProc
	for _, r := range a.running {
		if _, already := a.toEvict[r.SpecID]; already {
			continue // already guaranteed to be gone
		}
		if r.RAMMB <= 0 {
			continue // unknown occupancy: nothing to charge, nothing to free
		}
		sum += r.RAMMB
		holders = append(holders, r)
	}
	if sum <= a.snap.HostRAMBudgetMB {
		return
	}

	idleHolders := a.evictableOf(holders)
	a.orderVictims(idleHolders)
	for _, r := range idleHolders {
		if sum <= a.snap.HostRAMBudgetMB {
			break
		}
		if !a.evictable(r) {
			continue // a higher sibling stays running (higherSiblingStays)
		}
		a.toEvict[r.SpecID] = r
		sum -= r.RAMMB
	}
	if sum > a.snap.HostRAMBudgetMB {
		a.blocked = true
		a.ramBlocked = true
	}
}

// ruleWSLRAM is rule 6 a second time, against the WSL VM's own RAM pool
// (#381), for a wsl candidate only. The VM has a ceiling of its own, inside the
// host's, so a wsl start must fit both: the host pool (rule 6, unchanged --
// the VM's memory is host memory too, so every wsl process is already in that
// sum) and this one, which sums only the wsl processes (RunningProc.WSL) and
// draws its victims only from them. A native process holds nothing in the VM,
// so draining one frees nothing this arithmetic can count; a native candidate
// takes nothing from the VM, so the rule does not run for it.
//
// Everything else is rule 6's discipline, unchanged, so the two pools cannot
// disagree about how a pool is read: a budget <= 0 is unconstrained (no
// sampling call has answered yet); while a RAM bootstrap is in flight the
// candidate Waits, evicting nothing (an unaccounted bootstrap may be a wsl one,
// taking VM memory nobody has measured); an unknown-RAM candidate is charged
// the bootstrap guess; an occupant of unknown RAM is neither charged nor
// drawn; victims are the idle ones, in orderVictims' order, asked evictable as
// each is drawn; and what idle victims cannot free makes it a WaitOnRAM, which
// the next VM sample re-asks, and a WaitOnWSLVM, which asks for that sample
// when no wsl process is live to bring one (owner.wslVMWaitDistros). The
// candidate's own demand against the whole VM was decided in step 1
// (wslRAMOwnDemandRefusal).
//
// WHERE IT RUNS. BEFORE the host pool (rule 6), narrow before broad, for the
// reason rule 3 runs before rule 6. Only a wsl process can free VM RAM, so the
// wsl victims this pool needs are needed whatever the host pool decides, and
// each also frees its RAM on the host. Drawn first, they are already gone when
// the host pool sums and draws, which then asks only for what is still
// missing. The other order over-evicts: with a host pool of 12000 MB, a VM
// pool of 10000 MB, an older idle native process and an idle wsl process of
// 6000 MB each, and a wsl candidate of 5000 MB, the host pool would draw the
// older native process first (17000 -> 11000 fits the host), and this pool
// would then still need the wsl one -- two evictions where the wsl one alone
// fits both pools. After rules 1, 3, 4 and 5 for the reason rule 6 is: a process they
// queued is gone, never charged twice or drawn again. Before rule 2, for the
// reason every resource rule runs before it: the process-count limit asks only
// for the evictions the resources have not already supplied.
func (a *admission) ruleWSLRAM() {
	if !a.spec.IsWSL() || a.snap.WSLRAMBudgetMB <= 0 {
		return
	}
	if a.snap.RAMBootstrapInFlight {
		a.blocked = true
		a.ramBlocked = true
		return
	}
	candidateRAM := a.spec.RAMMB
	if specHasUnknownRAM(a.spec) {
		candidateRAM = a.snap.RAMBootstrapGuessMB
	}
	if candidateRAM <= 0 {
		return
	}
	sum := candidateRAM
	var holders []RunningProc
	for _, r := range a.running {
		if !r.WSL {
			continue // holds nothing in the VM
		}
		if _, already := a.toEvict[r.SpecID]; already {
			continue // already guaranteed to be gone
		}
		if r.RAMMB <= 0 {
			continue // unknown occupancy: nothing to charge, nothing to free
		}
		sum += r.RAMMB
		holders = append(holders, r)
	}
	if sum <= a.snap.WSLRAMBudgetMB {
		return
	}

	idleHolders := a.evictableOf(holders)
	a.orderVictims(idleHolders)
	for _, r := range idleHolders {
		if sum <= a.snap.WSLRAMBudgetMB {
			break
		}
		if !a.evictable(r) {
			continue // a higher sibling stays running (higherSiblingStays)
		}
		a.toEvict[r.SpecID] = r
		sum -= r.RAMMB
	}
	if sum > a.snap.WSLRAMBudgetMB {
		a.blocked = true
		a.ramBlocked = true
		a.vmBlocked = true
	}
}

// ruleProcessCount is rule 2: the process-count limit, evaluated last so it only
// asks for as many ADDITIONAL evictions as rules 1, 3, 4, 5 and 6 (both of its
// pools) have not already provided.
func (a *admission) ruleProcessCount() {
	if a.snap.MaxProcesses <= 0 {
		return
	}
	remaining := len(a.running) - len(a.toEvict)
	deficit := remaining + 1 - a.snap.MaxProcesses
	if deficit <= 0 {
		return
	}
	var candidates []RunningProc
	for _, r := range a.running {
		if _, already := a.toEvict[r.SpecID]; already {
			continue
		}
		if a.mayEvict(r) {
			candidates = append(candidates, r)
		}
	}
	a.orderVictims(candidates)
	drawn := 0
	for _, r := range candidates {
		if drawn == deficit {
			break
		}
		if !a.evictable(r) {
			continue // a higher sibling stays running (higherSiblingStays)
		}
		a.toEvict[r.SpecID] = r
		drawn++
	}
	if drawn < deficit {
		a.blocked = true
	}
}

// decide builds the final Decision from the accumulated state (step 7). Any
// non-evictable blocker anywhere makes the answer Wait with an empty Evict list;
// otherwise an empty toEvict means OK and a non-empty one means
// Evict-then-retry, ordered oldest LastUsed first -- except that the instances
// of one spec take that spec's places highest index first.
func (a *admission) decide() Decision {
	if a.blocked {
		return Decision{
			Wait:        true,
			WaitOnRAM:   a.ramBlocked,
			WaitOnWSLVM: a.vmBlocked,
			WaitOnGrace: a.blockedSolelyByGrace(),
			Message:     a.waitMessage,
			Evict:       []string{},
		}
	}
	if len(a.toEvict) == 0 {
		return Decision{OK: true, Evict: []string{}}
	}

	victims := make([]RunningProc, 0, len(a.toEvict))
	for _, r := range a.toEvict {
		victims = append(victims, r)
	}
	sortOldestFirst(victims)
	// The drain order keeps "highest index leaves first" too: the owner begins
	// the drains in list order, so S#2 is asked to leave before S#1.
	siblingsHighestFirst(victims)
	evict := make([]string, len(victims))
	for i, r := range victims {
		evict[i] = r.SpecID
	}
	// VRAMBootstrap is surfaced only here, on the Evict decision: if a bootstrap
	// toucher was queued but the start ended up blocked (a non-evictable toucher
	// elsewhere), the function returned the Wait above with an empty Evict list
	// and no shot was spent. The owner burns the one-shot only for this return.
	return Decision{Evict: evict, VRAMBootstrap: a.bootstrapEvicted}
}

// blockedSolelyByGrace reports whether this blocked admission would NOT have
// blocked -- an OK, or an Evict -- had nothing been grace-protected, i.e. the
// ONLY thing keeping it from proceeding is one or more within-grace victims. It
// is Decision.WaitOnGrace, and it exists because such a Wait is woken by nothing
// else (see that field): the grace window is the sole reason, and it resolves by
// itself when the window expires.
//
// It is computed by RE-RUNNING the whole admission against the same snapshot with
// the grace window turned off, and asking whether that counterfactual proceeds.
// The grace window only ever removes a process from the victim set (isEvictable);
// it never changes the resource arithmetic, the matrix, the process limit or any
// terminal (those read no clock), so a re-run with grace off differs from this
// one in exactly one way -- which within-grace models may now be drawn -- and its
// verdict is precisely "would this have gone through if grace were not
// protecting anyone". A hard blocker (busy, still-loading, pinned, matrix-
// incompatible, or simply not enough idle room even with grace off) still blocks
// the counterfactual, so WaitOnGrace stays false and no grace wake is scheduled
// for a Wait a grace expiry could not unblock.
//
// The re-run is cheap (Admit is pure -- no clock, store or measurement) and
// bounded: the counterfactual snapshot has grace 0, so the EvictionGrace > 0
// guard below stops it recursing a second time. It runs only on the already-rare
// blocked path, and only when a grace window is actually configured.
//
// The counterfactual runs steps 0-7 only (admitSteps), never the NoEvict gate:
// that gate would rewrite every non-OK answer into a NoCapacity that reads as
// "proceeds". Under NoEvict the flag never reaches the caller as a wake (step 8
// replaces the Wait), but it still names the cause in NoCapacity's Message.
func (a *admission) blockedSolelyByGrace() bool {
	if a.snap.EvictionGrace <= 0 {
		return false
	}
	noGrace := a.snap
	noGrace.EvictionGrace = 0
	alt := admitSteps(noGrace, a.spec)
	// Proceeds == not a Wait and not a terminal: an OK or an Evict. A terminal
	// cannot actually arise here (no terminal fired on the grace-on run, and
	// terminals are grace-independent), but requiring Reason == "" states the
	// intent exactly and is robust to future terminals.
	return !alt.Wait && alt.Reason == ""
}
