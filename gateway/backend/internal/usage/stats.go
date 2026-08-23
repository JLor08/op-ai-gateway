// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"math"
	"sort"
	"strings"
)

// IsError is the single error predicate shared by the list status filter, the
// stats error_count, and the frontend chip color. status="error" catches
// mid-stream failures that still carry http_status=200.
func IsError(status string, httpStatus int) bool {
	return status == "error" || httpStatus >= 400
}

var sortWhitelist = map[string]bool{
	"created_at":        true,
	"latency_ms":        true,
	"total_tokens":      true,
	"prompt_per_second": true,
	"tokens_per_second": true,
	"http_status":       true,
	"model":             true,
	"requested_model":   true,
	"server_name":       true,
	"token_name":        true,
}

// NormalizeSort clamps an unknown sort key to the default "created_at".
func NormalizeSort(sort string) string {
	if sortWhitelist[sort] {
		return sort
	}
	return "created_at"
}

// NormalizeOrder returns "asc" or "desc" (default/other -> "desc").
func NormalizeOrder(order string) string {
	if strings.ToLower(order) == "asc" {
		return "asc"
	}
	return "desc"
}

// NormalizeLimit clamps to the allowed page sizes (default 25).
func NormalizeLimit(limit int) int {
	switch limit {
	case 25, 50, 100:
		return limit
	default:
		return 25
	}
}

// NormalizePage clamps to a 1-based page.
func NormalizePage(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// TotalPages = ceil(total/limit); 0 when there is nothing to page.
func TotalPages(total, limit int) int {
	if limit <= 0 {
		return 0
	}
	return (total + limit - 1) / limit
}

// sturgesBins = clamp(ceil(log2(n)) + 1, 5, 20).
func sturgesBins(n int) int {
	if n < 1 {
		return 5
	}
	bins := int(math.Ceil(math.Log2(float64(n)))) + 1
	if bins < 5 {
		return 5
	}
	if bins > 20 {
		return 20
	}
	return bins
}

// nearestRank returns the value at rank ceil(p/100 * n) (1-based) of a sorted slice.
func nearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// ComputeHistogram builds a Sturges-binned distribution over the NON-ZERO values
// of the input (one value per row in the filtered set). Zeros are dropped so
// mock/error rows with 0 t/s do not skew bins or percentiles. Edge cases:
//   - N==0            -> empty Histogram, Bins is a non-nil empty slice (all fields 0)
//   - N==1 or min==max -> single bin [min,min], BinSize 0, Count N (no div-by-zero)
//
// Bins is ALWAYS non-nil: a nil slice marshals to JSON "bins":null, and the
// frontend Histogram type declares bins as a non-null array (SpeedHistogram
// reads bins.length), so a null would crash the Activity view on the empty state.
func ComputeHistogram(values []float64) Histogram {
	nonZero := make([]float64, 0, len(values))
	for _, v := range values {
		if v != 0 {
			nonZero = append(nonZero, v)
		}
	}
	n := len(nonZero)
	if n == 0 {
		return Histogram{Bins: []HistogramBin{}}
	}
	sort.Float64s(nonZero)
	lo, hi := nonZero[0], nonZero[n-1]

	h := Histogram{
		Min: lo,
		Max: hi,
		P50: nearestRank(nonZero, 50),
		P95: nearestRank(nonZero, 95),
		P99: nearestRank(nonZero, 99),
	}
	if n == 1 || lo == hi {
		h.Bins = []HistogramBin{{X0: lo, X1: lo, Count: n}}
		return h
	}

	binCount := sturgesBins(n)
	binSize := (hi - lo) / float64(binCount)
	h.BinSize = binSize
	bins := make([]HistogramBin, binCount)
	for i := 0; i < binCount; i++ {
		bins[i].X0 = lo + float64(i)*binSize
		bins[i].X1 = lo + float64(i+1)*binSize
	}
	for _, v := range nonZero {
		idx := int((v - lo) / binSize)
		if idx >= binCount {
			idx = binCount - 1
		}
		if idx < 0 {
			idx = 0
		}
		bins[idx].Count++
	}
	h.Bins = bins
	return h
}
