// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

const oneMiB int64 = 1024 * 1024

// vramSample builds one telemetry sample carrying the given per-index
// (used, total) byte pairs, in MiB, so a test reads in the units an operator
// does.
func vramSample(gpus ...routing.GPUSample) routing.TelemetrySample {
	return routing.TelemetrySample{ServerID: "srv1", GPUs: gpus}
}

func vramGPU(index int, usedMiB, totalMiB int64) routing.GPUSample {
	return routing.GPUSample{Index: index, MemUsedBytes: usedMiB * oneMiB, MemTotalBytes: totalMiB * oneMiB}
}

// TestVRAMStabilityTolerance pins the noise floor: 1 % of the card, but never
// below the absolute floor, because 1 % of a small card is smaller than the
// jitter another process makes.
func TestVRAMStabilityTolerance(t *testing.T) {
	cases := []struct {
		name       string
		totalBytes int64
		want       int64
	}{
		// 1 % of 24 GiB, exactly: 25769803776/100 truncated to bytes.
		{"a 24 GiB card is governed by the percentage", 24576 * oneMiB, 24576 * oneMiB / 100},
		{"a small card is governed by the absolute floor", 2048 * oneMiB, 64 * oneMiB},
		{"an unknown total still yields the floor", 0, 64 * oneMiB},
		{"a negative total still yields the floor", -1, 64 * oneMiB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramStabilityTolerance(tc.totalBytes); got != tc.want {
				t.Fatalf("vramStabilityTolerance(%d) = %d, want %d", tc.totalBytes, got, tc.want)
			}
		})
	}
}

// TestVRAMWindowStable is the gate that decides whether a phase produced a
// reading at all. A window that is too short, that is missing a watched card,
// or that drifts by more than the tolerance is NOT a reading -- reporting
// nothing is the contract, never a number measured through movement.
func TestVRAMWindowStable(t *testing.T) {
	steady := []routing.TelemetrySample{
		vramSample(vramGPU(0, 500, 24576), vramGPU(1, 300, 24576)),
		vramSample(vramGPU(0, 501, 24576), vramGPU(1, 300, 24576)),
		vramSample(vramGPU(0, 500, 24576), vramGPU(1, 302, 24576)),
	}
	drifting := []routing.TelemetrySample{
		vramSample(vramGPU(0, 500, 24576)),
		vramSample(vramGPU(0, 900, 24576)), // +400 MiB > the 245.76 MiB tolerance
		vramSample(vramGPU(0, 950, 24576)),
	}
	cases := []struct {
		name    string
		window  []routing.TelemetrySample
		watched []int
		want    bool
	}{
		{"a steady window over both watched cards", steady, []int{0, 1}, true},
		{"a steady window over one watched card", steady, []int{1}, true},
		{"a drifting card is not stable", drifting, []int{0}, false},
		{"a window shorter than K is never stable", steady[:2], []int{0}, false},
		{
			name:    "a watched card missing from one sample is not stable",
			window:  []routing.TelemetrySample{steady[0], vramSample(vramGPU(0, 500, 24576)), steady[2]},
			watched: []int{0, 1},
			want:    false,
		},
		{"an empty watched set proves nothing", steady, nil, false},
		{"an empty window proves nothing", nil, []int{0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramWindowStable(tc.window, tc.watched); got != tc.want {
				t.Fatalf("vramWindowStable(%d samples, %v) = %v, want %v", len(tc.window), tc.watched, got, tc.want)
			}
		})
	}
}

