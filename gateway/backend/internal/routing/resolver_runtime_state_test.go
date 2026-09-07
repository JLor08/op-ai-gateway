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
type fakeRuntimeStateEntry struct {
	state         string
	active, queue int
}

// fakeRuntimeState is a test RuntimeModelStateChecker keyed by server id (the seeded
// harness uses one app model per server, so serverID alone disambiguates), mirroring
// fakeLoaded's shape. A server absent from byServer reports ok=false (unknown), exactly
// like *gateway.runtimeModelStateChecker scanning a registry with no matching entry.
type fakeRuntimeState struct {
	byServer map[string]fakeRuntimeStateEntry
}

func (f *fakeRuntimeState) RuntimeModelState(serverID, appModelName string) (string, int, int, bool) {
	e, ok := f.byServer[serverID]
	if !ok {
		return "", 0, 0, false
	}
	return e.state, e.active, e.queue, true
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
// cold-starting another. Neither server has the model loaded/running here (no
// LoadedModelChecker installed). srv_fast's per-server telemetry is deliberately made
// WORSE than srv_slow's (raw score 980 < 1070, so a plain score-only resolver would pick
// srv_slow) while the runtime-status registry reports the model already "starting" on
// srv_fast and unknown/stopped on srv_slow -- the resolver must still prefer srv_fast,
// since routing to the already-loading instance avoids spinning up a redundant second
// process.
func TestResolverPrefersStartingOverColdStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Make srv_fast's raw per-server score worse than srv_slow's (980 < 1070) so the
	// test proves the starting preference, not the fixture's default latency advantage.
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_fast", ReportedAt: now, ActiveRequests: 10, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_fast: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetRuntimeModelStateChecker(&fakeRuntimeState{byServer: map[string]fakeRuntimeStateEntry{
		"srv_fast": {state: "starting"},
		// srv_slow deliberately absent: unknown/stopped, not starting.
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (prefer the already-starting instance over a worse-scored cold candidate)", target.ServerID)
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
