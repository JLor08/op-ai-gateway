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
	SpecID   string
	GPUs     map[int]int // gpu index -> effective VRAM MB
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
//   - Wait: OK is false, Wait is true, Evict is empty. Every remaining
//     blocker is busy, still loading, or (outside the unknown-VRAM case
//     below) pinned: nothing can be evicted to help right now, so the caller
//     queues the request and re-asks on the next completion.
//   - Reason: OK is false, Wait is false, Evict is empty, and Reason names
//     a durable, non-transient block -- one that cannot resolve by
//     eviction OR by waiting, so reporting it as either of the other two
//     outcomes would be actively misleading (Wait would hang the caller
//     until its admission-wait timeout on every single request; Evict
//     would ask for a drain-stop that can never restore a fit). Two
//     distinct causes share this shape, both worth telling apart via
//     Message since both surface as the same State:
//   - StatePendingVRAMUnknown: a VRAM demand on one of the contested
//     GPUs is unknown -- the CANDIDATE's own (rule 4) or a running
//     OCCUPANT's (rule 5) -- and a PINNED process sits on the other
//     side of that contention: pinned processes are never evicted,
//     and a pinned process never finishes, so waiting cannot help
//     either. Message is empty for this cause -- Reason alone is
//     unambiguous. Terminal is not the same as permanent: the manager
//     re-evaluates this state after notPermittedRetryInterval, so a
//     measurement landing on the pinned occupant (rule 5's case) or
//     an operator filling the estimate in clears it without anything
//     being evicted.
//   - StateNotPermitted: spec's own declared demand on some GPU
//     already exceeds that GPU's budget on its own, with nothing
//     running at all. No eviction, on this GPU or any other, can ever
//     make it fit -- this is an operator misconfiguration (an
//     over-estimated VRAM demand, or a budget shrunk below it), the
//     same "configuration error, visible in the portal, not
//     transient" meaning the design doc already assigns
//     StateNotPermitted for a refused binary/directory. Message
//     distinguishes this VRAM-budget cause from that one, since both
//     share the State.
type Decision struct {
	OK      bool
	Reason  State    // "" unless a terminal case above applies
	Message string   // set only alongside a terminal Reason that needs disambiguating context; "" otherwise
	Evict   []string // spec IDs to drain-stop first, oldest LastUsed first; never nil
	Wait    bool
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

// touchesAnyGPU reports whether r occupies any GPU index named in idx.
func touchesAnyGPU(r RunningProc, idx []int) bool {
	for _, i := range idx {
		if _, ok := r.GPUs[i]; ok {
			return true
		}
	}
	return false
}

// specGPUIndexes returns every GPU index spec.GPUs names, known-VRAM and
// unknown-VRAM alike: the unknown-VRAM rule (rule 4) requires the spec to
// start alone on ALL of its GPUs, not only the one(s) whose demand happens
// to be unknown.
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

// procHasUnknownVRAMOn reports whether r occupies any GPU index named in
// idx with an UNKNOWN demand. It is the occupant-side mirror of
// specHasUnknownVRAM, and rule 5 in Admit is the rule it feeds.
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
// process is not on that card, which rule 5 must ignore rather than read as
// unknown -- rule 5 is per-GPU, like rule 3 and unlike rule 1.
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
func procHasUnknownVRAMOn(r RunningProc, idx []int) bool {
	for _, i := range idx {
		if v, ok := r.GPUs[i]; ok && v == 0 {
			return true
		}
	}
	return false
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
// compatibility, the process-count limit, and per-GPU VRAM arithmetic --
// plus the unknown-VRAM-alone rule, applied SYMMETRICALLY to both sides of
// the pair (rule 4 for the candidate's own unknown demand, rule 5 for a
// running occupant's), and computes the idle-victim set that would unblock
// the start if one exists.
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
//  1. Unknown-VRAM-vs-pinned short-circuit, for an unknown demand on
//     EITHER side of the pair. This is one of two conditions that can
//     resolve neither by eviction nor by waiting, so it is checked, and
//     returned, before any other rule.
//  2. Collect the matrix (rule 1) and unknown-VRAM (rules 4 and 5)
//     blockers against the self-filtered running set: every disallowed
//     pair; -- if spec has any unknown-VRAM GPU -- every process touching
//     any of spec's GPUs; and every process whose OWN demand on one of
//     spec's GPUs is unknown. An evictable blocker is queued for
//     eviction; a non-evictable (busy or still-loading) blocker marks the
//     whole decision blocked. Rules 4 and 5 name the same occupants when
//     both sides are unknown, which is harmless: toEvict is keyed by spec
//     id, so the overlap costs a map write and never a duplicate victim.
//  3. Per-GPU budget (rule 3), evaluated with step 2's already-queued
//     evictions notionally already gone: for every GPU spec has a KNOWN
//     demand for, sum spec's own demand plus every remaining toucher's
//     demand. A GPU absent from Budgets, and one whose budget is 0, are
//     both unconstrained and are skipped identically (see
//     PolicySnapshot.Budgets). If spec's own demand alone already exceeds a
//     real, positive budget,
//     that is the SECOND unresolvable-by-eviction-or-waiting condition --
//     no toucher, present or absent, changes that -- so it returns a
//     terminal StateNotPermitted immediately rather than evicting or
//     waiting pointlessly. Otherwise, overflow evicts idle touchers of
//     THAT gpu only, oldest LastUsed first, until it fits or idle
//     candidates run out.
//  4. Process-count limit (rule 2), evaluated last so it only asks for as
//     many ADDITIONAL evictions as steps 2-3 have not already supplied.
//  5. Build the Decision: any non-evictable blocker anywhere makes the
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

	// Step 1. A pinned process is never an eviction victim and never
	// finishes, so an unknown demand contested with one resolves by neither
	// eviction nor waiting: returned ahead of every other rule so nothing
	// below can propose a victim or a Wait that could not possibly help.
	// Both directions land here because both are the same contention seen
	// from opposite ends -- one card, two processes, and no number for at
	// least one of them.
	for _, r := range running {
		if !r.Pinned {
			continue
		}
		// Rule 4, candidate side: spec's own demand is unknown, so it may
		// only run alone on its GPUs, and this pinned process holds one.
		if unknownVRAM && touchesAnyGPU(r, gpuIdx) {
			return Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}}
		}
		// Rule 5, occupant side: the pinned process's OWN demand on one of
		// spec's GPUs is unknown, so that card cannot be shared with it
		// either -- whatever spec declares for itself. Note this implies
		// touchesAnyGPU: procHasUnknownVRAMOn needs the key present.
		if procHasUnknownVRAMOn(r, gpuIdx) {
			return Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}}
		}
	}

	toEvict := make(map[string]RunningProc)
	blocked := false

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
	// pinned toucher already short-circuited above, so only idle/busy
	// remain here.
	if unknownVRAM {
		for _, r := range running {
			if !touchesAnyGPU(r, gpuIdx) {
				continue
			}
			if isEvictable(r) {
				toEvict[r.SpecID] = r
			} else {
				blocked = true
			}
		}
	}

	// Rule 5: the mirror of rule 4 -- every remaining occupant whose OWN
	// demand on one of spec's GPUs is unknown, blocking the sharing of that
	// card exactly as an unknown candidate does. Any pinned such occupant
	// already short-circuited above, so only idle/busy remain here.
	//
	// It belongs in step 2, next to rules 1 and 4, for two reasons: it is
	// evaluated against the ORIGINAL running set, so its verdict never
	// depends on what another rule happened to queue first, and rule 3
	// below must already see its victims as gone -- an occupant queued here
	// is skipped by rule 3's sum, which is the only consistent reading
	// (charging it a number there cannot work; see procHasUnknownVRAMOn).
	// It must not move after rule 2 either: the process-count limit runs
	// last precisely so it asks only for victims the earlier rules have not
	// already supplied.
	for _, r := range running {
		if !procHasUnknownVRAMOn(r, gpuIdx) {
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
		if g.VRAMMB > budget {
			// spec's own single-GPU demand alone already exceeds the
			// budget: no eviction, on this GPU or any other, can ever
			// make this fit, and waiting cannot help either since
			// nothing here depends on what else happens to be running.
			// A durable operator-configuration problem, not a transient
			// contention one.
			return Decision{
				Reason: StateNotPermitted,
				Message: fmt.Sprintf(
					"spec %s: gpu %d demand %d MB exceeds budget %d MB on its own",
					spec.ID, g.Index, g.VRAMMB, budget,
				),
				Evict: []string{},
			}
		}

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

	// Rule 2: process-count limit, asking only for as many ADDITIONAL
	// evictions as rules 1/3/4 have not already provided.
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
		return Decision{Wait: true, Evict: []string{}}
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
