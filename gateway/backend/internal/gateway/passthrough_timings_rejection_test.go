// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// rejectingProxyProvider is the only fake in this package that can answer a
// native passthrough with a NON-2xx status, and the only one that implements
// provider.LiveProgressRejectionMemo -- the two capabilities issue #81's
// rejection residual needs together. Every pre-existing fake hardcodes 200
// (server_test.go, passthrough_progress_test.go, passthrough_usage_scan_test.go,
// native_passthrough_test.go), so before this type the 4xx-after-injection path
// had never been driven from a gateway test at all.
//
// It is file-local rather than a new field on the shared recordingProxyProvider,
// following the precedent of erroringProxyProvider and noContentTypeProxyProvider
// (native_passthrough_test.go): the memo half would be dead weight on the twelve
// call sites that only need a 200.
type rejectingProxyProvider struct {
	// status is the upstream's answer. Set per test; there is no zero default
	// on purpose, so a test that forgets it fails loudly rather than silently
	// exercising the 200 path.
	status int
	// respBody is relayed verbatim as the upstream body.
	respBody string

	// gotBody is the body the gateway actually relayed -- the premise every
	// assertion here rests on (that the injected key really was on the wire).
	gotBody []byte
	// proxyCalls counts ProxyNative calls, so an absence assertion cannot pass
	// because the request never reached proxyNative.
	proxyCalls int

	// rejected is the memo's state, keyed by RouteID exactly as the real memo is.
	rejected map[string]bool
	// recordCalls counts RecordLiveProgressRejection calls, so "recorded once"
	// is distinguishable from "recorded on every frame".
	recordCalls int
}

func newRejectingProxyProvider(status int, respBody string) *rejectingProxyProvider {
	return &rejectingProxyProvider{status: status, respBody: respBody, rejected: map[string]bool{}}
}

func (p *rejectingProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, provider.ErrUnavailable
}

func (p *rejectingProxyProvider) ProxyNative(_ context.Context, _ routing.Target, _ string, body []byte) (*provider.ProxyResponse, error) {
	p.proxyCalls++
	p.gotBody = append([]byte(nil), body...)
	return &provider.ProxyResponse{
		StatusCode: p.status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(p.respBody)),
	}, nil
}

func (p *rejectingProxyProvider) LiveProgressRejected(target routing.Target) bool {
	return p.rejected[target.RouteID]
}

func (p *rejectingProxyProvider) RecordLiveProgressRejection(target routing.Target) {
	p.recordCalls++
	p.rejected[target.RouteID] = true
}

var (
	_ provider.Client                    = (*rejectingProxyProvider)(nil)
	_ provider.NativeProxyClient         = (*rejectingProxyProvider)(nil)
	_ provider.LiveProgressRejectionMemo = (*rejectingProxyProvider)(nil)
)

// liveTimingsRefusalMsg is the substring every assertion in this file matches
// the refusal Warn on. It lives in one place so the positive and the three
// negatives cannot drift apart -- the failure mode that matters is a build whose
// warning fires on requests it should not, and a negative matching a different
// substring than the positive would never see it.
//
// It is deliberately a substring of the message rather than the whole of it: the
// wording carries the remedy for an operator reading a log, so it will change,
// and coupling four assertions to its exact prose would make every rewording a
// test edit. "live timings" plus the level is specific enough -- nothing else in
// this package logs those two words at Warn.
const liveTimingsRefusalMsg = "live timings"

// assertNoLiveTimingsRefusalWarning is the negative half of the refusal
// diagnostic, and it exists because the Warn and the memo write share ONE
// condition at proxyNative. Splitting that condition -- so the line fires on
// every injected request rather than only on a refused one -- is a one-character
// edit that the memo assertions alone would not notice, and the whole point of
// residual 4 is that this line is TRUSTWORTHY at the default level. A diagnostic
// that cries wolf is worse than the silence it replaced.
//
// The package already demands exactly this discipline for the per-request debug
// FIELD (TestPassthroughNativeDebugLineRecordsTheInjection: "a field emitted only
// when true is indistinguishable, to an operator grepping a log, from a build
// that never had the field"). This is the same rule for the line.
func assertNoLiveTimingsRefusalWarning(t *testing.T, buf *logbuffer.Buffer, why string) {
	t.Helper()
	recs := buf.Snapshot()
	if findLogRecord(recs, "WARN", liveTimingsRefusalMsg) {
		t.Fatalf("a live-timings refusal WARN was emitted although %s; records = %+v", why, recs)
	}
}

