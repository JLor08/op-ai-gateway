// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// seedCoResidencyApp creates one server_agent application with three mappings
// on its own server, so a test can write pairs between any two of them.
func seedCoResidencyApp(ctx context.Context, t *testing.T, s routing.Store, suffix string, now time.Time) (appID string, mappings [3]string) {
	t.Helper()
	serverID, appID := "srv_"+suffix, "app_"+suffix
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: serverID, Name: serverID, Domain: suffix + ".example.test", Provider: routing.ProviderMock,
		Endpoint: "mock://" + suffix, Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{
		ID: appID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	for i, id := range []string{"map_" + suffix + "_a", "map_" + suffix + "_b", "map_" + suffix + "_c"} {
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: id, ApplicationID: appID, GatewayModelName: id + "-model", AppModelName: id + "-up",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping %s: %v", id, err)
		}
		mappings[i] = id
	}
	return appID, mappings
}

// TestRoutingStoreCoResidencyVerdicts proves all three drivers persist the
// per-pair verdict: an explicit 'allow' and an explicit 'forbid' round-trip
// unchanged and in the documented order, a rule that predates the verdict (an
// empty Verdict) is stored as 'allow' -- the pre-rework meaning of a row --
// an unknown verdict is rejected without touching the stored set, and the
// replace stays a full replace.
func TestRoutingStoreCoResidencyVerdicts(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		appID, m := seedCoResidencyApp(ctx, t, s, "cv", now)

		// Submitted in reversed order so the ordered read is exercised too.
		if err := s.SetCoResidencyRules(ctx, appID, []routing.CoResidencyRule{
			{ApplicationID: appID, MappingAID: m[1], MappingBID: m[2], Verdict: routing.CoResidencyVerdictForbid, CreatedAt: now},
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[2], Verdict: routing.CoResidencyVerdictAllow, CreatedAt: now},
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[1], Verdict: routing.CoResidencyVerdictForbid, CreatedAt: now},
		}); err != nil {
			t.Fatalf("set allow + forbid pairs: %v", err)
		}
		rules, err := s.CoResidencyRulesByApplication(ctx, appID)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		want := []struct{ a, b, verdict string }{
			{m[0], m[1], routing.CoResidencyVerdictForbid},
			{m[0], m[2], routing.CoResidencyVerdictAllow},
			{m[1], m[2], routing.CoResidencyVerdictForbid},
		}
		if len(rules) != len(want) {
			t.Fatalf("read back %d rules, want %d: %+v", len(rules), len(want), rules)
		}
		for i, w := range want {
			if rules[i].MappingAID != w.a || rules[i].MappingBID != w.b || rules[i].Verdict != w.verdict {
				t.Fatalf("rule %d = (%s, %s, %q), want (%s, %s, %q); full read: %+v",
					i, rules[i].MappingAID, rules[i].MappingBID, rules[i].Verdict, w.a, w.b, w.verdict, rules)
			}
		}

		// A rule with no verdict is an allow: that is what a row has always
		// meant, so a caller that predates the verdict keeps its semantics.
		if err := s.SetCoResidencyRules(ctx, appID, []routing.CoResidencyRule{
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[1], CreatedAt: now},
		}); err != nil {
			t.Fatalf("set legacy rule without verdict: %v", err)
		}
		if rules, _ = s.CoResidencyRulesByApplication(ctx, appID); len(rules) != 1 || rules[0].Verdict != routing.CoResidencyVerdictAllow {
			t.Fatalf("a rule without a verdict must be stored as allow, got %+v", rules)
		}

		// An unknown verdict is rejected, and the rejected set must not
		// partially apply: the previous one-rule set stays.
		if err := s.SetCoResidencyRules(ctx, appID, []routing.CoResidencyRule{
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[2], Verdict: routing.CoResidencyVerdictForbid, CreatedAt: now},
			{ApplicationID: appID, MappingAID: m[1], MappingBID: m[2], Verdict: "maybe", CreatedAt: now},
		}); err == nil {
			t.Fatal("an unknown verdict must be rejected")
		}
		if rules, _ = s.CoResidencyRulesByApplication(ctx, appID); len(rules) != 1 || rules[0].MappingBID != m[1] {
			t.Fatalf("a rejected set must leave the previous set untouched, got %+v", rules)
		}
		// The vocabulary is exact: no case-folding, no whitespace tolerance.
		for _, bad := range []string{"Allow", "FORBID", " allow", "deny"} {
			if err := s.SetCoResidencyRules(ctx, appID, []routing.CoResidencyRule{
				{ApplicationID: appID, MappingAID: m[0], MappingBID: m[1], Verdict: bad, CreatedAt: now},
			}); err == nil {
				t.Fatalf("verdict %q must be rejected", bad)
			}
		}

		// Full replace still clears.
		if err := s.SetCoResidencyRules(ctx, appID, nil); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if rules, _ = s.CoResidencyRulesByApplication(ctx, appID); rules == nil || len(rules) != 0 {
			t.Fatalf("cleared read must be non-nil, empty, got %#v", rules)
		}
	})
}

