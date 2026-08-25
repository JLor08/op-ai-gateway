// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// decodePreBranchModelOverrideMap is a VERBATIM copy of DecodeModelOverrideMap
// as it stood before this branch — the decoder an operator who rolls the binary
// back would be running. It is duplicated here rather than referenced because
// the real one no longer exists in this tree, and the whole point of the tests
// below is to hold the new encoder against the OLD reader.
//
// Its shape is what makes rollback fragile: json.Unmarshal into
// map[string]string fails on the FIRST object-valued row and returns nil for
// the WHOLE map, not just that row.
func decodePreBranchModelOverrideMap(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func TestDecodeModelOverrideRulesLegacyStringMap(t *testing.T) {
	// Rows written before this feature hold a plain requested->model map. They
	// must keep working and must default both switches to false, so no existing
	// token changes behavior.
	got := DecodeModelOverrideRules(`{"gpt-4o":"qwen3-32b"}`)
	want := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy decode = %#v, want %#v", got, want)
	}
}

func TestDecodeModelOverrideRulesObjectForm(t *testing.T) {
	got := DecodeModelOverrideRules(`{"gpt-4o":{"to":"qwen3-32b","offer":true,"hide_target":true}}`)
	want := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("object decode = %#v, want %#v", got, want)
	}
}

func TestDecodeModelOverrideRulesBlankAndMalformed(t *testing.T) {
	// Malformed is "none", never an error: a bad row must not break token auth.
	for _, in := range []string{"", "   ", "not json", "[1,2]"} {
		if got := DecodeModelOverrideRules(in); got != nil {
			t.Fatalf("DecodeModelOverrideRules(%q) = %#v, want nil", in, got)
		}
	}
}

func TestDecodeModelOverrideRulesDropsEmptyTarget(t *testing.T) {
	// A rule with no target cannot route anywhere; keeping it would advertise an
	// alias that resolves to nothing.
	if got := DecodeModelOverrideRules(`{"a":{"to":"  ","offer":true},"b":"x"}`); !reflect.DeepEqual(
		got, map[string]ModelOverrideRule{"b": {To: "x"}}) {
		t.Fatalf("drop-empty decode = %#v", got)
	}
}

func TestEncodeModelOverrideRulesRoundTrip(t *testing.T) {
	in := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true}}
	if got := DecodeModelOverrideRules(EncodeModelOverrideRules(in)); !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %#v, want %#v", got, in)
	}
}

func TestEncodeModelOverrideRulesEmptyIsBlank(t *testing.T) {
	// "no entries" round-trips as the empty string, not "{}" — the column default.
	if got := EncodeModelOverrideRules(nil); got != "" {
		t.Fatalf("EncodeModelOverrideRules(nil) = %q, want \"\"", got)
	}
}

// --- rollback compatibility of the WRITE shape -----------------------------
//
// The read side accepts both shapes, so the only thing that decides whether a
// downgrade is survivable is which shape the encoder WRITES. These four tests
// pin that decision in both directions.

// TestEncodeWritesLegacyFormWhenNoSwitchIsUsed: a map whose rows use neither
// switch must come out byte-identical to what the pre-branch encoder wrote, so
// a deployment that never touches the new switches is fully downgradable.
func TestEncodeWritesLegacyFormWhenNoSwitchIsUsed(t *testing.T) {
	in := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}, "o3": {To: "coder"}}
	got := EncodeModelOverrideRules(in)
	want := `{"gpt-4o":"qwen3-32b","o3":"coder"}`
	if got != want {
		t.Fatalf("encoded = %s, want the legacy string form %s", got, want)
	}
	// And it still round-trips through the CURRENT decoder unchanged, both
	// switches false.
	if back := DecodeModelOverrideRules(got); !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip = %#v, want %#v", back, in)
	}
}

// TestEncodeWritesObjectFormOnlyForSwitchedRows: the object form is per ROW,
// not per map. One row using a switch must not drag the untouched rows out of
// the legacy shape with it — those rows survive a rollback on their own only
// while they stay strings.
func TestEncodeWritesObjectFormOnlyForSwitchedRows(t *testing.T) {
	in := map[string]ModelOverrideRule{
		"plain":  {To: "qwen3-32b"},
		"offers": {To: "qwen3-32b", Offer: true},
		"hides":  {To: "coder", HideTarget: true},
	}
	got := EncodeModelOverrideRules(in)
	want := `{"hides":{"to":"coder","hide_target":true},"offers":{"to":"qwen3-32b","offer":true},"plain":"qwen3-32b"}`
	if got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
	if back := DecodeModelOverrideRules(got); !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip = %#v, want %#v", back, in)
	}
}

// TestPreBranchDecoderReadsAnUnswitchedEncode is the rollback direction itself:
// the value this branch writes, read by the binary an operator rolls back to.
// Nothing is lost.
func TestPreBranchDecoderReadsAnUnswitchedEncode(t *testing.T) {
	in := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}, "o3": {To: "coder"}}
	got := decodePreBranchModelOverrideMap(EncodeModelOverrideRules(in))
	want := map[string]string{"gpt-4o": "qwen3-32b", "o3": "coder"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pre-branch decode = %#v, want %#v — a rollback would lose these overrides", got, want)
	}
}

// TestPreBranchDecoderLosesTheWholeMapOnASwitchedRow pins the documented
// RESIDUAL, so it stays a known limitation rather than a surprise. The old
// decoder's failure is all-or-nothing: one object row and the entire map reads
// as nil, taking the untouched sibling row with it. This cannot be fixed from
// this side — the faulty decoder is in the old binary — which is exactly why
// the encoder above avoids emitting objects unless a row truly needs one.
func TestPreBranchDecoderLosesTheWholeMapOnASwitchedRow(t *testing.T) {
	in := map[string]ModelOverrideRule{"plain": {To: "qwen3-32b"}, "offers": {To: "coder", Offer: true}}
	if got := decodePreBranchModelOverrideMap(EncodeModelOverrideRules(in)); got != nil {
		t.Fatalf("pre-branch decode = %#v, want nil (the residual documented on migration63Up)", got)
	}
}

// TestLegacyColumnValueReEncodesIdentically is the forward direction, and the
// one the design calls out: a row written by the OLD binary, read after the
// migration, saved again by the NEW one. Both switches stay false and the
// column value is unchanged — so a token that predates this feature can be
// edited under the new binary and still be readable by the old one.
func TestLegacyColumnValueReEncodesIdentically(t *testing.T) {
	legacy := `{"gpt-4o":"qwen3-32b","o3":"coder"}`
	rules := DecodeModelOverrideRules(legacy)
	for name, rule := range rules {
		if rule.Offer || rule.HideTarget {
			t.Fatalf("legacy row %q gained a switch: %#v", name, rule)
		}
	}
	if got := EncodeModelOverrideRules(rules); got != legacy {
		t.Fatalf("re-encoded = %s, want the original %s", got, legacy)
	}
}
