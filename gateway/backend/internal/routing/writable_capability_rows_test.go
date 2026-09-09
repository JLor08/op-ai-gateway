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
// CheckedAt on every sample. Equal verdict AND equal rank is the exact
// condition -- see TestWritableCapabilityRowsUpgradesTheRankOfAnAgreeingRow
// for the other half, where an agreeing verdict at a HIGHER rank must write.
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

// TestWritableCapabilityRowsUpgradesTheRankOfAnAgreeingRow is rule 2's other
// half: a write whose verdict AGREES with the stored one must still land when
// its rank is HIGHER, because the rank is itself a fact only a write can
// change.
//
// The case that matters is the vision benchmark confirming what a probe or a
// migrated legacy row already asserted. Comparing the verdict alone dropped
// that write, with two consequences: the per-capability tooltip -- the
// headline gain of the capability table -- attributed a genuinely MEASURED
// verdict to a probe re-reading a document, and the row stayed at rank 1, so
// the next probe could still overwrite a real measurement.
//
// The equal-rank rows are the guard against solving that by writing always:
// a repeat from the same writer, and a probe over an agreeing legacy row,
// must both stay silent -- a build capability reports the same verdict every
// second for its whole life. A rank can only ever RISE for one (mapping,
// capability), at most twice, so this cannot amplify.
func TestWritableCapabilityRowsUpgradesTheRankOfAnAgreeingRow(t *testing.T) {
	cases := []struct {
		name      string
		incoming  string
		current   string
		wantWrite bool
		why       string
	}{
		{
			name: "benchmark confirms a probe row", incoming: CapabilitySourceVisionBenchmark,
			current: CapabilitySourceLlamaCppProps, wantWrite: true,
			why: "a real measurement of a verdict only /props had asserted -- the row must stop being probe-overwritable",
		},
		{
			name: "benchmark confirms a legacy row", incoming: CapabilitySourceVisionBenchmark,
			current: CapabilitySourceLegacy, wantWrite: true,
			why: "same, for a verdict migration 78 inherited from a column whose writer is unknowable",
		},
		{
			name: "manual confirms a benchmark row", incoming: CapabilitySourceManual,
			current: CapabilitySourceVisionBenchmark, wantWrite: true,
			why: "an operator agreeing with the measurement still makes the verdict permanently theirs (rank 3)",
		},
		{
			name: "benchmark repeats itself", incoming: CapabilitySourceVisionBenchmark,
			current: CapabilitySourceVisionBenchmark, wantWrite: false,
			why: "equal rank, equal verdict: nothing left to record",
		},
		{
			name: "probe repeats itself", incoming: CapabilitySourceLlamaCppProps,
			current: CapabilitySourceLlamaCppProps, wantWrite: false,
			why: "the amplification this rule exists to stop -- one write per sample, forever",
		},
		{
			name: "probe agrees with a legacy row", incoming: CapabilitySourceLlamaCppProps,
			current: CapabilitySourceLegacy, wantWrite: false,
			why: "legacy and a probe are the SAME rank, so there is no upgrade to record",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := map[string]CapabilityRow{
				CapabilityVision: {Capability: CapabilityVision, Verdict: CapabilityYes, Source: tc.current},
			}
			// The SAME verdict as the stored row, deliberately: this test is
			// only about rule 2, so rule 1 must be the only other thing that
			// could speak, and it permits every pair here.
			reported := []CapabilityRow{{Capability: CapabilityVision, Verdict: CapabilityYes, Source: tc.incoming}}
			got := WritableCapabilityRows(reported, stored)
			if gotWrite := len(got) == 1; gotWrite != tc.wantWrite {
				t.Fatalf("incoming %s over stored %s (verdicts agree) wrote=%v, want %v -- %s",
					tc.incoming, tc.current, gotWrite, tc.wantWrite, tc.why)
			}
		})
	}
}

// TestWritableCapabilityRowsKeepsTheFirstRowPerCapability pins rule 0. The
// capability vocabulary is open on purpose, so a reported name CAN collide
// with one the code reasons about -- Ollama's manifest-declared capabilities
// literally include "vision", and the runtime write-back carries a dedicated
// live-progress verdict beside an agent's open verdict list. Both producers
// emit their STRUCTURED verdicts first, so keeping the first occurrence is
// what makes "structured beats unstructured" a rule rather than an accident.
//
// Before rule 0 both rows passed and both reached the store, where the upsert
// loop's ordering made the LAST one win -- the opposite outcome, decided by a
// loop in another package rather than by anything anyone had stated.
func TestWritableCapabilityRowsKeepsTheFirstRowPerCapability(t *testing.T) {
	// A structured "no" followed by an open-vocabulary "yes" for the same
	// name, with nothing on file: exactly the Ollama collision.
	reported := []CapabilityRow{
		{Capability: CapabilityVision, Verdict: CapabilityNo, Source: CapabilitySourceLlamaCppProps},
		{Capability: CapabilityVision, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps},
	}
	got := WritableCapabilityRows(reported, map[string]CapabilityRow{})
	if len(got) != 1 {
		t.Fatalf("WritableCapabilityRows returned %d rows (%+v), want exactly 1 -- a duplicated capability name must never reach the upsert twice", len(got), got)
	}
	if got[0].Verdict != CapabilityNo {
		t.Fatalf("the surviving row is %+v, want the FIRST occurrence (the structured no) -- an open-vocabulary name must not override a structured verdict", got[0])
	}

	// The first occurrence claims the name even when it is itself dropped by
	// rule 2. Otherwise a later duplicate could still write, and "the first
	// occurrence decides" would hold only when the first one happened to be
	// writable.
	stored := map[string]CapabilityRow{
		CapabilityVision: {Capability: CapabilityVision, Verdict: CapabilityNo, Source: CapabilitySourceLlamaCppProps},
	}
	if got := WritableCapabilityRows(reported, stored); got != nil {
		t.Fatalf("WritableCapabilityRows = %+v, want nil -- the first occurrence agreed with the stored row, so the duplicate behind it must not write either", got)
	}

	// Distinct names are untouched by rule 0.
	two := []CapabilityRow{
		{Capability: CapabilityVision, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps},
		{Capability: CapabilityTools, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps},
	}
	if got := WritableCapabilityRows(two, map[string]CapabilityRow{}); len(got) != 2 {
		t.Fatalf("WritableCapabilityRows returned %d rows for two DIFFERENT capabilities (%+v), want 2", len(got), got)
	}
}
