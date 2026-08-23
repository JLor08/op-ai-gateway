// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBenchmarkRegistryTryStartSingleRunPerServer(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	run, ok := reg.TryStart("srv_a", "all", "speed", 3, now, func() {})
	if !ok || run == nil {
		t.Fatalf("first TryStart = (%v, %v), want (run, true)", run, ok)
	}

	// A second start while the first is running must be rejected.
	if r2, ok2 := reg.TryStart("srv_a", "all", "speed", 3, now, func() {}); ok2 || r2 != nil {
		t.Fatalf("second TryStart while running = (%v, %v), want (nil, false)", r2, ok2)
	}

	// A different server is independent.
	if _, ok := reg.TryStart("srv_b", "all", "speed", 1, now, func() {}); !ok {
		t.Fatalf("TryStart for a different server should succeed")
	}

	// After finishing, TryStart succeeds again and the server is no longer busy.
	run.finish("")
	if reg.ServerBusy("srv_a") {
		t.Fatalf("ServerBusy(srv_a) = true after finish, want false")
	}
	if _, ok := reg.TryStart("srv_a", "all", "speed", 2, now, func() {}); !ok {
		t.Fatalf("TryStart after finish should succeed")
	}
}

func TestBenchmarkRegistryServerBusyAndStatus(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	if reg.ServerBusy("srv_a") {
		t.Fatalf("ServerBusy on unknown server = true, want false")
	}
	if st := reg.Status("srv_a"); st.Running || st.ServerID != "" {
		t.Fatalf("Status on unknown server = %#v, want zero", st)
	}

	run, ok := reg.TryStart("srv_a", "single", "speed", 2, now, func() {})
	if !ok {
		t.Fatalf("TryStart failed")
	}
	if !reg.ServerBusy("srv_a") {
		t.Fatalf("ServerBusy(srv_a) = false while running, want true")
	}
	st := reg.Status("srv_a")
	if !st.Running || st.ServerID != "srv_a" || st.Scope != "single" || st.Total != 2 || st.Done != 0 {
		t.Fatalf("Status while running = %#v", st)
	}
	if !st.StartedAt.Equal(now) {
		t.Fatalf("Status.StartedAt = %v, want %v", st.StartedAt, now)
	}

	run.addResult(BenchmarkResult{MappingID: "map_1", GatewayModelName: "qwen-coder", GenTokensPerSecond: 42.5})
	st = reg.Status("srv_a")
	if st.Done != 1 || len(st.Results) != 1 {
		t.Fatalf("Status after one result: Done=%d Results=%d", st.Done, len(st.Results))
	}
	if st.Results[0].MappingID != "map_1" || st.Results[0].GenTokensPerSecond != 42.5 {
		t.Fatalf("result not reflected: %#v", st.Results[0])
	}

	// snapshot returns a copy: mutating it must not affect the stored status.
	st.Results[0].MappingID = "mutated"
	if again := reg.Status("srv_a"); again.Results[0].MappingID != "map_1" {
		t.Fatalf("Status returned a shared slice; got %q", again.Results[0].MappingID)
	}

	run.finish("boom")
	st = reg.Status("srv_a")
	if st.Running || st.Error != "boom" {
		t.Fatalf("Status after finish = %#v, want Running=false Error=boom", st)
	}
	// Results survive finish.
	if len(st.Results) != 1 {
		t.Fatalf("Results dropped on finish: %d", len(st.Results))
	}
}

// TestBenchmarkRegistryTryStartConcurrentAtomicity fans out many goroutines that
// all TryStart the SAME server at once and asserts EXACTLY one wins — the
// registry's core single-run-per-server guarantee under contention.
func TestBenchmarkRegistryTryStartConcurrentAtomicity(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	const n = 20
	var wins int64
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			start.Wait() // barrier: release all goroutines together
			if _, ok := reg.TryStart("srv_a", "all", "speed", 1, now, func() {}); ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	start.Done()
	done.Wait()

	if wins != 1 {
		t.Fatalf("concurrent TryStart winners = %d, want exactly 1", wins)
	}
	if !reg.ServerBusy("srv_a") {
		t.Fatalf("ServerBusy(srv_a) = false after a winning TryStart, want true")
	}
}

