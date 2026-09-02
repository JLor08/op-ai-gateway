// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- fixture ---------------------------------------------------------------

// vramFakeProvider is the streaming upstream a VRAM run drives. onStream is
// the hook that makes the fixture model reality: the thing that LOADS the
// model is the thing that allocates VRAM, so a test raises the fake GPU's
// used bytes from inside CompleteStream rather than guessing when the load
// happened.
type vramFakeProvider struct {
	mu       sync.Mutex
	calls    int
	streamed []string
	loaded   map[string]bool
	onStream func()
	err      error
}

func (p *vramFakeProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *vramFakeProvider) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	p.mu.Lock()
	p.calls++
	p.streamed = append(p.streamed, req.Model)
	hook, err := p.onStream, p.err
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if hook != nil {
		hook()
	}
	if e := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); e != nil {
		return e
	}
	u := inference.Usage{OutputTokens: 1, TokensPerSecond: 10}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

// LoadedModels makes the fake a provider.LoadedModelLister, so a test can put
// the target model in the loaded set and drive the already-resident branch.
func (p *vramFakeProvider) LoadedModels(context.Context, routing.Target, string, string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.loaded))
	for name := range p.loaded {
		out = append(out, name)
	}
	return out, nil
}

func (p *vramFakeProvider) streamCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type vramFixture struct {
	srv         *Server
	mem         *routing.MemoryStore
	provider    *vramFakeProvider
	notifies    func() []string
	target      benchmarkTarget
	targetSpec  string
	siblingSpec string
	// usedMiB is the fake GPU 0 / GPU 1 used memory the sample driver reports.
	used0, used1 atomic.Int64
	// statuses is what the runtime-status driver publishes; guarded because
	// the driver goroutine reads it while the test writes it.
	statusMu sync.Mutex
	statuses []RuntimeStatusDTO
}

type vramFixtureOpts struct {
	targetAppType     string // defaults to routing.ProviderServerAgent
	targetAdminState  string
	siblingAdminState string
	targetPinned      bool
	siblingPinned     bool
	targetGPUs        []routing.RuntimeSpecGPU
	noSiblingSpec     bool
	siblingDisabled   bool
	noTargetSpec      bool
	extraApplication  *routing.Application
	os                string // reported host OS; "" = linux
}

// shrinkVRAMTimings drives every VRAM run bound down to milliseconds so a
// test exercises the real sequence without sleeping out real-world waits.
func shrinkVRAMTimings(t *testing.T) {
	t.Helper()
	old := []any{
		vramIsolationBindDelayWS, vramIsolationBindDelayPoll, vramIsolationDrainBound,
		vramPhaseSettle, vramPhaseWindowBound, vramMeasuredWaitBound, vramRestoreTimeout,
	}
	vramIsolationBindDelayWS = 10 * time.Millisecond
	vramIsolationBindDelayPoll = 30 * time.Millisecond
	vramIsolationDrainBound = 2 * time.Second
	vramPhaseSettle = 5 * time.Millisecond
	vramPhaseWindowBound = 2 * time.Second
	vramMeasuredWaitBound = 300 * time.Millisecond
	vramRestoreTimeout = 2 * time.Second
	t.Cleanup(func() {
		vramIsolationBindDelayWS = old[0].(time.Duration)
		vramIsolationBindDelayPoll = old[1].(time.Duration)
		vramIsolationDrainBound = old[2].(time.Duration)
		vramPhaseSettle = old[3].(time.Duration)
		vramPhaseWindowBound = old[4].(time.Duration)
		vramMeasuredWaitBound = old[5].(time.Duration)
		vramRestoreTimeout = old[6].(time.Duration)
	})
}

func newVRAMFixture(t *testing.T, opts vramFixtureOpts) *vramFixture {
	t.Helper()
	shrinkVRAMTimings(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()

	appType := opts.targetAppType
	if appType == "" {
		appType = routing.ProviderServerAgent
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateAIServer", mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: appType, Port: 9000, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must("CreateApplication", mem.CreateApplication(ctx, app))
	if opts.extraApplication != nil {
		must("CreateApplication(extra)", mem.CreateApplication(ctx, *opts.extraApplication))
	}
	targetMapping := routing.ModelMapping{ID: "map_target", ApplicationID: app.ID, GatewayModelName: "gw-target", AppModelName: "up-target", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must("CreateMapping(target)", mem.CreateMapping(ctx, targetMapping))
	must("CreateMapping(sibling)", mem.CreateMapping(ctx, routing.ModelMapping{ID: "map_sib", ApplicationID: app.ID, GatewayModelName: "gw-sib", AppModelName: "up-sib", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))

	seedSpec := func(id, mappingID, adminState string, pinned, enabled bool, gpus []routing.RuntimeSpecGPU) {
		t.Helper()
		must("UpsertRuntimeSpec("+id+")", mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: id, MappingID: mappingID, Enabled: enabled, Binary: "/usr/local/bin/llama-server",
			Args: "[]", Env: "{}", HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			Pinned: pinned, AdminState: adminState, CreatedAt: now, UpdatedAt: now,
		}))
		if len(gpus) > 0 {
			for i := range gpus {
				gpus[i].SpecID = id
			}
			must("SetRuntimeSpecGPUs("+id+")", mem.SetRuntimeSpecGPUs(ctx, id, gpus))
		}
	}
	targetGPUs := opts.targetGPUs
	if targetGPUs == nil {
		targetGPUs = []routing.RuntimeSpecGPU{{GPUIndex: 0, VRAMEstimateMB: 18000}}
	}
	if !opts.noTargetSpec {
		seedSpec("rspec_target", "map_target", opts.targetAdminState, opts.targetPinned, true, targetGPUs)
	}
	if !opts.noSiblingSpec {
		seedSpec("rspec_sib", "map_sib", opts.siblingAdminState, opts.siblingPinned, !opts.siblingDisabled, nil)
	}

	hostOS := opts.os
	if hostOS == "" {
		hostOS = "linux"
	}
	must("UpsertTelemetry", mem.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: "srv1", ReportedAt: now, OS: hostOS, GPUCount: 2,
		VRAMTotalBytes: 2 * 24576 * oneMiB, UpdatedAt: now,
	}))

	dir := portal.NewMemoryDirectory(nil)
	portalSvc := portal.NewService(portal.ServiceDeps{
		Users: dir, Groups: dir, Usage: usage.NewRecorder(), Routes: mem,
		Clock: func() time.Time { return now },
	})
	var notifyMu sync.Mutex
	var notified []string
	portalSvc.SetRuntimeConfigChangedHook(func(serverID string) {
		notifyMu.Lock()
		defer notifyMu.Unlock()
		notified = append(notified, serverID)
	})

	fake := &vramFakeProvider{loaded: map[string]bool{}}
	srv := &Server{
		Provider:      fake,
		Routes:        mem,
		Portal:        portalSvc,
		Benchmarks:    NewBenchmarkRegistry(),
		ServerPerf:    NewServerPerfRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
		AgentFeatures: NewAgentFeaturesRegistry(),
		AgentStreams:  NewAgentStreamRegistry(),
	}
	srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})

	f := &vramFixture{
		srv: srv, mem: mem, provider: fake, target: benchmarkTarget{
			server:  routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test"},
			app:     app,
			mapping: targetMapping,
		},
		targetSpec:  "rspec_target",
		siblingSpec: "rspec_sib",
		notifies: func() []string {
			notifyMu.Lock()
			defer notifyMu.Unlock()
			return append([]string(nil), notified...)
		},
	}
	f.used0.Store(500 * oneMiB)
	f.used1.Store(300 * oneMiB)
	// Every spec stopped, from BEFORE the run began -- the state test-plan
	// item 3 requires the run to refuse to read as isolation on its own.
	f.setStatuses(
		RuntimeStatusDTO{SpecID: "rspec_target", State: "stopped"},
		RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
	)
	return f
}

