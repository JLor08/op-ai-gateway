// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The VRAM run's FAIL-CLOSED guards, and the stable strings its refusals are
// made of. Everything here was verified to survive deletion: the code is
// right, and nothing would have noticed if it stopped being.
//
// They are collected in one file because they share a shape. Each is a branch
// whose whole job is to withhold something -- a spec's isolation evidence, a
// definitive-looking report, a run that cannot achieve what it promises -- in
// a state the happy path never reaches. A test that only drives the happy path
// cannot distinguish "withheld correctly" from "never withheld", so each of
// these drives the state itself.

// runWith drives runVRAMProbe over a plan the caller may adjust -- or over a
// server whose state it changed -- AFTER the plan was computed and BEFORE the
// run body starts. That window is where the volatile facts live: the trigger
// refused on them minutes-to-milliseconds ago, and an agent report can flip
// them in between.
func (f *vramFixture) runWith(t *testing.T, adjust func(plan *vramRunPlanned)) BenchmarkStatus {
	t.Helper()
	ctx := context.Background()
	plan, err := f.srv.vramRunPlan(ctx, f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	if adjust != nil {
		adjust(&plan)
	}
	run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)
	return f.srv.Benchmarks.Status("srv1")
}

// vramOneResult returns the single result a VRAM run produces.
func vramOneResult(t *testing.T, status BenchmarkStatus) BenchmarkResult {
	t.Helper()
	if status.Running {
		t.Fatal("the run did not finish")
	}
	if len(status.Results) != 1 {
		t.Fatalf("results = %#v, want exactly one", status.Results)
	}
	return status.Results[0]
}

// --- the isolation wait's two fail-closed guards ---------------------------

// TestVRAMIsolationRefusesASpecTheAgentNeverReported is the fail-closed
// direction of "present in the frame": a spec the agent has never mentioned --
// a launch spec created moments ago, or an agent restarted and not yet caught
// up -- arrives with present=false and an empty state, and a frame that says
// NOTHING about a spec is not evidence about that spec.
//
// Verified to matter: turning `!present || !vramStateNoProcess(state)` into
// `present && !vramStateNoProcess(state)` inverts the guard into the
// fail-OPEN direction (an absent spec falls straight through to the evidence
// write) and survived the whole suite. Isolated is the one field in this
// feature whose entire contract is that it is evidence.
func TestVRAMIsolationRefusesASpecTheAgentNeverReported(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	// The agent reports the target and has never heard of the sibling.
	f.setStatuses(RuntimeStatusDTO{SpecID: "rspec_target", State: "stopped"})
	f.drive(t)

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 80 * time.Millisecond
	defer func() { vramIsolationDrainBound = oldBound }()

	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_sib", "rspec_target"}, map[string]bool{}, vramIsolationBindDelay)
	if ok {
		t.Fatalf("a spec the agent never reported was accepted as isolated: %v", evidence)
	}
	if got := evidence["rspec_sib"]; got != "" {
		t.Fatalf("absent spec evidence = %q, want none: the frame said nothing about it", got)
	}
	// The reported one IS confirmed -- partial evidence is still recorded so
	// the report can be audited -- but the set is not.
	if evidence["rspec_target"] != vramEvidenceNoProcessAtWrite {
		t.Fatalf("target evidence = %q, want %q", evidence["rspec_target"], vramEvidenceNoProcessAtWrite)
	}
	if vramIsolationConfirmed([]string{"rspec_sib", "rspec_target"}, evidence) {
		t.Fatal("Isolated must be false while one spec carries no evidence at all")
	}
}

// TestVRAMIsolationRefusesAnEmptySpecSet is the same rule at the other end:
// "nothing enumerated, nothing awaited, isolated claimed" is precisely the
// shape of the file-mode defect the whole Isolated contract exists to prevent,
// so an empty set is a TIMEOUT, never a vacuous success.
//
// vramIsolationConfirmed already refuses an empty enumeration, so a run that
// got here would report isolated:false -- but it would still go on to MEASURE
// and to publish a number, which is the outcome the D2.0 refusals exist to
// make impossible. Unreachable today only because vramRunPlan guarantees a
// non-empty target spec id; the guard survived being inverted.
func TestVRAMIsolationRefusesAnEmptySpecSet(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.drive(t)

	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		nil, map[string]bool{}, vramIsolationBindDelay)
	if ok {
		t.Fatalf("an empty spec set was reported as a confirmed isolation: %v", evidence)
	}
	if len(evidence) != 0 {
		t.Fatalf("evidence = %v, want none", evidence)
	}
}

