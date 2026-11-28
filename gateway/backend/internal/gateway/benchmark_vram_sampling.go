// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"cmp"
	"op-ai-gateway/internal/routing"
	"slices"
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

// vramGPUByKey indexes one telemetry sample's GPU rows by their GPU key
// (vendor, index) -- the same key the spec's GPU rows, the per-GPU budgets and
// the agent's measurer all use. A mixed NVIDIA + AMD host reports an index 0
// of each vendor, so the index alone would drop a card. A repeated key keeps
// the first row: the sample is a snapshot of distinct cards, and a duplicate
// is malformed input rather than an update.
//
// The sample's vendors are taken as they are: a run resolves a vendor-less
// sample to the server's legacy vendor before it reads it
// (vramResolveSampleVendors).
func vramGPUByKey(sample routing.TelemetrySample) map[routing.GPUKey]routing.GPUSample {
	out := make(map[routing.GPUKey]routing.GPUSample, len(sample.GPUs))
	for _, gpu := range sample.GPUs {
		key := routing.GPUKey{Vendor: gpu.Vendor, Index: gpu.Index}
		if _, seen := out[key]; seen {
			continue
		}
		out[key] = gpu
	}
	return out
}

// vramResolveSampleVendors returns sample with every GPU that carries no
// vendor -- an agent without gpu_vendor_identity reports none -- given the
// server's stored legacy vendor (resolvedGPUKey), so the run reads the cards
// by the keys the spec's rows carry. sample itself is shared with every other
// reader of the live ring and is never modified: the GPUs are copied when one
// changes. An empty legacyVendor (never backfilled) changes nothing.
func vramResolveSampleVendors(sample routing.TelemetrySample, legacyVendor string) routing.TelemetrySample {
	if legacyVendor == "" || !slices.ContainsFunc(sample.GPUs, func(g routing.GPUSample) bool { return g.Vendor == "" }) {
		return sample
	}
	gpus := slices.Clone(sample.GPUs)
	for i := range gpus {
		if gpus[i].Vendor == "" {
			gpus[i].Vendor = legacyVendor
		}
	}
	sample.GPUs = gpus
	return sample
}

// vramResolveSpecGPUVendors is vramResolveSampleVendors for the spec's GPU
// rows: a row without a vendor stands for the server's legacy vendor, which is
// the card the agent runs it on. rows is not modified.
func vramResolveSpecGPUVendors(rows []routing.RuntimeSpecGPU, legacyVendor string) []routing.RuntimeSpecGPU {
	if legacyVendor == "" || !slices.ContainsFunc(rows, func(r routing.RuntimeSpecGPU) bool { return r.GPUVendor == "" }) {
		return rows
	}
	out := slices.Clone(rows)
	for i := range out {
		if out[i].GPUVendor == "" {
			out[i].GPUVendor = legacyVendor
		}
	}
	return out
}

// sortGPUKeys orders keys by vendor, then index: the one stable order every
// list of GPU keys in a VRAM report is given.
func sortGPUKeys(keys []routing.GPUKey) {
	slices.SortFunc(keys, func(a, b routing.GPUKey) int {
		return cmp.Or(cmp.Compare(a.Vendor, b.Vendor), cmp.Compare(a.Index, b.Index))
	})
}

// vramWindowStable reports whether every watched card held still across the
// whole window: present in EVERY sample, and varying by no more than its own
// tolerance between the window's smallest and largest reading.
//
// Three refusals, each of which would otherwise turn movement into a number:
// a window shorter than vramStabilityWindow (not enough evidence yet), a
// watched card missing from any sample (the run cannot difference what it
// cannot see -- vramCardSpan reports that one), and an EMPTY watched set
// (nothing was checked, so nothing is stable -- the same non-vacuous
// discipline vramIsolationConfirmed applies to an empty enumeration).
//
// Every card is judged on ITS OWN tolerance, which is why the span and the
// comparison are one step per card rather than a shared bound: see
// vramCardToleranceMB for what a shared bound discards.
func vramWindowStable(window []routing.TelemetrySample, watched []routing.GPUKey) bool {
	if len(window) < vramStabilityWindow || len(watched) == 0 {
		return false
	}
	byKey := make([]map[routing.GPUKey]routing.GPUSample, 0, len(window))
	for _, sample := range window {
		byKey = append(byKey, vramGPUByKey(sample))
	}
	for _, key := range watched {
		lo, hi, total, complete := vramCardSpan(byKey, key)
		if !complete || hi-lo > vramStabilityTolerance(total) {
			return false
		}
	}
	return true
}

