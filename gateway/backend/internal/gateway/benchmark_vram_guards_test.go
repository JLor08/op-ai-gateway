// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"testing"
	"time"
)

// The VRAM run's fail-closed guards, pinned one at a time.
//
// Every test here exists because deleting the line it names left the whole
// suite GREEN. They are guards rather than features, so nothing else
// exercises them: each one only ever refuses an input the happy path does not
// produce, and a guard no test can fail against is a guard that can be
// removed by a well-meaning cleanup years from now -- at which point the run
// reports a number it did not earn, which is the one outcome this feature
// treats as worse than reporting nothing.
//
// The two `vramAwaitMeasured` guards that runtimeStatusDTOsFromSamples
// already makes unreachable in production (a frame with GPU measurements but
// no MeasuredAt; a non-positive measurement) are pinned at THIS level
// deliberately: the redundancy is a property of today's only producer, while
// the guard is a property of the function, and a second producer -- or a
// changed DTO builder -- would silently inherit the absence.

// vramGuardServer is the smallest Server the guards need: the two volatile
// registries the run reads its live evidence from, and nothing else.
func vramGuardServer() *Server {
	return &Server{
		ServerPerf:    NewServerPerfRegistry(),
		RuntimeStatus: NewRuntimeStatusRegistry(),
	}
}

// vramGuardStatusDriver publishes serverID's runtime-status snapshot every
// couple of milliseconds until the test ends -- what the ~1 s telemetry
// ingest does in production, compressed.
//
// A driver is what makes these tests mean anything: `vramAwaitMeasured`
// subscribes ITSELF, after the load, and a frame published before that
// subscription reaches only the snapshot. So "delivered on the stream" cannot
// be faked by seeding the registry, which is exactly the distinction the
// freshness discipline rests on.
func vramGuardStatusDriver(t *testing.T, srv *Server, serverID string, next func() []RuntimeStatusDTO) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				srv.RuntimeStatus.publish(serverID, next())
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// vramGuardShortMeasuredWait shortens strategy (a)'s wait so a test that
// asserts NOTHING is reported does not sit out the production bound.
func vramGuardShortMeasuredWait(t *testing.T) {
	t.Helper()
	old := vramMeasuredWaitBound
	vramMeasuredWaitBound = 80 * time.Millisecond
	t.Cleanup(func() { vramMeasuredWaitBound = old })
}

// TestVRAMAwaitMeasuredTakesOnlyTheTargetSpecsMeasurement pins the spec-id
// guard. A runtime-status frame carries EVERY spec on the server, so without
// it the first sibling that happens to carry a per-process measurement is
// reported as the target's measured_mb -- a number for a model this run never
// loaded, offered for the target's estimate field.
//
// The sibling is deliberately FIRST in the frame and carries a measurement on
// the same card, because a frame is walked in order and a guard that only
// works when the target comes first is not a guard.
func TestVRAMAwaitMeasuredTakesOnlyTheTargetSpecsMeasurement(t *testing.T) {
	srv := vramGuardServer()
	vramGuardShortMeasuredWait(t)
	measuredAt := time.Now().UTC()
	vramGuardStatusDriver(t, srv, "srv1", func() []RuntimeStatusDTO {
		return []RuntimeStatusDTO{
			{
				SpecID: "rspec_sib", State: "running", PID: 7, MeasuredAt: measuredAt,
				GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 999}},
			},
			{
				SpecID: "rspec_target", State: "running", PID: 5, MeasuredAt: measuredAt,
				GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 12345}},
			},
		}
	})

	got := srv.vramAwaitMeasured(context.Background(), "srv1", "rspec_target")
	if len(got) != 1 || got[0] != 12345 {
		t.Fatalf("measured = %v, want map[0:12345] -- the TARGET's measurement, never a sibling's", got)
	}
}

// TestVRAMAwaitMeasuredRequiresAMeasurementTimestamp pins the watermark
// guard: GPU rows without a MeasuredAt are not a measurement.
//
// MeasuredAt is what makes the value this RUN's rather than one of unknown
// age -- the whole reason strategy (a) reads the stream instead of the stored
// row, which has no timestamp at all. Today runtimeStatusDTOsFromSamples
// pairs the two, so this shape cannot arrive; the guard is what keeps that a
// producer detail rather than a load-bearing assumption.
func TestVRAMAwaitMeasuredRequiresAMeasurementTimestamp(t *testing.T) {
	srv := vramGuardServer()
	vramGuardShortMeasuredWait(t)
	vramGuardStatusDriver(t, srv, "srv1", func() []RuntimeStatusDTO {
		return []RuntimeStatusDTO{{
			SpecID: "rspec_target", State: "running", PID: 5,
			GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 12345}},
		}}
	})

	if got := srv.vramAwaitMeasured(context.Background(), "srv1", "rspec_target"); got != nil {
		t.Fatalf("measured = %v, want nothing: a reading with no measurement time is of unknown age", got)
	}
}