func (f *vramFixture) setStatuses(statuses ...RuntimeStatusDTO) {
	f.statusMu.Lock()
	f.statuses = statuses
	f.statusMu.Unlock()
}

func (f *vramFixture) currentStatuses() []RuntimeStatusDTO {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	return append([]RuntimeStatusDTO(nil), f.statuses...)
}

func (f *vramFixture) sample() routing.TelemetrySample {
	return routing.TelemetrySample{ServerID: "srv1", GPUs: []routing.GPUSample{
		{Index: 0, Name: "NVIDIA RTX 6000", UUID: "GPU-aaa", MemUsedBytes: f.used0.Load(), MemTotalBytes: 24576 * oneMiB},
		{Index: 1, Name: "NVIDIA RTX 6000", UUID: "GPU-bbb", MemUsedBytes: f.used1.Load(), MemTotalBytes: 24576 * oneMiB},
	}}
}

// seedLatestSample publishes one sample so the preconditions (and the
// watched-index resolution) can read a GPU-bearing latest sample without the
// live driver running.
func (f *vramFixture) seedLatestSample() {
	f.srv.ServerPerf.publish(f.sample())
}

// drive publishes the fixture's current sample and runtime-status snapshot on
// both live streams every tick, until the test ends -- what the ~1 s telemetry
// ingest does in production, compressed.
func (f *vramFixture) drive(t *testing.T) {
	t.Helper()
	// Publish once SYNCHRONOUSLY, so a test that reads the registry's state
	// immediately after drive() is not racing the ticker.
	f.srv.ServerPerf.publish(f.sample())
	f.srv.RuntimeStatus.publish("srv1", f.currentStatuses())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				f.srv.ServerPerf.publish(f.sample())
				f.srv.RuntimeStatus.publish("srv1", f.currentStatuses())
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

func (f *vramFixture) run(t *testing.T) BenchmarkStatus {
	t.Helper()
	ctx := context.Background()
	plan, err := f.srv.vramRunPlan(ctx, f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)
	return f.srv.Benchmarks.Status("srv1")
}

func (f *vramFixture) adminState(t *testing.T, specID string) string {
	t.Helper()
	spec, ok, err := f.mem.RuntimeSpecByID(context.Background(), specID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(%q) = (%v, %v)", specID, ok, err)
	}
	return spec.AdminState
}

// countAdminStateWrites counts how many admin_state values differ from "" in
// the whole store -- a cheap "was anything written at all" probe for the
// refusal tests, which must write NOTHING.
func (f *vramFixture) allAdminStates(t *testing.T) map[string]string {
	t.Helper()
	specs, err := f.mem.RuntimeSpecsByApplication(context.Background(), "app1")
	if err != nil {
		t.Fatalf("RuntimeSpecsByApplication: %v", err)
	}
	out := map[string]string{}
	for _, spec := range specs {
		out[spec.ID] = spec.AdminState
	}
	return out
}

func vramRefusalCode(t *testing.T, err error) string {
	t.Helper()
	var refusal *vramRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *vramRefusal", err)
	}
	return refusal.code
}

// --- test-plan item 2: the four preconditions, and NO spec written ---------

// TestVRAMRunPlanRefusesInFileMode is the precondition that exists for a
// concrete defect: in file mode the agent re-reads its own local file and
// never looks at a pushed document, so every admin_state write returns 200
// and stops nothing -- and on a server whose specs are ALL already stopped
// the run would then confirm every one of them without waiting for anything
// and report Isolated: true for a fleet it never touched.
//
// The assertion is the WRITE COUNT, not just the status: that is the half
// that would have caught the original defect.
func TestVRAMRunPlanRefusesInFileMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *vramFixture)
	}{
		{
			name:  "the volatile flag set by the agent's upward report",
			setup: func(f *vramFixture) { f.srv.RuntimeStatus.SetFileMode("srv1", true) },
		},
		{
			// The volatile flag is false for a file-mode server until the
			// first report arrives after a gateway restart, so the DURABLE
			// persisted report is cross-checked too.
			name: "the durable persisted runtime report, after a gateway restart",
			setup: func(f *vramFixture) {
				report, _ := json.Marshal(map[string]any{"source": "file", "collected_at": time.Now().UTC()})
				if err := f.mem.UpsertServerRuntimeReport(context.Background(), routing.ServerRuntimeReport{
					ServerID: "srv1", CollectedAt: time.Now().UTC(), ReportJSON: string(report), UpdatedAt: time.Now().UTC(),
				}); err != nil {
					t.Fatalf("UpsertServerRuntimeReport: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVRAMFixture(t, vramFixtureOpts{})
			f.seedLatestSample()
			tc.setup(f)

			_, err := f.srv.vramRunPlan(context.Background(), f.target)
			if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMIsolationUnavailable {
				t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMIsolationUnavailable)
			}
			for specID, state := range f.allAdminStates(t) {
				if state != "" {
					t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
				}
			}
			if got := f.notifies(); len(got) != 0 {
				t.Fatalf("a refused run notified the agent: %#v", got)
			}
		})
	}
}

// TestVRAMRunPlanRefusesWithoutTheRuntimeManagerFeature is the same gate
// PushRuntimeConfig fail-closes on: an agent that has not declared the
// feature has no runtime driver applying the document at all.
func TestVRAMRunPlanRefusesWithoutTheRuntimeManagerFeature(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.srv.AgentFeatures.Set("srv1", []string{"agent_proxy"})

	_, err := f.srv.vramRunPlan(context.Background(), f.target)
	if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMIsolationUnavailable {
		t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMIsolationUnavailable)
	}
	for specID, state := range f.allAdminStates(t) {
		if state != "" {
			t.Fatalf("spec %q was written (admin_state %q) by a refused run", specID, state)
		}
	}
}

