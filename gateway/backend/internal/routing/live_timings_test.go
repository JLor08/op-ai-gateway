// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// TestLiveTimingsCapableKind walks every value either kind vocabulary can put
// in front of LiveTimingsCapableKind -- all seven application types and all
// five RuntimeSpecTypes -- because the predicate's whole job is to answer for a
// string drawn from EITHER of them (an Application.Type, or the string form of
// an EffectiveRuntimeSpecType), and only ONE value may answer true.
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
//     gate's own set, and on these two kinds the two sets still agree -- but
//     they are no longer the same set. See the vllm bullet below.
//   - ProviderVLLM and RuntimeSpecTypeVLLM are FALSE, and they are the ONE
//     place this set and internal/provider's liveProgressUpstreams disagree.
//     vLLM's /v1/responses accepts timings_per_token and does nothing with it:
//     measured 2026-09-12 against a live vLLM upstream through the gateway,
//     a streamed request carrying the flag produced 48 data frames and NOT ONE
//     carrying a timings object -- not on the partials, not on the terminal
//     frame. (One deployment, one build, whose build identifier was not
//     recorded; the same scope caveat the repository already applies to #80's
//     measurement.) The opt-in this set decides defaults ON for a newly
//     created application, so keeping vllm here would default a switch on that
//     provably delivers nothing. The GATE keeps vllm, because the pair it
//     sends on /v1/chat/completions -- timings_per_token AND
//     stream_options.continuous_usage_stats -- includes a first-class vLLM
//     field that works there.
//   - "" is FALSE, so a spec whose type never resolved to anything takes the
//     default-off rather than being read as "unset, therefore fine".
//   - "LLAMA_CPP" and " llama_cpp" are FALSE. The lookup is a plain map hit on
//     an ALREADY-NORMALIZED kind: case-sensitive like portal.validEndpointMode,
//     but -- unlike that function -- it does not trim either. Nothing untrimmed
//     can reach a stored Type, by two DIFFERENT mechanisms and not by one
//     shared one: portal.normalizeApplicationType trims inside its own switch
//     (it switches over strings.TrimSpace(raw)), whereas
//     portal.validRuntimeSpecType trims nothing -- portal.putRuntimeSpec, the
//     only function that calls it, binds specType from
//     strings.TrimSpace(req.Type) before validating it and stores that same
//     trimmed local as the spec's Type. Its own doc comment says "callers pass
//     req.Type through untouched", which is about the EMPTY value being a
//     legitimate stored kind, not about whitespace; reading it as "it trims"
//     is the trap.
func TestLiveTimingsCapableKind(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{ProviderLlamaCPP, true},
		{ProviderVLLM, false},
		{string(RuntimeSpecTypeLlamaCpp), true},
		{string(RuntimeSpecTypeVLLM), false},
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
		t.Errorf("the table expects %d capable kinds but liveTimingsCapableKinds has %d: fix the table to match the set. Do NOT reflexively re-level internal/provider's liveProgressUpstreams -- since 2026-09-12 the two are no longer one set, and the only rule left is that every kind HERE is also in the gate",
			capableRows, len(liveTimingsCapableKinds))
	}
	for kind := range liveTimingsCapableKinds {
		if _, listed := wantByKind[kind]; !listed {
			t.Errorf("liveTimingsCapableKinds contains %q, which no row above answers for: every member of the set needs its own row", kind)
		}
	}
}

