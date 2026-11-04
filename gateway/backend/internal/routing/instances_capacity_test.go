// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"slices"
	"testing"
	"time"
)

// fakeInstanceActivity is a test InstanceActivityChecker: in-flight requests
// by gateway instance key. asked records every key it was asked about.
type fakeInstanceActivity struct {
	byKey map[string]int
	asked []string
}

func (f *fakeInstanceActivity) InstanceActivity(instanceKey string) int {
	f.asked = append(f.asked, instanceKey)
	return f.byKey[instanceKey]
}

// fakeDemandSink is a test DemandSink recording every signal it receives.
type fakeDemandSink struct {
	got []DemandSignal
}

func (f *fakeDemandSink) Signal(d DemandSignal) { f.got = append(f.got, d) }

// timeoutAdmission is an admission controller whose every wait times out, so
// a request that parks returns ErrAdmissionQueueTimeout after one wait.
func timeoutAdmission() *fakeAdmission {
	return &fakeAdmission{onWait: func(int) error { return ErrAdmissionQueueTimeout }}
}

var instanceCapacityNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func chatRequest(model string) inference.Request {
	return inference.Request{Model: model, APIFlavor: "openai_chat"}
}

// An instance candidate is judged by its own in-flight count against the
// spec's parallelism, not by the server's: here the server cap alone would
// call srv_inst full (one request in flight, max_concurrency 1), yet instance
// 2 is free and serves the request.
func TestInstanceCapUsesInstanceActivity(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 1), func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}}
	activity := &fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}}
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetInstanceSource(src)
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5", "qwen2.5#2"}}})
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 1}}, 0)
	r.SetInstanceActivityChecker(activity)
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)

	target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ProviderModel != "qwen2.5#2" || target.InstanceKey != "map_inst#2" {
		t.Fatalf("target = %q (%q), want the free instance qwen2.5#2 (map_inst#2)", target.ProviderModel, target.InstanceKey)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %v; want neither while an instance is free", adm.calls, sink.got)
	}
	if !slices.Contains(activity.asked, "map_inst#0") || !slices.Contains(activity.asked, "map_inst#2") {
		t.Fatalf("instance activity asked for %v, want both instance keys", activity.asked)
	}
	// The cap reads the parallelism the expansion carried on each candidate:
	// one source question for the turn, none per candidate.
	if len(src.asked) != 1 {
		t.Fatalf("instance source asked %d times (%v), want once for the one turn", len(src.asked), src.asked)
	}
}

// An instance serves as many requests at once as its spec's parallelism.
func TestInstanceCapHonoursParallelism(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inFlight int
		parks    bool
	}{
		{"a second request shares instance 0", 1, false},
		{"instance 0 at its parallelism parks the third", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := instanceCapacityNow
			r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
			r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 2}}})
			r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": tc.inFlight}})
			adm := timeoutAdmission()
			r.SetAdmissionController(adm)

			target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
			if tc.parks {
				if !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
					t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
				}
				return
			}
			if err != nil || target.InstanceKey != "map_inst#0" || adm.calls != 0 {
				t.Fatalf("Resolve = %q, %v after %d waits; want map_inst#0 without a wait", target.InstanceKey, err, adm.calls)
			}
		})
	}
}

// Every instance busy parks the request in the admission queue even though
// the mapping sets no max_concurrency (which, for a mapping without
// instances, means "no cap" and fails open), and tells the demand sink the
// model needs another instance -- once, on the turn that parked.
func TestAllInstancesBusyQueuesAndSignals(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_inst#2": 1}})
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)

	_, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
	if !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout from the queue", err)
	}
	if adm.calls != 1 || !slices.Equal(adm.gotServers, []string{"srv_inst"}) {
		t.Fatalf("WaitForSlot calls %d on %v, want 1 on [srv_inst]", adm.calls, adm.gotServers)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
}

// Without a demand sink the all-busy request still queues.
func TestAllInstancesBusyWithoutSinkStillQueues(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)
	r.SetDemandSink(nil)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
	}
}

