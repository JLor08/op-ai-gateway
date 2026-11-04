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

// A pinned request -- a token's affinity hit or a server override -- is served by
// the instances of its mapping on the pinned server: the pinned instance while it is
// free, a free sibling on that server otherwise. With none free a request overflows
// onto the pinned instance (pins never queue) -- onto one whose live slot is free when
// it needs one -- and only a live session, or a transcription finding every live slot
// held, parks for a slot on that server; both ask for another instance there. The
// pin itself outlives every instance change. These tests seed srv_inst (the pinned
// server, instances of map_inst) beside an idle srv_b serving the same model, which a
// pin must never move to.

var pinnedToken = auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}

// pinnedKey is the affinity key pinnedToken's chat requests for qwen-coder use.
var pinnedKey = AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI}

// audioPinKey is the affinity key pinnedToken's live sessions and transcriptions
// of qwen-coder use.
var audioPinKey = AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio}

// pinnedInstanceStore is instanceCandidateStore with affinity enabled on app_inst
// (which serves the audio flavor too, so a live session can resolve to it) and an
// idle second server srv_b serving qwen-coder from map_b.
func pinnedInstanceStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	return pinnedInstanceStoreWithCap(t, now, 0)
}

// pinnedInstanceStoreWithCap is pinnedInstanceStore with map_inst's
// max_concurrency set to maxConcurrency.
func pinnedInstanceStoreWithCap(t *testing.T, now time.Time, maxConcurrency int) *MemoryStore {
	t.Helper()
	store := instanceCandidateStore(t, now, maxConcurrency)
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
	seedPinFor(t, store, now, pinnedKey, "srv_inst", k)
}

