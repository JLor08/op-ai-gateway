// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
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
	if applied, err := f.store.UpdateClusterMemberUsage(f.ctx, m, gpus); err != nil || !applied {
		f.t.Fatalf("store usage: applied=%v err=%v", applied, err)
	}
}

// begin begins a run of the head and moves the clock on, so the next run
// gets another identity.
func (f *usageFixture) begin() time.Time {
	f.t.Helper()
	run := f.u.runBegan(f.ctx, usageHead)
	if run.IsZero() {
		f.t.Fatal("runBegan recorded no run")
	}
	f.advance(time.Second)
	return run
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
	f.begin()
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
	f.begin()
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
	// Outside a run, even the head's hold counts for nothing.
	f.observeGPU(usageHead, 5000, map[int]int{0: 10000})
	f.wantStored(usageGPUServer, 0, map[int]int{0: 0})
	f.begin()
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
	f.begin()
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
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000, 1: 4000})
	f.advance(time.Minute)
	f.u.runEnded(f.ctx, usageHead, run, true)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 9000, 1: 4000})
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
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, run, false)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 20000})
	m, _ := f.member(usageGPUServer)
	if !m.RunStartedAt.IsZero() || m.RunPeakRAMMB != 0 {
		t.Fatalf("run fields after the end = %+v, want cleared", m)
	}
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("an unformed end pushed %v, want nothing", pushed)
	}
	// A new run begun over a run nobody ended replaces it: the old run's
	// peak replaces nothing.
	f.begin()
	f.observeGPU(usageHead, 2000, nil)
	next := f.begin()
	f.u.runEnded(f.ctx, usageHead, next, true)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 20000})
}

// TestMemberUsagePushesMemberServerAtRunEnd: the member server's document is
// pushed when a formed run's peak changed what is stored (after the head
// disconnects, the agent no longer holds the value), and only then.
func TestMemberUsagePushesMemberServerAtRunEnd(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.observe(f.ctx, usageCPUServer, []RuntimeStatusDTO{usageRow(usageMemberSpec(usageCPUServer), usageHead, 3000, nil)})
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("rises pushed %v", pushed)
	}
	f.u.runEnded(f.ctx, usageHead, run, true)
	if got := slices.Sorted(slices.Values(f.takePushes())); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("pushed %v, want the GPU member's server only (the CPU member's stored value equals its peak)", got)
	}

	// The same peak again changes nothing and pushes nothing.
	run = f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, run, true)
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

// TestClusterUsageClockIsUTC pins that the session clock the server gives the
// member usage reads UTC without a monotonic reading: session bounds are
// compared with usage event times, which are UTC, and a SQL engine that
// keeps times as text compares them as written.
func TestClusterUsageClockIsUTC(t *testing.T) {
	srv := NewTestServer()
	got := srv.clusterUsage.now()
	if got.Location() != time.UTC {
		t.Fatalf("session clock zone = %v, want UTC", got.Location())
	}
	if got != got.Round(0) {
		t.Fatal("session clock carries a monotonic reading")
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
	if err := srv.Routes.BeginClusterMemberRun(ctx, usageHead, now, 0); err != nil {
		t.Fatal(err)
	}
	m := members[0]
	m.RAMMeasuredMB, m.RunPeakRAMMB, m.Stale, m.MeasuredAt, m.RunStartedAt = 7000, 6000, true, now, now
	if applied, err := srv.Routes.UpdateClusterMemberUsage(ctx, m, []routing.ClusterMemberGPU{{GPUIndex: 0, VRAMMeasuredMB: 30000, RunPeakVRAMMB: 25000}}); err != nil || !applied {
		t.Fatalf("seed usage: applied=%v err=%v", applied, err)
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
	if got.RAMMeasuredMB != 0 || got.RunPeakRAMMB != 0 || got.Stale || !got.MeasuredAt.IsZero() || !got.RunStartedAt.Equal(now) {
		t.Fatalf("member after reset = %+v, want its usage cleared and its run kept", got)
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
	// noStored: the store holds no run (currentRun answers zero).
	noStored bool
	// silentCtx, when set, receives the context closeSilentSessions got and
	// when, and the call waits for that context to end.
	silentCtx chan silentCall
	unheard   int
}

type silentCall struct {
	ctx context.Context
	at  time.Time
}

// The recorder's runs: a begun run is "begun", the stored run it reports is
// "stored"; events name the run an end was for.
var (
	recordedBegun  = time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	recordedStored = time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
)

func runName(run time.Time) string {
	switch {
	case run.Equal(recordedBegun):
		return "begun"
	case run.Equal(recordedStored):
		return "stored"
	}
	return run.String()
}

func (r *runRecorder) runBegan(_ context.Context, head string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "began:"+head)
	return recordedBegun
}

func (r *runRecorder) currentRun(_ context.Context, head string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "continued:"+head)
	if r.noStored {
		return time.Time{}
	}
	return recordedStored
}

func (r *runRecorder) runEnded(_ context.Context, head string, run time.Time, formed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if formed {
		r.events = append(r.events, "ended:formed:"+runName(run)+":"+head)
		return
	}
	r.events = append(r.events, "ended:unformed:"+runName(run)+":"+head)
}

func (r *runRecorder) closeSilentSessions(ctx context.Context, _ func(string) (time.Time, bool)) {
	r.mu.Lock()
	ch := r.silentCtx
	r.mu.Unlock()
	if ch == nil {
		return
	}
	ch <- silentCall{ctx: ctx, at: time.Now()}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
}

func (r *runRecorder) closeUnheardSessions(context.Context, time.Time, func(string) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unheard++
	return true
}

func (r *runRecorder) unheardCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unheard
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
	// The head's server has reported, without a process for the head.
	f.statuses.publish(orchHeadServer, []RuntimeStatusDTO{})
	return f, runs
}

