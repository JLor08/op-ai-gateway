// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "time"

// Limit-period whitelist (design spec §5): the four calendar periods a
// principal's request-/token-quota or cost-budget may be aligned to, plus ""
// meaning "this particular limit is off". Exported so both
// internal/gateway's PrincipalLimiter (enforcement) and internal/portal's
// LimitConfigDTO validation/display (management, Phase 2's portal task) share
// the exact same whitelist and calendar math — they can never drift on which
// period strings are valid or what "the start of this period" means.
const (
	LimitPeriodHour  = "hour"
	LimitPeriodDay   = "day"
	LimitPeriodWeek  = "week"
	LimitPeriodMonth = "month"
)

// ValidLimitPeriod reports whether period is a recognized calendar period —
// one of the four LimitPeriod* constants — or the empty string ("this limit
// is off"). Any other value (including whitespace or a different case) is
// invalid.
func ValidLimitPeriod(period string) bool {
	switch period {
	case "", LimitPeriodHour, LimitPeriodDay, LimitPeriodWeek, LimitPeriodMonth:
		return true
	default:
		return false
	}
}

// PeriodStart returns the UTC-aligned start of the calendar period containing
// now — design spec §5:
//
//   - "hour"  -> start of now's UTC hour
//   - "day"   -> 00:00 UTC of now's UTC day
//   - "week"  -> Monday 00:00 UTC of now's UTC week (ISO-style, week starts
//     Monday — this is the one case with a nontrivial calculation: it can
//     land in the previous month or even the previous year)
//   - "month" -> the 1st, 00:00 UTC, of now's UTC month
//   - anything else (including "", meaning "this limit is off") -> the zero
//     time.Time; callers never actually reach PeriodStart for an off limit
//     (they gate on period != "" first), but a caller that did would get an
//     unambiguous "no period" sentinel rather than a bogus date.
//
// Pure function of its two arguments — no clock access, no state — so it is
// trivially and deterministically unit-testable.
func PeriodStart(period string, now time.Time) time.Time {
	now = now.UTC()
	switch period {
	case LimitPeriodHour:
		return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC)
	case LimitPeriodDay:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case LimitPeriodWeek:
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		// time.Weekday: Sunday=0, Monday=1, ..., Saturday=6. Days to subtract to
		// reach the most recent Monday: Monday->0, Tuesday->1, ..., Sunday->6.
		daysSinceMonday := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -daysSinceMonday)
	case LimitPeriodMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}
