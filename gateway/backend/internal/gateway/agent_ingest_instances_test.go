// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// runtimeConfigReadSpy wraps a portal.API and reports every
// AgentRuntimeConfig derive on reads: the first thing a runtime-config push
// pass does once its gates let it through, so a read is the observable sign
// that a push for that server ran.
type runtimeConfigReadSpy struct {
	portal.API
	reads chan string
}

func (p *runtimeConfigReadSpy) AgentRuntimeConfig(ctx context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error) {
	select {
	case p.reads <- serverID:
	default:
	}
	if p.API == nil {
		return portal.AgentRuntimeConfigDTO{}, nil
	}
	return p.API.AgentRuntimeConfig(ctx, serverID)
}

// spyRuntimeConfigPushes swaps srv.Portal for a runtimeConfigReadSpy over it.
func spyRuntimeConfigPushes(srv *Server) *runtimeConfigReadSpy {
	spy := &runtimeConfigReadSpy{API: srv.Portal, reads: make(chan string, 64)}
	srv.Portal = spy
	return spy
}

// expectPush fails unless a push pass for serverID derives a document within
// a generous bound.
func (p *runtimeConfigReadSpy) expectPush(t *testing.T, step, serverID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-p.reads:
			if got == serverID {
				return
			}
		case <-deadline:
			t.Fatalf("%s: no runtime-config push for %s", step, serverID)
		}
	}
}

// expectNoPush fails if a push pass for serverID derives a document within a
// short window. A push runs at once on its own goroutine, so the window only
// has to cover scheduling.
func (p *runtimeConfigReadSpy) expectNoPush(t *testing.T, step, serverID string) {
	t.Helper()
	window := time.After(200 * time.Millisecond)
	for {
		select {
		case got := <-p.reads:
			if got == serverID {
				t.Fatalf("%s: a runtime-config push ran for %s, want none", step, serverID)
			}
		case <-window:
			return
		}
	}
}

// TestAgentFeatureRuntimeInstancesNamesOneFeature pins the portal's spelling
// of the agent's runtime_instances declaration to this package's: the portal
// builds each agent's document by it, the ingest records it, and two literals
// that drifted apart would push every agent the document of one without it.
func TestAgentFeatureRuntimeInstancesNamesOneFeature(t *testing.T) {
	if portal.AgentFeatureRuntimeInstances != runtimeInstancesFeature {
		t.Fatalf("portal.AgentFeatureRuntimeInstances = %q, runtimeInstancesFeature = %q, want one name",
			portal.AgentFeatureRuntimeInstances, runtimeInstancesFeature)
	}
}

// TestIngestPushesTheRuntimeConfigWhenRuntimeInstancesFlips pins the push that
// follows a change of the agent's runtime_instances declaration: the
// runtime-config document carries a replicable spec's instance settings only
// for an agent that declares it, so the declaration is an input of the
// document, and a sample that changes it pushes the new document at once
// instead of leaving it to the agent's next poll. A sample that makes the
// declaration known pushes too, whatever it declares -- the first sample after
// a gateway start, or the first readable one after a blob that could not be
// read -- because a document that depends on it was held until then. A sample
// that repeats the declaration pushes nothing, and neither does an unreadable
// one: it makes the declaration unknown, which holds the document.
func TestIngestPushesTheRuntimeConfigWhenRuntimeInstancesFlips(t *testing.T) {
	srv := NewTestServer()
	spy := spyRuntimeConfigPushes(srv)
	ingestCaps := func(t *testing.T, capabilities string) {
		t.Helper()
		req, raw := ingestReq(t, `{"host":{"cpu_util_pct":1},"capabilities":`+capabilities+`}`)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	ingest := func(t *testing.T, features string) {
		t.Helper()
		ingestCaps(t, `{"features":[`+features+`]}`)
	}

	ingest(t, `"runtime_manager"`)
	spy.expectPush(t, "a first sample, which makes the declaration known", "mock-host-qwen")
	ingest(t, `"runtime_manager"`)
	spy.expectNoPush(t, "the same declaration again", "mock-host-qwen")
	ingest(t, `"runtime_manager","runtime_instances"`)
	spy.expectPush(t, "runtime_instances declared", "mock-host-qwen")
	ingest(t, `"runtime_manager","runtime_instances"`)
	spy.expectNoPush(t, "the same declaration again", "mock-host-qwen")
	ingest(t, `"runtime_manager"`)
	spy.expectPush(t, "runtime_instances withdrawn", "mock-host-qwen")
	ingestCaps(t, `"not-an-object"`)
	spy.expectNoPush(t, "an unreadable declaration", "mock-host-qwen")
	ingest(t, `"runtime_manager"`)
	spy.expectPush(t, "a readable declaration after an unreadable one", "mock-host-qwen")
}

// instanceIngestStore wraps a MemoryStore and records every write the
// telemetry ingest's write-backs make, so a test can assert what was written
// and how often, not only the final stored state (a fold and an alternating
// pair of writes can end on the same value).
type instanceIngestStore struct {
	*routing.MemoryStore
	mu               sync.Mutex
	gpuWrites        []string
	ramWrites        []string
	startupWrites    []string
	contextWrites    []string
	capabilityWrites int
}

func wrapInstanceIngestStore(srv *Server) *instanceIngestStore {
	st := &instanceIngestStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = st
	return st
}

func (s *instanceIngestStore) record(list *[]string, entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*list = append(*list, entry)
}

func (s *instanceIngestStore) writes(list *[]string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), *list...)
}

