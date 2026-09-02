// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"sort"
	"strconv"
	"strings"
)

// vramBytesPerMB is the byte-to-MB conversion this whole feature uses. It is
// 1 MiB, NOT 10^6, because that is what the number it is compared against
// means everywhere else: the agent's per-process measurer writes its
// pid -> gpuIndex -> MB map in MiB, and so does every spec's VRAMMB estimate
// and every per-GPU budget. Two units for one column is a wrong number, not a
// rounding difference.
const vramBytesPerMB int64 = 1024 * 1024

// The stability gate's three knobs. THEY ARE REASONED, NOT MEASURED: they
// were chosen against the agent's ~1 s telemetry cadence and the observation
// that a delta's noise floor is other processes rather than sampling
// quantization (1 MiB on NVIDIA). They must be validated on a real multi-GPU
// host before they are treated as settled.
//
// Vars rather than consts so a test can shorten them, following the
// coldLoadPollGap / coldLoadMaxWait precedent in benchmark_runner.go.
var (
	// vramStabilityWindow is K: how many CONSECUTIVE samples must agree
	// before a phase is treated as having produced a reading. 3 is ~3 s at
	// the default 1 s agent cadence.
	vramStabilityWindow = 3
	// vramStabilityFloorBytes is the absolute per-card tolerance. 1 % of a
	// small card is below the jitter a neighbouring process makes, so the
	// percentage alone would make a quiet 2 GiB card unmeasurable.
	vramStabilityFloorBytes = 64 * vramBytesPerMB
	// vramStabilityPct is the relative per-card tolerance, as a fraction of
	// the card's total memory.
	vramStabilityPct = 0.01
)

// vramStabilityTolerance is the per-card tolerance for a card of totalBytes:
// the larger of the relative and absolute floors. An unknown or nonsensical
// total (0, negative -- a sample that reported no total) degrades to the
// absolute floor rather than to zero tolerance, which would make every window
// unstable and every run inconclusive.
func vramStabilityTolerance(totalBytes int64) int64 {
	tol := vramStabilityFloorBytes
	if totalBytes > 0 {
		if relative := int64(float64(totalBytes) * vramStabilityPct); relative > tol {
			tol = relative
		}
	}
	return tol
}

// vramGPUByIndex indexes one telemetry sample's GPU rows by their HOST index
// -- the same index space the spec's GPU rows, the per-GPU budgets and the
// agent's measurer all use. A repeated index keeps the first row: the sample
// is a snapshot of distinct cards, and a duplicate is malformed input rather
// than an update.
func vramGPUByIndex(sample routing.TelemetrySample) map[int]routing.GPUSample {
	out := make(map[int]routing.GPUSample, len(sample.GPUs))
	for _, gpu := range sample.GPUs {
		if _, seen := out[gpu.Index]; seen {
			continue
		}
		out[gpu.Index] = gpu
	}
	return out
}

// vramWindowStable reports whether every watched card held still across the
// whole window: present in EVERY sample, and varying by no more than its own
// tolerance between the window's smallest and largest reading.
//
// Three refusals, each of which would otherwise turn movement into a number:
// a window shorter than vramStabilityWindow (not enough evidence yet), a
// watched card missing from any sample (the run cannot difference what it
// cannot see), and an EMPTY watched set (nothing was checked, so nothing is
// stable -- the same non-vacuous discipline vramIsolationConfirmed applies to
// an empty enumeration).
func vramWindowStable(window []routing.TelemetrySample, watched []int) bool {
	if len(window) < vramStabilityWindow || len(watched) == 0 {
		return false
	}
	byIndex := make([]map[int]routing.GPUSample, 0, len(window))
	for _, sample := range window {
		byIndex = append(byIndex, vramGPUByIndex(sample))
	}
	for _, index := range watched {
		var lo, hi, total int64
		first := true
		for _, snapshot := range byIndex {
			gpu, ok := snapshot[index]
			if !ok {
				return false // a watched card vanished mid-window
			}
			if first {
				lo, hi, total, first = gpu.MemUsedBytes, gpu.MemUsedBytes, gpu.MemTotalBytes, false
				continue
			}
			if gpu.MemUsedBytes < lo {
				lo = gpu.MemUsedBytes
			}
			if gpu.MemUsedBytes > hi {
				hi = gpu.MemUsedBytes
			}
			if gpu.MemTotalBytes > total {
				total = gpu.MemTotalBytes
			}
		}
		if hi-lo > vramStabilityTolerance(total) {
			return false
		}
	}
	return true
}