// TestClusterRunsFollowTheOrchestration pins where the orchestration begins
// and ends a head's run for the member usage: a start of a head without a
// process begins one; the head's stop ends it once, naming it -- formed when
// the cluster formed, whether the explicit stop or the head's exit on
// telemetry passes it on first; a start that never formed ends its run
// unformed; a head found formed, or still running, continues the stored run
// instead of beginning one.
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
	if got := runs.take(); !slices.Equal(got, []string{"ended:formed:begun:" + orchHead}) {
		t.Fatalf("runs after the stop = %v, want one formed end of the begun run", got)
	}

	// The head's own exit (an idle-out) ends a formed run as well.
	g, gruns := newRunFixture(t)
	g.formed()
	gruns.take()
	g.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	g.settle()
	if got := gruns.take(); !slices.Equal(got, []string{"ended:formed:begun:" + orchHead}) {
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
	if got := hruns.take(); !slices.Equal(got, []string{"began:" + orchHead, "ended:unformed:begun:" + orchHead}) {
		t.Fatalf("runs of an unformed start = %v, want begun, then ended unformed", got)
	}

	// A cluster the telemetry already shows formed (a restarted gateway)
	// begins no run: it continues the stored one, and its end names it.
	k, kruns := newRunFixture(t)
	k.ackAll()
	k.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	k.hold(0, 1)
	if err := k.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	k.frame(orchHeadServer, RuntimeStatusDTO{SpecID: orchHead, Model: orchHeadModel, State: agentSpecStateStopped})
	k.settle()
	if got := kruns.take(); !slices.Equal(got, []string{"continued:" + orchHead, "ended:formed:stored:" + orchHead}) {
		t.Fatalf("runs of a cluster found formed = %v, want the stored run continued and ended formed", got)
	}
}

// TestRestartWithPartialTelemetryContinuesTheRun: after a gateway restart a
// request can start a cluster whose head still runs while a member has not
// reported yet, so formation is not seen. The start must continue the stored
// run of the running process, never begin one (which would discard its
// peaks); a head whose server has not reported at all is treated the same.
func TestRestartWithPartialTelemetryContinuesTheRun(t *testing.T) {
	f, runs := newRunFixture(t)
	f.ackAll()
	f.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	f.hold(0) // member 1 has not reported since the restart
	f.holdOnHeadEnsure(0, 1)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := runs.take(); !slices.Equal(got, []string{"continued:" + orchHead}) {
		t.Fatalf("runs = %v, want the stored run continued, none begun", got)
	}

	g := newOrchFixture(t, 2, "relay", "lan")
	gruns := &runRecorder{}
	g.o.deps.runs = gruns
	g.ackAll()
	g.holdOnHeadEnsure(0, 1)
	if err := g.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := gruns.take(); !slices.Equal(got, []string{"continued:" + orchHead}) {
		t.Fatalf("runs with the head server unheard = %v, want the stored run continued", got)
	}
}