// A mapping that is not instance-capable keeps the server cap: with no
// max_concurrency it fails open however busy the server is, and with one it
// queues at that cap -- never signalling demand, since no instance could help.
func TestNonInstanceCandidateCapUnchanged(t *testing.T) {
	t.Run("no max_concurrency fails open", func(t *testing.T) {
		now := instanceCapacityNow
		r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{}})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 5}}, 0)
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 5}})
		adm := timeoutAdmission()
		sink := &fakeDemandSink{}
		r.SetAdmissionController(adm)
		r.SetDemandSink(sink)

		target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
		if err != nil || target.InstanceKey != "map_inst#0" {
			t.Fatalf("Resolve = %q, %v; want map_inst#0", target.InstanceKey, err)
		}
		if adm.calls != 0 || len(sink.got) != 0 {
			t.Fatalf("waits %d, demand signals %v; want neither (no queue for a mapping without a cap)", adm.calls, sink.got)
		}
	})
	t.Run("max_concurrency still queues at the server cap", func(t *testing.T) {
		now := instanceCapacityNow
		r := NewResolver(instanceCandidateStore(t, now, 1), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{}})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 1}}, 0)
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{}})
		adm := timeoutAdmission()
		sink := &fakeDemandSink{}
		r.SetAdmissionController(adm)
		r.SetDemandSink(sink)

		if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
			t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
		}
		if len(sink.got) != 0 {
			t.Fatalf("demand signals = %v, want none for a mapping without instances", sink.got)
		}
	})
}

// A request parked because every instance was busy is offered, on the next
// turn, an instance that started running while it waited: the expansion is
// re-read on every admission turn and the cap judges that turn's instances.
func TestResolveReExpandsEachAdmissionTurn(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}}
	sink := &fakeDemandSink{}
	r.SetInstanceSource(src)
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	r.SetDemandSink(sink)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			// A second wait means the second turn did not see the new instance:
			// fail with an assertion rather than park forever.
			return ErrAdmissionQueueTimeout
		}
		// While the request waits, instance 1 reaches running; instance 0 stays busy.
		src.views["map_inst"] = InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}
		return nil
	}}
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ProviderModel != "qwen2.5#1" || target.InstanceKey != "map_inst#1" {
		t.Fatalf("target = %q (%q), want the instance that started during the wait, qwen2.5#1 (map_inst#1)", target.ProviderModel, target.InstanceKey)
	}
	if adm.calls != 1 || len(sink.got) != 1 {
		t.Fatalf("waits %d, demand signals %d; want 1 and 1 (only the first turn was all busy)", adm.calls, len(sink.got))
	}
	if n := len(src.asked); n != 2 {
		t.Fatalf("instance source asked %d times (%v), want once per admission turn, 2", n, src.asked)
	}
}

// A group request parked because its member's instances were all busy sees,
// on the walk's next turn, an instance that started while it waited. The
// demand signal names the member that needs the instance, not the group.
func TestGroupReExpandsEachAdmissionTurn(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{
			{ID: "gm_q", GroupID: "grp1", MemberGatewayName: "qwen-coder", Priority: 0},
		}},
	}})
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}}
	sink := &fakeDemandSink{}
	r.SetInstanceSource(src)
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	r.SetDemandSink(sink)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			return ErrAdmissionQueueTimeout // the second turn did not see the new instance
		}
		src.views["map_inst"] = InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}
		return nil
	}}
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("coder-group"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.InstanceKey != "map_inst#1" {
		t.Fatalf("target = %q (%q), want the instance that started during the wait, map_inst#1", target.ProviderModel, target.InstanceKey)
	}
	if adm.calls != 1 || !slices.Equal(adm.gotServers, []string{"srv_inst"}) {
		t.Fatalf("WaitForSlot calls %d on %v, want 1 on [srv_inst]", adm.calls, adm.gotServers)
	}
	if want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}}; !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v (the member's model)", sink.got, want)
	}
	if n := len(src.asked); n != 2 {
		t.Fatalf("instance source asked %d times (%v), want once per walk turn, 2", n, src.asked)
	}
}

// A group member is expanded before the loaded-only filter, which then keeps
// a running instance by its own address even though instance 0 is not loaded.
func TestGroupExpandsBeforeLoadedOnlyFilter(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{4}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#4"}}})

	cands, live, err := r.eligibleCandidates(context.Background(), auth.Token{}, "qwen-coder", APIFlavorOpenAI, "openai_chat", nil, groupWalk{policy: GroupPolicy{LoadedOnly: true}})
	if err != nil {
		t.Fatalf("eligibleCandidates: %v", err)
	}
	if !live || !slices.Equal(candidateInstances(cands), []int{4}) {
		t.Fatalf("eligible instances = %v (live %v), want [4]: the loaded-only filter keeps the running instance", candidateInstances(cands), live)
	}
}

