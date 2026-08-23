// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"math"
	"testing"
	"time"
)

func TestIdleTrackerUnknownIsZero(t *testing.T) {
	tr := newIdleTracker(time.Hour)
	if got := tr.Idle("srv-never-observed"); got != 0 {
		t.Fatalf("Idle for a never-observed server = %v, want 0", got)
	}
}

func TestIdleTrackerTracksDown(t *testing.T) {
	tr := newIdleTracker(time.Hour)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tr.Observe("srv1", 100, base)
	if got := tr.Idle("srv1"); got != 100 {
		t.Fatalf("Idle after first observe = %v, want 100", got)
	}

	tr.Observe("srv1", 50, base.Add(time.Minute))
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle after a lower observe = %v, want 50", got)
	}

	// A higher sample within the window must NOT raise the tracked minimum.
	tr.Observe("srv1", 80, base.Add(2*time.Minute))
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle after a higher-but-in-window observe = %v, want unchanged 50", got)
	}

	// An exactly-equal sample also replaces (keeps) the minimum -- and refreshes
	// its timestamp, since the "<=" branch is taken.
	tr.Observe("srv1", 50, base.Add(3*time.Minute))
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle after an equal observe = %v, want 50", got)
	}
}

func TestIdleTrackerRisesAfterWindowAges(t *testing.T) {
	tr := newIdleTracker(10 * time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tr.Observe("srv1", 50, base)
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle = %v, want 50", got)
	}

	// Still within the window: a higher sample does not reset the minimum.
	tr.Observe("srv1", 80, base.Add(5*time.Minute))
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle within window = %v, want unchanged 50", got)
	}

	// The tracked minimum (set at base) is now more than the 10-minute window
	// in the past -- the next observation must reset (idle rises again).
	tr.Observe("srv1", 80, base.Add(11*time.Minute))
	if got := tr.Idle("srv1"); got != 80 {
		t.Fatalf("Idle after the window aged out = %v, want reset to 80", got)
	}
}

func TestIdleTrackerPerServerIndependence(t *testing.T) {
	tr := newIdleTracker(time.Hour)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.Observe("srv1", 100, base)
	tr.Observe("srv2", 20, base)
	if got := tr.Idle("srv1"); got != 100 {
		t.Fatalf("srv1 Idle = %v, want 100", got)
	}
	if got := tr.Idle("srv2"); got != 20 {
		t.Fatalf("srv2 Idle = %v, want 20", got)
	}
}

func TestIdleTrackerIgnoresNonFiniteAndNegative(t *testing.T) {
	tr := newIdleTracker(time.Hour)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.Observe("srv1", 50, base)
	tr.Observe("srv1", -10, base.Add(time.Minute))
	tr.Observe("srv1", math.NaN(), base.Add(2*time.Minute))
	tr.Observe("srv1", math.Inf(1), base.Add(3*time.Minute))
	tr.Observe("srv1", math.Inf(-1), base.Add(4*time.Minute))
	if got := tr.Idle("srv1"); got != 50 {
		t.Fatalf("Idle after non-finite/negative samples = %v, want unchanged 50", got)
	}
}

func TestIdleTrackerNilSafe(t *testing.T) {
	var tr *idleTracker
	tr.Observe("srv1", 10, time.Now()) // must not panic
	if got := tr.Idle("srv1"); got != 0 {
		t.Fatalf("Idle on a nil tracker = %v, want 0", got)
	}
}

func TestIdleTrackerEmptyServerIDIgnored(t *testing.T) {
	tr := newIdleTracker(time.Hour)
	tr.Observe("", 10, time.Now())
	if got := tr.Idle(""); got != 0 {
		t.Fatalf("Idle for an empty server id = %v, want 0 (Observe must ignore it)", got)
	}
}

func TestNewIdleTrackerDefaultWindow(t *testing.T) {
	tr := newIdleTracker(0)
	if tr.window != time.Hour {
		t.Fatalf("default window = %v, want 1h", tr.window)
	}
	tr2 := newIdleTracker(-5 * time.Second)
	if tr2.window != time.Hour {
		t.Fatalf("negative window default = %v, want 1h", tr2.window)
	}
}
