// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"maps"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"sync"
	"testing"
	"time"
)

// threeMappingFixture is a server_agent application with three mappings, the
// smallest fixture that can hold one forbid pair, one allow pair and a pair with
// no row at all.
type threeMappingFixture struct {
	svc       *Service
	routes    *routing.MemoryStore
	serverID  string
	appID     string
	ids       [3]string // mapping ids in ascending (canonical) order
	notifyLog func() []string
}

func newThreeMappingFixture(t *testing.T) threeMappingFixture {
	t.Helper()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routes := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routes, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routes, server.ID, now)
	ids := [3]string{
		seedMapping(t, routes, app.ID, "m1", now).ID,
		seedMapping(t, routes, app.ID, "m2", now).ID,
		seedMapping(t, routes, app.ID, "m3", now).ID,
	}
	// Mapping ids are random: sort so ids[0] < ids[1] < ids[2] and a test can
	// name canonical pairs without re-sorting.
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return threeMappingFixture{
		svc: svc, routes: routes, serverID: server.ID, appID: app.ID, ids: ids,
		notifyLog: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), calls...)
		},
	}
}

// TestSetCoResidencyRoundTripsModeAndPerPairVerdict is the Task 2.3 headline: a
// whole-set PUT carrying mode=opt_out and a FORBID pair stores both, and the
// read path returns the mode and each pair's verdict intact -- a forbid row is
// not collapsed into "allowed".
func TestSetCoResidencyRoundTripsModeAndPerPairVerdict(t *testing.T) {
	f := newThreeMappingFixture(t)
	ctx := context.Background()

	put, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{
		Mode: string(routing.CoResidencyModeOptOut),
		Pairs: []CoResidencyPairDTO{
			// Submitted reversed on purpose: canonicalisation still applies.
			{A: f.ids[1], B: f.ids[0], Verdict: routing.CoResidencyVerdictForbid},
			{A: f.ids[1], B: f.ids[2], Verdict: routing.CoResidencyVerdictAllow},
		},
	})
	if err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	want := CoResidencyDTO{
		Mode: "opt_out",
		Pairs: []CoResidencyPairDTO{
			{A: f.ids[0], B: f.ids[1], Verdict: "forbid"},
			{A: f.ids[1], B: f.ids[2], Verdict: "allow"},
		},
	}
	assertCoResidencyDTO(t, "PUT response", put, want)

	got, err := f.svc.GetCoResidency(ctx, ownerToken(), f.appID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	assertCoResidencyDTO(t, "GET", got, want)

	// And at the store: the verdicts and the mode really are persisted, not
	// merely echoed.
	mode, err := f.routes.GetCoResidencyMode(ctx, f.appID)
	if err != nil || mode != routing.CoResidencyModeOptOut {
		t.Fatalf("stored mode = %q, %v; want opt_out", mode, err)
	}
	rules, err := f.routes.CoResidencyRulesByApplication(ctx, f.appID)
	if err != nil {
		t.Fatalf("CoResidencyRulesByApplication: %v", err)
	}
	if len(rules) != 2 || rules[0].Verdict != routing.CoResidencyVerdictForbid || rules[1].Verdict != routing.CoResidencyVerdictAllow {
		t.Fatalf("stored rules = %#v, want [forbid, allow]", rules)
	}
}

