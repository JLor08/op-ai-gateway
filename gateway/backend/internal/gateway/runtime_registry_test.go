// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"testing"
	"time"
)

func TestAgentFeaturesRegistrySetAndHas(t *testing.T) {
	r := newAgentFeaturesRegistry()
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a server that never reported must not have any feature")
	}
	r.Set("srv-a", []string{"runtime_manager", "foo"})
	if !r.Has("srv-a", "runtime_manager") {
		t.Fatal("Has must report a just-set feature")
	}
	if r.Has("srv-a", "bar") {
		t.Fatal("Has must not report an undeclared feature")
	}
	if r.Has("srv-b", "runtime_manager") {
		t.Fatal("a DIFFERENT server's report must not leak")
	}
}

// A fresh Set call REPLACES the prior set (a telemetry sample is a full
// snapshot, never a delta) -- an agent that stops declaring a feature (e.g. a
// downgrade or an older binary reconnecting) must see Has flip back to false.
func TestAgentFeaturesRegistrySetReplacesNotMerges(t *testing.T) {
	r := newAgentFeaturesRegistry()
	r.Set("srv-a", []string{"runtime_manager"})
	r.Set("srv-a", []string{})
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a later empty Set must clear the previously-declared feature")
	}
}

// nil-safe on every method (the package-wide convention): a bare *Server
// built directly in a test, bypassing New, must keep working.
func TestAgentFeaturesRegistryNilSafe(t *testing.T) {
	var r *agentFeaturesRegistry
	r.Set("srv-a", []string{"runtime_manager"}) // must not panic
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a nil registry must report false, never panic or true")
	}
}

func TestRuntimeStatusRegistrySetAndIsFileMode(t *testing.T) {
	r := newRuntimeStatusRegistry()
	if r.IsFileMode("srv-a") {
		t.Fatal("a server that never reported must default to false (not file mode)")
	}
	r.SetFileMode("srv-a", true)
	if !r.IsFileMode("srv-a") {
		t.Fatal("IsFileMode must report a just-set flag")
	}
	r.SetFileMode("srv-a", false)
	if r.IsFileMode("srv-a") {
		t.Fatal("SetFileMode(false) must clear the flag")
	}
	if r.IsFileMode("srv-b") {
		t.Fatal("a DIFFERENT server's flag must not leak")
	}
}

// TestRuntimeStatusRegistrySubscribeSnapshotAndUpdate mirrors
// TestServerPerfRegistrySubscribeSnapshotAndDelta: subscribe atomically
// returns the current snapshot, and a later publish is delivered on the
// subscriber channel as a full-snapshot `update`. Unlike serverPerfRegistry,
// each publish REPLACES the whole per-server list (never appends), matching
// agentFeaturesRegistry.Set's "always a snapshot" discipline.
func TestRuntimeStatusRegistrySubscribeSnapshotAndUpdate(t *testing.T) {
	r := newRuntimeStatusRegistry()
	t0 := time.Now().UTC()
	r.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1", State: "running", Since: t0}})

	snap, ch, unsub := r.subscribe("srv-a")
	if len(snap) != 1 || snap[0].SpecID != "spec-1" {
		t.Fatalf("snapshot = %#v, want one entry spec-1", snap)
	}

	r.publish("srv-a", []RuntimeStatusDTO{
		{SpecID: "spec-1", State: "running", Since: t0},
		{SpecID: "spec-2", State: "starting", Since: t0},
	})
	select {
	case got := <-ch:
		if len(got) != 2 || got[1].SpecID != "spec-2" {
			t.Fatalf("update = %#v, want two entries incl. spec-2", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the update")
	}

	unsub()
	// A later publish after unsub must not panic and must not be delivered.
	r.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-3"}})
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("received %#v after unsub, want nothing", got)
		}
	case <-time.After(100 * time.Millisecond):
		// No delivery is the expected outcome.
	}
}

