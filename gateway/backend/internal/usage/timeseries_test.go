// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var tsBase = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func tsEvent(offsetSecs int, latencyMS int64, in, out int) Event {
	return Event{
		CreatedAt:    tsBase.Add(time.Duration(offsetSecs) * time.Second),
		LatencyMS:    latencyMS,
		InputTokens:  in,
		OutputTokens: out,
	}
}

// An event lands in the bucket selected by its CreatedAt.
func TestComputeTimeSeriesBucketAssignment(t *testing.T) {
	from := tsBase
	to := tsBase.Add(30 * time.Second)
	// bucket=10s -> 3 buckets: [0,10),[10,20),[20,30). Event at +12s -> bucket 1.
	ts := ComputeTimeSeries([]Event{tsEvent(12, 0, 0, 0)}, from, to, 10)

	if ts.BucketSeconds != 10 {
		t.Fatalf("BucketSeconds = %d, want 10", ts.BucketSeconds)
	}
	if len(ts.Points) != 3 {
		t.Fatalf("Points = %d, want 3", len(ts.Points))
	}
	if !ts.Points[0].T.Equal(from) {
		t.Fatalf("Points[0].T = %v, want %v", ts.Points[0].T, from)
	}
	if !ts.Points[1].T.Equal(from.Add(10 * time.Second)) {
		t.Fatalf("Points[1].T = %v, want +10s", ts.Points[1].T)
	}
	if ts.Points[1].Connections != 1 {
		t.Fatalf("bucket 1 Connections = %d, want 1", ts.Points[1].Connections)
	}
	if ts.Points[0].Connections != 0 || ts.Points[2].Connections != 0 {
		t.Fatalf("only bucket 1 should have a connection: %#v", ts.Points)
	}
}

// promptSum/bucketSecs and completionSum/bucketSecs.
func TestComputeTimeSeriesThroughput(t *testing.T) {
	from := tsBase
	to := tsBase.Add(10 * time.Second) // single 10s bucket
	events := []Event{tsEvent(1, 0, 100, 50), tsEvent(2, 0, 100, 50)}

	ts := ComputeTimeSeries(events, from, to, 10)
	if len(ts.Points) != 1 {
		t.Fatalf("Points = %d, want 1", len(ts.Points))
	}
	p := ts.Points[0]
	if p.Connections != 2 {
		t.Fatalf("Connections = %d, want 2", p.Connections)
	}
	if p.PromptTokensPerSecond != 20 {
		t.Fatalf("PromptTokensPerSecond = %v, want 20 (200 input / 10s)", p.PromptTokensPerSecond)
	}
	if p.CompletionTokensPerSecond != 10 {
		t.Fatalf("CompletionTokensPerSecond = %v, want 10 (100 output / 10s)", p.CompletionTokensPerSecond)
	}
}

// A 3s-latency event over 1s buckets contributes to 3 consecutive buckets'
// Concurrency, while its Connection lands only in the CreatedAt bucket.
func TestComputeTimeSeriesConcurrencyOverlap(t *testing.T) {
	from := tsBase
	to := tsBase.Add(5 * time.Second) // 5 buckets of 1s
	// CreatedAt=+3s, latency 3000ms -> interval [+0s,+3s].
	ts := ComputeTimeSeries([]Event{tsEvent(3, 3000, 0, 0)}, from, to, 1)

	if len(ts.Points) != 5 {
		t.Fatalf("Points = %d, want 5", len(ts.Points))
	}
	wantConc := []int{1, 1, 1, 0, 0}
	for i, want := range wantConc {
		if ts.Points[i].Concurrency != want {
			t.Fatalf("bucket %d Concurrency = %d, want %d (%#v)", i, ts.Points[i].Concurrency, want, ts.Points)
		}
	}
	// Connection only in the CreatedAt bucket (+3s -> bucket 3).
	if ts.Points[3].Connections != 1 {
		t.Fatalf("bucket 3 Connections = %d, want 1", ts.Points[3].Connections)
	}
	for i, p := range ts.Points {
		if i != 3 && p.Connections != 0 {
			t.Fatalf("bucket %d Connections = %d, want 0", i, p.Connections)
		}
	}
}

// to <= from -> non-nil empty Points, with the envelope fields still set.
func TestComputeTimeSeriesEmptyWindow(t *testing.T) {
	from := tsBase
	for _, to := range []time.Time{tsBase, tsBase.Add(-time.Second)} {
		ts := ComputeTimeSeries([]Event{tsEvent(0, 0, 0, 0)}, from, to, 5)
		if ts.Points == nil {
			t.Fatalf("Points is nil for to=%v, want non-nil empty", to)
		}
		if len(ts.Points) != 0 {
			t.Fatalf("Points = %d for to=%v, want 0", len(ts.Points), to)
		}
		if ts.BucketSeconds != 5 || !ts.From.Equal(from) || !ts.To.Equal(to) {
			t.Fatalf("envelope not set: %#v", ts)
		}
	}
}

