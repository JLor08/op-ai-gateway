// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// noParkRequest is a chat request for model that must not wait in the
// admission queue (inference.Request.NoPark).
func noParkRequest(model string) inference.Request {
	req := chatRequest(model)
	req.NoPark = true
	return req
}

// A request that must not park finds every instance at its cap: it never
// waits, is served by the least loaded instance, and signals the demand
// itself -- once, naming the instance-capable mappings it could use -- since
// no park turn will.
func TestNoParkPicksTheLeastLoadedInstanceAndSignalsOnce(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2, 3}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 3, "map_inst#2": 1, "map_inst#3": 2}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, noParkRequest("qwen-coder"))
	if err != nil {
		t.Fatalf("Resolve: %v, want the least loaded instance", err)
	}
	if adm.calls != 0 {
		t.Fatalf("admission waits = %d, want none for a request that must not park", adm.calls)
	}
	if target.InstanceKey != "map_inst#2" || target.ProviderModel != "qwen2.5#2" || !target.InstanceCapable || !target.InstancePool {
		t.Fatalf("target = %q (%q, capable %v, pool %v), want map_inst#2 (qwen2.5#2), the least loaded instance", target.InstanceKey, target.ProviderModel, target.InstanceCapable, target.InstancePool)
	}
	want := DemandSignal{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}
	if !demandSignalsEqual(sink.got, []DemandSignal{want}) {
		t.Fatalf("demand signals = %+v, want exactly %+v", sink.got, want)
	}
}

// A request that must not park and finds a free instance is served there as
// any request is, and signals nothing: no instance is missing.
func TestNoParkUnderCapSignalsNothing(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, noParkRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve = %q, %v; want map_inst#2, the free instance", target.InstanceKey, err)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %+v; want neither", adm.calls, sink.got)
	}
}

// A request that must not park on a model without instances, at its server
// cap, is served there anyway -- it never waits -- and signals nothing: no
// instance could be started for it.
func TestNoParkWithoutInstancesFailsOpen(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 1), func() time.Time { return now }, nil)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 1}}, 0)
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, noParkRequest("qwen-coder"))
	if err != nil || target.RouteID != "map_inst" {
		t.Fatalf("Resolve = %q, %v; want map_inst, served at its cap", target.RouteID, err)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %+v; want neither", adm.calls, sink.got)
	}
}

// A group whose every member is at capacity, resolved for a request that must
// not park, never waits: it is served by its first member's least loaded
// instance, and signals once, for that member by its own model.
func TestNoParkGroupServesTheFirstBusyMember(t *testing.T) {
	now := instanceCapacityNow
	r, _ := demandGroupResolver(t, now, "sticky", 0)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{
		"map_inst": {Replicable: true, Parallelism: 1, Running: []int{1}},
		"map_b":    {Replicable: true, Parallelism: 1},
	}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 2, "map_inst#1": 1, "map_b#0": 1}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, noParkRequest("coder-group"))
	if err != nil {
		t.Fatalf("Resolve: %v, want the first member's least loaded instance", err)
	}
	if adm.calls != 0 {
		t.Fatalf("admission waits = %d, want none", adm.calls)
	}
	if target.Model != "qwen-coder" || target.InstanceKey != "map_inst#1" {
		t.Fatalf("target = %s on %q, want qwen-coder on map_inst#1", target.Model, target.InstanceKey)
	}
	want := DemandSignal{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, MappingIDs: []string{"map_inst"}}
	if !demandSignalsEqual(sink.got, []DemandSignal{want}) {
		t.Fatalf("demand signals = %+v, want exactly %+v", sink.got, want)
	}
}