// TestVRAMRunPlanRefusesANonAgentManagedTarget covers the case
// AuthorizeBenchmarkScope lets through: it gates ownership, not application
// type, so a mapping-scope run can name a model on an application the agent
// does not manage. The target's process is then not in the spec set the run
// enumerates, so "the target among them" is simply false -- and the refusal
// must land BEFORE any sibling is drained.
func TestVRAMRunPlanRefusesANonAgentManagedTarget(t *testing.T) {
	t.Run("the owning application is not server_agent", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{targetAppType: routing.ProviderLlamaCPP})
		f.seedLatestSample()
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMNotAgentManaged {
			t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMNotAgentManaged)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("sibling %q was drained (admin_state %q) before the refusal", specID, state)
			}
		}
	})
	t.Run("the target has no enabled launch spec", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{noTargetSpec: true})
		f.seedLatestSample()
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMNotAgentManaged {
			t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMNotAgentManaged)
		}
		if state := f.adminState(t, f.siblingSpec); state != "" {
			t.Fatalf("sibling drained (admin_state %q) for a target with no spec to measure", state)
		}
	})
}

// TestVRAMRunPlanRefusesAGPULessHost is the correction an earlier revision of
// the design got wrong: a host with no GPU collector emits no GPUSample at
// all, so there is nothing to difference -- and a host with no GPU has
// nothing this feature could measure either (a CPU-only model's cost is RAM,
// and this feature claims nothing about RAM).
func TestVRAMRunPlanRefusesAGPULessHost(t *testing.T) {
	t.Run("no sample has ever arrived", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMNoGPUSamples {
			t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMNoGPUSamples)
		}
	})
	t.Run("samples arrive but carry no GPU", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.srv.ServerPerf.publish(routing.TelemetrySample{ServerID: "srv1", CPUUtilPct: 3})
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMNoGPUSamples {
			t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMNoGPUSamples)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("spec %q was drained (admin_state %q) with nothing to measure", specID, state)
			}
		}
	})
}

// --- test-plan item 4: the isolation refusals -----------------------------

// TestVRAMRunPlanRefusesAPreExistingOverride is what makes the restore
// unambiguous: the run only ever restores to "", so it never has to
// reconstruct what an operator's override was -- which, after a gateway
// restart, it could not know. The TARGET's own override counts.
func TestVRAMRunPlanRefusesAPreExistingOverride(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts vramFixtureOpts
	}{
		{"a sibling already carries an override", vramFixtureOpts{siblingAdminState: "force_running"}},
		{"the target itself already carries an override", vramFixtureOpts{targetAdminState: "force_stopped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVRAMFixture(t, tc.opts)
			f.seedLatestSample()
			_, err := f.srv.vramRunPlan(context.Background(), f.target)
			if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMIsolationBlocked {
				t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMIsolationBlocked)
			}
			var refusal *vramRefusal
			_ = errors.As(err, &refusal)
			if refusal.msg == "" {
				t.Fatal("the refusal must NAME the blocking spec")
			}
		})
	}
}

// TestVRAMRunPlanRefusesAPinnedSiblingAndProceedsOnAPinnedTarget: pinned is
// an operator's standing instruction that a model stays up, and silently
// breaking it for a benchmark is a worse surprise than refusing and naming
// it. The TARGET may be pinned -- stopping the target is the point of the run
// -- so that half must proceed.
func TestVRAMRunPlanRefusesAPinnedSiblingAndProceedsOnAPinnedTarget(t *testing.T) {
	t.Run("a pinned sibling is refused", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{siblingPinned: true})
		f.seedLatestSample()
		_, err := f.srv.vramRunPlan(context.Background(), f.target)
		if got := vramRefusalCode(t, err); got != codeBenchmarkVRAMIsolationBlocked {
			t.Fatalf("refusal code = %q, want %q", got, codeBenchmarkVRAMIsolationBlocked)
		}
	})
	t.Run("a pinned target proceeds", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{targetPinned: true})
		f.seedLatestSample()
		plan, err := f.srv.vramRunPlan(context.Background(), f.target)
		if err != nil {
			t.Fatalf("vramRunPlan on a pinned target = %v, want it to proceed", err)
		}
		if plan.targetSpecID != "rspec_target" {
			t.Fatalf("plan.targetSpecID = %q", plan.targetSpecID)
		}
	})
}

// TestVRAMRunPlanEnumeratesEveryEnabledSpecIncludingTheTarget pins D2's
// central choice. The target must be drained too, because the load core
// short-circuits on an already-resident model: leaving the target up would
// make the baseline window already contain the model and yield a DEFINITIVE
// delta of ~0 for the commonest case an operator would probe. A DISABLED
// spec is nothing the agent runs, so it is not part of the fleet to drain.
func TestVRAMRunPlanEnumeratesEveryEnabledSpecIncludingTheTarget(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	plan, err := f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	if len(plan.specIDs) != 2 || plan.specIDs[0] != "rspec_sib" || plan.specIDs[1] != "rspec_target" {
		t.Fatalf("plan.specIDs = %v, want both specs with the target among them", plan.specIDs)
	}

	disabled := newVRAMFixture(t, vramFixtureOpts{siblingDisabled: true})
	disabled.seedLatestSample()
	plan, err = disabled.srv.vramRunPlan(context.Background(), disabled.target)
	if err != nil {
		t.Fatalf("vramRunPlan (disabled sibling): %v", err)
	}
	if len(plan.specIDs) != 1 || plan.specIDs[0] != "rspec_target" {
		t.Fatalf("plan.specIDs = %v, want only the target (a disabled spec is not run)", plan.specIDs)
	}
}

