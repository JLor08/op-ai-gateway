// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
)

// benchmarkOmits names the routing.Target fields benchmarkTargetReq
// DELIBERATELY leaves at their zero value, each with the reason it does not
// apply to a benchmark stream. benchmarkTargetReq builds an inference.Request
// directly and dispatches through provider.CompleteStream (keyed on
// Target.Provider), so it never enters the native-passthrough / usage-recording
// paths that read the fields below -- see routing.Target's field docs, the I3
// comment in benchmark_runner.go, and the F2 comment in benchmark_runner_test.go.
var benchmarkOmits = map[string]bool{
	// Deliberately "" so the live-progress rejection memo (which never memoizes an
	// empty RouteID) cannot suppress a repeat across a run's 64 streams; also gates
	// the opportunistic-metrics side path (RouteID != ""), which a benchmark -- the
	// authoritative measurement -- must never trigger. (benchmark_runner.go:258-263)
	"RouteID": true,
	// No ActiveRequest is registered and no InferenceEvent emitted on the benchmark
	// stream (streamOnce -> CompleteStream), so nothing reads Target.ServerID here.
	"ServerID": true,
	// Target.APIFlavor is an affinity/dispatch input with no reader on the
	// CompleteStream path (dispatch keys on Provider); the benchmark's directly built
	// request needs no flavor.
	"APIFlavor": true,
	// APIFlavors / ResponsesMode / MessagesMode / ResponsesLiveTimingsEnabled are all
	// read only by the native-passthrough dispatch layer (native_passthrough.go,
	// wantsResponsesLiveTimings), which a chat-completions benchmark stream never
	// enters.
	"APIFlavors":                  true,
	"ResponsesMode":               true,
	"MessagesMode":                true,
	"ResponsesLiveTimingsEnabled": true,
	// Read only on the real-inference usage path (inference_complete.go), itself
	// gated on RouteID != ""; a benchmark computes its own throughput and must not
	// EWMA-update the mapping from its own synthetic stream.
	"OpportunisticMetrics": true,
}

// TestBenchmarkTargetReqSetsEveryFieldOrDocumentsOmission guards the SECOND real
// inference-target builder (benchmarkTargetReq) the way
// TestTargetFromPopulatesEveryField (internal/routing) guards the first. The
// issue that motivates both (#83): routing.Target is hand-assembled, a Go
// literal has no "...rest" spread, and a field added to Target and wired at only
// one site silently carries a zero value at the others. benchmarkTargetReq is a
// REAL inference target (not a test fixture), so a new dispatch-relevant field
// left unset here is a live bug -- exactly how the live-progress inputs were
// missed on this path before (see the F2 comment in benchmark_runner_test.go).
//
// Given a fully-populated server_agent benchmarkTarget, every Target field must
// be non-zero UNLESS it is named in benchmarkOmits with a reason. Adding a field
// to Target forces a decision here: wire it into benchmarkTargetReq, or record
// why the benchmark stream does not carry it.
func TestBenchmarkTargetReqSetsEveryFieldOrDocumentsOmission(t *testing.T) {
	// benchServerAgentTarget (benchmark_runner_test.go) is a server_agent target
	// with a set-mode spec token; add the live-progress inputs so LiveProgressSupport
	// and LiveProgressSpecType are non-zero too. Every source is then non-zero, so a
	// field left at zero means benchmarkTargetReq did not carry it.
	tgt := benchServerAgentTarget()
	tgt.liveProgressSupport = "supported"
	tgt.spec.Type = string(routing.RuntimeSpecTypeLlamaCpp)

	target, _ := benchmarkTargetReq(tgt)

	tv := reflect.ValueOf(target)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if benchmarkOmits[name] {
			continue
		}
		if tv.Field(i).IsZero() {
			t.Fatalf("benchmarkTargetReq left routing.Target.%s at its zero value for a fully-populated server_agent target.\n"+
				"benchmarkTargetReq (internal/gateway/benchmark_runner.go) is a REAL inference target. A field added to routing.Target must be wired here too, OR named in benchmarkOmits with the reason it does not apply to a benchmark stream.\n"+
				"If it is set here but from a source this test does not populate, set that source on the benchmarkTarget fixture above. The sibling builder targetFrom is guarded by TestTargetFromPopulatesEveryField in internal/routing.", name)
		}
	}
}
