// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

func TestStandingFullestPicksHighestUtilization(t *testing.T) {
	s := LimitStanding{
		{Kind: LimitKindRequestQuota, Used: 50, Cap: 100},
		{Kind: LimitKindTokenQuota, Used: 900, Cap: 1000},
		{Kind: LimitKindCostBudget, Used: 0.2, Cap: 1},
	}
	got, ok := s.Fullest()
	if !ok || got.Kind != LimitKindTokenQuota {
		t.Fatalf("Fullest() = (%v,%v), want token quota", got.Kind, ok)
	}
}

func TestStandingFullestTieBreaks(t *testing.T) {
	week := utc(2026, 3, 23, 0, 0, 0)
	day := utc(2026, 3, 19, 0, 0, 0)
	s := LimitStanding{
		{Kind: LimitKindRequestQuota, Used: 50, Cap: 100, Reset: day},
		{Kind: LimitKindTokenQuota, Used: 500, Cap: 1000, Reset: week},
	}
	if got, _ := s.Fullest(); got.Kind != LimitKindTokenQuota {
		t.Fatalf("equal utilization: Fullest() = %v, want the later reset (token quota)", got.Kind)
	}
	s[1].Reset = day
	if got, _ := s.Fullest(); got.Kind != LimitKindRequestQuota {
		t.Fatalf("equal reset too: Fullest() = %v, want the earlier position (request quota)", got.Kind)
	}
}

func TestStandingFullestRefusesFailedOrEmpty(t *testing.T) {
	if _, ok := LimitStanding(nil).Fullest(); ok {
		t.Fatal("nil standing: ok = true, want false")
	}
	s := LimitStanding{
		{Kind: LimitKindRequestQuota, Used: 50, Cap: 100},
		{Kind: LimitKindTokenQuota, Cap: 1000, Failed: true},
	}
	if _, ok := s.Fullest(); ok {
		t.Fatal("standing with a failed reading: ok = true, want false")
	}
}

func TestStandingBlockingLatestReset(t *testing.T) {
	s := LimitStanding{
		{Kind: LimitKindRequestQuota, Used: 10, Cap: 10, Reset: utc(2026, 3, 19, 0, 0, 0)},
		{Kind: LimitKindTokenQuota, Used: 5, Cap: 1000, Reset: utc(2026, 3, 23, 0, 0, 0)},
		{Kind: LimitKindCostBudget, Used: 1, Cap: 1, Reset: utc(2026, 4, 1, 0, 0, 0)},
	}
	if got, ok := s.Blocking(LimitKindRequestQuota); !ok || got.Kind != LimitKindCostBudget {
		t.Fatalf("Blocking() = (%v,%v), want cost budget", got.Kind, ok)
	}
	// A failed reading is never breached, however high its Used.
	s[2] = LimitReading{Kind: LimitKindCostBudget, Used: 9, Cap: 1, Reset: utc(2026, 4, 1, 0, 0, 0), Failed: true}
	if got, ok := s.Blocking(LimitKindRequestQuota); !ok || got.Kind != LimitKindRequestQuota {
		t.Fatalf("Blocking() with failed cost = (%v,%v), want request quota", got.Kind, ok)
	}
}

func TestStandingBlockingFallback(t *testing.T) {
	s := LimitStanding{
		{Kind: LimitKindRequestQuota, Used: 1, Cap: 10},
		{Kind: LimitKindTokenQuota, Cap: 1000, Failed: true},
	}
	if got, ok := s.Blocking(LimitKindTokenQuota); !ok || got.Kind != LimitKindTokenQuota {
		t.Fatalf("Blocking(token) = (%v,%v), want the token reading", got.Kind, ok)
	}
	if _, ok := s.Blocking(LimitKindCostBudget); ok {
		t.Fatal("Blocking(cost) with no cost reading: ok = true, want false")
	}
}
