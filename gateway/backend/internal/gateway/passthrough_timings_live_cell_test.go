// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// flaggedPartialFrames is the upstream shape the operator's switch exists to
// produce: `response.output_text.delta` partials each carrying a top-level
// `timings` object, and NO terminal `response.completed` -- the truncated-but-
// clean shape llama.cpp produces when it simply stops. The frames and their
// numbers are the ones
// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate already
// uses, reused verbatim so the two tests differ in exactly ONE thing: there the
// CLIENT sent `timings_per_token`, here the operator's opt-in made the gateway
// inject it.
//
// predicted_per_second falls frame to frame (42.5 then 38.25) on purpose: it is
// a cumulative average over the generation, so an implementation that fed either
// surface the accumulator's running MAX would pin both at 42.5 and fail below.
var flaggedPartialFrames = []string{
	"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi","timings":{"predicted_per_second":42.5,"predicted_n":7}}` + "\n\n",
	"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":" there","timings":{"predicted_per_second":38.25,"predicted_n":12}}` + "\n\n",
}

// TestOperatorOptInPutsAnUpstreamRateOnTheLiveRow is the test issue #81 is
// ABOUT, and the one the two shipping PRs never wrote. #81's opening sentence is
// that on the flavor Codex actually uses "the running-connections panel's live
// tokens/sec cell is empty for the whole request"; everything else in the issue
// is machinery for ending that. Nothing pinned that it ends.
//
// The gap was structural, not an oversight of one assertion. The only harness
// that can turn the gate ON (newLiveTimingsTestServer) drove a single
// `response.completed` frame carrying no `timings` at all, because "these tests
// are about the REQUEST"; and every test that asserts an upstream-labelled live
// rate uses newNativeProxyTestServer, whose fixture application resolves to kind
// "custom" and carries no opt-in, so its flag always came from the client's own
// body. The feature's user-visible promise therefore rested on the composition
// of two separately-tested halves, and no test crossed them.
//
// This one crosses them, and asserts all four surfaces of one request:
//
//   - the WIRE: the relayed body carries the key, and the client never sent it;
//   - the LIVE CELL: an "upstream"-labelled rate and the upstream's own
//     mid-stream count, which is what the panel renders;
//   - the RECORDED row: the rate the stream REPORTED (38.25), not the peak
//     (42.5) -- #80's invariant, which #81 named as its prerequisite because the
//     flag is what creates the many-partials population in the first place. No
//     test had ever exercised the two together, so the flag's own traffic was
//     the one traffic the peak fix was never checked against;
//   - the ISOLATION: the recorded row still reports no tokens, because
//     `predicted_n` rides a field of its own and must never reach the
//     accumulator's counts.
func TestOperatorOptInPutsAnUpstreamRateOnTheLiveRow(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{pieces: flaggedPartialFrames, gap: framePacing}
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())
	prov.observe = func() { got = snapshotLiveRow(t, srv) }

	// No timings_per_token in the client's body: the operator's switch is the
	// only possible source of the key upstream.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(liveTimingsStreamBody))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The premise, asserted FIRST: without it every number below could be
	// explained by a request that never reached proxyNative at all. This is the
	// evidence postPassthrough's proxyCalls check supplies for the other tests
	// in this file, which this one cannot use (it is typed to
	// *recordingProxyProvider).
	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("relayed body does not carry the injected key, so nothing below is about the opt-in: %s", prov.gotBody)
	}

	if !got.hasProgress {
		t.Fatal("the in-flight row carried no progress counter at all")
	}
	if got.ttftMS <= 0 {
		t.Fatalf("live ttft_ms = %d, want > 0", got.ttftMS)
	}
	if got.source != "upstream" {
		t.Fatalf("live tokens_per_second_source = %q, want %q -- the whole point of the injection is that llama.cpp's OWN measurement rides the partials; a %q label would mean the gateway derived it instead", got.source, "upstream", "gateway")
	}
	if got.tps != 38.25 {
		t.Fatalf("live tokens_per_second = %v, want 38.25 (the MOST RECENT flagged frame's predicted_per_second, not the running max 42.5)", got.tps)
	}
	if got.outputTokens != 12 {
		t.Fatalf("live output_tokens = %d, want 12 -- the latest partial's timings.predicted_n, the exact mid-stream count #81's open question decided to take", got.outputTokens)
	}

	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	// This stream is truncated AND status success, so its rate is a ROUTING
	// input: recordUsage's EWMA feed is gated on success, so it reaches
	// UpdateMappingOpportunisticMetrics -- the throughput the scorer and a
	// group's MinTokensPerSecond gate read back. #81 measured the peak bias at
	// +10.2% and made #80 its prerequisite for exactly this reason.
	if events[0].TokensPerSecond != 38.25 {
		t.Fatalf("recorded TokensPerSecond = %v, want 38.25 -- the last rate this stream reported; 42.5 is the accumulator's peak, and with the operator's flag creating the many-partials population, recording a peak would bias routing upward on every request the feature improves", events[0].TokensPerSecond)
	}
	if events[0].OutputTokens != 0 || events[0].TotalTokens != 0 {
		t.Fatalf("recorded OutputTokens/TotalTokens = %d/%d, want 0/0 -- no frame carried a usage object, so 12 in either field means timings.predicted_n reached the accumulator's recorded counts through the INJECTED flag's traffic", events[0].OutputTokens, events[0].TotalTokens)
	}
}

