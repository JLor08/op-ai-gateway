// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"reflect"
	"testing"
	"time"
)

// minSpeedFixture is one server+application+mapping serving gatewayName, with a
// measured generation speed and an optional concurrency ceiling. Two fixtures sharing
// the same gatewayName are two CANDIDATES of the same group member.
type minSpeedFixture struct {
	serverID, appID, mappingID, gatewayName string
	genTPS                                  float64
	maxConcurrency                          int
}

// seedMinSpeedStore seeds one independent server/app/mapping per fixture, mirroring
// seededGroupStore's fixture shape but letting each test control the mapping's
// measured speed and concurrency ceiling.
func seedMinSpeedStore(t *testing.T, now time.Time, fixtures ...minSpeedFixture) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	for _, f := range fixtures {
		if err := store.CreateAIServer(ctx, AIServer{ID: f.serverID, Name: f.serverID, Domain: f.serverID + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", f.serverID, err)
		}
		if err := store.CreateApplication(ctx, Application{ID: f.appID, ServerID: f.serverID, Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", f.appID, err)
		}
		if err := store.CreateMapping(ctx, ModelMapping{ID: f.mappingID, ApplicationID: f.appID, GatewayModelName: f.gatewayName, AppModelName: f.gatewayName + "-up", MaxConcurrency: f.maxConcurrency, GenTokensPerSecond: f.genTPS, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", f.mappingID, err)
		}
		if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: f.serverID, ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", f.serverID, err)
		}
	}
	return store
}

// telemetryCountingStore wraps a resolverStore and counts TelemetryByServer calls, so a
// test can assert the no-op invariant: MinTokensPerSecond == 0 must add ZERO extra store
// reads over the pre-floor behavior (the sole call comes from argmaxByScore scoring the
// winning candidate; none from a floor check, which must not run at all when the floor
// is 0).
type telemetryCountingStore struct {
	resolverStore
	telemetryCalls int
}

func (s *telemetryCountingStore) TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error) {
	s.telemetryCalls++
	return s.resolverStore.TelemetryByServer(ctx, serverID)
}

// --- Case 1: member-level floor ----------------------------------------------

// A floor of 20 tok/s excludes the member whose only candidate measures 10 tok/s; the
// 50 tok/s member is served instead.
func TestResolverGroupMinSpeedFiltersSlowMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_slow", appID: "app_slow", mappingID: "map_slow", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_fast", appID: "app_fast", mappingID: "map_fast", gatewayName: "coder-b", genTPS: 50},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_fast" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_fast, coder-b} (coder-a's 10 tok/s is below the 20 floor)", target.ServerID, target.Model)
	}
}

// --- Case 2: candidate-level floor (the hole the spec calls out) -------------

// One member offers TWO candidates: a 50 tok/s candidate (at its concurrency cap) and a
// 10 tok/s candidate (free). With a member-level filter ("does this member have ANY
// candidate above the floor?") the whole candidate set would pass unfiltered, and when
// the 50 candidate is at capacity the walk would happily fall back to serving the 10 —
// exactly the hole the spec calls out. With a CANDIDATE-level filter the 10 tok/s
// candidate never enters the pool at all, so when the 50 is at capacity the member
// queues on the fast candidate's server alone; it is never served via the slow one.
func TestResolverGroupMinSpeedGatesCandidateNotMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_fast", appID: "app_fast", mappingID: "map_fast", gatewayName: "solo", genTPS: 50, maxConcurrency: 1},
		minSpeedFixture{serverID: "srv_slow", appID: "app_slow", mappingID: "map_slow", gatewayName: "solo", genTPS: 10},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"solo-group": {policy: policy, members: []GroupMember{
		{ID: "gm_solo", GroupID: "grp2", MemberGatewayName: "solo", Priority: 0},
	}}}})
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_fast": 1}}, 30*time.Second) // srv_fast at its cap of 1
	sentinel := errors.New("test: queue check")
	adm := &fakeAdmission{onWait: func(int) error { return sentinel }}
	resolver.SetAdmissionController(adm)

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "solo-group", APIFlavor: "openai_chat"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Resolve err = %v, want the admission sentinel (the member must queue, not fall back to the slow candidate)", err)
	}
	if adm.calls != 1 {
		t.Fatalf("WaitForSlot calls = %d, want 1", adm.calls)
	}
	if len(adm.gotServers) != 1 || adm.gotServers[0] != "srv_fast" {
		t.Fatalf("WaitForSlot servers = %v, want [srv_fast] only — the 10 tok/s candidate must never enter the pool", adm.gotServers)
	}
}

// --- Case 3: floor unreachable + error fallback -------------------------------

