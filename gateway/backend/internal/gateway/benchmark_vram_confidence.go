// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"sort"
	"strconv"
	"strings"
)

// The VRAM run's confidence gates: what stands between "a window produced a
// number" and "the run is allowed to report it".
//
// They exist because the honesty gates the run shipped with all constrain
// movement INSIDE a phase window, while the run's exposure is mostly BETWEEN
// them. The isolation is proved once, before the baseline; then come a settle,
// a baseline window, the whole load (bounded only by the startup timeout, by
// far the longest part of the run), and another settle -- and a neighbour that
// allocates anywhere in there leaves both windows individually stable and has
// its allocation added wholesale to delta_mb. The gates below close that
// window from three independent directions, and every one of them reports
// INCONCLUSIVE WITH A REASON rather than a number: a definitive-looking figure
// from a contaminated window is the worst outcome this feature has, strictly
// worse than no figure at all.
const (
	// vramInconclusiveIsolationLost: this run's isolation did not hold for the
	// whole run, so the report may not claim it. The commonest route is an
	// operator's own "Force start" on a drained sibling -- nothing disables
	// those actions while a run holds the server reservation -- but a request
	// straight to the agent's own router port does it too.
	//
	// TWO PRODUCERS, one reason, because the operator's next action is the
	// same for both -- something outside the run changed the fleet, so find it
	// and measure again:
	//
	//  1. the END-OF-RUN re-verification below (vramIsolationLost): a spec this
	//     run had drained held a live process again by the end of the
	//     measurement. Observed on the status stream.
	//  2. the ISOLATION WAIT (vramAwaitIsolation's gate): a freshly derived
	//     runtime-config document no longer force-stops every enumerated spec,
	//     so the override this run wrote has already been taken over. Read off
	//     the document rather than off a process -- it catches the revocation
	//     BEFORE any sibling has had time to start, which is strictly earlier
	//     than (1) can see it.
	vramInconclusiveIsolationLost = "isolation_lost"
	// vramInconclusiveStrategyDisagreement: the two INDEPENDENT numbers
	// disagree beyond what the difference in the quantities can explain. The
	// delta is the model's marginal cost across a window; the agent's
	// measurement is that process's attributed usage. They may legitimately
	// differ -- the delta carries the CUDA context and the driver reserve the
	// per-process measurer excludes -- but a gap past that allowance means one
	// of them measured something the other did not, which for the delta means
	// something else allocated inside the window. Running both strategies is
	// only worth its cost if the run actually cross-checks them.
	vramInconclusiveStrategyDisagreement = "strategy_disagreement"
)

// vramEvidenceRestartedDuringRun is the third isolation-evidence value, and
// the only one that is not evidence OF isolation: this run confirmed the spec
// had no process, and then saw it with one again before the measurement ended.
//
// It is recorded per spec rather than only as an inconclusive reason so the
// stored payload NAMES what broke the isolation, which is the whole reason
// IsolationEvidence is reported alongside the boolean. Because it sits outside
// the closed set vramIsolationConfirmed accepts, recording it for an
// ENUMERATED spec flips VRAMReport.Isolated through the very function that
// computes that field. A spec created AFTER the enumeration is not in that
// list, so the caller refuses on the lost set itself as well -- see
// vramIsolationLost's own doc.
const vramEvidenceRestartedDuringRun = "restarted_during_run"

// vramWarningUndeclaredGPUAllocation: the model allocated on a card the launch
// spec does not declare.
//
// This WARNS rather than refusing, and the distinction is the spec's own GPU
// rows. Every per-card number on the report is still correct -- the declared
// card's delta is that card's real cost -- so there is nothing dishonest to
// suppress. What is wrong is the SPEC: `set_visible_devices` defaults to
// false, so the child sees every card whatever the spec declares, and a spec
// that declares one card for a model that uses two describes the model
// incorrectly. The operator's action is to add the missing GPU row, which is
// what the warning says; refusing the run would withhold the very numbers that
// tell them which row to add.
//
// The run must not SILENTLY halve the answer, though, which is the half that
// was missing: the undeclared card is reported as an item of its own (marked
// unattributable, because there is no row to apply it to yet) and counts
// toward the headline, so the model's total cost is complete on the report.
const vramWarningUndeclaredGPUAllocation = "undeclared_gpu_allocation"

