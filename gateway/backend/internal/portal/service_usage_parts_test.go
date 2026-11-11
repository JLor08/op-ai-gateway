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
	rec.Record(usage.Event{ID: "ev_cluster", UserID: "usr_1", Model: "big", Host: "srv_head", EnergyWh: 3500, EnergySource: "measured", CreatedAt: now})
	rec.Record(usage.Event{ID: "ev_plain", UserID: "usr_1", Model: "big", Host: "srv_head", EnergyWh: 1000, EnergySource: "measured", CreatedAt: now})
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

	for _, dim := range []string{"model", "server", "user"} {
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
