// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

func seededResolverStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	servers := []struct {
		serverID, appID, mappingID string
		latency                    int
	}{
		{"srv_fast", "app_fast", "map_fast", 100},
		{"srv_slow", "app_slow", "map_slow", 900},
	}
	for _, s := range servers {
		if err := store.CreateAIServer(ctx, AIServer{ID: s.serverID, Name: s.serverID, Domain: s.serverID + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := store.CreateApplication(ctx, Application{ID: s.appID, ServerID: s.serverID, Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication: %v", err)
		}
		if err := store.CreateMapping(ctx, ModelMapping{ID: s.mappingID, ApplicationID: s.appID, GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: s.serverID, ReportedAt: now, LatencyMS: s.latency, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry: %v", err)
		}
	}
	return store
}

func TestResolverRoutesToMappingBackedTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast", target.ServerID)
	}
	if target.Provider != ProviderMock {
		t.Fatalf("target.Provider = %q, want mock", target.Provider)
	}
	if target.Endpoint != "http://srv_fast.test:8000" {
		t.Fatalf("target.Endpoint = %q", target.Endpoint)
	}
	if target.ProviderModel != "qwen2.5" {
		t.Fatalf("target.ProviderModel = %q, want qwen2.5", target.ProviderModel)
	}
	if target.RouteID != "map_fast" {
		t.Fatalf("target.RouteID = %q, want the serving mapping id map_fast", target.RouteID)
	}
	if target.Timeout != 30000*time.Millisecond {
		t.Fatalf("target.Timeout = %v", target.Timeout)
	}
}

func TestResolverReusesAffinityForSameTokenModelAndFlavor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	first, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if second.ServerID != first.ServerID || second.RouteID != first.RouteID {
		t.Fatalf("affinity not reused: first=%#v second=%#v", first, second)
	}
	aff, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI})
	if err != nil || !ok {
		t.Fatalf("Affinity ok=%v err=%v", ok, err)
	}
	if aff.ApplicationID != "app_fast" || aff.ServerID != "srv_fast" {
		t.Fatalf("affinity row = %#v", aff)
	}
}

func TestResolverBreaksExpiredAffinity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff_expired", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(-time.Minute), LastUsedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID == "srv_slow" {
		t.Fatalf("expired affinity was reused: %#v", target)
	}
}

func TestResolverBreaksAffinityWhenApplicationInactive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff_pin", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	app, err := store.ApplicationByID(ctx, "app_slow")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.Status = ServerStatusDisabled
	if err := store.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast after affinity break", target.ServerID)
	}
	if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI}); ok {
		aff, _, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI})
		if aff.ApplicationID != "app_fast" {
			t.Fatalf("re-established affinity = %#v", aff)
		}
	}
}

func TestResolverReturnsNoModelRouteAndNoHealthyHost(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "missing", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("error = %v, want ErrNoModelRoute", err)
	}
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_bad", Name: "bad", Domain: "bad.test", Status: ServerStatusActive, HealthStatus: HealthUnhealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_bad", ServerID: "srv_bad", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_bad", ApplicationID: "app_bad", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("error = %v, want ErrNoHealthyHost", err)
	}
}

func TestResolverDoesNotShareAffinityAcrossTokens(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "tok_a", UserID: "usr_dev", Active: true}, req); err != nil {
		t.Fatalf("Resolve tok_a: %v", err)
	}
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "tok_b", UserID: "usr_dev", Active: true}, req); err != nil {
		t.Fatalf("Resolve tok_b: %v", err)
	}
	if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_a", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI}); !ok {
		t.Fatalf("affinity for tok_a missing")
	}
	if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_b", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI}); !ok {
		t.Fatalf("affinity for tok_b missing")
	}
}

// fakeReachability is a test ReachabilityChecker: any app id present in
// unreachable is reported unreachable; every other id (including unknown) is
// reachable, mirroring the lenient registry default.
type fakeReachability struct {
	unreachable map[string]bool
}

func (f fakeReachability) Reachable(appID string) bool { return !f.unreachable[appID] }

// With a checker marking the fast app unreachable, selectCandidate skips it and
// the resolver falls through to the slower (but reachable) server.
func TestResolverSkipsUnreachableCandidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	checker := fakeReachability{unreachable: map[string]bool{"app_fast": true}}
	resolver := NewResolver(store, func() time.Time { return now }, checker)

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (fast app unreachable)", target.ServerID)
	}
}

// A pinned affinity to an application that has become unreachable is not
// returned; the resolver falls through to re-selection among reachable apps.
func TestResolverBreaksAffinityWhenApplicationUnreachable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff_pin", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	checker := fakeReachability{unreachable: map[string]bool{"app_slow": true}}
	resolver := NewResolver(store, func() time.Time { return now }, checker)

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID == "srv_slow" {
		t.Fatalf("pinned affinity to an unreachable app was returned: %#v", target)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast after re-selection", target.ServerID)
	}
}

// A nil checker keeps the pre-reachability behavior: the fast server wins even
// though a non-nil checker could have excluded it.
func TestResolverNilCheckerIsLenient(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nil checker lenient)", target.ServerID)
	}
}

// fakeBusy is a test ServerBusyChecker: any server id present in busy is reported
// busy; every other id is available.
type fakeBusy struct {
	busy map[string]bool
}

func (f fakeBusy) ServerBusy(serverID string) bool { return f.busy[serverID] }

