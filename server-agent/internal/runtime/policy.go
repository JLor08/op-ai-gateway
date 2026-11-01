// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"sort"
	"time"
)

// RunningProc is one currently-running managed process, as the admission
// policy needs to see it. GPUs holds the EFFECTIVE VRAM the process is
// occupying per GPU index -- the measured value when the agent has one,
// else the spec's estimate. Admit trusts this number exactly as given; it
// never re-derives or looks up an estimate elsewhere.
type RunningProc struct {
	SpecID string
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
	LastUsed time.Time // idle-victim ordering: oldest evicted first
	// Starting is true while this process is still loading -- it has been
	// exec'd but has never passed a health probe, so it is serving nothing
	// and its InFlight is necessarily 0. Such a process is NEVER an eviction
	// victim (isEvictable), which is the C3 fix. See that function.
	Starting bool
}

// PolicySnapshot is the pure input to Admit: the currently running set plus
// the configured constraints, all as of one instant. No clock, no store, no
// live process handles -- a caller assembles a fresh snapshot for every
// admission decision.
type PolicySnapshot struct {
	Running      []RunningProc
	MaxProcesses int // 0 = unlimited
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
	// Allowed is the canonical (a<=b) spec-ID pair set: true = the pair
	// may run together. Build it with Config.AllowedPairs(), never by
	// hand-rolling a map from Config.Coresident's raw wire-order pairs --
	// AllowedPairs canonicalizes each pair via PairKey so a lookup never
	// depends on which side is the candidate and which is the
	// already-running process.
	Allowed map[[2]string]bool
}

