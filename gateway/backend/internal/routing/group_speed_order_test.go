// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"testing"
	"time"
)

// speedGroup builds a "coder-group" over the given member gateway names, in that manual
// (priority) order, under policy — so a test can state the manual order explicitly and
// then assert the speed order overrides it.
func speedGroup(policy GroupPolicy, names ...string) *fakeGroups {
	members := make([]GroupMember, 0, len(names))
	for i, name := range names {
		members = append(members, GroupMember{ID: "gm_" + name, GroupID: "grp_speed", MemberGatewayName: name, Priority: i})
	}
	return &fakeGroups{groups: map[string]fakeGroup{"coder-group": {policy: policy, members: members}}}
}

// groupReadCountingStore counts the two store reads the group path makes per member —
// the mapping lookup and the telemetry lookup — so the no-op invariant can be asserted
// on read COUNT, not just on the served target: a priority-ordered group must not pay for
// the ordering pass (which reads every member's mappings + telemetry).
type groupReadCountingStore struct {
	resolverStore
	mappingCalls   int
	telemetryCalls int
}

func (s *groupReadCountingStore) ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error) {
	s.mappingCalls++
	return s.resolverStore.ActiveMappingsForModel(ctx, gatewayModel, apiFlavor)
}

func (s *groupReadCountingStore) TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error) {
	s.telemetryCalls++
	return s.resolverStore.TelemetryByServer(ctx, serverID)
}

// --- Case 1: the fastest member wins over the manual order -------------------

// coder-b is LAST in the manual order but measures 5x coder-a's speed; member_order=speed
// must serve it.
func TestResolverGroupSpeedOrderServesFastestNotFirst(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}, "coder-a", "coder-b"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (50 tok/s beats the manually-first 10 tok/s)", target.ServerID, target.Model)
	}
}

// --- Case 2: an unmeasured member sorts LAST ---------------------------------

// coder-a has no measured speed at all (effective 0) and is manually first; a measured but
// slow coder-b must still outrank it, so an unmeasured member is a last resort rather than
// an accidental winner (0 must not read as "unknown, so try it first").
func TestResolverGroupSpeedOrderUnmeasuredSortsLast(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 0}, // unmeasured
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 5},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}, "coder-a", "coder-b"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (an unmeasured member must sort last)", target.ServerID, target.Model)
	}
}

// --- Case 3: ties keep the manual order --------------------------------------

// coder-b and coder-c measure exactly the same; the slow coder-a is manually first. The
// tie must resolve to the manually-earlier of the two equals (coder-b), so the ordering
// stays a stable refinement of the manual order rather than an arbitrary permutation.
func TestResolverGroupSpeedOrderTieKeepsManualOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},
		minSpeedFixture{serverID: "srv_c", appID: "app_c", mappingID: "map_c", gatewayName: "coder-c", genTPS: 50},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}, "coder-a", "coder-b", "coder-c"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (equal speeds must keep the manual relative order)", target.ServerID, target.Model)
	}
}

// --- Case 4: the climb margin ------------------------------------------------

// climbMarginResolver pins a climb_up + speed session to the slower coder-b, then makes
// the faster coder-a available again so the next Resolve faces the climb decision. coder-a
// is manually LAST, so only the speed ordering can make it a climb target at all — the
// climb here exercises the ordering and the margin together. It returns the resolver, the
// loaded-model registry (so the caller can flip the free-climb precondition) and the token.
func climbMarginResolver(t *testing.T, now time.Time, fastTPS float64, marginPercent int) (*Resolver, *fakeLoaded, auth.Token) {
	t.Helper()
	ctx := context.Background()
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: fastTPS},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 100},
	)
	unreach := map[string]bool{"app_a": true} // coder-a down at pin time
	loaded := &fakeLoaded{byServer: map[string][]string{}}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(speedGroup(GroupPolicy{
		FailoverMode: modeClimbUp, MemberOrder: MemberOrderSpeed, ClimbSpeedMarginPercent: marginPercent,
	}, "coder-b", "coder-a"))
	resolver.SetLoadedModelChecker(loaded)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}

	t1, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #1 (pin the slower member): %v", err)
	}
	if t1.Model != "coder-b" {
		t.Fatalf("#1 target model = %q, want coder-b (coder-a is down at pin time)", t1.Model)
	}
	delete(unreach, "app_a") // coder-a recovers
	return resolver, loaded, token
}

// A candidate only 10% faster than the pin does not clear a 20% margin: the session stays
// where it is, even though the faster member is available AND loaded (so the free-climb
// rule alone would have moved it).
func TestResolverGroupSpeedOrderClimbBelowMarginKeepsPin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resolver, loaded, token := climbMarginResolver(t, now, 110, 20) // 110 vs 100 = +10%
	loaded.byServer["srv_a"] = []string{"coder-a-up"}               // free climb would be allowed

	target, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (+10%% does not clear the 20%% margin)", target.ServerID, target.Model)
	}
}

// A candidate 50% faster clears the 20% margin AND is already loaded: the session climbs.
func TestResolverGroupSpeedOrderClimbAboveMarginClimbsWhenLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resolver, loaded, token := climbMarginResolver(t, now, 150, 20) // 150 vs 100 = +50%
	loaded.byServer["srv_a"] = []string{"coder-a-up"}

	target, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (+50%% clears the 20%% margin and the target is loaded)", target.ServerID, target.Model)
	}
}

