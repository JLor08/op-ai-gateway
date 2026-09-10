// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file pin the per-flavor completeness table in
// docs/superpowers/specs/2026-09-10-passthrough-progress-design.md §3 — the one
// that says what a NATIVE-PASSTHROUGH streaming request can honestly report on
// the running-connections panel, per API flavor. The asymmetry between the two
// columns is the design, so each row is asserted in BOTH directions: a value
// where one exists, and an explicit absence where none does. A later change that
// satisfied the Responses column by counting delta frames as tokens, or by
// deriving a second rate inside the scanner, has to break one of these.
//
// Every one of them observes the row while the request is still IN FLIGHT (see
// progressObservingProxyProvider), through liveProgressDTO — the exact
// resolution GET /api/portal/usage/active performs — rather than inspecting the
// counters directly, so the assertion is about what the panel would display.

// liveRow is one in-flight request's panel-visible progress, as
// snapshotLiveRow resolved it.
type liveRow struct {
	// hasProgress records whether the ActiveRequest carried a counter at all,
	// which is the buffered case's whole claim (nil, not zero).
	hasProgress  bool
	outputTokens int
	tps          float64
	source       string
	ttftMS       int64
}

// snapshotLiveRow resolves the single in-flight request's live progress exactly
// as the active-requests endpoint does. Called from inside the upstream body's
// final Read, so it runs on the request's own goroutine while the row is still
// registered.
func snapshotLiveRow(t *testing.T, srv *Server) liveRow {
	t.Helper()
	rows := srv.Active.Snapshot()
	if len(rows) != 1 {
		t.Errorf("in-flight rows = %d, want exactly 1 (the request being served)", len(rows))
		return liveRow{}
	}
	row := rows[0]
	tokens, tps, source, ttft := liveProgressDTO(row, time.Now().UTC())
	return liveRow{hasProgress: row.Progress != nil, outputTokens: tokens, tps: tps, source: source, ttftMS: ttft}
}

// progressObservingProxyProvider serves a native-passthrough response body in
// timed pieces — like pacedNativeProxyProvider, so REAL wall-clock time separates
// one SSE frame from the next instead of two time.Now() calls a few nanoseconds
// apart — and invokes observe from inside the final, EOF-returning Read.
//
// That final Read is the one point where a test can see the finished picture of a
// live row: every piece has been written to the client AND fed to the usage
// scanner (nativeCopier.writeChunk feeds it per chunk), the paced gaps have put
// a measurable window between the first content frame and now, and proxyNative's
// deferred Active.Remove has not run yet. Observing from ProxyNative's head
// instead (recordingProxyProvider.onProxy) would only ever see a row with no
// frames scanned at all.
type progressObservingProxyProvider struct {
	pieces  []string
	gap     time.Duration
	observe func()
	// gotBody is the body actually relayed upstream, kept so a test can assert
	// what the gateway did and did NOT add to it.
	gotBody []byte
}

func (*progressObservingProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (*progressObservingProxyProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{}})
}

func (p *progressObservingProxyProvider) ProxyNative(_ context.Context, _ routing.Target, _ string, body []byte) (*provider.ProxyResponse, error) {
	p.gotBody = append([]byte(nil), body...)
	return &provider.ProxyResponse{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(&observingProxyBody{
			pieces:  append([]string(nil), p.pieces...),
			gap:     p.gap,
			observe: p.observe,
		}),
	}, nil
}

// observingProxyBody is pacedProxyBody plus the one-shot observe hook fired on
// the EOF read (see progressObservingProxyProvider).
type observingProxyBody struct {
	pieces  []string
	gap     time.Duration
	observe func()
}

func (r *observingProxyBody) Read(p []byte) (int, error) {
	if len(r.pieces) == 0 {
		if r.observe != nil {
			r.observe()
			r.observe = nil
		}
		return 0, io.EOF
	}
	time.Sleep(r.gap)
	n := copy(p, r.pieces[0])
	r.pieces[0] = r.pieces[0][n:]
	if r.pieces[0] == "" {
		r.pieces = r.pieces[1:]
	}
	return n, nil
}

// framePacing is the wall-clock gap forced between two upstream frames in these
// tests. It has to clear minGatewayRateWindow (50ms) with margin, because these
// assertions are about numbers the panel DISPLAYS and that floor deliberately
// suppresses a rate measured over a shorter window; it also has to exceed 1ms so
// a TTFT expressed in whole milliseconds is non-zero.
const framePacing = 80 * time.Millisecond