// --- the run body's three withholding branches ----------------------------

// TestVRAMRunRefusesWhenTheAgentWentToFileModeAfterTheTrigger is the run
// body's LAST defence against the file-mode defect, and it was the only one of
// the four D2.0 refusals with no test at all.
//
// IsFileMode and the declared feature set are both written by telemetry
// ingest, so either can flip between the trigger's refusal and the run body --
// and proceeding then drains a fleet whose agent never reads the document,
// while every admin_state write returns 200. On a server whose specs are all
// already in a no-process state the run would confirm every one of them and
// report Isolated: true for a fleet it never touched. Deleting this re-check
// entirely survived the suite.
//
// The assertions are the two that matter: NOTHING was written, and the report
// is NIL -- "the run never reached the measurement phase" is a different
// screen from an inconclusive result, and the operator belongs on the first.
func TestVRAMRunRefusesWhenTheAgentWentToFileModeAfterTheTrigger(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)

	status := f.runWith(t, func(*vramRunPlanned) {
		// The agent's next upward report reveals it is configured from a
		// local file. The trigger could not have known.
		f.srv.RuntimeStatus.SetFileMode("srv1", true)
	})
	res := vramOneResult(t, status)
	if res.VRAM != nil {
		t.Fatalf("VRAM = %#v, want nil: the run never reached the measurement phase", res.VRAM)
	}
	if res.Error == "" {
		t.Fatal("Error is empty, want the file-mode reason")
	}
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q was written (admin_state %q) by a run that refused before the drain", specID, state)
		}
	}
	if got := f.notifies(); len(got) != 0 {
		t.Fatalf("a refused run notified the agent: %#v", got)
	}
	if got := f.provider.streamCount(); got != 0 {
		t.Fatalf("streaming requests = %d, want 0", got)
	}
}

// TestVRAMRunAFailedDrainWriteSaysWhy is the run_failed value's other three
// assignments, and the drain one is the sharpest: a write failure leaves part
// of the fleet force_stopped, so the report MUST still be published -- and an
// EMPTY Inconclusive on a published report is read by the pinned contract as
// "a definitive result", here one with zero GPUs, i.e. a measurement of
// nothing. Deleting the assignment survived the suite, because the only
// covered run_failed path was the load failure.
//
// The realistic trigger is an operator override landing between the
// enumeration and the write: the drain is a compare-and-set against "", so it
// is refused rather than clobbering them.
func TestVRAMRunAFailedDrainWriteSaysWhy(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)

	status := f.runWith(t, func(*vramRunPlanned) {
		// The specs are drained in ascending id order, so the sibling's write
		// lands and the target's is refused -- which is also the interesting
		// shape: something WAS force-stopped and has to be reported.
		if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(context.Background(), f.targetSpec, "", "force_running"); err != nil {
			t.Fatalf("operator override: %v", err)
		}
	})
	res := vramOneResult(t, status)
	if res.VRAM == nil {
		t.Fatal("VRAM = nil: a run that had already force-stopped a spec must report what it touched")
	}
	if res.VRAM.Inconclusive != vramInconclusiveRunFailed {
		t.Fatalf("Inconclusive = %q, want %q: an empty value reads as a definitive result", res.VRAM.Inconclusive, vramInconclusiveRunFailed)
	}
	if res.Error == "" {
		t.Fatal("Error is empty, want the write failure")
	}
	if len(res.VRAM.DrainedSpecIDs) != 1 || res.VRAM.DrainedSpecIDs[0] != f.siblingSpec {
		t.Fatalf("DrainedSpecIDs = %v, want [%s]", res.VRAM.DrainedSpecIDs, f.siblingSpec)
	}
	if len(res.VRAM.GPUs) != 0 {
		t.Fatalf("GPUs = %#v, want none", res.VRAM.GPUs)
	}
	// The one spec it did drain is restored, and the operator's own override
	// is left exactly as they set it.
	if state := f.adminState(t, f.siblingSpec); state != "" {
		t.Fatalf("sibling admin_state = %q, want empty", state)
	}
	if state := f.adminState(t, f.targetSpec); state != "force_running" {
		t.Fatalf("target admin_state = %q, want the operator's own force_running", state)
	}
}

