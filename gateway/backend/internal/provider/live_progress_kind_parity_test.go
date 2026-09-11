// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestLiveProgressUpstreamsMatchesRoutingCapableKinds pins the two hand-written
// kind lists together: liveProgressUpstreams (live_progress.go, the GATE's shape
// clause -- which upstreams may be SENT the parameters) and
// routing.LiveTimingsCapableKind (which upstreams a newly created application or
// spec gets the opt-in switched ON for). They answer different questions and
// must not be merged, but they are the same set for the same reason, and a
// divergence would mean the portal defaults an opt-in ON for a kind the gate
// will never honour -- or leaves it OFF for one it would.
//
// It lives in package provider because liveProgressUpstreams is unexported; it
// changes no production code here, and part 1 of issue #81 deliberately touches
// none of this package's behaviour.
//
// It is the only thing that notices a COHERENT one-sided change -- one where
// that side's own tests were updated in the same edit, which is what a
// deliberate change looks like. Measured, not assumed: adding "ollama" to
// routing's set together with its two rows in TestLiveTimingsCapableKind, and
// separately adding it to liveProgressUpstreams together with its four
// expectations here (TestWantsLiveProgressAllowList,
// TestWantsLiveProgressThreeLayerRule twice, and
// TestWantsLiveProgressReadsTheJoinedVerdict), each leave the ENTIRE backend
// suite green except this test. An incoherent edit does also trip that side's
// own tests -- they pin every kind in both vocabularies behaviourally -- but
// those are the tests the same edit would update.
//
// The two directions are NOT symmetric, because only one of the two sets is
// visible from here:
//   - gate => capable iterates the real map, so it catches a kind added to the
//     gate alone no matter which kind that is;
//   - capable => gate has to go through the enumeration below, since
//     routing's set is unexported and another package cannot range over it. The
//     other half of that direction lives in routing's own
//     TestLiveTimingsCapableKind, which ranges over the set and fails for any
//     member it has no row for. The staleness check at the end is what keeps
//     this enumeration from quietly falling behind the vocabularies.
func TestLiveProgressUpstreamsMatchesRoutingCapableKinds(t *testing.T) {
	for kind := range liveProgressUpstreams {
		if !routing.LiveTimingsCapableKind(kind) {
			t.Errorf("liveProgressUpstreams has %q but routing.LiveTimingsCapableKind(%q) is false -- the gate would send the parameters to a kind no create path ever opts in", kind, kind)
		}
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
		want := routing.LiveTimingsCapableKind(kind)
		if inGate != want {
			t.Errorf("%q: liveProgressUpstreams=%v routing.LiveTimingsCapableKind=%v -- the two kind lists have drifted", kind, inGate, want)
		}
	}

	// Not a drift claim, and deliberately phrased so it cannot be mistaken for
	// one: the two sets can agree perfectly and still hold a kind this
	// enumeration has never heard of, and then the loop above is checking
	// thirteen strings that no longer describe the vocabularies. Checked on the
	// gate's keys only -- a size comparison would also fire whenever the
	// CAPABLE side changed, duplicating the drift errors above with a message
	// that points at the wrong file.
	for kind := range liveProgressUpstreams {
		if _, ok := listed[kind]; !ok {
			t.Errorf("liveProgressUpstreams has %q, which this test's kind list does not enumerate: add it there too, or the per-kind loop stops covering the whole vocabulary", kind)
		}
	}
}