// A fresh subscribe on a server with no prior publish gets an empty snapshot
// (mirrors serverPerfRegistry.subscribe: the registry itself may hand back a
// bare nil here, same as r.rings[unknownID] would -- the non-nil-on-the-wire
// guarantee is enforced at the SSE serialization boundary by
// nonNilRuntimeStatuses, not inside the registry; see the test below).
func TestRuntimeStatusRegistrySubscribeNoPriorPublish(t *testing.T) {
	r := newRuntimeStatusRegistry()
	snap, _, unsub := r.subscribe("srv-never-reported")
	defer unsub()
	if len(snap) != 0 {
		t.Fatalf("snapshot = %#v, want empty", snap)
	}
}

// nonNilRuntimeStatuses is what actually guarantees the wire never sees a
// JSON null for the runtimes array -- exercised directly since
// runtimeStatusRegistry.subscribe itself may return a bare nil (see above).
func TestNonNilRuntimeStatuses(t *testing.T) {
	if out := nonNilRuntimeStatuses(nil); out == nil || len(out) != 0 {
		t.Fatalf("nonNilRuntimeStatuses(nil) = %#v, want non-nil empty", out)
	}
	in := []RuntimeStatusDTO{{SpecID: "spec-1"}}
	if out := nonNilRuntimeStatuses(in); len(out) != 1 || out[0].SpecID != "spec-1" {
		t.Fatalf("nonNilRuntimeStatuses(in) = %#v, want passthrough", out)
	}
}

// publish delivery is non-blocking: a subscriber whose channel buffer is
// full simply drops the update rather than stalling the publisher (mirrors
// serverPerfRegistry's discipline -- a slow reader recovers on its next
// receive/resubscribe, it never backs up the ingest path).
func TestRuntimeStatusRegistryPublishNonBlockingOnFullSubscriber(t *testing.T) {
	r := newRuntimeStatusRegistry()
	_, _, unsub := r.subscribe("srv-a")
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Overflow the subscriber buffer without ever draining ch. If publish
		// blocked on a full channel, this loop would hang and the test would
		// time out below.
		for i := 0; i < runtimeStatusSubBuffer+5; i++ {
			r.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1"}})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full subscriber channel")
	}
}

// Retain prunes fileMode and statuses entries for servers no longer in the
// live set, mirroring AgentPresenceRegistry.Retain -- a deleted server's
// state must not linger in memory forever.
func TestRuntimeStatusRegistryRetain(t *testing.T) {
	r := newRuntimeStatusRegistry()
	r.SetFileMode("srv-a", true)
	r.SetFileMode("srv-b", true)
	r.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1"}})
	r.publish("srv-b", []RuntimeStatusDTO{{SpecID: "spec-2"}})

	r.Retain(map[string]struct{}{"srv-a": {}})

	if !r.IsFileMode("srv-a") {
		t.Fatal("srv-a is still live, its file-mode flag must survive Retain")
	}
	if r.IsFileMode("srv-b") {
		t.Fatal("srv-b was pruned, its file-mode flag must be gone")
	}
	snapA, _, unsubA := r.subscribe("srv-a")
	unsubA()
	if len(snapA) != 1 {
		t.Fatalf("srv-a status = %#v, want it to survive Retain", snapA)
	}
	snapB, _, unsubB := r.subscribe("srv-b")
	unsubB()
	if len(snapB) != 0 {
		t.Fatalf("srv-b status = %#v, want it pruned by Retain", snapB)
	}
}

// nil-safe on every method, mirroring every other registry in this package.
func TestRuntimeStatusRegistryNilSafe(t *testing.T) {
	var r *runtimeStatusRegistry
	r.SetFileMode("srv-a", true) // must not panic
	if r.IsFileMode("srv-a") {
		t.Fatal("a nil registry must report false, never panic or true")
	}
	r.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1"}}) // must not panic
	r.Retain(map[string]struct{}{})                            // must not panic

	snap, ch, unsub := r.subscribe("srv-a")
	if snap != nil {
		t.Fatalf("nil registry snapshot = %v, want nil", snap)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil registry channel delivered a value, want closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("nil registry channel is not closed")
	}
	unsub() // must not panic
}
