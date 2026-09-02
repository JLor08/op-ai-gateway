// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"time"
)

// The measurement phase's bounds. Reasoned, not measured -- vars so tests
// shorten them (the coldLoadPollGap precedent), and each needs validating on
// real hardware before it is treated as settled.
var (
	// vramPhaseSettle is the pause after a phase boundary (the drain, the
	// load) before the stability window starts collecting, so the window does
	// not open on the tail of the transition it is meant to follow.
	vramPhaseSettle = 3 * time.Second
	// vramPhaseWindowBound is how long a phase waits for K consecutive
	// stable samples before giving up and reporting the phase inconclusive.
	vramPhaseWindowBound = 45 * time.Second
	// vramMeasuredWaitBound is how long strategy (a) waits for the agent's
	// OWN per-process measurement to appear on a post-load frame. The agent
	// dispatches a measurement on a child's first health pass and on its
	// housekeeping beat, so this covers a beat or two -- and expiring simply
	// means no measured_mb, never a stale one. A GUESS.
	vramMeasuredWaitBound = 30 * time.Second
)

// errVRAMIsolationTimedOut is the run's own error for an isolation that could
// not be confirmed within the bound. The measurement is abandoned, the
// restore still runs, and both facts are reported.
var errVRAMIsolationTimedOut = errors.New("vram benchmark: isolation timed out")

