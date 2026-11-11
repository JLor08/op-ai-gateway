// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"testing"
	"time"
)

// The member usage tests drive the real clusterUsage over a memory store that
// holds one head (usageHead on usageHeadServer) with two automatic members: a
// GPU member on srv_m0 (GPUs 0 and 1) and a CPU member on srv_m1.

const (
	usageHead       = "rspec_head"
	usageHeadServer = "srv_head"
	usageGPUServer  = "srv_m0"
	usageCPUServer  = "srv_m1"
)

type usageFixture struct {
	t     *testing.T
	ctx   context.Context
	store *routing.MemoryStore
	u     *clusterUsage

	clockMu sync.Mutex
	now     time.Time

	pushMu sync.Mutex
	pushed []string
}

func newUsageFixture(t *testing.T) *usageFixture {
	t.Helper()
	f := &usageFixture{t: t, ctx: context.Background(), store: routing.NewMemoryStore(), now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	seedUsageCluster(t, f.store, f.now)
	f.u = f.newUsage()
	return f
}

// newUsage builds a fresh clusterUsage over the fixture's store: a gateway
// started again.
func (f *usageFixture) newUsage() *clusterUsage {
	return newClusterUsage(f.store, func(serverID string) {
		f.pushMu.Lock()
		defer f.pushMu.Unlock()
		f.pushed = append(f.pushed, serverID)
	}, f.clock)
}

// seedUsageCluster stores the head chain and its two members on s.
func seedUsageCluster(t *testing.T, s routing.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{usageHeadServer, usageGPUServer, usageCPUServer} {
		must(s.CreateAIServer(ctx, routing.AIServer{
			ID: id, Name: id, Domain: id + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + id,
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}))
		must(s.SetServerOwners(ctx, id, []string{"usr_dev"}))
	}
	must(s.CreateApplication(ctx, routing.Application{
		ID: "app_usage_head", ServerID: usageHeadServer, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1, TimeoutMS: 30000, AffinityTTLSeconds: 300,
		Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}))
	must(s.CreateMapping(ctx, routing.ModelMapping{
		ID: "map_usage_head", ApplicationID: "app_usage_head", GatewayModelName: "big", AppModelName: "big",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}))
	must(s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: usageHead, MappingID: "map_usage_head", Enabled: true, Binary: "/opt/llama/llama-server", Args: "[]", Env: "{}",
		ClusterEnabled: true, MemberMode: "automatic", ClusterNetwork: "lan", RPCTransport: "relay", StartupTimeoutSeconds: 600,
		CreatedAt: now, UpdatedAt: now,
	}))
	must(s.SetClusterMembers(ctx, usageHead, []routing.ClusterMember{
		{HeadSpecID: usageHead, Position: 0, ServerID: usageGPUServer, Port: 47001},
		{HeadSpecID: usageHead, Position: 1, ServerID: usageCPUServer, Port: 47001},
	}, []routing.ClusterMemberGPU{
		{HeadSpecID: usageHead, ServerID: usageGPUServer, GPUIndex: 0, Position: 0},
		{HeadSpecID: usageHead, ServerID: usageGPUServer, GPUIndex: 1, Position: 1},
	}))
}

