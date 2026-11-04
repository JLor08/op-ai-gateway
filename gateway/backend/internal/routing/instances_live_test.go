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

// fakeLiveSlots is a test LiveSlotChecker: the instance keys whose live slot
// is held. asked records every key it was asked about.
type fakeLiveSlots struct {
	held  map[string]bool
	asked []string
}

func (f *fakeLiveSlots) LiveHeld(instanceKey string) bool {
	f.asked = append(f.asked, instanceKey)
	return f.held[instanceKey]
}

func liveRequest(model string) inference.Request {
	return inference.Request{Model: model, APIFlavor: APIFlavorOpenAIAudio, LiveSession: true}
}

// liveInstanceStore is instanceCandidateStore's qwen-coder mapping serving the
// audio flavor too, so a live session can resolve to it.
func liveInstanceStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := instanceCandidateStore(t, now, 0)
	app, err := store.ApplicationByID(context.Background(), "app_inst")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorOpenAIAudio}
	if err := store.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	return store
}

// A live session is never routed to an instance whose live slot is held: with
// instance 0's slot taken the session lands on instance 1, while a request
// that is not a live session is judged without the slots.
func TestLiveSessionSkipsAHeldInstance(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(liveInstanceStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{1}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5"}}})
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true}}
	r.SetLiveSlotChecker(slots)
	r.SetAdmissionController(timeoutAdmission())

	target, err := r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("live Resolve = %q, %v; want map_inst#1, the instance whose slot is free", target.InstanceKey, err)
	}
	if !target.InstanceCapable {
		t.Fatal("target.InstanceCapable = false, want true for an instance of a replicable spec")
	}
	plain := liveRequest("qwen-coder")
	plain.LiveSession = false
	slots.asked = nil
	if target, err := r.Resolve(context.Background(), auth.Token{}, plain); err != nil || target.InstanceKey != "map_inst#0" {
		t.Fatalf("non-live Resolve = %q, %v; want the loaded instance 0, judged without the live slots", target.InstanceKey, err)
	}
	if len(slots.asked) != 0 {
		t.Fatalf("live slots asked %v for a request that is not a live session, want none", slots.asked)
	}
}

// Every live slot of a replicable model held parks the live session in the
// admission queue, and the demand it signals is for a live slot.
func TestLiveSessionAllHeldParksAndSignalsLive(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(liveInstanceStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{1}}}})
	r.SetLiveSlotChecker(&fakeLiveSlots{held: map[string]bool{"map_inst#0": true, "map_inst#1": true}})
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	if _, err := r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("Resolve = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, Live: true, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
}

// A mapping without instances is not judged by its live slot: the resolver
// routes to it as before, and the session's own slot acquire answers busy.
func TestLiveSlotIgnoredForAMappingWithoutInstances(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(liveInstanceStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{}})
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true}}
	r.SetLiveSlotChecker(slots)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#0" || adm.calls != 0 {
		t.Fatalf("Resolve = %q, %v after %d waits; want map_inst#0 at once", target.InstanceKey, err, adm.calls)
	}
	if target.InstanceCapable || target.InstancePool {
		t.Fatalf("target capable %v pool %v, want neither for a pool without instances", target.InstanceCapable, target.InstancePool)
	}
	if len(slots.asked) != 0 {
		t.Fatalf("live slots asked %v, want none for a mapping without instances", slots.asked)
	}
}

// An instance the request names in ExcludeInstanceKeys is never selected --
// not even when it would win -- and a request that excludes every candidate
// has no healthy host.
func TestExcludedInstanceIsNeverSelected(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#2"}}})
	r.SetAdmissionController(timeoutAdmission())

	req := chatRequest("qwen-coder")
	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve = %q, %v; want the loaded map_inst#2", target.InstanceKey, err)
	}
	req.ExcludeInstanceKeys = []string{"map_inst#2"}
	if target, err := r.Resolve(context.Background(), auth.Token{}, req); err != nil || target.InstanceKey != "map_inst#0" {
		t.Fatalf("Resolve excluding map_inst#2 = %q, %v; want map_inst#0", target.InstanceKey, err)
	}
	req.ExcludeInstanceKeys = []string{"map_inst#2", "map_inst#0"}
	if _, err := r.Resolve(context.Background(), auth.Token{}, req); !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("Resolve excluding every instance err = %v, want ErrNoHealthyHost", err)
	}
}

