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

// twoMemberLoadedOnlyGroup builds a "coder-group" over coder-a (priority 0) then coder-b
// (priority 1) under the given policy, mirroring twoMemberGroup but letting the test set
// LoadedOnly (and any other policy field) explicitly.
func twoMemberLoadedOnlyGroup(policy GroupPolicy) *fakeGroups {
	return &fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: policy, members: []GroupMember{
			{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
			{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
		}},
	}}
}

// --- Case 1: prefer a loaded lower-priority member over a cold top one -------

// The top-priority member (coder-a) is cold; the lower-priority one (coder-b) is already
// loaded. loaded_only must serve coder-b rather than cold-start coder-a.
func TestResolverGroupLoadedOnlyPrefersLoadedOverTopPriority(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberLoadedOnlyGroup(GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_b": {"coder-b-up"}}})

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (coder-a is cold; coder-b is loaded)", target.ServerID, target.Model)
	}
}

// --- Case 2: nothing loaded anywhere -> the restriction is dropped -----------

// When no member has anything loaded, loaded_only must never be a dead end: the
// resolveGroup relaxation drops it and the ordinary walk serves the top-priority member
// (a cold load is allowed rather than refusing the request).
func TestResolverGroupLoadedOnlyFallsBackWhenNothingLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberLoadedOnlyGroup(GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{}}) // nothing loaded

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v, want the relaxed retry to serve the top-priority member", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (relaxed retry, ordinary priority walk)", target.ServerID, target.Model)
	}
}

// --- Case 3: climb_up + loaded_only never warms -------------------------------

// climb_up would ordinarily fire a best-effort load-ahead Warm for a cold higher-priority
// member; loaded_only must suppress that entirely (warming defeats the point of "never
// trigger a load"). No SetLoadedModelChecker is installed here (r.loaded stays nil), so
// the loaded filter itself is a no-op and member-1 qualifies as memberOK as soon as it is
// reachable — proving Warm is suppressed by policy.LoadedOnly directly, not merely because
// member-1 never became eligible.
func TestResolverGroupLoadedOnlyClimbUpNeverWarms(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true} // member-1 down at pin time
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberLoadedOnlyGroup(GroupPolicy{FailoverMode: modeClimbUp, LoadedOnly: true}))
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2 (coder-b)
		t.Fatalf("Resolve #1: %v", err)
	}

	delete(unreach, "app_a") // member-1 (coder-a) recovers, still "cold" (nil loaded checker)
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (no warm, keep serving the pin)", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("warmer.warmed = %v, want none — climb_up + loaded_only must never warm", warmer.warmed)
	}
}

// --- Case 4: pin gating -------------------------------------------------------

// A pinned member that is no longer loaded must be abandoned in favour of a member that
// IS loaded, even though the pin would otherwise still be perfectly servable (reachable,
// live mapping). The loaded filter runs on the pin's own selectMember call, not just the
// fresh walk.
func TestResolverGroupLoadedOnlyPinGatedWhenNoLongerLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	loaded := &fakeLoaded{byServer: map[string][]string{"srv_a": {"coder-a-up"}}} // coder-a loaded initially
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberLoadedOnlyGroup(GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}))
	resolver.SetLoadedModelChecker(loaded)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	t1, err := resolver.Resolve(ctx, token, req) // pins to coder-a (top priority, loaded)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if t1.ServerID != "srv_a" || t1.Model != "coder-a" {
		t.Fatalf("#1 target = {%q,%q}, want {srv_a, coder-a}", t1.ServerID, t1.Model)
	}

	// coder-a is evicted (no longer loaded); coder-b becomes loaded instead.
	delete(loaded.byServer, "srv_a")
	loaded.byServer["srv_b"] = []string{"coder-b-up"}
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (pin abandoned: coder-a is no longer loaded)", t2.ServerID, t2.Model)
	}
	aff, ok, err := store.Affinity(ctx, groupAffinityKey("tok1", "s1"))
	if err != nil || !ok {
		t.Fatalf("group pin missing after #2: ok=%v err=%v", ok, err)
	}
	if aff.ResolvedModel != "coder-b" {
		t.Fatalf("re-pin ResolvedModel = %q, want coder-b", aff.ResolvedModel)
	}
}

// --- Case 5: no-op invariant ---------------------------------------------------