// When the only candidate's server is benchmarking, selectCandidate skips it and
// the resolver reports no selectable host.
func TestResolverSkipsBusyServerNoRoute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_only", Name: "only", Domain: "only.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_only", ServerID: "srv_only", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_only", ApplicationID: "app_only", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_only": true}})

	if _, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("error = %v, want ErrNoHealthyHost (only server busy)", err)
	}
}

// With two candidate servers for the model, a busy one is skipped and the other
// is selected.
func TestResolverSkipsBusyServerAndPicksOther(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// The fast server (normally chosen) is benchmarking; only the slow one is free.
	resolver.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_fast": true}})

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (fast server busy)", target.ServerID)
	}
}

// A pinned affinity to a server that is now benchmarking is not reused; the
// resolver falls through to re-selection among the free servers.
func TestResolverBreaksAffinityWhenServerBusy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff_pin", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_slow": true}})

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID == "srv_slow" {
		t.Fatalf("pinned affinity to a busy server was returned: %#v", target)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast after re-selection", target.ServerID)
	}
	// The stale pin to the busy server must have been removed from the store, then
	// re-established to the freshly selected (free) server.
	aff, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI})
	if err != nil {
		t.Fatalf("Affinity re-query: %v", err)
	}
	if !ok {
		t.Fatalf("expected a fresh affinity after re-selection, found none")
	}
	if aff.ServerID != "srv_fast" || aff.ApplicationID != "app_fast" {
		t.Fatalf("stale busy-server pin not replaced: affinity = %#v", aff)
	}
}

// When two candidates for the same model have EQUAL telemetry latency (so the
// telemetry term can't decide) but srv_slow's mapping carries a high measured
// generation throughput, the speed score term (P4a) makes it win over srv_fast's
// zero-metric mapping.
func TestResolverPrefersHigherThroughputMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Neutralize the pre-existing latency difference so only the metric decides.
	for _, s := range []struct {
		serverID string
	}{{"srv_fast"}, {"srv_slow"}} {
		if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: s.serverID, ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", s.serverID, err)
		}
	}
	// Give the slow server's mapping a high measured generation throughput.
	mapping, err := store.MappingByID(ctx, "map_slow")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	mapping.GenTokensPerSecond = 500
	if err := store.UpdateMapping(ctx, mapping); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (higher gen throughput mapping)", target.ServerID)
	}
}

// The P4a no-op invariant at the resolver level: with the default seed (no mapping
// metrics set) the faster-telemetry server still wins exactly as before.
func TestResolverDefaultSeedUnchangedByMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (zero metrics: no-op)", target.ServerID)
	}
}

// A nil busy checker keeps the pre-benchmark behavior: the fast server wins.
func TestResolverNilBusyCheckerIsLenient(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// No SetServerBusyChecker call: busy stays nil.

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nil busy checker lenient)", target.ServerID)
	}
}

// fakeLoaded is a test LoadedModelChecker: it reports the loaded upstream (app)
// model names for a server id (the application id is ignored — the seed maps one
// app per server), mirroring *gateway.LoadedModelRegistry.LoadedAppModels.
type fakeLoaded struct {
	byServer map[string][]string
}

func (f *fakeLoaded) LoadedAppModels(appID, serverID string) []string {
	return f.byServer[serverID]
}

// The prefer-loaded partition DOMINATES the score: with the default seed srv_fast
// would win on telemetry latency, but because the requested model's upstream name
// ("qwen2.5") is reported RESIDENT on srv_slow (and not on srv_fast) the pool is
// restricted to the loaded server, so srv_slow wins even though it scores lower.
func TestResolverPrefersLoadedOverBetterScore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_slow": {"qwen2.5"}}})

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (loaded model dominates the score)", target.ServerID)
	}
}

// When the requested model is reported loaded on NEITHER server the loaded partition
// is empty, so the pool is unchanged and the best-scored server (srv_fast) wins.
func TestResolverNoLoadedFallsBackToScore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// A checker that reports the model loaded nowhere (some unrelated model only).
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{
		"srv_fast": {"some-other-model"},
		"srv_slow": {"another-model"},
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nothing loaded: falls back to score)", target.ServerID)
	}
}

// The prefer-loaded partition must FAIL OPEN (like the context-fit filter above it):
// if the requested model is loaded ONLY on a server whose Score is non-viable (here
// srv_slow, driven non-viable by a crushing active-request count), but an UNLOADED
// server (srv_fast) is healthy/viable, selection must fall back to the full pool and
// route to the unloaded-but-viable server rather than refuse the request with
// ErrNoHealthyHost.
func TestResolverLoadedNonViableFallsBackToUnloaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Make the loaded server (srv_slow) Score-non-viable: a huge active-request count
	// drives its score below zero so Score returns ok=false, while its telemetry stays
	// valid (non-negative, fresh) so it survives Pass 1 and enters the loaded partition.
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_slow", ReportedAt: now, ActiveRequests: 1000, LatencyMS: 900, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_slow: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// The requested model is resident ONLY on the non-viable srv_slow; srv_fast is
	// unloaded but healthy/viable.
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_slow": {"qwen2.5"}}})

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned error, want fail-open to the unloaded viable server: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (loaded server non-viable: fail open to unloaded)", target.ServerID)
	}
}

// A nil loaded checker keeps the pre-prefer-loaded behavior: the best-scored server
// (srv_fast) wins exactly as today. This is the P4a no-op invariant for this term.
func TestResolverNilLoadedCheckerIsLenient(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// No SetLoadedModelChecker call: loaded stays nil.

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nil loaded checker lenient)", target.ServerID)
	}
}