// Nothing in the group can actually be served: one member's sole candidate is below the
// floor (filtered out entirely), the other's sole candidate clears the floor but is
// unreachable for an unrelated reason. With MinSpeedFallbackError there is no relaxed
// retry, so the group reports ErrNoHealthyHost — it has a live mapping (the reachable-
// gate member), just nothing usable, not ErrNoModelRoute (which would wrongly claim the
// model itself is unknown).
func TestResolverGroupMinSpeedUnreachableFloorErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	checker := fakeReachability{unreachable: map[string]bool{"app_b": true}}
	resolver := NewResolver(store, func() time.Time { return now }, checker)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}}}})

	_, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("err = %v, want ErrNoHealthyHost", err)
	}
}

// --- Case 4: floor unreachable + ignore fallback ------------------------------

// The lone member's only candidate is below the floor; MinSpeedFallbackIgnore relaxes
// the floor to 0 on retry and serves the slow member rather than refusing the request.
func TestResolverGroupMinSpeedUnreachableFloorIgnored(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_solo", appID: "app_solo", mappingID: "map_solo", gatewayName: "coder-solo", genTPS: 10},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackIgnore}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"solo-group": {policy: policy, members: []GroupMember{
		{ID: "gm_solo", GroupID: "grp2", MemberGatewayName: "coder-solo", Priority: 0},
	}}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "solo-group", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v, want the slow member served under the ignore fallback", err)
	}
	if target.ServerID != "srv_solo" || target.Model != "coder-solo" {
		t.Fatalf("target = {%q,%q}, want {srv_solo, coder-solo}", target.ServerID, target.Model)
	}
}

// --- Pure floor exhaustion: no other gating at all ----------------------------

// EVERY member has live, otherwise-perfectly-routable mappings that are simply too
// slow — no reachability/capacity gating anywhere. This must classify as
// ErrNoHealthyHost, not ErrNoModelRoute: the members are known and live, just gated by
// speed. Distinguishes a too-slow member (memberUnavailable) from an unknown one
// (memberNoMapping).
func TestResolverGroupMinSpeedAllFilteredErrorsNoHealthyHost(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 15},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}}}})

	_, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("err = %v, want ErrNoHealthyHost (both members are live, just too slow)", err)
	}
	if errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v also matches ErrNoModelRoute, want ONLY ErrNoHealthyHost", err)
	}
}

// A member with NO live mapping at all (not merely a slow one) must still report
// ErrNoModelRoute even with a floor configured — the floor must never turn a genuinely
// unknown member into a "gated" one, pinning the memberNoMapping/memberUnavailable
// distinction from the other side.
func TestResolverGroupMinSpeedNoLiveMappingStillNoModelRoute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now) // no fixtures: "ghost" has no mapping anywhere
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"ghost-group": {policy: policy, members: []GroupMember{
		{ID: "gm_ghost", GroupID: "grp4", MemberGatewayName: "ghost", Priority: 0},
	}}}})

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "ghost-group", APIFlavor: "openai_chat"})
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute (the member has no live mapping at all)", err)
	}
}

// The pure all-filtered case still relaxes under MinSpeedFallbackIgnore: the first
// attempt reports ErrNoHealthyHost (tolerated), the retry with the floor dropped serves
// the top-priority member.
func TestResolverGroupMinSpeedAllFilteredIgnoreServesRelaxed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 15},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackIgnore}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v, want the top-priority member served under the ignore fallback", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (relaxed retry, priority order)", target.ServerID, target.Model)
	}
}

// --- Unmeasured candidates never satisfy a floor ------------------------------

// A candidate with no measured speed (GenTokensPerSecond == 0, i.e. effectiveGenTPS ==
// 0) never satisfies ANY positive floor, however small.
func TestResolverGroupMinSpeedUnmeasuredNeverSatisfies(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 0}, // unmeasured
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 5},
	)
	policy := GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 1, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (coder-a is unmeasured, so even a floor of 1 excludes it)", target.ServerID, target.Model)
	}
}

// --- No-op invariant -----------------------------------------------------------

// An explicit MinTokensPerSecond == 0 must resolve identically to a policy that omits
// the setting entirely AND must add zero extra store reads: the floor check must not
// run at all when the floor is 0.
func TestResolverGroupMinSpeedZeroIsNoOp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	base := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},
	)
	members := []GroupMember{
		{ID: "gm_a", GroupID: "grp3", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp3", MemberGatewayName: "coder-b", Priority: 1},
	}

	counting := &telemetryCountingStore{resolverStore: base}
	withFloor := NewResolver(counting, func() time.Time { return now }, nil)
	withFloor.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 0}, members: members},
	}})
	got, err := withFloor.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if counting.telemetryCalls != 1 {
		t.Fatalf("TelemetryByServer calls = %d, want 1 (an explicit 0 floor must add zero extra store reads)", counting.telemetryCalls)
	}

	baseline := NewResolver(base, func() time.Time { return now }, nil)
	baseline.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: members}, // no MinTokensPerSecond at all
	}})
	want, err := baseline.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("baseline Resolve: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit-zero-floor target differs from no-setting target:\n got=%#v\nwant=%#v", got, want)
	}
}