// vramDeltaMB is strategy (b)'s arithmetic for one card: used_after minus
// used_before, in MB, floored at 0. A NEGATIVE difference is UNKNOWN, never a
// negative allocation -- a card that freed memory across the window says
// something about its neighbours, nothing about the model.
func vramDeltaMB(beforeBytes, afterBytes int64) int {
	if afterBytes <= beforeBytes {
		return 0
	}
	return int((afterBytes - beforeBytes) / vramBytesPerMB)
}

// vramUsedMB converts a card's used bytes to MB, floored at 0.
func vramUsedMB(usedBytes int64) int {
	if usedBytes <= 0 {
		return 0
	}
	return int(usedBytes / vramBytesPerMB)
}

// vramFingerprintOf records what identified the card a number is attributed
// to, and NAMES which field did it. A stored VRAM number attributed to index 1
// after the cards were renumbered is worse than no number, so the result
// carries the strongest available identifier -- and says how strong it is,
// because GPUSample.UUID is populated only by the NVIDIA parse. On the two
// host classes the delta strategy exists to serve (AMD via ROCm, Apple via
// ioreg) it is always empty, so a UUID-only detector would silently verify
// nothing there.
//
// name+total catches a swap between UNLIKE cards only: two identical cards
// trading indices are indistinguishable, which is why the kind travels with
// the value instead of the portal rendering a bare "verified".
func vramFingerprintOf(gpu routing.GPUSample) (fingerprint, kind string) {
	if uuid := strings.TrimSpace(gpu.UUID); uuid != "" {
		return uuid, vramFingerprintUUID
	}
	name := strings.TrimSpace(gpu.Name)
	total := ""
	if gpu.MemTotalBytes > 0 {
		total = strconv.Itoa(vramUsedMB(gpu.MemTotalBytes)) + " MB"
	}
	switch {
	case name != "" && total != "":
		return name + " / " + total, vramFingerprintNameTotal
	case name != "":
		return name, vramFingerprintNameTotal
	case total != "":
		return total, vramFingerprintNameTotal
	default:
		return "", ""
	}
}

// vramWatchedIndexes decides which cards a run reads, and whether a number
// measured on them has anywhere to be applied.
//
// The spec's DECLARED GPU rows are the index set admission actually uses, so
// a number measured there lands on a row the operator can act on
// (attributable). A spec that declares NO rows has no such row anywhere: the
// run then watches every card the sample carries and marks the result
// unattributable, because there is nothing to attribute it to -- not because
// the number is worth less.
//
// Returns ascending, de-duplicated indexes. An empty result is what makes a
// GPU-less host inconclusive rather than zero: there is nothing to difference.
func vramWatchedIndexes(specGPUs []routing.RuntimeSpecGPU, sample routing.TelemetrySample) (indexes []int, attributable bool) {
	seen := map[int]struct{}{}
	if len(specGPUs) > 0 {
		for _, row := range specGPUs {
			if _, dup := seen[row.GPUIndex]; dup {
				continue
			}
			seen[row.GPUIndex] = struct{}{}
			indexes = append(indexes, row.GPUIndex)
		}
		sort.Ints(indexes)
		return indexes, true
	}
	for _, gpu := range sample.GPUs {
		if _, dup := seen[gpu.Index]; dup {
			continue
		}
		seen[gpu.Index] = struct{}{}
		indexes = append(indexes, gpu.Index)
	}
	sort.Ints(indexes)
	return indexes, false
}

// vramFloorMB is the floor the HEADLINE delta must clear to be a measurement
// at all: the largest per-card tolerance among the watched cards, in MB. A
// headline below that could be entirely noise on the noisiest watched card,
// and 0 means UNKNOWN everywhere else in this feature, so it must mean it
// here too. With nothing watched the floor is the absolute one, so a sub-floor
// number can never pass by default.
func vramFloorMB(watched []int, sample routing.TelemetrySample) int {
	byIndex := vramGPUByIndex(sample)
	floor := vramStabilityFloorBytes
	for _, index := range watched {
		if tol := vramStabilityTolerance(byIndex[index].MemTotalBytes); tol > floor {
			floor = tol
		}
	}
	return int(floor / vramBytesPerMB)
}

// vramHeadlineDeltaMB is the model's TOTAL marginal cost across the cards it
// was measured on -- the quantity the floor gate is applied to. A model split
// over two cards costs the sum of both deltas, so gating on one card alone
// would call a real split allocation sub-floor.
func vramHeadlineDeltaMB(items []VRAMGPUItem) int {
	total := 0
	for _, item := range items {
		total += item.DeltaMB
	}
	return total
}
