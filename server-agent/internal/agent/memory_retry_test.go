// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"errors"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeModuleFetch fails the first failures calls, then returns mods.
type fakeModuleFetch struct {
	mu       sync.Mutex
	calls    int
	failures int
	mods     []sample.MemoryModule
}

func (f *fakeModuleFetch) fetch(context.Context) ([]sample.MemoryModule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("helper not ready")
	}
	return f.mods, nil
}

func (f *fakeModuleFetch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func setMemoryRetryInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := memoryRetryInterval
	memoryRetryInterval = d
	t.Cleanup(func() { memoryRetryInterval = orig })
}

// runAgentFor starts a.Run, waits for cond, lets a few more retry intervals
// pass so a ticker that failed to stop would show, then stops the run.
func runAgentFor(t *testing.T, a *Agent, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	waitUntil(t, 10*time.Second, cond)
	time.Sleep(20 * memoryRetryInterval)
	cancel()
	<-done
}

func TestMemoryRetryFillsModulesAndResends(t *testing.T) {
	setMemoryRetryInterval(t, 5*time.Millisecond)
	// The decision itself is collector.HelperMemoryModulesNeeded's (Linux,
	// unprivileged, socket set, no modules), covered in the collector; here
	// it is forced so the retry loop runs on any test host.
	origNeeded := helperMemoryModulesNeeded
	helperMemoryModulesNeeded = func(_ sample.SystemReport, o collector.HardwareOptions) bool { return o.HelperSocket != "" }
	t.Cleanup(func() { helperMemoryModulesNeeded = origNeeded })

	frp := &fakeReporterPoster{}
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour, HelperSocket: "/run/op-agent-helper/agent.sock"}
	a := NewFromDeps(cfg, Deps{Poster: frp})
	if a.fetchMemoryModules == nil {
		t.Fatal("a helper socket must wire the default memory-module fetch")
	}
	want := []sample.MemoryModule{{Locator: "DIMM_A1", SizeBytes: 16 << 30, Type: "DDR5", SpeedMHz: 4800}}
	fetch := &fakeModuleFetch{failures: 2, mods: want}
	a.fetchMemoryModules = fetch.fetch

	runAgentFor(t, a, func() bool { return frp.reportCount() >= 2 })

	if n := fetch.count(); n != 3 {
		t.Fatalf("fetch calls = %d, want 3 (two failures, one success, then the ticker stops)", n)
	}
	if n := frp.reportCount(); n != 2 {
		t.Fatalf("system reports = %d, want 2 (startup + one after the modules arrived)", n)
	}
	frp.mu.Lock()
	first, last := frp.reports[0], frp.reports[1]
	frp.mu.Unlock()
	if !reflect.DeepEqual(last.Memory.Modules, want) {
		t.Fatalf("resent modules = %#v, want %#v", last.Memory.Modules, want)
	}
	if !reflect.DeepEqual(a.hw.Memory.Modules, want) {
		t.Fatalf("cached modules = %#v, want %#v", a.hw.Memory.Modules, want)
	}
	if first.CollectedAt != last.CollectedAt || first.Hostname != last.Hostname {
		t.Fatal("the resent report must keep the rest of the cached inventory")
	}
}

func TestMemoryRetryStopsQuietlyWithoutHelper(t *testing.T) {
	setMemoryRetryInterval(t, 5*time.Millisecond)
	frp := &fakeReporterPoster{}
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: frp})
	if a.fetchMemoryModules != nil {
		t.Fatal("no helper socket must leave the memory-module fetch unset")
	}
	fetch := &fakeModuleFetch{mods: []sample.MemoryModule{{Locator: "DIMM_A1"}}}
	a.fetchMemoryModules = fetch.fetch

	runAgentFor(t, a, func() bool { return frp.reportCount() >= 1 })

	if n := fetch.count(); n != 0 {
		t.Fatalf("fetch calls = %d, want 0 (not needed: no ticker)", n)
	}
	if n := frp.reportCount(); n != 1 {
		t.Fatalf("system reports = %d, want 1 (startup only)", n)
	}
}
