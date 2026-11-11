// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// TestClusterCostUsesEachHostPrice pins that a cluster request's cost is the
// sum of its energy parts, each at its own host's price: the head part at the
// head server's price_per_kwh, each member part at that member server's own
// (else the system default). The row, the stats total and every group agree.
func TestClusterCostUsesEachHostPrice(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	for _, s := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_mem", Name: "Member", Domain: "mem.example.test", PricePerKwh: 0.10},
		{ID: "srv_mem2", Name: "Member 2", Domain: "mem2.example.test"}, // system default
	} {
		if err := routes.CreateAIServer(ctx, s); err != nil {
			t.Fatalf("CreateAIServer %s: %v", s.ID, err)
		}
	}
	settings := NewMemorySystemSettings()
	if err := settings.SetSystemSetting(ctx, "energy_default_price_per_kwh", "0.50", now); err != nil {
		t.Fatal(err)
	}

	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "ev_cluster", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 3500, EnergySource: "measured", CreatedAt: now})
	rec.Record(usage.Event{ID: "ev_plain", UserID: "usr_1", Model: "big", Host: "srv_head", ServerName: "Head", EnergyWh: 1000, EnergySource: "measured", CreatedAt: now})
	if err := rec.SetEnergyParts(ctx, "ev_cluster", []usage.EnergyPart{
		{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
		{ServerID: "srv_mem", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
		{ServerID: "srv_mem2", Role: usage.EventHostRoleMember, Wh: 500, Source: "measured"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, SystemSettings: settings, Clock: func() time.Time { return now }})
	user := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	// head 1 kWh × 0.30 + member 2 kWh × 0.10 + member 0.5 kWh × 0.50 (default).
	const clusterCost = 0.30 + 0.20 + 0.25
	const plainCost = 0.30

	page, err := svc.Usage(user, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Data {
		want := plainCost
		if row.ID == "ev_cluster" {
			want = clusterCost
		}
		if !approxEq(row.CostEUR, want) {
			t.Fatalf("%s cost = %v, want %v", row.ID, row.CostEUR, want)
		}
	}

	stats, err := svc.UsageStats(user, usage.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !approxEq(stats.Totals.TotalCostEUR, clusterCost+plainCost) || !approxEq(stats.Totals.TotalEnergyWh, 4500) {
		t.Fatalf("totals = %+v, want cost %v over 4500 Wh", stats.Totals, clusterCost+plainCost)
	}

	// Grouped by server, every host's share shows under that server, at its
	// price; a member row counts no requests.
	byServer, err := svc.UsageGroups(user, usage.Query{}, "server")
	if err != nil {
		t.Fatal(err)
	}
	wantServers := map[string]UsageGroupDTO{
		"Head":     {Count: 2, EnergyWh: 2000, CostEUR: 0.60},
		"Member":   {Count: 0, EnergyWh: 2000, CostEUR: 0.20},
		"Member 2": {Count: 0, EnergyWh: 500, CostEUR: 0.25},
	}
	if len(byServer) != len(wantServers) {
		t.Fatalf("groups by server = %+v, want %d rows", byServer, len(wantServers))
	}
	for _, g := range byServer {
		want, ok := wantServers[g.Key]
		if !ok || g.Count != want.Count || !approxEq(g.EnergyWh, want.EnergyWh) || !approxEq(g.CostEUR, want.CostEUR) {
			t.Fatalf("group %q = %+v, want %+v", g.Key, g, want)
		}
	}

	for _, dim := range []string{"model", "user"} {
		groups, err := svc.UsageGroups(user, usage.Query{}, dim)
		if err != nil {
			t.Fatalf("groups by %s: %v", dim, err)
		}
		if len(groups) != 1 {
			t.Fatalf("groups by %s = %+v, want one", dim, groups)
		}
		g := groups[0]
		if g.Count != 2 || !approxEq(g.EnergyWh, 4500) || !approxEq(g.CostEUR, clusterCost+plainCost) {
			t.Fatalf("group by %s = %+v, want 2 requests, 4500 Wh, cost %v", dim, g, clusterCost+plainCost)
		}
	}
}

// TestMemberServerFilterShowsAggregatedShare pins the member server's view: a
// usage aggregate filtered by a server includes the energy that server drew as
// a member of other servers' cluster requests, at its own price -- energy and
// cost only, no request, count or token of those requests -- while the
// request list stays the requests served by that server.
func TestMemberServerFilterShowsAggregatedShare(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	routes := routing.NewMemoryStore()
	for _, s := range []routing.AIServer{
		{ID: "srv_head", Name: "Head", Domain: "head.example.test", PricePerKwh: 0.30},
		{ID: "srv_mem", Name: "Member", Domain: "mem.example.test", PricePerKwh: 0.10},
	} {
		if err := routes.CreateAIServer(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	rec := usage.NewRecorder()
	// R: another user's cluster request, served by Head with a part on Member.
	rec.Record(usage.Event{ID: "ev_r", UserID: "usr_other", Model: "big", Host: "srv_head", ServerName: "Head", InputTokens: 40, OutputTokens: 60, TotalTokens: 100, EnergyWh: 3000, EnergySource: "measured", CreatedAt: now})
	if err := rec.SetEnergyParts(ctx, "ev_r", []usage.EnergyPart{
		{ServerID: "srv_head", Role: usage.EventHostRoleHead, Wh: 1000, Source: "measured"},
		{ServerID: "srv_mem", Role: usage.EventHostRoleMember, Wh: 2000, Source: "measured"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})
	admin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}

	for _, q := range []usage.Query{
		{ScopeAll: true, ServerExact: "Member", HasServerExact: true},
		{ScopeAll: true, Server: "mem"},
	} {
		stats, err := svc.UsageStats(admin, q)
		if err != nil {
			t.Fatal(err)
		}
		tot := stats.Totals
		if tot.TotalRequests != 0 || tot.InputTokens != 0 || tot.OutputTokens != 0 || !approxEq(tot.TotalEnergyWh, 2000) || !approxEq(tot.TotalCostEUR, 0.20) {
			t.Fatalf("stats filtered by %+v = %+v, want no request or token, 2000 Wh at 0.20", q, tot)
		}
		groups, err := svc.UsageGroups(admin, q, "server")
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Key != "Member" || groups[0].Count != 0 || groups[0].TotalTokens != 0 ||
			!approxEq(groups[0].EnergyWh, 2000) || !approxEq(groups[0].CostEUR, 0.20) {
			t.Fatalf("groups filtered by %+v = %+v, want Member's share only", q, groups)
		}
		page, err := svc.Usage(admin, usage.Query{ScopeAll: true, ServerExact: q.ServerExact, HasServerExact: q.HasServerExact, Server: q.Server, Page: 1, Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 0 {
			t.Fatalf("rows filtered by %+v = %+v, want none: R was not served by Member", q, page.Data)
		}
	}

	// Filtered by the head, the request counts in full, as before.
	stats, err := svc.UsageStats(admin, usage.Query{ScopeAll: true, ServerExact: "Head", HasServerExact: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.TotalRequests != 1 || !approxEq(stats.Totals.TotalEnergyWh, 3000) || !approxEq(stats.Totals.TotalCostEUR, 0.50) {
		t.Fatalf("stats filtered by Head = %+v, want R in full: 3000 Wh, 0.30 + 0.20", stats.Totals)
	}
}