// TestPassthroughAnthropicStreamShowsLiveTTFTTokensAndDerivedRate pins the
// `anthropic_messages` column: all three cells are populated. The token count is
// the upstream's own cumulative figure off message_delta, and the rate is
// liveProgressDTO's window derivation over it — the only source there is, since
// llama.cpp attaches no `timings` object to any Anthropic frame, exactly as for
// this flavor's END-of-request rate (usageScanner.usage).
func TestPassthroughAnthropicStreamShowsLiveTTFTTokensAndDerivedRate(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
			"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":40}}` + "\n\n",
		},
		gap: framePacing,
	}
	srv := newNativeProxyTestServer(prov, false, true)
	prov.observe = func() { got = snapshotLiveRow(t, srv) }

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !got.hasProgress {
		t.Fatalf("in-flight row carried no Progress counter at all: a streaming passthrough request must get one")
	}
	if got.ttftMS <= 0 {
		t.Fatalf("live ttft_ms = %d, want > 0 (the first content_block_delta arrived %v after the request started)", got.ttftMS, framePacing)
	}
	if got.outputTokens != 40 {
		t.Fatalf("live output_tokens = %d, want 40 (message_delta's own cumulative usage.output_tokens, not a gateway count)", got.outputTokens)
	}
	if got.source != "gateway" {
		t.Fatalf("live tokens_per_second_source = %q, want %q (Anthropic frames carry no timings, so the window derivation is the only source)", got.source, "gateway")
	}
	if got.tps <= 0 {
		t.Fatalf("live tokens_per_second = %v, want > 0 (40 tokens over the generation window)", got.tps)
	}
}

// TestPassthroughAnthropicPlaceholderTokensNeverReachTheLiveRow is the negative
// that keeps the row above honest. message_start's `output_tokens: 1` is a
// PLACEHOLDER that mergePassthroughUsage cannot tell from a real total, and the
// live column has more to lose by it than the recorded row does: liveProgressDTO
// would divide that 1 by the generation window and DISPLAY the quotient as a
// measured rate for the whole rest of the stream. (The recorded row's own
// placeholder gate is TestPassthroughPlaceholderOnlyAnthropicStreamRecordsNoRate;
// this is the same discipline one surface further forward.)
//
// TWO independent rules keep it out, and the two cases below isolate them,
// because the realistic frame order exercises only the first — a mutation
// removing the second passed until this case was split out:
//
//   - "message_start first" is the real Anthropic ordering. The placeholder
//     arrives BEFORE any content frame, and nothing at all is published before
//     the first content frame (publishing earlier would also stamp the row's
//     TTFT off a bookkeeping frame).
//
//   - "placeholder after content" is deliberately adversarial: no llama.cpp
//     stream orders its frames this way. It is here so the "only an
//     AUTHORITATIVE usage frame may publish a count" rule is pinned
//     STRUCTURALLY rather than left to hold by accident of arrival order — an
//     upstream that repeats or reorders its bookkeeping frames must not be able
//     to put a 1 in the panel.
func TestPassthroughAnthropicPlaceholderTokensNeverReachTheLiveRow(t *testing.T) {
	const messageStart = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n"
	const contentDelta = "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n"

	for _, tc := range []struct {
		name   string
		pieces []string
	}{
		{"message_start first (the real ordering)", []string{messageStart + contentDelta, contentDelta}},
		{"placeholder after content (adversarial)", []string{contentDelta, messageStart}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got liveRow
			prov := &progressObservingProxyProvider{pieces: tc.pieces, gap: framePacing}
			srv := newNativeProxyTestServer(prov, false, true)
			prov.observe = func() { got = snapshotLiveRow(t, srv) }

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.ttftMS <= 0 {
				t.Fatalf("live ttft_ms = %d, want > 0 (content did arrive; only the token count is in question here)", got.ttftMS)
			}
			if got.outputTokens != 0 {
				t.Fatalf("live output_tokens = %d, want 0 (message_start's output_tokens is a placeholder, not a count to display)", got.outputTokens)
			}
			if got.tps != 0 || got.source != "" {
				t.Fatalf("live rate = %v (source %q), want 0/\"\" — a rate derived from the placeholder count would be displayed as measured", got.tps, got.source)
			}
		})
	}
}

// TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly pins the
// `openai_responses` column in the ordinary case: TTFT yes, and then two
// explicit absences. The `*.delta` partials carry no usage object, and counting
// them as tokens is rejected policy in this repo — there is no tokenizer to
// count with — so the token count has no honest source; with no count and no
// upstream `timings`, neither does a rate.
//
// It also pins the non-injection rule from the other side: the relayed body must
// not have grown a `timings_per_token` flag the client never sent, which is the
// one edit that would turn this row's two em-dashes into numbers.
func TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n",
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":" there"}` + "\n\n",
		},
		gap: framePacing,
	}
	srv := newNativeProxyTestServer(prov, true, false)
	prov.observe = func() { got = snapshotLiveRow(t, srv) }

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !got.hasProgress {
		t.Fatalf("in-flight row carried no Progress counter at all: a streaming passthrough request must get one")
	}
	if got.ttftMS <= 0 {
		t.Fatalf("live ttft_ms = %d, want > 0 (the first response.output_text.delta arrived %v in)", got.ttftMS, framePacing)
	}
	if got.outputTokens != 0 {
		t.Fatalf("live output_tokens = %d, want 0 (Responses partials carry no usage; a delta is not a token)", got.outputTokens)
	}
	if got.tps != 0 || got.source != "" {
		t.Fatalf("live rate = %v (source %q), want 0/\"\" (no upstream timings, and no count to derive one from)", got.tps, got.source)
	}
	if strings.Contains(string(prov.gotBody), "timings_per_token") {
		t.Fatalf("relayed body grew a timings_per_token flag the client never sent: %s", prov.gotBody)
	}
}

// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate pins the
// one cell of the `openai_responses` column that a CLIENT can fill: setting
// llama.cpp's `timings_per_token` makes the upstream attach a `timings` object to
// PARTIAL frames, and that rate is a real upstream measurement, so it is
// displayed and labelled "upstream" rather than "gateway" — the label is what
// makes a client-dependent difference in completeness visible instead of
// mysterious. The token count stays absent: `timings_per_token` buys a rate, not
// a count this gateway may claim.
//
// The two frames report a DECREASING rate on purpose. llama.cpp's
// predicted_per_second is a cumulative average over the generation, so it
// commonly falls as the KV cache grows; the assertion is on the LATEST value, so
// an implementation that fed the accumulator's running max would pin the column
// at the stream's early peak (42.5) and fail here.
func TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi","timings":{"predicted_per_second":42.5}}` + "\n\n",
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":" there","timings":{"predicted_per_second":38.25}}` + "\n\n",
		},
		gap: framePacing,
	}
	srv := newNativeProxyTestServer(prov, true, false)
	prov.observe = func() { got = snapshotLiveRow(t, srv) }

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi","timings_per_token":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.ttftMS <= 0 {
		t.Fatalf("live ttft_ms = %d, want > 0", got.ttftMS)
	}
	if got.source != "upstream" {
		t.Fatalf("live tokens_per_second_source = %q, want %q (this rate is llama.cpp's own measurement, not the gateway's)", got.source, "upstream")
	}
	if got.tps != 38.25 {
		t.Fatalf("live tokens_per_second = %v, want 38.25 (the MOST RECENT frame's predicted_per_second, not the running max 42.5)", got.tps)
	}
	if got.outputTokens != 0 {
		t.Fatalf("live output_tokens = %d, want 0 (timings_per_token yields a rate; the partials still report no usage)", got.outputTokens)
	}
	// The flag is present upstream because the CLIENT sent it — read, never
	// injected. rewriteModelField re-serializes the object, so only the field's
	// survival is asserted, not byte identity.
	if !strings.Contains(string(prov.gotBody), `"timings_per_token":true`) {
		t.Fatalf("relayed body dropped the client's own timings_per_token flag: %s", prov.gotBody)
	}
}