func (s *instanceIngestStore) UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex, measuredMB int) error {
	s.record(&s.gpuWrites, fmt.Sprintf("%s/%d=%d", specID, gpuIndex, measuredMB))
	return s.MemoryStore.UpdateRuntimeSpecGPUMeasured(ctx, specID, gpuIndex, measuredMB)
}

func (s *instanceIngestStore) UpdateRuntimeSpecRAMMeasured(ctx context.Context, specID string, measuredMB int) error {
	s.record(&s.ramWrites, fmt.Sprintf("%s=%d", specID, measuredMB))
	return s.MemoryStore.UpdateRuntimeSpecRAMMeasured(ctx, specID, measuredMB)
}

func (s *instanceIngestStore) UpdateRuntimeSpecStartupMeasured(ctx context.Context, specID string, ms int64) error {
	s.record(&s.startupWrites, fmt.Sprintf("%s=%d", specID, ms))
	return s.MemoryStore.UpdateRuntimeSpecStartupMeasured(ctx, specID, ms)
}

func (s *instanceIngestStore) UpdateMappingContextProbe(ctx context.Context, mappingID string, contextSize int, at time.Time) error {
	s.record(&s.contextWrites, fmt.Sprintf("%s=%d", mappingID, contextSize))
	return s.MemoryStore.UpdateMappingContextProbe(ctx, mappingID, contextSize, at)
}

func (s *instanceIngestStore) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	s.mu.Lock()
	s.capabilityWrites++
	s.mu.Unlock()
	return s.MemoryStore.UpsertMappingCapabilities(ctx, mappingID, rows)
}

// seedSiblingRuntimeIngestSpec adds specID beside firstSpecID (seeded by
// seedRuntimeIngestSpec), under the same server_agent application -- a server
// has one -- on a mapping of its own.
func seedSiblingRuntimeIngestSpec(t *testing.T, srv *Server, firstSpecID, specID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	mappingID := "map_" + specID
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{
		ID: mappingID, ApplicationID: "app_" + firstSpecID, GatewayModelName: "runtime-" + specID, AppModelName: "runtime-" + specID,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: specID, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert runtime spec: %v", err)
	}
}

