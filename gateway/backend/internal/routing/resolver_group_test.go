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

// fakeGroups is a test GroupResolver. groups maps a group's gateway model name to its
// ordered members + selection policy; locked marks model names whose direct (non-group)
// request must be refused (visibility == "locked").
type fakeGroups struct {
	groups map[string]fakeGroup
	locked map[string]bool
}

type fakeGroup struct {
	members []GroupMember
	policy  GroupPolicy
}

func (f *fakeGroups) Group(name string) ([]GroupMember, GroupPolicy, bool) {
	g, ok := f.groups[name]
	if !ok {
		return nil, GroupPolicy{}, false
	}
	return g.members, g.policy, true
}

func (f *fakeGroups) DirectAllowed(name string) bool { return !f.locked[name] }

// fakeWarmer is a test ModelWarmer recording every Warm call so a climb_up test can assert
// the load-ahead fires (or does not) with the expected member name.
type fakeWarmer struct {
	warmed []string
}

func (w *fakeWarmer) Warm(_ context.Context, name string) { w.warmed = append(w.warmed, name) }

// seededGroupStore seeds two independent single-server members, coder-a (srv_a) and
// coder-b (srv_b), each healthy with its own mapping. Distinct gateway model names so
// each is an independent routing target; a group over them is defined by fakeGroups.
func seededGroupStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	members := []struct {
		serverID, appID, mappingID, gwName string
	}{
		{"srv_a", "app_a", "map_a", "coder-a"},
		{"srv_b", "app_b", "map_b", "coder-b"},
	}
	for _, m := range members {
		if err := store.CreateAIServer(ctx, AIServer{ID: m.serverID, Name: m.serverID, Domain: m.serverID + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", m.serverID, err)
		}
		if err := store.CreateApplication(ctx, Application{ID: m.appID, ServerID: m.serverID, Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", m.appID, err)
		}
		if err := store.CreateMapping(ctx, ModelMapping{ID: m.mappingID, ApplicationID: m.appID, GatewayModelName: m.gwName, AppModelName: m.gwName + "-up", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.mappingID, err)
		}
		if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: m.serverID, ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", m.serverID, err)
		}
	}
	return store
}

// twoMemberGroup builds a "coder-group" over coder-a (priority 0) then coder-b (priority 1).
func twoMemberGroup(mode string) *fakeGroups {
	return &fakeGroups{groups: map[string]fakeGroup{
		"coder-group": {policy: GroupPolicy{FailoverMode: mode}, members: []GroupMember{
			{ID: "gm_a", GroupID: "grp1", MemberGatewayName: "coder-a", Priority: 0},
			{ID: "gm_b", GroupID: "grp1", MemberGatewayName: "coder-b", Priority: 1},
		}},
	}}
}

func groupReq(session string) inference.Request {
	// A real request carries the session in ClientSessionID (the extracted id the
	// resolver keys affinity on by default) as well as the explicit header SessionID.
	return inference.Request{Model: "coder-group", APIFlavor: "openai_chat", SessionID: session, ClientSessionID: session}
}

// groupAffinityKey mirrors the key a group pin is stored under (Model = group name).
func groupAffinityKey(tokenID, session string) AffinityKey {
	return AffinityKey{APITokenID: tokenID, Model: "coder-group", APIFlavor: APIFlavorOpenAI, SessionID: session}
}

// --- No-Op invariant ---------------------------------------------------------

// A group seam that does NOT know a plain model (not a group, not locked) leaves the
// single-model path byte-identical: the resolved Target is exactly the one a nil-seam
// resolver produces. Proves the group dispatch is transparent for non-group models.
func TestResolverGroupSeamNoOpForNonGroupModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	base := NewResolver(store, func() time.Time { return now }, nil) // nil groups
	want, err := base.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("baseline Resolve: %v", err)
	}

	withGroups := NewResolver(store, func() time.Time { return now }, nil)
	withGroups.SetGroupResolver(twoMemberGroup("sticky")) // knows coder-group, not qwen-coder
	got, err := withGroups.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("group-seam Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("non-group target differs with a group seam:\n got=%#v\nwant=%#v", got, want)
	}
}