// The margin is a gate ON TOP of the existing free-climb rule, not a replacement: a
// candidate 50% faster that is NOT loaded still does not move the session this turn.
func TestResolverGroupSpeedOrderClimbAboveMarginStaysWhenCold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resolver, _, token := climbMarginResolver(t, now, 150, 20) // clears the margin, but cold

	target, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (the free-climb rule still requires a loaded target)", target.ServerID, target.Model)
	}
}

// A 0 margin is a legitimate value meaning "no margin required": any strictly faster
// loaded member wins, so the same +10% candidate the 20% margin refused now climbs.
func TestResolverGroupSpeedOrderZeroMarginClimbsOnAnyGain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resolver, loaded, token := climbMarginResolver(t, now, 110, 0)
	loaded.byServer["srv_a"] = []string{"coder-a-up"}

	target, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (a 0 margin means any faster member wins)", target.ServerID, target.Model)
	}
}

// A PRIORITY-ordered group must never consult the margin at all — the margin is a
// speed-ordering concept, and the default of 20 would otherwise silently freeze every
// existing climb_up group whose members happen to be measured. Here the higher-priority
// coder-a is measurably SLOWER than the pinned coder-b (80 vs 100 tok/s), so no margin
// could ever be cleared; the session must still climb, because priority order does not
// care about speed. Deleting the `policy.MemberOrder == MemberOrderSpeed` guard around
// the margin check turns this into a kept pin and fails the test.
func TestResolverGroupPriorityOrderIgnoresClimbMargin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 80},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 100},
	)
	unreach := map[string]bool{"app_a": true} // the top-priority coder-a is down at pin time
	loaded := &fakeLoaded{byServer: map[string][]string{}}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(speedGroup(GroupPolicy{
		FailoverMode: modeClimbUp, MemberOrder: MemberOrderPriority, ClimbSpeedMarginPercent: 20,
	}, "coder-a", "coder-b"))
	resolver.SetLoadedModelChecker(loaded)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}

	t1, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #1 (pin the lower-priority member): %v", err)
	}
	if t1.Model != "coder-b" {
		t.Fatalf("#1 target model = %q, want coder-b (coder-a is down at pin time)", t1.Model)
	}

	delete(unreach, "app_a")                          // coder-a recovers...
	loaded.byServer["srv_a"] = []string{"coder-a-up"} // ...and is already loaded, so the free climb applies

	target, err := resolver.Resolve(ctx, token, groupReq("s1"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (a priority-ordered climb must not consult the speed margin)", target.ServerID, target.Model)
	}
}

// --- Case 5: speed order + loaded_only --------------------------------------

// coder-a offers a FAST candidate that is cold and a SLOW one that is loaded; coder-b's
// only candidate is loaded and in between. Under loaded_only the ordering must score each
// member on its ELIGIBLE candidates only — coder-a scores 20 (its loaded candidate), not
// 200 — so coder-b (50) is served. Ordering on the raw, unfiltered speeds would rank
// coder-a first and serve it on its 20 tok/s candidate instead.
func TestResolverGroupSpeedOrderWithLoadedOnlyRanksEligibleCandidates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a1", appID: "app_a1", mappingID: "map_a1", gatewayName: "coder-a", genTPS: 200}, // fast, cold
		minSpeedFixture{serverID: "srv_a2", appID: "app_a2", mappingID: "map_a2", gatewayName: "coder-a", genTPS: 20},  // slow, loaded
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},     // loaded
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed, LoadedOnly: true}, "coder-a", "coder-b"))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{
		"srv_a2": {"coder-a-up"},
		"srv_b":  {"coder-b-up"},
	}})

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (the fastest LOADED member; coder-a's fast candidate is cold)", target.ServerID, target.Model)
	}
}

// --- Case 6: no-op invariant ------------------------------------------------

// member_order=priority (the default) must resolve identically to a policy that omits the
// setting entirely AND must not pay for the ordering pass: the served target and BOTH
// store-read counts stay exactly what the manual walk costs — one mapping read and one
// telemetry read, stopping at the first available member.
func TestResolverGroupSpeedOrderPriorityIsNoOp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	base := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 10},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 50},
	)

	counting := &groupReadCountingStore{resolverStore: base}
	explicit := NewResolver(counting, func() time.Time { return now }, nil)
	explicit.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderPriority}, "coder-a", "coder-b"))
	got, err := explicit.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServerID != "srv_a" || got.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (priority order ignores the measured speeds)", got.ServerID, got.Model)
	}
	if counting.mappingCalls != 1 || counting.telemetryCalls != 1 {
		t.Fatalf("store reads = {mappings:%d telemetry:%d}, want {1 1} (a priority-ordered group must not run the ordering pass)", counting.mappingCalls, counting.telemetryCalls)
	}

	baseline := NewResolver(base, func() time.Time { return now }, nil)
	baseline.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky"}, "coder-a", "coder-b")) // no MemberOrder at all
	want, err := baseline.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("baseline Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("explicit-priority target differs from no-setting target:\n got=%#v\nwant=%#v", got, want)
	}
}

// A group that opts into speed ordering but has NO measurements anywhere behaves exactly
// like a priority-ordered one: every member scores 0, the stable sort leaves the manual
// order untouched, and the top member is served.
func TestResolverGroupSpeedOrderAllUnmeasuredKeepsManualOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_a", appID: "app_a", mappingID: "map_a", gatewayName: "coder-a", genTPS: 0},
		minSpeedFixture{serverID: "srv_b", appID: "app_b", mappingID: "map_b", gatewayName: "coder-b", genTPS: 0},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(speedGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}, "coder-a", "coder-b"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (no measurements => the manual order stands)", target.ServerID, target.Model)
	}
}
