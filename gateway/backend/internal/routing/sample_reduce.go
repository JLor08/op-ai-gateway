// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "time"

// This file holds the PURE sample-reduction helpers shared by the two
// routing.Store implementations: MemoryStore (memory_store.go, this package)
// and the SQL store (internal/store's SQLiteStore). Both back
// ServerAvailabilitySamples/TelemetrySamples with the exact same
// transition/gap/decimation contract; before RT-2 that logic was duplicated
// verbatim in both packages, kept in sync only by "mirrors ..." comments. It
// is extracted here (not into internal/store) because store already imports
// routing — the reverse import would be a cycle — and because the functions
// operate purely on routing's own sample types, with no store/DB dependency.

// AvailabilityGapFloor: consecutive samples farther apart than this are
// treated as a boundary (observer gap) and never collapsed, so a
// gateway-down gap stays visible even between two same-state samples.
const AvailabilityGapFloor = 10 * time.Minute

// AvailabilityStateKey returns the "reduction identity" of a sample: two
// samples with the same key are considered the same observed state and a
// contiguous run of them can be collapsed to its endpoints.
func AvailabilityStateKey(s ServerAvailabilitySample) string {
	key := s.Health
	if s.AgentReporting {
		key += "|1"
	} else {
		key += "|0"
	}
	if s.NetbirdConnected {
		key += "|1"
	} else {
		key += "|0"
	}
	return key
}

// ReduceAvailabilitySamples collapses runs of consecutive same-state,
// contiguous samples, keeping every state transition, every time-gap boundary
// (> gap), and both endpoints. Input must be ascending by reported_at.
// limit>0 caps the result with even-index decimation afterward (pathological
// flapping only).
func ReduceAvailabilitySamples(all []ServerAvailabilitySample, gap time.Duration, limit int) []ServerAvailabilitySample {
	// Pre-pass over the RAW spacing: flag a sample whose immediate predecessor
	// was more than the gap floor away (an observer gap — the gateway was not
	// sampling). This aligns with the gapBoundary keep rule below, so the kept
	// post-gap sample carries GapBefore=true even after a same-state run
	// collapses to [start,end]. all[0] has no in-window predecessor, so it
	// stays false.
	for i := 1; i < len(all); i++ {
		if all[i].ReportedAt.Sub(all[i-1].ReportedAt) > gap {
			all[i].GapBefore = true
		}
	}
	n := len(all)
	if n <= 2 {
		return all
	}
	out := make([]ServerAvailabilitySample, 0, n)
	out = append(out, all[0])
	for i := 1; i < n-1; i++ {
		changed := AvailabilityStateKey(all[i]) != AvailabilityStateKey(all[i-1]) ||
			AvailabilityStateKey(all[i]) != AvailabilityStateKey(all[i+1])
		gapBoundary := all[i].ReportedAt.Sub(all[i-1].ReportedAt) > gap ||
			all[i+1].ReportedAt.Sub(all[i].ReportedAt) > gap
		if changed || gapBoundary {
			out = append(out, all[i])
		}
	}
	out = append(out, all[n-1])
	if limit > 0 && len(out) > limit {
		out = DecimateAvailabilitySamples(out, limit)
	}
	return out
}

// DecimateAvailabilitySamples returns at most limit evenly-spaced samples,
// always keeping oldest+newest. limit<=0 means no cap. Ascending input.
func DecimateAvailabilitySamples(all []ServerAvailabilitySample, limit int) []ServerAvailabilitySample {
	n := len(all)
	if limit <= 0 || n <= limit {
		return all
	}
	if limit == 1 {
		return []ServerAvailabilitySample{all[n-1]}
	}
	out := make([]ServerAvailabilitySample, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, all[i*(n-1)/(limit-1)])
	}
	return out
}

// DecimateTelemetrySamples returns at most limit evenly-spaced samples across
// all, always keeping the oldest (first) and newest (last). limit <= 0 means
// "no cap". Because it never exceeds limit points, the caller gets a bounded
// series spanning the full window. Input must already be ascending by
// reported_at.
func DecimateTelemetrySamples(all []TelemetrySample, limit int) []TelemetrySample {
	n := len(all)
	if limit <= 0 || n <= limit {
		return all
	}
	if limit == 1 {
		return []TelemetrySample{all[n-1]}
	}
	out := make([]TelemetrySample, 0, limit)
	for i := 0; i < limit; i++ {
		// Map i in [0,limit-1] onto [0,n-1] so i==0 -> oldest and
		// i==limit-1 -> newest; the step >= 1 keeps indices strictly ascending.
		out = append(out, all[i*(n-1)/(limit-1)])
	}
	return out
}
