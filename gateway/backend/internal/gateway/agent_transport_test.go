// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"testing"
	"time"
)

// TestAgentTransportRegistryTracksLatestTLSAndPlainObservation pins the core
// contract: each Report stamps only its own side (LastTLSAt / LastPlainAt), the
// newer time always wins the Latest verdict, and the raw pair still travels
// so the portal can render both stamps if it ever needs to.
func TestAgentTransportRegistryTracksLatestTLSAndPlainObservation(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	step := func(d time.Duration) time.Time { return base.Add(d) }
	seq := []time.Time{step(0), step(time.Minute), step(2 * time.Minute), step(3 * time.Minute)}
	var i int
	r := NewAgentTransportRegistry()
	r.now = func() time.Time {
		t := seq[i]
		i++
		return t
	}

	if _, _, ok := r.LatestTransport("s1"); ok {
		t.Fatal("LatestTransport on an unknown server must report ok=false")
	}

	r.Report("s1", true)
	transport, at, ok := r.LatestTransport("s1")
	if !ok || transport != "tls" || !at.Equal(seq[0]) {
		t.Fatalf("after first TLS report: transport=%q at=%v ok=%v", transport, at, ok)
	}

	r.Report("s1", false)
	transport, at, ok = r.LatestTransport("s1")
	if !ok || transport != "plain" || !at.Equal(seq[1]) {
		t.Fatalf("newer plain report must win: transport=%q at=%v ok=%v", transport, at, ok)
	}

	r.Report("s1", true)
	transport, at, ok = r.LatestTransport("s1")
	if !ok || transport != "tls" || !at.Equal(seq[2]) {
		t.Fatalf("newer TLS report must beat an older plain: transport=%q at=%v", transport, at)
	}

	r.Report("s1", false)
	// A plain report stamped at seq[3] must NOT overwrite the seq[2] TLS mark.
	// If it did, the tie-break below would collapse and AnyTLSWithin would
	// silently degrade after every follow-up plain hop.
	transport, at, ok = r.LatestTransport("s1")
	if !ok || transport != "plain" || !at.Equal(seq[3]) {
		t.Fatalf("newest plain wins Latest: transport=%q at=%v", transport, at)
	}
	if !r.AnyTLSWithin(seq[3], 2*time.Minute) {
		t.Fatal("the TLS mark from seq[2] must remain visible after a later plain report")
	}
}

// TestAgentTransportRegistryTLSWithinAndNilSafety pins the arming predicate + the
// nil-registry no-op contract (mirrors AgentCertReportRegistry / AgentPresenceRegistry).
func TestAgentTransportRegistryTLSWithinAndNilSafety(t *testing.T) {
	t.Run("nil is a safe no-op", func(t *testing.T) {
		var r *AgentTransportRegistry
		r.Report("s1", true)
		if _, _, ok := r.LatestTransport("s1"); ok {
			t.Fatal("nil registry returned an observation")
		}
		if r.AnyTLSWithin(time.Now(), time.Minute) {
			t.Fatal("nil registry reported a TLS observation within the window")
		}
		r.Retain(map[string]struct{}{}) // must not panic
	})

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	r := NewAgentTransportRegistry()

	stampAll := func(times map[string]time.Time) {
		// Force a Report to write a specific timestamp by injecting the clock.
		for id, at := range times {
			at := at
			r.now = func() time.Time { return at }
			r.Report(id, true)
		}
	}

	stampAll(map[string]time.Time{
		"fresh": base.Add(-30 * time.Second),
		"stale": base.Add(-10 * time.Minute),
	})
	if !r.AnyTLSWithin(base, time.Minute) {
		t.Fatal("a fresh TLS observation must arm the gate")
	}
	if r.AnyTLSWithin(base, 5*time.Second) {
		t.Fatal("a fresh observation older than the window must NOT arm the gate")
	}

	r2 := NewAgentTransportRegistry()
	r2.now = func() time.Time { return base }
	r2.Report("s1", false)
	if r2.AnyTLSWithin(base, time.Minute) {
		t.Fatal("only a plain observation must never arm the gate")
	}

	// An empty registry never arms, whatever the window.
	if NewAgentTransportRegistry().AnyTLSWithin(base, time.Hour) {
		t.Fatal("an empty registry must not arm the gate")
	}
}

// TestAgentTransportRegistryRetainBoundsFleet pins the eviction contract the
// app-health loop relies on: only servers in the live set survive.
func TestAgentTransportRegistryRetainBoundsFleet(t *testing.T) {
	r := NewAgentTransportRegistry()
	r.Report("live", true)
	r.Report("gone", false)

	r.Retain(map[string]struct{}{"live": {}})
	if _, _, ok := r.LatestTransport("live"); !ok {
		t.Fatal("Retain evicted a live server")
	}
	if _, _, ok := r.LatestTransport("gone"); ok {
		t.Fatal("Retain kept a server missing from the live set")
	}

	// An empty live set clears everything (which the app-health loop would only
	// pass if the entire fleet is gone: a valid transient during a fresh boot).
	r.Retain(map[string]struct{}{})
	if _, _, ok := r.LatestTransport("live"); ok {
		t.Fatal("Retain(empty) did not clear the registry")
	}
}
