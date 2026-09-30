// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// refusalGPUSample is one steady GPU sample for the owned server: what the
// VRAM probe's plan needs to see a GPU, and what its stability windows read.
func refusalGPUSample() routing.TelemetrySample {
	return routing.TelemetrySample{ServerID: baOwnedServer, GPUs: []routing.GPUSample{
		{Index: 0, Name: "NVIDIA RTX 6000", UUID: "GPU-rf", MemUsedBytes: 500 * oneMiB, MemTotalBytes: 24576 * oneMiB},
	}}
}

// allowVRAMPlan makes the VRAM probe's plan pass for rf_text: the agent
// declares runtime_manager and the server has reported a GPU.
func (f *refusalFixture) allowVRAMPlan() {
	f.srv.AgentFeatures.Set(baOwnedServer, []string{"runtime_manager"})
	f.srv.ServerPerf.publish(refusalGPUSample())
}

// driveVRAMTelemetry publishes what the telemetry ingest would, every 2 ms
// until the test ends: the steady GPU sample, and rf_agent's three specs as
// stopped. That is what a VRAM run's isolation check and its stability
// windows wait for.
func (f *refusalFixture) driveVRAMTelemetry(t *testing.T) {
	t.Helper()
	statuses := []RuntimeStatusDTO{
		{SpecID: "rs_rf_text", State: "stopped"},
		{SpecID: "rs_rf_sd", State: "stopped"},
		{SpecID: "rs_rf_unread", State: "stopped"},
	}
	publish := func() {
		f.srv.ServerPerf.publish(refusalGPUSample())
		f.srv.RuntimeStatus.publish(baOwnedServer, statuses)
	}
	publish()
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
				publish()
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// Every starter checks ServerBusy before its spec read and reserves with
// TryStart after it, so a run reserved in between makes its TryStart fail.
// The read hook takes that reservation on the starter's first spec read,
// which makes the race deterministic. The starter must answer 409
// benchmark.already_running and leave the winning run's reservation in place:
// its TryStart-fail branch releases nothing.
func TestStartersLosingTheReservationRaceKeepTheWinningRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		prepare func(f *refusalFixture)
	}{
		{name: "benchmark", path: "/api/portal/applications/rf_agent/benchmark?mode=speed"},
		{name: "context probe", path: "/api/portal/mappings/rf_text/probe-context"},
		{name: "load", path: "/api/portal/mappings/rf_text/load"},
		{name: "vram probe", path: "/api/portal/mappings/rf_text/probe-vram", prepare: (*refusalFixture).allowVRAMPlan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefusalFixture(t)
			if tc.prepare != nil {
				tc.prepare(f)
			}
			var mu sync.Mutex
			won := 0
			f.specs.setOnRead(func(string) {
				if _, ok := f.srv.Benchmarks.TryStart(baOwnedServer, "winner", "winner", 1, time.Now(), func() {}); ok {
					mu.Lock()
					won++
					mu.Unlock()
				}
			})
			if status, code, _ := f.post(t, tc.path); status != http.StatusConflict || code != codeBenchmarkAlreadyRunning {
				t.Fatalf("POST %s after losing the race = %d %q, want 409 %s", tc.path, status, code, codeBenchmarkAlreadyRunning)
			}
			mu.Lock()
			reserved := won
			mu.Unlock()
			if reserved != 1 {
				t.Fatalf("the read hook reserved the server %d times, want exactly once (the starter must read a spec between ServerBusy and TryStart)", reserved)
			}
			if !f.srv.Benchmarks.ServerBusy(baOwnedServer) {
				t.Fatal("ServerBusy = false: the losing starter released the winning run's reservation")
			}
			if st := f.srv.Benchmarks.Status(baOwnedServer); st.Scope != "winner" || !st.Running {
				t.Fatalf("Status = %+v, want the winning run still reserved and running", st)
			}
		})
	}
}

// The application- and server-scope runs build each runnable target from the
// spec the partition read, so a set-mode server_agent child is measured with
// its own sealed token, never the application's.
func TestStartBenchmarkBuildsItsTargetsFromTheReadSpec(t *testing.T) {
	for _, tc := range []struct{ scope, path string }{
		{"application", "/api/portal/applications/rf_agent/benchmark?mode=speed"},
		{"server", "/api/portal/servers/" + baOwnedServer + "/benchmark?mode=speed"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			f := newRefusalFixture(t)
			token := f.sealSpecToken(t, "rf_text")
			if status, code, _ := f.post(t, tc.path); status != http.StatusAccepted {
				t.Fatalf("status = %d code = %q, want 202", status, code)
			}
			f.waitFinished(t)
			f.assertStreamedWithToken(t, "up-rf_text", token)
		})
	}
}

// The context probe's warm-load is built from the spec the starter read.
func TestStartContextProbeBuildsItsTargetFromTheReadSpec(t *testing.T) {
	f := newRefusalFixture(t)
	token := f.sealSpecToken(t, "rf_text")
	if status, code, _ := f.post(t, "/api/portal/mappings/rf_text/probe-context"); status != http.StatusAccepted {
		t.Fatalf("status = %d code = %q, want 202", status, code)
	}
	f.waitFinished(t)
	f.assertStreamedWithToken(t, "up-rf_text", token)
}

// The VRAM probe's load is built from the spec the starter read. The run is
// driven through its drain, isolation and baseline window to the load; the
// measurement it then reports does not matter here.
func TestStartVRAMProbeBuildsItsTargetFromTheReadSpec(t *testing.T) {
	shrinkVRAMTimings(t)
	f := newRefusalFixture(t)
	token := f.sealSpecToken(t, "rf_text")
	f.allowVRAMPlan()
	f.driveVRAMTelemetry(t)
	if status, code, _ := f.post(t, "/api/portal/mappings/rf_text/probe-vram"); status != http.StatusAccepted {
		t.Fatalf("status = %d code = %q, want 202", status, code)
	}
	f.waitFinished(t)
	f.assertStreamedWithToken(t, "up-rf_text", token)
}

// The scheduled run builds each target from the spec its skip check read.
func TestTriggerScheduledBenchmarkBuildsItsTargetsFromTheReadSpec(t *testing.T) {
	f := newRefusalFixture(t)
	ctx := context.Background()
	token := f.sealSpecToken(t, "rf_text")
	server, err := f.srv.Routes.AIServerByID(ctx, baOwnedServer)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	app, err := f.srv.Routes.ApplicationByID(ctx, "rf_agent")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	if !f.srv.TriggerScheduledBenchmark(ctx, server, app) {
		t.Fatal("TriggerScheduledBenchmark = false, want true (a run was launched)")
	}
	f.waitFinished(t)
	f.assertStreamedWithToken(t, "up-rf_text", token)
}
