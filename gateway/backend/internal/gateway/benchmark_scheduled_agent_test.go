// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// scheduledNeighbourSpecID is the launch spec of the second model the
// scheduled-run fixture puts next to the portal-shaped agent fixture's target.
const scheduledNeighbourSpecID = "rs_map2"

// scheduledStoredLoadMS is the load time the target's mapping carries from an
// earlier run.
const scheduledStoredLoadMS = 1234

// newScheduledAgentFixture is the portal-shaped agent fixture (map1 -> qwen
// under rs_map1) the way a scheduled run meets it on a real server: rs_map1
// is pinned, map1 carries a stored load time, and a second model, map2 ->
// llama under the pinned spec rs_map2, sits next to it. map2 is
// metrics-locked, so the scheduler never measures it. running and statuses
// are portalAgentOpts' fields of the same names.
func newScheduledAgentFixture(t *testing.T, running []string, statuses []RuntimeStatusDTO) *portalAgentFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	f := newPortalAgentFixture(t, portalAgentOpts{features: portalAgentAllFeatures, running: running, statuses: statuses})
	spec, _, err := f.mem.RuntimeSpecByMapping(ctx, "map1")
	must("RuntimeSpecByMapping", err)
	spec.Pinned = true
	must("UpsertRuntimeSpec map1", f.mem.UpsertRuntimeSpec(ctx, spec))
	must("UpdateMappingBenchmarkMetrics", f.mem.UpdateMappingBenchmarkMetrics(ctx, "map1", 0, 0, scheduledStoredLoadMS, now))
	must("CreateMapping map2", f.mem.CreateMapping(ctx, routing.ModelMapping{
		ID: "map2", ApplicationID: f.target.app.ID, GatewayModelName: "gw-llama", AppModelName: "llama",
		Status: routing.ServerStatusActive, MetricsLocked: true, CreatedAt: now, UpdatedAt: now,
	}))
	must("UpsertRuntimeSpec map2", f.mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
		ID: scheduledNeighbourSpecID, MappingID: "map2", Enabled: true, Pinned: true, Type: string(routing.RuntimeSpecTypeLlamaCpp),
		Binary: "/usr/local/bin/llama-server", Args: "[]", Env: "{}", HealthPath: "/health",
		HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180, CreatedAt: now, UpdatedAt: now,
	}))
	return f
}

// runtimeSpecs is every runtime spec of the fixture's application.
func (f *portalAgentFixture) runtimeSpecs(t *testing.T) []routing.RuntimeSpec {
	t.Helper()
	specs, err := f.mem.RuntimeSpecsByApplication(context.Background(), f.target.app.ID)
	if err != nil {
		t.Fatalf("RuntimeSpecsByApplication: %v", err)
	}
	return specs
}

// scheduled triggers the fixture application's scheduled benchmark the way
// the scheduler's tick does, waits for the run to end, and returns map1's
// history row and its mapping after the run.
func (f *portalAgentFixture) scheduled(t *testing.T) (routing.BenchmarkRun, routing.ModelMapping) {
	t.Helper()
	ctx := context.Background()
	if !f.srv.TriggerScheduledBenchmark(ctx, f.target.server, f.target.app) {
		t.Fatal("TriggerScheduledBenchmark = false, want a launched run")
	}
	waitFor(t, func() bool { return !f.srv.Benchmarks.Status("srv1").Running })
	rows, err := f.mem.BenchmarkRunsByMapping(ctx, "map1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("BenchmarkRunsByMapping = %d rows, %v; want one", len(rows), err)
	}
	if rows[0].Error != "" {
		t.Fatalf("history row error = %q, want none", rows[0].Error)
	}
	mapping, err := f.mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	return rows[0], mapping
}

// TestScheduledRunNeverStopsOrUnpins: a scheduled speed run writes no runtime
// spec, so it never stops or unpins an agent model. It records a load time
// only for a target that is cold on a server where every other model of the
// application reads stopped in a non-empty runtime status: a neighbour that
// runs would be evicted inside the cold pass, a pinned one in backoff starts
// by itself when its timer fires, and no status at all is no evidence. In
// every other case the mapping keeps the load time it has.
func TestScheduledRunNeverStopsOrUnpins(t *testing.T) {
	target := func(state string, pid int) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: portalAgentSpecID, Model: "qwen", State: state, PID: pid}
	}
	neighbour := func(state string, pid int) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: scheduledNeighbourSpecID, Model: "llama", State: state, PID: pid}
	}
	cases := []struct {
		name     string
		running  []string
		statuses []RuntimeStatusDTO
		wantLoad bool
	}{
		{"a resident pinned target", []string{"qwen"}, []RuntimeStatusDTO{target("running", 4242), neighbour("stopped", 0)}, false},
		{"a cold target next to a running neighbour", nil, []RuntimeStatusDTO{target("stopped", 0), neighbour("running", 4343)}, false},
		{"a cold target next to a pinned neighbour in backoff", nil, []RuntimeStatusDTO{target("stopped", 0), neighbour("backoff", 0)}, false},
		{"a cold target without any runtime status for the server", nil, nil, false},
		{"a cold target whose every other model reads stopped", nil, []RuntimeStatusDTO{target("stopped", 0), neighbour("stopped", 0)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScheduledAgentFixture(t, tc.running, tc.statuses)
			before := f.runtimeSpecs(t)
			row, mapping := f.scheduled(t)
			if after := f.runtimeSpecs(t); !reflect.DeepEqual(after, before) {
				t.Errorf("runtime specs after the run = %+v, want them unchanged %+v: a scheduled run writes no spec", after, before)
			}
			if row.GenTokensPerSecond != 42 {
				t.Errorf("GenTokensPerSecond = %v, want 42 from the warm pass's timings (throughput is measured regardless)", row.GenTokensPerSecond)
			}
			switch {
			case tc.wantLoad && row.LoadTimeMS < 250:
				t.Errorf("LoadTimeMS = %d, want >= 250: the router's cold start is 400 ms, on a server where everything else is stopped", row.LoadTimeMS)
			case tc.wantLoad && mapping.LoadTimeMS != row.LoadTimeMS:
				t.Errorf("mapping load_time_ms = %d, want the measured %d", mapping.LoadTimeMS, row.LoadTimeMS)
			case !tc.wantLoad && row.LoadTimeMS != 0:
				t.Errorf("LoadTimeMS = %d, want 0: no confirmed cold start on an otherwise empty server", row.LoadTimeMS)
			case !tc.wantLoad && mapping.LoadTimeMS != scheduledStoredLoadMS:
				t.Errorf("mapping load_time_ms = %d, want the stored %d kept", mapping.LoadTimeMS, scheduledStoredLoadMS)
			}
		})
	}
}
