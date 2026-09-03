// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The confidence gates: everything that stands between "a window produced a
// number" and "the run is allowed to report it". Each test here exists for a
// concrete way a definitive-looking number was reachable from a contaminated
// window -- which is the worst outcome this feature has, strictly worse than
// no number at all.

// vramCard builds one GPU row of a fake telemetry sample.
func vramCard(index int, name, uuid string, usedMiB, totalMiB int64) routing.GPUSample {
	return routing.GPUSample{
		Index: index, Name: name, UUID: uuid,
		MemUsedBytes: usedMiB * oneMiB, MemTotalBytes: totalMiB * oneMiB,
	}
}

func vramSampleOf(rows ...routing.GPUSample) routing.TelemetrySample {
	return routing.TelemetrySample{ServerID: "srv1", GPUs: rows}
}

// --- F1: isolation is proved once, and the whole load phase is unguarded ----

// TestVRAMRunRefusesANumberWhenIsolationBrokeDuringTheLoad is the critical
// gap: the isolation proof happens BEFORE the baseline, and everything after
// it -- the settle, the baseline window, the whole load (bounded only by the
// stream watchdog), the post-load settle -- was unmonitored. The stability
// gate only sees movement INSIDE a window, so a sibling started mid-load
// leaves both windows individually stable and its allocation is added
// wholesale to delta_mb and reported as definitive with isolated: true.
func TestVRAMRunRefusesANumberWhenIsolationBrokeDuringTheLoad(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		// The target costs 21000 MiB. A sibling force-started by a portal
		// click during the load allocates another 3000 MiB on the same card.
		f.used0.Store((500 + 21000 + 3000) * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{
				SpecID: "rspec_target", State: "running", PID: 5,
				GPUs:       []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 21000}},
				MeasuredAt: time.Now().UTC(),
			},
			// The sibling has a live process again: the isolation this run
			// proved before the baseline no longer holds.
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "running", PID: 6},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil {
		t.Fatal("VRAM = nil, want a report carrying the reason")
	}
	if report.Inconclusive != "isolation_lost" {
		t.Fatalf("Inconclusive = %q, want %q -- delta_mb %d was measured through a sibling's allocation",
			report.Inconclusive, "isolation_lost", vramHeadlineDeltaMB(report.GPUs))
	}
	if report.Isolated {
		t.Fatal("Isolated = true for a run whose isolation demonstrably broke")
	}
	if got := report.IsolationEvidence[f.siblingSpec]; got != "restarted_during_run" {
		t.Fatalf("IsolationEvidence[%s] = %q, want %q so the claim can be audited",
			f.siblingSpec, got, "restarted_during_run")
	}
	for _, item := range report.GPUs {
		if item.DeltaMB != 0 {
			t.Fatalf("a contaminated run reported a delta: %#v", item)
		}
	}
}

// TestVRAMRunAcceptsTheTargetsOwnProcessAtTheEnd is the other half of the
// gate: the run itself cleared the TARGET's override in order to load it, so
// the target is expected to have a live process by the end. Only a SIBLING
// with a process is contamination.
func TestVRAMRunAcceptsTheTargetsOwnProcessAtTheEnd(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		f.used0.Store(21500 * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{SpecID: "rspec_target", State: "running", PID: 5},
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result: the target running at the end is the point of the run", report)
	}
	if !report.Isolated {
		t.Fatalf("Isolated = false; evidence = %v", report.IsolationEvidence)
	}
}

// --- F9: the cross-check that is half the value of running both -------------

// TestVRAMRunRefusesANumberWhenTheTwoStrategiesDisagree: measured_mb is the
// INDEPENDENT quantity that contradicts a contaminated delta, and nothing
// compared the two. A non-managed neighbour allocating during the load -- by
// far the longest part of the run, and the phase no window covers -- leaves
// both windows stable while the agent's own per-process figure stays at the
// model's real cost. That gap IS the contamination signal.
func TestVRAMRunRefusesANumberWhenTheTwoStrategiesDisagree(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		// The model costs 21000 MiB; a neighbour the gateway cannot stop took
		// another 3000 MiB during the load. Isolation of the MANAGED fleet
		// still holds, so only the cross-check can catch this.
		f.used0.Store((500 + 21000 + 3000) * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{
				SpecID: "rspec_target", State: "running", PID: 5,
				GPUs:       []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 21000}},
				MeasuredAt: time.Now().UTC(),
			},
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil {
		t.Fatal("VRAM = nil, want a report carrying the reason")
	}
	if report.Inconclusive != "strategy_disagreement" {
		t.Fatalf("Inconclusive = %q, want %q", report.Inconclusive, "strategy_disagreement")
	}
	// The two numbers ARE the evidence for this reason, so they stay on the
	// report -- unlike below_floor, where there is nothing to show.
	if len(report.GPUs) != 1 || report.GPUs[0].DeltaMB != 24000 || report.GPUs[0].MeasuredMB != 21000 {
		t.Fatalf("GPUs = %#v, want both disagreeing numbers kept as the evidence", report.GPUs)
	}
}