// codeBenchmarkVRAMDeclaredGPUMissing is the fifth precondition refusal: the
// target's launch spec declares a GPU index this host does not report.
//
// It is a refusal rather than an inconclusive result because the run could
// never succeed and would drain the whole fleet first. vramWindowStable
// refuses a watched card that is missing from any sample -- the run cannot
// difference what it cannot see -- so every window burns its full bound and
// the run reports `baseline_unstable`, whose stated next action ("something on
// the card was moving; retry once the server is quiet") is wrong and can never
// work. The host's full card list is already in hand at trigger time, so the
// refusal that names the missing index costs nothing.
const codeBenchmarkVRAMDeclaredGPUMissing = "benchmark.vram_declared_gpu_missing"

// The cross-check's allowance. REASONED, NOT MEASURED -- vars so tests can
// tighten or widen them, and each needs validating against a real fleet
// alongside K, the stability tolerance and the phase settles.
var (
	// vramCrossCheckAllowanceMB is the absolute gap the two strategies may
	// show. The delta includes what the process's CUDA context and the
	// driver's own reserve took when the model loaded; a per-process measurer
	// attributes neither to the process. That difference is a few hundred MB
	// on NVIDIA and does not scale with the model, so it needs an absolute
	// allowance rather than only a relative one -- 10 % of a 2 GB model is
	// less than one context.
	vramCrossCheckAllowanceMB = 512
	// vramCrossCheckPct is the relative half of the same allowance, as a
	// fraction of the agent's own measurement: per-card attribution of a large
	// model drifts proportionally, not absolutely.
	vramCrossCheckPct = 0.10
)

// vramCardToleranceMB is one card's own stability tolerance in MB -- the
// per-card decision threshold, as distinct from vramFloorMB's headline floor,
// which is deliberately the LARGEST tolerance among every watched card. Using
// the headline floor per card discards a genuine allocation on a small card
// whenever a big one is watched beside it: on a host with an 80 GiB card (1 %
// = 819 MB) and an 8 GiB one (64 MB), every real 100-800 MB allocation on the
// small card reads as noise on the big one's terms.
func vramCardToleranceMB(gpu routing.GPUSample) int {
	return int(vramStabilityTolerance(gpu.MemTotalBytes) / vramBytesPerMB)
}

// vramIsolationLost reports which specs broke this run's isolation between the
// isolation proof and here, ascending. An empty result means the isolation the
// report claims held for the whole measurement, not merely at the drain.
//
// THE TARGET IS EXCLUDED, and that is the whole asymmetry: the run cleared the
// target's own override in order to load it, so the target holding a process
// at the end is the point of the run rather than contamination.
//
// Two questions, because a spec can break the isolation in two ways, and the
// two answers come from vramEnumeratedNotQuiet and vramUnenumeratedWithProcess
// -- each carrying the reasoning for the half it decides. They partition the
// snapshot (one reads only enumerated specs, the other only unenumerated
// ones), so their results need no de-duplication between them.
//
// It reads the runtime-status snapshot rather than waiting for a delivered
// frame, so it is up to one telemetry interval stale (the default is 1 s, and
// OP_AGENT_INTERVAL raises it). It is therefore a contamination DETECTOR, not
// a proof that nothing started in the final instant -- which is why the
// cross-check below stands beside it rather than behind it.
func (s *Server) vramIsolationLost(serverID string, plan vramRunPlanned) []string {
	enumerated := make(map[string]bool, len(plan.specIDs))
	for _, specID := range plan.specIDs {
		enumerated[specID] = true
	}
	stateBySpec := vramStateBySpec(s.RuntimeStatus.statusSnapshot(serverID))

	lost := append(
		vramEnumeratedNotQuiet(enumerated, plan.targetSpecID, stateBySpec),
		vramUnenumeratedWithProcess(enumerated, plan.targetSpecID, stateBySpec)...,
	)
	if len(lost) == 0 {
		return nil
	}
	sort.Strings(lost)
	return lost
}