// postRejecting drives one request and returns the recorder. Unlike
// postPassthrough it asserts NOTHING about the status -- that is the subject
// here -- but it keeps the same "the request reached proxyNative" discipline.
func postRejecting(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestInjectedTimingsRejectionIsVisibleAtTheDefaultLogLevel is residual 4 of the
// issue #81 audit. Before it, the ONLY signal naming the injection was the
// `timings_per_token_injected` attribute on proxyNative's per-request
// slog.Debug line -- while OP_AI_GATEWAY_LOG_LEVEL defaults to "info", so the
// "one grep" the architecture doc promised an operator holding an unexplained
// upstream 4xx returned nothing in any default deployment.
//
// The capture is deliberately withCapturedSlogAtTheDefaultLevel (INFO), not
// withCapturedSlog (DEBUG): at Debug the pre-existing line already carries the
// attribute, so a Debug capture would pass against the invisible behaviour this
// test exists to forbid.
//
// It also cannot settle for "some WARN record exists": nativeTerminalStatus
// already logs "inference native passthrough upstream error" at Warn for any
// non-2xx. The assertion is on a record that names the INJECTION.
func TestInjectedTimingsRejectionIsVisibleAtTheDefaultLogLevel(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	prov := newRejectingProxyProvider(http.StatusBadRequest, `{"error":"unknown field"}`)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	rec := postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)

	// Premise: the key really was on the wire, so the 400 is one the injection
	// plausibly earned rather than one the client's own body earned.
	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("relayed body does not carry the injected key, so this test proves nothing: %s", prov.gotBody)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream's 400 relayed verbatim", rec.Code)
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", liveTimingsRefusalMsg) {
		t.Fatalf("no WARN record naming the live-timings refusal at the default level; records = %+v", recs)
	}
	// The record must carry the fields an operator needs to act: which mapping,
	// and what the upstream answered.
	var found bool
	for _, r := range recs {
		if r.Level != "WARN" || !strings.Contains(r.Msg, liveTimingsRefusalMsg) {
			continue
		}
		found = true
		if got, ok := r.Attrs["status"]; !ok || got != int64(http.StatusBadRequest) {
			t.Errorf("WARN record status attr = %v (ok=%v), want 400", got, ok)
		}
		if got, ok := r.Attrs["route_id"]; !ok || got != "route-lt" {
			t.Errorf("WARN record route_id attr = %v (ok=%v), want route-lt", got, ok)
		}
	}
	if !found {
		t.Fatal("no WARN record matched for the attribute assertions")
	}
}

// TestInjectedTimingsRejectionIsRecordedForBothEndpoints is residual 1: the
// native path now WRITES the rejection memo that CompleteStream already reads,
// so a refusal learned on /v1/responses stops /v1/chat/completions asking too.
// Before this, the memo had one writer and one reader, both on the translate
// path, and issue #81's "a recorded rejection beats everything" had no
// implementation here in either direction.
func TestInjectedTimingsRejectionIsRecordedForBothEndpoints(t *testing.T) {
	prov := newRejectingProxyProvider(http.StatusBadRequest, `{"error":"unknown field"}`)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("relayed body does not carry the injected key: %s", prov.gotBody)
	}
	if prov.recordCalls != 1 {
		t.Fatalf("RecordLiveProgressRejection calls = %d, want exactly 1", prov.recordCalls)
	}
	// Keyed on the serving mapping, which is what makes the record reach
	// CompleteStream's guard for the SAME mapping and nothing else.
	if !prov.rejected["route-lt"] {
		t.Fatalf("rejection recorded under %v, want the mapping id route-lt", prov.rejected)
	}
}

