// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// newImagesOnlyVRAMFixture is the VRAM fixture with an images-only target, an
// sd-server child that has no chat endpoint, on an agent that declares the
// given features.
func newImagesOnlyVRAMFixture(t *testing.T, features ...string) *vramFixture {
	t.Helper()
	f := newVRAMFixture(t, vramFixtureOpts{targetAPIFlavors: []string{routing.APIFlavorOpenAIImages}})
	f.srv.AgentFeatures.Set("srv1", features)
	return f
}

// runPlanned runs one VRAM probe with an already-made plan, the way the
// trigger hands the run the plan it made before the reservation.
func (f *vramFixture) runPlanned(t *testing.T, plan vramRunPlanned) BenchmarkStatus {
	t.Helper()
	run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runVRAMProbe(context.Background(), run, "srv1", f.target, plan)
	return f.srv.Benchmarks.Status("srv1")
}

// errorCodeAndStatus is the HTTP status and the apierror code a recorder
// holds.
func errorCodeAndStatus(t *testing.T, rec *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body.Error.Code
}

// TestVRAMRunPlanTakesAnImagesOnlyTargetThroughTheEnsureRoute: the plan
// decides from the target's own spec in the fleet read, and an images-only
// target on an agent with runtime_ensure becomes an ensure plan that carries
// the first-generation caveat from the start.
func TestVRAMRunPlanTakesAnImagesOnlyTargetThroughTheEnsureRoute(t *testing.T) {
	f := newImagesOnlyVRAMFixture(t, "runtime_manager", runtimeEnsureFeature)
	f.seedLatestSample()

	plan, err := f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	if !plan.ensure {
		t.Fatal("plan.ensure = false, want an ensure plan for an images-only target")
	}
	if !containsString(plan.warnings, vramWarningFirstGenerationNotMeasured) {
		t.Fatalf("warnings = %v, want %q", plan.warnings, vramWarningFirstGenerationNotMeasured)
	}
}

// TestVRAMRunPlanRefusesAnImagesOnlyTargetWithoutRuntimeEnsure: without the
// feature the only way to start the child is a chat prompt it cannot answer,
// so the plan refuses before anything is written -- with the Load's own wire
// code for the same missing feature, not a benchmark.vram_* one.
func TestVRAMRunPlanRefusesAnImagesOnlyTargetWithoutRuntimeEnsure(t *testing.T) {
	f := newImagesOnlyVRAMFixture(t, "runtime_manager")
	f.seedLatestSample()

	_, err := f.srv.vramRunPlan(context.Background(), f.target)
	if got := vramRefusalCode(t, err); got != "benchmark.agent_ensure_unsupported" {
		t.Fatalf("refusal code = %q, want benchmark.agent_ensure_unsupported", got)
	}
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
		}
	}
	if got := f.notifies(); len(got) != 0 {
		t.Fatalf("a refused run notified the agent: %#v", got)
	}

	probe := httptest.NewRecorder()
	writeVRAMProbeError(probe, err)
	load := httptest.NewRecorder()
	writeBenchmarkError(load, errBenchmarkAgentEnsureUnsupported)
	probeStatus, probeCode := errorCodeAndStatus(t, probe)
	loadStatus, loadCode := errorCodeAndStatus(t, load)
	if probeStatus != http.StatusConflict || probeStatus != loadStatus || probeCode != loadCode {
		t.Fatalf("VRAM refusal = %d %q, Load refusal = %d %q; want one 409 wire code for both", probeStatus, probeCode, loadStatus, loadCode)
	}
}

// TestVRAMTextProbeNeedsNoRuntimeEnsure: a text target on an agent without
// runtime_ensure is planned, run and loaded by generating exactly as before --
// neither the plan nor the run's re-check asks for the feature.
func TestVRAMTextProbeNeedsNoRuntimeEnsure(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{targetAPIFlavors: []string{routing.APIFlavorOpenAI}})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	plan, err := f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	if plan.ensure || containsString(plan.warnings, vramWarningFirstGenerationNotMeasured) {
		t.Fatalf("plan.ensure = %v, warnings = %v; want a text plan without the caveat", plan.ensure, plan.warnings)
	}
	res := f.runPlanned(t, plan).Results[0]
	if res.Error != "" || res.VRAM == nil || res.VRAM.Inconclusive != "" {
		t.Fatalf("result = %+v, want a definitive text probe", res)
	}
	if f.provider.streamCount() != 1 || f.provider.ensureCount() != 0 {
		t.Fatalf("streams = %d, ensures = %d; want one generation and no ensure", f.provider.streamCount(), f.provider.ensureCount())
	}
}

// TestVRAMRunEnsuresAnImagesOnlyTarget: the run loads an ensure plan's target
// through the ensure route, never by generating, measures it, and reports the
// number with the first-generation caveat.
func TestVRAMRunEnsuresAnImagesOnlyTarget(t *testing.T) {
	f := newImagesOnlyVRAMFixture(t, "runtime_manager", runtimeEnsureFeature)
	f.seedLatestSample()
	f.drive(t)
	f.provider.onEnsure = func() { f.used0.Store(21500 * oneMiB) }

	status := f.run(t)
	res := status.Results[0]
	if res.Error != "" || res.VRAM == nil {
		t.Fatalf("result = %+v, want a report", res)
	}
	report := res.VRAM
	if report.Inconclusive != "" || len(report.GPUs) != 1 || report.GPUs[0].DeltaMB != 21000 {
		t.Fatalf("report = %+v, want a definitive 21000 MB delta", report)
	}
	if !containsString(report.Warnings, vramWarningFirstGenerationNotMeasured) {
		t.Fatalf("warnings = %v, want %q", report.Warnings, vramWarningFirstGenerationNotMeasured)
	}
	if f.provider.streamCount() != 0 || f.provider.ensureCount() != 1 {
		t.Fatalf("streams = %d, ensures = %d; want one ensure and no generation", f.provider.streamCount(), f.provider.ensureCount())
	}
	for _, specID := range []string{f.targetSpec, f.siblingSpec} {
		if state := f.adminState(t, specID); state != "" {
			t.Fatalf("%s admin_state after the run = %q, want empty", specID, state)
		}
	}
}

// TestVRAMRunRechecksRuntimeEnsureBeforeDraining: an agent that stopped
// declaring runtime_ensure between the trigger and the run would leave an
// ensure plan's target with no way to start, after the whole fleet was
// drained -- so the run checks again first, and refuses without writing.
func TestVRAMRunRechecksRuntimeEnsureBeforeDraining(t *testing.T) {
	f := newImagesOnlyVRAMFixture(t, "runtime_manager", runtimeEnsureFeature)
	f.seedLatestSample()
	f.drive(t)
	plan, err := f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil || !plan.ensure {
		t.Fatalf("vramRunPlan = (ensure %v, %v), want an ensure plan", plan.ensure, err)
	}
	f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})

	res := f.runPlanned(t, plan).Results[0]
	if want := "benchmark.agent_ensure_unsupported: " + msgBenchmarkVRAMAgentEnsureUnsupported; res.Error != want {
		t.Fatalf("res.Error = %q, want %q", res.Error, want)
	}
	if res.VRAM != nil {
		t.Fatalf("res.VRAM = %+v, want no report (nothing was drained)", res.VRAM)
	}
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
		}
	}
	if got := f.notifies(); len(got) != 0 {
		t.Fatalf("a refused run notified the agent: %#v", got)
	}
	if f.provider.ensureCount() != 0 || f.provider.streamCount() != 0 {
		t.Fatalf("ensures = %d, streams = %d; want neither", f.provider.ensureCount(), f.provider.streamCount())
	}
}