// Pins the estInputTokens arithmetic directly (it is otherwise only exercised via
// Resolve): empty -> 1, N words -> int(N*1.3)+1, and text-only counting (an image
// part contributes no words).
func TestEstInputTokens(t *testing.T) {
	// No messages: words 0 -> int(0*1.3)+1 = 1.
	if got := estInputTokens(inference.Request{}); got != 1 {
		t.Fatalf("estInputTokens(empty) = %d, want 1", got)
	}
	// Ten words of text -> int(10*1.3)+1 = 14.
	tenWords := inference.Request{Messages: []inference.Message{{
		Role:    inference.RoleUser,
		Content: []inference.ContentPart{{Type: inference.ContentText, Text: "one two three four five six seven eight nine ten"}},
	}}}
	if got := estInputTokens(tenWords); got != 14 {
		t.Fatalf("estInputTokens(10 words) = %d, want 14", got)
	}
	// An image-only message contributes no words (Text() is text-only) -> 1.
	imageOnly := inference.Request{Messages: []inference.Message{{
		Role:    inference.RoleUser,
		Content: []inference.ContentPart{{Type: inference.ContentImage, ImageURL: "data:image/png;base64,AAAA"}},
	}}}
	if got := estInputTokens(imageOnly); got != 1 {
		t.Fatalf("estInputTokens(image only) = %d, want 1", got)
	}
}

// Pins the "unknown fits" and exact-fit boundary semantics of requestFitsContext.
func TestRequestFitsContext(t *testing.T) {
	cases := []struct {
		name        string
		contextSize int
		need        int
		want        bool
	}{
		{"unknown-zero-fits-any", 0, 1_000_000, true},
		{"unknown-negative-fits-any", -1, 1_000_000, true},
		{"exact-fit-boundary", 100, 100, true},
		{"one-over-boundary", 100, 101, false},
		{"normal-fit", 100, 50, true},
		{"normal-no-fit", 100, 200, false},
	}
	for _, tc := range cases {
		if got := requestFitsContext(tc.contextSize, tc.need); got != tc.want {
			t.Errorf("%s: requestFitsContext(%d, %d) = %v, want %v", tc.name, tc.contextSize, tc.need, got, tc.want)
		}
	}
}

// contextFitMessages builds a single user message with enough words that the
// coarse estInputTokens estimate clearly exceeds a tiny (8-token) context window.
func contextFitMessages() []inference.Message {
	return []inference.Message{{
		Role: inference.RoleUser,
		Content: []inference.ContentPart{{
			Type: inference.ContentText,
			Text: "one two three four five six seven eight nine ten eleven twelve",
		}},
	}}
}

// A request whose estimated prompt clearly exceeds srv_fast's tiny context window
// drops srv_fast from the candidate pool; srv_slow (large context) wins even though
// its telemetry latency would otherwise lose to srv_fast.
func TestResolverContextFitFiltersTooSmall(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)

	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.ContextSize = 8
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	slow, err := store.MappingByID(ctx, "map_slow")
	if err != nil {
		t.Fatalf("MappingByID map_slow: %v", err)
	}
	slow.ContextSize = 100000
	if err := store.UpdateMapping(ctx, slow); err != nil {
		t.Fatalf("UpdateMapping map_slow: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", Messages: contextFitMessages()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (srv_fast context too small)", target.ServerID)
	}
}

// Both mappings have an unknown (0) context size, so the context-fit filter keeps
// them all and the faster-telemetry server still wins exactly as before.
func TestResolverContextFitUnknownKept(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // both mappings ContextSize 0 (unknown)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", Messages: contextFitMessages()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (unknown context not filtered)", target.ServerID)
	}
}

// When EVERY candidate's context window is too small for the request the filter
// would empty the set; the fail-open falls back to all reachable candidates and the
// best-scored one is returned rather than surfacing ErrNoHealthyHost.
func TestResolverContextFitFailOpen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	for _, id := range []string{"map_fast", "map_slow"} {
		m, err := store.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.ContextSize = 8
		if err := store.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", MaxTokens: 4096, Messages: contextFitMessages()})
	if err != nil {
		t.Fatalf("Resolve returned error, want fail-open candidate: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (fail-open picks the best-scored)", target.ServerID)
	}
}

func TestResolverKeepsSessionAffinitiesSeparate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}
	// A real request carries the session in ClientSessionID (the extracted id the
	// resolver keys affinity on by default) as well as the explicit header SessionID.
	if _, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", SessionID: "session-a", ClientSessionID: "session-a"}); err != nil {
		t.Fatalf("Resolve session-a: %v", err)
	}
	if _, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", SessionID: "session-b", ClientSessionID: "session-b"}); err != nil {
		t.Fatalf("Resolve session-b: %v", err)
	}
	if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "session-a"}); !ok {
		t.Fatalf("affinity session-a missing")
	}
	if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "session-b"}); !ok {
		t.Fatalf("affinity session-b missing")
	}
}

func TestResolverBreaksAffinityWhenApplicationDisablesAffinity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	app, err := store.ApplicationByID(ctx, "app_slow")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AffinityTTLSeconds = 0
	if err := store.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (affinity broken by TTL<=0)", target.ServerID)
	}
}

func TestResolverBreaksAffinityWhenApplicationDropsFlavor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	app, err := store.ApplicationByID(ctx, "app_slow")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.APIFlavors = []string{APIFlavorAnthropic} // drop openai
	if err := store.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (affinity broken by flavor drop)", target.ServerID)
	}
}

// fakeActivity is a test ServerActivityChecker returning raw per-server state so the
// window comparison stays on the resolver side (deterministic under the injected clock).
type fakeActivity struct {
	inFlight map[string]int
	last     map[string]time.Time
}