// TestVRAMRunPlanWarnsRatherThanRefusing covers the two conditions that
// degrade confidence without making the run pointless. Refusing on a
// non-managed neighbour would make the feature unusable on exactly the
// migration-path deployments the architecture blesses (llama-swap coexisting
// with the managed runtime), and refusing on a POST-transport agent costs the
// run for the same result -- so both warn and, for the transport, extend the
// bound.
func TestVRAMRunPlanWarnsRatherThanRefusing(t *testing.T) {
	now := time.Now().UTC()
	f := newVRAMFixture(t, vramFixtureOpts{extraApplication: &routing.Application{
		ID: "app_swap", ServerID: "srv1", Type: routing.ProviderLlamaCPP, Port: 8100, Scheme: "http",
		TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}})
	f.seedLatestSample()

	plan, err := f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	if !containsString(plan.warnings, vramWarningNonManagedApplications) {
		t.Fatalf("warnings = %v, want %q", plan.warnings, vramWarningNonManagedApplications)
	}
	// No open agent WebSocket: the push binds only on the agent's runtime
	// poll, so the bound must cover that interval.
	if !containsString(plan.warnings, vramWarningPostTransportAgent) {
		t.Fatalf("warnings = %v, want %q", plan.warnings, vramWarningPostTransportAgent)
	}
	if plan.bindDelay != vramIsolationBindDelayPoll {
		t.Fatalf("bindDelay = %v, want the poll interval %v", plan.bindDelay, vramIsolationBindDelayPoll)
	}

	// With a connected agent the push is immediate and the bound need only
	// cover the drain.
	f.srv.AgentStreams.add("srv1", &agentStreamConn{out: make(chan []byte, 1)})
	plan, err = f.srv.vramRunPlan(context.Background(), f.target)
	if err != nil {
		t.Fatalf("vramRunPlan (WS): %v", err)
	}
	if containsString(plan.warnings, vramWarningPostTransportAgent) {
		t.Fatalf("warnings = %v, want no transport warning for a WS-connected agent", plan.warnings)
	}
	if plan.bindDelay != vramIsolationBindDelayWS {
		t.Fatalf("bindDelay = %v, want the WS delay %v", plan.bindDelay, vramIsolationBindDelayWS)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- test-plan items 3 and 4: the isolation wait --------------------------

// TestVRAMIsolationConfirmsAnIdleSiblingWithoutAnyTransition is the case the
// naive sequence times out on, and it is written first for that reason: a
// force_stopped write against a spec with NO LIVE PROCESS does nothing at all
// -- no state change, no frame -- so a bounded wait for a `stopped`
// TRANSITION never completes for an idle sibling. Every spec IS present in
// every status frame, so its STATE is readable; there is simply no edge.
func TestVRAMIsolationConfirmsAnIdleSiblingWithoutAnyTransition(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	// The target is up, the sibling has been idle all along.
	f.setStatuses(
		RuntimeStatusDTO{SpecID: "rspec_target", State: "running", PID: 4242},
		RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
	)
	f.drive(t)

	live := f.srv.vramLiveProcessBySpec("srv1")
	if !live["rspec_target"] || live["rspec_sib"] {
		t.Fatalf("live-at-write = %v, want only the target live", live)
	}
	// The drain lands; the target's process goes away, the sibling never had
	// one to lose.
	f.setStatuses(
		RuntimeStatusDTO{SpecID: "rspec_target", State: "stopped"},
		RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
	)

	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_sib", "rspec_target"}, live, vramIsolationBindDelayWS)
	if !ok {
		t.Fatalf("isolation timed out; evidence = %v", evidence)
	}
	if evidence["rspec_target"] != vramEvidenceStoppedAfterWrite {
		t.Fatalf("target evidence = %q, want %q", evidence["rspec_target"], vramEvidenceStoppedAfterWrite)
	}
	if evidence["rspec_sib"] != vramEvidenceNoProcessAtWrite {
		t.Fatalf("idle sibling evidence = %q, want %q", evidence["rspec_sib"], vramEvidenceNoProcessAtWrite)
	}
	if !vramIsolationConfirmed([]string{"rspec_sib", "rspec_target"}, evidence) {
		t.Fatal("both specs carry this run's own evidence, so isolation must be confirmed")
	}
}

// TestVRAMIsolationIgnoresAStaleStoppedFrame is test-plan item 3: a status
// snapshot that reported every spec stopped from BEFORE the run began is not
// evidence of anything this run did. The run reads only frames PUBLISHED
// AFTER it subscribed -- which is after its own write -- and for a spec that
// had no process it additionally waits out the transport's binding delay,
// because otherwise it would claim a refusal-to-start the agent has not yet
// been told about.
func TestVRAMIsolationIgnoresAStaleStoppedFrame(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	// A pre-run snapshot sits in the registry. Nothing else is ever
	// published, so the only "stopped everywhere" evidence available is
	// stale -- and the wait must therefore exhaust.
	f.srv.RuntimeStatus.publish("srv1", f.currentStatuses())

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 60 * time.Millisecond
	defer func() { vramIsolationDrainBound = oldBound }()

	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_sib", "rspec_target"}, map[string]bool{}, vramIsolationBindDelayWS)
	if ok {
		t.Fatalf("a stale snapshot was accepted as this run's own evidence: %v", evidence)
	}
	if len(evidence) != 0 {
		t.Fatalf("evidence = %v, want none", evidence)
	}
	if vramIsolationConfirmed([]string{"rspec_sib", "rspec_target"}, evidence) {
		t.Fatal("Isolated must be false without this run's own evidence")
	}
}

// TestVRAMIsolationWaitsOutTheBindingDelayBeforeClaimingNoProcess is the
// other half of item 3: a no-process spec is confirmed ONLY after the
// transport's binding delay has elapsed. On the POST transport the override
// binds up to a whole runtime-poll interval later, and until then
// "force_stopped refuses its restart" is a claim about a document the agent
// has not read.
func TestVRAMIsolationWaitsOutTheBindingDelayBeforeClaimingNoProcess(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)

	bind := 120 * time.Millisecond
	start := time.Now()
	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_sib", "rspec_target"}, map[string]bool{}, bind)
	elapsed := time.Since(start)
	if !ok {
		t.Fatalf("isolation timed out; evidence = %v", evidence)
	}
	if elapsed < bind {
		t.Fatalf("no_process_at_write recorded after %v, before the %v binding delay elapsed", elapsed, bind)
	}
	for _, specID := range []string{"rspec_sib", "rspec_target"} {
		if evidence[specID] != vramEvidenceNoProcessAtWrite {
			t.Fatalf("%s evidence = %q, want %q", specID, evidence[specID], vramEvidenceNoProcessAtWrite)
		}
	}
}

// TestVRAMIsolationTimesOutOnASpecStillRunning: a spec still running at the
// bound is a genuine isolation timeout, and the run must say so rather than
// measure through it.
func TestVRAMIsolationTimesOutOnASpecStillRunning(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.setStatuses(
		RuntimeStatusDTO{SpecID: "rspec_target", State: "stopped"},
		RuntimeStatusDTO{SpecID: "rspec_sib", State: "running", PID: 77},
	)
	f.drive(t)

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 80 * time.Millisecond
	defer func() { vramIsolationDrainBound = oldBound }()

	evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_sib", "rspec_target"}, map[string]bool{"rspec_sib": true}, vramIsolationBindDelayWS)
	if ok {
		t.Fatalf("a still-running sibling was reported isolated: %v", evidence)
	}
	// The target IS confirmed -- partial evidence is still recorded, so the
	// report can be audited -- but the set is not confirmed.
	if evidence["rspec_target"] != vramEvidenceNoProcessAtWrite {
		t.Fatalf("target evidence = %q", evidence["rspec_target"])
	}
	if vramIsolationConfirmed([]string{"rspec_sib", "rspec_target"}, evidence) {
		t.Fatal("Isolated must be false while one spec is still running")
	}
}

// TestVRAMIsolationRefusesAnUnrecognizedState is the fail-closed direction of
// the state mirror: the gateway carries a copy of the agent's closed state
// set, and a state it does not recognize (a future agent build) must never
// count as "no process" -- the run reports an isolation timeout instead of
// claiming an isolation it cannot justify.
func TestVRAMIsolationRefusesAnUnrecognizedState(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.setStatuses(RuntimeStatusDTO{SpecID: "rspec_target", State: "hibernating"})
	f.drive(t)

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 60 * time.Millisecond
	defer func() { vramIsolationDrainBound = oldBound }()

	if evidence, ok := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{"rspec_target"}, map[string]bool{}, vramIsolationBindDelayWS); ok {
		t.Fatalf("an unrecognized state was accepted as evidence: %v", evidence)
	}
}