// TestVRAMRunNoWatchedCardIsInconclusiveNotZero is the GPU-less branch inside
// the run body: the preconditions refused a server with no GPU sample, so
// reaching here means the cards VANISHED between the trigger and the
// measurement (an agent restarted onto a host whose driver is gone, a card
// pulled). Nothing to difference is inconclusive, never zero -- and deleting
// the assignment survived, leaving an empty Inconclusive that reads as a
// definitive result with no GPUs.
func TestVRAMRunNoWatchedCardIsInconclusiveNotZero(t *testing.T) {
	// A spec that declares no GPU rows, so the watched set comes from the
	// sample alone.
	f := newVRAMFixture(t, vramFixtureOpts{targetGPUs: []routing.RuntimeSpecGPU{}})
	f.seedLatestSample()
	f.drive(t)

	status := f.runWith(t, func(plan *vramRunPlanned) {
		// The cards are gone by the time the run reads them.
		plan.baseline.GPUs = nil
	})
	res := vramOneResult(t, status)
	if res.VRAM == nil || res.VRAM.Inconclusive != vramInconclusiveNoSamples {
		t.Fatalf("VRAM = %#v, want Inconclusive %q", res.VRAM, vramInconclusiveNoSamples)
	}
	if len(res.VRAM.GPUs) != 0 {
		t.Fatalf("GPUs = %#v, want none", res.VRAM.GPUs)
	}
	// It still says what it force-stopped, and it still put it back.
	if len(res.VRAM.DrainedSpecIDs) == 0 {
		t.Fatal("DrainedSpecIDs is empty: the run drained the fleet and must name it")
	}
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q left at admin_state %q", specID, state)
		}
	}
}

// --- a contamination check that could not be made --------------------------

// TestVRAMRunSaysWhenItCouldNotCheckForContamination is the honesty half of
// the already_resident signal: it is documented as load-bearing ("a
// contamination SIGNAL, not a convenience"), and on most agent-managed
// applications it cannot fire at all.
//
// modelResident answers false when the provider is no LoadedModelLister, when
// the application has no loaded_models_path (operator-entered, with NO
// default -- so it is empty unless somebody filled it in, and the child of a
// server_agent application sits behind the agent's router), when the mapping
// has no app model name, or on any probe error. ensureResidentForRun then
// reported alreadyResident=false, the run measured through the contamination,
// the baseline already contained the model, and the ~0 delta fell out at the
// floor gate as `below_floor` -- whose next action is "the window missed the
// allocation, measure again when the server is quiet", which fails
// identically every time. The reason an operator needed was
// already_resident's: something the gateway cannot stop is serving this model.
//
// A signal that is unavailable must be REPORTED as unavailable, so the run
// carries the caveat rather than letting a wrong reason stand in for it.
func TestVRAMRunSaysWhenItCouldNotCheckForContamination(t *testing.T) {
	t.Run("the target application has no loaded-models probe", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.drive(t)
		f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

		res := vramOneResult(t, f.run(t))
		if res.VRAM == nil {
			t.Fatal("VRAM = nil, want a report")
		}
		if !containsString(res.VRAM.Warnings, vramWarningResidencyUnknown) {
			t.Fatalf("warnings = %v, want %q: the run could not check whether something else was already serving this model", res.VRAM.Warnings, vramWarningResidencyUnknown)
		}
	})

	t.Run("a probe that answered leaves no caveat", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.drive(t)
		// The application has a loaded-models endpoint and it answers: the
		// model is not resident, which is a real answer and not a missing one.
		f.target.app.LoadedModelsPath = "/loaded"
		f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

		res := vramOneResult(t, f.run(t))
		if res.VRAM == nil {
			t.Fatal("VRAM = nil, want a report")
		}
		if containsString(res.VRAM.Warnings, vramWarningResidencyUnknown) {
			t.Fatalf("warnings = %v, want no residency caveat: the probe answered", res.VRAM.Warnings)
		}
	})
}

// --- a cancelled run is a cancelled run ------------------------------------