// ingestInstanceRows ingests one telemetry sample for mock-host-qwen whose
// agent declares features (a comma-separated list of JSON strings, or "") and
// reports rows, each one runtime entry's JSON object.
func ingestInstanceRows(t *testing.T, srv *Server, features string, rows ...string) {
	t.Helper()
	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":[` + features + `]},"runtimes":[` + strings.Join(rows, ",") + `]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// instanceRow is one instance's runtime entry of spec specID: instance k is
// addressed runtime-model (k = 0) or runtime-model#k, and extra is a raw
// fragment of further fields (with its leading comma), or "".
func instanceRow(specID string, k int, state, extra string) string {
	model := "runtime-model"
	if k > 0 {
		model = fmt.Sprintf("runtime-model#%d", k)
	}
	return fmt.Sprintf(`{"spec_id":%q,"instance":%d,"model":%q,"state":%q%s}`, specID, k, model, state, extra)
}

// TestIngestFoldsInstanceVRAMToMax pins the VRAM write-back over a replicable
// spec's instance rows: the spec's measured VRAM per GPU is the largest of its
// instances' in one sample, written once, and a sample that measures the same
// writes nothing -- in either row order -- where writing each row in turn
// would rewrite the column twice a second, forever, as the two values took
// turns. When an instance is gone, the fold of the rows left is written.
func TestIngestFoldsInstanceVRAMToMax(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_fold")
	st := wrapInstanceIngestStore(srv)
	zero := instanceRow("rspec_fold", 0, "running", `,"gpus":[{"index":0,"vram_measured_mb":3000}]`)
	one := instanceRow("rspec_fold", 1, "running", `,"gpus":[{"index":0,"vram_measured_mb":3200}]`)

	ingestInstanceRows(t, srv, "", zero, one)
	if got := st.writes(&st.gpuWrites); !slices.Equal(got, []string{"rspec_fold/0=3200"}) {
		t.Fatalf("gpu writes = %v, want exactly [rspec_fold/0=3200], the larger of the two instances", got)
	}
	ingestInstanceRows(t, srv, "", zero, one)
	ingestInstanceRows(t, srv, "", one, zero)
	if got := st.writes(&st.gpuWrites); len(got) != 1 {
		t.Fatalf("gpu writes after two identical samples = %v, want still the one write", got)
	}
	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), "rspec_fold")
	if err != nil || len(gpus) != 1 || gpus[0].VRAMMeasuredMB != 3200 {
		t.Fatalf("stored gpu rows = %+v (err %v), want the measured 3200", gpus, err)
	}
	ingestInstanceRows(t, srv, "", zero)
	if got := st.writes(&st.gpuWrites); !slices.Equal(got, []string{"rspec_fold/0=3200", "rspec_fold/0=3000"}) {
		t.Fatalf("gpu writes after instance 1 went = %v, want the fold of the remaining row, 3000", got)
	}
}

// TestIngestFoldsInstanceRAMToMax is the RAM half of the fold: the spec's
// measured host RAM is the largest of its instances' in one sample, written
// once, and an identical sample in either row order writes nothing.
func TestIngestFoldsInstanceRAMToMax(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ram_fold")
	st := wrapInstanceIngestStore(srv)
	zero := instanceRow("rspec_ram_fold", 0, "running", `,"ram_measured_mb":5000`)
	one := instanceRow("rspec_ram_fold", 1, "running", `,"ram_measured_mb":6000`)

	ingestInstanceRows(t, srv, "", zero, one)
	ingestInstanceRows(t, srv, "", one, zero)
	ingestInstanceRows(t, srv, "", zero, one)
	if got := st.writes(&st.ramWrites); !slices.Equal(got, []string{"rspec_ram_fold=6000"}) {
		t.Fatalf("ram writes = %v, want exactly [rspec_ram_fold=6000]", got)
	}
	if got := storedRAM(t, srv, "rspec_ram_fold").RAMMeasuredMB; got != 6000 {
		t.Fatalf("stored RAMMeasuredMB = %d, want 6000", got)
	}
}

// TestIngestStartupWrittenOnTenPercentChange pins the startup write-back: the
// spec's startup_measured_ms is the largest non-zero startup_ms of its
// instance rows in the sample, written only when it moved by more than 10 %
// of the stored value, or when nothing is stored yet. The measurement is an
// input of a replicable spec's pushed idle timeout, so writing every jitter
// would push the agent a new document for nothing.
func TestIngestStartupWrittenOnTenPercentChange(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_startup")
	seedSiblingRuntimeIngestSpec(t, srv, "rspec_startup", "rspec_unmeasured")
	ctx := context.Background()
	if err := srv.Routes.UpdateRuntimeSpecStartupMeasured(ctx, "rspec_startup", 10000); err != nil {
		t.Fatalf("seed startup: %v", err)
	}
	st := wrapInstanceIngestStore(srv)
	sample := func(zeroMS, oneMS int) {
		t.Helper()
		ingestInstanceRows(t, srv, "",
			instanceRow("rspec_startup", 0, "running", fmt.Sprintf(`,"startup_ms":%d`, zeroMS)),
			instanceRow("rspec_startup", 1, "running", fmt.Sprintf(`,"startup_ms":%d`, oneMS)))
	}

	sample(10500, 9000) // 5 % above the stored 10000
	if got := st.writes(&st.startupWrites); len(got) != 0 {
		t.Fatalf("startup writes after 10500 = %v, want none (5 %% is jitter)", got)
	}
	sample(11000, 0) // exactly 10 %: not more than 10 %
	if got := st.writes(&st.startupWrites); len(got) != 0 {
		t.Fatalf("startup writes after 11000 = %v, want none (exactly 10 %% is not more)", got)
	}
	sample(9000, 11500) // 15 % above, from instance 1
	if got := st.writes(&st.startupWrites); !slices.Equal(got, []string{"rspec_startup=11500"}) {
		t.Fatalf("startup writes after 11500 = %v, want [rspec_startup=11500]", got)
	}
	sample(11000, 0) // 4 % below the stored 11500
	sample(0, 0)     // nothing measured
	if got := st.writes(&st.startupWrites); len(got) != 1 {
		t.Fatalf("startup writes = %v, want still the one: jitter and an unmeasured sample write nothing", got)
	}
	spec, ok, err := srv.Routes.RuntimeSpecByID(ctx, "rspec_startup")
	if err != nil || !ok || spec.StartupMeasuredMS != 11500 {
		t.Fatalf("stored startup = %d (ok=%v err=%v), want 11500", spec.StartupMeasuredMS, ok, err)
	}

	ingestInstanceRows(t, srv, "", instanceRow("rspec_unmeasured", 0, "running", `,"startup_ms":400`))
	if got := st.writes(&st.startupWrites); !slices.Contains(got, "rspec_unmeasured=400") {
		t.Fatalf("startup writes = %v, want the first measurement of a spec with nothing stored written whatever its size", got)
	}
}