// --- test-plan item 6: the restore ----------------------------------------

// TestVRAMRestoreRunsOnACancelledRunContext asserts the property directly,
// rather than trusting a `defer`: the run body's context is cancelled exactly
// when the restore matters most (the run finished, or the operator cancelled
// it), so the restore must not be on it.
func TestVRAMRestoreRunsOnACancelledRunContext(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(context.Background(), f.targetSpec, "", "force_stopped"); err != nil {
		t.Fatalf("seed the override: %v", err)
	}
	cancel()

	if failed := f.srv.vramRestore(ctx, []string{f.targetSpec}); len(failed) != 0 {
		t.Fatalf("restore on a cancelled context reported %v as failed", failed)
	}
	if state := f.adminState(t, f.targetSpec); state != "" {
		t.Fatalf("admin_state after the restore = %q, want empty", state)
	}
}

// TestVRAMRestoreReReadsSoAnOperatorEditSurvives is the trap the architecture
// records for the assembled-body write: a full-document replace of a spec
// captured BEFORE the run reverts every field an operator edited DURING it --
// and a launch spec is exactly what an operator opens while a model is
// stopped.
func TestVRAMRestoreReReadsSoAnOperatorEditSurvives(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(ctx, f.targetSpec, "", "force_stopped"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The operator edits the launch spec while the model is stopped.
	spec, _, err := f.mem.RuntimeSpecByID(ctx, f.targetSpec)
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	spec.Binary = "/opt/llama-new/llama-server"
	spec.IdleTimeoutSeconds = 4242
	if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("operator edit: %v", err)
	}

	if failed := f.srv.vramRestore(ctx, []string{f.targetSpec}); len(failed) != 0 {
		t.Fatalf("restore reported %v as failed", failed)
	}
	after, _, err := f.mem.RuntimeSpecByID(ctx, f.targetSpec)
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	if after.AdminState != "" {
		t.Fatalf("admin_state = %q, want empty", after.AdminState)
	}
	if after.Binary != "/opt/llama-new/llama-server" || after.IdleTimeoutSeconds != 4242 {
		t.Fatalf("the restore reverted the operator's edit: binary %q, idle %d", after.Binary, after.IdleTimeoutSeconds)
	}
}

