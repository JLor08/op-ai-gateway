// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"testing"
	"time"
)

// TestNormalizeProcessTree pins the closed set of process_tree values: ""
// (omitted) reads as direct, the two values are accepted after trimming, and
// anything else -- a different case included -- is refused.
func TestNormalizeProcessTree(t *testing.T) {
	cases := []struct {
		in     string
		want   ProcessTree
		wantOK bool
	}{
		{"", ProcessTreeDirect, true},
		{"  ", ProcessTreeDirect, true},
		{"direct", ProcessTreeDirect, true},
		{"job", ProcessTreeJob, true},
		{" job ", ProcessTreeJob, true},
		{"Job", "", false},
		{"bogus", "", false},
		{"group", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeProcessTree(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("NormalizeProcessTree(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestMemoryStoreRuntimeSpecProcessTreeDefault pins the memory store against
// the SQL column default: a spec written with no process_tree reads back as
// direct, and a job spec round-trips.
func TestMemoryStoreRuntimeSpecProcessTreeDefault(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Domain: "h", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateApplication(ctx, Application{ID: "app_1", ServerID: "srv_1", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))

	must(t, m.UpsertRuntimeSpec(ctx, RuntimeSpec{ID: "spec_1", MappingID: "map_1", CreatedAt: now, UpdatedAt: now}))
	got, _, err := m.RuntimeSpecByMapping(ctx, "map_1")
	must(t, err)
	if got.ProcessTree != ProcessTreeDirect {
		t.Fatalf("an unset process_tree reads %q, want direct", got.ProcessTree)
	}
	got.ProcessTree = ProcessTreeJob
	must(t, m.UpsertRuntimeSpec(ctx, got))
	again, _, err := m.RuntimeSpecByMapping(ctx, "map_1")
	must(t, err)
	if again.ProcessTree != ProcessTreeJob {
		t.Fatalf("job did not round-trip: %q", again.ProcessTree)
	}
}