// Release forgets the run entirely (undoing a reservation), unlike finish which
// keeps a terminal errored status.
func TestBenchmarkRegistryRelease(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	if _, ok := reg.TryStart("srv_a", "all", "speed", 1, now, func() {}); !ok {
		t.Fatalf("TryStart failed")
	}
	reg.Release("srv_a")
	if reg.ServerBusy("srv_a") {
		t.Fatalf("ServerBusy(srv_a) = true after Release, want false")
	}
	// Release leaves no zombie status behind (unlike finish).
	if st := reg.Status("srv_a"); st.Running || st.ServerID != "" || st.Error != "" {
		t.Fatalf("Status after Release = %#v, want zero (no zombie)", st)
	}
	// The server can be reserved again.
	if _, ok := reg.TryStart("srv_a", "all", "speed", 1, now, func() {}); !ok {
		t.Fatalf("TryStart after Release should succeed")
	}

	// A nil registry Release is a no-op.
	var nilReg *BenchmarkRegistry
	nilReg.Release("srv_a")
}

func TestBenchmarkRegistryNilSafe(t *testing.T) {
	var reg *BenchmarkRegistry

	if reg.ServerBusy("srv_a") {
		t.Fatalf("nil ServerBusy = true, want false")
	}
	if st := reg.Status("srv_a"); st.Running || st.ServerID != "" {
		t.Fatalf("nil Status = %#v, want zero", st)
	}
	if run, ok := reg.TryStart("srv_a", "all", "speed", 1, time.Now(), func() {}); ok || run != nil {
		t.Fatalf("nil TryStart = (%v, %v), want (nil, false)", run, ok)
	}
}