// TestVRAMRunACancelledRunIsNotAnAgentThatWentAway is the conflation every
// bounded wait in this run produces, and it sent operators to the wrong place.
//
// vramStableWindow returns (sample{}, sawSample, false) on ctx.Done() exactly
// as on its own timer, and its leading sleepCtx returns false on cancellation
// so sawSample is false too; vramAwaitIsolation returns (evidence, false) for
// both as well. The runner then read those as `no_samples` -- documented and
// rendered as "GPU readings stopped arriving during the run. Check the agent
// on this server." -- or as `isolation_timeout`, in both cases with an EMPTY
// Error. An operator who pressed stop on their own run was told to go and
// inspect a telemetry pipeline that was fine.
//
// The context is the only thing that can tell the two apart, so the run asks
// it: run_failed, with the cancellation as the Error the operator reads.
func TestVRAMRunACancelledRunIsNotAnAgentThatWentAway(t *testing.T) {
	t.Run("cancelled before the isolation wait", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.drive(t)
		plan, err := f.srv.vramRunPlan(context.Background(), f.target)
		if err != nil {
			t.Fatalf("vramRunPlan: %v", err)
		}
		run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
		if !ok {
			t.Fatal("TryStart did not start")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)

		res := vramOneResult(t, f.srv.Benchmarks.Status("srv1"))
		if res.VRAM == nil {
			t.Fatal("VRAM = nil: the run had already force-stopped specs")
		}
		if res.VRAM.Inconclusive != vramInconclusiveRunFailed {
			t.Fatalf("Inconclusive = %q, want %q: an isolation the operator cancelled is not an isolation that timed out", res.VRAM.Inconclusive, vramInconclusiveRunFailed)
		}
		if res.Error != context.Canceled.Error() {
			t.Fatalf("Error = %q, want %q", res.Error, context.Canceled.Error())
		}
	})

	t.Run("cancelled at the load, before the post-load window", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.drive(t)
		ctx, cancel := context.WithCancel(context.Background())
		// The stop button, landing as deep into the run as it realistically
		// can: the isolation is confirmed, the baseline is taken, the model is
		// loading.
		f.provider.onStream = func() { cancel() }
		plan, err := f.srv.vramRunPlan(context.Background(), f.target)
		if err != nil {
			t.Fatalf("vramRunPlan: %v", err)
		}
		run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), cancel)
		if !ok {
			t.Fatal("TryStart did not start")
		}
		f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)

		res := vramOneResult(t, f.srv.Benchmarks.Status("srv1"))
		if res.VRAM == nil || res.VRAM.Inconclusive != vramInconclusiveRunFailed {
			t.Fatalf("VRAM = %#v, want Inconclusive %q, not a GPU-sample or stability verdict", res.VRAM, vramInconclusiveRunFailed)
		}
		if res.Error == "" {
			t.Fatal("Error is empty: a run that stopped because it was cancelled must say so")
		}
	})
}

// --- the refusals' wire strings -------------------------------------------

// TestVRAMRefusalCodesAreStableWireStrings pins the four precondition codes as
// LITERALS, which is the only thing that holds them still.
//
// They are stable API error codes: gateway/frontend/src/components/shared/
// format.ts maps each literal to its own i18n key, and they are an operator's
// only signal for a refused run. Every existing assertion compares the
// response against the Go constant, so renaming a constant's VALUE kept the
// whole suite green while the portal fell back to rendering the raw code --
// verified by renaming benchmark.vram_isolation_blocked. This is the same
// discipline TestVRAMResultVocabularyIsPinned applies to the persisted result
// vocabulary, for the same reason.
func TestVRAMRefusalCodesAreStableWireStrings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{codeBenchmarkVRAMNotAgentManaged, "benchmark.vram_not_agent_managed"},
		{codeBenchmarkVRAMIsolationUnavailable, "benchmark.vram_isolation_unavailable"},
		{codeBenchmarkVRAMNoGPUSamples, "benchmark.vram_no_gpu_samples"},
		{codeBenchmarkVRAMIsolationBlocked, "benchmark.vram_isolation_blocked"},
	} {
		if tc.got != tc.want {
			t.Errorf("stable error code drift: got %q, want %q", tc.got, tc.want)
		}
	}
}