// runVRAMProbe loads ONE model alone on its server and measures what it
// costs, so an operator can resolve an unknown VRAM demand deliberately
// instead of guessing an estimate or waiting for a per-process measurer that
// does not exist on AMD or Apple hosts.
//
// IT REPORTS A NUMBER AND WRITES NEITHER OF THE TWO IT COULD. Not
// vram_measured_mb: that field is agent-owned and feeds admission arithmetic
// as the spec's own declared demand, where a breach by a MEASURED value is
// terminal -- so a gateway-computed delta that overshot (a neighbour
// allocating inside the window) would refuse every future start of a model
// that had been working, with no operator action having occurred. Not
// vram_estimate_mb either: that one is operator-owned, and putting a machine
// number in a field whose whole meaning is "what the operator declares" takes
// the decision away from them. The run reports; the operator applies.
//
// The sequence, and why each step exists:
//
//  1. re-check the two VOLATILE reachability gates, which an agent report can
//     flip between the trigger and here;
//  2. read which specs have a process to stop, then drain EVERY enabled spec
//     including the target -- a baseline taken while the target is resident
//     measures nothing at all;
//  3. confirm the drain against this run's own evidence, never against a 200
//     from a write;
//  4. baseline: K consecutive stable samples, or inconclusive;
//  5. clear the TARGET's override only, and load it through the shared load
//     core -- which loads BY GENERATING, so a backend that allocates on first
//     use has necessarily already done so before the post-load window opens.
//     There is deliberately no second generation step;
//  6. a target that reports RESIDENT after a confirmed drain is contamination,
//     not a shortcut: something the gateway could not stop is serving it;
//  7. settle, then the same stability gate, then the floor gate;
//  8. restore every override, target included, on a context that is not this
//     run's.
//
// Like runLoadModel and runContextProbe it does NOT call Release: the
// terminal status must LINGER so the frontend's poll can read the result.
func (s *Server) runVRAMProbe(ctx context.Context, run *benchmarkRun, serverID string, tgt benchmarkTarget, plan vramRunPlanned) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	// report is nil until the run has actually written something: nil means
	// "never reached the measurement phase", which is a different message to
	// the operator than a report carrying an Inconclusive reason.
	var report *VRAMReport

	// The terminal defer, registered FIRST so it runs LAST -- after the
	// restore defer below has recorded what it could not clear.
	defer func() {
		if report != nil {
			report.normalizeGPUs()
			res.VRAM = report
		}
		run.addResult(res)
		run.finish(res.Error)
		s.Benchmarks.publish(serverID, run.snapshot())
		// The history row is EVIDENCE, not authority, and it is written on a
		// context that is not the run's own for the same reason the restore
		// is: a cancelled run must still record what it did.
		historyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramHistoryWriteTimeout)
		defer cancel()
		_ = s.Routes.InsertBenchmarkRun(historyCtx, vramHistoryRow(tgt.mapping.ID, serverID, time.Now().UTC(), report, res.Error))
	}()

	// (1) The two volatile gates again. The trigger already refused on them,
	// but IsFileMode and the declared feature set are both written by
	// telemetry ingest, so either can flip in between -- and proceeding then
	// would drain a fleet whose agent never reads the document.
	if reason, unavailable := s.vramIsolationUnavailable(ctx, serverID); unavailable {
		res.Error = reason
		return
	}

	// (2) Which specs even have a process to stop, read BEFORE the write.
	liveAtWrite := s.vramLiveProcessBySpec(serverID)
	drained, drainErr := s.vramDrain(ctx, plan.specIDs)
	// DrainedSpecIDs is the AUDIT set -- everything this run force-stopped,
	// the target included -- because that is what the portal must name if the
	// gateway dies before the restore. pendingRestore is the separate,
	// shrinking set of overrides still to clear: the target leaves it the
	// moment the run clears its override to load it.
	report = &VRAMReport{DrainedSpecIDs: drained, Warnings: plan.warnings}
	pendingRestore := drained
	// The restore defer is registered as soon as anything MIGHT have been
	// written, so it runs on every exit from here on -- including a panic
	// mid-unwind -- and before the terminal defer publishes the report.
	defer func() {
		report.RestoreFailed, report.RestoreTakenOver = s.vramRestore(ctx, pendingRestore)
	}()
	if drainErr != nil {
		// The write itself failed -- a concurrent operator override, a spec
		// deleted or retyped between the enumeration and the write, or a store
		// error. The isolation was never achieved, so there is no number; the
		// report survives only to name what the run had already drained.
		report.Inconclusive = vramInconclusiveRunFailed
		res.Error = drainErr.Error()
		return
	}

	// (3) Isolation is EVIDENCE, never an assumption. A 200 from an
	// admin_state write is not evidence: in file mode every such write
	// returns 200 and stops nothing.
	evidence, confirmed := s.vramAwaitIsolation(ctx, serverID, plan.specIDs, liveAtWrite, plan.bindDelay)
	report.IsolationEvidence = evidence
	report.Isolated = vramIsolationConfirmed(plan.specIDs, evidence)
	if !confirmed {
		if !vramStoppedByCancellation(ctx, report, &res) {
			report.Inconclusive = vramInconclusiveIsolationTimeout
			res.Error = errVRAMIsolationTimedOut.Error()
		}
		return
	}

	// Which cards to read, and whether a number measured on them has a row to
	// be applied to.
	specGPUs, err := s.Routes.RuntimeSpecGPUs(ctx, plan.targetSpecID)
	if err != nil {
		report.Inconclusive = vramInconclusiveRunFailed
		res.Error = err.Error()
		return
	}
	watched, attributable := vramWatchedIndexes(specGPUs, plan.baseline)
	if len(watched) == 0 {
		// The GPU-less case, reached only if the cards vanished between the
		// precondition and here. Nothing to difference is inconclusive, never
		// zero.
		report.Inconclusive = vramInconclusiveNoSamples
		return
	}

	// (4) The baseline window.
	baseline, sawSample, stable := s.vramStableWindow(ctx, serverID, watched)
	if vramStoppedByCancellation(ctx, report, &res) {
		return
	}
	switch {
	case !sawSample:
		report.Inconclusive = vramInconclusiveNoSamples
		return
	case !stable:
		report.Inconclusive = vramInconclusiveBaselineUnstable
		return
	}

	// (5) Clear the TARGET's override only. Its siblings stay force_stopped,
	// so the target starts alone without any admission arithmetic having to
	// be trusted.
	if _, err := s.Portal.SetBenchmarkRuntimeSpecAdminState(ctx, plan.targetSpecID, "force_stopped", ""); err != nil {
		report.Inconclusive = vramInconclusiveRunFailed
		res.Error = err.Error()
		return
	}
	// The target's override is gone, so the deferred restore must not try to
	// clear it again -- the compare-and-set would correctly refuse, and the
	// spec would be reported as a restore failure it is not. DrainedSpecIDs
	// keeps naming it: this run did force-stop it.
	pendingRestore = vramWithout(pendingRestore, plan.targetSpecID)

	alreadyResident, residencyProbed, err := s.ensureResidentForRun(ctx, tgt)
	if !residencyProbed {
		// The contamination check could not be MADE -- no loaded-models probe
		// on this application, or the probe failed. "Not resident" is then an
		// unanswered question rather than a no, and the caveat is the only
		// thing standing between the operator and the wrong next action: an
		// undetected already-resident model surfaces as a sub-floor delta,
		// whose message sends them to retry a run that fails identically.
		report.Warnings = append(report.Warnings, vramWarningResidencyUnknown)
	}
	if err != nil {
		report.Inconclusive = vramInconclusiveRunFailed
		res.Error = err.Error()
		return
	}
	// (6) The resident short-circuit is a contamination SIGNAL. The drain was
	// confirmed, so a model that still reports resident is being served by
	// something this gateway did not stop -- a non-managed application on the
	// same host, most likely. A delta measured against that baseline would be
	// ~0 and definitive, which is worse than no number.
	if alreadyResident {
		report.Inconclusive = vramInconclusiveAlreadyResident
		return
	}

	// (7) The post-load window, then the floor gate.
	after, sawSample, stable := s.vramStableWindow(ctx, serverID, watched)
	if vramStoppedByCancellation(ctx, report, &res) {
		return
	}
	switch {
	case !sawSample:
		report.Inconclusive = vramInconclusiveNoSamples
		return
	case !stable:
		report.Inconclusive = vramInconclusivePostLoadUnstable
		return
	}

	measured := s.vramAwaitMeasured(ctx, serverID, plan.targetSpecID)

	// (8) Isolation again, at the END. Step 3 proved it once, before the
	// baseline, and every window since only ever constrained movement INSIDE
	// itself -- so a sibling started anywhere in between (a portal "Force
	// start", a request straight to the agent's own router) left both windows
	// individually stable and had its whole allocation added to delta_mb.
	lost := s.vramIsolationLost(serverID, plan)
	for _, specID := range lost {
		// evidence IS report.IsolationEvidence, so the stored payload names
		// what broke the isolation rather than only saying that it broke.
		evidence[specID] = vramEvidenceRestartedDuringRun
	}
	// Recomputed through the one function that owns this field, so the boolean
	// and the evidence behind it cannot drift. The lost-set conjunct is what
	// covers a spec created AFTER the enumeration: that one is in no list
	// vramIsolationConfirmed reads.
	report.Isolated = len(lost) == 0 && vramIsolationConfirmed(plan.specIDs, evidence)
	if len(lost) > 0 {
		report.Inconclusive = vramInconclusiveIsolationLost
		return
	}

	// (9) The two numbers, the floor gate, and the cross-check between them.
	items := vramReportItems(watched, attributable, plan, baseline, after, measured)
	if undeclared := vramUndeclaredAllocations(watched, baseline, after, measured); len(undeclared) > 0 {
		// A card the spec does not declare that nonetheless allocated: the
		// per-card numbers stay correct, the MODEL's total was the half being
		// silently halved, and the spec's GPU rows are what the operator has
		// to fix. Reported and warned, not refused -- see the warning's doc.
		items = append(items, vramReportItems(undeclared, false, plan, baseline, after, measured)...)
		report.Warnings = append(report.Warnings, vramWarningUndeclaredGPUAllocation)
	}
	if vramHeadlineDeltaMB(items) < vramFloorMB(watched, after) {
		// No model costs ~0 MB, so a sub-floor headline can only mean the
		// window missed the allocation or something else absorbed it. 0 means
		// UNKNOWN everywhere else in this feature and must mean it here too.
		report.Inconclusive = vramInconclusiveBelowFloor
		report.GPUs = nil
		return
	}
	report.GPUs = vramVisibleItems(items)
	if vramStrategiesDisagree(report.GPUs, after) {
		// The one contaminant no other gate can see: a neighbour the gateway
		// cannot stop, allocating during the load, with the managed fleet
		// provably isolated the whole time. The two disagreeing numbers STAY
		// on the report -- unlike below_floor, where there is nothing to show,
		// here they are the evidence for the reason.
		report.Inconclusive = vramInconclusiveStrategyDisagreement
	}
}

