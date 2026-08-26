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
}

// PolicySnapshot is the pure input to Admit: the currently running set plus
// the configured constraints, all as of one instant. No clock, no store, no
// live process handles -- a caller assembles a fresh snapshot for every
// admission decision.
type PolicySnapshot struct {
	Running      []RunningProc
	MaxProcesses int         // 0 = unlimited
	Budgets      map[int]int // gpu index -> budget MB; an ABSENT index is unconstrained, never zero-budget
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
//     blocker is busy (or, outside the unknown-VRAM case below, pinned):
//     nothing can be evicted to help right now, so the caller queues the
//     request and re-asks on the next completion.
//   - Reason: OK is false, Wait is false, Evict is empty, and Reason names
//     a durable, non-transient block -- one that cannot resolve by
//     eviction OR by waiting, so reporting it as either of the other two
//     outcomes would be actively misleading (Wait would hang the caller
//     until its admission-wait timeout on every single request; Evict
//     would ask for a drain-stop that can never restore a fit). Two
//     distinct causes share this shape, both worth telling apart via
//     Message since both surface as the same State:
//   - StatePendingVRAMUnknown: spec has unknown VRAM demand and a
//     PINNED process already occupies one of its GPUs (pinned
//     processes are never evicted, and a pinned process never
//     finishes, so waiting cannot help either). Message is empty for
//     this cause -- Reason alone is unambiguous.
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
// never a pinned process, regardless of its InFlight count, and only when
// it is serving no request right now.
func isEvictable(r RunningProc) bool {
	return !r.Pinned && r.InFlight == 0
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
// plus the unknown-VRAM-alone rule, and computes the idle-victim set that
// would unblock the start if one exists.
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
//  1. Unknown-VRAM-vs-pinned short-circuit. This is one of two conditions
//     that can resolve neither by eviction nor by waiting, so it is
//     checked, and returned, before any other rule.
//  2. Collect the matrix (rule 1) and unknown-VRAM (rule 4) blockers
//     against the self-filtered running set: every disallowed pair, and
//     -- if spec has any unknown-VRAM GPU -- every process touching any
//     of spec's GPUs. An evictable blocker is queued for eviction; a
//     non-evictable (busy) blocker marks the whole decision blocked.
//  3. Per-GPU budget (rule 3), evaluated with step 2's already-queued
//     evictions notionally already gone: for every GPU spec has a KNOWN
//     demand for, sum spec's own demand plus every remaining toucher's
//     demand. A GPU absent from Budgets is unconstrained, never
//     zero-budget. If spec's own demand alone already exceeds the budget,
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

	if unknownVRAM {
		for _, r := range running {
			if r.Pinned && touchesAnyGPU(r, gpuIdx) {
				return Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}}
			}
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

	// Rule 3: per-GPU VRAM arithmetic, only over GPUs with a KNOWN demand.
	for _, g := range spec.GPUs {
		if g.VRAMMB == 0 {
			continue
		}
		budget, budgeted := snap.Budgets[g.Index]
		if !budgeted {
			continue // absent from Budgets = unconstrained, never zero-budget
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
