// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import "time"

// TimeSeriesPoint is one bucket of the activity time-series. T is the bucket
// start (oldest-first ordering across a TimeSeries).
type TimeSeriesPoint struct {
	T                         time.Time `json:"t"`
	Connections               int       `json:"connections"`                  // completed requests whose CreatedAt is in the bucket
	Concurrency               int       `json:"concurrency"`                  // requests whose [CreatedAt-LatencyMS, CreatedAt] overlaps the bucket
	PromptTokensPerSecond     float64   `json:"prompt_tokens_per_second"`     // sum(InputTokens) in bucket / bucketSecs
	CompletionTokensPerSecond float64   `json:"completion_tokens_per_second"` // sum(OutputTokens) in bucket / bucketSecs
	// EnergyWh is sum(EnergyWh) of the bucket's CreatedAt-attributed events, in
	// watt-hours (NOT divided by bucketSecs — an energy total per bucket, mirroring
	// Connections rather than the per-second throughput fields).
	EnergyWh float64 `json:"energy_wh"`
}

// TimeSeries is the bucketed activity aggregate. Points is ALWAYS non-nil (an
// empty slice when there are no buckets) so it never marshals to JSON null.
type TimeSeries struct {
	Points        []TimeSeriesPoint `json:"points"`
	BucketSeconds int               `json:"bucket_seconds"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
}

// maxTimeSeriesBuckets caps the number of buckets defensively so pathological
// (from,to,bucket) inputs cannot allocate unbounded slices. When the requested
// resolution would exceed this, ComputeTimeSeries COARSENS the bucket (widens it
// to fit the whole window in this many buckets) rather than truncating the window.
const maxTimeSeriesBuckets = 5000

// ComputeTimeSeries buckets events into [from+i*bucket, from+(i+1)*bucket)
// windows and aggregates connections, concurrency, and token throughput per
// bucket. It is pure: guards bucketSecs<=0 (treated as 1) and to<=from (returns
// a non-nil empty Points), and caps the bucket count. Points are ordered
// oldest->newest. Callers must set From/To (the endpoint always sets To=now).
func ComputeTimeSeries(events []Event, from, to time.Time, bucketSecs int) TimeSeries {
	if bucketSecs <= 0 {
		bucketSecs = 1
	}
	ts := TimeSeries{
		Points:        []TimeSeriesPoint{},
		BucketSeconds: bucketSecs,
		From:          from,
		To:            to,
	}
	if !to.After(from) {
		return ts
	}

	span := to.Sub(from)
	bucket := time.Duration(bucketSecs) * time.Second
	// n = ceil(span/bucket).
	n := int((span + bucket - 1) / bucket)
	if n <= 0 {
		return ts
	}
	// Coarsening (NOT truncation): if the requested resolution would produce more
	// than maxTimeSeriesBuckets buckets (e.g. window=1y with bucket=1s -> ~31M
	// buckets), widen the bucket to the smallest whole-second size that still fits
	// the ENTIRE [from,to) window in maxTimeSeriesBuckets buckets, then recompute
	// n. This keeps the full window covered at a coarser resolution instead of the
	// old behavior, which clamped n but left `from` unchanged and thus silently
	// dropped the older tail of the window. It also bounds slice allocation for
	// absurd window/bucket combinations. bucketSecs is updated so the per-bucket
	// timestamps, the throughput divisor, and the reported BucketSeconds all agree.
	if n > maxTimeSeriesBuckets {
		spanSecs := int64((span + time.Second - 1) / time.Second) // ceil to whole seconds
		effSecs := (spanSecs + maxTimeSeriesBuckets - 1) / maxTimeSeriesBuckets
		if effSecs < 1 {
			effSecs = 1
		}
		bucketSecs = int(effSecs)
		bucket = time.Duration(bucketSecs) * time.Second
		n = int((span + bucket - 1) / bucket)
		ts.BucketSeconds = bucketSecs
	}

	connections := make([]int, n)
	concurrency := make([]int, n)
	promptSum := make([]float64, n)
	completionSum := make([]float64, n)
	energySum := make([]float64, n)

	for _, e := range events {
		// Connections + token/energy sums attributed to the CreatedAt bucket, only
		// for events whose CreatedAt falls in [from, to).
		if !e.CreatedAt.Before(from) && e.CreatedAt.Before(to) {
			idx := int(e.CreatedAt.Sub(from) / bucket)
			if idx >= 0 && idx < n {
				connections[idx]++
				promptSum[idx] += float64(e.InputTokens)
				completionSum[idx] += float64(e.OutputTokens)
				energySum[idx] += e.EnergyWh
			}
		}

		// Concurrency: the request interval [start, end] with start = CreatedAt -
		// LatencyMS and end = CreatedAt. A bucket [bStart, bEnd) counts the event
		// when bStart < end AND start < bEnd (open on the shared endpoint, so an
		// interval ending exactly at a bucket boundary does not spill into the
		// next bucket).
		end := e.CreatedAt
		start := e.CreatedAt.Add(-time.Duration(e.LatencyMS) * time.Millisecond)
		de := end.Sub(from)   // interval end relative to window start
		ds := start.Sub(from) // interval start relative to window start
		if de <= 0 {
			continue // interval ends at/before the window start: no overlap
		}
		// firstIdx: smallest i with (i+1)*bucket > ds  ->  floor(ds/bucket), >=0.
		firstIdx := 0
		if ds > 0 {
			firstIdx = int(ds / bucket)
		}
		// lastIdx: largest i with i*bucket < de  ->  ceil(de/bucket) - 1.
		lastIdx := int((de+bucket-1)/bucket) - 1
		if lastIdx >= n {
			lastIdx = n - 1
		}
		for i := firstIdx; i <= lastIdx; i++ {
			if i >= 0 && i < n {
				concurrency[i]++
			}
		}
	}

	points := make([]TimeSeriesPoint, n)
	secs := float64(bucketSecs)
	for i := 0; i < n; i++ {
		points[i] = TimeSeriesPoint{
			T:                         from.Add(time.Duration(i) * bucket),
			Connections:               connections[i],
			Concurrency:               concurrency[i],
			PromptTokensPerSecond:     promptSum[i] / secs,
			CompletionTokensPerSecond: completionSum[i] / secs,
			EnergyWh:                  energySum[i],
		}
	}
	ts.Points = points
	return ts
}