// seedPinFor pins key to instance k of serverID's mapping (app_inst on srv_inst,
// app_<serverID> elsewhere).
func seedPinFor(t *testing.T, store *MemoryStore, now time.Time, key AffinityKey, serverID string, k int) {
	t.Helper()
	appID := "app_inst"
	if serverID != "srv_inst" {
		appID = "app_" + serverID
	}
	if err := store.UpsertAffinity(context.Background(), RouteAffinity{
		ID: affinityID(key), APITokenID: key.APITokenID, UserID: "usr_dev", Model: key.Model, APIFlavor: key.APIFlavor,
		ApplicationID: appID, ServerID: serverID, Instance: k,
		ExpiresAt: now.Add(30 * time.Minute), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
}

// storedPin is pinnedKey's affinity row; the test fails when there is none.
func storedPin(t *testing.T, store *MemoryStore) RouteAffinity {
	t.Helper()
	return storedPinFor(t, store, pinnedKey)
}

// storedPinFor is key's affinity row; the test fails when there is none.
func storedPinFor(t *testing.T, store *MemoryStore, key AffinityKey) RouteAffinity {
	t.Helper()
	aff, ok, err := store.Affinity(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Affinity = %v, %v; want the pin kept", ok, err)
	}
	return aff
}

// serveAudioOn lets serverID's application serve the audio flavor too.
func serveAudioOn(t *testing.T, store *MemoryStore, serverID string) {
	t.Helper()
	app, err := store.ApplicationByID(context.Background(), "app_"+serverID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorOpenAIAudio}
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
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
// instance 0 of its mapping, past its server's cap (max_concurrency 1 with nine in
// flight), never queued and never signalling -- whatever instance an older row
// names. A live session overridden onto that server is not parked either, though
// its one slot is held: its own slot acquire answers it, as before.
func TestAffinityNonReplicableUnchanged(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStoreWithCap(t, now, 1)
	seedInstancePin(t, store, now, 2)
	r := NewResolver(store, func() time.Time { return now }, nil)
	src := &fakeInstanceSource{views: map[string]InstanceView{}}
	r.SetInstanceSource(src)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_inst": 9}}, 0)
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 9}})
	r.SetLiveSlotChecker(&fakeLiveSlots{held: map[string]bool{"map_inst#0": true}})
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)

	// The control: with srv_b down, an unpinned request finds srv_inst at its cap
	// and parks.
	if err := store.SetServerHealth(context.Background(), "srv_b", HealthUnhealthy); err != nil {
		t.Fatalf("SetServerHealth: %v", err)
	}
	if _, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("unpinned Resolve = %v after %d waits; want ErrAdmissionQueueTimeout after 1, the server being at its cap", err, adm.calls)
	}
	adm.calls = 0
	override := func(req inference.Request) inference.Request {
		req.ServerOverrideID = "srv_inst"
		return req
	}
	for _, req := range []inference.Request{chatRequest("qwen-coder"), override(chatRequest("qwen-coder")), override(liveRequest("qwen-coder"))} {
		target, err := r.Resolve(context.Background(), pinnedToken, req)
		if err != nil || target.ServerID != "srv_inst" || target.InstanceKey != "map_inst#0" || target.ProviderModel != "qwen2.5" {
			t.Fatalf("Resolve (override %q, live %v) = %q on %q (%q), %v; want instance 0 on srv_inst", req.ServerOverrideID, req.LiveSession, target.InstanceKey, target.ServerID, target.ProviderModel, err)
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

// transcribingPinnedStore is pinnedInstanceStore with map_inst able to
// transcribe and pinnedToken's audio key pinned to instance 0 of map_inst.
func transcribingPinnedStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := pinnedInstanceStore(t, now)
	if err := store.UpsertMappingCapabilities(context.Background(), "map_inst", []CapabilityRow{
		{Capability: CapabilityTranscription, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	seedPinFor(t, store, now, audioPinKey, "srv_inst", 0)
	return store
}

// A pinned transcription needs a free live slot as an unpinned one does: it is
// never sent to an instance whose slot a live session holds. With a slot free on
// an instance that is only busy, it overflows onto that one -- so does a pinned
// live session that must not park -- and with every slot on the pinned server
// held it parks for one there, as a live session does, asking for another
// instance on that server.
func TestPinnedTranscriptionNeedsAFreeLiveSlot(t *testing.T) {
	now := instanceCapacityNow
	store := transcribingPinnedStore(t, now)
	load := map[string]int{"map_inst#0": 1, "map_inst#1": 2}
	r, adm, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 2, Running: []int{1}}, load)
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true}}
	r.SetLiveSlotChecker(slots)

	// Instance 1's slot is free, only its parallelism is taken.
	target, err := r.Resolve(context.Background(), pinnedToken, transcriptionRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("Resolve = %q, %v; want map_inst#1, not the pinned instance 0 whose live slot is held", target.InstanceKey, err)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, ServerID: "srv_inst", MappingIDs: []string{"map_inst"}}}
	if adm.calls != 0 || !demandSignalsEqual(sink.got, want) {
		t.Fatalf("waits %d, demand signals %+v; want none and %+v", adm.calls, sink.got, want)
	}
	for _, req := range []inference.Request{transcriptionRequest("qwen-coder"), liveRequest("qwen-coder")} {
		req.NoPark = true
		if target, err := r.Resolve(context.Background(), pinnedToken, req); err != nil || target.InstanceKey != "map_inst#1" || adm.calls != 0 {
			t.Fatalf("NoPark (live %v) Resolve = %q, %v after %d waits; want map_inst#1 at once", req.LiveSession, target.InstanceKey, err, adm.calls)
		}
	}

	// Every slot held: it parks on srv_inst and takes the slot that frees.
	sink.got = nil
	slots.held["map_inst#1"] = true
	parked := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			return ErrAdmissionQueueTimeout
		}
		slots.held["map_inst#1"], load["map_inst#1"] = false, 0
		return nil
	}}
	r.SetAdmissionController(parked)
	target, err = r.Resolve(context.Background(), pinnedToken, transcriptionRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("every slot held: Resolve = %q, %v; want map_inst#1, the slot that freed", target.InstanceKey, err)
	}
	if parked.calls != 1 || !slices.Equal(parked.gotServers, []string{"srv_inst"}) || !demandSignalsEqual(sink.got, want) {
		t.Fatalf("waits %d on %v, demand signals %+v; want 1 on [srv_inst] and %+v", parked.calls, parked.gotServers, sink.got, want)
	}
}

