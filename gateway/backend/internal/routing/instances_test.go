// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"slices"
	"testing"
	"time"
)

// fakeInstanceSource is a test InstanceSource keyed by mapping id: a mapping
// absent from views is not instance-capable (ok=false). asked records every
// mapping id it was asked about, in order, so a test can tell whether (and how
// often) the resolver consulted it.
type fakeInstanceSource struct {
	views map[string]InstanceView
	asked []string
}

func (f *fakeInstanceSource) InstanceView(_ context.Context, _ string, m ModelMapping) (InstanceView, bool) {
	f.asked = append(f.asked, m.ID)
	v, ok := f.views[m.ID]
	return v, ok
}

// agentCandidate is one server_agent candidate serving upstream model m from
// mapping map_m on srv.
func agentCandidate() MappingCandidate {
	return MappingCandidate{
		Server:      AIServer{ID: "srv", Status: ServerStatusActive, HealthStatus: HealthHealthy},
		Application: Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}},
		Mapping:     ModelMapping{ID: "map_m", GatewayModelName: "gm", AppModelName: "m"},
	}
}

func TestInstanceAddressAndKey(t *testing.T) {
	for _, tc := range []struct {
		base string
		k    int
		want string
	}{{"m", 0, "m"}, {"m", 2, "m#2"}, {"foo#2", 0, "foo#2"}, {"foo#2", 3, "foo#2#3"}} {
		if got := InstanceAddress(tc.base, tc.k); got != tc.want {
			t.Errorf("InstanceAddress(%q, %d) = %q, want %q", tc.base, tc.k, got, tc.want)
		}
	}
	if got := InstanceKey("map_m", 0); got != "map_m#0" {
		t.Errorf("InstanceKey(map_m, 0) = %q, want map_m#0 (instance 0 carries its index too)", got)
	}
	if got := InstanceKey("map_m", 7); got != "map_m#7" {
		t.Errorf("InstanceKey(map_m, 7) = %q, want map_m#7", got)
	}
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"", 0},
		{"map_m#0", 0},
		{"map_m#2", 2},
		{"map#x#63", 63},
		{"map_m", 0},
		{"map_m#", 0},
		{"map_m#02", 0},
		{"map_m#-1", 0},
		{"map_m#64", 0},
		{"map_m#+1", 0},
	} {
		if got := (Target{InstanceKey: tc.key}).Instance(); got != tc.want {
			t.Errorf("Target{InstanceKey: %q}.Instance() = %d, want %d", tc.key, got, tc.want)
		}
	}
	for _, tc := range []struct {
		address string
		k       int
		want    string
	}{{"m#2", 2, "m"}, {"m", 0, "m"}, {"foo#2", 0, "foo#2"}, {"foo#2#2", 2, "foo#2"}, {"m#3", 2, "m#3"}} {
		if got := InstanceBase(tc.address, tc.k); got != tc.want {
			t.Errorf("InstanceBase(%q, %d) = %q, want %q", tc.address, tc.k, got, tc.want)
		}
	}
}

func TestExpandInstancesAddsRunningInstances(t *testing.T) {
	src := &fakeInstanceSource{views: map[string]InstanceView{
		"map_m": {Replicable: true, Parallelism: 1, Running: []int{2, 3}, Present: []int{1, 2, 3}},
	}}
	got := expandInstances(context.Background(), []MappingCandidate{agentCandidate()}, src)
	if instances := candidateInstances(got); !slices.Equal(instances, []int{0, 2, 3}) {
		t.Fatalf("instances = %v, want [0 2 3] (instance 0 and every running instance, never a starting one)", instances)
	}
	resolver := NewResolver(NewMemoryStore(), nil, nil)
	wantModels := []string{"m", "m#2", "m#3"}
	wantKeys := []string{"map_m#0", "map_m#2", "map_m#3"}
	for i, c := range got {
		target, err := resolver.targetFrom(context.Background(), c, APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("targetFrom(instance %d): %v", c.Instance, err)
		}
		if target.ProviderModel != wantModels[i] || target.InstanceKey != wantKeys[i] || target.RouteID != "map_m" {
			t.Errorf("instance %d: ProviderModel %q InstanceKey %q RouteID %q, want %q %q map_m",
				c.Instance, target.ProviderModel, target.InstanceKey, target.RouteID, wantModels[i], wantKeys[i])
		}
	}
}

// A view whose running list repeats an index or names one no address can
// carry yields each valid instance once.
func TestExpandInstancesIgnoresInvalidRunningIndexes(t *testing.T) {
	src := &fakeInstanceSource{views: map[string]InstanceView{
		"map_m": {Replicable: true, Running: []int{0, -1, 2, 2, MaxInstanceIndex + 1, MaxInstanceIndex}},
	}}
	got := candidateInstances(expandInstances(context.Background(), []MappingCandidate{agentCandidate()}, src))
	if !slices.Equal(got, []int{0, 2, MaxInstanceIndex}) {
		t.Fatalf("instances = %v, want [0 2 %d]", got, MaxInstanceIndex)
	}
}