// TestIngestStartupWriteOfAReplicableSpecPushesTheRuntimeConfig pins that a
// startup write the document depends on reaches the agent: a replicable
// spec's pushed idle timeout is derived from startup_measured_ms, which the
// ingest writes outside the portal, so the ingest pushes that server's
// document after the write. A spec that is not replicable pushes nothing --
// its document does not carry the measurement -- and neither does a sample
// that writes nothing.
func TestIngestStartupWriteOfAReplicableSpecPushesTheRuntimeConfig(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_rep")
	seedSiblingRuntimeIngestSpec(t, srv, "rspec_rep", "rspec_single")
	ctx := context.Background()
	rep, _, err := srv.Routes.RuntimeSpecByID(ctx, "rspec_rep")
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	rep.Replicable = true
	if err := srv.Routes.UpsertRuntimeSpec(ctx, rep); err != nil {
		t.Fatalf("make the spec replicable: %v", err)
	}
	spy := spyRuntimeConfigPushes(srv)
	const features = `"runtime_manager","runtime_instances"`

	// The first sample declares runtime_instances, which pushes on its own.
	ingestInstanceRows(t, srv, features)
	spy.expectPush(t, "runtime_instances declared", "mock-host-qwen")

	ingestInstanceRows(t, srv, features, instanceRow("rspec_single", 0, "running", `,"startup_ms":30000`))
	spy.expectNoPush(t, "a startup write of a spec that is not replicable", "mock-host-qwen")
	ingestInstanceRows(t, srv, features, instanceRow("rspec_rep", 0, "running", `,"startup_ms":30000`))
	spy.expectPush(t, "a startup write of a replicable spec", "mock-host-qwen")
	ingestInstanceRows(t, srv, features, instanceRow("rspec_rep", 0, "running", `,"startup_ms":30500`))
	spy.expectNoPush(t, "jitter, which writes nothing", "mock-host-qwen")
}

// TestIngestContextFromInstanceZero pins the context write-back over instance
// rows: the mapping's context size comes from the spec's instance-0 row,
// whatever order the rows arrive in, else from its first running row, and a
// spec with neither -- only a starting burst instance -- writes nothing.
// Writing every row in turn would flip the column between the instances'
// values.
func TestIngestContextFromInstanceZero(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ctx")
	st := wrapInstanceIngestStore(srv)
	const features = `"runtime_model_probe"`

	ingestInstanceRows(t, srv, features,
		instanceRow("rspec_ctx", 1, "running", `,"context_size":4096`),
		instanceRow("rspec_ctx", 0, "running", `,"context_size":8192`))
	if got := st.writes(&st.contextWrites); !slices.Equal(got, []string{"map_rspec_ctx=8192"}) {
		t.Fatalf("context writes = %v, want exactly instance 0's [map_rspec_ctx=8192]", got)
	}
	ingestInstanceRows(t, srv, features,
		instanceRow("rspec_ctx", 2, "starting", `,"context_size":1000`),
		instanceRow("rspec_ctx", 3, "running", `,"context_size":2048`))
	if got := st.writes(&st.contextWrites); !slices.Equal(got, []string{"map_rspec_ctx=8192", "map_rspec_ctx=2048"}) {
		t.Fatalf("context writes = %v, want the first running row's 2048 without an instance-0 row", got)
	}
	ingestInstanceRows(t, srv, features, instanceRow("rspec_ctx", 2, "starting", `,"context_size":1000`))
	if got := st.writes(&st.contextWrites); len(got) != 2 {
		t.Fatalf("context writes = %v, want nothing from a sample with neither an instance-0 nor a running row", got)
	}
}