// A pinned live session that finds every live slot on its pinned server held
// parks there for one, bounded by the pinned application's queue timeout counted
// from its admission start, and takes the slot that frees; the pin follows it.
func TestAffinityLiveParkHonoursItsDeadline(t *testing.T) {
	now := instanceCapacityNow
	store := pinnedInstanceStore(t, now)
	app, err := store.ApplicationByID(context.Background(), "app_inst")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AdmissionQueueTimeoutSeconds = 30
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	seedPinFor(t, store, now, audioPinKey, "srv_inst", 0)
	clock := now
	r, _, sink := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}, map[string]int{})
	r.clock = func() time.Time { return clock }
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true, "map_inst#1": true}}
	r.SetLiveSlotChecker(slots)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call > 1 {
			return ErrAdmissionQueueTimeout
		}
		slots.held["map_inst#1"] = false
		return nil
	}}
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), pinnedToken, liveRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("Resolve = %q, %v; want map_inst#1, the slot that freed", target.InstanceKey, err)
	}
	if adm.calls != 1 || !slices.Equal(adm.gotServers, []string{"srv_inst"}) || adm.gotTimeout != 30*time.Second {
		t.Fatalf("WaitForSlot calls %d on %v for %v, want 1 on [srv_inst] for 30s", adm.calls, adm.gotServers, adm.gotTimeout)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, ServerID: "srv_inst", Live: true, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
	if aff := storedPinFor(t, store, audioPinKey); aff.ServerID != "srv_inst" || aff.Instance != 1 {
		t.Fatalf("pin = server %q instance %d, want srv_inst instance 1", aff.ServerID, aff.Instance)
	}

	// The deadline counts from the request's admission start: 10 s spent before
	// leave 20 s, and a wait that outlasts them ends in the queue's timeout without
	// a second wait.
	slots.held["map_inst#1"] = true
	deadline := &fakeAdmission{onWait: func(int) error {
		clock = clock.Add(21 * time.Second)
		return nil
	}}
	r.SetAdmissionController(deadline)
	req := liveRequest("qwen-coder")
	req.AdmissionStart = now.Add(-10 * time.Second)
	if _, err := r.Resolve(context.Background(), pinnedToken, req); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
	}
	if deadline.calls != 1 || deadline.gotTimeout != 20*time.Second {
		t.Fatalf("WaitForSlot calls %d for %v, want 1 for the 20s left", deadline.calls, deadline.gotTimeout)
	}
}

// A pinned request parked for a slot re-checks its pinned server on every turn:
// a pin whose server went down while it waited is passed over for a fresh
// selection, and an override answers that its server is unavailable -- unless it
// forces through an unreachable one, which keeps waiting there.
func TestPinnedParkRechecksThePinnedServer(t *testing.T) {
	setup := func(t *testing.T) (*MemoryStore, *Resolver, *fakeLiveSlots) {
		t.Helper()
		now := instanceCapacityNow
		store := pinnedInstanceStore(t, now)
		serveAudioOn(t, store, "srv_b")
		seedPinFor(t, store, now, audioPinKey, "srv_inst", 0)
		r, _, _ := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}, map[string]int{})
		slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true, "map_inst#1": true}}
		r.SetLiveSlotChecker(slots)
		return store, r, slots
	}
	// goesDown is a queue whose first wait takes srv_inst down; later waits free
	// instance 1's slot when free is set, and time out otherwise.
	goesDown := func(store *MemoryStore, slots *fakeLiveSlots, free bool) *fakeAdmission {
		return &fakeAdmission{onWait: func(call int) error {
			if call == 1 {
				return store.SetServerHealth(context.Background(), "srv_inst", HealthUnhealthy)
			}
			if !free {
				return ErrAdmissionQueueTimeout
			}
			slots.held["map_inst#1"] = false
			return nil
		}}
	}

	t.Run("affinity pin", func(t *testing.T) {
		store, r, slots := setup(t)
		adm := goesDown(store, slots, false)
		r.SetAdmissionController(adm)
		target, err := r.Resolve(context.Background(), pinnedToken, liveRequest("qwen-coder"))
		if err != nil || target.ServerID != "srv_b" || adm.calls != 1 {
			t.Fatalf("Resolve = %q on %q, %v after %d waits; want the fresh selection's srv_b after 1", target.InstanceKey, target.ServerID, err, adm.calls)
		}
	})
	t.Run("server override", func(t *testing.T) {
		store, r, slots := setup(t)
		adm := goesDown(store, slots, false)
		r.SetAdmissionController(adm)
		req := liveRequest("qwen-coder")
		req.ServerOverrideID = "srv_inst"
		if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrServerOverrideServerUnavailable) || adm.calls != 1 {
			t.Fatalf("Resolve err = %v after %d waits, want ErrServerOverrideServerUnavailable after 1", err, adm.calls)
		}
	})
	t.Run("server override forced through", func(t *testing.T) {
		store, r, slots := setup(t)
		adm := goesDown(store, slots, true)
		r.SetAdmissionController(adm)
		req := liveRequest("qwen-coder")
		req.ServerOverrideID = "srv_inst"
		req.ServerOverrideForceUnreachable = true
		if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#1" || adm.calls != 2 {
			t.Fatalf("Resolve = %q, %v after %d waits; want map_inst#1 after 2", target.InstanceKey, err, adm.calls)
		}
	})
}