// TestMemberUsageRunContinuesAcrossARestart: the run lives in the store, so
// a restarted gateway continues it -- the peaks before the restart count at
// the formed end.
func TestMemberUsageRunContinuesAcrossARestart(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	run := f.begin()
	f.observeGPU(usageHead, 6000, map[int]int{0: 10000})
	f.u = f.newUsage()
	if got := f.u.currentRun(f.ctx, usageHead); !got.Equal(run) {
		t.Fatalf("stored run after the restart = %v, want %v", got, run)
	}
	f.observeGPU(usageHead, 4000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, run, true)
	f.wantStored(usageGPUServer, 6000, map[int]int{0: 10000})
}

// TestMemberUsageLateEndLeavesTheNextRunAlone: a run's end can land after the
// next run began (the end runs aside, the next start does not wait for it).
// It names its run, so it neither ends the new run nor replaces the stored
// value with the new run's partial peak.
func TestMemberUsageLateEndLeavesTheNextRunAlone(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	old := f.begin()
	f.observeGPU(usageHead, 7000, map[int]int{0: 11000})
	next := f.begin()
	f.observeGPU(usageHead, 2000, map[int]int{0: 3000})
	f.u.runEnded(f.ctx, usageHead, old, true)
	f.wantStored(usageGPUServer, 8000, map[int]int{0: 12000})
	m, gpus := f.member(usageGPUServer)
	if !m.RunStartedAt.Equal(next) || m.RunPeakRAMMB != 2000 || gpus[0].RunPeakVRAMMB != 3000 {
		t.Fatalf("the new run after the late end = %+v %+v, want it intact", m, gpus[0])
	}
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("the late end pushed %v", pushed)
	}
	f.u.runEnded(f.ctx, usageHead, next, true)
	f.wantStored(usageGPUServer, 2000, map[int]int{0: 3000})
}

// portalRewrite does to a membership what the portal's save does on a change
// of the distribution: the values kept as a stale floor, the run reset.
func (f *usageFixture) portalRewrite() {
	f.t.Helper()
	members, gpus, err := f.store.ClusterMembers(f.ctx, usageHead)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range members {
		members[i].RunStartedAt, members[i].RunHeadInstance, members[i].RunPeakRAMMB = time.Time{}, 0, 0
		members[i].Stale = members[i].RAMMeasuredMB > 0
	}
	for i := range gpus {
		gpus[i].RunPeakVRAMMB = 0
	}
	if err := f.store.SetClusterMembers(f.ctx, usageHead, members, gpus); err != nil {
		f.t.Fatal(err)
	}
}

// TestRunEndKeepsAnEditsStaleMark: a cluster edited while it runs (its
// distribution changed) keeps its member values as a stale floor; the old
// run's formed end must neither replace them with the old split's peak nor
// clear the stale mark, and pushes nothing.
func TestRunEndKeepsAnEditsStaleMark(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.portalRewrite()
	// The old process still reports the hold while it drains: outside a run
	// it counts for nothing.
	f.observeGPU(usageHead, 9000, map[int]int{0: 13000})
	f.u.runEnded(f.ctx, usageHead, run, true)
	f.wantStored(usageGPUServer, 8000, map[int]int{0: 12000})
	if m, _ := f.member(usageGPUServer); !m.Stale {
		t.Fatalf("member after the old run's end = %+v, want it still stale", m)
	}
	if pushed := f.takePushes(); len(pushed) != 0 {
		t.Fatalf("pushed %v, want nothing", pushed)
	}
}

// rewritingStore is a memory store that runs a membership rewrite right after
// the next ClusterMembers read: a portal save landing between the gateway's
// read and its write.
type rewritingStore struct {
	*routing.MemoryStore
	mu    sync.Mutex
	after func()
}

func (s *rewritingStore) ClusterMembers(ctx context.Context, head string) ([]routing.ClusterMember, []routing.ClusterMemberGPU, error) {
	m, g, err := s.MemoryStore.ClusterMembers(ctx, head)
	s.mu.Lock()
	hook := s.after
	s.after = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return m, g, err
}