// --- Failover order ----------------------------------------------------------

// The first member (coder-a) is healthy, so the group serves it.
func TestResolverGroupServesFirstMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberGroup("sticky"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (first member)", target.ServerID, target.Model)
	}
}

// The first member is unreachable, so the group fails over to the second (coder-b).
func TestResolverGroupFailsOverToSecondMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: map[string]bool{"app_a": true}})
	resolver.SetGroupResolver(twoMemberGroup("sticky"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" || target.Model != "coder-b" {
		t.Fatalf("target = {%q,%q}, want {srv_b, coder-b} (first member unreachable)", target.ServerID, target.Model)
	}
}

// --- At-capacity failover + queue --------------------------------------------

// The first member is at its concurrency cap (with an admission controller wired) while
// the second is free: the walk skips the capped member and serves the free one WITHOUT
// queuing.
func TestResolverGroupAtCapacitySkipsToFreeMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	fast, err := store.MappingByID(ctx, "map_a")
	if err != nil {
		t.Fatalf("MappingByID map_a: %v", err)
	}
	fast.MaxConcurrency = 1
	if err := store.UpdateMapping(ctx, fast); err != nil {
		t.Fatalf("UpdateMapping map_a: %v", err)
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberGroup("sticky"))
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_a": 1}}, 30*time.Second) // srv_a at cap
	adm := &fakeAdmission{}
	resolver.SetAdmissionController(adm)

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.ServerID != "srv_b" {
		t.Fatalf("target.ServerID = %q, want srv_b (coder-a at cap, coder-b free)", target.ServerID)
	}
	if adm.calls != 0 {
		t.Fatalf("WaitForSlot calls = %d, want 0 (a free member must not trigger a queue)", adm.calls)
	}
}

// EVERY member is at capacity: the walk finds nobody, queues on the UNION of at-capacity
// servers, and once the controller frees a slot the re-walk serves that member.
func TestResolverGroupAllAtCapacityQueuesUnionThenServes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	for _, id := range []string{"map_a", "map_b"} {
		m, err := store.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.MaxConcurrency = 1
		if err := store.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	activity := &fakeActivity{inFlight: map[string]int{"srv_a": 1, "srv_b": 1}} // both at cap
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(twoMemberGroup("sticky"))
	resolver.SetServerActivityChecker(activity, 30*time.Second)
	adm := &fakeAdmission{onWait: func(call int) error {
		activity.inFlight["srv_a"] = 0 // free the first member so the re-walk admits it
		return nil
	}}
	resolver.SetAdmissionController(adm)

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("Resolve: %v, want served after slot frees", err)
	}
	if target.ServerID != "srv_a" {
		t.Fatalf("target.ServerID = %q, want srv_a (freed member wins on re-walk)", target.ServerID)
	}
	if adm.calls != 1 {
		t.Fatalf("WaitForSlot calls = %d, want 1 (queued once on the union)", adm.calls)
	}
	if len(adm.gotServers) != 2 || adm.gotServers[0] != "srv_a" || adm.gotServers[1] != "srv_b" {
		t.Fatalf("WaitForSlot servers = %v, want [srv_a srv_b] (union of at-capacity members)", adm.gotServers)
	}
}

