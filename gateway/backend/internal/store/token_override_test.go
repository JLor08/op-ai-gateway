// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"reflect"
	"testing"
)

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