// TestUsageWriteNeverUndoesAConcurrentEdit: a portal save that marks the
// values stale and resets the run between the gateway's read of a row and
// its write wins -- neither a rise nor a formed end writes back the stale
// mark or the values it read.
func TestUsageWriteNeverUndoesAConcurrentEdit(t *testing.T) {
	f := newUsageFixture(t)
	rs := &rewritingStore{MemoryStore: f.store}
	f.u = newClusterUsage(rs, func(string) {}, f.clock)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, false)
	f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})

	rs.mu.Lock()
	rs.after = f.portalRewrite
	rs.mu.Unlock()
	f.observeGPU(usageHead, 9500, map[int]int{0: 14000})
	f.wantStored(usageGPUServer, 8000, map[int]int{0: 12000})
	if m, _ := f.member(usageGPUServer); !m.Stale {
		t.Fatalf("after a rise raced the edit: %+v, want it stale", m)
	}

	// The same race at a formed end: the end read the run before the edit.
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000}, true)
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	rs.mu.Lock()
	rs.after = f.portalRewrite
	rs.mu.Unlock()
	f.u.runEnded(f.ctx, usageHead, run, true)
	f.wantStored(usageGPUServer, 8000, map[int]int{0: 12000})
	if m, _ := f.member(usageGPUServer); !m.Stale {
		t.Fatalf("after a formed end raced the edit: %+v, want it stale", m)
	}
}

// TestMemberUsageStaleClearsOnlyWhenEveryPartWasMeasured: a formed run that
// measured one GPU of a stale member replaces that GPU's value, but the
// member stays stale while another stored part was not measured in the run.
func TestMemberUsageStaleClearsOnlyWhenEveryPartWasMeasured(t *testing.T) {
	f := newUsageFixture(t)
	f.storeUsage(usageGPUServer, 8000, map[int]int{0: 12000, 1: 6000}, true)
	run := f.begin()
	f.observeGPU(usageHead, 5000, map[int]int{0: 9000})
	f.u.runEnded(f.ctx, usageHead, run, true)
	f.wantStored(usageGPUServer, 5000, map[int]int{0: 9000, 1: 6000})
	if m, _ := f.member(usageGPUServer); !m.Stale {
		t.Fatalf("member = %+v, want it still stale: GPU 1 was not measured", m)
	}
}

// failingSessionsStore fails the open-session listing the given number of
// times.
type failingSessionsStore struct {
	*routing.MemoryStore
	fails int
}

func (s *failingSessionsStore) OpenRPCSessionsByMember(ctx context.Context, member string) ([]routing.RPCSession, error) {
	if s.fails > 0 {
		s.fails--
		return nil, errors.New("store unavailable")
	}
	return s.MemoryStore.OpenRPCSessionsByMember(ctx, member)
}

// TestSessionSyncFailureOpensNoDuplicate: a restarted gateway that cannot
// read the sessions the store has open must not open another one (the energy
// accounting would count the member twice); it syncs on the next frame.
func TestSessionSyncFailureOpensNoDuplicate(t *testing.T) {
	f := newUsageFixture(t)
	f.observeGPU(usageHead, 0, nil)
	fs := &failingSessionsStore{MemoryStore: f.store, fails: 1}
	f.u = newClusterUsage(fs, func(string) {}, f.clock)
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("open sessions = %v, want the one session adopted, none opened twice", got)
	}
}

// TestSessionsOfAGoneMembershipCloseAtTheFirstFrame: a session the store has
// open for a head the member server no longer serves (deleted while the
// gateway was down) is closed by the server's first frame.
func TestSessionsOfAGoneMembershipCloseAtTheFirstFrame(t *testing.T) {
	f := newUsageFixture(t)
	if err := f.store.OpenRPCSession(f.ctx, routing.RPCSession{HeadSpecID: "rspec_deleted", MemberServerID: usageGPUServer, Since: f.clock()}); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Second)
	f.u.observe(f.ctx, usageGPUServer, nil)
	open, err := f.store.OpenRPCSessionsByMember(f.ctx, usageGPUServer)
	if err != nil || len(open) != 0 {
		t.Fatalf("open sessions = %+v err=%v, want the gone head's closed", open, err)
	}
}

// TestSilentMemberSessionsClose: a member server whose telemetry stays away
// past the stale threshold has its sessions closed at its last frame, and
// syncs again when it reports.
func TestSilentMemberSessionsClose(t *testing.T) {
	f := newUsageFixture(t)
	last := f.clock()
	f.observeGPU(usageHead, 0, nil)
	f.advance(time.Minute)
	f.u.closeSilentSessions(f.ctx, func(string) (time.Time, bool) { return last, false })
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("open while reporting = %v", got)
	}
	f.u.closeSilentSessions(f.ctx, func(id string) (time.Time, bool) { return last, id == usageGPUServer })
	at, err := f.store.RPCSessionsAt(f.ctx, usageHead, 0, last)
	if err != nil || len(at) != 1 || at[0].Until == nil || !at[0].Until.Equal(last) {
		t.Fatalf("session = %+v err=%v, want it closed at the last frame", at, err)
	}
	f.advance(time.Second)
	f.observeGPU(usageHead, 0, nil)
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("open after the server came back = %v, want a new session", got)
	}
}