// TestVRAMDeltaMB pins the arithmetic and the one rule that makes a delta
// honest: a negative difference is UNKNOWN (0), never a negative allocation.
func TestVRAMDeltaMB(t *testing.T) {
	cases := []struct {
		name          string
		before, after int64
		want          int
	}{
		{"a whole-MiB allocation", 500 * oneMiB, 21500 * oneMiB, 21000},
		{"a partial MiB floors down", 0, 3*oneMiB + 700_000, 3},
		{"an unchanged card is zero", 700 * oneMiB, 700 * oneMiB, 0},
		{"a card that FREED memory is unknown, not negative", 900 * oneMiB, 500 * oneMiB, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramDeltaMB(tc.before, tc.after); got != tc.want {
				t.Fatalf("vramDeltaMB(%d, %d) = %d, want %d", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// TestVRAMFingerprintOf is test-plan item 10: the fingerprint records the
// STRONGEST AVAILABLE field and names which, because GPUSample.UUID is
// NVIDIA-only -- so a UUID-only drift detector would be empty on exactly the
// two host classes (AMD, Apple) the delta strategy exists to serve. A sample
// with neither claims nothing at all.
func TestVRAMFingerprintOf(t *testing.T) {
	cases := []struct {
		name     string
		sample   routing.GPUSample
		wantFP   string
		wantKind string
	}{
		{
			name:     "an NVIDIA sample is verified by uuid",
			sample:   routing.GPUSample{Index: 0, Name: "NVIDIA GeForce RTX 4090", UUID: "GPU-1234abcd", MemTotalBytes: 24576 * oneMiB},
			wantFP:   "GPU-1234abcd",
			wantKind: vramFingerprintUUID,
		},
		{
			name:     "a ROCm sample has no uuid and falls back to name plus total",
			sample:   routing.GPUSample{Index: 1, Name: "Radeon RX 7900 XTX", MemTotalBytes: 24576 * oneMiB},
			wantFP:   "Radeon RX 7900 XTX / 24576 MB",
			wantKind: vramFingerprintNameTotal,
		},
		{
			name:     "a total size with no name still identifies the card weakly",
			sample:   routing.GPUSample{Index: 0, MemTotalBytes: 16384 * oneMiB},
			wantFP:   "16384 MB",
			wantKind: vramFingerprintNameTotal,
		},
		{
			name:     "a sample with neither claims nothing",
			sample:   routing.GPUSample{Index: 2},
			wantFP:   "",
			wantKind: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp, kind := vramFingerprintOf(tc.sample)
			if fp != tc.wantFP || kind != tc.wantKind {
				t.Fatalf("vramFingerprintOf(%+v) = (%q, %q), want (%q, %q)", tc.sample, fp, kind, tc.wantFP, tc.wantKind)
			}
		})
	}
}

// TestVRAMWatchedIndexes pins which cards a run reads, and the attribution
// that rides with the choice: the spec's declared rows are the index set
// admission actually uses, so a number measured there has a row to be applied
// to. A spec that declares none has no such row anywhere, so every index is
// watched and every result is marked unattributable.
func TestVRAMWatchedIndexes(t *testing.T) {
	sample := vramSample(vramGPU(0, 100, 24576), vramGPU(1, 100, 24576), vramGPU(2, 100, 24576))

	idx, attributable := vramWatchedIndexes([]routing.RuntimeSpecGPU{{GPUIndex: 2}, {GPUIndex: 0}}, sample)
	if !attributable {
		t.Fatal("declared GPU rows must be attributable")
	}
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 2 {
		t.Fatalf("declared indexes = %v, want [0 2] (ascending)", idx)
	}

	idx, attributable = vramWatchedIndexes(nil, sample)
	if attributable {
		t.Fatal("a spec with no declared GPU rows has no row to attribute a number to")
	}
	if len(idx) != 3 || idx[0] != 0 || idx[2] != 2 {
		t.Fatalf("undeclared indexes = %v, want every index in the sample", idx)
	}

	// A GPU-less sample yields nothing to watch, which is what makes the
	// GPU-less host inconclusive rather than zero.
	if idx, _ := vramWatchedIndexes(nil, routing.TelemetrySample{ServerID: "srv1"}); len(idx) != 0 {
		t.Fatalf("a sample with no GPUs yielded %v, want nothing to watch", idx)
	}
}

// TestVRAMFloorMB pins the floor the headline delta must clear: the largest
// per-card tolerance among the watched cards. A headline below that could be
// entirely noise on the noisiest card, and 0 means UNKNOWN everywhere else in
// this feature.
func TestVRAMFloorMB(t *testing.T) {
	sample := vramSample(vramGPU(0, 100, 2048), vramGPU(1, 100, 49152))
	// Card 1's 1 % (480 MiB) dominates card 0's absolute floor (64 MiB).
	if got, want := vramFloorMB([]int{0, 1}, sample), 491; got != want {
		t.Fatalf("vramFloorMB([0 1]) = %d, want %d", got, want)
	}
	if got, want := vramFloorMB([]int{0}, sample), 64; got != want {
		t.Fatalf("vramFloorMB([0]) = %d, want %d", got, want)
	}
	// No watched card at all: the floor is the absolute one, so a bogus
	// sub-floor number can never pass by default.
	if got, want := vramFloorMB(nil, sample), 64; got != want {
		t.Fatalf("vramFloorMB(nil) = %d, want %d", got, want)
	}
}

// TestVRAMHeadlineDeltaMB pins what the floor gate is applied to: the model's
// TOTAL marginal cost across the cards it was measured on. A model split over
// two cards costs the sum, and gating on one card alone would call a real
// split allocation sub-floor.
func TestVRAMHeadlineDeltaMB(t *testing.T) {
	items := []VRAMGPUItem{{Index: 0, DeltaMB: 11000}, {Index: 1, DeltaMB: 11000}}
	if got, want := vramHeadlineDeltaMB(items), 22000; got != want {
		t.Fatalf("vramHeadlineDeltaMB = %d, want %d", got, want)
	}
	if got := vramHeadlineDeltaMB(nil); got != 0 {
		t.Fatalf("vramHeadlineDeltaMB(nil) = %d, want 0", got)
	}
}