func TestExpandInstancesNoViewUnchanged(t *testing.T) {
	cands := []MappingCandidate{agentCandidate()}
	// ok=false: a spec that is not replicable, or an agent without instances
	// beside one with them -- instance 0 alone, exactly the candidate it was.
	src := &fakeInstanceSource{views: map[string]InstanceView{}}
	if got := expandInstances(context.Background(), cands, src); len(got) != 1 || got[0].Instance != 0 || got[0].Mapping.ID != "map_m" {
		t.Fatalf("expandInstances without a view = %+v, want the one candidate, instance 0", got)
	}
	if got := expandInstances(context.Background(), cands, nil); len(got) != 1 || got[0].Instance != 0 {
		t.Fatalf("expandInstances with no source = %+v, want the one candidate, instance 0", got)
	}
	// Only an agent runs instances: a candidate of any other application is
	// never asked about.
	direct := agentCandidate()
	direct.Application.Type = ProviderMock
	src = &fakeInstanceSource{views: map[string]InstanceView{"map_m": {Replicable: true, Running: []int{1}}}}
	if got := expandInstances(context.Background(), []MappingCandidate{direct}, src); len(got) != 1 || len(src.asked) != 0 {
		t.Fatalf("expandInstances(direct application) = %+v, asked %v, want the one candidate and no question", got, src.asked)
	}
}

func TestModelLoadedOnUsesInstanceAddress(t *testing.T) {
	loaded := &fakeLoaded{byServer: map[string][]string{"srv": {"m#2"}}}
	zero, two := agentCandidate(), agentCandidate()
	two.Instance = 2
	if !modelLoadedOn(loaded, two) {
		t.Fatal("instance 2 with m#2 loaded: modelLoadedOn = false, want true")
	}
	if modelLoadedOn(loaded, zero) {
		t.Fatal("instance 0 with only m#2 loaded: modelLoadedOn = true, want false")
	}
	// Swap protection reads the same per-instance answer: on a busy server the
	// loaded instance is no swap, instance 0 (not loaded) would be one.
	r := NewResolver(NewMemoryStore(), nil, nil)
	r.SetLoadedModelChecker(loaded)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv": 1}}, 30*time.Second)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if r.swapProtected(two, now) {
		t.Fatal("swapProtected(instance 2, loaded) = true, want false")
	}
	if !r.swapProtected(zero, now) {
		t.Fatal("swapProtected(instance 0, not loaded, server busy) = false, want true")
	}
}

// fakeRuntimeStateByName is a RuntimeModelStateChecker answering per upstream
// name, so each instance address reads its own state.
type fakeRuntimeStateByName struct {
	byName map[string]fakeRuntimeStateEntry
	asked  []string
}

func (f *fakeRuntimeStateByName) RuntimeModelState(_, appModelName string) (string, int, int, bool, bool) {
	f.asked = append(f.asked, appModelName)
	e, ok := f.byName[appModelName]
	if !ok {
		return "", 0, 0, false, false
	}
	return e.state, e.active, e.queue, true, !e.noMetrics
}

func TestRuntimeStateOverlayPerInstance(t *testing.T) {
	checker := &fakeRuntimeStateByName{byName: map[string]fakeRuntimeStateEntry{
		"m":   {state: "running", active: 9, queue: 4},
		"m#2": {state: "starting", active: 1, queue: 0},
	}}
	r := NewResolver(NewMemoryStore(), nil, nil)
	r.SetRuntimeModelStateChecker(checker)
	zero, two := agentCandidate(), agentCandidate()
	two.Instance = 2

	if !r.modelStartingOn(two) || r.modelStartingOn(zero) {
		t.Fatalf("modelStartingOn: instance 2 %v, instance 0 %v, want true/false (each instance reads its own state)", r.modelStartingOn(two), r.modelStartingOn(zero))
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if tel, _ := r.mergeRuntimeModelMetrics(two, ServerTelemetry{}, false, now); tel.ActiveRequests != 1 || tel.QueueDepth != 0 {
		t.Fatalf("instance 2 overlay = active %d queue %d, want m#2's 1/0", tel.ActiveRequests, tel.QueueDepth)
	}
	if tel, _ := r.mergeRuntimeModelMetrics(zero, ServerTelemetry{}, false, now); tel.ActiveRequests != 9 || tel.QueueDepth != 4 {
		t.Fatalf("instance 0 overlay = active %d queue %d, want m's 9/4", tel.ActiveRequests, tel.QueueDepth)
	}
	if !slices.Contains(checker.asked, "m#2") {
		t.Fatalf("checker asked %v, want the instance address m#2", checker.asked)
	}
}

// instanceCandidateStore seeds one server_agent application on srv_inst
// serving qwen-coder as upstream qwen2.5 from mapping map_inst, with the given
// max_concurrency.
func instanceCandidateStore(t *testing.T, now time.Time, maxConcurrency int) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_inst", Name: "inst", Domain: "inst.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_inst", ServerID: "srv_inst", Type: ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_inst", ApplicationID: "app_inst", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", MaxConcurrency: maxConcurrency, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_inst", ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return store
}