func (f *usageFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *usageFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

func (f *usageFixture) takePushes() []string {
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	out := f.pushed
	f.pushed = nil
	return out
}

func usageMemberSpec(serverID string) string { return "rpca_" + usageHead + "_" + serverID }

// usageRow is a member's status row: held by heldBy ("" none), measuring
// ramMB of RAM and vram per GPU index.
func usageRow(spec, heldBy string, ramMB int, vram map[int]int) RuntimeStatusDTO {
	r := RuntimeStatusDTO{SpecID: spec, State: agentSpecStateRunning, Kind: portal.AgentSpecKindRPCServer, RPC: &RuntimeRPCStatusDTO{HeldBy: heldBy, DeviceCount: 1}, RAMMeasuredMB: ramMB}
	for _, idx := range slices.Sorted(func(yield func(int) bool) {
		for k := range vram {
			if !yield(k) {
				return
			}
		}
	}) {
		r.GPUs = append(r.GPUs, RuntimeGPUStatusDTO{Index: idx, VRAMMeasuredMB: vram[idx]})
	}
	return r
}

// observeGPU reports one sample of the GPU member's server.
func (f *usageFixture) observeGPU(heldBy string, ramMB int, vram map[int]int) {
	f.u.observe(f.ctx, usageGPUServer, []RuntimeStatusDTO{usageRow(usageMemberSpec(usageGPUServer), heldBy, ramMB, vram)})
}

// member returns the stored row of serverID's membership and its GPUs by
// index.
func (f *usageFixture) member(serverID string) (routing.ClusterMember, map[int]routing.ClusterMemberGPU) {
	f.t.Helper()
	members, gpus, err := f.store.ClusterMembers(f.ctx, usageHead)
	if err != nil {
		f.t.Fatal(err)
	}
	i := slices.IndexFunc(members, func(m routing.ClusterMember) bool { return m.ServerID == serverID })
	if i < 0 {
		f.t.Fatalf("no member on %s", serverID)
	}
	byIndex := map[int]routing.ClusterMemberGPU{}
	for _, g := range gpus {
		if g.ServerID == serverID {
			byIndex[g.GPUIndex] = g
		}
	}
	return members[i], byIndex
}

// store writes a stored usage onto serverID's membership directly.
func (f *usageFixture) storeUsage(serverID string, ramMB int, vram map[int]int, stale bool) {
	f.t.Helper()
	m, _ := f.member(serverID)
	m.RAMMeasuredMB, m.Stale, m.MeasuredAt = ramMB, stale, f.clock()
	var gpus []routing.ClusterMemberGPU
	for idx, v := range vram {
		gpus = append(gpus, routing.ClusterMemberGPU{GPUIndex: idx, VRAMMeasuredMB: v})
	}
	if err := f.store.UpdateClusterMemberUsage(f.ctx, m, gpus); err != nil {
		f.t.Fatal(err)
	}
}

func (f *usageFixture) wantStored(serverID string, ramMB int, vram map[int]int) {
	f.t.Helper()
	m, gpus := f.member(serverID)
	if m.RAMMeasuredMB != ramMB {
		f.t.Fatalf("%s stored ram = %d, want %d", serverID, m.RAMMeasuredMB, ramMB)
	}
	for idx, want := range vram {
		if got := gpus[idx].VRAMMeasuredMB; got != want {
			f.t.Fatalf("%s stored vram[%d] = %d, want %d", serverID, idx, got, want)
		}
	}
}

// TestMemberUsageOnlyFromOwningServer: a sample counts only for a member the
// reporting server owns. Another server naming the member's id writes
// nothing, and neither does the server a manual membership row names when the
// manual spec itself belongs to another server.
func TestMemberUsageOnlyFromOwningServer(t *testing.T) {
	f := newUsageFixture(t)
	forged := usageRow(usageMemberSpec(usageGPUServer), usageHead, 9000, map[int]int{0: 20000})
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{forged})
	f.u.observe(f.ctx, usageHeadServer, []RuntimeStatusDTO{forged})
	f.wantStored(usageGPUServer, 0, map[int]int{0: 0})

	// A manual member whose spec lives on srv_m1 while its membership row
	// names srv_m0: neither server's sample counts.
	if err := f.store.UpsertRPCMemberSpec(f.ctx, routing.RPCMemberSpec{ID: "rpcm_moved", ServerID: usageCPUServer, Enabled: true, Binary: "/opt/llama/ggml-rpc-server", Args: "[]", Env: "{}", CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	m, _ := f.member(usageGPUServer)
	members := []routing.ClusterMember{
		{HeadSpecID: usageHead, Position: 0, ServerID: usageGPUServer, MemberSpecID: "rpcm_moved", Port: m.Port},
		{HeadSpecID: usageHead, Position: 1, ServerID: usageCPUServer, Port: 47001},
	}
	if err := f.store.SetClusterMembers(f.ctx, usageHead, members, nil); err != nil {
		t.Fatal(err)
	}
	manual := usageRow("rpcm_moved", usageHead, 7000, nil)
	f.u.observe(f.ctx, usageGPUServer, []RuntimeStatusDTO{manual})
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{manual})
	f.wantStored(usageGPUServer, 0, nil)

	// The owner's own sample counts.
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{usageRow(usageMemberSpec(usageCPUServer), usageHead, 3000, nil)})
	f.wantStored(usageCPUServer, 3000, nil)
}

