// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// A pinned request -- a token's affinity hit or a server override -- is served by
// the instances of its mapping on the pinned server: the pinned instance while it is
// free, a free sibling on that server otherwise. With none free a request that is not
// a live session overflows onto the pinned instance (pins never queue) and a live
// session parks for a slot on that server; both ask for another instance there. The
// pin itself outlives every instance change. These tests seed srv_inst (the pinned
// server, instances of map_inst) beside an idle srv_b serving the same model, which a
// pin must never move to.

var pinnedToken = auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}

// pinnedKey is the affinity key pinnedToken's chat requests for qwen-coder use.
var pinnedKey = AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI}

// pinnedInstanceStore is instanceCandidateStore with affinity enabled on app_inst
// (which serves the audio flavor too, so a live session can resolve to it) and an
// idle second server srv_b serving qwen-coder from map_b.
func pinnedInstanceStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := instanceCandidateStore(t, now, 0)
	app, err := store.ApplicationByID(context.Background(), "app_inst")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AffinityTTLSeconds = 1800
	app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorOpenAIAudio}
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	addAgentMapping(t, store, now, "srv_b", "map_b", "qwen-coder", "qwen2.5", 0, 1800)
	return store
}

// seedInstancePin pins pinnedKey to instance k of map_inst on srv_inst.
func seedInstancePin(t *testing.T, store *MemoryStore, now time.Time, k int) {
	t.Helper()
	if err := store.UpsertAffinity(context.Background(), RouteAffinity{
		ID: affinityID(pinnedKey), APITokenID: pinnedKey.APITokenID, UserID: "usr_dev", Model: pinnedKey.Model, APIFlavor: pinnedKey.APIFlavor,
		ApplicationID: "app_inst", ServerID: "srv_inst", Instance: k,
		ExpiresAt: now.Add(30 * time.Minute), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
}

// storedPin is pinnedKey's affinity row; the test fails when there is none.
func storedPin(t *testing.T, store *MemoryStore) RouteAffinity {
	t.Helper()
	aff, ok, err := store.Affinity(context.Background(), pinnedKey)
	if err != nil || !ok {
		t.Fatalf("Affinity = %v, %v; want the pin kept", ok, err)
	}
	return aff
}

// pinnedResolver is a resolver over store whose instance source reports map_inst
// with view (map_b runs no instances), with the given per-instance load, a queue
// whose every wait times out, and a demand sink.
func pinnedResolver(store *MemoryStore, now time.Time, view InstanceView, load map[string]int) (*Resolver, *fakeAdmission, *fakeDemandSink) {
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": view}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: load})
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)
	return r, adm, sink
}

// The pin a fresh selection writes names the instance it chose.
func TestAffinityWritesInstance(t *testing.T) {
	now := instanceCapacityNow
	store := instanceCandidateStore(t, now, 0)
	app, err := store.ApplicationByID(context.Background(), "app_inst")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AffinityTTLSeconds = 1800
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	r, _, _ := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{2}}, map[string]int{"map_inst#0": 1})

	target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve = %q, %v; want the free instance map_inst#2", target.InstanceKey, err)
	}
	if aff := storedPin(t, store); aff.ServerID != "srv_inst" || aff.Instance != 2 {
		t.Fatalf("pin = server %q instance %d, want srv_inst instance 2", aff.ServerID, aff.Instance)
	}
}

// A pin to a free instance is served there, though the plain selection would choose
// another one.
func TestAffinityReusesPinnedInstance(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	r, adm, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1, 2}}, map[string]int{})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5", "qwen2.5#1", "qwen2.5#2"}, "srv_b": {"qwen2.5"}}})

	// The control: a token without a pin is served on another instance.
	plain, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder"))
	if err != nil || plain.InstanceKey == "map_inst#2" {
		t.Fatalf("plain Resolve = %q, %v; want a selection other than map_inst#2", plain.InstanceKey, err)
	}
	seedInstancePin(t, store, now, 2)
	target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#2" || target.ProviderModel != "qwen2.5#2" {
		t.Fatalf("pinned Resolve = %q (%q), %v; want the pinned map_inst#2 (qwen2.5#2)", target.InstanceKey, target.ProviderModel, err)
	}
	if !target.InstanceCapable || !target.InstancePool {
		t.Fatalf("target capable %v pool %v, want both for an instance of a replicable spec", target.InstanceCapable, target.InstancePool)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %v; want neither while the pinned instance is free", adm.calls, sink.got)
	}
	if aff := storedPin(t, store); aff.Instance != 2 {
		t.Fatalf("pin instance = %d, want 2", aff.Instance)
	}
}