// TestResolverGroupQueueDeadlinePreservedWhenUnionBecomesUnbounded proves the
// finite-once-finite-forever property: once a bounded admission deadline is set
// (a member with a positive admission_queue_timeout_seconds was in the at-capacity
// union), it is enforced on EVERY subsequent wake — even if that bounded member
// later leaves the union and the remaining at-cap members are all-unbounded. A
// finite wait must never be silently promoted back to unbounded.
func TestResolverGroupQueueDeadlinePreservedWhenUnionBecomesUnbounded(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cur := base // mutable clock shared with onWait
	store := seededGroupStore(t, base)
	// Member A (coder-a) has a 10s admission timeout; member B (coder-b) is unbounded (0).
	appA, err := store.ApplicationByID(ctx, "app_a")
	if err != nil {
		t.Fatalf("ApplicationByID app_a: %v", err)
	}
	appA.AdmissionQueueTimeoutSeconds = 10
	if err := store.UpdateApplication(ctx, appA); err != nil {
		t.Fatalf("UpdateApplication app_a: %v", err)
	}
	// Both members at capacity.
	for _, id := range []string{"map_a", "map_b"} {
		m, err := store.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.MaxConcurrency = 1
		if err := store.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	activity := &fakeActivity{inFlight: map[string]int{"srv_a": 1, "srv_b": 1}}
	reach := fakeReachability{unreachable: map[string]bool{}}
	resolver := NewResolver(store, func() time.Time { return cur }, reach)
	resolver.SetGroupResolver(twoMemberGroup("sticky"))
	resolver.SetServerActivityChecker(activity, 30*time.Second)
	adm := &fakeAdmission{onWait: func(call int) error {
		// After the first queue (deadline set from A's 10s): A goes DOWN and the clock
		// jumps past the deadline. The remaining at-cap union is {srv_b}, all-unbounded.
		reach.unreachable["app_a"] = true
		cur = base.Add(11 * time.Second)
		return nil
	}}
	resolver.SetAdmissionController(adm)

	if _, err := resolver.Resolve(ctx, auth.Token{}, groupReq("")); !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("Resolve err = %v, want ErrAdmissionQueueTimeout (the finite deadline must survive the union going all-unbounded)", err)
	}
	if adm.calls != 1 {
		t.Fatalf("WaitForSlot calls = %d, want 1 (the 2nd iteration must hit the preserved deadline, not queue again unbounded)", adm.calls)
	}
}

// --- Sticky mode -------------------------------------------------------------

// Sticky lifecycle: (1) first member down → pin the second; (2) first member recovers →
// still serve the pinned second (no climb-back); (3) pinned second goes down → re-walk to
// the recovered first + re-pin.
func TestResolverGroupStickyPinFallThroughRepin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true} // member-1 (coder-a) down
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup("sticky"))
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	// (1) member-1 down → pin member-2.
	t1, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if t1.ServerID != "srv_b" || t1.Model != "coder-b" {
		t.Fatalf("#1 target = {%q,%q}, want {srv_b, coder-b}", t1.ServerID, t1.Model)
	}
	aff, ok, err := store.Affinity(ctx, groupAffinityKey("tok1", "s1"))
	if err != nil || !ok {
		t.Fatalf("group pin missing after #1: ok=%v err=%v", ok, err)
	}
	if aff.Model != "coder-group" || aff.ResolvedModel != "coder-b" {
		t.Fatalf("#1 pin = {Model:%q ResolvedModel:%q}, want {coder-group, coder-b}", aff.Model, aff.ResolvedModel)
	}

	// (2) member-1 recovers → sticky keeps member-2 (no climb-back).
	delete(unreach, "app_a")
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (sticky: no climb-back)", t2.ServerID, t2.Model)
	}

	// (3) pinned member-2 goes down → re-walk to member-1 + re-pin.
	unreach["app_b"] = true
	t3, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	if t3.ServerID != "srv_a" || t3.Model != "coder-a" {
		t.Fatalf("#3 target = {%q,%q}, want {srv_a, coder-a} (pin down → re-walk)", t3.ServerID, t3.Model)
	}
	aff3, ok, err := store.Affinity(ctx, groupAffinityKey("tok1", "s1"))
	if err != nil || !ok {
		t.Fatalf("group pin missing after #3: ok=%v err=%v", ok, err)
	}
	if aff3.ResolvedModel != "coder-a" {
		t.Fatalf("#3 re-pin ResolvedModel = %q, want coder-a", aff3.ResolvedModel)
	}
}

// --- climb_up mode -----------------------------------------------------------