// A running instance is a candidate like instance 0, and the target it
// resolves to addresses it while still attributing to the mapping.
func TestResolveRoutesToRunningInstance(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Running: []int{2}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#2"}}})

	target, err := r.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ProviderModel != "qwen2.5#2" || target.InstanceKey != "map_inst#2" || target.RouteID != "map_inst" {
		t.Fatalf("target = ProviderModel %q InstanceKey %q RouteID %q, want qwen2.5#2 map_inst#2 map_inst", target.ProviderModel, target.InstanceKey, target.RouteID)
	}
}

// A request parked in the admission queue is offered, on the turn after its
// wait, an instance that started while it waited: every turn of the admission
// loop expands the candidates afresh. Instance 0's own cap parks the request;
// during the wait instance 1 starts and instance 0 frees, and the loaded
// partition, reading each instance by its own address, picks the new one.
func TestResolveSeesInstanceStartedDuringAdmissionWait(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}}
	loaded := &fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5"}}}
	activity := &fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}} // at its parallelism
	r.SetInstanceSource(src)
	r.SetLoadedModelChecker(loaded)
	r.SetInstanceActivityChecker(activity)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			return ErrAdmissionQueueTimeout // the second turn did not see the new instance
		}
		// While the request waits, instance 1 reaches running and instance 0 frees.
		src.views["map_inst"] = InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}
		loaded.byServer["srv_inst"] = []string{"qwen2.5#1"}
		activity.byKey["map_inst#0"] = 0
		return nil
	}}
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if adm.calls != 1 {
		t.Fatalf("WaitForSlot calls = %d, want 1 (the first turn parks)", adm.calls)
	}
	if target.ProviderModel != "qwen2.5#1" || target.InstanceKey != "map_inst#1" {
		t.Fatalf("target = %q (%q), want the instance that started during the wait, qwen2.5#1 (map_inst#1)", target.ProviderModel, target.InstanceKey)
	}
	if n := len(src.asked); n != 2 {
		t.Fatalf("instance source asked %d times (%v), want once per admission turn, 2", n, src.asked)
	}
}

// Two consecutive requests see the instances running at their own time: the
// second is offered the instance that started after the first was routed.
func TestResolveExpandsInstancesPerRequest(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true}}}
	loaded := &fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5"}}}
	r.SetInstanceSource(src)
	r.SetLoadedModelChecker(loaded)
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	first, err := r.Resolve(context.Background(), auth.Token{}, req)
	if err != nil || first.ProviderModel != "qwen2.5" {
		t.Fatalf("first Resolve = %q, %v; want qwen2.5", first.ProviderModel, err)
	}
	src.views["map_inst"] = InstanceView{Replicable: true, Running: []int{3}}
	loaded.byServer["srv_inst"] = []string{"qwen2.5#3"}
	second, err := r.Resolve(context.Background(), auth.Token{}, req)
	if err != nil || second.ProviderModel != "qwen2.5#3" {
		t.Fatalf("second Resolve = %q, %v; want qwen2.5#3", second.ProviderModel, err)
	}
}

// A group member's candidates expand like a plain model's, so a group can
// serve from a running instance.
func TestGroupMemberRoutesToRunningInstance(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}, members: []GroupMember{
			{ID: "gm_q", GroupID: "grp1", MemberGatewayName: "qwen-coder", Priority: 0},
		}},
	}})
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Running: []int{4}}}})
	// Only instance 4 is loaded: the loaded-only filter keeps it by its own
	// address, where reading instance 0's would drop the member.
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#4"}}})

	target, err := r.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "coder-group", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ProviderModel != "qwen2.5#4" || target.InstanceKey != "map_inst#4" || target.RouteID != "map_inst" {
		t.Fatalf("target = ProviderModel %q InstanceKey %q RouteID %q, want qwen2.5#4 map_inst#4 map_inst", target.ProviderModel, target.InstanceKey, target.RouteID)
	}
}

func candidateInstances(cands []MappingCandidate) []int {
	out := make([]int, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Instance)
	}
	return out
}