func (f *fakeActivity) ServerActivity(serverID string) (int, time.Time) {
	return f.inFlight[serverID], f.last[serverID]
}

func TestResolverSwapProtectsBusyNotLoadedServer(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1}}, 30*time.Second)

	target, err := resolver.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("ServerID = %q, want srv_slow (srv_fast swap-protected)", target.ServerID)
	}
}

func TestResolverSwapProtectRecencyWindowBoundary(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)

	r1 := NewResolver(store, func() time.Time { return now }, nil)
	r1.SetServerActivityChecker(&fakeActivity{last: map[string]time.Time{"srv_fast": now.Add(-29 * time.Second)}}, 30*time.Second)
	t1, err := r1.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil || t1.ServerID != "srv_slow" {
		t.Fatalf("inside window: ServerID=%q err=%v, want srv_slow", t1.ServerID, err)
	}

	r2 := NewResolver(store, func() time.Time { return now }, nil)
	r2.SetServerActivityChecker(&fakeActivity{last: map[string]time.Time{"srv_fast": now.Add(-31 * time.Second)}}, 30*time.Second)
	t2, err := r2.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil || t2.ServerID != "srv_fast" {
		t.Fatalf("outside window: ServerID=%q err=%v, want srv_fast", t2.ServerID, err)
	}

	// Exactly at the window (== swapProtectWindow): the comparison is half-open (<), so
	// a completion exactly swapProtectWindow ago is NOT protected and the latency winner
	// (srv_fast) is chosen. Pins the boundary.
	r3 := NewResolver(store, func() time.Time { return now }, nil)
	r3.SetServerActivityChecker(&fakeActivity{last: map[string]time.Time{"srv_fast": now.Add(-30 * time.Second)}}, 30*time.Second)
	t3, err := r3.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil || t3.ServerID != "srv_fast" {
		t.Fatalf("at window boundary: ServerID=%q err=%v, want srv_fast (half-open, not protected)", t3.ServerID, err)
	}
}

func TestResolverSwapProtectNeverProtectsLoadedServer(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}})
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 3}}, 30*time.Second)

	target, err := resolver.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("ServerID = %q, want srv_fast (loaded server not swap-protected)", target.ServerID)
	}
}

func TestResolverSwapProtectFailOpenWhenAllProtected(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1, "srv_slow": 1}}, 30*time.Second)

	target, err := resolver.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve should fail open, got err: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("ServerID = %q, want srv_fast (fail-open)", target.ServerID)
	}
}

// Swap protection must FAIL OPEN on VIABILITY, not just emptiness (the P4b must-fix,
// mirroring the prefer-loaded fail-open): if protection excludes some candidates and the
// SURVIVING candidates are all Score-non-viable while an EXCLUDED busy candidate WAS
// viable, selection must fall back to the full pool and route to the busy-but-viable
// server rather than refuse the request with ErrNoHealthyHost. Here srv_slow is
// busy-not-loaded (so swap-protected) but Score-viable, while srv_fast is idle
// (unprotected) yet driven Score-non-viable by a crushing active-request count.
func TestResolverSwapProtectFailsOpenOnNonViableSurvivors(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Make the surviving (idle, unprotected) server srv_fast Score-non-viable: a huge
	// active-request count drives its score below zero so Score returns ok=false, while
	// its telemetry stays valid (non-negative, fresh) so it survives Pass 1.
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_fast", ReportedAt: now, ActiveRequests: 1000, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_fast: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// srv_slow is busy (in flight) and not loaded, so it is swap-protected; but it is
	// Score-viable. srv_fast is idle (unprotected) but non-viable.
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_slow": 1}}, 30*time.Second)

	// Empty token id bypasses affinity so this exercises selectCandidate directly.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned error, want fail-open to the busy viable server: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (survivor non-viable: fail open to the busy viable server)", target.ServerID)
	}
}

// Model loaded on one server (idle) and busy-not-loaded on the OTHER server: swap
// protection drops the busy one AND prefer-loaded picks the loaded one, so the loaded
// idle server wins even though it would otherwise lose on telemetry latency.
func TestResolverSwapProtectCrossServer(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// The requested model's upstream name is resident on the idle srv_slow.
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_slow": {"qwen2.5"}}})
	// srv_fast is busy and not loaded, so it is swap-protected and excluded.
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1}}, 30*time.Second)

	target, err := resolver.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (busy srv_fast swap-protected; loaded srv_slow preferred)", target.ServerID)
	}
}

func TestResolverNilActivityCheckerIsLenient(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(context.Background(), auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("ServerID = %q, want srv_fast (nil activity checker no-op)", target.ServerID)
	}
}

// The capacity cap (CP3) excludes a server already at its effective concurrency ceiling.
// srv_fast is the requested model's LOADED server (so prefer-loaded would otherwise win it,
// and swap-protection cannot touch a loaded server) AND it is at capacity
// (max_concurrency=2, in-flight=2). The cap must exclude it anyway — the cap does not spare
// a loaded candidate — so selection falls to srv_slow. Marking srv_fast loaded isolates the
// cap from swap-protection: both filters key off in-flight load, so a loaded flag is the only
// way to attribute srv_fast's exclusion specifically to the capacity cap (absent the cap,
// prefer-loaded would pick the loaded srv_fast).
func TestCapacityCapExcludesOverloadedServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.MaxConcurrency = 2
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	slow, err := store.MappingByID(ctx, "map_slow")
	if err != nil {
		t.Fatalf("MappingByID map_slow: %v", err)
	}
	slow.MaxConcurrency = 8
	if err := store.UpdateMapping(ctx, slow); err != nil {
		t.Fatalf("UpdateMapping map_slow: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// srv_fast is loaded (so prefer-loaded would win it; swap-protection can't touch it) but
	// at capacity (2/2), while srv_slow is idle and under its cap (0/8).
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}})
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 2, "srv_slow": 0}}, 30*time.Second)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (srv_fast at capacity, excluded by the cap)", target.ServerID)
	}
}