// climb_up: pinned to member-2 (member-1 was down at pin time); member-1 recovers AND is
// already LOADED → climb to it now (no cold-start stall) and re-pin. Warmer NOT called.
func TestResolverGroupClimbUpToLoadedHigherPriority(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true} // member-1 (coder-a) down at pin time
	loaded := &fakeLoaded{byServer: map[string][]string{}}
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup(modeClimbUp))
	resolver.SetLoadedModelChecker(loaded)
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	// (1) member-1 down → pin member-2.
	t1, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if t1.ServerID != "srv_b" || t1.Model != "coder-b" {
		t.Fatalf("#1 target = {%q,%q}, want {srv_b, coder-b}", t1.ServerID, t1.Model)
	}

	// (2) member-1 recovers AND is resident → climb to it, re-pin.
	delete(unreach, "app_a")
	loaded.byServer["srv_a"] = []string{"coder-a-up"} // coder-a's upstream model loaded on srv_a
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_a" || t2.Model != "coder-a" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_a, coder-a} (climb to loaded higher-priority)", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("warmer.warmed = %v, want none (climb target already loaded)", warmer.warmed)
	}
	aff, ok, err := store.Affinity(ctx, groupAffinityKey("tok1", "s1"))
	if err != nil || !ok {
		t.Fatalf("group pin missing after climb: ok=%v err=%v", ok, err)
	}
	if aff.ResolvedModel != "coder-a" {
		t.Fatalf("re-pin ResolvedModel = %q, want coder-a (climbed pin)", aff.ResolvedModel)
	}
}

// climb_up: pinned to member-2; member-1 is available but NOT loaded → keep serving the pin
// THIS turn and fire the load-ahead warmer exactly once for member-1 (no climb; pin stays).
func TestResolverGroupClimbUpWarmsHigherPriorityNotLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true}
	loaded := &fakeLoaded{byServer: map[string][]string{}} // nothing loaded
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup(modeClimbUp))
	resolver.SetLoadedModelChecker(loaded)
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2
		t.Fatalf("Resolve #1: %v", err)
	}

	// member-1 available but cold → keep the pin, warm member-1 once.
	delete(unreach, "app_a")
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (keep the pin while warming)", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 1 || warmer.warmed[0] != "coder-a" {
		t.Fatalf("warmer.warmed = %v, want [coder-a] (load-ahead the higher-priority member)", warmer.warmed)
	}
	aff, ok, err := store.Affinity(ctx, groupAffinityKey("tok1", "s1"))
	if err != nil || !ok {
		t.Fatalf("group pin missing after #2: ok=%v err=%v", ok, err)
	}
	if aff.ResolvedModel != "coder-b" {
		t.Fatalf("pin ResolvedModel = %q, want coder-b (no climb while warming)", aff.ResolvedModel)
	}
}

// climb_up with a NIL warmer: pinned to member-2, member-1 available but not loaded → no
// panic, keeps serving the pin (passive climb — only switches once loaded by other traffic).
func TestResolverGroupClimbUpNilWarmerPassive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup(modeClimbUp))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{}})
	// No SetModelWarmer: r.warmer stays nil.
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2
		t.Fatalf("Resolve #1: %v", err)
	}

	delete(unreach, "app_a") // member-1 available but cold, nil warmer
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2 (nil warmer must not panic): %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (nil warmer → passive, keep pin)", t2.ServerID, t2.Model)
	}
}

// climb_up: the pin is already the highest-priority available member → serve it, never run a
// climb switch, never warm.
func TestResolverGroupClimbUpPinAlreadyBestNoWarm(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, nil) // all reachable
	resolver.SetGroupResolver(twoMemberGroup(modeClimbUp))
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	// (1) everything up → pin member-1 (highest priority).
	t1, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if t1.ServerID != "srv_a" || t1.Model != "coder-a" {
		t.Fatalf("#1 target = {%q,%q}, want {srv_a, coder-a}", t1.ServerID, t1.Model)
	}
	// (2) pin is already the best available → serve it, no climb, no warm.
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_a" || t2.Model != "coder-a" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_a, coder-a}", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("warmer.warmed = %v, want none (pin is already the highest-priority member)", warmer.warmed)
	}
}