// An explicit LoadedOnly == false must resolve identically to a policy that omits the
// setting entirely.
func TestResolverGroupLoadedOnlyFalseIsNoOp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	members := []GroupMember{
		{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
	}

	withFalse := NewResolver(store, func() time.Time { return now }, nil)
	withFalse.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky", LoadedOnly: false}, members: members},
	}})
	got, err := withFalse.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	baseline := NewResolver(store, func() time.Time { return now }, nil)
	baseline.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: members}, // no LoadedOnly at all
	}})
	want, err := baseline.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("baseline Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("explicit-false target differs from no-setting target:\n got=%#v\nwant=%#v", got, want)
	}
}

// climb_up + LoadedOnly == false must warm exactly like it did before loaded_only existed.
func TestResolverGroupLoadedOnlyFalseWarmsLikeBefore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true}
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberLoadedOnlyGroup(GroupPolicy{FailoverMode: modeClimbUp, LoadedOnly: false}))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{}}) // nothing loaded
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2
		t.Fatalf("Resolve #1: %v", err)
	}

	delete(unreach, "app_a") // member-1 available but cold
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b}", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 1 || warmer.warmed[0] != "coder-a" {
		t.Fatalf("warmer.warmed = %v, want [coder-a] (LoadedOnly=false must warm exactly like before)", warmer.warmed)
	}
}

// --- Case 6: both settings + min_speed_fallback=error never drops the floor ---

// The sole candidate is BELOW the speed floor but IS loaded. With both LoadedOnly and a
// speed floor set and MinSpeedFallback=error, the attempts are (floor+loaded), then
// (floor only) — LoadedOnly is dropped first but the floor never is. Since the candidate
// fails the floor regardless of loadedness, both attempts fail and the group reports
// ErrNoHealthyHost (a live, too-slow member), never falling back further to a
// floor-dropped attempt (which would incorrectly serve it).
func TestResolverGroupLoadedOnlyAndFloorBothWithErrorNeverDropsFloor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_solo", appID: "app_solo", mappingID: "map_solo", gatewayName: "coder-solo", genTPS: 10}, // below the floor of 20
	)
	policy := GroupPolicy{FailoverMode: "sticky", LoadedOnly: true, MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackError}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"solo-group": {policy: policy, members: []GroupMember{
		{ID: "gm_solo", GroupID: "grp2", MemberGatewayName: "coder-solo", Priority: 0},
	}}}})
	// The sole candidate IS loaded, proving the FLOOR (not the loaded filter) is what
	// rejects it on both attempts.
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_solo": {"coder-solo-up"}}})

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "solo-group", APIFlavor: "openai_chat"})
	if !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("err = %v, want ErrNoHealthyHost (attempt1 floor+loaded fails; attempt2 floor-only still fails; the floor is never dropped)", err)
	}
	if errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v also matches ErrNoModelRoute, want ONLY ErrNoHealthyHost", err)
	}
}

// --- Case 7: both settings + min_speed_fallback=ignore must reach fully relaxed ---

// The relaxation ladder must be MONOTONE: once loaded_only is dropped (attempt 2), it must
// STAY dropped in attempt 3 (the floor-dropped attempt), never come back. The sole candidate
// is both below the floor AND not loaded, so only the fully-relaxed attempt (neither filter)
// can serve it. A ladder that rebuilds each relaxation from the original policy would
// resurrect loaded_only in attempt 3 and this would incorrectly report ErrNoHealthyHost
// instead of serving the member — exactly the bug this test guards against.
func TestResolverGroupLoadedOnlyAndFloorBothWithIgnoreReachesFullyRelaxed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seedMinSpeedStore(t, now,
		minSpeedFixture{serverID: "srv_solo", appID: "app_solo", mappingID: "map_solo", gatewayName: "coder-solo", genTPS: 10}, // below the floor of 20
	)
	policy := GroupPolicy{FailoverMode: "sticky", LoadedOnly: true, MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackIgnore}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"solo-group": {policy: policy, members: []GroupMember{
		{ID: "gm_solo", GroupID: "grp2", MemberGatewayName: "coder-solo", Priority: 0},
	}}}})
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{}}) // nothing loaded anywhere

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "solo-group", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve: %v, want the fully-relaxed attempt (neither loaded_only nor the floor) to serve coder-solo", err)
	}
	if target.ServerID != "srv_solo" || target.Model != "coder-solo" {
		t.Fatalf("target = {%q,%q}, want {srv_solo, coder-solo}", target.ServerID, target.Model)
	}
}