// A pinned request that parked refreshes its pin only while the row is still the
// one it was served from: a pin that another request repointed, or that was
// deleted, while it waited is left as it is now.
func TestAffinityRefreshAfterParkKeepsANewerRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, store *MemoryStore)
		check  func(t *testing.T, store *MemoryStore)
	}{
		{"repointed", func(t *testing.T, store *MemoryStore) {
			seedPinFor(t, store, instanceCapacityNow, audioPinKey, "srv_b", 0)
		}, func(t *testing.T, store *MemoryStore) {
			if aff := storedPinFor(t, store, audioPinKey); aff.ServerID != "srv_b" || aff.ApplicationID != "app_srv_b" {
				t.Fatalf("pin = %q/%q, want the repointed srv_b/app_srv_b", aff.ServerID, aff.ApplicationID)
			}
		}},
		{"deleted", func(t *testing.T, store *MemoryStore) {
			if err := store.DeleteAffinity(context.Background(), audioPinKey); err != nil {
				t.Fatalf("DeleteAffinity: %v", err)
			}
		}, func(t *testing.T, store *MemoryStore) {
			if _, ok, err := store.Affinity(context.Background(), audioPinKey); err != nil || ok {
				t.Fatalf("Affinity = %v, %v; want the deleted pin to stay deleted", ok, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := instanceCapacityNow
			store := pinnedInstanceStore(t, now)
			seedPinFor(t, store, now, audioPinKey, "srv_inst", 0)
			r, _, _ := pinnedResolver(store, now, InstanceView{Replicable: true, Parallelism: 1, Running: []int{1}}, map[string]int{})
			slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true, "map_inst#1": true}}
			r.SetLiveSlotChecker(slots)
			r.SetAdmissionController(&fakeAdmission{onWait: func(call int) error {
				if call > 1 {
					return ErrAdmissionQueueTimeout
				}
				tc.change(t, store)
				slots.held["map_inst#1"] = false
				return nil
			}})
			target, err := r.Resolve(context.Background(), pinnedToken, liveRequest("qwen-coder"))
			if err != nil || target.InstanceKey != "map_inst#1" {
				t.Fatalf("Resolve = %q, %v; want map_inst#1 on the pinned server", target.InstanceKey, err)
			}
			tc.check(t, store)
		})
	}
}

// A group's pin names the instance its member was served on.
func TestGroupPinWritesInstance(t *testing.T) {
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
	r.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "qwen-coder", Priority: 0},
	}}}})

	target, err := r.Resolve(context.Background(), pinnedToken, chatRequest("coder-group"))
	if err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve = %q, %v; want the free instance map_inst#2", target.InstanceKey, err)
	}
	aff := storedPinFor(t, store, AffinityKey{APITokenID: "tok_dev", Model: "coder-group", APIFlavor: APIFlavorOpenAI})
	if aff.ResolvedModel != "qwen-coder" || aff.ServerID != "srv_inst" || aff.Instance != 2 {
		t.Fatalf("group pin = member %q on %q instance %d, want qwen-coder on srv_inst instance 2", aff.ResolvedModel, aff.ServerID, aff.Instance)
	}
}