// TestVRAMRestoreReportsFailureWhenSomebodyElseOwnsTheField: if the
// freshly-read admin_state is no longer the force_stopped this run wrote, do
// not write at all -- and say which spec was left that way, because the
// operator is the one who has to sort it out.
func TestVRAMRestoreReportsFailureWhenSomebodyElseOwnsTheField(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(ctx, f.targetSpec, "", "force_stopped"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(ctx, f.targetSpec, "force_stopped", "force_running"); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	failed := f.srv.vramRestore(ctx, []string{f.targetSpec})
	if len(failed) != 1 || failed[0] != f.targetSpec {
		t.Fatalf("restore_failed = %v, want [%s]", failed, f.targetSpec)
	}
	if state := f.adminState(t, f.targetSpec); state != "force_running" {
		t.Fatalf("admin_state = %q, want the takeover's force_running left alone", state)
	}
}

// TestVRAMRestoreTreatsADeletedSpecAsRestored: a spec deleted mid-run took
// its override with it, so it is not something an operator must clear by hand.
func TestVRAMRestoreTreatsADeletedSpecAsRestored(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	if failed := f.srv.vramRestore(context.Background(), []string{"rspec_gone"}); len(failed) != 0 {
		t.Fatalf("restore_failed = %v, want none for a deleted spec", failed)
	}
}

// --- test-plan items 7, 8, 9: the whole run -------------------------------

// TestVRAMRunReportsADefinitiveDelta is the happy path, end to end, and it
// pins the four things that make the number trustworthy: the drain happened
// with this run's own evidence, exactly ONE streaming request loaded the
// model, the post-load window opened after it, and every override was
// restored.
func TestVRAMRunReportsADefinitiveDelta(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	// The load allocates on the card the spec declares.
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	status := f.run(t)
	if status.Running {
		t.Fatal("the run did not finish")
	}
	if len(status.Results) != 1 {
		t.Fatalf("results = %#v", status.Results)
	}
	res := status.Results[0]
	if res.Error != "" {
		t.Fatalf("res.Error = %q", res.Error)
	}
	report := res.VRAM
	if report == nil {
		t.Fatal("res.VRAM = nil, want a report")
	}
	if report.Inconclusive != "" {
		t.Fatalf("Inconclusive = %q, want a definitive result", report.Inconclusive)
	}
	if !report.Isolated {
		t.Fatalf("Isolated = false; evidence = %v", report.IsolationEvidence)
	}
	if len(report.DrainedSpecIDs) != 2 {
		t.Fatalf("DrainedSpecIDs = %v, want both specs", report.DrainedSpecIDs)
	}
	if len(report.RestoreFailed) != 0 {
		t.Fatalf("RestoreFailed = %v, want none", report.RestoreFailed)
	}
	if len(report.GPUs) != 1 || report.GPUs[0].Index != 0 {
		t.Fatalf("GPUs = %#v, want exactly the declared index 0", report.GPUs)
	}
	item := report.GPUs[0]
	if item.DeltaMB != 21000 {
		t.Fatalf("DeltaMB = %d, want 21000", item.DeltaMB)
	}
	if item.BaselineUsedMB != 500 {
		t.Fatalf("BaselineUsedMB = %d, want 500", item.BaselineUsedMB)
	}
	if !item.Attributable {
		t.Fatal("a declared GPU row must be attributable")
	}
	if item.FingerprintKind != vramFingerprintUUID || item.Fingerprint != "GPU-aaa" {
		t.Fatalf("fingerprint = (%q, %q)", item.Fingerprint, item.FingerprintKind)
	}
	if item.UnifiedMemory {
		t.Fatal("a linux/NVIDIA host must not be labelled unified memory")
	}

	// One load, one generation: the regression guard for the removed
	// second-generation step.
	if got := f.provider.streamCount(); got != 1 {
		t.Fatalf("upstream streaming requests = %d, want exactly 1", got)
	}
	// Every override restored, target included.
	for _, specID := range []string{f.targetSpec, f.siblingSpec} {
		if state := f.adminState(t, specID); state != "" {
			t.Fatalf("%s admin_state after the run = %q, want empty", specID, state)
		}
	}
	// A kind=="vram" history row was recorded, carrying the payload in its
	// own column.
	rows, err := f.mem.BenchmarkRunsByMapping(context.Background(), "map_target", 10)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != benchmarkKindVRAM || rows[0].VRAMJSON == "" {
		t.Fatalf("history rows = %#v", rows)
	}
	if rows[0].CapacityCurve != "" {
		t.Fatalf("a VRAM payload leaked into capacity_curve: %q", rows[0].CapacityCurve)
	}
}

// TestVRAMRunOwnershipGuard is test-plan item 11, named after the rule it
// protects: after a FULL VRAM run, the spec's vram_measured_mb and
// vram_estimate_mb are both unchanged. The run reports a number; the operator
// applies it. vram_measured_mb is agent-owned and feeds admission arithmetic
// as the spec's own declared demand -- a gateway-computed delta that
// overshoots would refuse every future start of a model that had been
// working, terminally, with no operator action having occurred.
func TestVRAMRunOwnershipGuard(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }
	if err := f.mem.UpdateRuntimeSpecGPUMeasured(ctx, f.targetSpec, 0, 19750); err != nil {
		t.Fatalf("seed the agent's own measurement: %v", err)
	}

	status := f.run(t)
	if got := status.Results[0].VRAM; got == nil || got.Inconclusive != "" {
		t.Fatalf("expected a definitive run, got %#v", got)
	}

	gpus, err := f.mem.RuntimeSpecGPUs(ctx, f.targetSpec)
	if err != nil {
		t.Fatalf("RuntimeSpecGPUs: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("gpus = %#v", gpus)
	}
	if gpus[0].VRAMMeasuredMB != 19750 {
		t.Fatalf("vram_measured_mb = %d, want the agent's own 19750 UNCHANGED", gpus[0].VRAMMeasuredMB)
	}
	if gpus[0].VRAMEstimateMB != 18000 {
		t.Fatalf("vram_estimate_mb = %d, want the operator's own 18000 UNCHANGED", gpus[0].VRAMEstimateMB)
	}
}

// TestVRAMRunAlreadyResidentIsContaminationNotAShortcut: after the drain was
// confirmed, a model that STILL reports resident is being served by something
// the gateway did not stop. The load core would return "loaded" without
// loading, the baseline would already contain the model, and the delta would
// be a definitive ~0 -- so the run reports inconclusive and says why, and
// reports NO delta.
func TestVRAMRunAlreadyResidentIsContaminationNotAShortcut(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.target.app.LoadedModelsPath = "/loaded"
	f.provider.loaded["up-target"] = true

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil {
		t.Fatal("VRAM = nil, want a report carrying the reason")
	}
	if report.Inconclusive != vramInconclusiveAlreadyResident {
		t.Fatalf("Inconclusive = %q, want %q", report.Inconclusive, vramInconclusiveAlreadyResident)
	}
	for _, item := range report.GPUs {
		if item.DeltaMB != 0 {
			t.Fatalf("an inconclusive run reported a delta: %#v", item)
		}
	}
	if got := f.provider.streamCount(); got != 0 {
		t.Fatalf("streaming requests = %d, want 0 (the model was already resident)", got)
	}
}

// TestVRAMRunBelowFloorIsInconclusive: no model costs ~0 MB, so a
// confirmed-resident model whose headline delta is under the noise floor can
// only mean the window missed the allocation or something else absorbed it.
// 0 means UNKNOWN everywhere else in this feature and must mean it here too.
func TestVRAMRunBelowFloorIsInconclusive(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	// The load allocates 8 MiB: real movement, far under the floor.
	f.provider.onStream = func() { f.used0.Store(508 * oneMiB) }

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || report.Inconclusive != vramInconclusiveBelowFloor {
		t.Fatalf("report = %#v, want Inconclusive %q", report, vramInconclusiveBelowFloor)
	}
	for _, item := range report.GPUs {
		if item.DeltaMB != 0 {
			t.Fatalf("an inconclusive run reported a delta: %#v", item)
		}
	}
}

// TestVRAMRunUnstableWindowsAreInconclusive covers the contaminated window:
// a neighbour moving during either phase trips the stability gate, and the
// run reports which phase rather than a number measured through movement.
func TestVRAMRunUnstableWindowsAreInconclusive(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		// A neighbour churns the watched card continuously.
		// A MONOTONIC ramp, not a two-value flip: a flip at the sampler's own
		// period aliases onto one value and looks perfectly stable.
		churn := make(chan struct{})
		go func() {
			for {
				select {
				case <-churn:
					return
				default:
				}
				f.used0.Add(400 * oneMiB)
				time.Sleep(time.Millisecond)
			}
		}()
		t.Cleanup(func() { close(churn) })
		f.drive(t)

		oldBound := vramPhaseWindowBound
		vramPhaseWindowBound = 120 * time.Millisecond
		defer func() { vramPhaseWindowBound = oldBound }()

		status := f.run(t)
		report := status.Results[0].VRAM
		if report == nil || report.Inconclusive != vramInconclusiveBaselineUnstable {
			t.Fatalf("report = %#v, want Inconclusive %q", report, vramInconclusiveBaselineUnstable)
		}
		if got := f.provider.streamCount(); got != 0 {
			t.Fatalf("streaming requests = %d, want 0 (no baseline, no load)", got)
		}
	})
	t.Run("post-load", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.seedLatestSample()
		f.drive(t)
		// The load starts a neighbour churning, so the post-load window
		// never settles.
		churn := make(chan struct{})
		var once sync.Once
		f.provider.onStream = func() {
			f.used0.Store(21500 * oneMiB)
			go func() {
				for {
					select {
					case <-churn:
						return
					default:
					}
					f.used0.Add(400 * oneMiB)
					time.Sleep(time.Millisecond)
				}
			}()
		}
		t.Cleanup(func() { once.Do(func() { close(churn) }) })

		oldBound := vramPhaseWindowBound
		vramPhaseWindowBound = 150 * time.Millisecond
		defer func() { vramPhaseWindowBound = oldBound }()

		status := f.run(t)
		report := status.Results[0].VRAM
		if report == nil || report.Inconclusive != vramInconclusivePostLoadUnstable {
			t.Fatalf("report = %#v, want Inconclusive %q", report, vramInconclusivePostLoadUnstable)
		}
	})
}