// When every candidate is at/over its cap the filter empties the pool; the fail-open routes
// to the (single) best candidate anyway rather than surfacing ErrNoHealthyHost. CP4 replaces
// this with the admission queue.
func TestCapacityCapFailsOpenWhenAllAtCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_only", Name: "only", Domain: "only.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_only", ServerID: "srv_only", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_only", ApplicationID: "app_only", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", MaxConcurrency: 2, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_only", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_only": 2}}, 30*time.Second)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve should fail open when all candidates at capacity, got err: %v", err)
	}
	if target.ServerID != "srv_only" {
		t.Fatalf("target.ServerID = %q, want srv_only (fail-open)", target.ServerID)
	}
}

// The cap is a no-op when max_concurrency is unknown (0): the filter appends every candidate
// unconditionally, so a heavily-loaded server is NOT excluded by the cap. srv_fast is loaded
// (neutralizing swap-protection, which is orthogonal) and carries in-flight 99; with
// max_concurrency 0 it still wins (the pre-CP3 prefer-loaded result). The contrast resolver
// (same everything but max_concurrency 1 on srv_fast) proves the win flips to srv_slow only
// once the cap is active — so the 0 case is genuinely the cap's no-op, not prefer-loaded doing
// the work.
func TestCapacityCapNoOpWhenMaxConcurrencyZero(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // MaxConcurrency 0 everywhere

	rZero := NewResolver(store, func() time.Time { return now }, nil)
	rZero.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}})
	rZero.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 99}}, 30*time.Second)
	tZero, err := rZero.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve (max_concurrency 0): %v", err)
	}
	if tZero.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (max_concurrency 0: cap never excludes)", tZero.ServerID)
	}

	// Contrast: set srv_fast max_concurrency 1 (< 99 in flight); now the cap bites even the
	// loaded server and the winner flips to srv_slow, proving the 0 case above was the cap's
	// no-op rather than prefer-loaded masking it.
	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.MaxConcurrency = 1
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	rCap := NewResolver(store, func() time.Time { return now }, nil)
	rCap.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}})
	rCap.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 99}}, 30*time.Second)
	tCap, err := rCap.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve (max_concurrency 1): %v", err)
	}
	if tCap.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (cap now excludes the loaded, over-capacity srv_fast)", tCap.ServerID)
	}
}

// The cap is gated on r.activity != nil: without an activity source there is no in-flight
// signal, so even a set max_concurrency (1) is inert and selection is the pre-CP3 baseline.
func TestCapacityCapNoOpWhenActivityNil(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.MaxConcurrency = 1
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// No SetServerActivityChecker call: activity stays nil, so the cap is a no-op.

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (nil activity: cap inert)", target.ServerID)
	}
}

// A session reservation reduces the effective cap by one slot per live pinned session:
// effectiveCap = max_concurrency − activeReservedSessions. srv_fast has max_concurrency 3 and
// 2 in flight, srv_slow is uncapped. With no reservation 2 < 3 so srv_fast is admitted (and
// wins via prefer-loaded); after reserving one session on srv_fast the effective cap drops to
// 2, so 2 < 2 is false and srv_fast is excluded, flipping the winner to srv_slow. srv_fast is
// loaded to neutralize swap-protection so the flip is attributable to the reservation alone.
func TestSessionReservationReducesEffectiveCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.MaxConcurrency = 3
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	// srv_slow stays uncapped (max_concurrency 0), so it always survives the cap as fallback.
	loaded := &fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}}
	activity := &fakeActivity{inFlight: map[string]int{"srv_fast": 2}}

	// No reservation: effectiveCap 3, 2 < 3 => srv_fast admitted and wins.
	rNoRes := NewResolver(store, func() time.Time { return now }, nil)
	rNoRes.SetLoadedModelChecker(loaded)
	rNoRes.SetServerActivityChecker(activity, 30*time.Second)
	tNoRes, err := rNoRes.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve (no reservation): %v", err)
	}
	if tNoRes.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (2 < effectiveCap 3)", tNoRes.ServerID)
	}

	// One reserved session on srv_fast: effectiveCap 2, 2 < 2 false => srv_fast excluded.
	rRes := NewResolver(store, func() time.Time { return now }, nil)
	rRes.SetLoadedModelChecker(loaded)
	rRes.SetServerActivityChecker(activity, 30*time.Second)
	rRes.SetSessionReservation(60 * time.Second)
	rRes.reservation.touch("srv_fast", "sess1", now)
	tRes, err := rRes.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve (reserved): %v", err)
	}
	if tRes.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (reservation drops effectiveCap to 2)", tRes.ServerID)
	}
}

// A pinned (affinity) request bypasses the capacity cap entirely: resolveAffinity returns the
// pinned server before selectCandidate (where the cap lives) ever runs. Here an initial
// Resolve pins srv_fast, then srv_fast is driven far over its cap (max_concurrency 1, 5 in
// flight); a second Resolve with the same token+session+model still returns srv_fast.
func TestPinnedRequestBypassesCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // AffinityTTLSeconds 1800 on both apps
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", SessionID: "sess1"}

	// First Resolve (no cap yet) pins srv_fast.
	first, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if first.ServerID != "srv_fast" {
		t.Fatalf("first pin = %q, want srv_fast", first.ServerID)
	}

	// Now drive srv_fast far over its cap and add the activity source.
	fast, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	fast.MaxConcurrency = 1
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 5}}, 30*time.Second)

	// Second Resolve reuses the pin, bypassing the cap.
	second, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if second.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (pinned request bypasses the cap)", second.ServerID)
	}
}