// TestWatchdogSilentSince pins the silence threshold the session close uses:
// the watchdog's stale threshold, max(3 intervals, staleFloor).
func TestWatchdogSilentSince(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	if _, silent := f.o.watch.silentSince(orchMemberServer(0)); silent {
		t.Fatal("a server never heard from is judged silent")
	}
	f.frame(orchMemberServer(0))
	f.settle()
	arrived := f.clock()
	f.advance(15 * time.Second)
	if _, silent := f.o.watch.silentSince(orchMemberServer(0)); silent {
		t.Fatal("silent at the threshold, want only past it")
	}
	f.advance(time.Second)
	if last, silent := f.o.watch.silentSince(orchMemberServer(0)); !silent || !last.Equal(arrived) {
		t.Fatalf("silentSince = %v %v, want silent since %v", last, silent, arrived)
	}
}

// TestKeyedLocksShrink: a lock nobody holds or waits for leaves the map.
func TestKeyedLocksShrink(t *testing.T) {
	var k keyedLocks
	unlock := k.lock("a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.lock("a")()
	}()
	unlock()
	<-done
	k.lock("b")()
	if n := k.size(); n != 0 {
		t.Fatalf("locks held in the map = %d, want 0", n)
	}
}

// TestRestartWithoutAStoredRunBegins: a restarted gateway asked to start a
// cluster before the head's server reported continues the stored run -- but
// when the store holds none (the last run ended normally), it begins one, or
// the whole new run would measure nothing.
func TestRestartWithoutAStoredRunBegins(t *testing.T) {
	f := newOrchFixture(t, 2, "relay", "lan")
	runs := &runRecorder{noStored: true}
	f.o.deps.runs = runs
	f.ackAll()
	f.holdOnHeadEnsure(0, 1)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := runs.take(); !slices.Equal(got, []string{"continued:" + orchHead, "began:" + orchHead}) {
		t.Fatalf("runs = %v, want the empty stored run found, then a run begun", got)
	}

	// A head seen still running continues even an empty stored run: its
	// process's peaks are unknown, and a begun run would replace by a part.
	g := newOrchFixture(t, 2, "relay", "lan")
	gruns := &runRecorder{noStored: true}
	g.o.deps.runs = gruns
	g.ackAll()
	g.frame(orchHeadServer, watchHeadRow(agentSpecStateRunning))
	g.hold(0)
	g.holdOnHeadEnsure(0, 1)
	if err := g.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := gruns.take(); !slices.Equal(got, []string{"continued:" + orchHead}) {
		t.Fatalf("runs with the head running = %v, want only continued", got)
	}
}

// TestSilentCloseRechecksUnderTheLock: a frame of the member server handled
// between the reconciler's verdict and the close makes the server heard
// again; the close must see that and leave its sessions open and synced.
func TestSilentCloseRechecksUnderTheLock(t *testing.T) {
	f := newUsageFixture(t)
	last := f.clock()
	f.observeGPU(usageHead, 0, nil)
	calls := 0
	f.u.closeSilentSessions(f.ctx, func(string) (time.Time, bool) {
		calls++
		// The first verdict says silent; by the time the close holds the
		// server's lock, a frame arrived.
		return last, calls == 1
	})
	if calls < 2 {
		t.Fatalf("silence judged %d times, want it judged again under the server's lock", calls)
	}
	if got := f.openSessions(); !slices.Equal(got, []string{usageGPUServer}) {
		t.Fatalf("open sessions = %v, want the session kept", got)
	}
	f.u.mu.Lock()
	synced := f.u.seen[usageGPUServer]
	f.u.mu.Unlock()
	if !synced {
		t.Fatal("the server was marked unsynced")
	}
}