// TestVRAMAwaitMeasuredRejectsANonPositiveMeasurement pins the third guard.
// `0` means UNKNOWN throughout this feature -- the telemetry ingest and the
// write-back apply that same rule to this very array -- so a zero (or a
// nonsensical negative) must produce no entry at all rather than a per-card
// "this model is free" that the apply affordance would offer as a number.
func TestVRAMAwaitMeasuredRejectsANonPositiveMeasurement(t *testing.T) {
	srv := vramGuardServer()
	vramGuardShortMeasuredWait(t)
	vramGuardStatusDriver(t, srv, "srv1", func() []RuntimeStatusDTO {
		return []RuntimeStatusDTO{{
			SpecID: "rspec_target", State: "running", PID: 5, MeasuredAt: time.Now().UTC(),
			GPUs: []RuntimeGPUStatusDTO{
				{Index: 0, VRAMMeasuredMB: 0},
				{Index: 1, VRAMMeasuredMB: -5},
			},
		}}
	})

	if got := srv.vramAwaitMeasured(context.Background(), "srv1", "rspec_target"); got != nil {
		t.Fatalf("measured = %v, want nothing: 0 means unknown, and a negative reading is nonsense", got)
	}
}

// TestVRAMAwaitMeasuredNeverReadsTheSubscriptionSnapshot is strategy (a)'s
// freshness discipline itself, and the one mutation the two existing
// post-load tests could not catch: both of them set their statuses from
// inside the load hook, which lands in the snapshot as well as on the stream,
// so a function that read the snapshot first would pass them both.
//
// Here the measurement exists ONLY in the snapshot -- published before the
// call, i.e. before the load this run performed -- and every delivered frame
// says the target is starting with nothing measured yet. A value of unknown
// age is what strategy (a) refuses to report, so the honest answer is nothing.
func TestVRAMAwaitMeasuredNeverReadsTheSubscriptionSnapshot(t *testing.T) {
	srv := vramGuardServer()
	vramGuardShortMeasuredWait(t)
	// The stale reading: a real one, from a previous run, still sitting in the
	// registry's last snapshot when this run subscribes.
	srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{
		SpecID: "rspec_target", State: "running", PID: 5, MeasuredAt: time.Now().UTC().Add(-time.Hour),
		GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 12345}},
	}})
	vramGuardStatusDriver(t, srv, "srv1", func() []RuntimeStatusDTO {
		return []RuntimeStatusDTO{{SpecID: "rspec_target", State: "starting"}}
	})

	if got := srv.vramAwaitMeasured(context.Background(), "srv1", "rspec_target"); got != nil {
		t.Fatalf("measured = %v, want nothing: the subscription's snapshot predates the load", got)
	}
}

// TestVRAMUsedMBFloorsANonsensicalReading pins the non-positive guard on the
// conversion every reported baseline goes through.
//
// BaselineUsedMB has no `omitempty`, so whatever this returns is rendered
// verbatim on the report and persisted in the history row's payload -- and a
// negative "used" figure is not a small allocation, it is a broken sample.
// Note the shape of the bug the guard prevents: Go truncates toward zero, so
// a sub-MiB negative divides to 0 on its own and only a reading past -1 MiB
// makes the missing guard visible at all.
func TestVRAMUsedMBFloorsANonsensicalReading(t *testing.T) {
	cases := []struct {
		name      string
		usedBytes int64
		want      int
	}{
		{"a whole-MiB reading", 700 * oneMiB, 700},
		{"a partial MiB floors down", 3*oneMiB + 700_000, 3},
		{"nothing used at all", 0, 0},
		{"a sub-MiB negative", -1, 0},
		{"a negative past one MiB is UNKNOWN, never a negative allocation", -2 * oneMiB, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramUsedMB(tc.usedBytes); got != tc.want {
				t.Fatalf("vramUsedMB(%d) = %d, want %d", tc.usedBytes, got, tc.want)
			}
		})
	}
}

// TestVRAMStableWindowSettlesBeforeItOpens pins the per-phase settle -- the
// only thing keeping a window from opening on the tail of the transition it
// is meant to follow, and until now a documented-as-unvalidated knob with no
// behavioural test of any kind.
//
// The fake host holds the PRE-transition figure for the first stretch of the
// settle and the post-transition one afterwards. Both stretches are
// individually stable, so a window that opened immediately would find K
// agreeing samples within a few milliseconds and return the reading from
// BEFORE the phase boundary -- a baseline that still contained the drained
// model, or a post-load window that missed the allocation. The assertion is
// therefore on the VALUE the window reports, not on elapsed time.
func TestVRAMStableWindowSettlesBeforeItOpens(t *testing.T) {
	srv := vramGuardServer()
	oldSettle, oldBound := vramPhaseSettle, vramPhaseWindowBound
	// The switch happens well inside the settle, and K samples at the 2 ms
	// driver cadence take ~6 ms -- so an unsettled window has ~40 ms of
	// pre-transition readings to lock onto, and a settled one can only ever
	// see post-transition ones.
	vramPhaseSettle = 200 * time.Millisecond
	vramPhaseWindowBound = 3 * time.Second
	t.Cleanup(func() { vramPhaseSettle, vramPhaseWindowBound = oldSettle, oldBound })

	const before, after = 500, 21500
	switchAt := time.Now().Add(50 * time.Millisecond)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				used := int64(before)
				if time.Now().After(switchAt) {
					used = after
				}
				srv.ServerPerf.publish(vramSample(vramGPU(0, used, 24576)))
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	sample, sawSample, stable := srv.vramStableWindow(context.Background(), "srv1", []int{0})
	if !sawSample || !stable {
		t.Fatalf("vramStableWindow = (sawSample %v, stable %v), want a stable window", sawSample, stable)
	}
	if got := sample.GPUs[0].MemUsedBytes / oneMiB; got != after {
		t.Fatalf("window reading = %d MiB, want %d: it opened on the tail of the transition it was meant to follow", got, after)
	}
}
