// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// TestLiveTimingsCapableKind walks every value either kind vocabulary can put
// in front of LiveTimingsCapableKind -- all seven application types and all
// five RuntimeSpecTypes -- because the predicate's whole job is to answer for a
// string drawn from EITHER of them (an Application.Type, or the string form of
// an EffectiveRuntimeSpecType), and only two values may answer true.
//
// The load-bearing rows:
//   - ProviderServerAgent is FALSE. A server_agent application is not an
//     inference server at all; what serves is whatever EffectiveRuntimeSpecType
//     resolves its spec to, so the caller has to ask about THAT string. A true
//     here would switch the opt-in on for every agent-managed application
//     whatever runtime sits underneath it.
//   - ProviderLlamaSwap and ProviderLiteLLM are FALSE even though either often
//     fronts a llama.cpp: both resolve a model to an arbitrary downstream that
//     can be api.openai.com, which answers 400 on an unrecognized body key.
//     internal/provider/live_progress.go records the same reasoning for the
//     gate's own set, which is why the two sets agree here.
//   - "" is FALSE, so a spec whose type never resolved to anything takes the
//     default-off rather than being read as "unset, therefore fine".
//   - "LLAMA_CPP" and " llama_cpp" are FALSE. The lookup is a plain map hit on
//     an ALREADY-NORMALIZED kind: case-sensitive like portal.validEndpointMode,
//     but -- unlike that function -- it does not trim either, because
//     normalizeApplicationType (portal/service_applications.go:1000) and
//     validRuntimeSpecType (portal/service_runtime.go:997) have already trimmed
//     everything that can reach a stored Type.
func TestLiveTimingsCapableKind(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{ProviderLlamaCPP, true},
		{ProviderVLLM, true},
		{string(RuntimeSpecTypeLlamaCpp), true},
		{string(RuntimeSpecTypeVLLM), true},
		{ProviderMock, false},
		{ProviderOllama, false},
		{ProviderLlamaSwap, false},
		{ProviderLiteLLM, false},
		{ProviderServerAgent, false},
		{string(RuntimeSpecTypeTGI), false},
		{string(RuntimeSpecTypeOllama), false},
		{string(RuntimeSpecTypeCustom), false},
		{"", false},
		{"LLAMA_CPP", false},
		{" llama_cpp", false},
	}
	for _, tc := range cases {
		if got := LiveTimingsCapableKind(tc.kind); got != tc.want {
			t.Errorf("LiveTimingsCapableKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}

	// A table of literals can only ever answer for the kinds someone thought
	// to list, and the rows above are worth exactly as much as their coverage
	// of the SET. So the set itself is walked here in the other direction: a
	// kind added to liveTimingsCapableKinds without a row above fails HERE,
	// rather than sliding silently into the portal's create-time default. This
	// is also the half that keeps internal/provider's tripwire honest -- that
	// one can iterate its own map but cannot see this set from another package.
	wantByKind := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if prev, dup := wantByKind[tc.kind]; dup && prev != tc.want {
			t.Fatalf("the table answers %v and %v for the same kind %q: the two vocabularies SHARE this string, so two rows for it cannot disagree",
				prev, tc.want, tc.kind)
		}
		wantByKind[tc.kind] = tc.want
	}
	capableRows := 0
	for _, want := range wantByKind {
		if want {
			capableRows++
		}
	}
	if capableRows != len(liveTimingsCapableKinds) {
		t.Errorf("the table expects %d capable kinds but liveTimingsCapableKinds has %d: widen the table and re-check internal/provider's liveProgressUpstreams",
			capableRows, len(liveTimingsCapableKinds))
	}
	for kind := range liveTimingsCapableKinds {
		if _, listed := wantByKind[kind]; !listed {
			t.Errorf("liveTimingsCapableKinds contains %q, which no row above answers for: every member of the set needs its own row", kind)
		}
	}
}

// TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies pins the
// coincidence the single-map design rests on: an application type and a
// RuntimeSpecType are different Go types with different validators, and
// LiveTimingsCapableKind serves both from ONE map only because the two kinds
// that may answer true spell themselves identically. internal/provider's
// wantsLiveProgress leans on the same equality when it swaps target.Provider
// for target.LiveProgressSpecType on a server_agent target.
//
// If a later rename broke either pair, the predicate would keep compiling and
// start answering false for one whole vocabulary -- a silent default-off for
// every runtime spec, with no type error anywhere. The pairs are compared
// through a slice so the check is a real runtime comparison and not a constant
// the compiler folds away.
func TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies(t *testing.T) {
	for _, pair := range []struct {
		provider string
		specType RuntimeSpecType
	}{
		{ProviderLlamaCPP, RuntimeSpecTypeLlamaCpp},
		{ProviderVLLM, RuntimeSpecTypeVLLM},
	} {
		if pair.provider != string(pair.specType) {
			t.Errorf("provider %q and runtime spec type %q no longer share one string: LiveTimingsCapableKind's single map cannot serve both vocabularies any more",
				pair.provider, pair.specType)
			continue
		}
		if !LiveTimingsCapableKind(pair.provider) {
			t.Errorf("LiveTimingsCapableKind(%q) is false for a kind both vocabularies call capable", pair.provider)
		}
	}
}