// vramEnumeratedNotQuiet is vramIsolationLost's first question: every
// ENUMERATED sibling must still be present in a state this gateway recognizes
// as having no process.
//
// ABSENT counts as lost, and that is not over-strict: vramAwaitIsolation only
// ever recorded evidence for a spec it saw PRESENT in a no-process state, so
// every one of them was in the agent's report during this run, and a snapshot
// lingers rather than being cleared. A spec that has since vanished from it
// means the agent's own picture changed under the measurement.
//
// Iterating the enumerated SET rather than the caller's slice is what keeps a
// repeated spec id from being reported twice.
func vramEnumeratedNotQuiet(enumerated map[string]bool, targetSpecID string, stateBySpec map[string]string) []string {
	var lost []string
	for specID := range enumerated {
		if specID == targetSpecID {
			continue
		}
		if state, present := stateBySpec[specID]; !present || !vramStateNoProcess(state) {
			lost = append(lost, specID)
		}
	}
	return lost
}

// vramUnenumeratedWithProcess is vramIsolationLost's second question: no spec
// OUTSIDE the enumerated set may hold a process either.
//
// The enumeration is a trigger-time fact, and a launch spec created (or
// re-enabled) after it is neither drained nor required to carry evidence -- so
// checking only the enumerated set would leave exactly that spec free to start
// and be measured as though it were the target.
//
// The test is the POSITIVE one (vramStatesWithProcess), not the negation of
// vramStateNoProcess, and the asymmetry against the enumerated half is
// deliberate: an unrecognized state on a spec this run never drained is no
// evidence that something started, whereas on an enumerated spec the same
// unrecognized state withdraws evidence this run had already recorded.
func vramUnenumeratedWithProcess(enumerated map[string]bool, targetSpecID string, stateBySpec map[string]string) []string {
	var lost []string
	for specID, state := range stateBySpec {
		if specID == targetSpecID || enumerated[specID] {
			continue
		}
		if vramStatesWithProcess[state] {
			lost = append(lost, specID)
		}
	}
	return lost
}

// vramUndeclaredAllocations reports the cards OUTSIDE the watched set that this
// run saw allocate, ascending.
//
// It exists because `set_visible_devices` defaults to false, so the child
// process sees every card on the host whatever its launch spec declares. A
// model that splits onto a card the spec does not declare therefore produced a
// per-card delta on the declared card that was individually correct and, as
// the model's total cost, silently half the answer -- with the agent's own
// per-card measurement, the one number that proved it, filtered out before
// anything could compare them.
//
// EITHER signal counts, because they fail on different host classes. A window
// delta over that card's own tolerance is available everywhere (it is the whole
// reason strategy (b) exists); a positive per-process measurement is the
// stronger evidence where a measurer exists, since it attributes the allocation
// to this very spec rather than to the host.
//
// These deltas are NOT stability-gated: the two windows only ever held the
// declared cards still, and widening the gate to every card in the host would
// make the feature unusable on exactly the deployments that coexist with a
// non-managed neighbour, which §13 blesses. So an undeclared figure is
// reported as unattributable, alongside a warning, and never as the headline
// on its own.
func vramUndeclaredAllocations(watched []int, baseline, after routing.TelemetrySample, measured map[int]int) []int {
	isWatched := make(map[int]bool, len(watched))
	for _, index := range watched {
		isWatched[index] = true
	}
	beforeByIndex := vramGPUByIndex(baseline)
	afterByIndex := vramGPUByIndex(after)

	seen := map[int]struct{}{}
	var out []int
	consider := func(index int, allocated bool) {
		if isWatched[index] || !allocated {
			return
		}
		if _, dup := seen[index]; dup {
			return
		}
		seen[index] = struct{}{}
		out = append(out, index)
	}
	for index, gpu := range afterByIndex {
		delta := vramDeltaMB(beforeByIndex[index].MemUsedBytes, gpu.MemUsedBytes)
		consider(index, delta >= vramCardToleranceMB(gpu))
	}
	for index, mb := range measured {
		consider(index, mb > 0)
	}
	sort.Ints(out)
	return out
}