// A pinned instance at its cap gives way to the least loaded free sibling on the
// pinned server -- never to the idle other server -- and the pin follows the request
// there.
func TestAffinityPinnedInstanceBusyUsesSibling(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	seedInstancePin(t, store, now, 2)
	r, adm, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 2, Running: []int{1, 2}},
		map[string]int{"map_inst#0": 1, "map_inst#2": 2})

	target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("Resolve = %q, %v; want map_inst#1, the least loaded free instance on the pinned server", target.InstanceKey, err)
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %v; want neither while a sibling is free", adm.calls, sink.got)
	}
	if aff := storedPin(t, store); aff.ServerID != "srv_inst" || aff.Instance != 1 {
		t.Fatalf("pin = server %q instance %d, want srv_inst instance 1", aff.ServerID, aff.Instance)
	}
}

// With every instance on the pinned server at its cap, a request that is not a live
// session is still served at once, on the pinned instance, and asks for another
// instance on that server.
func TestAffinityAllBusyOverflowsNonLive(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	seedInstancePin(t, store, now, 2)
	r, adm, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1, 2}},
		map[string]int{"map_inst#0": 1, "map_inst#1": 1, "map_inst#2": 1})

	target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve = %q, %v; want the pinned map_inst#2", target.InstanceKey, err)
	}
	if adm.calls != 0 {
		t.Fatalf("WaitForSlot calls = %d, want none: a pinned request never queues", adm.calls)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ServerID: "srv_inst", MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
	if aff := storedPin(t, store); aff.ServerID != "srv_inst" || aff.Instance != 2 {
		t.Fatalf("pin = server %q instance %d, want srv_inst instance 2", aff.ServerID, aff.Instance)
	}

	// A check-only resolve overflows the same way and signals nothing.
	sink.got = nil
	check := chatRequest("qwen-coder")
	check.CheckOnly = true
	if target, err := r.Resolve(context.Background(), pinnedToken, check); err != nil || target.InstanceKey != "map_inst#2" || len(sink.got) != 0 {
		t.Fatalf("check-only Resolve = %q, %v with signals %v; want map_inst#2 and none", target.InstanceKey, err, sink.got)
	}
}

// An override's live session that finds every live slot on the named server held
// parks for a slot there -- waiting on that server alone and asking for a live
// instance on it -- and takes the slot that frees.
func TestOverrideLiveAllBusyParks(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	r, _, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}, map[string]int{})
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true, "map_inst#1": true}}
	r.SetLiveSlotChecker(slots)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			return ErrAdmissionQueueTimeout
		}
		slots.held["map_inst#1"] = false // instance 1's session ends while the request waits
		return nil
	}}
	r.SetAdmissionController(adm)
	req := liveRequest("qwen-coder")
	req.ServerOverrideID = "srv_inst"

	target, err := r.Resolve(context.Background(), pinnedToken, req)
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("Resolve = %q, %v; want map_inst#1, the slot that freed", target.InstanceKey, err)
	}
	if adm.calls != 1 || len(adm.gotServers) != 1 || adm.gotServers[0] != "srv_inst" {
		t.Fatalf("WaitForSlot calls %d on %v, want 1 on [srv_inst]", adm.calls, adm.gotServers)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, ServerID: "srv_inst", Live: true, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}

	// Every slot staying held ends in the queue's answer, not a busy slot.
	slots.held["map_inst#1"] = true
	r.SetAdmissionController(timeoutAdmission())
	if _, err := r.Resolve(context.Background(), pinnedToken, req); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
	}
}