func TestResolverBreaksAffinityWhenNoActiveMappingRemains(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	if err := store.UpsertAffinity(ctx, RouteAffinity{ID: "aff", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, ApplicationID: "app_slow", ServerID: "srv_slow", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity: %v", err)
	}
	mapping, err := store.MappingByID(ctx, "map_slow")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	mapping.Status = ServerStatusDisabled
	if err := store.UpdateMapping(ctx, mapping); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (affinity broken by no active mapping)", target.ServerID)
	}
}

// TestCapacityCapFailsOpenToViableAtCapServer: the cap must fail open on VIABILITY, not
// only emptiness. If the sole Score-viable candidate is at capacity (excluded by the cap)
// and the under-cap survivor is Score-non-viable, the request must still be served by the
// viable-at-cap server (the upstream then queues it), NOT refused with ErrNoHealthyHost.
// Regression for the CP3 verification's confirmed MAJOR (the cap had an emptiness-only
// fail-open, mirroring the P4a/P4b viability fail-open that this restores).
func TestCapacityCapFailsOpenToViableAtCapServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)

	// srv_fast: max_concurrency 2, at cap (inFlight 2), but healthy/fresh telemetry => viable.
	mf, err := store.MappingByID(ctx, "map_fast")
	if err != nil {
		t.Fatalf("MappingByID map_fast: %v", err)
	}
	mf.MaxConcurrency = 2
	if err := store.UpdateMapping(ctx, mf); err != nil {
		t.Fatalf("UpdateMapping map_fast: %v", err)
	}
	// srv_slow: max_concurrency 8, under cap (inFlight 0), but overloaded telemetry
	// (ActiveRequests 100) drives its Score <= 0 => non-viable.
	ms, err := store.MappingByID(ctx, "map_slow")
	if err != nil {
		t.Fatalf("MappingByID map_slow: %v", err)
	}
	ms.MaxConcurrency = 8
	if err := store.UpdateMapping(ctx, ms); err != nil {
		t.Fatalf("UpdateMapping map_slow: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_slow", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ActiveRequests: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_slow: %v", err)
	}

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 2, "srv_slow": 0}}, 30*time.Second)
	// Mark srv_fast loaded so swap-protection never protects it — isolates the CAP's
	// viability fail-open as the thing under test (srv_fast is excluded by the cap, not swap).
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_fast": {"qwen2.5"}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve error %v, want the viable-at-cap srv_fast served (cap must fail open on viability)", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (viable-at-cap; srv_slow is under-cap but non-viable)", target.ServerID)
	}
}

// TestSessionReservationTouchPrunesStale: touch prunes a server's stale entries on write,
// so the reservation map stays bounded by LIVE sessions even when the cap's read-time
// activeCount prune never runs for that server (a max_concurrency==0 / pinned-only server).
// Regression for the CP3 verification's confirmed MEDIUM (unbounded growth).
func TestSessionReservationTouchPrunesStale(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	res := newSessionReservation(60 * time.Second)
	// Two sessions touched now, one touched long ago (stale).
	res.touch("srv1", "old", now.Add(-120*time.Second))
	res.touch("srv1", "a", now)
	res.touch("srv1", "b", now) // this write must prune "old"
	if got := len(res.byServer["srv1"]); got != 2 {
		t.Fatalf("map size = %d, want 2 (stale 'old' pruned on write, 'a'+'b' live)", got)
	}
	if _, stillThere := res.byServer["srv1"]["old"]; stillThere {
		t.Fatal("stale 'old' entry survived a later touch (write-time prune missing => unbounded growth)")
	}
	if got := res.activeCount("srv1", now); got != 2 {
		t.Fatalf("activeCount = %d, want 2", got)
	}
}

// fakeAdmission is a test AdmissionController. It records each call's inputs and returns a
// scripted result per call (onWait may mutate external state, e.g. free a slot). A nil onWait
// returns nil (slot freed).
type fakeAdmission struct {
	calls      int
	gotServers []string
	gotTimeout time.Duration
	onWait     func(call int) error
}

func (f *fakeAdmission) WaitForSlot(_ context.Context, serverIDs []string, timeout time.Duration) error {
	f.calls++
	f.gotServers = append([]string(nil), serverIDs...)
	f.gotTimeout = timeout
	if f.onWait != nil {
		return f.onWait(f.calls)
	}
	return nil
}

// singleCapCandidateStore seeds ONE server/app/mapping serving qwen-coder with the given
// max_concurrency, mirroring TestCapacityCapFailsOpenWhenAllAtCap so there is exactly one
// decisive candidate (no viable alternative to route around the cap).
func singleCapCandidateStore(t *testing.T, now time.Time, maxConcurrency int) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_only", Name: "only", Domain: "only.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_only", ServerID: "srv_only", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_only", ApplicationID: "app_only", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", MaxConcurrency: maxConcurrency, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_only", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return store
}