// TestLiveTimingsCapableKindsSizeIsPinned is a breadcrumb, not a property: it
// asserts the SIZE of the set and nothing about its contents, so that changing
// the size cannot happen without an edit right here -- at a site whose failure
// message names the OTHER hand-written list and says how far the two still have
// to agree: every kind HERE is in the gate, and the gate deliberately holds one
// kind this set does not.
//
// Why a size pin is needed on top of everything else. internal/provider's
// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind no longer checks
// equality in either direction: the contract since 2026-09-12 is that the
// capable set is a SUBSET of the gate, with exactly one recorded divergence
// (vllm, which the gate keeps and this set dropped). The only direction it can
// check exactly is "every member of the GATE is either capable or that one
// divergence", because that direction ranges over the real
// liveProgressUpstreams map. The capable => gate direction runs
// through a hand-listed enumeration of both vocabularies over there, and the
// staleness loop that guards that enumeration iterates the GATE's keys. So a
// COHERENT capable-side addition of a kind that neither vocabulary lists yet
// slips through every check we have: add a future ProviderSGLang = "sglang" to
// liveTimingsCapableKinds and a {ProviderSGLang, true} row to the table above,
// and routing's tests pass, all three loops in the provider-side tripwire stay
// silent, and the portal would default the opt-in ON for a kind the gate never
// sends the parameters to. Closing that for real needs an exported accessor
// for this set, i.e. production API whose only caller is a test; this is the
// cheap mitigation instead.
func TestLiveTimingsCapableKindsSizeIsPinned(t *testing.T) {
	// Changing this number is the deliberate act. Read the failure message
	// before you do.
	const pinnedSize = 1
	if len(liveTimingsCapableKinds) != pinnedSize {
		t.Fatalf("liveTimingsCapableKinds has %d kinds, pinned at %d: a kind was added or removed here. Do NOT go and make internal/provider's liveProgressUpstreams match -- that is the wrong half to follow: the two sets stopped being one set on 2026-09-12 and the gate deliberately holds vllm, which this set does not. The rule is only that every kind HERE is also in the gate; read the divergence note on liveTimingsCapableKinds first. And note that the provider-side parity test cannot see this set and enumerates kind strings by hand, so it stays SILENT about a kind neither list mentions yet",
			len(liveTimingsCapableKinds), pinnedSize)
	}
}

// TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies pins the
// coincidence the single-map design rests on: an application type and a
// RuntimeSpecType are different Go types with different validators, and
// LiveTimingsCapableKind serves both from ONE map only because the kinds the
// two vocabularies have IN COMMON spell themselves identically -- llama_cpp,
// the one kind that may answer true, and vllm, which answers false here and is
// still looked up through that same single map. internal/provider's
// wantsLiveProgress leans on the same equality when it swaps target.Provider
// for target.LiveProgressSpecType on a server_agent target.
//
// If a later rename broke either pair, the predicate would keep compiling and
// start answering false for one whole vocabulary -- a silent default-off for
// every runtime spec, with no type error anywhere. The pairs are compared
// through a slice so the check is a real runtime comparison and not a constant
// the compiler folds away.
//
// The vllm pair is still here even though vllm is no longer capable, and the
// two halves of each row are now checked SEPARATELY, because the string
// equality outlived the membership: internal/provider's wantsLiveProgress
// swaps target.Provider for target.LiveProgressSpecType on a server_agent
// target and looks the result up in liveProgressUpstreams, which still lists
// vllm. Delete this row and that swap loses its only guard. The capable column
// is what says which vocabulary-crossing kind this set still opts in.
func TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies(t *testing.T) {
	for _, pair := range []struct {
		provider string
		specType RuntimeSpecType
		capable  bool
	}{
		{ProviderLlamaCPP, RuntimeSpecTypeLlamaCpp, true},
		{ProviderVLLM, RuntimeSpecTypeVLLM, false},
	} {
		if pair.provider != string(pair.specType) {
			t.Errorf("provider %q and runtime spec type %q no longer share one string: LiveTimingsCapableKind's single map cannot serve both vocabularies any more, and internal/provider's wantsLiveProgress loses the same equality",
				pair.provider, pair.specType)
			continue
		}
		if got := LiveTimingsCapableKind(pair.provider); got != pair.capable {
			t.Errorf("LiveTimingsCapableKind(%q) = %v, want %v: one string, one verdict -- both vocabularies have to get the same answer out of the single map",
				pair.provider, got, pair.capable)
		}
	}
}
