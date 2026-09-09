// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// TestCapabilitySourceRank pins the numeric total order every capability
// write path (a probe, the vision benchmark, and any future writer) is
// compared through -- WritableCapabilityRows' own doc lists the four bands,
// this is what makes sure the switch actually produces them. Includes the
// rank of a source string nothing in this codebase currently writes: the
// "fail SAFE" clause (an unrecognised source ranks like a probe, not like a
// human) had no test of its own before this one.
func TestCapabilitySourceRank(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   int
	}{
		{"manual", CapabilitySourceManual, 3},
		{"vision_benchmark", CapabilitySourceVisionBenchmark, 2},
		{"llama_cpp_props", CapabilitySourceLlamaCppProps, 1},
		{"legacy", CapabilitySourceLegacy, 1},
		{"absent (empty source, no stored row)", "", 0},
		{"unrecognised source ranks as a probe, not a human", "some_unknown_future_source", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilitySourceRank(tc.source); got != tc.want {
				t.Fatalf("capabilitySourceRank(%q) = %d, want %d", tc.source, got, tc.want)
			}
		})
	}
}

// TestWritableCapabilityRowsRankPairs pins WritableCapabilityRows' precedence
// rule directly, in the package it lives in: permitted iff
// rank(incoming) >= rank(current). Task 3's review (mutation (a)) found this
// rule had NO test in internal/routing -- `go test ./internal/routing/`
// stayed green when the check that became this rule was deleted, because it
// was pinned only indirectly through the two write-path packages that call
// it. This is that missing pin.
//
// It is written against a representative source per rank rather than the
// four named constants pairwise, so it exercises every (incoming rank,
// current rank) combination the doc comment reasons through without growing
// to one row per pair of named sources. Every case reports a verdict that
// DIFFERS from the stored one, so a permitted write here is never confused
// with the change-detection rule (rule 2) passing instead -- that rule gets
// its own tests below.
func TestWritableCapabilityRowsRankPairs(t *testing.T) {
	const (
		rank3        = CapabilitySourceManual
		rank2        = CapabilitySourceVisionBenchmark
		rank1Probe   = CapabilitySourceLlamaCppProps
		rank1Legacy  = CapabilitySourceLegacy
		rank1Unknown = "some_unknown_future_source"
	)

	cases := []struct {
		name      string
		incoming  string
		current   string // "" means no stored row at all (rank 0)
		wantWrite bool
	}{
		// incoming rank 3 (manual) against every current rank -- always writable.
		{"manual over absent (3 vs 0)", rank3, "", true},
		{"manual over probe (3 vs 1)", rank3, rank1Probe, true},
		{"manual over legacy (3 vs 1)", rank3, rank1Legacy, true},
		{"manual over benchmark (3 vs 2)", rank3, rank2, true},
		{"manual over manual (3 vs 3, tie)", rank3, rank3, true},

		// incoming rank 2 (benchmark) -- loses only to manual.
		{"benchmark over absent (2 vs 0)", rank2, "", true},
		{"benchmark over probe (2 vs 1)", rank2, rank1Probe, true},
		{"benchmark over legacy (2 vs 1)", rank2, rank1Legacy, true},
		{"benchmark over benchmark (2 vs 2, tie)", rank2, rank2, true},
		{"benchmark BLOCKED by manual (2 vs 3)", rank2, rank3, false},

		// incoming rank 1 (a probe, a legacy source, or an unrecognised
		// source) -- ties with another rank-1 source (repairs its own or a
		// legacy row's drift), loses to anything ranked higher.
		{"probe over absent (1 vs 0)", rank1Probe, "", true},
		{"probe over its own kind (1 vs 1, tie)", rank1Probe, rank1Probe, true},
		{"probe over legacy (1 vs 1, tie)", rank1Probe, rank1Legacy, true},
		{"legacy over probe (1 vs 1, tie)", rank1Legacy, rank1Probe, true},
		{"unrecognised source over probe (1 vs 1, tie)", rank1Unknown, rank1Probe, true},
		{"probe BLOCKED by benchmark (1 vs 2)", rank1Probe, rank2, false},
		{"probe BLOCKED by manual (1 vs 3)", rank1Probe, rank3, false},

		// rank 0 (an empty incoming source) is not a real caller shape --
		// ValidateCapabilityRow rejects it -- so it is exercised only as a
		// CURRENT (absent-row) rank above, never as an incoming one.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := map[string]CapabilityRow{}
			if tc.current != "" {
				stored[CapabilityVision] = CapabilityRow{
					Capability: CapabilityVision, Verdict: CapabilityNo, Source: tc.current,
				}
			}
			reported := []CapabilityRow{{
				Capability: CapabilityVision, Verdict: CapabilityYes, Source: tc.incoming,
			}}
			got := WritableCapabilityRows(reported, stored)
			gotWrite := len(got) == 1
			if gotWrite != tc.wantWrite {
				t.Fatalf("WritableCapabilityRows(incoming=%q, current=%q) wrote=%v, want %v", tc.incoming, tc.current, gotWrite, tc.wantWrite)
			}
		})
	}
}