// bucketSecs <= 0 is treated as 1.
func TestComputeTimeSeriesGuardsBucketSecs(t *testing.T) {
	from := tsBase
	to := tsBase.Add(3 * time.Second)
	ts := ComputeTimeSeries([]Event{tsEvent(1, 0, 0, 0)}, from, to, 0)
	if ts.BucketSeconds != 1 {
		t.Fatalf("BucketSeconds = %d, want 1 (0 -> 1)", ts.BucketSeconds)
	}
	if len(ts.Points) != 3 {
		t.Fatalf("Points = %d, want 3 (1s buckets over 3s)", len(ts.Points))
	}
}

// A valid window with zero events yields n non-nil zero-valued points.
func TestComputeTimeSeriesZeroEvents(t *testing.T) {
	from := tsBase
	to := tsBase.Add(60 * time.Second)
	ts := ComputeTimeSeries(nil, from, to, 5)
	if ts.Points == nil {
		t.Fatalf("Points is nil, want non-nil")
	}
	if len(ts.Points) != 12 {
		t.Fatalf("Points = %d, want 12 (60s / 5s)", len(ts.Points))
	}
	for i, p := range ts.Points {
		if p.Connections != 0 || p.Concurrency != 0 || p.PromptTokensPerSecond != 0 || p.CompletionTokensPerSecond != 0 {
			t.Fatalf("bucket %d not zero: %#v", i, p)
		}
	}
}

// A window/bucket combination that would exceed maxTimeSeriesBuckets is COARSENED,
// not truncated: the returned series must still cover the ENTIRE [from,to) window
// (first bucket at from, last bucket covering to) with <= maxTimeSeriesBuckets
// buckets, and the reported BucketSeconds must reflect the widened bucket. This is
// the regression guard for the old silent-truncation bug (which left `from` fixed
// and clamped the count, dropping the tail of the window).
func TestComputeTimeSeriesCoarsensInsteadOfTruncating(t *testing.T) {
	from := tsBase
	to := tsBase.Add(365 * 24 * time.Hour) // 1y window
	// Requesting 1s buckets over a year would be ~31.5M buckets.
	ts := ComputeTimeSeries(nil, from, to, 1)

	if len(ts.Points) == 0 {
		t.Fatalf("Points is empty, want a coarsened series")
	}
	if len(ts.Points) > 5000 {
		t.Fatalf("Points = %d, want <= 5000 (coarsened)", len(ts.Points))
	}
	// BucketSeconds must be widened from the requested 1s and match the bucket
	// stride actually used for the points.
	if ts.BucketSeconds <= 1 {
		t.Fatalf("BucketSeconds = %d, want > 1 (coarsened)", ts.BucketSeconds)
	}
	// First bucket starts exactly at from.
	if !ts.Points[0].T.Equal(from) {
		t.Fatalf("Points[0].T = %v, want from=%v", ts.Points[0].T, from)
	}
	// The point stride equals BucketSeconds.
	if len(ts.Points) >= 2 {
		stride := ts.Points[1].T.Sub(ts.Points[0].T)
		if stride != time.Duration(ts.BucketSeconds)*time.Second {
			t.Fatalf("stride = %v, want %ds", stride, ts.BucketSeconds)
		}
	}
	// The last bucket must cover `to`: its start < to and its end (start+bucket)
	// >= to, so the whole window is represented (no truncation).
	last := ts.Points[len(ts.Points)-1].T
	bucket := time.Duration(ts.BucketSeconds) * time.Second
	if !last.Before(to) {
		t.Fatalf("last bucket start %v not before to %v", last, to)
	}
	if last.Add(bucket).Before(to) {
		t.Fatalf("last bucket end %v does not cover to %v (window truncated)", last.Add(bucket), to)
	}
}