// The request context reaches the instance source, so a spec read it makes
// on a cache miss is traced under the request and cancelled with it.
func TestExpandInstancesPassesRequestContext(t *testing.T) {
	type ctxKey struct{}
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	src := &ctxRecordingInstanceSource{}
	r.SetInstanceSource(src)
	ctx := context.WithValue(context.Background(), ctxKey{}, "req")
	if _, err := r.Resolve(ctx, auth.Token{}, chatRequest("qwen-coder")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(src.ctxs) != 1 || src.ctxs[0].Value(ctxKey{}) != "req" {
		t.Fatalf("instance source saw %d contexts, want 1 carrying the request's", len(src.ctxs))
	}
}

// ctxRecordingInstanceSource records the context of every question and
// answers every mapping as not instance-capable.
type ctxRecordingInstanceSource struct {
	ctxs []context.Context
}

func (s *ctxRecordingInstanceSource) InstanceView(ctx context.Context, _ string, _ ModelMapping) (InstanceView, bool) {
	s.ctxs = append(s.ctxs, ctx)
	return InstanceView{}, false
}

// The expansion carries the view it read onto every candidate it yields, so
// the capacity check judges the same view the candidates came from: an
// instance-capable mapping's instances carry its parallelism (at least 1), a
// mapping without instances carries nothing.
func TestExpandInstancesCarriesTheViewsCapacity(t *testing.T) {
	plain := agentCandidate()
	plain.Mapping.ID = "map_plain"
	src := &fakeInstanceSource{views: map[string]InstanceView{"map_m": {Replicable: true, Parallelism: 3, Running: []int{2}}}}
	got := expandInstances(context.Background(), []MappingCandidate{agentCandidate(), plain}, src)
	if len(got) != 3 {
		t.Fatalf("expanded %d candidates, want 3 (map_m#0, map_m#2, map_plain#0)", len(got))
	}
	for _, c := range got[:2] {
		if !c.InstanceCapable || c.InstanceParallelism != 3 {
			t.Fatalf("map_m instance %d: capable %v parallelism %d, want true 3", c.Instance, c.InstanceCapable, c.InstanceParallelism)
		}
	}
	if got[2].InstanceCapable || got[2].InstanceParallelism != 0 {
		t.Fatalf("map_plain: capable %v parallelism %d, want false 0", got[2].InstanceCapable, got[2].InstanceParallelism)
	}
	src.views["map_m"] = InstanceView{Replicable: true}
	if got := expandInstances(context.Background(), []MappingCandidate{agentCandidate()}, src); got[0].InstanceParallelism != 1 {
		t.Fatalf("an unset parallelism = %d, want 1", got[0].InstanceParallelism)
	}
}

// mixedFleetStore seeds qwen-coder on two agent servers: srv_inst (mapping
// map_inst, instance-capable in the tests that give it a view) and srv_old
// (mapping map_old, max_concurrency oldMax, an agent without instances).
func mixedFleetStore(t *testing.T, now time.Time, oldMax int) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := instanceCandidateStore(t, now, 0)
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_old", Name: "old", Domain: "old.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_old", ServerID: "srv_old", Type: ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_old", ApplicationID: "app_old", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", MaxConcurrency: oldMax, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_old", ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return store
}

