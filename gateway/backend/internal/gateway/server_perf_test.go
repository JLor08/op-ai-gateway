// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestServerPerfRegistrySubscribeSnapshotAndDelta(t *testing.T) {
	reg := NewServerPerfRegistry()
	t0 := time.Now().UTC()
	reg.publish(routing.TelemetrySample{ServerID: "srv_1", ReportedAt: t0, CPUUtilPct: 10})

	snap, ch, unsub := reg.subscribe("srv_1")
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].CPUUtilPct != 10 {
		t.Fatalf("snapshot[0].CPUUtilPct = %v, want 10", snap[0].CPUUtilPct)
	}

	reg.publish(routing.TelemetrySample{ServerID: "srv_1", ReportedAt: t0.Add(time.Second), CPUUtilPct: 20})
	select {
	case got := <-ch:
		if got.CPUUtilPct != 20 {
			t.Fatalf("delta CPUUtilPct = %v, want 20", got.CPUUtilPct)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delta sample")
	}

	unsub()
	// A later publish after unsub must not panic and must not be delivered.
	reg.publish(routing.TelemetrySample{ServerID: "srv_1", ReportedAt: t0.Add(2 * time.Second), CPUUtilPct: 30})
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("received %v after unsub, want nothing", got.CPUUtilPct)
		}
	case <-time.After(100 * time.Millisecond):
		// No delivery is the expected outcome.
	}
}

func TestServerPerfRegistryRingCap(t *testing.T) {
	reg := NewServerPerfRegistry()
	t0 := time.Now().UTC()
	total := serverPerfRingCap + 50
	for i := 0; i < total; i++ {
		reg.publish(routing.TelemetrySample{
			ServerID:   "srv_1",
			ReportedAt: t0.Add(time.Duration(i) * time.Second),
			CPUUtilPct: float64(i),
		})
	}

	snap, _, unsub := reg.subscribe("srv_1")
	defer unsub()
	if len(snap) != serverPerfRingCap {
		t.Fatalf("snapshot len = %d, want %d", len(snap), serverPerfRingCap)
	}
	// The oldest 50 were evicted, so the first retained sample is #51 (index 50).
	if snap[0].CPUUtilPct != float64(50) {
		t.Fatalf("snapshot[0].CPUUtilPct = %v, want 50 (oldest not evicted)", snap[0].CPUUtilPct)
	}
	if snap[len(snap)-1].CPUUtilPct != float64(total-1) {
		t.Fatalf("snapshot[last].CPUUtilPct = %v, want %d", snap[len(snap)-1].CPUUtilPct, total-1)
	}
}

func TestServerPerfRegistryNilSafe(t *testing.T) {
	var reg *serverPerfRegistry
	// Must not panic.
	reg.publish(routing.TelemetrySample{ServerID: "srv_1", CPUUtilPct: 10})

	snap, ch, unsub := reg.subscribe("srv_1")
	if snap != nil {
		t.Fatalf("nil registry snapshot = %v, want nil", snap)
	}
	// The channel must be closed (a receive returns immediately, not-ok).
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil registry channel delivered a value, want closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("nil registry channel is not closed")
	}
	// unsub must be a no-op that does not panic.
	unsub()
}