// TestResolverQueuesWhenAllAtCapThenServes: the single decisive candidate is at its cap, so
// selectCandidate signals errAllAtCapacity and Resolve parks in the admission queue. The
// controller frees the slot (mutating the shared activity map) and returns nil; on retry the
// candidate is now under-cap and is served. Verifies the wait-retry loop end-to-end.
func TestResolverQueuesWhenAllAtCapThenServes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := singleCapCandidateStore(t, now, 1)
	activity := &fakeActivity{inFlight: map[string]int{"srv_only": 1}} // at cap (1 < 1 is false)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(activity, 30*time.Second)
	adm := &fakeAdmission{onWait: func(call int) error {
		activity.inFlight["srv_only"] = 0 // free the slot so the retry admits
		return nil
	}}
	resolver.SetAdmissionController(adm)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v, want served after slot frees", err)
	}
	if target.ServerID != "srv_only" {
		t.Fatalf("target.ServerID = %q, want srv_only (served after queue wait)", target.ServerID)
	}
	if adm.calls < 1 {
		t.Fatalf("WaitForSlot calls = %d, want >= 1 (request should have queued)", adm.calls)
	}
	if len(adm.gotServers) != 1 || adm.gotServers[0] != "srv_only" {
		t.Fatalf("WaitForSlot servers = %v, want [srv_only]", adm.gotServers)
	}
}

// TestResolverQueueTimeoutReturns503Sentinel: an at-cap candidate whose queue wait times out
// propagates ErrAdmissionQueueTimeout (→ HTTP 503) from Resolve.
func TestResolverQueueTimeoutReturns503Sentinel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := singleCapCandidateStore(t, now, 1)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_only": 1}}, 30*time.Second)
	resolver.SetAdmissionController(&fakeAdmission{onWait: func(call int) error { return ErrAdmissionQueueTimeout }})

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout", err)
	}
}

// TestResolverQueueFullReturns503Sentinel: an at-cap candidate whose queue is full propagates
// ErrAdmissionQueueFull (→ HTTP 503) from Resolve.
func TestResolverQueueFullReturns503Sentinel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := singleCapCandidateStore(t, now, 1)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_only": 1}}, 30*time.Second)
	resolver.SetAdmissionController(&fakeAdmission{onWait: func(call int) error { return ErrAdmissionQueueFull }})

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if !errors.Is(err, ErrAdmissionQueueFull) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueFull", err)
	}
}

// TestResolverNoControllerFailsOpenAtCap: with NO admission controller wired, the CP3 fail-open
// is preserved — an at-cap candidate is still served (never queued), no error. The no-op
// invariant of this task.
func TestResolverNoControllerFailsOpenAtCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := singleCapCandidateStore(t, now, 1)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_only": 1}}, 30*time.Second)
	// No SetAdmissionController: r.admission stays nil, so selectCandidate keeps the CP3 fail-open.

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve = %v, want fail-open serve (no controller)", err)
	}
	if target.ServerID != "srv_only" {
		t.Fatalf("target.ServerID = %q, want srv_only (CP3 fail-open, no queuing)", target.ServerID)
	}
}

// TestResolverPinnedNeverQueues: a pinned (affinity) request bypasses selectCandidate (and so
// the admission queue) entirely — resolveAffinity returns first. Here an initial Resolve pins
// srv_fast; then BOTH candidate servers are driven at cap so an unpinned request WOULD queue;
// a second Resolve with the same token+session+model returns srv_fast and never calls
// WaitForSlot.
func TestResolverPinnedNeverQueues(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // AffinityTTLSeconds 1800 on both apps
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", SessionID: "sess1"}

	// First Resolve pins srv_fast (no cap/controller yet).
	first, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if first.ServerID != "srv_fast" {
		t.Fatalf("first pin = %q, want srv_fast", first.ServerID)
	}

	// Drive BOTH servers over cap so an unpinned request would queue, and wire a controller
	// that fails the test if it is ever consulted for a pinned request.
	for _, id := range []string{"map_fast", "map_slow"} {
		m, err := store.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.MaxConcurrency = 1
		if err := store.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1, "srv_slow": 1}}, 30*time.Second)
	adm := &fakeAdmission{onWait: func(call int) error { return ErrAdmissionQueueTimeout }}
	resolver.SetAdmissionController(adm)

	second, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("second Resolve: %v (pinned request must never queue)", err)
	}
	if second.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (pinned bypass)", second.ServerID)
	}
	if adm.calls != 0 {
		t.Fatalf("WaitForSlot calls = %d, want 0 (a pinned request must never queue)", adm.calls)
	}
}