// TestTranslateModeApplicationWithTheOptInInjectsNothing is the second
// mode-conditional gap, and its honest claim is narrower than it looks.
//
// wantsResponsesLiveTimings has NO endpoint-mode conjunct: for a translate-mode
// target with the opt-in on, ALL FIVE of its conditions hold and the predicate
// answers TRUE. A test asserting "the predicate refuses" would pin a false
// belief. The protection is STRUCTURAL -- the injection call site sits inside
// tryProxyNative's `case routing.EndpointModePassthrough` arm, and the translate
// default returns without ever calling proxyNative -- so the only honest
// assertion is end to end: no raw body was ever relayed, hence none could carry
// the key.
//
// That is worth pinning rather than assuming, because it is the one protection
// no unit test can see. A future second caller of the gate from outside the
// passthrough arm would inject on a translate request with nothing failing.
func TestTranslateModeApplicationWithTheOptInInjectsNothing(t *testing.T) {
	// The spec form is the only one this harness can express: it hardcodes both
	// application modes to passthrough so one seed can serve the /v1/responses
	// positive and the /v1/messages negative. The spec's own mode is what
	// targetFrom reads for a server_agent application, so switching it there
	// reaches the same decision.
	//
	// The control below is derived from THIS spec value, differing only in the
	// mode, so the two halves cannot disagree about anything else by
	// construction. Two weaker shapes were tried and both left the control
	// vacuous for the mutation unique to this test -- an edit that disables the
	// opt-in on the translate side alone: a second constructor call reads an
	// untouched seed, and so does a second copy of a shared base, because the
	// edit lands after the copy. Copying the translate spec itself is the only
	// form where "opted in" cannot differ between the halves.
	translateSpec := *serverAgentSpecOptedIn().spec
	translateSpec.ResponsesMode = routing.EndpointModeTranslate
	seed := serverAgentSpecOptedIn()
	seed.spec = &translateSpec

	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, seed)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(liveTimingsStreamBody))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Served, not refused: a 404 or a resolve failure would satisfy the
	// assertions below for entirely the wrong reason, so the request having been
	// handled by the TRANSLATE path is part of the claim.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s -- the request must have been SERVED by translation, or the zero proxy calls below prove nothing", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0: a translate-mode endpoint must never reach the native passthrough path", prov.proxyCalls)
	}
	if len(prov.gotBody) != 0 {
		t.Fatalf("a raw body reached the provider on a translate-mode endpoint: %s", prov.gotBody)
	}
	// ANTI-VACUITY CONTROL, and it has to be built this way. Asserting the
	// predicate against a hand-built routing.Target literal would prove nothing:
	// the literal would satisfy the gate even if the SEED's opt-in were false, so
	// `spec.ResponsesLiveTimingsEnabled = false` would leave this test green
	// while it silently stopped testing the mode at all.
	//
	// Instead the spec the first half actually used runs again with the ONE
	// field under test flipped back. If it is genuinely opted in, this run
	// injects; if it is not, the control fails and says so. So the zero
	// injections above are attributable to the MODE and to nothing else: the two
	// specs are the same value apart from ResponsesMode.
	passthroughSpec := translateSpec
	passthroughSpec.ResponsesMode = routing.EndpointModePassthrough
	control := serverAgentSpecOptedIn()
	control.spec = &passthroughSpec
	controlProv := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	controlSrv := newLiveTimingsTestServer(t, controlProv, control)

	got := postPassthrough(t, controlSrv, controlProv, "/v1/responses", liveTimingsStreamBody)
	if !strings.Contains(got, `"timings_per_token":true`) {
		t.Fatalf("the control run did NOT inject (%s), so the seed is not opted in and the translate assertions above prove nothing about the mode", got)
	}
}