// TestUnheardMemberSessionsClose: sessions the store holds open on a member
// server that has not reported since the gateway started are closed -- at the
// server's last telemetry before the start when it is known, else at the
// gateway's start. A server heard since the start, whose own frames sync its
// sessions, is left alone.
func TestUnheardMemberSessionsClose(t *testing.T) {
	f := newUsageFixture(t)
	since := f.clock()
	for _, member := range []string{usageGPUServer, usageCPUServer, usageHeadServer} {
		if err := f.store.OpenRPCSession(f.ctx, routing.RPCSession{HeadSpecID: usageHead, MemberServerID: member, Since: since}); err != nil {
			t.Fatal(err)
		}
	}
	lastSeen := since.Add(30 * time.Second)
	bootAt := since.Add(time.Minute)
	f.u.lastSeen = func(_ context.Context, id string) (time.Time, bool) {
		return lastSeen, id == usageGPUServer
	}
	if !f.u.closeUnheardSessions(f.ctx, bootAt, func(id string) bool { return id == usageHeadServer }) {
		t.Fatal("the sweep reported a failure")
	}
	until := map[string]*time.Time{}
	rows, err := f.store.RPCSessionsAt(f.ctx, usageHead, 0, since)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		until[r.MemberServerID] = r.Until
	}
	if u := until[usageGPUServer]; u == nil || !u.Equal(lastSeen) {
		t.Fatalf("GPU member closed at %v, want its last telemetry %v", u, lastSeen)
	}
	if u := until[usageCPUServer]; u == nil || !u.Equal(bootAt) {
		t.Fatalf("CPU member closed at %v, want the gateway's start %v", u, bootAt)
	}
	if u := until[usageHeadServer]; u != nil {
		t.Fatalf("a heard server's session closed at %v", u)
	}
}

// TestReconcileSweepsUnheardSessionsOnceAfterBootGrace: the unheard-server
// sweep runs after the boot grace only -- a member may simply not have
// reported yet before -- and once.
func TestReconcileSweepsUnheardSessionsOnceAfterBootGrace(t *testing.T) {
	f, runs := newRunFixture(t)
	f.o.Reconcile(context.Background())
	if n := runs.unheardCalls(); n != 0 {
		t.Fatalf("sweeps in the boot grace = %d", n)
	}
	f.advance(61 * time.Second)
	f.o.Reconcile(context.Background())
	f.o.Reconcile(context.Background())
	if n := runs.unheardCalls(); n != 1 {
		t.Fatalf("sweeps after the boot grace = %d, want 1", n)
	}
}

// TestSessionCloseHasItsOwnBudget: the session closes of a reconciler pass
// run under their own bound, so a slow store there cannot use up the pass's
// budget for the kept-up heads.
func TestSessionCloseHasItsOwnBudget(t *testing.T) {
	f, runs := newRunFixture(t)
	calls := make(chan silentCall, 1)
	runs.mu.Lock()
	runs.silentCtx = calls
	runs.mu.Unlock()
	f.o.Reconcile(context.Background())
	call := <-calls
	deadline, ok := call.ctx.Deadline()
	if !ok || deadline.Sub(call.at) > f.o.t.sessionWait+50*time.Millisecond {
		t.Fatalf("session close deadline in %v, want its own bound of %v", deadline.Sub(call.at), f.o.t.sessionWait)
	}
	f.portal.mu.Lock()
	keptErr := f.portal.keptCtxErr
	f.portal.mu.Unlock()
	if keptErr != nil {
		t.Fatalf("the kept-up heads were listed on an ended context: %v", keptErr)
	}
}

// TestUnheardSessionCloseNeverPrecedesItsStart: after the gateway's clock
// stepped backwards, its start can lie before a session it finds open; the
// session then closes at its own start, never with an end before its start
// (an interval the member-energy lookup could not place).
func TestUnheardSessionCloseNeverPrecedesItsStart(t *testing.T) {
	f := newUsageFixture(t)
	since := f.clock()
	if err := f.store.OpenRPCSession(f.ctx, routing.RPCSession{HeadSpecID: usageHead, MemberServerID: usageGPUServer, Since: since}); err != nil {
		t.Fatal(err)
	}
	bootAt := since.Add(-time.Minute)
	f.u.lastSeen = func(context.Context, string) (time.Time, bool) { return time.Time{}, false }
	if !f.u.closeUnheardSessions(f.ctx, bootAt, func(string) bool { return false }) {
		t.Fatal("the sweep reported a failure")
	}
	rows, err := f.store.RPCSessionsAt(f.ctx, usageHead, 0, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Until == nil || !rows[0].Until.Equal(since) {
		t.Fatalf("sessions = %+v, want one closed at its start %v", rows, since)
	}
}