// TestMemberUsageOnlyWhileHeldByThatHead: the reported hold only gates a
// sample: one without a hold, or with another head's, writes nothing.
func TestMemberUsageOnlyWhileHeldByThatHead(t *testing.T) {
	f := newUsageFixture(t)
	f.observeGPU("", 5000, map[int]int{0: 10000})
	f.observeGPU("rspec_other", 6000, map[int]int{0: 11000})
	f.wantStored(usageGPUServer, 0, map[int]int{0: 0})
	f.observeGPU(usageHead, 4000, map[int]int{0: 9000})
	f.wantStored(usageGPUServer, 4000, map[int]int{0: 9000, 1: 0})
}

// TestMemberUsageRisesDuringRun: during a run the stored value only rises and
// the run's own peak is kept beside it.
func TestMemberUsageRisesDuringRun(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000, 1: 3000}, false)
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 4000, map[int]int{0: 10000, 1: 4000})
	f.observeGPU(usageHead, 6000, map[int]int{0: 9000, 1: 5000})
	f.observeGPU(usageHead, 5000, map[int]int{0: 8000, 1: 4500})
	f.wantStored(usageGPUServer, 8000, map[int]int{0: 12000, 1: 5000})
	m, gpus := f.member(usageGPUServer)
	if m.RunPeakRAMMB != 6000 || gpus[0].RunPeakVRAMMB != 10000 || gpus[1].RunPeakVRAMMB != 5000 {
		t.Fatalf("run peaks = ram %d vram %d/%d, want 6000 and 10000/5000", m.RunPeakRAMMB, gpus[0].RunPeakVRAMMB, gpus[1].RunPeakVRAMMB)
	}
	if m.RunStartedAt.IsZero() {
		t.Fatal("the run's start is not recorded")
	}
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("rises pushed %v, want nothing (the agent charges max(live, demand) itself)", pushed)
	}
}

// TestMemberUsageRunPeakReplacesAtFormedEnd: when a run that formed ends, its
// own peak replaces the stored value -- also a lower one, and a stale one --
// and the run's fields are cleared for the next run.
func TestMemberUsageRunPeakReplacesAtFormedEnd(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000, 1: 6000}, true)
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.advance(time.Minute)
	f.u.runEnded(f.ctx, usageHead, true)
	// GPU 1 measured nothing in this run, so its stored value stays.
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 9000, 1: 6000})
	m, gpus := f.member(usageGPUServer)
	if m.Stale || !m.MeasuredAt.Equal(f.clock()) {
		t.Fatalf("after the formed end: stale %v measured_at %v, want fresh at %v", m.Stale, m.MeasuredAt, f.clock())
	}
	if !m.RunStartedAt.IsZero() || m.RunPeakRAMMB != 0 || gpus[0].RunPeakVRAMMB != 0 {
		t.Fatalf("run fields after the end = %+v %+v, want cleared", m, gpus)
	}
	// The CPU member held nothing in this run: its row is untouched.
	f.wantStored(usageCPUServer, 0, nil)
}

// TestMemberUsageUnformedRunKeepsRises: a run that never formed replaces
// nothing: what it raised stays, and a higher stored value is not lowered.
func TestMemberUsageUnformedRunKeepsRises(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 3000, map[int]int{0: 20000}, false)
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, false)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 20000})
	m, _ := f.member(usageGPUServer)
	if !m.RunStartedAt.IsZero() || m.RunPeakRAMMB != 0 {
		t.Fatalf("run fields after the end = %+v, want cleared", m)
	}
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("an unformed end pushed %v, want nothing", pushed)
	}
	// A new run begun over a run nobody ended counts that one as unformed.
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 2000, nil)
	f.u.runBegan(f.ctx, usageHead)
	f.u.runEnded(f.ctx, usageHead, true)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 20000})
}

// TestMemberUsagePushesMemberServerAtRunEnd: the member server's document is
// pushed when a formed run's peak changed what is stored (after the head
// disconnects, the agent no longer holds the value), and only then.
func TestMemberUsagePushesMemberServerAtRunEnd(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{usageRow(usageMemberSpec(usageCPUServer), usageHead, 3000, nil)})
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("rises pushed %v", pushed)
	}
	f.u.runEnded(f.ctx, usageHead, true)
	if got := slices.Sorted(slices.Values(f.takePushes())); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("pushed %v, want the GPU member's server only (the CPU member's stored value equals its peak)", got)
	}

	// The same peak again changes nothing and pushes nothing.
	f.u.runBegan(f.ctx, usageHead)
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, true)
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("an unchanged formed end pushed %v", pushed)
	}
}

