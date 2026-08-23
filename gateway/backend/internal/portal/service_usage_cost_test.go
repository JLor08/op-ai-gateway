// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"math"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// approxEq tolerates the float64 rounding dust a chain of divisions/
// multiplications/sums can accumulate (energy_wh/1000 * price, summed across
// several rows/servers).
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestServiceUsageSetsCostEURPerRow proves the P3 §8 cost derivation:
// cost_eur = (energy_wh/1000) * price(server), price(server) =
// ai_servers.price_per_kwh when > 0, else the system default
// energy_default_price_per_kwh — for three rows: a server with its own price
// set (server price wins), a server with NO price set (falls to the system
// default), and a row whose server no longer exists (a missing server also
// falls to the system default, no error).
func TestServiceUsageSetsCostEURPerRow(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_priced", Name: "Priced", Domain: "priced.example.test", PricePerKwh: 0.30}); err != nil {
		t.Fatalf("CreateAIServer priced: %v", err)
	}
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_unpriced", Name: "Unpriced", Domain: "unpriced.example.test"}); err != nil {
		t.Fatalf("CreateAIServer unpriced: %v", err)
	}
	// srv_missing is deliberately NEVER created.

	settings := NewMemorySystemSettings()
	if err := settings.SetSystemSetting(ctx, "energy_default_price_per_kwh", "0.20", now); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}

	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "row_priced", UserID: "usr_1", Host: "srv_priced", EnergyWh: 2000, CreatedAt: now})
	rec.Record(usage.Event{ID: "row_unpriced", UserID: "usr_1", Host: "srv_unpriced", EnergyWh: 1000, CreatedAt: now})
	rec.Record(usage.Event{ID: "row_missing", UserID: "usr_1", Host: "srv_missing", EnergyWh: 500, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, SystemSettings: settings, Clock: func() time.Time { return now }})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("page total = %d, want 3", page.Total)
	}
	byID := map[string]float64{}
	for _, row := range page.Data {
		byID[row.ID] = row.CostEUR
	}

	// srv_priced: 2000Wh/1000 * 0.30 = 0.60 -- the server's own price wins over
	// the 0.20 system default.
	if !approxEq(byID["row_priced"], 0.60) {
		t.Fatalf("row_priced CostEUR = %v, want 0.60", byID["row_priced"])
	}
	// srv_unpriced: price_per_kwh is 0 (unset) -> falls to the 0.20 default:
	// 1000Wh/1000 * 0.20 = 0.20.
	if !approxEq(byID["row_unpriced"], 0.20) {
		t.Fatalf("row_unpriced CostEUR = %v, want 0.20 (default fallback)", byID["row_unpriced"])
	}
	// srv_missing: AIServerByID errors (no such server) -> also falls to the
	// 0.20 default, with NO error surfaced: 500Wh/1000 * 0.20 = 0.10.
	if !approxEq(byID["row_missing"], 0.10) {
		t.Fatalf("row_missing CostEUR = %v, want 0.10 (missing server -> default, no error)", byID["row_missing"])
	}
}

// TestServiceUsageCostZeroWhenNoPriceConfigured proves that when NEITHER the
// server's own price NOR the system default is set, cost_eur is 0 regardless
// of how much energy was attributed to the row.
func TestServiceUsageCostZeroWhenNoPriceConfigured(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_free", Name: "Free", Domain: "free.example.test"}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// No SystemSettings dependency at all (nil) -- systemDefaultPricePerKwh must
	// degrade to 0, not panic or error.
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "row_free", UserID: "usr_1", Host: "srv_free", EnergyWh: 5000, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("page data = %d, want 1", len(page.Data))
	}
	if page.Data[0].CostEUR != 0 {
		t.Fatalf("CostEUR = %v, want 0 (no price configured anywhere)", page.Data[0].CostEUR)
	}
}