// climb_up: the pin goes DOWN while a higher-priority member is available but COLD → fall
// down immediately to the first available member (no wait-for-load), and never warm the
// served member (falling down is not a climb).
func TestResolverGroupClimbUpPinDownFallsDownColdNoWarm(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true} // member-1 down at pin time
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup(modeClimbUp))
	resolver.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{}}) // member-1 cold
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2
		t.Fatalf("Resolve #1: %v", err)
	}

	// pin (member-2) now down, member-1 available but cold → fall down to member-1 now.
	delete(unreach, "app_a")
	unreach["app_b"] = true
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_a" || t2.Model != "coder-a" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_a, coder-a} (fall down to first available even cold)", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("warmer.warmed = %v, want none (falling down never warms the served member)", warmer.warmed)
	}
}

// Sticky regression: pinned to member-2 while member-1 is available AND loaded → STILL serve
// member-2 (no climb-back) and NEVER warm — proves sticky ignores both climb and load-ahead.
func TestResolverGroupStickyNeverClimbsNeverWarms(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	unreach := map[string]bool{"app_a": true} // member-1 down at pin time
	loaded := &fakeLoaded{byServer: map[string][]string{}}
	warmer := &fakeWarmer{}
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup("sticky")) // STICKY
	resolver.SetLoadedModelChecker(loaded)
	resolver.SetModelWarmer(warmer)
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	if _, err := resolver.Resolve(ctx, token, req); err != nil { // pin member-2
		t.Fatalf("Resolve #1: %v", err)
	}

	// member-1 recovers AND is loaded → sticky STILL keeps member-2, no climb, no warm.
	delete(unreach, "app_a")
	loaded.byServer["srv_a"] = []string{"coder-a-up"}
	t2, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if t2.ServerID != "srv_b" || t2.Model != "coder-b" {
		t.Fatalf("#2 target = {%q,%q}, want {srv_b, coder-b} (sticky: no climb even to a loaded higher-priority member)", t2.ServerID, t2.Model)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("sticky warmer.warmed = %v, want none (sticky never warms)", warmer.warmed)
	}
}

// --- Locked / error semantics ------------------------------------------------

// A model whose visibility is "locked" (DirectAllowed==false) requested directly is
// refused with ErrNoModelRoute even though it has live mappings; an unlocked model still
// resolves.
func TestResolverGroupLockedModelDirectRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{locked: map[string]bool{"coder-a": true}}) // coder-a locked, no groups

	// Locked model requested directly → refused.
	if _, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "coder-a", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("locked direct request err = %v, want ErrNoModelRoute", err)
	}
	// An unlocked model still resolves.
	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "coder-b", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("unlocked Resolve: %v", err)
	}
	if target.ServerID != "srv_b" {
		t.Fatalf("target.ServerID = %q, want srv_b (unlocked model resolves)", target.ServerID)
	}
}

// A group whose OWN visibility is "locked" (DirectAllowed==false for the group
// NAME) requested directly is refused with ErrNoModelRoute BEFORE the failover walk
// — the exact parallel to a locked model. A locked group is effectively unreachable
// but stays visible/revertible in the admin UI.
func TestResolverGroupLockedGroupDirectRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	g := twoMemberGroup("sticky")
	g.locked = map[string]bool{"coder-group": true} // the GROUP name itself is locked
	resolver.SetGroupResolver(g)

	if _, err := resolver.Resolve(ctx, auth.Token{}, groupReq("")); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("locked group direct request err = %v, want ErrNoModelRoute", err)
	}
}

// A group whose OWN visibility is "hidden" is delisted from the offered listing but
// a DIRECT request still runs the failover walk and serves a member — hidden ≠
// locked, and the registry only locks "locked" (DirectAllowed stays true).
func TestResolverGroupHiddenGroupDirectResolves(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// A hidden group is NOT locked → fakeGroups.locked is empty → DirectAllowed true.
	resolver.SetGroupResolver(twoMemberGroup("sticky"))

	target, err := resolver.Resolve(ctx, auth.Token{}, groupReq(""))
	if err != nil {
		t.Fatalf("hidden group direct request: %v", err)
	}
	if target.ServerID != "srv_a" || target.Model != "coder-a" {
		t.Fatalf("target = {%q,%q}, want {srv_a, coder-a} (hidden group still serves its top member)", target.ServerID, target.Model)
	}
}