// vramStoppedByCancellation records a run the operator (or the trigger's own
// cancel) ended, and reports whether that is what happened -- so the caller
// stops attributing it to the condition its timer was watching for.
//
// EVERY bounded wait in this run answers ctx.Done() and its own timer
// IDENTICALLY: vramStableWindow returns (sample{}, sawSample, false) for both,
// and its leading sleepCtx returns false on cancellation so sawSample is false
// too; vramAwaitIsolation returns (evidence, false) for both. Read without
// asking the context, a cancellation therefore surfaced as `no_samples` --
// which the vocabulary defines, and the portal renders, as "GPU readings
// stopped arriving during the run, check the agent on this server" -- or as
// `baseline_unstable`/`post_load_unstable`/`isolation_timeout`, each with an
// EMPTY Error. An operator who cancelled their own run was sent to inspect a
// telemetry pipeline that was fine.
//
// run_failed is the right value for it: the run stopped after it had written
// something, so Error is what the operator reads, and the report still has to
// name the fleet it drained. vramAwaitMeasured shares the conflation
// harmlessly -- both of its paths mean "no measured_mb" and neither becomes a
// reason.
func vramStoppedByCancellation(ctx context.Context, report *VRAMReport, res *BenchmarkResult) bool {
	err := ctx.Err()
	if err == nil {
		return false
	}
	report.Inconclusive = vramInconclusiveRunFailed
	res.Error = err.Error()
	return true
}