// TestVRAMRunToleratesTheDriverReserveBetweenTheTwoStrategies is the gate's
// other direction: the two numbers are NOT the same quantity and may
// legitimately differ. A delta carries the process's CUDA context and the
// driver reserve, which a per-process measurer excludes, so a few hundred MB
// of gap must not cost the operator their run.
func TestVRAMRunToleratesTheDriverReserveBetweenTheTwoStrategies(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		f.used0.Store((500 + 21000) * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{
				SpecID: "rspec_target", State: "running", PID: 5,
				GPUs:       []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 20600}},
				MeasuredAt: time.Now().UTC(),
			},
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result: 400 MB of driver reserve is not a disagreement", report)
	}
}

// --- F7: an allocation on a card the spec does not declare ------------------

// TestVRAMRunReportsAnAllocationOnAnUndeclaredCard: set_visible_devices
// defaults to false, so the child sees every card whatever the spec declares.
// A model that splits onto an undeclared card produced a DEFINITIVE per-card
// delta that silently halved the model's total cost -- and the agent's own
// per-card measurement, the one number that proves the under-report, was
// filtered out before anything could compare them.
func TestVRAMRunReportsAnAllocationOnAnUndeclaredCard(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{
		targetGPUs: []routing.RuntimeSpecGPU{{GPUIndex: 0, VRAMEstimateMB: 6000}},
	})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		// llama.cpp splits over cards 0 and 1 at 6144 MiB each; the spec
		// declares card 0 only.
		f.used0.Store((500 + 6144) * oneMiB)
		f.used1.Store((300 + 6144) * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{
				SpecID: "rspec_target", State: "running", PID: 5,
				GPUs: []RuntimeGPUStatusDTO{
					{Index: 0, VRAMMeasuredMB: 6144},
					{Index: 1, VRAMMeasuredMB: 6144},
				},
				MeasuredAt: time.Now().UTC(),
			},
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if len(report.GPUs) != 2 {
		t.Fatalf("GPUs = %#v, want BOTH halves of the model reported", report.GPUs)
	}
	byIndex := map[int]VRAMGPUItem{}
	for _, item := range report.GPUs {
		byIndex[item.Index] = item
	}
	if got := byIndex[0]; got.DeltaMB != 6144 || !got.Attributable {
		t.Fatalf("card 0 = %#v, want 6144 MB on the DECLARED row", got)
	}
	if got := byIndex[1]; got.DeltaMB != 6144 || got.Attributable {
		t.Fatalf("card 1 = %#v, want 6144 MB reported and marked unattributable", got)
	}
	if got := byIndex[1].MeasuredMB; got != 6144 {
		t.Fatalf("card 1 measured_mb = %d, want the agent's own 6144 kept, not filtered out", got)
	}
	if total := vramHeadlineDeltaMB(report.GPUs); total != 12288 {
		t.Fatalf("headline = %d, want the model's whole 12288 MB", total)
	}
	if !vramHasWarning(report.Warnings, "undeclared_gpu_allocation") {
		t.Fatalf("Warnings = %v, want %q -- the spec's GPU rows are incomplete",
			report.Warnings, "undeclared_gpu_allocation")
	}
}

// TestVRAMRunDoesNotWarnAboutAQuietUndeclaredCard is the negative half: a card
// the spec does not declare and the model did not touch is not an undeclared
// allocation, and a warning that fires on every multi-GPU run is not a warning.
func TestVRAMRunDoesNotWarnAboutAQuietUndeclaredCard(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{
		targetGPUs: []routing.RuntimeSpecGPU{{GPUIndex: 0, VRAMEstimateMB: 18000}},
	})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if vramHasWarning(report.Warnings, "undeclared_gpu_allocation") {
		t.Fatalf("Warnings = %v, want no undeclared-allocation warning: card 1 never moved", report.Warnings)
	}
	if len(report.GPUs) != 1 || report.GPUs[0].Index != 0 {
		t.Fatalf("GPUs = %#v, want exactly the declared index", report.GPUs)
	}
}

