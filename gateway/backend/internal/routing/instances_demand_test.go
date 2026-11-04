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

// demandSignalsEqual compares demand signals field by field (a signal carries
// a slice, so == does not apply).
func demandSignalsEqual(a, b []DemandSignal) bool {
	return slices.EqualFunc(a, b, func(x, y DemandSignal) bool {
		return x.Model == y.Model && x.APIFlavor == y.APIFlavor && x.ServerID == y.ServerID && x.Live == y.Live && slices.Equal(x.MappingIDs, y.MappingIDs)
	})
}

// addAgentMapping seeds one more server_agent server, application and mapping
// serving gateway model gwName (upstream upName), with the given
// max_concurrency and affinity TTL.
func addAgentMapping(t *testing.T, store *MemoryStore, now time.Time, serverID, mappingID, gwName, upName string, maxConcurrency, affinityTTL int) {
	t.Helper()
	ctx := context.Background()
	appID := "app_" + serverID
	if err := store.CreateAIServer(ctx, AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := store.CreateApplication(ctx, Application{ID: appID, ServerID: serverID, Type: ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: affinityTTL, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gwName, AppModelName: upName, MaxConcurrency: maxConcurrency, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: serverID, ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry %s: %v", serverID, err)
	}
}

// Without an admission controller an all-busy request does not park: it fails
// open onto a busy candidate, and nothing is signalled, since no request waits
// for the instance a signal would start.
func TestNoDemandSignalWithoutAQueue(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_inst#2": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); err != nil {
		t.Fatalf("Resolve: %v (want the fail-open pick)", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("demand signals = %+v, want none on the fail-open path", sink.got)
	}
}

// A parked request signals once per turn it parks: a request that waits two
// turns signals twice, each time right before it waits.
func TestDemandSignalOncePerParkTurn(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_inst#2": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	var signalledBeforeWait []int
	adm := &fakeAdmission{onWait: func(call int) error {
		signalledBeforeWait = append(signalledBeforeWait, len(sink.got))
		if call == 2 {
			return ErrAdmissionQueueTimeout
		}
		return nil
	}}
	r.SetAdmissionController(adm)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout after two turns", err)
	}
	if !slices.Equal(signalledBeforeWait, []int{1, 2}) {
		t.Fatalf("signals seen at each wait = %v, want [1 2]: one per park turn, sent before the wait", signalledBeforeWait)
	}
	want := DemandSignal{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}
	if !demandSignalsEqual(sink.got, []DemandSignal{want, want}) {
		t.Fatalf("demand signals = %+v, want two of %+v", sink.got, want)
	}
}

// The signal names the instance-capable mappings the parked request could
// use: not a mapping on an agent without instances (at its own cap beside
// them), and not one on a server the principal is not provisioned for.
func TestDemandSignalNamesTheUsableInstanceMappings(t *testing.T) {
	now := instanceCapacityNow
	store := mixedFleetStore(t, now, 1)
	addAgentMapping(t, store, now, "srv_inst2", "map_inst2", "qwen-coder", "qwen2.5", 0, 0)
	addAgentMapping(t, store, now, "srv_far", "map_far", "qwen-coder", "qwen2.5", 0, 0)
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
		"map_inst":  {Replicable: true, Parallelism: 1},
		"map_inst2": {Replicable: true, Parallelism: 1},
		"map_far":   {Replicable: true, Parallelism: 1},
	}})
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_old": 1}}, 0)
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_inst2#0": 1, "map_far#0": 1}})
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_inst": true, "srv_inst2": true, "srv_old": true}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	r.SetAdmissionController(timeoutAdmission())

	if _, err := r.Resolve(context.Background(), auth.Token{ID: "tok"}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
	}
	if len(sink.got) != 1 {
		t.Fatalf("demand signals = %+v, want one", sink.got)
	}
	ids := slices.Clone(sink.got[0].MappingIDs)
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"map_inst", "map_inst2"}) {
		t.Fatalf("MappingIDs = %v, want [map_inst map_inst2]: the instance-capable mappings the request may use", sink.got[0].MappingIDs)
	}
}

// demandGroupResolver builds a resolver over a group "coder-group" of
// qwen-coder (map_inst on srv_inst, priority 0) and coder-b (map_b on srv_b,
// priority 1, affinity enabled so a session pins to it), in mode.
func demandGroupResolver(t *testing.T, now time.Time, mode string, bMaxConcurrency int) (*Resolver, *MemoryStore) {
	t.Helper()
	store := instanceCandidateStore(t, now, 0)
	addAgentMapping(t, store, now, "srv_b", "map_b", "coder-b", "coder-b-up", bMaxConcurrency, 1800)
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: mode}, members: []GroupMember{
			{ID: "gm_q", GroupID: "grp1", MemberGatewayName: "qwen-coder", Priority: 0},
			{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
		}},
	}})
	return r, store
}