// TestVRAMRunNoSamplesMidRunIsInconclusive: GPU samples that STOP arriving
// mid-run are a different failure from a host that never had any (which is
// refused before the run starts), and the operator's next action differs.
func TestVRAMRunNoSamplesMidRunIsInconclusive(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	// Drive ONLY the runtime-status stream, so the isolation completes and
	// then no GPU sample ever arrives.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				f.srv.RuntimeStatus.publish("srv1", f.currentStatuses())
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	oldBound := vramPhaseWindowBound
	vramPhaseWindowBound = 100 * time.Millisecond
	defer func() { vramPhaseWindowBound = oldBound }()

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || report.Inconclusive != vramInconclusiveNoSamples {
		t.Fatalf("report = %#v, want Inconclusive %q", report, vramInconclusiveNoSamples)
	}
}

// TestVRAMRunIsolationTimeoutStillRestores is the named risk's mitigation
// under the one condition that provokes it: the run created the overrides, so
// leaving them on a timeout is strictly worse than clearing them. It aborts
// the measurement, STILL attempts the restore, and reports both facts. This
// is a deliberate divergence from the portal's own start/stop discipline,
// which chose NOT to clear an override on timeout because it cannot tell a
// wedged child from a slow one -- a benchmark can, because it wrote them.
func TestVRAMRunIsolationTimeoutStillRestores(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.setStatuses(RuntimeStatusDTO{SpecID: "rspec_sib", State: "running", PID: 9},
		RuntimeStatusDTO{SpecID: "rspec_target", State: "running", PID: 10})
	f.drive(t)

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 80 * time.Millisecond
	defer func() { vramIsolationDrainBound = oldBound }()

	status := f.run(t)
	res := status.Results[0]
	if res.Error == "" {
		t.Fatal("res.Error is empty, want the isolation timeout reported")
	}
	report := res.VRAM
	if report == nil || report.Inconclusive != vramInconclusiveIsolationTimeout {
		t.Fatalf("report = %#v, want Inconclusive %q", report, vramInconclusiveIsolationTimeout)
	}
	if report.Isolated {
		t.Fatal("Isolated = true after an isolation timeout")
	}
	if len(report.DrainedSpecIDs) != 2 {
		t.Fatalf("DrainedSpecIDs = %v, want the drained set reported so the portal can name it", report.DrainedSpecIDs)
	}
	for _, specID := range []string{f.targetSpec, f.siblingSpec} {
		if state := f.adminState(t, specID); state != "" {
			t.Fatalf("%s admin_state = %q after a timeout, want the override cleared anyway", specID, state)
		}
	}
	if got := f.provider.streamCount(); got != 0 {
		t.Fatalf("streaming requests = %d, want 0 (the measurement was aborted)", got)
	}
}

// TestVRAMRunEveryAdminStateWriteNotifies is test-plan item 5 at the run
// level: one notification per spec on the drain AND one per spec on the
// restore. Without it the drain silently degrades into a 60 s wait, and worse,
// a no-process spec could start mid-measurement because the refusal never
// arrived.
func TestVRAMRunEveryAdminStateWriteNotifies(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	f.run(t)
	if got := f.notifies(); len(got) != 4 {
		t.Fatalf("runtime-changed notifications = %#v, want 4 (two specs drained, two restored)", got)
	}
}

// TestVRAMRunStrategyAReportsOnlyAPostLoadMeasurement is test-plan item 9.
// The stored vram_measured_mb has no timestamp and the write-back skips an
// unchanged value, so polling the store for "a positive value appears" reads
// an arbitrarily old number as this run's result -- while demanding that the
// value CHANGE fails in the normal case where this run measures exactly what
// the last one did. The run therefore reads the measurement off the live
// status stream, and accepts only a value carried by a frame that arrived
// after the load.
func TestVRAMRunStrategyAReportsOnlyAPostLoadMeasurement(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	// A stale stored measurement, and a status stream carrying nothing.
	if err := f.mem.UpdateRuntimeSpecGPUMeasured(ctx, f.targetSpec, 0, 12345); err != nil {
		t.Fatalf("seed the stored measurement: %v", err)
	}
	f.drive(t)
	f.provider.onStream = func() {
		f.used0.Store(21500 * oneMiB)
		// The agent's own per-process measurement starts riding the stream
		// only after the load -- and it reports EXACTLY the stored value, the
		// case a change-detection approach gets wrong.
		f.setStatuses(
			RuntimeStatusDTO{
				SpecID: "rspec_target", State: "running", PID: 5,
				GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 12345}}, MeasuredAt: time.Now().UTC(),
			},
			RuntimeStatusDTO{SpecID: "rspec_sib", State: "stopped"},
		)
	}

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if len(report.GPUs) != 1 || report.GPUs[0].MeasuredMB != 12345 {
		t.Fatalf("GPUs = %#v, want measured_mb 12345 accepted from a post-load frame", report.GPUs)
	}
}

// TestVRAMRunStrategyANeverReportsAStaleStoredValue is the negative half of
// item 9: with no post-load frame carrying a measurement, measured_mb stays 0
// even though the store holds a number. Strategy (a) reports nothing rather
// than something stale.
func TestVRAMRunStrategyANeverReportsAStaleStoredValue(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	if err := f.mem.UpdateRuntimeSpecGPUMeasured(ctx, f.targetSpec, 0, 12345); err != nil {
		t.Fatalf("seed the stored measurement: %v", err)
	}
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if report.GPUs[0].MeasuredMB != 0 {
		t.Fatalf("measured_mb = %d, want 0 -- the stored value is of unknown age", report.GPUs[0].MeasuredMB)
	}
	if report.GPUs[0].DeltaMB != 21000 {
		t.Fatalf("delta_mb = %d, want the delta still reported", report.GPUs[0].DeltaMB)
	}
}