// vramHasWarning is a local helper: does warnings carry want?
func vramHasWarning(warnings []string, want string) bool {
	for _, w := range warnings {
		if w == want {
			return true
		}
	}
	return false
}

// --- F10 / F17: the per-card decision, and when it is applied ---------------

// TestVRAMReportItemsKeepsASmallCardsRealAllocation is F10: the HEADLINE floor
// is deliberately the LARGEST per-card tolerance among the watched cards, and
// reusing it as the per-card inclusion threshold discards a genuine allocation
// on a small card whenever a big one is watched alongside it.
func TestVRAMReportItemsKeepsASmallCardsRealAllocation(t *testing.T) {
	const gib = 1024
	// An 80 GiB H100 (tolerance 1 % = 819 MB) beside an 8 GiB card (64 MB).
	before := vramSampleOf(
		vramCard(0, "H100", "GPU-a", 100, 80*gib),
		vramCard(1, "Small", "GPU-b", 100, 8*gib),
	)
	after := vramSampleOf(
		vramCard(0, "H100", "GPU-a", 5100, 80*gib),
		vramCard(1, "Small", "GPU-b", 700, 8*gib),
	)
	items := vramReportItems([]int{0, 1}, false, vramRunPlanned{baseline: before}, before, after, nil)
	if len(items) != 2 {
		t.Fatalf("items = %#v, want both cards: the small card's 600 MB is a real allocation, not noise on the H100", items)
	}
	if items[1].DeltaMB != 600 {
		t.Fatalf("small card delta = %d, want 600", items[1].DeltaMB)
	}
}

// TestVRAMReportItemsSumsBeforeItFilters is F17: for a spec that declares no
// GPU rows the per-card filter ran BEFORE the headline sum, so the identical
// measurement reported 600 MB with declared rows and NOTHING without them --
// contradicting vramHeadlineDeltaMB's own stated rationale, that a model split
// over two cards costs the sum of both deltas.
func TestVRAMReportItemsSumsBeforeItFilters(t *testing.T) {
	const gib = 1024
	before := vramSampleOf(
		vramCard(0, "L40S", "GPU-a", 100, 48*gib),
		vramCard(1, "L40S", "GPU-b", 100, 48*gib),
	)
	after := vramSampleOf(
		vramCard(0, "L40S", "GPU-a", 400, 48*gib),
		vramCard(1, "L40S", "GPU-b", 400, 48*gib),
	)
	watched := []int{0, 1}
	plan := vramRunPlanned{baseline: before}
	declared := vramReportItems(watched, true, plan, before, after, nil)
	undeclared := vramReportItems(watched, false, plan, before, after, nil)

	if got, want := vramHeadlineDeltaMB(declared), 600; got != want {
		t.Fatalf("declared headline = %d, want %d", got, want)
	}
	if got := vramHeadlineDeltaMB(undeclared); got != vramHeadlineDeltaMB(declared) {
		t.Fatalf("undeclared headline = %d, want the same %d: it is the same measurement, and the floor gate belongs to the SUM",
			got, vramHeadlineDeltaMB(declared))
	}
}

// TestVRAMRunAnUndeclaredSplitIsNotBelowFloor is F17 through the whole run:
// two cards each moving less than one card's tolerance, summing to more than
// the floor, on a spec that declares no GPU rows.
func TestVRAMRunAnUndeclaredSplitIsNotBelowFloor(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{targetGPUs: []routing.RuntimeSpecGPU{}})
	f.seedLatestSample()
	f.drive(t)
	// The fixture's cards are 24576 MiB, so one card's tolerance is 245 MB.
	// 200 MB each is under that; 400 MB together is over it.
	f.provider.onStream = func() {
		f.used0.Store(700 * oneMiB)
		f.used1.Store(500 * oneMiB)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result: 400 MB across two cards clears the floor", report)
	}
	if got := vramHeadlineDeltaMB(report.GPUs); got != 400 {
		t.Fatalf("headline = %d, want 400", got)
	}
	if len(report.GPUs) != 2 {
		t.Fatalf("GPUs = %#v, want both contributing cards", report.GPUs)
	}
}