// TestGetCoResidencyDefaultsToOptOut: a fresh application reads back the store's
// default mode with an empty, non-nil pair list.
func TestGetCoResidencyDefaultsToOptOut(t *testing.T) {
	f := newThreeMappingFixture(t)
	got, err := f.svc.GetCoResidency(context.Background(), ownerToken(), f.appID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if got.Mode != "opt_out" {
		t.Fatalf("mode = %q, want opt_out (the default for a new application)", got.Mode)
	}
	if got.Pairs == nil || len(got.Pairs) != 0 {
		t.Fatalf("pairs = %#v, want non-nil empty", got.Pairs)
	}
}

// TestSetCoResidencyRejectsUnknownModeAndVerdict: both are rejected BEFORE any
// store call, so a bad PUT leaves the previously stored mode and set untouched.
func TestSetCoResidencyRejectsUnknownModeAndVerdict(t *testing.T) {
	f := newThreeMappingFixture(t)
	ctx := context.Background()
	seed := SetCoResidencyRequest{
		Mode:  "opt_in",
		Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[1], Verdict: "allow"}},
	}
	if _, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, seed); err != nil {
		t.Fatalf("seed SetCoResidency: %v", err)
	}
	before := len(f.notifyLog())

	cases := []struct {
		name    string
		req     SetCoResidencyRequest
		wantErr error
	}{
		{
			name:    "unknown mode",
			req:     SetCoResidencyRequest{Mode: "sometimes", Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[2], Verdict: "forbid"}}},
			wantErr: ErrCoResidencyModeInvalid,
		},
		{
			name:    "unknown verdict",
			req:     SetCoResidencyRequest{Mode: "opt_out", Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[2], Verdict: "maybe"}}},
			wantErr: ErrCoResidencyPairInvalid,
		},
		{
			// A pair-level problem next to an otherwise valid mode change must
			// still reject the whole PUT without applying the mode.
			name:    "foreign mapping with a valid mode change",
			req:     SetCoResidencyRequest{Mode: "opt_out", Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: "map_nope", Verdict: "forbid"}}},
			wantErr: ErrCoResidencyPairInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			got, err := f.svc.GetCoResidency(ctx, ownerToken(), f.appID)
			if err != nil {
				t.Fatalf("GetCoResidency: %v", err)
			}
			assertCoResidencyDTO(t, "state after a rejected PUT", got, CoResidencyDTO{
				Mode:  "opt_in",
				Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[1], Verdict: "allow"}},
			})
		})
	}
	if n := len(f.notifyLog()); n != before {
		t.Fatalf("a rejected PUT notified the agent %d time(s); it changed nothing", n-before)
	}
}

// TestSetCoResidencyOmittedModeKeepsStoredModeAndEmptyVerdictIsAllow pins the
// two lenient defaults: a PUT that names no mode leaves the stored mode alone
// (never silently flips it), and a pair with no verdict is an allow -- what a
// pair meant before verdicts existed, and what the store normalises it to.
func TestSetCoResidencyOmittedModeKeepsStoredModeAndEmptyVerdictIsAllow(t *testing.T) {
	f := newThreeMappingFixture(t)
	ctx := context.Background()
	if _, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{Mode: "opt_in"}); err != nil {
		t.Fatalf("SetCoResidency opt_in: %v", err)
	}
	got, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{
		Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[1]}},
	})
	if err != nil {
		t.Fatalf("SetCoResidency (no mode): %v", err)
	}
	assertCoResidencyDTO(t, "PUT without a mode", got, CoResidencyDTO{
		Mode:  "opt_in",
		Pairs: []CoResidencyPairDTO{{A: f.ids[0], B: f.ids[1], Verdict: "allow"}},
	})
}

// TestSetCoResidencyModeSwitchKeepsVerdicts: re-PUTting the same pairs with the
// other mode changes only the mode -- the explicit verdicts survive the switch.
func TestSetCoResidencyModeSwitchKeepsVerdicts(t *testing.T) {
	f := newThreeMappingFixture(t)
	ctx := context.Background()
	pairs := []CoResidencyPairDTO{
		{A: f.ids[0], B: f.ids[1], Verdict: "forbid"},
		{A: f.ids[1], B: f.ids[2], Verdict: "allow"},
	}
	for _, mode := range []string{"opt_out", "opt_in", "opt_out"} {
		got, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{Mode: mode, Pairs: pairs})
		if err != nil {
			t.Fatalf("SetCoResidency %s: %v", mode, err)
		}
		assertCoResidencyDTO(t, "mode "+mode, got, CoResidencyDTO{Mode: mode, Pairs: pairs})
	}
}

// TestSetCoResidencyModeChangeNotifiesAgent: the mode is an input to the
// effective list the agent receives, and SetCoResidencyMode does not bump
// updated_at, so the portal write must notify the agent itself -- even when no
// pair changed.
func TestSetCoResidencyModeChangeNotifiesAgent(t *testing.T) {
	f := newThreeMappingFixture(t)
	ctx := context.Background()
	if _, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{Mode: "opt_in"}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	got := f.notifyLog()
	if len(got) != 1 || got[0] != f.serverID {
		t.Fatalf("notifications = %#v, want exactly [%q]", got, f.serverID)
	}
}