// Coarse windows with a matching coarse bucket that stays under the cap are used
// verbatim (no coarsening): a 12h window in 3600s (1h) buckets -> 12 points, one
// connection attributed to its CreatedAt bucket, with the requested BucketSeconds.
func TestComputeTimeSeriesCoarseBucketNoCoarsening(t *testing.T) {
	from := tsBase
	to := tsBase.Add(12 * time.Hour)
	// Event 90 minutes in -> bucket index 1 for 1h buckets.
	ts := ComputeTimeSeries([]Event{tsEvent(90*60, 0, 100, 50)}, from, to, 3600)

	if ts.BucketSeconds != 3600 {
		t.Fatalf("BucketSeconds = %d, want 3600 (no coarsening)", ts.BucketSeconds)
	}
	if len(ts.Points) != 12 {
		t.Fatalf("Points = %d, want 12 (12h / 1h)", len(ts.Points))
	}
	if ts.Points[1].Connections != 1 {
		t.Fatalf("bucket 1 Connections = %d, want 1", ts.Points[1].Connections)
	}
	// Throughput divides by the requested 3600s bucket.
	if ts.Points[1].PromptTokensPerSecond != 100.0/3600.0 {
		t.Fatalf("PromptTokensPerSecond = %v, want 100/3600", ts.Points[1].PromptTokensPerSecond)
	}
	total := 0
	for _, p := range ts.Points {
		total += p.Connections
	}
	if total != 1 {
		t.Fatalf("total connections = %d, want 1", total)
	}
}

// ComputeTimeSeries sums EnergyWh per bucket alongside the token throughput
// sums, attributed to the CreatedAt bucket exactly like promptSum/completionSum
// (a plain per-bucket total, NOT divided by bucketSecs).
func TestComputeTimeSeriesEnergySum(t *testing.T) {
	from := tsBase
	to := tsBase.Add(10 * time.Second) // single 10s bucket
	e1 := tsEvent(1, 0, 100, 50)
	e1.EnergyWh = 0.4
	e2 := tsEvent(2, 0, 100, 50)
	e2.EnergyWh = 0.6
	outOfWindow := tsEvent(999, 0, 100, 50)
	outOfWindow.EnergyWh = 100 // must not leak into the in-window bucket

	ts := ComputeTimeSeries([]Event{e1, e2, outOfWindow}, from, to, 10)
	if len(ts.Points) != 1 {
		t.Fatalf("Points = %d, want 1", len(ts.Points))
	}
	if !approxEqual(ts.Points[0].EnergyWh, 1.0) {
		t.Fatalf("EnergyWh = %v, want 1.0 (0.4+0.6, out-of-window excluded)", ts.Points[0].EnergyWh)
	}
}

// The memory Recorder.TimeSeries filters via matchUsage (From/To + scope) and
// then computes the series over the matched events.
func TestRecorderTimeSeriesRespectsScopeAndWindow(t *testing.T) {
	rec := NewRecorder()
	from := tsBase
	to := tsBase.Add(10 * time.Second)
	// Two events for usr_1 in one 10s bucket, one for usr_2, one out of window.
	mine1 := tsEvent(1, 0, 100, 0)
	mine1.UserID = "usr_1"
	mine2 := tsEvent(2, 0, 100, 0)
	mine2.UserID = "usr_1"
	theirs := tsEvent(3, 0, 100, 0)
	theirs.UserID = "usr_2"
	outOfWindow := tsEvent(999, 0, 100, 0)
	outOfWindow.UserID = "usr_1"
	for _, e := range []Event{mine1, mine2, theirs, outOfWindow} {
		rec.Record(e)
	}

	// Own scope: only usr_1's two in-window events -> 200 input / 10s = 20 t/s.
	own, err := rec.TimeSeries(Query{UserID: "usr_1", From: from, To: to}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if len(own.Points) != 1 {
		t.Fatalf("own Points = %d, want 1", len(own.Points))
	}
	if own.Points[0].Connections != 2 {
		t.Fatalf("own Connections = %d, want 2", own.Points[0].Connections)
	}
	if own.Points[0].PromptTokensPerSecond != 20 {
		t.Fatalf("own PromptTokensPerSecond = %v, want 20", own.Points[0].PromptTokensPerSecond)
	}

	// ScopeAll: all three in-window events counted.
	all, err := rec.TimeSeries(Query{ScopeAll: true, From: from, To: to}, 10)
	if err != nil {
		t.Fatalf("TimeSeries returned err: %v", err)
	}
	if all.Points[0].Connections != 3 {
		t.Fatalf("all Connections = %d, want 3", all.Points[0].Connections)
	}
}

// The JSON envelope uses the field names the frontend consumes.
func TestTimeSeriesJSONFieldNames(t *testing.T) {
	ts := ComputeTimeSeries([]Event{tsEvent(1, 500, 10, 20)}, tsBase, tsBase.Add(5*time.Second), 5)
	blob, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	for _, key := range []string{
		`"points":`, `"bucket_seconds":`, `"from":`, `"to":`,
		`"t":`, `"connections":`, `"concurrency":`,
		`"prompt_tokens_per_second":`, `"completion_tokens_per_second":`, `"energy_wh":`,
	} {
		if !strings.Contains(s, key) {
			t.Fatalf("missing JSON key %s in %s", key, s)
		}
	}
	if strings.Contains(s, `"points":null`) {
		t.Fatalf("points must never marshal to null: %s", s)
	}
}
