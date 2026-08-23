// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"testing"
	"time"
)

func periodUTC(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
}

// TestPeriodStart pins PeriodStart against independently-verified calendar
// boundaries, including the two trickiest cases: a week spanning a month
// boundary and a week spanning a year boundary (design spec §5).
func TestPeriodStart(t *testing.T) {
	cases := []struct {
		name   string
		period string
		now    time.Time
		want   time.Time
	}{
		{"hour mid", LimitPeriodHour, periodUTC(2026, 3, 15, 13, 47, 22), periodUTC(2026, 3, 15, 13, 0, 0)},
		{"hour exact boundary", LimitPeriodHour, periodUTC(2026, 1, 1, 0, 0, 0), periodUTC(2026, 1, 1, 0, 0, 0)},
		{"day mid", LimitPeriodDay, periodUTC(2026, 3, 15, 13, 47, 22), periodUTC(2026, 3, 15, 0, 0, 0)},
		{"week midweek", LimitPeriodWeek, periodUTC(2026, 3, 18, 9, 0, 0), periodUTC(2026, 3, 16, 0, 0, 0)},
		{"week on monday", LimitPeriodWeek, periodUTC(2026, 8, 10, 0, 0, 1), periodUTC(2026, 8, 10, 0, 0, 0)},
		{"week saturday same month", LimitPeriodWeek, periodUTC(2026, 8, 8, 23, 59, 59), periodUTC(2026, 8, 3, 0, 0, 0)},
		{"week spans month boundary", LimitPeriodWeek, periodUTC(2026, 2, 1, 5, 0, 0), periodUTC(2026, 1, 26, 0, 0, 0)},
		{"week spans year boundary", LimitPeriodWeek, periodUTC(2026, 1, 1, 10, 0, 0), periodUTC(2025, 12, 29, 0, 0, 0)},
		{"month mid", LimitPeriodMonth, periodUTC(2026, 3, 15, 13, 47, 22), periodUTC(2026, 3, 1, 0, 0, 0)},
		{"unknown period", "bogus", periodUTC(2026, 3, 15, 13, 47, 22), time.Time{}},
		{"off (empty) period", "", periodUTC(2026, 3, 15, 13, 47, 22), time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PeriodStart(tc.period, tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("PeriodStart(%q, %v) = %v, want %v", tc.period, tc.now, got, tc.want)
			}
			if tc.want.IsZero() {
				return
			}
			if got.Location() != time.UTC {
				t.Fatalf("PeriodStart(%q, %v) location = %v, want UTC", tc.period, tc.now, got.Location())
			}
		})
	}
}

// TestPeriodStartAcceptsNonUTCInput proves PeriodStart normalizes a non-UTC
// input to UTC before computing the boundary, rather than silently computing
// against the wrong wall-clock day/hour.
func TestPeriodStartAcceptsNonUTCInput(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	// 2026-03-15 02:00 +05:00 == 2026-03-14 21:00 UTC.
	now := time.Date(2026, 3, 15, 2, 0, 0, 0, loc)
	got := PeriodStart(LimitPeriodDay, now)
	want := periodUTC(2026, 3, 14, 0, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("PeriodStart(day, %v) = %v, want %v (the UTC day, not the local one)", now, got, want)
	}
}

func TestValidLimitPeriod(t *testing.T) {
	valid := []string{"", LimitPeriodHour, LimitPeriodDay, LimitPeriodWeek, LimitPeriodMonth}
	for _, p := range valid {
		if !ValidLimitPeriod(p) {
			t.Fatalf("ValidLimitPeriod(%q) = false, want true", p)
		}
	}
	invalid := []string{"Hour", "days", "yearly", " day", "day "}
	for _, p := range invalid {
		if ValidLimitPeriod(p) {
			t.Fatalf("ValidLimitPeriod(%q) = true, want false", p)
		}
	}
}
