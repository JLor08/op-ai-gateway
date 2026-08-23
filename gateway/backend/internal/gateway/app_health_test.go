// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// signaled consumes a pending broker signal on ch and reports whether one was
// present, without blocking.
func signaled(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestAppHealthRegistryNilSafe(t *testing.T) {
	var reg *AppHealthRegistry
	// None of these may panic on a nil registry.
	reg.Set("app1", false, time.Now(), "boom")
	if got := reg.Snapshot(); got != nil {
		t.Fatalf("nil registry Snapshot() = %v, want nil", got)
	}
	if !reg.Reachable("app1") {
		t.Fatalf("nil registry Reachable() = false, want true (lenient)")
	}
}

func TestAppHealthRegistryReachableUnknownTrue(t *testing.T) {
	reg := NewAppHealthRegistry(nil)
	if !reg.Reachable("never-seen") {
		t.Fatalf("Reachable(unknown) = false, want true (lenient cold-start)")
	}
}

func TestAppHealthRegistrySetAndReachable(t *testing.T) {
	reg := NewAppHealthRegistry(nil)
	now := time.Now()
	reg.Set("app1", false, now, "boom")
	if reg.Reachable("app1") {
		t.Fatalf("Reachable(app1) = true after Set(false), want false")
	}
	reg.Set("app1", true, now, "")
	if !reg.Reachable("app1") {
		t.Fatalf("Reachable(app1) = false after Set(true), want true")
	}
}

func TestAppHealthRegistrySnapshotCopies(t *testing.T) {
	reg := NewAppHealthRegistry(nil)
	at := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reg.Set("app1", false, at, "boom")

	snap := reg.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	h := snap["app1"]
	if h.Reachable || h.LastError != "boom" || !h.LastCheckedAt.Equal(at) {
		t.Fatalf("snapshot entry = %#v", h)
	}
	// The snapshot is a copy: mutating it must not leak into the registry.
	snap["app1"] = appHealth{Reachable: true}
	if reg.Reachable("app1") {
		t.Fatalf("snapshot mutation leaked into the registry")
	}
}

func TestAppHealthRegistryApplicationHealthUnknown(t *testing.T) {
	reg := NewAppHealthRegistry(nil)
	reachable, lastCheckedAt, known := reg.ApplicationHealth("never-seen")
	if known {
		t.Fatalf("known = true for an unprobed app, want false")
	}
	if !reachable {
		t.Fatalf("reachable = false for an unknown app, want true (lenient)")
	}
	if !lastCheckedAt.IsZero() {
		t.Fatalf("lastCheckedAt = %v for an unknown app, want zero", lastCheckedAt)
	}
}

func TestAppHealthRegistryApplicationHealthKnown(t *testing.T) {
	reg := NewAppHealthRegistry(nil)
	at := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reg.Set("app1", false, at, "boom")

	reachable, lastCheckedAt, known := reg.ApplicationHealth("app1")
	if !known {
		t.Fatalf("known = false after Set, want true")
	}
	if reachable {
		t.Fatalf("reachable = true after Set(false), want false")
	}
	if !lastCheckedAt.Equal(at) {
		t.Fatalf("lastCheckedAt = %v, want %v", lastCheckedAt, at)
	}
}

func TestAppHealthRegistryApplicationHealthNilSafe(t *testing.T) {
	var reg *AppHealthRegistry
	reachable, lastCheckedAt, known := reg.ApplicationHealth("app1")
	if known || !reachable || !lastCheckedAt.IsZero() {
		t.Fatalf("nil registry ApplicationHealth = (%v, %v, %v), want (true, zero, false)", reachable, lastCheckedAt, known)
	}
}

// The registry pokes the broker only when reachability transitions, so the SSE
// stream does not fire on every probe tick.
func TestAppHealthRegistryPublishesOnlyOnTransition(t *testing.T) {
	broker := usage.NewBroker()
	ch := broker.Register()
	reg := NewAppHealthRegistry(broker)
	now := time.Now()

	// First observation reachable == the lenient cold-start default: no signal.
	reg.Set("app1", true, now, "")
	if signaled(ch) {
		t.Fatal("first reachable observation must not publish")
	}
	// reachable -> unreachable: transition, publish.
	reg.Set("app1", false, now, "boom")
	if !signaled(ch) {
		t.Fatal("reachable->unreachable transition must publish")
	}
	// steady unreachable: no publish.
	reg.Set("app1", false, now, "boom")
	if signaled(ch) {
		t.Fatal("steady unreachable must not publish")
	}
	// unreachable -> reachable: transition, publish.
	reg.Set("app1", true, now, "")
	if !signaled(ch) {
		t.Fatal("unreachable->reachable transition must publish")
	}
	// steady reachable: no publish.
	reg.Set("app1", true, now, "")
	if signaled(ch) {
		t.Fatal("steady reachable must not publish")
	}
	// First observation of a NEW app that is unreachable: publish.
	reg.Set("app2", false, now, "down")
	if !signaled(ch) {
		t.Fatal("first unreachable observation must publish")
	}
}