// vramStrategiesDisagree reports whether any card's two INDEPENDENT numbers
// are further apart than the difference in the quantities can explain.
//
// It is the cross-check that is half the value of running both strategies, and
// it is the only gate that can catch a neighbour the gateway cannot see: a
// non-managed application allocating during the load leaves both windows
// individually stable and the managed fleet provably isolated, so nothing else
// in the run has any reason to doubt the delta. The agent's own per-process
// figure does, because it counts only the target's own pages.
//
// Only a card carrying BOTH numbers is checked. A host with no per-process
// measurer -- AMD via ROCm, Apple via ioreg, which is the case the delta
// strategy exists for -- has nothing to cross-check against, and demanding a
// second opinion there would refuse every run on exactly those hosts.
func vramStrategiesDisagree(items []VRAMGPUItem, after routing.TelemetrySample) bool {
	afterByIndex := vramGPUByIndex(after)
	for _, item := range items {
		if item.DeltaMB <= 0 || item.MeasuredMB <= 0 {
			continue
		}
		gap := item.DeltaMB - item.MeasuredMB
		if gap < 0 {
			gap = -gap
		}
		allowance := vramCardToleranceMB(afterByIndex[item.Index])
		if vramCrossCheckAllowanceMB > allowance {
			allowance = vramCrossCheckAllowanceMB
		}
		if relative := int(vramCrossCheckPct * float64(item.MeasuredMB)); relative > allowance {
			allowance = relative
		}
		if gap > allowance {
			return true
		}
	}
	return false
}

// vramVisibleItems drops the items that carry no information, and is applied
// only AFTER the headline gate has run.
//
// The ordering is the point. A declared card is always reported, even at 0 --
// a card the operator asked about that showed nothing is an answer. An
// UNATTRIBUTABLE card has no row to be applied to, so a list of every quiet
// card in the host would be noise; but filtering those out BEFORE the headline
// was summed is what made the identical measurement definitive with declared
// GPU rows and `below_floor` without them, contradicting vramHeadlineDeltaMB's
// own rationale that a model split over two cards costs the sum of both.
//
// The test is "contributed nothing", NOT "below this card's tolerance". A card
// whose delta is under its own tolerance can still be part of a headline that
// cleared the floor -- two cards at 300 MB each on 48 GiB cards, say -- and
// dropping it would leave a DEFINITIVE report with zero GPU items, which the
// result contract reads as a measurement of nothing.
func vramVisibleItems(items []VRAMGPUItem) []VRAMGPUItem {
	out := make([]VRAMGPUItem, 0, len(items))
	for _, item := range items {
		if !item.Attributable && item.DeltaMB <= 0 && item.MeasuredMB <= 0 {
			continue
		}
		out = append(out, item)
	}
	return out
}

// vramDeclaredGPUMissing is the fifth precondition: refuse when the target's
// launch spec declares a GPU index this host does not report. See
// codeBenchmarkVRAMDeclaredGPUMissing for why this is a refusal and not an
// inconclusive result.
//
// It reads the same trigger-time sample the watched set and the fingerprints
// come from, so it costs one store read and no extra telemetry. A store error
// is returned as itself: this gate may not swallow a read failure into "no
// missing cards", which is the direction that drains a fleet for a run that
// cannot succeed.
func (s *Server) vramDeclaredGPUMissing(ctx context.Context, targetSpecID string, sample routing.TelemetrySample) error {
	specGPUs, err := s.Routes.RuntimeSpecGPUs(ctx, targetSpecID)
	if err != nil {
		return err
	}
	present := vramGPUByIndex(sample)
	seen := map[int]struct{}{}
	var missing []int
	for _, row := range specGPUs {
		if _, ok := present[row.GPUIndex]; ok {
			continue
		}
		if _, dup := seen[row.GPUIndex]; dup {
			continue
		}
		seen[row.GPUIndex] = struct{}{}
		missing = append(missing, row.GPUIndex)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Ints(missing)
	labels := make([]string, 0, len(missing))
	for _, index := range missing {
		labels = append(labels, strconv.Itoa(index))
	}
	return &vramRefusal{
		code: codeBenchmarkVRAMDeclaredGPUMissing,
		msg: "this launch spec declares GPU index " + strings.Join(labels, ", ") +
			", which this server does not report; correct the spec's GPU rows first",
	}
}