// TestResolverQueueTimeoutIsMaxAcrossCandidates: the queue wait timeout passed to the
// controller is the MAX admission_queue_timeout_seconds across all candidate apps. Both
// candidate servers are at cap (whole pool capped-empty → queue), and the controller stops the
// loop so the recorded gotTimeout can be asserted.
func TestResolverQueueTimeoutIsMaxAcrossCandidates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	run := func(t *testing.T, fastSecs, slowSecs, wantSecs int) {
		t.Helper()
		store := seededResolverStore(t, now)
		for _, m := range []struct {
			appID string
			mapID string
			secs  int
		}{{"app_fast", "map_fast", fastSecs}, {"app_slow", "map_slow", slowSecs}} {
			app, err := store.ApplicationByID(ctx, m.appID)
			if err != nil {
				t.Fatalf("ApplicationByID %s: %v", m.appID, err)
			}
			app.AdmissionQueueTimeoutSeconds = m.secs
			if err := store.UpdateApplication(ctx, app); err != nil {
				t.Fatalf("UpdateApplication %s: %v", m.appID, err)
			}
			mp, err := store.MappingByID(ctx, m.mapID)
			if err != nil {
				t.Fatalf("MappingByID %s: %v", m.mapID, err)
			}
			mp.MaxConcurrency = 1
			if err := store.UpdateMapping(ctx, mp); err != nil {
				t.Fatalf("UpdateMapping %s: %v", m.mapID, err)
			}
		}
		resolver := NewResolver(store, func() time.Time { return now }, nil)
		resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1, "srv_slow": 1}}, 30*time.Second)
		adm := &fakeAdmission{onWait: func(call int) error { return ErrAdmissionQueueTimeout }}
		resolver.SetAdmissionController(adm)

		_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
		if !errors.Is(err, ErrAdmissionQueueTimeout) {
			t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout (both at cap)", err)
		}
		if adm.gotTimeout != time.Duration(wantSecs)*time.Second {
			t.Fatalf("WaitForSlot timeout = %v, want %ds (max across candidates)", adm.gotTimeout, wantSecs)
		}
		if len(adm.gotServers) != 2 {
			t.Fatalf("WaitForSlot servers = %v, want both candidate servers", adm.gotServers)
		}
	}

	t.Run("10_and_30", func(t *testing.T) { run(t, 10, 30, 30) })
	t.Run("0_and_30", func(t *testing.T) { run(t, 0, 30, 30) })
}

// TestResolverQueueTimeoutIsWallClockDeadline: a bounded admission_queue_timeout_seconds is
// an ABSOLUTE wall-clock deadline. Even if the controller keeps returning nil (the queue's
// internal liveness re-check wakes the caller sub-second to re-read the cap), the resolver
// must 503 once the deadline elapses rather than re-arm the full timeout every iteration and
// wait forever. Regression for the re-verification finding that the re-check defeated a
// bounded timeout under sustained saturation. Uses an advancing clock so the deadline passes.
func TestResolverQueueTimeoutIsWallClockDeadline(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := singleCapCandidateStore(t, base, 1) // maxC=1
	// Bound the wait to 1s on the only candidate app.
	app, err := store.ApplicationByID(ctx, "app_only")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AdmissionQueueTimeoutSeconds = 1
	if err := store.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	// Clock advances 400ms per call, so the 1s deadline elapses after a few loop iterations.
	var calls int
	clock := func() time.Time {
		t := base.Add(time.Duration(calls) * 400 * time.Millisecond)
		calls++
		return t
	}
	resolver := NewResolver(store, clock, nil)
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_only": 1}}, 30*time.Second) // stays at cap forever
	// The controller ALWAYS returns nil (simulating the liveness re-check), never freeing a slot.
	resolver.SetAdmissionController(&fakeAdmission{onWait: func(int) error { return nil }})

	if _, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve = %v, want ErrAdmissionQueueTimeout (bounded timeout must be a wall-clock deadline honored despite recheck-nil)", err)
	}
}

// TestAffinitySessionMode proves the resolver keys route affinity on the extracted
// ClientSessionID by default (new mode) and on the explicit X-OP-AI-Gateway-Session-ID
// header (req.SessionID) in legacy mode — the latter being byte-identical to the
// pre-feature behavior (the No-Op invariant).
func TestAffinitySessionMode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}

	// Default (new) mode: affinity keys on ClientSessionID; the explicit header is empty.
	t.Run("default keys on ClientSessionID", func(t *testing.T) {
		store := seededResolverStore(t, now)
		resolver := NewResolver(store, func() time.Time { return now }, nil)

		if _, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", ClientSessionID: "A"}); err != nil {
			t.Fatalf("Resolve ClientSessionID=A: %v", err)
		}
		affA, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "A"})
		if err != nil || !ok {
			t.Fatalf("affinity for ClientSessionID=A missing (ok=%v err=%v)", ok, err)
		}
		if affA.SessionID != "A" {
			t.Fatalf("stored affinity SessionID = %q, want %q (new mode keys on ClientSessionID)", affA.SessionID, "A")
		}
		// A distinct ClientSessionID must produce a DISTINCT affinity id.
		if _, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", ClientSessionID: "B"}); err != nil {
			t.Fatalf("Resolve ClientSessionID=B: %v", err)
		}
		affB, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "B"})
		if err != nil || !ok {
			t.Fatalf("affinity for ClientSessionID=B missing (ok=%v err=%v)", ok, err)
		}
		if affA.ID == affB.ID {
			t.Fatalf("affinity ids not distinct across ClientSessionIDs: A=%q B=%q", affA.ID, affB.ID)
		}
	})

	// Legacy mode: ClientSessionID is IGNORED; affinity keys on the empty explicit header
	// — byte-identical to the pre-feature behavior (the No-Op invariant).
	t.Run("legacy ignores ClientSessionID", func(t *testing.T) {
		store := seededResolverStore(t, now)
		resolver := NewResolver(store, func() time.Time { return now }, nil)
		resolver.SetAffinitySessionMode(true)

		if _, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat", ClientSessionID: "A"}); err != nil {
			t.Fatalf("Resolve legacy ClientSessionID=A: %v", err)
		}
		// The affinity must be keyed on the EMPTY explicit header, not on ClientSessionID.
		aff, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: ""})
		if err != nil || !ok {
			t.Fatalf("legacy affinity keyed on empty header missing (ok=%v err=%v)", ok, err)
		}
		if aff.SessionID != "" {
			t.Fatalf("legacy stored affinity SessionID = %q, want empty (ClientSessionID must be ignored)", aff.SessionID)
		}
		if _, ok, _ := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "A"}); ok {
			t.Fatalf("legacy mode wrongly keyed affinity on ClientSessionID=A")
		}
	})
}