// failCoResidencyRulesStore fails SetCoResidencyRules on an otherwise-real store.
type failCoResidencyRulesStore struct {
	routing.Store
	err error
}

func (s *failCoResidencyRulesStore) SetCoResidencyRules(context.Context, string, []routing.CoResidencyRule) error {
	return s.err
}

// TestSetCoResidencyNotifiesWhenOnlyTheModeWasWritten: the mode and the pairs are
// two store calls, not one transaction. When the second fails after the first
// landed, the stored mode (hence the effective list) HAS changed, so the agent
// must still be told -- otherwise it would run on a stale list until some
// unrelated write.
func TestSetCoResidencyNotifiesWhenOnlyTheModeWasWritten(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	routes := routing.NewMemoryStore()
	boom := errors.New("db down")
	var mu sync.Mutex
	var calls []string
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: &failCoResidencyRulesStore{Store: routes, err: boom},
		Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routes, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{Mode: "opt_in"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the rules-write failure", err)
	}
	if mode, _ := routes.GetCoResidencyMode(ctx, app.ID); mode != routing.CoResidencyModeOptIn {
		t.Fatalf("stored mode = %q, want opt_in (the first write landed)", mode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != server.ID {
		t.Fatalf("notifications = %#v, want exactly [%q]: the mode write changed the effective list", calls, server.ID)
	}
}

func assertCoResidencyDTO(t *testing.T, label string, got, want CoResidencyDTO) {
	t.Helper()
	if got.Mode != want.Mode {
		t.Fatalf("%s: mode = %q, want %q", label, got.Mode, want.Mode)
	}
	if got.Pairs == nil {
		t.Fatalf("%s: pairs is nil, want non-nil", label)
	}
	if len(got.Pairs) != len(want.Pairs) {
		t.Fatalf("%s: pairs = %#v, want %#v", label, got.Pairs, want.Pairs)
	}
	for i := range want.Pairs {
		if got.Pairs[i] != want.Pairs[i] {
			t.Fatalf("%s: pairs[%d] = %#v, want %#v", label, i, got.Pairs[i], want.Pairs[i])
		}
	}
}

// --- Task 2.4: the effective allowed-pair list the agent receives -------------

// coRule builds a stored explicit pair as the store returns it.
func coRule(a, b, verdict string) routing.CoResidencyRule {
	return routing.CoResidencyRule{ApplicationID: "app", MappingAID: a, MappingBID: b, Verdict: verdict}
}

// specsFor maps mapping ids m1..mN to spec ids s1..sN, the shape AgentRuntimeConfig
// passes in: only ENABLED specs are in it.
func specsFor(ids ...string) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = "s" + id[1:]
	}
	return out
}

