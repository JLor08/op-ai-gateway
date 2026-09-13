// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind holds the two
// hand-written kind lists in the one relation that survives: liveProgressUpstreams
// (live_progress.go, the GATE's shape clause -- which upstreams may be SENT the
// two streaming parameters on /v1/chat/completions) must contain every kind
// routing.LiveTimingsCapableKind opts a newly created application or spec in
// for. Not the other way round, and not equality.
//
// They WERE one set, and until 2026-09-12 this test asserted exactly that.
// What broke the equality is a measurement, not a refactor: a streamed vLLM
// /v1/responses request carrying timings_per_token produced 48 data frames and
// not one timings object, so the key is inert there rather than merely
// tolerated. routing's set answers "which kind should the opt-in DEFAULT ON
// for", so vllm left it; this map answers "which kind may be SENT the
// parameters", and vllm stays, because the pair it sends includes
// stream_options.continuous_usage_stats, a first-class vLLM field that works.
// That is the ONE divergence, it is named in the first loop below, and a
// second one has to be argued for there.
//
// The relation that remains still matters: a capable kind missing from the
// gate would mean the portal defaults an opt-in ON for a kind the gate refuses
// to send anything to.
//
// It has a blind spot, named so it is not met as a surprise, and it exists
// because the relation now spans TWO endpoints: a kind measured to ANSWER
// timings_per_token on /v1/responses while rejecting this map's parameter pair
// on /v1/chat/completions would belong in routing's capable set and NOT here,
// and the second loop below would report that correct configuration as an
// error. The first loop's recorded-divergence escape hatch covers only the
// opposite direction. Such a kind needs its own exception, argued for the way
// vllm's was.
//
// It lives in package provider because liveProgressUpstreams is unexported.
//
// The two directions are NOT symmetric, because only one of the two sets is
// visible from here:
//   - over the GATE: the first loop ranges over the real map, so it catches any
//     kind added to the gate alone -- whichever kind that is, except the ONE
//     recorded divergence the loop names. Measured:
//     adding "ollama" to liveProgressUpstreams together with its four
//     expectations elsewhere in this package (TestWantsLiveProgressAllowList,
//     TestWantsLiveProgressThreeLayerRule twice, and
//     TestWantsLiveProgressReadsTheJoinedVerdict) leaves the entire backend
//     suite green except this loop. Nothing else in the tree can see that edit.
//   - over the CAPABLE set: the second loop has to go through the enumeration
//     below, since routing's set is unexported and another package cannot range
//     over it. The other half of that direction lives in routing's own
//     TestLiveTimingsCapableKind, which ranges over the set and fails for any
//     member it has no row for. The staleness check at the end is what keeps
//     this enumeration from quietly falling behind the vocabularies.
//
// Two things this test deliberately does NOT guard, named so they are not
// assumed:
//   - It does not pin that vllm is still in the gate. Removing
//     routing.ProviderVLLM from liveProgressUpstreams leaves every loop here
//     silent; what fails is three expectations in two OTHER tests of this
//     package -- TestWantsLiveProgressAllowList (wantsLiveProgress("vllm") =
//     false, want true) and TestWantsLiveProgressThreeLayerRule twice, on its
//     "undetermined + vllm shape implies tolerant" row and on its
//     "undetermined + server_agent whose effective spec type is vllm" row,
//     which reaches the same map entry through target.LiveProgressSpecType.
//   - It does not fire for a kind in NEITHER vocabulary. Add a future
//     routing.ProviderSGLang = "sglang" to routing's capable set with its row in
//     TestLiveTimingsCapableKind and nothing here notices -- the enumeration
//     never mentions it, and the first loop iterates the GATE's keys, where it
//     does not appear either. Closing that from this side needs an exported
//     accessor for routing's set, i.e. production API whose only caller is a
//     test. The mitigation lives next to the set instead: routing's
//     TestLiveTimingsCapableKindsSizeIsPinned fails on any change to its SIZE.
func TestLiveProgressUpstreamsCoverEveryRoutingCapableKind(t *testing.T) {
	// Over the real gate map: every kind the gate may send the parameters to
	// is either live-timings-capable or THE one recorded divergence.
	for kind := range liveProgressUpstreams {
		if kind == routing.ProviderVLLM || routing.LiveTimingsCapableKind(kind) {
			continue
		}
		t.Errorf("liveProgressUpstreams has %q, which routing.LiveTimingsCapableKind calls incapable and which is not the one recorded divergence (%q): either add it to routing's capable set, or record here why the gate may send the parameters to a kind no create path ever opts in",
			kind, routing.ProviderVLLM)
	}

	// Every value in BOTH kind vocabularies, including the three that spell
	// themselves the same in each ("vllm", "llama_cpp", "ollama"), plus the
	// empty spec type that means "auto-detect from the binary" and is a
	// legitimate stored value. Listed rather than derived: neither vocabulary
	// exposes an enumeration (portal.normalizeApplicationType and
	// portal.validRuntimeSpecType are unexported switches in a third package).
	kinds := []string{
		routing.ProviderMock, routing.ProviderOllama, routing.ProviderVLLM, routing.ProviderLlamaCPP,
		routing.ProviderLlamaSwap, routing.ProviderLiteLLM, routing.ProviderServerAgent,
		string(routing.RuntimeSpecTypeVLLM), string(routing.RuntimeSpecTypeLlamaCpp),
		string(routing.RuntimeSpecTypeTGI), string(routing.RuntimeSpecTypeOllama),
		string(routing.RuntimeSpecTypeCustom), "",
	}
	listed := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		listed[kind] = struct{}{}
		_, inGate := liveProgressUpstreams[kind]
		if routing.LiveTimingsCapableKind(kind) && !inGate {
			t.Errorf("%q: routing.LiveTimingsCapableKind is true but liveProgressUpstreams does not list it -- the portal would default the opt-in ON for a kind the gate refuses to send the parameters to", kind)
		}
	}

	// Not a drift claim, and deliberately phrased so it cannot be mistaken for
	// one: the two sets can stand in the right relation and still hold a kind
	// this enumeration has never heard of, and then the loop above is checking
	// thirteen strings that no longer describe the vocabularies. Checked on the
	// gate's keys only -- a size comparison would also fire whenever the
	// CAPABLE side changed, duplicating the errors above with a message that
	// points at the wrong file.
	for kind := range liveProgressUpstreams {
		if _, ok := listed[kind]; !ok {
			t.Errorf("liveProgressUpstreams has %q, which this test's kind list does not enumerate: add it there too, or the per-kind loop stops covering the whole vocabulary", kind)
		}
	}
}