// TestServiceUsageStatsTotalsEnergyAndWeightedCost proves UsageStats returns a
// plain SUM(energy_wh) in TotalEnergyWh alongside a PER-SERVER-PRICE-WEIGHTED
// TotalCostEUR: two servers with DIFFERENT prices must each be costed at
// their own rate, not a single blended rate.
func TestServiceUsageStatsTotalsEnergyAndWeightedCost(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_cheap", Name: "Cheap", Domain: "cheap.example.test", PricePerKwh: 0.10}); err != nil {
		t.Fatalf("CreateAIServer cheap: %v", err)
	}
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_pricey", Name: "Pricey", Domain: "pricey.example.test", PricePerKwh: 0.50}); err != nil {
		t.Fatalf("CreateAIServer pricey: %v", err)
	}

	rec := usage.NewRecorder()
	// srv_cheap: 1000Wh (1kWh) @ 0.10 -> 0.10 EUR.
	rec.Record(usage.Event{ID: "row_cheap_1", UserID: "usr_1", Host: "srv_cheap", EnergyWh: 400, CreatedAt: now})
	rec.Record(usage.Event{ID: "row_cheap_2", UserID: "usr_1", Host: "srv_cheap", EnergyWh: 600, CreatedAt: now})
	// srv_pricey: 2000Wh (2kWh) @ 0.50 -> 1.00 EUR.
	rec.Record(usage.Event{ID: "row_pricey_1", UserID: "usr_1", Host: "srv_pricey", EnergyWh: 2000, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})

	stats, err := svc.UsageStats(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{})
	if err != nil {
		t.Fatalf("UsageStats returned err: %v", err)
	}
	if stats.Totals.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", stats.Totals.TotalRequests)
	}
	// Plain SUM, no price weighting: 400+600+2000 = 3000 Wh.
	if !approxEq(stats.Totals.TotalEnergyWh, 3000) {
		t.Fatalf("TotalEnergyWh = %v, want 3000", stats.Totals.TotalEnergyWh)
	}
	// Weighted cost: (1000/1000*0.10) + (2000/1000*0.50) = 0.10 + 1.00 = 1.10.
	// A naively-blended single rate (e.g. total_wh/1000 * some average price)
	// would NOT reproduce this value, proving the per-server weighting.
	if !approxEq(stats.Totals.TotalCostEUR, 1.10) {
		t.Fatalf("TotalCostEUR = %v, want 1.10 (per-server weighted)", stats.Totals.TotalCostEUR)
	}
}

// TestServiceUsageStatsScopedToOwnUser proves UsageStats' weighted-cost
// computation respects the same own/all scope as the base Stats() call (it
// reuses EnergyByServer(ctx, q) with the SAME q applyUsageScope already
// pinned) -- another user's energy/cost must not leak in.
func TestServiceUsageStatsScopedToOwnUser(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	routes := routing.NewMemoryStore()
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_mine", Name: "Mine", Domain: "mine.example.test", PricePerKwh: 0.10}); err != nil {
		t.Fatalf("CreateAIServer mine: %v", err)
	}
	if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv_theirs", Name: "Theirs", Domain: "theirs.example.test", PricePerKwh: 9.99}); err != nil {
		t.Fatalf("CreateAIServer theirs: %v", err)
	}

	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "row_mine", UserID: "usr_1", Host: "srv_mine", EnergyWh: 1000, CreatedAt: now})
	rec.Record(usage.Event{ID: "row_theirs", UserID: "usr_2", Host: "srv_theirs", EnergyWh: 1000, CreatedAt: now})

	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})

	stats, err := svc.UsageStats(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{})
	if err != nil {
		t.Fatalf("UsageStats returned err: %v", err)
	}
	if !approxEq(stats.Totals.TotalEnergyWh, 1000) {
		t.Fatalf("TotalEnergyWh = %v, want 1000 (own scope only)", stats.Totals.TotalEnergyWh)
	}
	if !approxEq(stats.Totals.TotalCostEUR, 0.10) {
		t.Fatalf("TotalCostEUR = %v, want 0.10 (usr_2's expensive server excluded)", stats.Totals.TotalCostEUR)
	}
}
