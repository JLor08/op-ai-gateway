// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "time"

// VendorGroupThresholds are the used-percent values (1..100) at which a group
// moves on from a member, per limit category.
type VendorGroupThresholds struct {
	FiveHourPct, WeeklyPct, CreditsPct int
}

// Thresholds returns the group's three switch-over thresholds.
func (g VendorAccountGroup) Thresholds() VendorGroupThresholds {
	return VendorGroupThresholds{
		FiveHourPct: g.ThresholdFiveHourPct,
		WeeklyPct:   g.ThresholdWeeklyPct,
		CreditsPct:  g.ThresholdCreditsPct,
	}
}

// VendorGroupCandidate is one group member as seen by the selector. Usable
// covers the structural rules (member exists, account enabled, subscription on
// and so on); ThresholdsApply says whether the member's usage may be held
// against the group's thresholds (the member belongs to the group owner, or its
// owner shares usage with members). A zero CooldownUntil means no cooldown.
type VendorGroupCandidate struct {
	AccountID       string
	Usable          bool
	Usage           VendorAccountUsage
	HasUsage        bool
	ThresholdsApply bool
	CooldownUntil   time.Time
}

// Block categories reported by VendorGroupMemberBlocks.
const (
	VendorBlockFiveHour = "five_hour"
	VendorBlockWeekly   = "weekly"
	VendorBlockCredits  = "credits"
	VendorBlockCooldown = "cooldown"
)

// VendorMemberBlock is one reason a member is currently skipped. FreeAt is when
// the block lifts; nil means unknown.
type VendorMemberBlock struct {
	Category string
	Pct      float64
	FreeAt   *time.Time
}

// VendorGroupMemberBlocks lists why c is blocked at now, ordered five_hour,
// weekly, credits, cooldown. A threshold category blocks when its percent is
// known and at or above the threshold and its window has not already reset (a
// stale snapshot past its reset counts as renewed). An empty result means the
// member is available.
func VendorGroupMemberBlocks(c VendorGroupCandidate, t VendorGroupThresholds, now time.Time) []VendorMemberBlock {
	var blocks []VendorMemberBlock
	if c.ThresholdsApply && c.HasUsage {
		add := func(category string, pct float64, threshold int, resetAt *time.Time) {
			if pct < 0 || pct < float64(threshold) {
				return
			}
			if resetAt != nil && !resetAt.After(now) {
				return
			}
			blocks = append(blocks, VendorMemberBlock{Category: category, Pct: pct, FreeAt: resetAt})
		}
		add(VendorBlockFiveHour, c.Usage.FiveHourPct, t.FiveHourPct, c.Usage.FiveHourResetAt)
		add(VendorBlockWeekly, c.Usage.WeeklyPct, t.WeeklyPct, c.Usage.WeeklyResetAt)
		add(VendorBlockCredits, c.Usage.SpendUsedPct, t.CreditsPct, c.Usage.SpendResetAt)
	}
	if c.CooldownUntil.After(now) {
		until := c.CooldownUntil
		blocks = append(blocks, VendorMemberBlock{Category: VendorBlockCooldown, FreeAt: &until})
	}
	return blocks
}

// VendorGroupMemberFreeAt returns the moment every block lifted, the latest
// FreeAt. It is false when any block has no known end.
func VendorGroupMemberFreeAt(blocks []VendorMemberBlock) (time.Time, bool) {
	var latest time.Time
	for _, b := range blocks {
		if b.FreeAt == nil {
			return time.Time{}, false
		}
		if b.FreeAt.After(latest) {
			latest = *b.FreeAt
		}
	}
	return latest, true
}

// SelectVendorGroupMember picks the member to serve a request: the first usable
// candidate with no blocks. When every usable candidate is blocked and
// allowFallback is set, the one that frees up earliest (unknown last, ties by
// index) is chosen so the request still gets an answer. No usable candidate, or
// all blocked without fallback, is (-1, false).
func SelectVendorGroupMember(cands []VendorGroupCandidate, t VendorGroupThresholds, now time.Time, allowFallback bool) (index int, ok bool) {
	best := -1
	var bestFree time.Time
	bestKnown := false
	for i, c := range cands {
		if !c.Usable {
			continue
		}
		blocks := VendorGroupMemberBlocks(c, t, now)
		if len(blocks) == 0 {
			return i, true
		}
		if !allowFallback {
			continue
		}
		free, known := VendorGroupMemberFreeAt(blocks)
		switch {
		case best == -1,
			known && !bestKnown,
			known && bestKnown && free.Before(bestFree):
			best, bestFree, bestKnown = i, free, known
		}
	}
	if best == -1 {
		return -1, false
	}
	return best, true
}