// TestUninjectedRejectionIsNotRecorded is the other direction, and the one a
// careless implementation gets wrong: a 400 the CLIENT's own body earned must
// not be attributed to a key the gateway never added. Here the client sets
// timings_per_token itself, so injectTimingsPerToken leaves the body untouched
// and injectedLiveTimings is false even though the gate said yes.
func TestUninjectedRejectionIsNotRecorded(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	prov := newRejectingProxyProvider(http.StatusBadRequest, `{"error":"bad input"}`)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	postRejecting(t, srv, "/v1/responses", `{"model":"gw-model","stream":true,"input":"hi","timings_per_token":true}`)

	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want exactly 1 (the request must have reached proxyNative)", prov.proxyCalls)
	}
	if prov.recordCalls != 0 {
		t.Fatalf("RecordLiveProgressRejection calls = %d, want 0: the client set the key, so the gateway injected nothing to blame", prov.recordCalls)
	}
	assertNoLiveTimingsRefusalWarning(t, buf, "the client's own key earned this 400")
}

// TestNonSchemaRejectionIsNotRecorded pins the STATUS class. The native path has
// no retry to absorb a wrong guess, so treating every 4xx as a parameter
// refusal would let an auth failure or a wrong path suppress the figure for the
// whole TTL on an upstream that never objected to the key.
func TestNonSchemaRejectionIsNotRecorded(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		prov := newRejectingProxyProvider(status, `{"error":"nope"}`)
		srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

		postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)

		if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
			t.Fatalf("status %d: relayed body does not carry the injected key: %s", status, prov.gotBody)
		}
		if prov.recordCalls != 0 {
			t.Errorf("status %d: RecordLiveProgressRejection calls = %d, want 0 (only 400/422 are a schema refusal)", status, prov.recordCalls)
		}
		assertNoLiveTimingsRefusalWarning(t, buf, fmt.Sprintf("status %d is not a schema refusal", status))
	}
	// ...and the second member of the class IS recorded, so the test above is
	// not passing because nothing is ever recorded.
	prov := newRejectingProxyProvider(http.StatusUnprocessableEntity, `{"error":"unprocessable"}`)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())
	postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)
	if prov.recordCalls != 1 {
		t.Fatalf("422: RecordLiveProgressRejection calls = %d, want 1", prov.recordCalls)
	}
}

// TestRecordedRejectionSuppressesTheNextInjection closes residual 1's loop: the
// memo is not write-only here. The gate's five conditions stay a PURE function
// of target+flavor+stream (that purity is what makes TestWantsResponsesLiveTimings
// cover the whole rule), so the memo is ANDed at proxyNative's call site --
// exactly as internal/provider's own guard does it at
// `wantsLiveProgress(target) && !c.liveProgress.rejects(target.RouteID)`, the
// conjunct the gate's doc comment predicted "a caller from here would silently
// drop".
func TestRecordedRejectionSuppressesTheNextInjection(t *testing.T) {
	prov := newRejectingProxyProvider(http.StatusBadRequest, `{"error":"unknown field"}`)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	// First request: injected, refused, recorded.
	postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)
	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("first request did not carry the injected key: %s", prov.gotBody)
	}

	// Second request, same mapping: the operator's switch is still on and all
	// five conditions still hold, but the recorded refusal vetoes the injection.
	prov.status = http.StatusOK
	prov.respBody = terminalOnlyResponsesStream
	postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)

	if prov.proxyCalls != 2 {
		t.Fatalf("ProxyNative calls = %d, want 2", prov.proxyCalls)
	}
	if strings.Contains(string(prov.gotBody), "timings_per_token") {
		t.Fatalf("second request still carries the key after a recorded rejection: %s", prov.gotBody)
	}
	// And nothing re-records: there was no injection to be refused.
	if prov.recordCalls != 1 {
		t.Fatalf("RecordLiveProgressRejection calls = %d, want 1 (the second request injected nothing)", prov.recordCalls)
	}
}

// TestASuccessfulInjectionRecordsNothing is the plainest both-directions guard:
// the memo must stay empty for the happy path, or every opted-in mapping would
// veto itself after one request.
func TestASuccessfulInjectionRecordsNothing(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	prov := newRejectingProxyProvider(http.StatusOK, terminalOnlyResponsesStream)
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	rec := postRejecting(t, srv, "/v1/responses", liveTimingsStreamBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("relayed body does not carry the injected key: %s", prov.gotBody)
	}
	if prov.recordCalls != 0 {
		t.Fatalf("RecordLiveProgressRejection calls = %d, want 0 for a 200", prov.recordCalls)
	}
	// The sharpest of the three negatives: this is the feature's HAPPY path, so a
	// warning here would fire on every flagged request an operator ever serves.
	assertNoLiveTimingsRefusalWarning(t, buf, "the upstream answered 200")
}