// vramWithout returns ids with one id removed, preserving order.
func vramWithout(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == drop {
			continue
		}
		out = append(out, id)
	}
	return out
}

// vramReportItems builds the per-GPU result from the two stable windows plus
// whatever the agent's own measurer contributed.
//
// The two numbers are NOT the same quantity and are never averaged: DeltaMB is
// the model's marginal cost on that card (a constant neighbour, the driver
// reserve and ECC overhead all cancel out of it), while MeasuredMB is that
// process's attributed usage as the agent's own measurer reports it. They may
// legitimately differ, and a host without a per-process measurer only ever
// has the delta.
//
// EVERY index handed in is reported, unfiltered: the headline gate is applied
// to the SUM of these deltas, so dropping a card here would decide the floor
// question before the sum that the floor is meant to judge. Trimming the quiet
// unattributable cards back out is vramVisibleItems' job, and it runs AFTER
// that gate.
//
// The FINGERPRINT is read from the post-load window's own sample rather than
// from the trigger-time one, because that is the sample the numbers came from.
// Recording the trigger-time identity beside a window-time number means that
// if the cards were renumbered in between -- the exact event a fingerprint
// exists to detect -- the item pairs the OLD card's uuid with the NEW card's
// figure, which is worse than recording no identity at all.
func vramReportItems(watched []int, attributable bool, plan vramRunPlanned, baseline, after routing.TelemetrySample, measured map[int]int) []VRAMGPUItem {
	beforeByIndex := vramGPUByIndex(baseline)
	afterByIndex := vramGPUByIndex(after)

	items := make([]VRAMGPUItem, 0, len(watched))
	for _, index := range watched {
		delta := vramDeltaMB(beforeByIndex[index].MemUsedBytes, afterByIndex[index].MemUsedBytes)
		fingerprint, kind := vramFingerprintOf(afterByIndex[index])
		items = append(items, VRAMGPUItem{
			Index:           index,
			Fingerprint:     fingerprint,
			FingerprintKind: kind,
			UnifiedMemory:   plan.unifiedMemory,
			BaselineUsedMB:  vramUsedMB(beforeByIndex[index].MemUsedBytes),
			DeltaMB:         delta,
			MeasuredMB:      measured[index],
			Attributable:    attributable,
		})
	}
	return items
}