func TestEffectiveAllowedPairs(t *testing.T) {
	const allow, forbid = routing.CoResidencyVerdictAllow, routing.CoResidencyVerdictForbid
	cases := []struct {
		name  string
		mode  routing.CoResidencyMode
		specs map[string]string
		rules []routing.CoResidencyRule
		want  [][2]string
	}{
		{
			// The pre-0.15.0-agent guarantee: opt-out is ENUMERATED, so an agent
			// that only reads the plain pair list still co-locates by default.
			name:  "opt_out over four specs minus one forbid is five of six pairs",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1", "m2", "m3", "m4"),
			rules: []routing.CoResidencyRule{coRule("m2", "m3", forbid)},
			want:  [][2]string{{"s1", "s2"}, {"s1", "s3"}, {"s1", "s4"}, {"s2", "s4"}, {"s3", "s4"}},
		},
		{
			name:  "opt_out with no rules allows every pair",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1", "m2", "m3"),
			want:  [][2]string{{"s1", "s2"}, {"s1", "s3"}, {"s2", "s3"}},
		},
		{
			name:  "opt_out: an explicit allow is the default restated, listed once",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1", "m2", "m3"),
			rules: []routing.CoResidencyRule{coRule("m1", "m2", allow), coRule("m2", "m3", forbid)},
			want:  [][2]string{{"s1", "s2"}, {"s1", "s3"}},
		},
		{
			name:  "opt_in is only the explicit allows",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2", "m3", "m4"),
			rules: []routing.CoResidencyRule{coRule("m1", "m2", allow), coRule("m2", "m4", allow)},
			want:  [][2]string{{"s1", "s2"}, {"s2", "s4"}},
		},
		{
			name:  "opt_in: a forbid row adds nothing",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2", "m3", "m4"),
			rules: []routing.CoResidencyRule{coRule("m1", "m2", allow), coRule("m3", "m4", forbid)},
			want:  [][2]string{{"s1", "s2"}},
		},
		{
			name:  "opt_in with no rules allows nothing",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2", "m3"),
			want:  [][2]string{},
		},
		{
			// A mode the store should never return must not quietly become the
			// permissive default: default-allow is the direction that can OOM a GPU.
			name:  "an unknown mode resolves like opt_in",
			mode:  routing.CoResidencyMode("bogus"),
			specs: specsFor("m1", "m2", "m3"),
			rules: []routing.CoResidencyRule{coRule("m1", "m3", allow)},
			want:  [][2]string{{"s1", "s3"}},
		},
		{
			name:  "an unknown verdict is not an allow, under either mode",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2"),
			rules: []routing.CoResidencyRule{coRule("m1", "m2", "maybe")},
			want:  [][2]string{},
		},
		{
			// A spec that is disabled or has no mapping is absent from specs: it
			// never appears, neither as an opt-out default nor via an explicit allow.
			name:  "a pair touching a spec that is not enabled is dropped (opt_out)",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1", "m2", "m3"),
			rules: []routing.CoResidencyRule{coRule("m3", "m9", allow)},
			want:  [][2]string{{"s1", "s2"}, {"s1", "s3"}, {"s2", "s3"}},
		},
		{
			name:  "a pair touching a spec that is not enabled is dropped (opt_in)",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2"),
			rules: []routing.CoResidencyRule{coRule("m1", "m2", allow), coRule("m2", "m9", allow)},
			want:  [][2]string{{"s1", "s2"}},
		},
		{
			// Rows are stored canonical, but the computation does not rely on it.
			name:  "a rule stored in reverse order still matches its pair",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1", "m2"),
			rules: []routing.CoResidencyRule{coRule("m2", "m1", forbid)},
			want:  [][2]string{},
		},
		{
			name:  "a self pair is never produced",
			mode:  routing.CoResidencyModeOptIn,
			specs: specsFor("m1", "m2"),
			rules: []routing.CoResidencyRule{coRule("m1", "m1", allow)},
			want:  [][2]string{},
		},
		{
			name:  "a single enabled spec has no pair",
			mode:  routing.CoResidencyModeOptOut,
			specs: specsFor("m1"),
			want:  [][2]string{},
		},
		{
			name:  "no enabled specs",
			mode:  routing.CoResidencyModeOptOut,
			specs: map[string]string{},
			want:  [][2]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveAllowedPairs(tc.mode, tc.specs, tc.rules)
			if got == nil {
				t.Fatalf("got nil, want a non-nil list (it is a JSON array on the wire)")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("pairs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEffectiveAllowedPairsIsDeterministicAndKeepsTheLegacyOrientation: the list is
// part of the config document whose ETag the agent compares, so it must not
// depend on map iteration order; and each pair keeps the orientation the old
// per-row translation used -- the spec of the lower mapping id first -- so an
// existing opt_in deployment's document is byte-identical after the upgrade.
func TestEffectiveAllowedPairsIsDeterministicAndKeepsTheLegacyOrientation(t *testing.T) {
	// Spec ids deliberately sort the OPPOSITE way to their mapping ids.
	specs := map[string]string{"m1": "sz", "m2": "sy", "m3": "sx", "m4": "sa"}
	rules := []routing.CoResidencyRule{coRule("m1", "m2", "allow"), coRule("m2", "m3", "allow"), coRule("m3", "m4", "allow")}
	want := [][2]string{{"sz", "sy"}, {"sy", "sx"}, {"sx", "sa"}}
	for range 50 {
		if got := effectiveAllowedPairs(routing.CoResidencyModeOptIn, specs, rules); !slices.Equal(got, want) {
			t.Fatalf("opt_in pairs = %v, want %v", got, want)
		}
	}
	all := effectiveAllowedPairs(routing.CoResidencyModeOptOut, specs, nil)
	wantAll := [][2]string{{"sz", "sy"}, {"sz", "sx"}, {"sz", "sa"}, {"sy", "sx"}, {"sy", "sa"}, {"sx", "sa"}}
	for range 50 {
		if got := effectiveAllowedPairs(routing.CoResidencyModeOptOut, specs, nil); !slices.Equal(got, wantAll) {
			t.Fatalf("opt_out pairs = %v, want %v", got, wantAll)
		}
	}
	if !slices.Equal(all, wantAll) {
		t.Fatalf("opt_out pairs = %v, want %v", all, wantAll)
	}
}

// fourSpecFixture is the threeMappingFixture's bigger sibling for the agent-config
// build: four mappings, each with an ENABLED runtime spec, so AgentRuntimeConfig
// has four specs to pair up.
type fourSpecFixture struct {
	svc      *Service
	routes   *routing.MemoryStore
	serverID string
	appID    string
	mapIDs   [4]string // ascending
	specIDs  map[string]string
}

func newFourSpecFixture(t *testing.T) fourSpecFixture {
	t.Helper()
	f3 := newThreeMappingFixture(t) // one app, owner/admin wiring, notify hook
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fourth := seedMapping(t, f3.routes, f3.appID, "m4", now).ID
	ids := [4]string{f3.ids[0], f3.ids[1], f3.ids[2], fourth}
	slices.Sort(ids[:])
	specIDs := make(map[string]string, len(ids))
	for _, mappingID := range ids {
		spec, err := f3.svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Enabled: true, Binary: "/usr/bin/llama-server",
			GPUs: []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 1000}},
		})
		if err != nil {
			t.Fatalf("PutRuntimeSpec %s: %v", mappingID, err)
		}
		specIDs[mappingID] = spec.ID
	}
	return fourSpecFixture{svc: f3.svc, routes: f3.routes, serverID: f3.serverID, appID: f3.appID, mapIDs: ids, specIDs: specIDs}
}

// pairSet canonicalises the wire pairs to a set of unordered spec-id pairs.
func pairSet(pairs [][2]string) map[[2]string]bool {
	out := make(map[[2]string]bool, len(pairs))
	for _, p := range pairs {
		if p[0] > p[1] {
			p[0], p[1] = p[1], p[0]
		}
		out[p] = true
	}
	return out
}

func (f fourSpecFixture) specPair(a, b int) [2]string {
	p := [2]string{f.specIDs[f.mapIDs[a]], f.specIDs[f.mapIDs[b]]}
	if p[0] > p[1] {
		p[0], p[1] = p[1], p[0]
	}
	return p
}

// TestAgentRuntimeConfigCoresidentIsTheEffectiveListUnderEitherMode drives the real
// service: a stored forbid and a stored allow, the mode flipped back and forth
// through the store's mode-only write. The stored rows never change; only the
// list computed from them does, in SPEC id space, on the existing
// `coresident` wire field.
func TestAgentRuntimeConfigCoresidentIsTheEffectiveListUnderEitherMode(t *testing.T) {
	f := newFourSpecFixture(t)
	ctx := context.Background()

	// forbid 0-1, allow 2-3; every other pair has no row.
	if _, err := f.svc.SetCoResidency(ctx, ownerToken(), f.appID, SetCoResidencyRequest{
		Mode: "opt_out",
		Pairs: []CoResidencyPairDTO{
			{A: f.mapIDs[0], B: f.mapIDs[1], Verdict: "forbid"},
			{A: f.mapIDs[2], B: f.mapIDs[3], Verdict: "allow"},
		},
	}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	storedRules := func() []routing.CoResidencyRule {
		rules, err := f.routes.CoResidencyRulesByApplication(ctx, f.appID)
		if err != nil {
			t.Fatalf("CoResidencyRulesByApplication: %v", err)
		}
		return rules
	}
	wantRules := storedRules()
	config := func() AgentRuntimeConfigDTO {
		dto, err := f.svc.AgentRuntimeConfig(ctx, f.serverID)
		if err != nil {
			t.Fatalf("AgentRuntimeConfig: %v", err)
		}
		return dto
	}

	// opt_out: all six pairs minus the one forbid.
	optOut := config()
	wantOptOut := map[[2]string]bool{
		f.specPair(0, 2): true, f.specPair(0, 3): true,
		f.specPair(1, 2): true, f.specPair(1, 3): true, f.specPair(2, 3): true,
	}
	if len(optOut.Coresident) != 5 || !maps.Equal(pairSet(optOut.Coresident), wantOptOut) {
		t.Fatalf("opt_out coresident = %v, want the five pairs %v (all six minus the 0-1 forbid)", optOut.Coresident, wantOptOut)
	}
	for _, p := range optOut.Coresident {
		for _, id := range p {
			if _, isMapping := f.specIDs[id]; isMapping {
				t.Fatalf("coresident leaked a MAPPING id: %v", p)
			}
		}
	}

	// Flip to opt_in through the store's mode-only write: the rows are untouched.
	if err := f.routes.SetCoResidencyMode(ctx, f.appID, routing.CoResidencyModeOptIn); err != nil {
		t.Fatalf("SetCoResidencyMode opt_in: %v", err)
	}
	if got := storedRules(); !slices.Equal(got, wantRules) {
		t.Fatalf("stored rules changed by a mode switch:\n got %#v\nwant %#v", got, wantRules)
	}
	optIn := config()
	if len(optIn.Coresident) != 1 || !maps.Equal(pairSet(optIn.Coresident), map[[2]string]bool{f.specPair(2, 3): true}) {
		t.Fatalf("opt_in coresident = %v, want only the explicit allow [%v]", optIn.Coresident, f.specPair(2, 3))
	}
	if optIn.ETag == optOut.ETag {
		t.Fatalf("the two modes produce different lists, so the document etag must differ")
	}

	// And back: nothing was lost, the document is byte-for-byte the first one.
	if err := f.routes.SetCoResidencyMode(ctx, f.appID, routing.CoResidencyModeOptOut); err != nil {
		t.Fatalf("SetCoResidencyMode opt_out: %v", err)
	}
	if got := storedRules(); !slices.Equal(got, wantRules) {
		t.Fatalf("stored rules changed by switching back:\n got %#v\nwant %#v", got, wantRules)
	}
	if again := config(); again.ETag != optOut.ETag || !slices.Equal(again.Coresident, optOut.Coresident) {
		t.Fatalf("opt_out -> opt_in -> opt_out did not restore the document:\n got %v\nwant %v", again.Coresident, optOut.Coresident)
	}
}

// TestAgentRuntimeConfigNewApplicationDefaultsToCoLocating: a server_agent
// application that nobody has touched is opt_out, so its enabled specs may all be
// co-resident -- enumerated for the agent, with no rule rows at all.
func TestAgentRuntimeConfigNewApplicationDefaultsToCoLocating(t *testing.T) {
	f := newFourSpecFixture(t)
	dto, err := f.svc.AgentRuntimeConfig(context.Background(), f.serverID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(dto.Coresident) != 6 {
		t.Fatalf("coresident = %v, want all six pairs of four specs under the default opt_out", dto.Coresident)
	}
}

// failCoResidencyModeStore fails GetCoResidencyMode on an otherwise-real store.
type failCoResidencyModeStore struct {
	routing.Store
	err error
}

func (s *failCoResidencyModeStore) GetCoResidencyMode(context.Context, string) (routing.CoResidencyMode, error) {
	return "", s.err
}

// TestAgentRuntimeConfigPropagatesAModeReadFailure: like every other read in the
// build, a failed mode read is an error, never a guessed mode. A guess would
// publish either an empty list (stopping every co-resident model) or an
// everything-allowed one; an error keeps the agent on its last known-good config.
func TestAgentRuntimeConfigPropagatesAModeReadFailure(t *testing.T) {
	f := newFourSpecFixture(t)
	boom := errors.New("db down")
	svc := newServerTestServiceWithRoutes(t, time.Now(), &failCoResidencyModeStore{Store: f.routes, err: boom})
	if _, err := svc.AgentRuntimeConfig(context.Background(), f.serverID); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the mode-read failure", err)
	}
}