// A group walk signals only when the request parks: a preferred member whose
// instances are all busy is skipped without a signal when a later member
// serves, and so is the climb_up probe of that member on the pinned session's
// next turn -- in neither case does a request wait for another instance.
func TestGroupSignalsNothingForSkippedOrProbedMembers(t *testing.T) {
	now := instanceCapacityNow
	r, _ := demandGroupResolver(t, now, modeClimbUp, 0)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_inst#2": 1}})
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{}}, 0)
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := chatRequest("coder-group")
	req.ClientSessionID = "s1"

	first, err := r.Resolve(context.Background(), token, req)
	if err != nil || first.Model != "coder-b" {
		t.Fatalf("first Resolve = %q, %v; want coder-b, the member that serves", first.Model, err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("after the walk skipped the busy member: demand signals = %+v, want none", sink.got)
	}
	second, err := r.Resolve(context.Background(), token, req)
	if err != nil || second.Model != "coder-b" {
		t.Fatalf("pinned Resolve = %q, %v; want coder-b (no climb to the busy member)", second.Model, err)
	}
	if len(sink.got) != 0 || adm.calls != 0 {
		t.Fatalf("after the climb probe: demand signals %+v, waits %d; want neither", sink.got, adm.calls)
	}
}

// A group whose every member is at capacity parks, and right before each wait
// it signals once for each member whose instances are all busy -- each by its
// own model and mappings, never the group -- and not for a member that has no
// instances.
func TestGroupParkSignalsEachBusyInstanceMember(t *testing.T) {
	t.Run("one instance member beside a capped member without instances", func(t *testing.T) {
		now := instanceCapacityNow
		r, _ := demandGroupResolver(t, now, "sticky", 1)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
		r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_b": 1}}, 0)
		sink := &fakeDemandSink{}
		r.SetDemandSink(sink)
		var signalledBeforeWait []int
		r.SetAdmissionController(&fakeAdmission{onWait: func(call int) error {
			signalledBeforeWait = append(signalledBeforeWait, len(sink.got))
			if call == 2 {
				return ErrAdmissionQueueTimeout
			}
			return nil
		}})

		if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("coder-group")); !errors.Is(err, ErrAdmissionQueueTimeout) {
			t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
		}
		if !slices.Equal(signalledBeforeWait, []int{1, 2}) {
			t.Fatalf("signals seen at each wait = %v, want [1 2]", signalledBeforeWait)
		}
		want := DemandSignal{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}
		if !demandSignalsEqual(sink.got, []DemandSignal{want, want}) {
			t.Fatalf("demand signals = %+v, want two of %+v", sink.got, want)
		}
	})
	t.Run("two instance members", func(t *testing.T) {
		now := instanceCapacityNow
		r, _ := demandGroupResolver(t, now, "sticky", 0)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
			"map_inst": {Replicable: true, Parallelism: 1},
			"map_b":    {Replicable: true, Parallelism: 1},
		}})
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_b#0": 1}})
		sink := &fakeDemandSink{}
		r.SetDemandSink(sink)
		r.SetAdmissionController(timeoutAdmission())

		if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("coder-group")); !errors.Is(err, ErrAdmissionQueueTimeout) {
			t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
		}
		want := []DemandSignal{
			{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}},
			{Model: "coder-b", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_b"}},
		}
		if !demandSignalsEqual(sink.got, want) {
			t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
		}
	})
}

// A group without an admission controller never parks, so it never signals:
// its all-busy members fail open.
func TestGroupWithoutQueueSignalsNothing(t *testing.T) {
	now := instanceCapacityNow
	r, _ := demandGroupResolver(t, now, "sticky", 0)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
		"map_inst": {Replicable: true, Parallelism: 1},
		"map_b":    {Replicable: true, Parallelism: 1},
	}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_b#0": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("coder-group")); err != nil {
		t.Fatalf("Resolve: %v (want the fail-open pick)", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("demand signals = %+v, want none without a queue", sink.got)
	}
}

// roomAdmission is a test admission controller that reports whether it has room
// (AdmissionRoom) and, like the real queue, refuses a waiter as full when it has
// none; with room every wait times out.
type roomAdmission struct {
	room  bool
	calls int
}

func (a *roomAdmission) HasRoom() bool { return a.room }

func (a *roomAdmission) WaitForSlot(context.Context, []string, time.Duration) error {
	a.calls++
	if !a.room {
		return ErrAdmissionQueueFull
	}
	return ErrAdmissionQueueTimeout
}