// TestVRAMRunUndeclaredGPUsAreUnattributable: a spec that declares no GPU
// rows has no row a number could be applied to, so the run watches every card
// and reports only the indexes whose delta clears the floor, marked
// unattributable.
func TestVRAMRunUndeclaredGPUsAreUnattributable(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{targetGPUs: []routing.RuntimeSpecGPU{}})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if len(report.GPUs) != 1 || report.GPUs[0].Index != 0 {
		t.Fatalf("GPUs = %#v, want only the card that moved", report.GPUs)
	}
	if report.GPUs[0].Attributable {
		t.Fatal("a run over undeclared cards must mark its result unattributable")
	}
}

// TestVRAMRunLabelsUnifiedMemoryOnApple: on Apple silicon mem_used/mem_total
// are unified SYSTEM memory read from ioreg, not dedicated VRAM. A number
// labelled as VRAM when it is system memory is a wrong number, not a vague
// one, so the label travels with the per-GPU item.
func TestVRAMRunLabelsUnifiedMemoryOnApple(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{os: "darwin"})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

	status := f.run(t)
	report := status.Results[0].VRAM
	if report == nil || len(report.GPUs) == 0 {
		t.Fatalf("report = %#v", report)
	}
	if !report.GPUs[0].UnifiedMemory {
		t.Fatal("a darwin host's figure must be labelled unified memory")
	}
}

// TestVRAMRunAHardErrorAfterTheDrainStillSaysWhy closes the one gap the
// result contract had: a run that stopped on a hard error AFTER it had
// written something must still be reported, because DrainedSpecIDs and
// RestoreFailed are the only place an operator learns which specs the run
// touched -- and a report carrying an EMPTY inconclusive with zero GPUs would
// read as "a definitive result that measured nothing".
func TestVRAMRunAHardErrorAfterTheDrainStillSaysWhy(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	// The upstream refuses the load: the drain already happened, so the run is
	// past the point where a nil report would be the honest answer.
	f.provider.err = errors.New("dial tcp 127.0.0.1:18080: connection refused")

	status := f.run(t)
	res := status.Results[0]
	if res.Error == "" {
		t.Fatal("res.Error is empty, want the upstream failure reported")
	}
	report := res.VRAM
	if report == nil {
		t.Fatal("VRAM = nil: the drained set must still reach the operator")
	}
	if report.Inconclusive != vramInconclusiveRunFailed {
		t.Fatalf("Inconclusive = %q, want %q -- an empty reason reads as a definitive result", report.Inconclusive, vramInconclusiveRunFailed)
	}
	if len(report.GPUs) != 0 {
		t.Fatalf("GPUs = %#v, want none", report.GPUs)
	}
	if len(report.DrainedSpecIDs) != 2 {
		t.Fatalf("DrainedSpecIDs = %v, want both specs named", report.DrainedSpecIDs)
	}
	// The siblings are restored; the target's override was already cleared to
	// load it, so nothing is left force-stopped either way.
	for _, specID := range []string{f.targetSpec, f.siblingSpec} {
		if state := f.adminState(t, specID); state != "" {
			t.Fatalf("%s admin_state = %q after a failed load, want empty", specID, state)
		}
	}
	if len(report.RestoreFailed) != 0 {
		t.Fatalf("RestoreFailed = %v, want none", report.RestoreFailed)
	}
}

// TestStartVRAMProbeEndpoint exercises the HTTP handler, which nothing else
// reaches: the route wiring, the 404-no-leak gate, and the property that makes
// the whole precondition design safe -- a refusal answers a NAMED 409 and
// leaves the server UNRESERVED, so it is never excluded from routing by a run
// that never happened.
func TestStartVRAMProbeEndpoint(t *testing.T) {
	shrinkVRAMTimings(t)
	s := newBenchmarkActiveFixture(t)
	s.Provider = &vramFakeProvider{loaded: map[string]bool{}}
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// A plain upstream application, and an agent-managed one, on the same server.
	must("CreateApplication(plain)", s.Routes.CreateApplication(ctx, routing.Application{ID: "vr_plain", ServerID: baOwnedServer, Type: routing.ProviderMock, Port: 8300, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping(plain)", s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "vr_plain_map", ApplicationID: "vr_plain", GatewayModelName: "gw-plain", AppModelName: "up-plain", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateApplication(agent)", s.Routes.CreateApplication(ctx, routing.Application{ID: "vr_agent", ServerID: baOwnedServer, Type: routing.ProviderServerAgent, Port: 9100, Scheme: "http", TimeoutMS: 600000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping(agent)", s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "vr_agent_map", ApplicationID: "vr_agent", GatewayModelName: "gw-agent", AppModelName: "up-agent", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("UpsertRuntimeSpec", s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: "vr_spec", MappingID: "vr_agent_map", Enabled: true, Binary: "/usr/local/bin/llama-server",
		Args: "[]", Env: "{}", HealthPath: "/health", CreatedAt: now, UpdatedAt: now,
	}))
	s.AgentFeatures.Set(baOwnedServer, []string{"runtime_manager"})

	post := func(mappingID string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/"+mappingID+"/probe-vram", nil)
		req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Error.Code
	}

	// Unknown mapping => AuthorizeBenchmarkScope errors => 404, no leak.
	if code, _ := post("does_not_exist"); code != http.StatusNotFound {
		t.Fatalf("unknown mapping: status = %d, want 404", code)
	}
	// A mapping on a plain upstream application: AuthorizeBenchmarkScope gates
	// ownership, not application type, so this really does reach the runner.
	if code, errCode := post("vr_plain_map"); code != http.StatusConflict || errCode != codeBenchmarkVRAMNotAgentManaged {
		t.Fatalf("plain application: status = %d code = %q, want 409 %q", code, errCode, codeBenchmarkVRAMNotAgentManaged)
	}
	// The agent-managed target, but the server has never reported a GPU.
	if code, errCode := post("vr_agent_map"); code != http.StatusConflict || errCode != codeBenchmarkVRAMNoGPUSamples {
		t.Fatalf("no GPU samples: status = %d code = %q, want 409 %q", code, errCode, codeBenchmarkVRAMNoGPUSamples)
	}
	// THE PROPERTY THAT MATTERS: none of those refusals reserved the server, so
	// it was never excluded from routing, and nothing was written.
	if s.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatal("a refused VRAM probe left the server reserved")
	}
	spec, ok, err := s.Routes.RuntimeSpecByID(ctx, "vr_spec")
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID = (%v, %v)", ok, err)
	}
	if spec.AdminState != "" {
		t.Fatalf("admin_state = %q after refusals, want empty (nothing written)", spec.AdminState)
	}
	// Method wiring: the route accepts POST only.
	getReq := httptest.NewRequest(http.MethodGet, "/api/portal/mappings/vr_agent_map/probe-vram", nil)
	getReq.Header.Set("Authorization", "Bearer "+baOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, getReq)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on probe-vram: status = %d, want 405", rec.Code)
	}
}