// TestRoutingStoreCoResidencyMode proves all three drivers agree on the
// per-application co-residency mode: a new application is opt_out, the mode
// round-trips both ways, an unknown mode is rejected without changing the
// stored one, an unknown application is ErrNotFound on both accessors, the
// mode is independent of the explicit pair rows (switching it is lossless),
// it survives an ordinary application update, and it does not outlive its
// application.
func TestRoutingStoreCoResidencyMode(t *testing.T) {
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		appID, m := seedCoResidencyApp(ctx, t, s, "cm", now)

		mode := func(t *testing.T) routing.CoResidencyMode {
			t.Helper()
			got, err := s.GetCoResidencyMode(ctx, appID)
			if err != nil {
				t.Fatalf("GetCoResidencyMode: %v", err)
			}
			return got
		}

		// New applications are opt_out, the new standard.
		if got := mode(t); got != routing.CoResidencyModeOptOut {
			t.Fatalf("a new application's mode = %q, want opt_out", got)
		}

		if err := s.SetCoResidencyMode(ctx, appID, routing.CoResidencyModeOptIn); err != nil {
			t.Fatalf("set opt_in: %v", err)
		}
		if got := mode(t); got != routing.CoResidencyModeOptIn {
			t.Fatalf("mode after setting opt_in = %q", got)
		}

		// An unknown mode is rejected and leaves the stored one alone.
		for _, bad := range []routing.CoResidencyMode{"", "opt-in", "OPT_OUT", "default"} {
			if err := s.SetCoResidencyMode(ctx, appID, bad); err == nil {
				t.Fatalf("mode %q must be rejected", bad)
			}
		}
		if got := mode(t); got != routing.CoResidencyModeOptIn {
			t.Fatalf("a rejected mode changed the stored one: %q, want opt_in", got)
		}

		// Unknown application: ErrNotFound on both accessors.
		if _, err := s.GetCoResidencyMode(ctx, "app_missing"); err != ErrNotFound {
			t.Fatalf("GetCoResidencyMode on an unknown application: want ErrNotFound, got %v", err)
		}
		if err := s.SetCoResidencyMode(ctx, "app_missing", routing.CoResidencyModeOptOut); err != ErrNotFound {
			t.Fatalf("SetCoResidencyMode on an unknown application: want ErrNotFound, got %v", err)
		}

		// Switching the mode is lossless: the explicit rows are untouched, in
		// both directions.
		rules := []routing.CoResidencyRule{
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[1], Verdict: routing.CoResidencyVerdictAllow, CreatedAt: now},
			{ApplicationID: appID, MappingAID: m[0], MappingBID: m[2], Verdict: routing.CoResidencyVerdictForbid, CreatedAt: now},
		}
		if err := s.SetCoResidencyRules(ctx, appID, rules); err != nil {
			t.Fatalf("set rules: %v", err)
		}
		for _, to := range []routing.CoResidencyMode{routing.CoResidencyModeOptOut, routing.CoResidencyModeOptIn} {
			if err := s.SetCoResidencyMode(ctx, appID, to); err != nil {
				t.Fatalf("switch to %s: %v", to, err)
			}
			got, err := s.CoResidencyRulesByApplication(ctx, appID)
			if err != nil || len(got) != 2 ||
				got[0].Verdict != routing.CoResidencyVerdictAllow || got[1].Verdict != routing.CoResidencyVerdictForbid {
				t.Fatalf("switching to %s changed the explicit rows: err=%v rows=%+v", to, err, got)
			}
		}
		// ... and replacing the rows does not move the mode.
		if err := s.SetCoResidencyRules(ctx, appID, nil); err != nil {
			t.Fatalf("clear rules: %v", err)
		}
		if got := mode(t); got != routing.CoResidencyModeOptIn {
			t.Fatalf("replacing the rules changed the mode: %q, want opt_in", got)
		}

		// An ordinary application update (the portal's PATCH writes the whole
		// row) must not clobber the mode, which is not a field of it.
		app, err := s.ApplicationByID(ctx, appID)
		if err != nil {
			t.Fatalf("ApplicationByID: %v", err)
		}
		app.Priority++
		app.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, app); err != nil {
			t.Fatalf("UpdateApplication: %v", err)
		}
		if got := mode(t); got != routing.CoResidencyModeOptIn {
			t.Fatalf("an application update changed the mode: %q, want opt_in", got)
		}

		// The mode dies with its application: a re-created application with
		// the same id is a new application and starts opt_out, not with the
		// old one's opt_in.
		if err := s.DeleteApplication(ctx, appID); err != nil {
			t.Fatalf("DeleteApplication: %v", err)
		}
		if _, err := s.GetCoResidencyMode(ctx, appID); err != ErrNotFound {
			t.Fatalf("GetCoResidencyMode after the application was deleted: want ErrNotFound, got %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: appID, ServerID: "srv_cm", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("re-create application: %v", err)
		}
		if got := mode(t); got != routing.CoResidencyModeOptOut {
			t.Fatalf("a re-created application inherited the deleted one's mode: %q, want opt_out", got)
		}
	})
}