// TestWritableCapabilityRowsNilReportedReturnsNil pins the nil-in/nil-out
// shape a caller's `len(rows) == 0` early-return depends on: no verdicts
// reported means no work, and iterating a nil slice must fall straight
// through to the nil zero value of out rather than panicking or allocating.
func TestWritableCapabilityRowsNilReportedReturnsNil(t *testing.T) {
	got := WritableCapabilityRows(nil, map[string]CapabilityRow{
		CapabilityVision: {Capability: CapabilityVision, Verdict: CapabilityNo, Source: CapabilitySourceLlamaCppProps},
	})
	if got != nil {
		t.Fatalf("WritableCapabilityRows(nil, ...) = %#v, want nil", got)
	}
}

// TestWritableCapabilityRowsReturnsNilNotEmptySlice is
// TestWritableCapabilityRowsNilReportedReturnsNil's sibling for the OTHER way
// to end up with nothing writable: a non-nil reported slice whose only entry
// is outranked. The doc comment promises nil, "not an empty slice" -- this
// pins that stronger, documented guarantee (a caller checking `rows != nil`
// instead of `len(rows) != 0` must also be correct).
func TestWritableCapabilityRowsReturnsNilNotEmptySlice(t *testing.T) {
	stored := map[string]CapabilityRow{
		CapabilityVision: {Capability: CapabilityVision, Verdict: CapabilityNo, Source: CapabilitySourceManual},
	}
	reported := []CapabilityRow{{Capability: CapabilityVision, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps}}
	got := WritableCapabilityRows(reported, stored)
	if got != nil {
		t.Fatalf("WritableCapabilityRows = %#v (len %d), want nil, not merely empty", got, len(got))
	}
}

// TestWritableCapabilityRowsAgreeingLegacyRowIsNotRewritten pins rule 2
// (change detection) specifically for a LEGACY row, a case the review named
// as uncovered: a legacy row's rank (1) does not block an incoming probe's
// rank-1 write under rule 1 (a tie is writable), so without rule 2 an
// agreeing legacy verdict would be pointlessly re-stamped with a fresh
// CheckedAt on every sample. The comparison is verdict-only, not
// source-or-CheckedAt, so this would also fail if rule 2 were changed to
// compare anything else.
func TestWritableCapabilityRowsAgreeingLegacyRowIsNotRewritten(t *testing.T) {
	stored := map[string]CapabilityRow{
		CapabilityVision: {Capability: CapabilityVision, Verdict: CapabilityYes, Source: CapabilitySourceLegacy},
	}
	reported := []CapabilityRow{{Capability: CapabilityVision, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps}}
	got := WritableCapabilityRows(reported, stored)
	if got != nil {
		t.Fatalf("WritableCapabilityRows = %#v, want nil -- an agreeing legacy verdict must not be rewritten even though rank permits it", got)
	}
}