// vramStableWindow settles, then collects live per-GPU samples until K
// CONSECUTIVE ones agree over every watched card, and returns the last sample
// of that window.
//
// Only samples that arrive on the subscription count -- the ring snapshot is
// discarded, so nothing collected here predates the phase boundary this window
// is measuring. That is the same structural watermark the isolation wait uses:
// subscribe registers under the registry's lock before returning, so a
// delivered sample was published after registration.
//
// The window's LAST sample is the phase's reading rather than a mean. The
// window is stable by construction, so every member is within tolerance of
// every other, and the last one is the closest in time to the phase boundary
// that follows.
//
// sawSample distinguishes the two failures the operator must act on
// differently: samples that STOPPED arriving (the agent went away mid-run)
// versus samples that arrived but never held still (a neighbour moving).
func (s *Server) vramStableWindow(ctx context.Context, serverID string, watched []int) (sample routing.TelemetrySample, sawSample, stable bool) {
	if !sleepCtx(ctx, vramPhaseSettle) {
		return routing.TelemetrySample{}, false, false
	}
	_, samples, unsub := s.ServerPerf.subscribe(serverID)
	defer unsub()

	timer := time.NewTimer(vramPhaseWindowBound)
	defer timer.Stop()
	window := make([]routing.TelemetrySample, 0, vramStabilityWindow)
	for {
		select {
		case <-ctx.Done():
			return routing.TelemetrySample{}, sawSample, false
		case <-timer.C:
			return routing.TelemetrySample{}, sawSample, false
		case fresh, open := <-samples:
			if !open {
				return routing.TelemetrySample{}, sawSample, false
			}
			sawSample = true
			window = append(window, fresh)
			if len(window) > vramStabilityWindow {
				window = window[len(window)-vramStabilityWindow:]
			}
			if vramWindowStable(window, watched) {
				return window[len(window)-1], true, true
			}
		}
	}
}

// vramAwaitMeasured is strategy (a): the agent's OWN per-process measurement
// for the target spec, read off the live status stream.
//
// IT CANNOT COME FROM THE STORE. The stored row is
// {SpecID, GPUIndex, VRAMEstimateMB, VRAMMeasuredMB} with no timestamp, and
// the telemetry write-back deliberately skips an UNCHANGED value -- so
// polling the store for "a positive value appears" reads an arbitrarily old
// number as this run's result, while requiring the value to CHANGE fails in
// the normal case where this run measures exactly what the last one did. The
// status stream carries the same number with the gateway's own arrival time,
// and subscribing here (after the load) means every frame received is one
// that arrived after it.
//
// Reading it crosses no ownership boundary: it is a number the agent
// produced. An empty result is the honest answer on a host with no
// per-process measurer, which is the case the delta strategy exists for --
// strategy (a) reports nothing rather than something stale.
//
// IT RETURNS THE AGENT'S WHOLE PER-CARD MAP, unfiltered by the watched set.
// Filtering it to the spec's declared indexes discarded exactly the reading
// that mattered most: `set_visible_devices` defaults to false, so a model can
// allocate on a card its spec does not declare, and the agent's measurement on
// that card is the strongest available evidence of it -- while dropping it made
// the remaining half agree with the declared-card delta and so reinforced
// confidence in half a number. The caller decides which indexes have a row to
// be applied to; that is not this function's question.
func (s *Server) vramAwaitMeasured(ctx context.Context, serverID, specID string) map[int]int {
	_, frames, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()

	timer := time.NewTimer(vramMeasuredWaitBound)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case frame, open := <-frames:
			if !open {
				return nil
			}
			for _, status := range frame {
				if status.SpecID != specID || status.MeasuredAt.IsZero() {
					continue
				}
				out := map[int]int{}
				for _, gpu := range status.GPUs {
					// A measured 0 is UNKNOWN, the same rule the ingest and
					// the write-back apply to this very array.
					if gpu.VRAMMeasuredMB <= 0 {
						continue
					}
					out[gpu.Index] = gpu.VRAMMeasuredMB
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
}