// Decision is Admit's answer. Exactly one of four shapes holds:
//   - OK: spec may start now. Evict is empty, Wait is false, Reason is "".
//   - Evict-then-retry: OK is false, Evict names the idle victims to
//     drain-stop first, oldest LastUsed first. Once they are gone, Admit
//     applied again to the reduced running set is expected to return OK.
//   - Wait: OK is false, Wait is true, Evict is empty. Nothing MAY be evicted
//     to help right now, so the caller queues the request and re-asks on the
//     next completion. Two different situations reach that, and only the
//     first is "nothing CAN be evicted": every remaining blocker is busy,
//     still loading, or (outside the unknown-VRAM case below) pinned. In the
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
//     spec queued behind a busy neighbour is ordinary operation.
//   - Reason: OK is false, Wait is false, Evict is empty, and Reason names
//     a durable, non-transient block -- one that cannot resolve by
//     eviction OR by waiting, so reporting it as either of the other two
//     outcomes would be actively misleading (Wait would hang the caller
//     until its admission-wait timeout on every single request; Evict
//     would ask for a drain-stop that can never restore a fit). Two
//     States share this shape, carrying four distinct causes between
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
//   - StateNotPermitted: a durable operator-configuration block. Three
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
//     None resolves by eviction or by waiting. All carry the same
//     "configuration error, visible in the portal, not transient"
//     meaning the design doc already assigns StateNotPermitted for a
//     refused binary/directory, so Message is the only thing separating
//     all four.
type Decision struct {
	OK     bool
	Reason State // "" unless a terminal case above applies
	// Message is the operator-facing explanation, set on exactly two shapes:
	// alongside a terminal Reason that needs disambiguating context, and
	// alongside the precedence-induced Wait (rule 4), which is the one Wait
	// that need never resolve by itself. "" otherwise -- and in particular on
	// every ordinary Wait.
	Message string
	Evict   []string // spec IDs to drain-stop first, oldest LastUsed first; never nil
	Wait    bool
	// WaitOnRAM is set only together with Wait, and only when rule 6 (the
	// host-RAM pool) was among what blocked: the idle processes could not free
	// enough. It tells the caller this particular Wait can end by itself the
	// moment the hardware sample refreshes -- the pool is built from the host's
	// live MemAvailable, and the sample that follows a victim's exit is the one
	// that finally shows its freed RAM -- unlike a Wait on the matrix, the
	// process limit or a VRAM budget, which no new sample can change. The
	// manager re-asks a WaitOnRAM spec when a sample lands, and nothing else
	// (see owner.wakeOnHostSample).
	WaitOnRAM bool
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
// never a pinned process, never one that is still loading, and only when it
// is serving no request right now.
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
func isEvictable(r RunningProc) bool {
	return !r.Pinned && !r.Starting && r.InFlight == 0
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
func pinnedUnknownOccupantRefusal(snap PolicySnapshot, spec Spec, running []RunningProc, gpuIdx []int) (Decision, bool) {
	var (
		blocker    RunningProc
		sharedGPU  int
		pairClosed bool
		found      bool
	)
	for _, r := range running {
		if !r.Pinned || !procHasUnknownVRAM(r) {
			continue
		}
		gpu, shares := firstSharedGPU(r, gpuIdx)
		if !shares {
			continue
		}
		closed := !snap.Allowed[PairKey(spec.ID, r.SpecID)]
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

// sortOldestFirst orders procs by LastUsed ascending: the eviction order
// Admit always uses (oldest idle victim first). Ties in LastUsed break on
// SpecID so the output is fully deterministic -- sort.Slice is not stable,
// and map iteration order (toEvict, in Admit) is randomized per run, so
// without a total order a tie would otherwise pick a different victim on
// every run.
func sortOldestFirst(procs []RunningProc) {
	sort.Slice(procs, func(i, j int) bool {
		if !procs[i].LastUsed.Equal(procs[j].LastUsed) {
			return procs[i].LastUsed.Before(procs[j].LastUsed)
		}
		return procs[i].SpecID < procs[j].SpecID
	})
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
// both directions and thrashing forever. Rule 4 in the body carries the
// measurement and the reasoning; the one case the order does not decide (both
// sides unknown) is a recorded acceptance in
// 11-risks-and-technical-debt.md §11.4.
//
// Kept as a small, deliberately linear pipeline rather than a clever one,
// since it will be read far more often than it will be changed:
//
//  0. Self-filter. A RunningProc sharing spec.ID -- e.g. a double-start
//     race, or a snapshot assembled a moment too early or late -- is never
//     a blocker or a victim for its own candidate: it is not a foreign
//     occupant competing with spec for anything (the gateway's coresident
//     matrix never contains a self-pair, so leaving it in would read as
//     matrix-incompatible with itself). Filtered out once, up front, so
//     none of the rules below can propose evicting the very process being
//     started or wait on it forever.
//  1. Rule 3's own-demand half, hoisted ahead of everything that reads
//     the running set: a declared demand that exceeds its own GPU's
//     budget by itself refuses terminally, because no running set, no
//     eviction and no wait can change that verdict. It reads only spec
//     and snap.Budgets, so running it first cannot disturb another rule's
//     inputs -- and running it LAST let the resolvable cause in step 2
//     shadow this permanent one (see ownDemandRefusal). Its host-wide
//     twin follows it: a KNOWN RAM demand that exceeds the whole
//     host-RAM budget by itself refuses terminally for the same reason
//     (see ramOwnDemandRefusal).
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
//     still-loading) one marks the whole decision blocked. RULE 4 NEVER
//     QUEUES A VICTIM: known demand beats unknown demand, so a candidate
//     of unknown demand blocks on the occupants of its cards instead of
//     evicting them -- which is also why rules 4 and 5 no longer name the
//     same occupant when both sides are unknown. Rule 5 alone claims that
//     one. See rule 4 in the body for the convergence defect this closes,
//     and Decision's Wait shape for what it costs the caller.
//  4. Per-GPU budget (rule 3), evaluated with step 3's already-queued
//     evictions notionally already gone: for every GPU spec has a KNOWN
//     demand for, sum spec's own demand plus every remaining toucher's
//     demand. A GPU absent from Budgets, and one whose budget is 0, are
//     both unconstrained and are skipped identically (see
//     PolicySnapshot.Budgets). Overflow evicts idle touchers of THAT gpu
//     only, oldest LastUsed first, until it fits or idle candidates run
//     out. Spec's own demand is already known to fit each budget by
//     itself: step 1 refused it otherwise.
//  5. Host-RAM pool (rule 6), the host-scoped twin of step 4, evaluated with
//     steps 3-4's queued evictions notionally already gone: the candidate's RAM
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
//  7. Build the Decision: any non-evictable blocker anywhere makes the
//     answer Wait -- never a partial Evict list, since evicting only some
//     of the blockers would not actually unblock a retry. Otherwise an
//     empty evict set means OK; a non-empty one means Evict-then-retry,
//     ordered oldest LastUsed first.
func Admit(snap PolicySnapshot, spec Spec) Decision {
	running := make([]RunningProc, 0, len(snap.Running))
	for _, r := range snap.Running {
		if r.SpecID != spec.ID {
			running = append(running, r)
		}
	}

	unknownVRAM := specHasUnknownVRAM(spec)
	gpuIdx := specGPUIndexes(spec)

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

	// Step 2. A pinned process is never an eviction victim and never
	// finishes, so an unknown demand contested with one resolves by neither
	// eviction nor waiting: returned ahead of every rule that proposes
	// either, so nothing below can offer a victim or a Wait that could not
	// possibly help. Both directions land here because both are the same
	// contention seen from opposite ends -- one card, two processes, and no
	// number for at least one of them.
	//
	// Rule 4, candidate side: spec's own demand is unknown, so it may only
	// run alone on its GPUs, and a pinned process holds one of them. Every
	// qualifying occupant yields the identical Decision, so this half needs
	// no tie-break to stay deterministic over a map-ordered Running.
	if unknownVRAM {
		for _, r := range running {
			if r.Pinned && touchesAnyGPU(r, gpuIdx) {
				return Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}}
			}
		}
	}
	// Rule 5, occupant side: a pinned process with an unknown demand of its
	// own holds one of spec's GPUs, so that card cannot be shared with it
	// either -- whatever spec declares for itself. This half DOES name the
	// occupant it refuses on, so it picks one deterministically and reports
	// a closed co-residency pair as the closed pair it is.
	if dec, refuse := pinnedUnknownOccupantRefusal(snap, spec, running, gpuIdx); refuse {
		return dec
	}

	toEvict := make(map[string]RunningProc)
	blocked := false
	// ramBlocked records that rule 6 was among the causes of `blocked`, which
	// is what Decision.WaitOnRAM reports. It is deliberately separate from
	// blocked: a start waiting on a busy matrix neighbour must not be woken by
	// every hardware sample, and only the RAM verdict depends on one.
	ramBlocked := false
	// waitMessage is set by rule 4 alone, and only ever reaches a Wait: rule 4
	// setting it also sets blocked, and blocked always answers Wait. See rule
	// 4 for why that one Wait is the only one that explains itself.
	waitMessage := ""

	// Rule 1: matrix compatibility.
	for _, r := range running {
		if snap.Allowed[PairKey(spec.ID, r.SpecID)] {
			continue
		}
		if isEvictable(r) {
			toEvict[r.SpecID] = r
		} else {
			blocked = true
		}
	}

	// Rule 4: unknown VRAM -- every remaining toucher of spec's GPUs. Any
	// pinned toucher already short-circuited above, so only idle, busy and
	// still-loading ones remain here, and NONE of them is evicted for this
	// candidate: rule 4 only ever BLOCKS now.
	//
	// KNOWN DEMAND BEATS UNKNOWN DEMAND (ADR-032). Rules 4 and 5 were symmetric
	// in outcome as well as in subject, and that was a convergence defect
	// rather than a nicety: rule 5 evicts an unknown-demand occupant for a
	// known-demand candidate, while rule 4 evicted a known-demand occupant
	// for an unknown-demand candidate -- so a mixed pair contesting one card
	// evicted in BOTH directions. Measured on one card, both idle, the pair
	// open and no budget at all: candidate a (unknown demand) answered
	// Evict [b] and candidate b (known demand) answered Evict [a], so a host
	// alternating requests between the two paid a cold load for every single
	// request, forever, with no state in between that either request could be
	// served from.
	//
	// The operator's resolution is a TOTAL ORDER, and rule 4 is the half that
	// gives something up: a candidate whose own demand is unknown still
	// insists on having its cards to itself -- that is rule 4's entire point,
	// §5.3's "may start only alone on that GPU" -- but it may no longer
	// drain-stop a correctly-configured occupant to get there. It loses only
	// the privilege of killing a spec whose estimate someone filled in; it
	// keeps the refusal to share. So the answer is Wait -- which the
	// occupant's own idle timeout resolves while destroying nothing, WHEN IT
	// HAS ONE: Spec.IdleTimeoutSeconds == 0 means never unload, and then the
	// wait lasts until a measurement or an operator estimate arrives (§5.3
	// states that price). For an occupant that can never leave the answer is
	// the terminal step 2 already returned above. A total order converges, and
	// the spec that loses is always the misconfigured one.
	//
	// THE BLOCK IS UNCONDITIONAL, not "unless some other rule would have
	// evicted that occupant anyway". A closed matrix cell (rule 1) or an
	// overflowing sibling budget (rule 3) does supply an independent reason
	// to drain-stop the same occupant, and honouring the order only where no
	// such reason exists would leave those pairs evicting in both directions
	// -- the same defect, one rule over. The order is a property of the two
	// SPECS, not of this loop.
	//
	// A TOUCHER OF UNKNOWN DEMAND IS RULE 5's SUBJECT AND IS LEFT TO IT.
	// Rule 5 below scans exactly `procHasUnknownVRAM(r) && touchesAnyGPU(r,
	// gpuIdx)` over this same running set, ungated by the candidate's own
	// demand, so every occupant this loop would still have evicted after the
	// change -- an equally-unknown one, the tie the order does not decide --
	// gets the identical evict-if-idle / block-otherwise treatment there.
	// Repeating it here would be a branch whose deletion changes no decision.
	// That tie keeps its pre-existing mutual eviction and is a recorded
	// acceptance (11-risks-and-technical-debt.md §11.4) pinned by
	// TestAdmitUnknownTieStillEvictsBothWays -- which is also the test that
	// fails if rule 5's scan is ever narrowed out from under this comment.
	//
	// IT IS THE ONE WAIT THAT REPORTS ITSELF. Every other Wait in this
	// function resolves without anyone doing anything -- a busy neighbour
	// finishes, a queued victim drains -- but this one need not resolve at
	// all: the occupant is spared here precisely because it is not being
	// evicted, and owner.scanIdle skips a spec whose IdleTimeoutSeconds is 0,
	// which is the documented "never unload". An idle, unpinned,
	// never-again-requested occupant therefore keeps the card indefinitely
	// while this candidate requeues to its admission timeout on every
	// request. So this branch fills in Decision.Message, which the manager
	// records as the candidate's last_error (the portal renders it in an
	// always-visible column); the ordinary Waits deliberately keep saying
	// nothing, since an error on every contested start is how a diagnostic
	// stops being read.
	//
	// The PINNED variant of this same block, returned by step 2 above, needs
	// no message and still has none: it reports the terminal
	// StatePendingVRAMUnknown, and that state on this spec's own row already
	// names the missing number as this spec's (§5.3). A message is added here
	// precisely because a Wait has no state to speak for it.
	if unknownVRAM {
		var (
			precedenceBlocker string
			precedenceGPU     int
			foundBlocker      bool
		)
		for _, r := range running {
			gpu, shares := firstSharedGPU(r, gpuIdx)
			if !shares || procHasUnknownVRAM(r) {
				continue
			}
			blocked = true
			// Lowest spec id wins, as in pinnedUnknownOccupantRefusal and
			// sortOldestFirst: PolicySnapshot.Running arrives in map order
			// (owner.buildSnapshot ranges over o.specs), so "the first one
			// seen" would name a different spec on every admission against an
			// unchanged host -- in an operator-facing message.
			if !foundBlocker || r.SpecID < precedenceBlocker {
				precedenceBlocker, precedenceGPU, foundBlocker = r.SpecID, gpu, true
			}
		}
		if foundBlocker {
			// Both cards are named because they need not be the same one:
			// rule 4 demands aloneness on ALL of the candidate's cards, so the
			// card being held and the card whose number is missing can differ
			// (see specGPUIndexes). unknownGPU is the first in spec.GPUs
			// order, which is the order rule 3's messages already name cards
			// in.
			unknownGPU := 0
			for _, g := range spec.GPUs {
				if g.VRAMMB == 0 {
					unknownGPU = g.Index
					break
				}
			}
			waitMessage = fmt.Sprintf(
				"spec %s: its own demand on gpu %d is unknown, so it waits for spec %s to leave gpu %d rather than evicting it",
				spec.ID, unknownGPU, precedenceBlocker, precedenceGPU,
			)
		}
	}

	// Rule 5: rule 4's mirror in SUBJECT -- every remaining occupant that has
	// an unknown demand of its OWN and holds one of spec's GPUs, making that
	// card unshareable exactly as an unknown candidate does. It is the
	// winning half of the order above, so unlike rule 4 it still EVICTS:
	// idle occupants are queued as victims, and it is the only rule that
	// queues an occupant of unknown demand at all. Any pinned such occupant
	// already short-circuited above, so only idle/busy/starting remain here.
	//
	// It belongs in step 3, next to rules 1 and 4, for two reasons: it is
	// evaluated against the ORIGINAL running set, so its verdict never
	// depends on what another rule happened to queue first, and rule 3
	// below must already see its victims as gone -- an occupant queued here
	// is skipped by rule 3's sum, which is the only consistent reading
	// (charging it a number there cannot work; see procHasUnknownVRAM's own
	// doc comment). It must not move after rule 2 either: the process-count
	// limit runs last precisely so it asks only for victims the earlier
	// rules have not already supplied.
	for _, r := range running {
		if !procHasUnknownVRAM(r) || !touchesAnyGPU(r, gpuIdx) {
			continue
		}
		if isEvictable(r) {
			toEvict[r.SpecID] = r
		} else {
			blocked = true
		}
	}

	// Rule 3: per-GPU VRAM arithmetic, only over GPUs with a KNOWN demand
	// and a REAL (positive) budget.
	for _, g := range spec.GPUs {
		if g.VRAMMB == 0 {
			continue
		}
		// Absent from Budgets and a budget of 0 both mean "no budget for
		// this GPU" = unconstrained, and must behave identically -- see
		// PolicySnapshot.Budgets, and routing.ServerGPUBudget.BudgetMB in
		// the gateway (gateway/backend/internal/routing/store.go), which is
		// where that meaning is defined. The check lives HERE, at the one
		// place that interprets a budget, rather than in
		// owner.buildSnapshot where the map happens to be built: Budgets is
		// an exported field on an exported struct that any caller can
		// populate, so filtering at a single producer would leave every
		// other producer free to reintroduce the divergence.
		budget := snap.Budgets[g.Index]
		if budget <= 0 {
			continue
		}
		// spec's own demand is already known to fit this budget by itself:
		// step 1 (ownDemandRefusal) returned the terminal refusal otherwise,
		// before anything here or in step 2 ran.
		sum := g.VRAMMB
		var touchers []RunningProc
		for _, r := range running {
			if _, already := toEvict[r.SpecID]; already {
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
			continue
		}

		var idleTouchers []RunningProc
		for _, r := range touchers {
			if isEvictable(r) {
				idleTouchers = append(idleTouchers, r)
			}
		}
		sortOldestFirst(idleTouchers)
		for _, r := range idleTouchers {
			if sum <= budget {
				break
			}
			toEvict[r.SpecID] = r
			sum -= r.GPUs[g.Index]
		}
		if sum > budget {
			blocked = true
		}
	}

	// Rule 6: the host-wide RAM pool. RAM is a host resource, so unlike rule 3
	// there is ONE sum, over every running process, and victims are drawn from
	// ALL of them rather than from one GPU's occupants.
	//
	// It runs after rules 1, 3, 4 and 5 for the reason rule 3 runs after 1, 4
	// and 5: a process one of them already queued is guaranteed to be gone, so
	// its RAM is skipped here rather than charged twice, and it is not drawn
	// again -- a VRAM victim that also holds the RAM we need must not be joined
	// by a second, innocent one. It runs before rule 2 for the reason rules 1-5
	// do: the process-count limit asks only for the victims still missing.
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
	// evictable (isEvictable), so it reserves what it occupies: a second
	// admission sees the first one's headroom already spoken for and cannot
	// double-commit it. That is the interlock for a KNOWN demand. The interlock
	// for an UNKNOWN one is snap.RAMBootstrapInFlight, checked first below: a
	// bootstrapping process reserves nothing in the arithmetic (its RAMMB is the
	// unknown 0), so while one is mid-window EVERY other RAM admission Waits rather
	// than racing it for RAM whose true owner has not finished claiming it.
	//
	// The candidate's own demand against the whole pool is NOT decided here:
	// it is rule 6's own-demand half, hoisted to step 1 like rule 3's (see
	// ramOwnDemandRefusal), so by this point spec.RAMMB fits the budget alone
	// and every Wait below is behind holders that cannot be evicted right now:
	// a busy or still-loading one resolves by itself when it finishes, a pinned
	// one is the operator's own choice.
	if snap.HostRAMBudgetMB > 0 {
		switch {
		case snap.RAMBootstrapInFlight:
			// The cold-start interlock (task 4.1). Another spec's RAM is mid-
			// bootstrap and unaccounted, so no new RAM admission -- known demand or
			// a second bootstrap alike -- may proceed until it settles or leaves.
			// It is a Wait that evicts nothing (blocked always returns an empty
			// Evict list), and WaitOnRAM so the next hardware sample re-asks it.
			blocked = true
			ramBlocked = true
		default:
			// The candidate's own charge. A KNOWN demand is itself; an UNKNOWN one
			// (spec.RAMMB == 0, specHasUnknownRAM) is charged the conservative
			// cold-start guess instead of nothing (task 4.2), so its first start is
			// weighed against the pool like any other -- fitting with no eviction
			// when there is headroom, pre-evicting idle oldest-first only when the
			// host is genuinely too tight, and Waiting (below) rather than
			// blind-starting when nothing idle can make room. A guess of 0 (the
			// knob off) leaves candidateRAM 0, so this whole rule is skipped and the
			// spec starts unconstrained, exactly as before the bootstrap existed.
			candidateRAM := spec.RAMMB
			if specHasUnknownRAM(spec) {
				candidateRAM = snap.RAMBootstrapGuessMB
			}
			if candidateRAM <= 0 {
				break
			}
			sum := candidateRAM
			var holders []RunningProc
			for _, r := range running {
				if _, already := toEvict[r.SpecID]; already {
					continue // already guaranteed to be gone
				}
				if r.RAMMB <= 0 {
					continue // unknown occupancy: nothing to charge, nothing to free
				}
				sum += r.RAMMB
				holders = append(holders, r)
			}
			if sum > snap.HostRAMBudgetMB {
				var idleHolders []RunningProc
				for _, r := range holders {
					if isEvictable(r) {
						idleHolders = append(idleHolders, r)
					}
				}
				sortOldestFirst(idleHolders)
				for _, r := range idleHolders {
					if sum <= snap.HostRAMBudgetMB {
						break
					}
					toEvict[r.SpecID] = r
					sum -= r.RAMMB
				}
				if sum > snap.HostRAMBudgetMB {
					blocked = true
					ramBlocked = true
				}
			}
		}
	}

	// Rule 2: process-count limit, asking only for as many ADDITIONAL
	// evictions as rules 1, 3, 4, 5 and 6 have not already provided.
	if snap.MaxProcesses > 0 {
		remaining := len(running) - len(toEvict)
		deficit := remaining + 1 - snap.MaxProcesses
		if deficit > 0 {
			var candidates []RunningProc
			for _, r := range running {
				if _, already := toEvict[r.SpecID]; already {
					continue
				}
				if isEvictable(r) {
					candidates = append(candidates, r)
				}
			}
			sortOldestFirst(candidates)
			if len(candidates) < deficit {
				blocked = true
			}
			for i := 0; i < deficit && i < len(candidates); i++ {
				toEvict[candidates[i].SpecID] = candidates[i]
			}
		}
	}

	if blocked {
		return Decision{Wait: true, WaitOnRAM: ramBlocked, Message: waitMessage, Evict: []string{}}
	}
	if len(toEvict) == 0 {
		return Decision{OK: true, Evict: []string{}}
	}

	victims := make([]RunningProc, 0, len(toEvict))
	for _, r := range toEvict {
		victims = append(victims, r)
	}
	sortOldestFirst(victims)
	evict := make([]string, len(victims))
	for i, r := range victims {
		evict[i] = r.SpecID
	}
	return Decision{Evict: evict}
}