// An override picks among the named server's instances like a pin: the request's
// preferred instance while it is free, the least loaded free one otherwise, and with
// none free (not a live session) the preferred one without waiting.
func TestOverridePicksInstancesOnTheServer(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	load := map[string]int{"map_inst#0": 1}
	r, adm, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1, 2}}, load)
	req := chatRequest("qwen-coder")
	req.ServerOverrideID = "srv_inst"

	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("Resolve = %q, %v; want map_inst#1, the first free instance", target.InstanceKey, err)
	}
	req.PreferredInstanceKey = "map_inst#2"
	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("preferring map_inst#2: Resolve = %q, %v; want it", target.InstanceKey, err)
	}
	load["map_inst#1"], load["map_inst#2"] = 1, 1
	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("all busy: Resolve = %q, %v; want the preferred map_inst#2", target.InstanceKey, err)
	}
	if adm.calls != 0 || len(sink.got) != 1 || sink.got[0].ServerID != "srv_inst" {
		t.Fatalf("waits %d, demand signals %+v; want none and one for srv_inst", adm.calls, sink.got)
	}
	// An instance the request found gone is passed over.
	load["map_inst#1"], load["map_inst#2"] = 0, 0
	req.ExcludeInstanceKeys = []string{"map_inst#1"}
	req.PreferredInstanceKey = ""
	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("map_inst#1 excluded: Resolve = %q, %v; want map_inst#2", target.InstanceKey, err)
	}
}

// A pinned instance that is gone -- the agent retired it, or restarted and no longer
// reports instances at all -- is no error: the request is served on the pinned server
// as it is now, and the pin stays.
func TestAffinityPinnedInstanceVanishedFallsBack(t *testing.T) {
	t.Run("instance retired", func(t *testing.T) {
		now := instanceCapacityNow
		store := pinnedInstanceStore(t, now)
		seedInstancePin(t, store, now, 3)
		r, adm, _ := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}, map[string]int{"map_inst#0": 1})

		target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
		if err != nil || target.InstanceKey != "map_inst#1" {
			t.Fatalf("Resolve = %q, %v; want map_inst#1, the free instance on the pinned server", target.InstanceKey, err)
		}
		if adm.calls != 0 {
			t.Fatalf("WaitForSlot calls = %d, want none", adm.calls)
		}
		if aff := storedPin(t, store); aff.ServerID != "srv_inst" || aff.Instance != 1 {
			t.Fatalf("pin = server %q instance %d, want srv_inst instance 1", aff.ServerID, aff.Instance)
		}
	})
	t.Run("agent reports no instances", func(t *testing.T) {
		now := instanceCapacityNow
		store := pinnedInstanceStore(t, now)
		seedInstancePin(t, store, now, 3)
		r := NewResolver(store, func() time.Time { return now }, nil)
		r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{}})

		target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("qwen-coder"))
		if err != nil || target.ServerID != "srv_inst" || target.InstanceKey != "map_inst#0" || target.ProviderModel != "qwen2.5" {
			t.Fatalf("Resolve = %q on %q (%q), %v; want instance 0 on srv_inst", target.InstanceKey, target.ServerID, target.ProviderModel, err)
		}
		if aff := storedPin(t, store); aff.ServerID != "srv_inst" {
			t.Fatalf("pin server = %q, want srv_inst", aff.ServerID)
		}
	})
}

// A pin to a mapping that does not run instances is served exactly as before: on
// instance 0 of its mapping, judged by no cap, never queued and never signalling --
// whatever instance an older row names.
func TestAffinityNonReplicableUnchanged(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	seedInstancePin(t, store, now, 2)
	r := NewResolver(store, func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{}}
	r.SetInstanceSource(src)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 9}}, 0)
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 9}})
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)

	for _, req := range []inference.Request{chatRequest("qwen-coder"), func() inference.Request {
		o := chatRequest("qwen-coder")
		o.ServerOverrideID = "srv_inst"
		return o
	}()} {
		target, err := r.Resolve(context.Background(), pinnedToken, req)
		if err != nil || target.ServerID != "srv_inst" || target.InstanceKey != "map_inst#0" || target.ProviderModel != "qwen2.5" {
			t.Fatalf("Resolve (override %q) = %q on %q (%q), %v; want instance 0 on srv_inst", req.ServerOverrideID, target.InstanceKey, target.ServerID, target.ProviderModel, err)
		}
		if target.InstanceCapable || target.InstancePool {
			t.Fatalf("target capable %v pool %v, want neither for a mapping without instances", target.InstanceCapable, target.InstancePool)
		}
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %v; want neither", adm.calls, sink.got)
	}
	if aff := storedPin(t, store); aff.ServerID != "srv_inst" {
		t.Fatalf("pin server = %q, want srv_inst", aff.ServerID)
	}
}