// TestPassthroughBufferedRequestKeepsNilProgress pins the buffered row: no
// progress at all, deliberately. A non-streaming passthrough response arrives as
// one payload — there are no frames to time, no first-content stamp can form,
// and a "TTFT" measured off it would just be the total request duration wearing
// another quantity's name. nil (not an allocated, permanently-zero struct) is
// what makes the panel show "never measured" rather than "measured 0".
//
// The recorded usage row is asserted too: the nil path must not cost the
// buffered response its token accounting, which is what a panic or an early
// return on the nil counter would look like.
func TestPassthroughBufferedRequestKeepsNilProgress(t *testing.T) {
	var got liveRow
	observed := false
	prov := &progressObservingProxyProvider{
		pieces: []string{`{"type":"message","usage":{"input_tokens":8,"output_tokens":40}}`},
		gap:    framePacing,
	}
	srv := newNativeProxyTestServer(prov, false, true)
	prov.observe = func() {
		observed = true
		got = snapshotLiveRow(t, srv)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !observed {
		t.Fatalf("the in-flight row was never observed: the buffered claim was not actually exercised")
	}
	if got.hasProgress {
		t.Fatalf("a buffered passthrough request was given a Progress counter: it must stay nil (nothing to measure), not be allocated and read zero")
	}
	if got.outputTokens != 0 || got.tps != 0 || got.source != "" || got.ttftMS != 0 {
		t.Fatalf("buffered live progress = %+v, want every field zero/empty", got)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].OutputTokens != 40 {
		t.Fatalf("recorded OutputTokens = %d, want 40 (the nil live counter must not disturb the recorded row)", events[0].OutputTokens)
	}
}

// countingOpportunisticStore counts UpdateMappingOpportunisticMetrics calls and
// forwards them, so a test can assert HOW MANY writes a request performed rather
// than what they contained — the only assertion that catches a second, in-flight
// write, since an extra write carrying an identical-looking value would leave the
// stored number indistinguishable.
type countingOpportunisticStore struct {
	routing.Store
	calls atomic.Int64
}

func (c *countingOpportunisticStore) UpdateMappingOpportunisticMetrics(ctx context.Context, id string, genSample, promptSample, alpha float64, at time.Time) error {
	c.calls.Add(1)
	return c.Store.UpdateMappingOpportunisticMetrics(ctx, id, genSample, promptSample, alpha, at)
}

// TestPassthroughLiveProgressWritesNoRoutingInput pins decision (c): the
// in-flight figure is DISPLAY ONLY. The end-of-request rate feeds
// UpdateMappingOpportunisticMetrics' EWMA, which the scorer and a model group's
// MinTokensPerSecond gate read; an in-flight sample is measured over a shorter,
// mid-generation window and would never self-correct once blended in there.
//
// The opt-in is deliberately switched ON and the stream deliberately produces a
// positive end-of-request rate, so the expected count is 1, not 0: a test with
// the opt-in off would pass no matter what this path did. One call means the
// single write that existed before this change, and no other.
func TestPassthroughLiveProgressWritesNoRoutingInput(t *testing.T) {
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
			"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":40}}` + "\n\n",
		},
		gap: framePacing,
	}
	routes := &countingOpportunisticStore{Store: routing.NewMemoryStore()}
	srv := newNativeModeTestServerOn(prov, routing.EndpointModeTranslate, routing.EndpointModePassthrough, routes, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].TokensPerSecond <= 0 {
		t.Fatalf("usage events = %+v, want exactly 1 with a positive rate (otherwise the end-of-request write would not have fired and this count would prove nothing)", events)
	}
	if got := routes.calls.Load(); got != 1 {
		t.Fatalf("UpdateMappingOpportunisticMetrics calls = %d, want exactly 1 (the unchanged end-of-request write); anything more is an in-flight sample reaching a routing input", got)
	}
}

// TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves pins the
// `openai_responses` cell the other two Responses tests leave uncovered: the
// flavor has no MID-STREAM source for a count, but the TERMINAL
// response.completed frame carries the upstream's own final
// `response.usage.output_tokens`, isTerminalUsageFrame accepts it, and
// publishProgress therefore puts it on the row. For the brief window between
// that frame and proxyNative's deferred Active.Remove, the still-active row
// displays that count AND the window rate liveProgressDTO derives from it.
//
// That is deliberate, not a leak: the figure is the upstream's own count over
// the real generation window — the same arithmetic and the same `gateway` label
// the Anthropic column carries for its whole stream. Only the mid-stream cells
// are empty here.
//
// It is also where "a delta is not a token" is pinned at its sharpest. The other
// two Responses tests assert an ABSENT count, which an implementation could
// satisfy by publishing nothing at all; this one asserts the displayed count is
// EXACTLY the upstream's 40 while two content deltas went past, so any
// gateway-counted contribution added to the upstream's figure shows up here as
// 42.
func TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n",
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":" there"}` + "\n\n",
			"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_x","usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}}}` + "\n\n",
		},
		gap: framePacing,
	}
	srv := newNativeProxyTestServer(prov, true, false)
	prov.observe = func() { got = snapshotLiveRow(t, srv) }

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !got.hasProgress {
		t.Fatalf("in-flight row carried no Progress counter at all: a streaming passthrough request must get one")
	}
	if got.ttftMS <= 0 {
		t.Fatalf("live ttft_ms = %d, want > 0 (the first response.output_text.delta arrived %v in)", got.ttftMS, framePacing)
	}
	if got.outputTokens != 40 {
		t.Fatalf("live output_tokens = %d, want exactly 40 — response.completed's nested response.usage.output_tokens, the upstream's own count with no delta-derived contribution added to it", got.outputTokens)
	}
	if got.source != "gateway" {
		t.Fatalf("live tokens_per_second_source = %q, want %q (the client set no timings_per_token, so the window derivation over the upstream's own count is the only source)", got.source, "gateway")
	}
	if got.tps <= 0 {
		t.Fatalf("live tokens_per_second = %v, want > 0 (40 tokens over the generation window)", got.tps)
	}
	if strings.Contains(string(prov.gotBody), "timings_per_token") {
		t.Fatalf("relayed body grew a timings_per_token flag the client never sent: %s", prov.gotBody)
	}
}
