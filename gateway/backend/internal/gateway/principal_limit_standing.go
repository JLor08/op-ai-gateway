// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "time"

// LimitKind names one of the three persistent limits a principal may carry. The
// values are deliberately equal to the reason strings Admit returns on a denial,
// so a denial's reason maps straight onto the reading that describes it.
type LimitKind string

const (
	LimitKindRequestQuota LimitKind = "request_quota"
	LimitKindTokenQuota   LimitKind = "token_quota"
	LimitKindCostBudget   LimitKind = "cost_budget"
)

// LimitReading is the state of one configured limit as admission saw it (design
// spec §5): the period aggregate, the configured cap, the period and the moment
// that period ends. Failed marks a reading whose aggregate could not be read:
// Used is then meaningless (zero), which must never be mistaken for "0 % used".
type LimitReading struct {
	Kind   LimitKind
	Used   float64
	Cap    float64
	Period string
	Reset  time.Time
	Failed bool
}

// Utilization is Used / Cap. Cap is positive for any configured limit.
func (r LimitReading) Utilization() float64 {
	if r.Cap <= 0 {
		return 0
	}
	return r.Used / r.Cap
}

// breached reports whether the limit is reached. A failed read never counts,
// matching Admit's fail-open treatment of a store error.
func (r LimitReading) breached() bool {
	return !r.Failed && r.Used >= r.Cap
}

// LimitStanding holds the readings of every configured limit in the fixed order
// request quota, token quota, cost budget. Nil or empty means no limit is
// configured, or the config could not be read.
type LimitStanding []LimitReading

// Fullest returns the reading to describe on a success (design spec §5): the
// highest utilization; ties go to the later reset, then to the earlier position.
// ok is false when the standing is empty or any reading failed, because a
// partial picture could name the wrong limit.
func (s LimitStanding) Fullest() (LimitReading, bool) {
	if len(s) == 0 {
		return LimitReading{}, false
	}
	best := s[0]
	for _, r := range s {
		if r.Failed {
			return LimitReading{}, false
		}
	}
	for _, r := range s[1:] {
		switch {
		case r.Utilization() > best.Utilization():
			best = r
		case r.Utilization() == best.Utilization() && r.Reset.After(best.Reset):
			best = r
		}
	}
	return best, true
}

// Blocking returns the reading to describe on a denial (design spec §6): among
// the breached readings the one with the latest reset (when the user can work
// again), ties to the earlier position. When none is breached, for example
// because the cache moved since Admit decided, it falls back to the reading of
// the kind Admit named, failed or not. ok is false when that kind is absent.
func (s LimitStanding) Blocking(fallback LimitKind) (LimitReading, bool) {
	var best LimitReading
	found := false
	for _, r := range s {
		if !r.breached() {
			continue
		}
		if !found || r.Reset.After(best.Reset) {
			best, found = r, true
		}
	}
	if found {
		return best, true
	}
	for _, r := range s {
		if r.Kind == fallback {
			return r, true
		}
	}
	return LimitReading{}, false
}
