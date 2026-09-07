// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// fakeRuntimeStateEntry is one server's fake live per-model runtime state/metrics.
// noMetrics models a reporting agent that has NOT declared runtime_model_probe: its
// lifecycle state is still valid (ok/state), but the active/queue metrics must not be
// trusted (metricsOK=false), so mergeRuntimeModelMetrics must not overlay them.
type fakeRuntimeStateEntry struct {
	state         string
	active, queue int
	noMetrics     bool
}

// fakeRuntimeState is a test RuntimeModelStateChecker keyed by server id (the seeded
// harness uses one app model per server, so serverID alone disambiguates), mirroring
// fakeLoaded's shape. A server absent from byServer reports ok=false (unknown), exactly
// like *gateway.runtimeModelStateChecker scanning a registry with no matching entry.
// metricsOK = ok && !noMetrics, mirroring the adapter's metricsOK = ok && features.Has.
type fakeRuntimeState struct {
	byServer map[string]fakeRuntimeStateEntry
}

func (f *fakeRuntimeState) RuntimeModelState(serverID, appModelName string) (string, int, int, bool, bool) {
	e, ok := f.byServer[serverID]
	if !ok {
		return "", 0, 0, false, false
	}
	return e.state, e.active, e.queue, true, !e.noMetrics
}

// Per-model live metrics (P4a source: the volatile runtime-status registry) must
// override per-server telemetry in scoring. The seed fixture already favors srv_fast on
// per-server telemetry (latency 100 vs 900 => raw score 1230 vs 1070); reporting
// srv_fast's MODEL heavily loaded (active=10, queue=5 => -350 penalty => 880) while
// srv_slow's model is idle (0, 0 => unchanged 1070) flips the winner to srv_slow,
// proving the merge actually feeds the scorer's penalty terms.
func TestResolverPerModelMetricsOverridePerServerTelemetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "running", active: 10, queue: 5},
		"srv_slow": {state: "running", active: 0, queue: 0},
	}})

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (per-model runtime metrics must override per-server telemetry in scoring)", target.ServerID)
	}
}

// Prefer an already-STARTING (loading) instance of the requested model over
// cold-starting another -- and prove it is the PARTITION (selecting within the
// starting-only subset before the full-pool argmax ever runs), not merely the
// unconditional per-model merge that argmaxByScore applies to every candidate.
//
// srv_fast is reported "starting" with a heavily-loaded per-model state (active=10,
// queue=5): after mergeRuntimeModelMetrics overlays those onto its (otherwise-fast,
// latency=100) telemetry, its score is
//
//	1000 (base) + 10*20 (priority) + 50 (weight) - 10*25 (active) - 5*20 (queue) - 100*0.2 (latency) = 880
//
// srv_slow is absent from the checker (ok=false), so it keeps its unmodified per-server
// telemetry (latency=900, active=0, queue=0):
//
//	1000 + 10*20 + 50 - 900*0.2 = 1070
//
// So srv_slow's MERGED score (1070) is higher than srv_fast's (880): a plain full-pool
// argmaxByScore over the merged pool -- i.e. what selectFromPool would do if the entire
// starting-partition block were deleted -- would pick srv_slow. The resolver must still
// return srv_fast, which can only happen because the starting partition evaluates it in
// an isolated singleton pool (where it is the only, and thus best, viable candidate) and
// returns it before the full-pool argmax ever runs.
func TestResolverPrefersStartingOverColdStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "starting", active: 10, queue: 5},
		// srv_slow deliberately absent: unknown/stopped, not starting -- keeps its good
		// (1070) per-server telemetry score, which BEATS srv_fast's merged 880.
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (the starting partition must pick it even though its merged score, 880, is BELOW srv_slow's merged score, 1070 -- proving the partition itself, not just the per-model merge, drives the choice)", target.ServerID)
	}
}

// The guarantee: a RUNNING (loaded) instance always beats a STARTING one. srv_slow is
// reported loaded (running) via the existing LoadedModelChecker; srv_fast is reported
// "starting" and keeps its normal (better) raw score. Because selectFromPool checks the
// loaded partition strictly BEFORE the starting partition, and the loaded pick here is
// viable, the resolver must return srv_slow without ever consulting the starting
// preference.
func TestResolverRunningBeatsStarting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_slow": {"qwen2.5"}}})
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "starting"},
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (running beats starting)", target.ServerID)
	}
}

// The mirror image of TestResolverPerModelMetricsOverridePerServerTelemetry: when the
// reporting agent has NOT declared runtime_model_probe (metricsOK=false), the per-model
// active/queue it carries are a fabricated 0 (or, here, stale/unreliable) and must NOT
// override per-server telemetry. srv_fast reports its MODEL heavily loaded (active=10,
// queue=5) but with noMetrics=true; because the merge is gated on metricsOK, that load is
// discarded and srv_fast keeps its good per-server score (1230), so it -- not srv_slow --
// wins. This is exactly the routing regression the gate fixes: a non-probing agent's
// model must not be scored on numbers it never actually measured.
func TestResolverPerModelMetricsIgnoredWithoutProbeFeature(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "running", active: 10, queue: 5, noMetrics: true},
		"srv_slow": {state: "running", active: 0, queue: 0, noMetrics: true},
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (per-model metrics from a non-probing agent must NOT override per-server telemetry)", target.ServerID)
	}
}

// Prefer-starting must keep working for an agent that reports a valid lifecycle state but
// has NOT declared runtime_model_probe (metricsOK=false): the state -- not the metrics --
// drives the starting partition. srv_fast is "starting" with noMetrics=true (so its
// active/queue are never overlaid), and the resolver must still pick it over the
// cold-start alternative, proving modelStartingOn keys off ok/state independent of the
// metrics gate.
func TestResolverPreferStartingWorksWithoutProbeFeature(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "starting", active: 10, queue: 5, noMetrics: true},
		// srv_slow absent: unknown/stopped, not starting.
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (prefer-starting must work without runtime_model_probe)", target.ServerID)
	}
}

// The starting preference must FAIL OPEN: when the only starting candidate is
// Score-non-viable (here, a crushing per-model active-request count reported by the
// runtime-status registry drives its score below zero), selection falls back to the
// full pool and picks the other, viable candidate instead of erroring out.
func TestResolverStartingNonViableFallsBackToFullPool(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "starting", active: 1000},
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned error, want fail-open to the full pool: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (starting candidate non-viable: fail open to full pool)", target.ServerID)
	}
}

// A nil RuntimeModelStateChecker keeps the pre-feature behavior byte-identical: no
// per-model metric overlay, no starting preference, no panic. The best-scored server
// (srv_fast) wins exactly as before this feature existed.
func TestResolverNilRuntimeStateCheckerIsLenient(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// No SetRuntimeModelStateChecker call: runtimeState stays nil.

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nil runtime-state checker lenient)", target.ServerID)
	}
}