// openSessions lists the members whose session of the head is open now.
func (f *usageFixture) openSessions() []string {
	f.t.Helper()
	rows, err := f.store.RPCSessionsAt(f.ctx, usageHead, 0, f.clock())
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Until == nil {
			out = append(out, r.MemberServerID)
		}
	}
	return out
}

// TestRPCSessionsOpenAndClose pins the session records: a hold by the head
// opens one (once, however many samples show it), its end closes it, and a
// session a restarted gateway finds open is closed by the first sample
// without the hold, or adopted -- not opened twice -- by one with it.
func TestRPCSessionsOpenAndClose(t *testing.T) {
	f := newUsageFixture(t)
	start := f.clock()
	f.observeGPU(usageHead, 0, nil)
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("open sessions = %v, want the GPU member's", got)
	}
	f.advance(time.Minute)
	f.observeGPU("", 0, nil)
	if got := f.openSessions(); len(got) != 0 {
		t.Fatalf("open after the release = %v", got)
	}
	at, err := f.store.RPCSessionsAt(f.ctx, usageHead, 0, start)
	if err != nil || len(at) != 1 || !at[0].Since.Equal(start) || at[0].Until == nil || !at[0].Until.Equal(f.clock()) {
		t.Fatalf("closed session = %+v err=%v, want since %v until %v", at, err, start, f.clock())
	}

	// Open sessions survive a gateway restart in the store: the first sample
	// with the hold adopts the session, the first without it closes it.
	f.advance(time.Minute)
	f.observeGPU(usageHead, 0, nil)
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{usageRow(usageMemberSpec(usageCPUServer), usageHead, 0, nil)})
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer, usageCPUServer}) {
		t.Fatalf("open before the restart = %v, want both members'", got)
	}
	f.u = f.newUsage()
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	f.u.observe(f.ctx, usageCPUServer, nil)
	// The GPU member's session is listed once: adopted, not opened again.
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("after the restart open = %v, want the GPU member's adopted and the CPU member's closed", got)
	}

	// A membership removed while its session is open closes the session at
	// its server's next sample.
	members, _, err := f.store.ClusterMembers(f.ctx, usageHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetClusterMembers(f.ctx, usageHead, members[1:], nil); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	if got := f.openSessions(); len(got) != 0 {
		t.Fatalf("open after the membership left = %v", got)
	}
}

// TestResetMemberUsage pins POST .../runtime/cluster-members/{serverID}/reset-usage:
// the member's stored usage and run peaks are cleared, the member server's
// document is pushed, and the answer is the head's runtime spec. A server
// that is no member answers 404 with the member's code.
func TestResetMemberUsage(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	seedUsageCluster(t, srv.Routes, now)
	var pushed []string
	srv.clusterUsage.push = func(id string) { pushed = append(pushed, id) }
	members, _, err := srv.Routes.ClusterMembers(ctx, usageHead)
	if err != nil {
		t.Fatal(err)
	}
	m := members[0]
	m.RAMMeasuredMB, m.RunPeakRAMMB, m.Stale, m.MeasuredAt, m.RunStartedAt = 7000, 6000, true, now, now
	if err := srv.Routes.UpdateClusterMemberUsage(ctx, m, []routing.ClusterMemberGPU{{GPUIndex: 0, VRAMMeasuredMB: 30000, RunPeakVRAMMB: 25000}}); err != nil {
		t.Fatal(err)
	}

	base := "/api/portal/mappings/map_usage_head/runtime/cluster-members/"
	rec := rpcMemberDo(t, srv, http.MethodPost, base+usageGPUServer+"/reset-usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	var dto portal.RuntimeSpecDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil || dto.ID != usageHead {
		t.Fatalf("body %s err=%v, want the head's runtime spec", rec.Body.String(), err)
	}
	members, gpus, err := srv.Routes.ClusterMembers(ctx, usageHead)
	if err != nil {
		t.Fatal(err)
	}
	got := members[0]
	if got.RAMMeasuredMB != 0 || got.RunPeakRAMMB != 0 || got.Stale || !got.MeasuredAt.IsZero() {
		t.Fatalf("member after reset = %+v, want its usage cleared", got)
	}
	for _, g := range gpus {
		if g.ServerID == usageGPUServer && (g.VRAMMeasuredMB != 0 || g.RunPeakVRAMMB != 0) {
			t.Fatalf("gpu after reset = %+v", g)
		}
	}
	if !slices.Equal(pushed, []string{usageGPUServer}) {
		t.Fatalf("pushed %v, want the member server", pushed)
	}

	rec = rpcMemberDo(t, srv, http.MethodPost, base+usageHeadServer+"/reset-usage", "")
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "runtime_spec.cluster_member_not_found" {
		t.Fatalf("no member = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base+"srv_nope/reset-usage", "")
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "server.not_found" {
		t.Fatalf("unknown server = %d %s", rec.Code, rec.Body.String())
	}
	if rec = rpcMemberDo(t, srv, http.MethodGet, base+usageGPUServer+"/reset-usage", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get = %d", rec.Code)
	}
}

// runRecorder records the runs the orchestration passes on.
type runRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *runRecorder) runBegan(_ context.Context, head string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "began:"+head)
}