// Members exist but all are gated (both servers unreachable) → ErrNoHealthyHost.
func TestResolverGroupAllMembersGatedNoHealthyHost(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, fakeReachability{unreachable: map[string]bool{"app_a": true, "app_b": true}})
	resolver.SetGroupResolver(twoMemberGroup("sticky"))

	if _, err := resolver.Resolve(ctx, auth.Token{}, groupReq("")); !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("err = %v, want ErrNoHealthyHost (members present but all gated)", err)
	}
}

// A group with zero members → ErrNoModelRoute (like an unknown model).
func TestResolverGroupEmptyMembersNoModelRoute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{"empty-group": {policy: GroupPolicy{FailoverMode: "sticky"}}}})

	if _, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "empty-group", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute (zero-member group)", err)
	}
}

// A group whose members exist but NONE has any live mapping (member names route to no
// active mapping) → ErrNoModelRoute (unknown model), NOT ErrNoHealthyHost. Distinguishes
// "no live mapping anywhere" (§3g, 404) from "live but all gated" (502). Contrast with
// TestResolverGroupAllMembersGatedNoHealthyHost, whose members DO have live mappings.
func TestResolverGroupNoLiveMappingNoModelRoute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := seededGroupStore(t, now) // seeds coder-a / coder-b, NOT ghost-x / ghost-y
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"ghost-group": {policy: GroupPolicy{FailoverMode: "sticky"}, members: []GroupMember{
			{ID: "gm_x", GroupID: "grpg", MemberGatewayName: "ghost-x", Priority: 0},
			{ID: "gm_y", GroupID: "grpg", MemberGatewayName: "ghost-y", Priority: 1},
		}},
	}})

	if _, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{Model: "ghost-group", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute (members present but no live mapping)", err)
	}
}

// --- No affinity leak --------------------------------------------------------

// affinityRecordingStore records every UpsertAffinity call so a test can assert the group
// path never writes an empty ResolvedModel (an unusable row that also leaks a reservation).
type affinityRecordingStore struct {
	Store
	upserts []RouteAffinity
}

func (s *affinityRecordingStore) UpsertAffinity(ctx context.Context, aff RouteAffinity) error {
	s.upserts = append(s.upserts, aff)
	return s.Store.UpsertAffinity(ctx, aff)
}

// Across a servable pin, a sticky reuse, a fall-through re-pin, AND a failed (all-gated)
// resolve, EVERY group upsert carries a non-empty ResolvedModel; the failed resolve writes
// no group row at all.
func TestResolverGroupNeverUpsertsEmptyResolvedModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	unreach := map[string]bool{"app_a": true} // start: member-1 down
	rec := &affinityRecordingStore{Store: seededGroupStore(t, now)}
	resolver := NewResolver(rec, func() time.Time { return now }, fakeReachability{unreachable: unreach})
	resolver.SetGroupResolver(twoMemberGroup("sticky"))
	token := auth.Token{ID: "tok1", UserID: "u1", Active: true}
	req := groupReq("s1")

	// Pin member-2, reuse it, then fall through + re-pin to member-1.
	if _, err := resolver.Resolve(ctx, token, req); err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if _, err := resolver.Resolve(ctx, token, req); err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	delete(unreach, "app_a")
	unreach["app_b"] = true
	if _, err := resolver.Resolve(ctx, token, req); err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	// Now every member is gated → resolve fails, and must write NO group row.
	unreach["app_a"] = true
	upsertsBefore := len(rec.upserts)
	if _, err := resolver.Resolve(ctx, token, req); !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("Resolve #4 err = %v, want ErrNoHealthyHost", err)
	}
	if len(rec.upserts) != upsertsBefore {
		t.Fatalf("a failed group resolve wrote %d affinity rows, want 0", len(rec.upserts)-upsertsBefore)
	}

	if len(rec.upserts) == 0 {
		t.Fatal("expected at least one group affinity upsert")
	}
	for i, aff := range rec.upserts {
		if aff.Model == "coder-group" && aff.ResolvedModel == "" {
			t.Fatalf("upsert #%d for the group has an empty ResolvedModel (affinity leak): %#v", i, aff)
		}
	}
}