// A mixed fleet: an instance-capable mapping next to one on an agent that
// runs no instances. Only the capable one is expanded and capped per
// instance; the other keeps the server cap. A free old server serves while
// every instance is busy; once it is at its own cap too, the request queues
// on both servers and asks for another instance.
func TestMixedFleetCapacity(t *testing.T) {
	views := func() map[string]InstanceView {
		return map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}
	}
	busy := map[string]int{"map_inst#0": 1, "map_inst#2": 1}

	t.Run("expansion", func(t *testing.T) {
		now := instanceCapacityNow
		cands, err := mixedFleetStore(t, now, 0).ActiveMappingsForModel(context.Background(), "qwen-coder", APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("ActiveMappingsForModel: %v", err)
		}
		src := &fakeInstanceSource{views: views()}
		got := expandInstances(context.Background(), cands, src)
		var keys []string
		for _, c := range got {
			keys = append(keys, InstanceKey(c.Mapping.ID, c.Instance))
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"map_inst#0", "map_inst#2", "map_old#0"}) {
			t.Fatalf("expanded keys = %v, want map_inst#0 map_inst#2 map_old#0", keys)
		}
		if len(src.asked) != 2 {
			t.Fatalf("source asked %v, want both mappings once", src.asked)
		}
	})
	t.Run("a free old server serves while every instance is busy", func(t *testing.T) {
		now := instanceCapacityNow
		r := NewResolver(mixedFleetStore(t, now, 0), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: views()})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 2}}, 0)
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: busy})
		adm := timeoutAdmission()
		sink := &fakeDemandSink{}
		r.SetAdmissionController(adm)
		r.SetDemandSink(sink)

		target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
		if err != nil || target.InstanceKey != "map_old#0" {
			t.Fatalf("Resolve = %q, %v; want the old server's map_old#0", target.InstanceKey, err)
		}
		if adm.calls != 0 || len(sink.got) != 0 {
			t.Fatalf("waits %d, demand signals %v; want neither while a candidate is under its cap", adm.calls, sink.got)
		}
	})
	// The scorer alone would pick an instance here: srv_inst carries no load
	// and holds the model resident, srv_old is loaded down and cold. Only each
	// instance's own cap -- its in-flight request at parallelism one -- sends
	// the request to the old server.
	t.Run("the per-instance cap overrides a better-scoring busy instance", func(t *testing.T) {
		now := instanceCapacityNow
		r := NewResolver(mixedFleetStore(t, now, 0), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: views()})
		r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5", "qwen2.5#2"}}})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_old": 6}}, 0)
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: busy})
		adm := timeoutAdmission()
		r.SetAdmissionController(adm)

		target, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
		if err != nil || target.InstanceKey != "map_old#0" || adm.calls != 0 {
			t.Fatalf("Resolve = %q, %v after %d waits; want map_old#0 at once, every instance being at its own cap", target.InstanceKey, err, adm.calls)
		}
	})
	t.Run("everything at its cap queues on both servers and signals", func(t *testing.T) {
		now := instanceCapacityNow
		r := NewResolver(mixedFleetStore(t, now, 1), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: views()})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 2, "srv_old": 1}}, 0)
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: busy})
		adm := timeoutAdmission()
		sink := &fakeDemandSink{}
		r.SetAdmissionController(adm)
		r.SetDemandSink(sink)

		if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) {
			t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
		}
		servers := slices.Clone(adm.gotServers)
		slices.Sort(servers)
		if adm.calls != 1 || !slices.Equal(servers, []string{"srv_inst", "srv_old"}) {
			t.Fatalf("WaitForSlot calls %d on %v, want 1 on both servers", adm.calls, adm.gotServers)
		}
		if want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}}; !demandSignalsEqual(sink.got, want) {
			t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
		}
	})
}

// countingTelemetryStore counts TelemetryByServer reads per server.
type countingTelemetryStore struct {
	*MemoryStore
	reads map[string]int
}

func (s *countingTelemetryStore) TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error) {
	s.reads[serverID]++
	return s.MemoryStore.TelemetryByServer(ctx, serverID)
}

// The instances of one mapping share their server, so one scoring pass reads
// that server's telemetry once however many instances it scores.
func TestScoringReadsEachServersTelemetryOncePerPass(t *testing.T) {
	now := instanceCapacityNow
	store := &countingTelemetryStore{MemoryStore: mixedFleetStore(t, now, 0), reads: map[string]int{}}
	r := NewResolver(store, func() time.Time { return now }, nil)
	cands, err := store.ActiveMappingsForModel(context.Background(), "qwen-coder", APIFlavorOpenAI)
	if err != nil {
		t.Fatalf("ActiveMappingsForModel: %v", err)
	}
	pool := expandInstances(context.Background(), cands, &fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Running: []int{2, 3}}}})
	if len(pool) != 4 {
		t.Fatalf("pool = %d candidates, want 4", len(pool))
	}
	wantOncePerServer := func(pass string) {
		t.Helper()
		if store.reads["srv_inst"] != 1 || store.reads["srv_old"] != 1 {
			t.Fatalf("%s: telemetry reads = %v, want one per server", pass, store.reads)
		}
		store.reads = map[string]int{}
	}

	if _, ok, err := r.argmaxByScore(context.Background(), pool, "qwen-coder", false, now); err != nil || !ok {
		t.Fatalf("argmaxByScore = ok %v, err %v", ok, err)
	}
	wantOncePerServer("argmaxByScore")
	if _, err := r.filterBySpeedFloor(context.Background(), pool, "qwen-coder", 1); err != nil {
		t.Fatalf("filterBySpeedFloor: %v", err)
	}
	wantOncePerServer("filterBySpeedFloor")
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Running: []int{2, 3}}}})
	members := []GroupMember{{ID: "gm_q", GroupID: "grp1", MemberGatewayName: "qwen-coder"}}
	if _, err := r.orderMembersBySpeed(context.Background(), auth.Token{}, members, APIFlavorOpenAI, "openai_chat", nil, groupWalk{}); err != nil {
		t.Fatalf("orderMembersBySpeed: %v", err)
	}
	wantOncePerServer("orderMembersBySpeed")
}