// A request the admission queue turns away as full never parks, so it signals no
// demand -- a refusal must not start an instance -- for a plain model and a group
// alike; with room in the queue the same request signals as it parks.
func TestNoDemandSignalWhenTheQueueIsFull(t *testing.T) {
	plain := func(t *testing.T) (*Resolver, inference.Request) {
		now := instanceCapacityNow
		r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
		return r, chatRequest("qwen-coder")
	}
	group := func(t *testing.T) (*Resolver, inference.Request) {
		r, _ := demandGroupResolver(t, instanceCapacityNow, "sticky", 0)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
			"map_inst": {Replicable: true, Parallelism: 1},
			"map_b":    {Replicable: true, Parallelism: 1},
		}})
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_b#0": 1}})
		return r, chatRequest("coder-group")
	}
	for name, build := range map[string]func(*testing.T) (*Resolver, inference.Request){"plain": plain, "group": group} {
		t.Run(name, func(t *testing.T) {
			r, req := build(t)
			sink := &fakeDemandSink{}
			r.SetDemandSink(sink)
			full := &roomAdmission{}
			r.SetAdmissionController(full)
			if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrAdmissionQueueFull) {
				t.Fatalf("Resolve err = %v, want ErrAdmissionQueueFull", err)
			}
			if len(sink.got) != 0 {
				t.Fatalf("demand signals = %+v for a request the full queue refused, want none", sink.got)
			}
			r.SetAdmissionController(&roomAdmission{room: true})
			if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrAdmissionQueueTimeout) {
				t.Fatalf("Resolve with room err = %v, want ErrAdmissionQueueTimeout", err)
			}
			if len(sink.got) == 0 {
				t.Fatal("no demand signalled by a request that parked, want one per busy member")
			}
		})
	}
}

// A request whose admission deadline has already passed is refused without
// waiting, and so without signalling: once its first wait ran the deadline out,
// the next turn ends the request with the one signal its one park sent.
func TestNoDemandSignalPastTheDeadline(t *testing.T) {
	now := instanceCapacityNow
	store := instanceCandidateStore(t, now, 0)
	app, err := store.ApplicationByID(context.Background(), "app_inst")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AdmissionQueueTimeoutSeconds = 10
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	clock := now
	r := NewResolver(store, func() time.Time { return clock }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := &fakeAdmission{onWait: func(int) error {
		clock = clock.Add(11 * time.Second) // the wait outlasts the deadline
		return nil
	}}
	r.SetAdmissionController(adm)

	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
	}
	if len(sink.got) != 1 {
		t.Fatalf("demand signals = %+v, want the one the request's one park sent", sink.got)
	}
}

// A request that names when its admission began (AdmissionStart: one client call
// resolving again, as a live session that lost a race for a slot does) waits only
// for what is left of the queue timeout counted from then -- for a plain model and
// a group alike -- and one whose timeout has run out is refused without a wait and
// without a signal.
func TestAdmissionStartKeepsOneDeadline(t *testing.T) {
	setup := func(t *testing.T, group bool) (*Resolver, *MemoryStore, inference.Request) {
		t.Helper()
		if group {
			r, store := demandGroupResolver(t, instanceCapacityNow, "sticky", 0)
			r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
				"map_inst": {Replicable: true, Parallelism: 1},
				"map_b":    {Replicable: true, Parallelism: 1},
			}})
			r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1, "map_b#0": 1}})
			return r, store, chatRequest("coder-group")
		}
		store := instanceCandidateStore(t, instanceCapacityNow, 0)
		r := NewResolver(store, func() time.Time { return instanceCapacityNow }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
		r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
		return r, store, chatRequest("qwen-coder")
	}
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "group"}[group], func(t *testing.T) {
			r, store, req := setup(t, group)
			for _, id := range []string{"app_inst", "app_srv_b"} {
				app, err := store.ApplicationByID(context.Background(), id)
				if err != nil {
					continue // the plain store has no srv_b
				}
				app.AdmissionQueueTimeoutSeconds = 10
				if err := store.UpdateApplication(context.Background(), app); err != nil {
					t.Fatalf("UpdateApplication %s: %v", id, err)
				}
			}
			sink := &fakeDemandSink{}
			r.SetDemandSink(sink)
			adm := timeoutAdmission()
			r.SetAdmissionController(adm)

			req.AdmissionStart = instanceCapacityNow.Add(-4 * time.Second)
			if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
				t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
			}
			if adm.gotTimeout != 6*time.Second {
				t.Fatalf("the wait was given %v, want the 6s left of the 10s timeout begun 4s ago", adm.gotTimeout)
			}
			signals := len(sink.got)
			req.AdmissionStart = instanceCapacityNow.Add(-10 * time.Second)
			if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
				t.Fatalf("Resolve past the deadline = %v after %d waits, want ErrAdmissionQueueTimeout without another wait", err, adm.calls)
			}
			if len(sink.got) != signals {
				t.Fatalf("demand signals = %+v, want none from the request whose deadline had passed", sink.got[signals:])
			}
		})
	}
}