// vramCardSpan is one card's spread across an indexed window: its smallest and
// largest used-bytes reading, and the largest total it reported.
//
// complete is false as soon as ONE sample does not carry the card at all --
// the run cannot difference what it cannot see, and a card that vanished
// mid-window says nothing about whether it held still. The caller refuses on
// it rather than judging the readings that did arrive.
//
// The LARGEST reported total wins, so a sample that reported no total at all
// (a 0) cannot shrink the tolerance the caller then applies -- which is the
// same direction vramStabilityTolerance takes for an unknown total, and the
// only one that does not turn a missing field into a stricter gate.
func vramCardSpan(byKey []map[routing.GPUKey]routing.GPUSample, key routing.GPUKey) (lo, hi, total int64, complete bool) {
	first := true
	for _, snapshot := range byKey {
		gpu, ok := snapshot[key]
		if !ok {
			return 0, 0, 0, false
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
	return lo, hi, total, true
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
// because GPUSample.UUID is populated only by the NVIDIA parse and by the AMD
// parse of rocm-smi 4.x. On the host classes the delta strategy exists to
// serve (Apple via ioreg, AMD with pre-4.0 rocm-smi) it is always empty, so a
// UUID-only detector would silently verify nothing there.
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

// vramWatchedKeys decides which cards a run reads, and whether a number
// measured on them has anywhere to be applied.
//
// The spec's DECLARED GPU rows are the key set admission actually uses, so
// a number measured there lands on a row the operator can act on
// (attributable). A spec that declares NO rows has no such row anywhere: the
// run then watches every card the sample carries and marks the result
// unattributable, because there is nothing to attribute it to -- not because
// the number is worth less.
//
// Returns de-duplicated keys in sortGPUKeys order. An empty result is what
// makes a GPU-less host inconclusive rather than zero: there is nothing to
// difference. Both inputs are taken with their vendors as they are; the run
// resolves vendor-less ones to the legacy vendor first
// (vramResolveSpecGPUVendors, vramResolveSampleVendors).
func vramWatchedKeys(specGPUs []routing.RuntimeSpecGPU, sample routing.TelemetrySample) (keys []routing.GPUKey, attributable bool) {
	seen := map[routing.GPUKey]struct{}{}
	add := func(key routing.GPUKey) {
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(specGPUs) > 0 {
		for _, row := range specGPUs {
			add(row.Key())
		}
		sortGPUKeys(keys)
		return keys, true
	}
	for _, gpu := range sample.GPUs {
		add(routing.GPUKey{Vendor: gpu.Vendor, Index: gpu.Index})
	}
	sortGPUKeys(keys)
	return keys, false
}

// vramFloorMB is the floor the HEADLINE delta must clear to be a measurement
// at all: the largest per-card tolerance among the watched cards, in MB. A
// headline below that could be entirely noise on the noisiest watched card,
// and 0 means UNKNOWN everywhere else in this feature, so it must mean it
// here too. With nothing watched the floor is the absolute one, so a sub-floor
// number can never pass by default.
func vramFloorMB(watched []routing.GPUKey, sample routing.TelemetrySample) int {
	byKey := vramGPUByKey(sample)
	floor := vramStabilityFloorBytes
	for _, key := range watched {
		if tol := vramStabilityTolerance(byKey[key].MemTotalBytes); tol > floor {
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