// TestBenchmarkRegistrySubscribeSnapshotAndPublish asserts Subscribe returns the
// current status snapshot and that a subsequent publish is delivered to the channel.
func TestBenchmarkRegistrySubscribeSnapshotAndPublish(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	run, ok := reg.TryStart("srv_a", "all", "speed", 2, now, func() {})
	if !ok {
		t.Fatalf("TryStart failed")
	}
	run.addResult(BenchmarkResult{MappingID: "map_1", GatewayModelName: "m1"})

	snap, ch, unsub := reg.Subscribe("srv_a")
	defer unsub()
	if !snap.Running || snap.Done != 1 || len(snap.Results) != 1 {
		t.Fatalf("subscribe snapshot = %#v, want the current running status (Done 1)", snap)
	}

	// A published frame is delivered to the subscriber.
	run.addResult(BenchmarkResult{MappingID: "map_2", GatewayModelName: "m2"})
	reg.publish("srv_a", run.snapshot())
	select {
	case got := <-ch:
		if got.Done != 2 || len(got.Results) != 2 {
			t.Fatalf("published frame = %#v, want Done 2 with 2 results", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("no frame delivered within 1s")
	}
}

// TestBenchmarkRegistryUnsubscribeStopsDelivery asserts that after unsub the sub
// entry is removed and a later publish delivers nothing to the stale channel.
func TestBenchmarkRegistryUnsubscribeStopsDelivery(t *testing.T) {
	reg := NewBenchmarkRegistry()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if _, ok := reg.TryStart("srv_a", "all", "speed", 1, now, func() {}); !ok {
		t.Fatalf("TryStart failed")
	}
	_, ch, unsub := reg.Subscribe("srv_a")
	unsub()

	// The empty subscriber set is removed entirely.
	reg.mu.Lock()
	_, exists := reg.subs["srv_a"]
	reg.mu.Unlock()
	if exists {
		t.Fatalf("subs[srv_a] still present after unsub, want removed")
	}

	// A publish after unsub delivers nothing to the stale channel.
	reg.publish("srv_a", BenchmarkStatus{Done: 99})
	select {
	case got := <-ch:
		t.Fatalf("received %#v after unsub, want no delivery", got)
	default:
	}

	// unsub is idempotent (must not panic on a second call).
	unsub()
}

// TestBenchmarkRegistrySubscribeNilSafe asserts a nil registry Subscribe returns a
// zero snapshot + a closed channel and that publish is a no-op.
func TestBenchmarkRegistrySubscribeNilSafe(t *testing.T) {
	var reg *BenchmarkRegistry
	snap, ch, unsub := reg.Subscribe("srv_a")
	if snap.Running || snap.ServerID != "" {
		t.Fatalf("nil Subscribe snapshot = %#v, want zero", snap)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatalf("nil Subscribe channel delivered a value, want closed")
		}
	case <-time.After(time.Second):
		t.Fatalf("nil Subscribe channel not closed (read blocked)")
	}
	unsub()                                 // no-op, must not panic
	reg.publish("srv_a", BenchmarkStatus{}) // no-op, must not panic
}

// TestBenchmarkRegistryPublishDropsOnFullBuffer asserts publish never blocks when a
// subscriber's buffer is full — the frame is dropped (the reader recovers on its
// snapshot).
func TestBenchmarkRegistryPublishDropsOnFullBuffer(t *testing.T) {
	reg := NewBenchmarkRegistry()
	// Register a size-1 channel directly (in-package) to exercise the drop path.
	ch := make(chan BenchmarkStatus, 1)
	reg.subs["srv_a"] = map[chan BenchmarkStatus]struct{}{ch: {}}

	done := make(chan struct{})
	go func() {
		reg.publish("srv_a", BenchmarkStatus{Done: 1}) // fills the buffer
		reg.publish("srv_a", BenchmarkStatus{Done: 2}) // buffer full -> dropped, must not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("publish blocked on a full buffer, want a non-blocking drop")
	}

	// Only the first frame is buffered; the second was dropped.
	got := <-ch
	if got.Done != 1 {
		t.Fatalf("buffered frame = %#v, want the first (Done 1)", got)
	}
	select {
	case extra := <-ch:
		t.Fatalf("second frame present = %#v, want dropped", extra)
	default:
	}
}

func TestBenchmarkRegistryActiveRuns(t *testing.T) {
	r := NewBenchmarkRegistry()
	now := time.Now()
	if got := r.ActiveRuns(); len(got) != 0 {
		t.Fatalf("empty registry: want 0 active runs, got %d", len(got))
	}
	// Two running servers + one finished.
	run1, ok := r.TryStart("srv-1", "server", "speed", 3, now, func() {})
	if !ok {
		t.Fatal("TryStart srv-1")
	}
	if _, ok := r.TryStart("srv-2", "mapping", "capacity", 1, now, func() {}); !ok {
		t.Fatal("TryStart srv-2")
	}
	run3, ok := r.TryStart("srv-3", "application", "both", 2, now, func() {})
	if !ok {
		t.Fatal("TryStart srv-3")
	}
	run3.finish("") // terminal — must be excluded
	_ = run1

	active := r.ActiveRuns()
	if len(active) != 2 {
		t.Fatalf("want 2 active runs, got %d (%+v)", len(active), active)
	}
	ids := map[string]BenchmarkStatus{}
	for _, s := range active {
		if !s.Running {
			t.Fatalf("ActiveRuns returned a non-running run: %+v", s)
		}
		ids[s.ServerID] = s
	}
	if _, ok := ids["srv-1"]; !ok {
		t.Fatal("srv-1 missing")
	}
	if _, ok := ids["srv-3"]; ok {
		t.Fatal("finished srv-3 must not appear")
	}
	if ids["srv-1"].Scope != "server" || ids["srv-1"].Total != 3 {
		t.Fatalf("srv-1 snapshot fields wrong: %+v", ids["srv-1"])
	}
	// A snapshot must be a deep copy: mutating the returned Results must not
	// affect the registry's internal state (Status mirrors this discipline).
	run1.addResult(BenchmarkResult{MappingID: "m1", GatewayModelName: "gw-1"})
	again := r.ActiveRuns()
	for _, s := range again {
		if s.ServerID == "srv-1" {
			if len(s.Results) != 1 || s.Results[0].MappingID != "m1" {
				t.Fatalf("srv-1 results not reflected/deep-copied: %+v", s.Results)
			}
			s.Results[0].MappingID = "mutated"
		}
	}
	if got := r.Status("srv-1"); len(got.Results) != 1 || got.Results[0].MappingID != "m1" {
		t.Fatalf("mutating a returned snapshot leaked into the registry: %+v", got.Results)
	}
	// Nil registry is a valid "feature off" value.
	var nilReg *BenchmarkRegistry
	if got := nilReg.ActiveRuns(); got != nil {
		t.Fatalf("nil registry ActiveRuns = %+v, want nil", got)
	}
}
