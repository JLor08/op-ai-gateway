// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"testing"
	"time"
)

var selNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func selAt(d time.Duration) *time.Time { t := selNow.Add(d); return &t }

func unknownUsage() VendorAccountUsage {
	return VendorAccountUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1}
}

func cand(id string, u VendorAccountUsage) VendorGroupCandidate {
	return VendorGroupCandidate{AccountID: id, Usable: true, Usage: u, HasUsage: true, ThresholdsApply: true}
}

var selThr = VendorGroupThresholds{FiveHourPct: 99, WeeklyPct: 99, CreditsPct: 99}

func TestSelectVendorGroupMemberPrefersFirstUnblocked(t *testing.T) {
	u := unknownUsage()
	u.FiveHourPct, u.FiveHourResetAt = 99, selAt(time.Hour)
	cands := []VendorGroupCandidate{cand("a", u), cand("b", unknownUsage())}
	if i, ok := SelectVendorGroupMember(cands, selThr, selNow, false); !ok || i != 1 {
		t.Fatalf("= (%d,%v), want 1", i, ok)
	}
}

func TestSelectVendorGroupMemberStaleSnapshotCountsAsRenewed(t *testing.T) {
	u := unknownUsage()
	u.FiveHourPct, u.FiveHourResetAt = 99.5, selAt(-time.Minute)
	cands := []VendorGroupCandidate{cand("a", u), cand("b", unknownUsage())}
	if i, ok := SelectVendorGroupMember(cands, selThr, selNow, false); !ok || i != 0 {
		t.Fatalf("= (%d,%v), want 0", i, ok)
	}
}