func (r *runRecorder) runEnded(_ context.Context, head string, formed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if formed {
		r.events = append(r.events, "ended:formed:"+head)
		return
	}
	r.events = append(r.events, "ended:unformed:"+head)
}

func (r *runRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func newRunFixture(t *testing.T) (*orchFixture, *runRecorder) {
	t.Helper()
	f := newOrchFixture(t, 2, "relay", "lan")
	runs := &runRecorder{}
	f.o.deps.runs = runs
	return f, runs
}

// TestClusterRunsFollowTheOrchestration pins where the orchestration begins
// and ends a head's run for the member usage: a start begins one; the head's
// stop ends it once -- formed when the cluster formed, whether the explicit
// stop or the head's exit on telemetry passes it on first; a start that
// never formed ends its run unformed; a head found formed after a gateway
// restart continues the stored run instead of beginning one.
func TestClusterRunsFollowTheOrchestration(t *testing.T) {
	f, runs := newRunFixture(t)
	f.formed()
	if got := runs.take(); !slices.Equal(got, []string{"began:" + orchHead}) {
		t.Fatalf("runs after the start = %v, want one begun", got)
	}
	if err := f.o.Stop(context.Background(), orchHead, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	f.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	f.settle()
	if got := runs.take(); !slices.Equal(got, []string{"ended:formed:" + orchHead}) {
		t.Fatalf("runs after the stop = %v, want one formed end", got)
	}

	// The head's own exit (an idle-out) ends a formed run as well.
	g, gruns := newRunFixture(t)
	g.formed()
	gruns.take()
	g.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	g.settle()
	if got := gruns.take(); !slices.Equal(got, []string{"ended:formed:" + orchHead}) {
		t.Fatalf("runs after the head exited = %v, want one formed end", got)
	}

	// A formation that never came: the head is stopped, and the run ends
	// unformed when the head's exit is seen.
	h, hruns := newRunFixture(t)
	h.ackAll()
	h.agent.onHeadEnsure = func() {
		h.hold(0) // member 1 is never attached
		h.statuses.publish(orchHeadServer, []RuntimeStatusDTO{watchHeadRow(agentSpecStateRunning)})
	}
	clusterErrOf(t, h.o.Ensure(context.Background(), orchHead))
	h.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	h.settle()
	if got := hruns.take(); !slices.Equal(got, []string{"began:" + orchHead, "ended:unformed:" + orchHead}) {
		t.Fatalf("runs of an unformed start = %v, want begun, then ended unformed", got)
	}

	// A cluster the telemetry already shows formed (a restarted gateway)
	// begins no run; its end is still passed on, formed.
	k, kruns := newRunFixture(t)
	k.ackAll()
	k.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	k.hold(0, 1)
	if err := k.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	k.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	k.settle()
	if got := kruns.take(); !slices.Equal(got, []string{"ended:formed:" + orchHead}) {
		t.Fatalf("runs of a cluster found formed = %v, want only its formed end", got)
	}
}
