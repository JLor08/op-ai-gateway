// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestIsErrorPredicate(t *testing.T) {
	cases := []struct {
		status string
		http   int
		want   bool
	}{
		{"success", 200, false},
		{"success", 399, false},
		{"success", 400, true},
		{"success", 500, true},
		{"error", 200, true}, // mid-stream failure: status error but http 200
		{"error", 500, true},
		{"", 0, false},
	}
	for _, c := range cases {
		if got := IsError(c.status, c.http); got != c.want {
			t.Fatalf("IsError(%q,%d) = %v, want %v", c.status, c.http, got, c.want)
		}
	}
}

func TestSturgesBinsClamp(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{2, 5},        // ceil(log2 2)+1 = 2 -> clamp low to 5
		{4, 5},        // 3 -> clamp low to 5
		{32, 6},       // ceil(5)+1 = 6
		{524288, 20},  // ceil(19)+1 = 20
		{1000000, 20}, // ceil(19.93)+1 = 21 -> clamp high to 20
	}
	for _, c := range cases {
		if got := sturgesBins(c.n); got != c.want {
			t.Fatalf("sturgesBins(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestComputeHistogramEmptyWhenNoNonZero(t *testing.T) {
	h := ComputeHistogram([]float64{0, 0, 0})
	if len(h.Bins) != 0 {
		t.Fatalf("Bins = %#v, want empty", h.Bins)
	}
	if h.Min != 0 || h.Max != 0 || h.BinSize != 0 || h.P50 != 0 || h.P95 != 0 || h.P99 != 0 {
		t.Fatalf("empty histogram = %#v, want all zero", h)
	}
}

// The empty case MUST carry a non-nil Bins slice: a nil slice marshals to JSON
// "bins":null, and the frontend reads bins.length -> null.length would crash the
// Activity view on the common empty/zero-only state.
func TestComputeHistogramEmptyBinsNonNilAndMarshalsArray(t *testing.T) {
	for _, in := range [][]float64{nil, {}, {0, 0}} {
		h := ComputeHistogram(in)
		if h.Bins == nil {
			t.Fatalf("ComputeHistogram(%v) Bins = nil, want non-nil empty slice", in)
		}
		if len(h.Bins) != 0 {
			t.Fatalf("ComputeHistogram(%v) Bins = %#v, want empty", in, h.Bins)
		}
		blob, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("marshal histogram: %v", err)
		}
		if strings.Contains(string(blob), `"bins":null`) {
			t.Fatalf("ComputeHistogram(%v) marshaled bins:null: %s", in, blob)
		}
		if !strings.Contains(string(blob), `"bins":[]`) {
			t.Fatalf("ComputeHistogram(%v) want bins:[], got: %s", in, blob)
		}
	}
}

func TestComputeHistogramSingleValue(t *testing.T) {
	h := ComputeHistogram([]float64{0, 4.2, 0})
	if len(h.Bins) != 1 {
		t.Fatalf("Bins = %#v, want single bin", h.Bins)
	}
	if !approxEqual(h.Bins[0].X0, 4.2) || !approxEqual(h.Bins[0].X1, 4.2) || h.Bins[0].Count != 1 {
		t.Fatalf("single bin = %#v", h.Bins[0])
	}
	if h.BinSize != 0 {
		t.Fatalf("BinSize = %v, want 0", h.BinSize)
	}
	if !approxEqual(h.P50, 4.2) || !approxEqual(h.P95, 4.2) || !approxEqual(h.P99, 4.2) {
		t.Fatalf("percentiles = %v/%v/%v, want 4.2", h.P50, h.P95, h.P99)
	}
}

func TestComputeHistogramAllEqualNoDivByZero(t *testing.T) {
	h := ComputeHistogram([]float64{5, 5, 5})
	if len(h.Bins) != 1 || h.Bins[0].Count != 3 || h.BinSize != 0 {
		t.Fatalf("all-equal histogram = %#v", h)
	}
	if !approxEqual(h.Min, 5) || !approxEqual(h.Max, 5) {
		t.Fatalf("min/max = %v/%v, want 5", h.Min, h.Max)
	}
}

func TestComputeHistogramDistributionAndPercentiles(t *testing.T) {
	// 1..10, no zeros. n=10 -> bins = ceil(log2 10)+1 = 5, binSize = 1.8.
	h := ComputeHistogram([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if len(h.Bins) != 5 {
		t.Fatalf("bins = %d, want 5", len(h.Bins))
	}
	if !approxEqual(h.Min, 1) || !approxEqual(h.Max, 10) || !approxEqual(h.BinSize, 1.8) {
		t.Fatalf("min/max/binSize = %v/%v/%v", h.Min, h.Max, h.BinSize)
	}
	for i, b := range h.Bins {
		if b.Count != 2 {
			t.Fatalf("bin %d count = %d, want 2", i, b.Count)
		}
	}
	if !approxEqual(h.Bins[0].X0, 1) || !approxEqual(h.Bins[4].X1, 10) {
		t.Fatalf("bin edges = [%v..%v]", h.Bins[0].X0, h.Bins[4].X1)
	}
	// nearest-rank: p50 -> ceil(.5*10)=5 -> sorted[4]=5; p95/p99 -> rank 10 -> 10.
	if !approxEqual(h.P50, 5) || !approxEqual(h.P95, 10) || !approxEqual(h.P99, 10) {
		t.Fatalf("percentiles = %v/%v/%v, want 5/10/10", h.P50, h.P95, h.P99)
	}
}

func TestNormalizers(t *testing.T) {
	if NormalizeSort("bogus") != "created_at" {
		t.Fatalf("NormalizeSort(bogus) = %q, want created_at", NormalizeSort("bogus"))
	}
	if NormalizeSort("latency_ms") != "latency_ms" {
		t.Fatalf("NormalizeSort(latency_ms) = %q", NormalizeSort("latency_ms"))
	}
	if NormalizeOrder("ASC") != "asc" || NormalizeOrder("weird") != "desc" {
		t.Fatalf("NormalizeOrder wrong: %q/%q", NormalizeOrder("ASC"), NormalizeOrder("weird"))
	}
	if NormalizeLimit(7) != 25 || NormalizeLimit(50) != 50 || NormalizeLimit(100) != 100 {
		t.Fatalf("NormalizeLimit wrong")
	}
	if NormalizePage(0) != 1 || NormalizePage(-3) != 1 || NormalizePage(4) != 4 {
		t.Fatalf("NormalizePage wrong")
	}
	if TotalPages(0, 25) != 0 || TotalPages(60, 25) != 3 || TotalPages(50, 25) != 2 {
		t.Fatalf("TotalPages wrong: %d/%d/%d", TotalPages(0, 25), TotalPages(60, 25), TotalPages(50, 25))
	}
}