// --- F12: the identity must describe the card the number came from ----------

// TestVRAMReportItemsFingerprintComesFromTheMeasuredWindow is F12: the
// fingerprint was read from the TRIGGER-TIME sample while both numbers come
// from the two stable windows, minutes later. If the cards were renumbered in
// between -- the exact event the fingerprint exists to detect -- the item
// recorded the OLD card's uuid next to the NEW card's number, which is worse
// than recording nothing at all.
func TestVRAMReportItemsFingerprintComesFromTheMeasuredWindow(t *testing.T) {
	const gib = 1024
	triggerTime := vramSampleOf(vramCard(0, "OLD", "GPU-old", 0, 24*gib))
	before := vramSampleOf(vramCard(0, "NEW", "GPU-new", 100, 24*gib))
	after := vramSampleOf(vramCard(0, "NEW", "GPU-new", 21500, 24*gib))

	items := vramReportItems([]int{0}, true, vramRunPlanned{baseline: triggerTime}, before, after, nil)
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Fingerprint != "GPU-new" {
		t.Fatalf("Fingerprint = %q, want %q -- the identity must come from the sample the number came from",
			items[0].Fingerprint, "GPU-new")
	}
}

// --- F11: a declared GPU index the host does not have -----------------------

// TestVRAMRunPlanRefusesADeclaredGPUTheHostDoesNotHave is F11: the GPU-sample
// precondition only checked that the host reports SOME card, never that the
// spec's declared indexes exist among them. A spec declaring index 3 on a
// 2-GPU host therefore passed every refusal, then made every stability window
// unstable forever -- vramWindowStable refuses a watched card missing from a
// sample -- and reported baseline_unstable, whose stated next action ("retry
// when the server is quiet") can never succeed. The fleet was drained first.
func TestVRAMRunPlanRefusesADeclaredGPUTheHostDoesNotHave(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{
		targetGPUs: []routing.RuntimeSpecGPU{{GPUIndex: 0}, {GPUIndex: 3, VRAMEstimateMB: 18000}},
	})
	f.seedLatestSample()

	_, err := f.srv.vramRunPlan(context.Background(), f.target)
	if got := vramRefusalCode(t, err); got != "benchmark.vram_declared_gpu_missing" {
		t.Fatalf("refusal code = %q, want %q", got, "benchmark.vram_declared_gpu_missing")
	}
	// A refused run writes nothing and notifies nobody -- the whole point of
	// refusing at the trigger rather than reporting inconclusive after the
	// drain.
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
		}
	}
	if got := f.notifies(); len(got) != 0 {
		t.Fatalf("a refused run notified the agent: %#v", got)
	}
}

// --- the wire strings, pinned as literals -----------------------------------

// TestVRAMConfidenceVocabularyIsPinned pins the new values as LITERALS rather
// than against their own constants. Each is persisted verbatim inside a
// kind=="vram" history row and is what the portal switches on to render an
// actionable sentence, so a rename silently reclassifies every stored row and
// leaves the portal with a value it has no message for.
func TestVRAMConfidenceVocabularyIsPinned(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{vramInconclusiveIsolationLost, "isolation_lost"},
		{vramInconclusiveStrategyDisagreement, "strategy_disagreement"},
		{vramEvidenceRestartedDuringRun, "restarted_during_run"},
		{vramWarningUndeclaredGPUAllocation, "undeclared_gpu_allocation"},
		{codeBenchmarkVRAMDeclaredGPUMissing, "benchmark.vram_declared_gpu_missing"},
	} {
		if tc.got != tc.want {
			t.Errorf("vocabulary drift: got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestVRAMEvidenceRestartedDuringRunIsNotIsolation: the third evidence value
// is not a member of the closed set Isolated is allowed to rest on, so
// recording it flips the boolean through the same function that computes it
// rather than by a second assignment somebody can forget.
func TestVRAMEvidenceRestartedDuringRunIsNotIsolation(t *testing.T) {
	evidence := map[string]string{
		"spec_a": vramEvidenceNoProcessAtWrite,
		"spec_b": vramEvidenceRestartedDuringRun,
	}
	if vramIsolationConfirmed([]string{"spec_a", "spec_b"}, evidence) {
		t.Fatal("vramIsolationConfirmed accepted restarted_during_run as isolation evidence")
	}
}