// TestIngestCapabilitiesFromInstanceZero is the capability half: the verdicts
// written for the mapping are those of the spec's instance-0 row, once, even
// when another instance reports a different one.
func TestIngestCapabilitiesFromInstanceZero(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps")
	st := wrapInstanceIngestStore(srv)
	ingestInstanceRows(t, srv, `"runtime_model_probe"`,
		instanceRow("rspec_caps", 1, "running", `,"live_progress_support":"unsupported"`),
		instanceRow("rspec_caps", 0, "running", `,"live_progress_support":"supported"`))
	st.mu.Lock()
	writes := st.capabilityWrites
	st.mu.Unlock()
	if writes != 1 {
		t.Fatalf("capability writes = %d, want 1 (instance 0's verdicts only)", writes)
	}
	caps, err := srv.Routes.MappingCapabilities(context.Background(), "map_rspec_caps")
	if err != nil {
		t.Fatalf("MappingCapabilities: %v", err)
	}
	if len(caps) != 1 || caps[0].Capability != routing.CapabilityLiveProgress || caps[0].Verdict != routing.CapabilityYes {
		t.Fatalf("capability rows = %+v, want instance 0's live_progress yes", caps)
	}
}

// TestIngestPublishesEveryInstanceRow pins the volatile status registry's
// side: every instance row is published as it came, with its instance index
// and measured startup, and the agent's base spec id on all of them -- the
// fold is only what is written to the store.
func TestIngestPublishesEveryInstanceRow(t *testing.T) {
	srv := NewTestServer()
	ingestInstanceRows(t, srv, "",
		instanceRow("rspec_pub", 0, "running", `,"startup_ms":1500`),
		instanceRow("rspec_pub", 2, "starting", ""))
	got := srv.RuntimeStatus.statusSnapshot("mock-host-qwen")
	if len(got) != 2 {
		t.Fatalf("published rows = %+v, want both instance rows", got)
	}
	if got[0].SpecID != "rspec_pub" || got[0].Instance != 0 || got[0].Model != "runtime-model" || got[0].StartupMS != 1500 {
		t.Fatalf("row 0 = %+v, want spec rspec_pub, instance 0, model runtime-model, startup 1500", got[0])
	}
	if got[1].SpecID != "rspec_pub" || got[1].Instance != 2 || got[1].Model != "runtime-model#2" || got[1].StartupMS != 0 {
		t.Fatalf("row 1 = %+v, want spec rspec_pub, instance 2, model runtime-model#2, no startup", got[1])
	}
}

// TestSpeedBenchmarkLoadTimeOfAReplicableSpecPushesTheRuntimeConfig pins the
// benchmark's half of the same rule: the load time a speed run measures is
// written to the mapping (load_time_ms), which a replicable spec's pushed idle
// timeout is derived from, so a run that measured one for a replicable spec
// pushes the server's document. A spec that is not replicable pushes nothing.
func TestSpeedBenchmarkLoadTimeOfAReplicableSpecPushesTheRuntimeConfig(t *testing.T) {
	for _, replicable := range []bool{true, false} {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}, coldDelay: 300 * time.Millisecond})
		srv := &Server{
			Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry(),
			Routes: routing.NewMemoryStore(), AgentFeatures: NewAgentFeaturesRegistry(),
		}
		srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		spy := spyRuntimeConfigPushes(srv)
		tgt := portalShapedAgentTarget(f)
		tgt.spec.Replicable = replicable

		res := srv.measureSpeedTarget(context.Background(), tgt)
		if res.Error != "" || res.LoadTimeMS <= 0 {
			t.Fatalf("replicable %v: measureSpeedTarget = error %q, load %d ms; want a measured load time", replicable, res.Error, res.LoadTimeMS)
		}
		if replicable {
			spy.expectPush(t, "a replicable spec's load time", "srv1")
		} else {
			spy.expectNoPush(t, "the load time of a spec that is not replicable", "srv1")
		}
	}
}