func TestVendorGroupMemberBlocks(t *testing.T) {
	t.Run("weekly nil reset blocks with unknown free-at", func(t *testing.T) {
		u := unknownUsage()
		u.WeeklyPct = 100
		b := VendorGroupMemberBlocks(cand("a", u), selThr, selNow)
		if len(b) != 1 || b[0].Category != VendorBlockWeekly || b[0].FreeAt != nil || b[0].Pct != 100 {
			t.Fatalf("blocks = %#v", b)
		}
		if _, known := VendorGroupMemberFreeAt(b); known {
			t.Fatal("free-at must be unknown")
		}
	})
	t.Run("credits threshold", func(t *testing.T) {
		u := unknownUsage()
		u.SpendUsedPct = 99
		if b := VendorGroupMemberBlocks(cand("a", u), selThr, selNow); len(b) != 1 || b[0].Category != VendorBlockCredits {
			t.Fatalf("at 99/99 blocks = %#v", b)
		}
		thr := selThr
		thr.CreditsPct = 100
		if b := VendorGroupMemberBlocks(cand("a", u), thr, selNow); len(b) != 0 {
			t.Fatalf("at 99/100 blocks = %#v", b)
		}
	})
	t.Run("unknown never blocks", func(t *testing.T) {
		if b := VendorGroupMemberBlocks(cand("a", unknownUsage()), selThr, selNow); len(b) != 0 {
			t.Fatalf("blocks = %#v", b)
		}
		c := cand("a", VendorAccountUsage{FiveHourPct: 100, WeeklyPct: 100, SpendUsedPct: 100})
		c.HasUsage = false
		if b := VendorGroupMemberBlocks(c, selThr, selNow); len(b) != 0 {
			t.Fatalf("no usage but blocks = %#v", b)
		}
	})
	t.Run("thresholds do not apply", func(t *testing.T) {
		u := unknownUsage()
		u.FiveHourPct = 100
		c := cand("a", u)
		c.ThresholdsApply = false
		if b := VendorGroupMemberBlocks(c, selThr, selNow); len(b) != 0 {
			t.Fatalf("blocks = %#v", b)
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		c := cand("a", unknownUsage())
		c.CooldownUntil = selNow.Add(30 * time.Second)
		b := VendorGroupMemberBlocks(c, selThr, selNow)
		if len(b) != 1 || b[0].Category != VendorBlockCooldown || b[0].FreeAt == nil || !b[0].FreeAt.Equal(selNow.Add(30*time.Second)) {
			t.Fatalf("blocks = %#v", b)
		}
		c.CooldownUntil = selNow.Add(-time.Second)
		if b := VendorGroupMemberBlocks(c, selThr, selNow); len(b) != 0 {
			t.Fatalf("past cooldown blocks = %#v", b)
		}
	})
	t.Run("cooldown applies even without threshold opt-in", func(t *testing.T) {
		c := cand("a", unknownUsage())
		c.ThresholdsApply, c.HasUsage = false, false
		c.CooldownUntil = selNow.Add(time.Second)
		if b := VendorGroupMemberBlocks(c, selThr, selNow); len(b) != 1 {
			t.Fatalf("blocks = %#v", b)
		}
	})
	t.Run("order", func(t *testing.T) {
		c := cand("a", VendorAccountUsage{FiveHourPct: 100, WeeklyPct: 100, SpendUsedPct: 100})
		c.CooldownUntil = selNow.Add(time.Second)
		b := VendorGroupMemberBlocks(c, selThr, selNow)
		want := []string{VendorBlockFiveHour, VendorBlockWeekly, VendorBlockCredits, VendorBlockCooldown}
		if len(b) != 4 {
			t.Fatalf("blocks = %#v", b)
		}
		for i := range want {
			if b[i].Category != want[i] {
				t.Fatalf("order = %#v", b)
			}
		}
	})
}

func TestSelectVendorGroupMemberFallback(t *testing.T) {
	a := unknownUsage()
	a.FiveHourPct, a.FiveHourResetAt = 100, selAt(2*time.Hour)
	b := unknownUsage()
	b.FiveHourPct, b.FiveHourResetAt = 100, selAt(30*time.Minute)
	b.WeeklyPct, b.WeeklyResetAt = 100, selAt(72*time.Hour)
	unk := unknownUsage()
	unk.WeeklyPct = 100

	t.Run("latest free-at per member, earliest member wins", func(t *testing.T) {
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{cand("b", b), cand("a", a)}, selThr, selNow, true); !ok || i != 1 {
			t.Fatalf("= (%d,%v), want 1", i, ok)
		}
	})
	t.Run("unknown sorts last", func(t *testing.T) {
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{cand("u", unk), cand("a", a)}, selThr, selNow, true); !ok || i != 1 {
			t.Fatalf("= (%d,%v), want 1", i, ok)
		}
	})
	t.Run("all unknown picks first", func(t *testing.T) {
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{cand("u", unk), cand("v", unk)}, selThr, selNow, true); !ok || i != 0 {
			t.Fatalf("= (%d,%v), want 0", i, ok)
		}
	})
	t.Run("ties keep index order", func(t *testing.T) {
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{cand("a1", a), cand("a2", a)}, selThr, selNow, true); !ok || i != 0 {
			t.Fatalf("= (%d,%v), want 0", i, ok)
		}
	})
	t.Run("no fallback", func(t *testing.T) {
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{cand("a", a), cand("b", b)}, selThr, selNow, false); ok || i != -1 {
			t.Fatalf("= (%d,%v), want (-1,false)", i, ok)
		}
	})
	t.Run("skips unusable even with fallback", func(t *testing.T) {
		x := cand("a", a)
		x.Usable = false
		if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{x, cand("b", b)}, selThr, selNow, true); !ok || i != 1 {
			t.Fatalf("= (%d,%v), want 1", i, ok)
		}
	})
	t.Run("no usable candidate", func(t *testing.T) {
		x := cand("a", unknownUsage())
		x.Usable = false
		for _, fb := range []bool{false, true} {
			if i, ok := SelectVendorGroupMember([]VendorGroupCandidate{x}, selThr, selNow, fb); ok || i != -1 {
				t.Fatalf("fallback=%v = (%d,%v)", fb, i, ok)
			}
		}
		if i, ok := SelectVendorGroupMember(nil, selThr, selNow, true); ok || i != -1 {
			t.Fatalf("nil = (%d,%v)", i, ok)
		}
	})
}