// A parked request keeps its exclusions on every admission turn: an excluded
// instance that frees while it waits is still never selected.
func TestExclusionHoldsAcrossAdmissionTurns(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(instanceCandidateStore(t, now, 0), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	activity := &fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}}
	r.SetInstanceActivityChecker(activity)
	adm := &fakeAdmission{onWait: func(call int) error {
		if call == 1 {
			activity.byKey["map_inst#0"] = 0
			return nil
		}
		return ErrAdmissionQueueTimeout
	}}
	r.SetAdmissionController(adm)

	req := chatRequest("qwen-coder")
	req.ExcludeInstanceKeys = []string{"map_inst#2"}
	target, err := r.Resolve(context.Background(), auth.Token{}, req)
	if err != nil || target.InstanceKey != "map_inst#0" || adm.calls != 1 {
		t.Fatalf("Resolve = %q, %v after %d waits; want map_inst#0 after one wait", target.InstanceKey, err, adm.calls)
	}
	if slices.Contains(activity.asked, "map_inst#2") {
		t.Fatalf("instance activity asked %v: the excluded instance was judged, want it filtered out first", activity.asked)
	}
}

// A session pinned to an instance (instance 0 here) that the request has lost
// is served by another instance on the pinned server rather than sent back to
// it.
func TestExclusionPassesOverAnAffinityPin(t *testing.T) {
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
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	// Fresh selection prefers the loaded instance 2; only the pin sends a request to 0.
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#2"}}})
	token := auth.Token{ID: "tok", UserID: "u1", Active: true}
	req := chatRequest("qwen-coder")
	req.ClientSessionID = "s1"
	key := AffinityKey{APITokenID: "tok", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "s1"}
	if err := store.UpsertAffinity(context.Background(), RouteAffinity{ID: affinityID(key), APITokenID: "tok", UserID: "u1", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "s1", ApplicationID: "app_inst", ServerID: "srv_inst", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}

	if target, err := r.Resolve(context.Background(), token, req); err != nil || target.InstanceKey != "map_inst#0" {
		t.Fatalf("pinned Resolve = %q, %v; want the pin's map_inst#0", target.InstanceKey, err)
	}
	req.ExcludeInstanceKeys = []string{"map_inst#0"}
	if target, err := r.Resolve(context.Background(), token, req); err != nil || target.InstanceKey != "map_inst#2" {
		t.Fatalf("Resolve excluding the pinned instance = %q, %v; want map_inst#2", target.InstanceKey, err)
	}
}

// A token's affinity pin and a server override are served by the mapping's
// instances on the pinned server, and their target says it is an instance of a
// replicable spec -- the contract Target.InstanceCapable documents: the pin on
// the instance it names (0 here), the override on the loaded one of its idle
// instances.
func TestPinnedAndOverrideTargetsAreInstances(t *testing.T) {
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
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{2}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5#2"}}})
	token := auth.Token{ID: "tok", UserID: "u1", Active: true}
	req := chatRequest("qwen-coder")
	req.ClientSessionID = "s1"
	key := AffinityKey{APITokenID: "tok", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "s1"}
	if err := store.UpsertAffinity(context.Background(), RouteAffinity{ID: affinityID(key), APITokenID: "tok", UserID: "u1", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "s1", ApplicationID: "app_inst", ServerID: "srv_inst", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}

	if fresh, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); err != nil || fresh.InstanceKey != "map_inst#2" || !fresh.InstanceCapable {
		t.Fatalf("fresh Resolve = %q capable %v, %v; want the loaded map_inst#2, instance-capable", fresh.InstanceKey, fresh.InstanceCapable, err)
	}
	if pinned, err := r.Resolve(context.Background(), token, req); err != nil || pinned.InstanceKey != "map_inst#0" || !pinned.InstanceCapable || !pinned.InstancePool {
		t.Fatalf("pinned Resolve = %q capable %v pool %v, %v; want the pinned map_inst#0, instance-capable", pinned.InstanceKey, pinned.InstanceCapable, pinned.InstancePool, err)
	}
	override := chatRequest("qwen-coder")
	override.ServerOverrideID = "srv_inst"
	if target, err := r.Resolve(context.Background(), auth.Token{}, override); err != nil || target.InstanceKey != "map_inst#2" || !target.InstanceCapable || !target.InstancePool {
		t.Fatalf("server-override Resolve = %q capable %v pool %v, %v; want the loaded map_inst#2, instance-capable", target.InstanceKey, target.InstanceCapable, target.InstancePool, err)
	}
}

// transcriptionRequest is a buffered transcription's request: the audio flavor
// and the transcription capability, no live session.
func transcriptionRequest(model string) inference.Request {
	return inference.Request{Model: model, APIFlavor: APIFlavorOpenAIAudio, RequiredCapabilities: []string{CapabilityTranscription}}
}

// liveTranscriptionStore is liveInstanceStore with map_inst able to
// transcribe, so a transcription request resolves to it.
func liveTranscriptionStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := liveInstanceStore(t, now)
	if err := store.UpsertMappingCapabilities(context.Background(), "map_inst", []CapabilityRow{
		{Capability: CapabilityTranscription, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	return store
}

// Every transcription needs its instance's live slot free, not only a live
// session: the upstream serves one transcription at a time while a live session
// holds it. So a buffered transcription passes over an instance whose live slot
// is held -- even below its parallelism, where the session's one request leaves
// room -- and parks, signalling demand that is not for a live slot, when every
// instance's slot is held.
func TestTranscriptionPassesOverALiveHeldInstance(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(liveTranscriptionStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 2, Running: []int{1}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5"}}})
	// The live session on instance 0 is one request of its two.
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: map[string]int{"map_inst#0": 1}})
	slots := &fakeLiveSlots{held: map[string]bool{"map_inst#0": true}}
	r.SetLiveSlotChecker(slots)
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, transcriptionRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#1" {
		t.Fatalf("transcription Resolve = %q, %v; want map_inst#1, the instance whose live slot is free", target.InstanceKey, err)
	}

	slots.held["map_inst#1"] = true
	if _, err := r.Resolve(context.Background(), auth.Token{}, transcriptionRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("Resolve with every live slot held = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
}

// liveMixedStore is mixedFleetStore with both applications serving the audio
// flavor: qwen-coder's map_inst (instance-capable where a test gives it a view)
// beside map_old on srv_old, a mapping without instances.
func liveMixedStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := mixedFleetStore(t, now, 0)
	for _, id := range []string{"app_inst", "app_old"} {
		app, err := store.ApplicationByID(context.Background(), id)
		if err != nil {
			t.Fatalf("ApplicationByID %s: %v", id, err)
		}
		app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorOpenAIAudio}
		if err := store.UpdateApplication(context.Background(), app); err != nil {
			t.Fatalf("UpdateApplication %s: %v", id, err)
		}
	}
	return store
}

// In a pool that holds an instance candidate, a mapping without instances is
// judged by its one live slot too: a live session passes over it while it is
// held -- onto a free instance, or into the admission queue, signalling demand
// for the instance-capable mapping -- instead of being sent to a slot it cannot
// take. The target says it came from such a pool (InstancePool), whichever
// candidate it is.
func TestLiveSessionPassesOverAHeldPlainMappingInAnInstancePool(t *testing.T) {
	now := instanceCapacityNow
	r := NewResolver(liveMixedStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1}}})
	// The old server has the model loaded, so it wins whenever its slot is not judged.
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_old": {"qwen2.5"}}})
	slots := &fakeLiveSlots{held: map[string]bool{}}
	r.SetLiveSlotChecker(slots)
	sink := &fakeDemandSink{}
	r.SetDemandSink(sink)
	adm := timeoutAdmission()
	r.SetAdmissionController(adm)

	target, err := r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_old#0" || target.InstanceCapable || !target.InstancePool {
		t.Fatalf("Resolve = %q capable %v pool %v, %v; want the loaded map_old#0, not instance-capable, from an instance pool", target.InstanceKey, target.InstanceCapable, target.InstancePool, err)
	}
	slots.held["map_old#0"] = true
	target, err = r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder"))
	if err != nil || target.InstanceKey != "map_inst#0" || !target.InstancePool {
		t.Fatalf("Resolve with map_old's slot held = %q pool %v, %v; want the free instance map_inst#0", target.InstanceKey, target.InstancePool, err)
	}
	slots.held["map_inst#0"] = true
	if _, err := r.Resolve(context.Background(), auth.Token{}, liveRequest("qwen-coder")); !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 {
		t.Fatalf("Resolve with every slot held = %v after %d waits, want ErrAdmissionQueueTimeout after 1", err, adm.calls)
	}
	want := []DemandSignal{{Model: "qwen-coder", APIFlavor: APIFlavorOpenAIAudio, Live: true, MappingIDs: []string{"map_inst"}}}
	if !demandSignalsEqual(sink.got, want) {
		t.Fatalf("demand signals = %+v, want %+v", sink.got, want)
	}
}
