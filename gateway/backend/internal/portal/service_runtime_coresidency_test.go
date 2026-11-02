// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
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
