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
	"testing"
	"time"
)

// TestPassthroughUsageSurvivesBeyondCaptureCap reproduces the bug this task
// fixes end-to-end: a response larger than defaultCaptureMaxBytes used to lose
// its trailing usage frame because parsePassthroughUsage ran exactly once,
// after the copy finished, over the SAME capped tee buffer capture uses. The
// content frames here are deliberately built past the cap so the capture tee
// stops accumulating well before the terminal response.completed frame
// arrives — proving usage no longer depends on that budget.
func TestPassthroughUsageSurvivesBeyondCaptureCap(t *testing.T) {
	// The content must clear the cap by a wide margin (not just a byte or two):
	// the copier tees in 32KB reads, and the cap-check only blocks a chunk once
	// respBuf.Len() is ALREADY over the cap, so a body only marginally bigger
	// than the cap can still fit entirely inside the chunks the tee accepts.
	// Building roughly 2x the cap before the terminal frame guarantees it lands
	// in a chunk the old (respBuf-based) parse would never see.
	var sb strings.Builder
	filler := strings.Repeat("x", 4096)
	for sb.Len() < 2*defaultCaptureMaxBytes {
		sb.WriteString("event: response.output_text.delta\n")
		sb.WriteString(`data: {"type":"response.output_text.delta","delta":"` + filler + `"}`)
		sb.WriteString("\n\n")
	}
	sb.WriteString("event: response.completed\n")
	sb.WriteString(`data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":3,"output_tokens":777,"total_tokens":780}}}`)
	sb.WriteString("\n\n")

	if sb.Len() <= 2*defaultCaptureMaxBytes {
		t.Fatalf("test body = %d bytes, want > %d (2x defaultCaptureMaxBytes)", sb.Len(), 2*defaultCaptureMaxBytes)
	}

	prov := &recordingProxyProvider{respBody: sb.String()}
	srv := newNativeProxyTestServer(prov, true, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].OutputTokens != 777 {
		t.Fatalf("OutputTokens = %d, want 777 (the terminal usage frame beyond the capture cap must still be recorded)", events[0].OutputTokens)
	}
	if events[0].InputTokens != 3 {
		t.Fatalf("InputTokens = %d, want 3", events[0].InputTokens)
	}
}

// TestParsePassthroughUsageReadsResponsesTimings pins step 6: llama.cpp
// attaches a `timings` object (prompt_per_second / predicted_per_second) to the
// terminal response.completed frame of a /v1/responses stream. Before this
// change parsePassthroughUsage never looked at it at all, so a passthrough
// Responses request always persisted 0.0 tokens/sec regardless of what the
// upstream actually reported.
func TestParsePassthroughUsageReadsResponsesTimings(t *testing.T) {
	body := "event: response.completed\ndata: " +
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}},"timings":{"prompt_per_second":120.5,"predicted_per_second":38.25}}` +
		"\n\n"
	u := parsePassthroughUsage("openai_responses", []byte(body))
	if u.PromptPerSecond != 120.5 {
		t.Fatalf("PromptPerSecond = %v, want 120.5", u.PromptPerSecond)
	}
	if u.TokensPerSecond != 38.25 {
		t.Fatalf("TokensPerSecond = %v, want 38.25", u.TokensPerSecond)
	}
}

// TestParsePassthroughUsageReadsResponsesDraftTokens carries Task 4's
// drafted-token counter (llama.cpp's `timings.draft_n`) through the same
// Responses-stream merge path TestParsePassthroughUsageReadsResponsesTimings
// proves above for prompt_per_second/predicted_per_second -- this is the
// Responses-API stream's terminal frame, the only Responses shape that
// carries `timings` at all.
func TestParsePassthroughUsageReadsResponsesDraftTokens(t *testing.T) {
	body := "event: response.completed\ndata: " +
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}},"timings":{"prompt_per_second":120.5,"predicted_per_second":38.25,"draft_n":9}}` +
		"\n\n"
	u := parsePassthroughUsage("openai_responses", []byte(body))
	if u.DraftTokens != 9 {
		t.Fatalf("DraftTokens = %v, want 9", u.DraftTokens)
	}
}

// TestParsePassthroughUsageResponsesDraftTokensAbsentIsZero pins the negative
// that matters: llama.cpp's server only emits `draft_n` under
// `if (n_draft_tokens > 0)`, so a `timings` object that carries its OTHER
// fields but no `draft_n` key -- exactly what a non-speculating endpoint
// sends -- must decode DraftTokens as 0, never infer a count from the
// sibling fields that ARE present.
func TestParsePassthroughUsageResponsesDraftTokensAbsentIsZero(t *testing.T) {
	body := "event: response.completed\ndata: " +
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}},"timings":{"prompt_per_second":120.5,"predicted_per_second":38.25}}` +
		"\n\n"
	u := parsePassthroughUsage("openai_responses", []byte(body))
	if u.DraftTokens != 0 {
		t.Fatalf("DraftTokens = %v, want 0 (no draft_n key on the timings object)", u.DraftTokens)
	}
}

// TestMergeResponsesUsageDraftTokensTakesRunningMax proves DraftTokens gets
// the SAME running-max treatment (takeMax) as its timings siblings above --
// never a plain overwrite, never a sum -- across repeated merges of the same
// stream, exactly as mergeResponsesUsage is called once per SSE frame.
func TestMergeResponsesUsageDraftTokensTakesRunningMax(t *testing.T) {
	var u inference.Usage
	mergeResponsesUsage(&u, []byte(`{"timings":{"draft_n":9}}`))
	mergeResponsesUsage(&u, []byte(`{"timings":{"draft_n":3}}`))
	if u.DraftTokens != 9 {
		t.Fatalf("DraftTokens = %v, want 9 (a later, smaller draft_n must not overwrite the running max)", u.DraftTokens)
	}
	mergeResponsesUsage(&u, []byte(`{"timings":{"draft_n":12}}`))
	if u.DraftTokens != 12 {
		t.Fatalf("DraftTokens = %v, want 12 (a later, larger draft_n must raise the running max)", u.DraftTokens)
	}
}

// TestPassthroughAnthropicRateUsesGenerationWindow pins step 7's ambiguity
// resolution #4: the Anthropic fallback rate is computed over the GENERATION
// WINDOW (first content frame -> completion), never the whole request. Here
// message_start (no content) arrives at t+0, the first content_block_delta at
// t+1s, and the terminal message_delta (completion, output_tokens=40) at t+3s.
// The correct rate is 40 tokens / 2s = 20.0 -- NOT 40/3s = 13.3, which is what
// a whole-request window (or mistaking message_start for content) would yield.
func TestPassthroughAnthropicRateUsesGenerationWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)

	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"), base)
	s.feed([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"), base.Add(time.Second))
	s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"), base.Add(3*time.Second))

	u := s.usage()
	if u.TokensPerSecond != 20.0 {
		t.Fatalf("TokensPerSecond = %v, want 20.0 (40 tokens over the 2s generation window, not 13.3 over the 3s whole request)", u.TokensPerSecond)
	}
	if u.OutputTokens != 40 {
		t.Fatalf("OutputTokens = %d, want 40", u.OutputTokens)
	}
}

// TestPassthroughAnthropicFallbackNeedsAContentFrame proves the fallback rate
// stays 0 -- never estimated -- when no content frame was ever observed (e.g.
// the stream errors out right after message_start), even though an output
// token count and elapsed time both exist.
func TestPassthroughAnthropicFallbackNeedsAContentFrame(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)

	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"), base)
	s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"), base.Add(3*time.Second))

	if got := s.usage().TokensPerSecond; got != 0 {
		t.Fatalf("TokensPerSecond = %v, want 0 (no content frame was ever observed)", got)
	}
}

// TestPassthroughAnthropicFallbackFloorsTheGenerationWindow pins #51's
// final-review finding: an authoritative message_delta that arrives a hair
// after the first content frame must not produce an implausible rate. Both
// cases have the SAME 40-token count and only differ in the generation
// window's width, straddling minGatewayRateWindow (50ms) on either side --
// this is deliberate: a fixture that only exercised one side could pass with
// the floor deleted entirely, whereas pinning both the suppressed 49ms case
// AND the honored, exactly-computed 51ms case cannot.
func TestPassthroughAnthropicFallbackFloorsTheGenerationWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	messageStart := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n")
	contentDelta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
	messageDelta := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n")

	t.Run("just under the floor is suppressed", func(t *testing.T) {
		s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)
		s.feed(messageStart, base)
		s.feed(contentDelta, base) // first content frame at t+0
		s.feed(messageDelta, base.Add(49*time.Millisecond))

		u := s.usage()
		if u.TokensPerSecond != 0 {
			t.Fatalf("TokensPerSecond = %v, want 0 (49ms generation window is below the 50ms floor)", u.TokensPerSecond)
		}
		if u.OutputTokens != 40 {
			t.Fatalf("OutputTokens = %d, want 40 (the count itself is unaffected by the rate floor)", u.OutputTokens)
		}
	})

	t.Run("just over the floor is honored exactly", func(t *testing.T) {
		s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)
		s.feed(messageStart, base)
		s.feed(contentDelta, base) // first content frame at t+0
		s.feed(messageDelta, base.Add(51*time.Millisecond))

		u := s.usage()
		want := 40.0 / 0.051
		if u.TokensPerSecond != want {
			t.Fatalf("TokensPerSecond = %v, want %v (40 tokens over the 51ms generation window)", u.TokensPerSecond, want)
		}
	})
}

// TestUsageScannerCarryBoundDropsOnPathologicalLine pins the bounded-carry
// contract: a "line" (no newline in sight) that grows past capBytes is dropped
// rather than retained and grown further, so a pathological or hostile
// upstream cannot make the gateway allocate without limit while scanning.
func TestUsageScannerCarryBoundDropsOnPathologicalLine(t *testing.T) {
	s := newUsageScanner("openai_responses", 16)
	s.feed([]byte("0123456789"), time.Now()) // 10 bytes, within bound
	if len(s.carry) != 10 {
		t.Fatalf("carry = %d bytes after first feed, want 10", len(s.carry))
	}
	s.feed([]byte("0123456789"), time.Now()) // now 20 bytes > 16 -> dropped
	if len(s.carry) != 0 {
		t.Fatalf("carry = %d bytes, want 0 (dropped once the bound was exceeded)", len(s.carry))
	}
}

// TestUsageScannerFinishRecoversUnterminatedFinalLine proves finish is what
// makes the scanner work at all for a buffered (non-streaming) response: such
// a body is typically ONE JSON object with no embedded newline, so feed alone
// (which only acts on complete '\n'-terminated lines) would never see it.
func TestUsageScannerFinishRecoversUnterminatedFinalLine(t *testing.T) {
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes)
	body := []byte(`{"id":"r","usage":{"input_tokens":5,"output_tokens":9}}`) // no trailing newline

	s.feed(body, time.Now())
	if got := s.usage().OutputTokens; got != 0 {
		t.Fatalf("OutputTokens = %d before finish, want 0 (no complete line yet)", got)
	}

	s.finish(time.Now())
	if got := s.usage().OutputTokens; got != 9 {
		t.Fatalf("OutputTokens = %d after finish, want 9 (the unterminated final line must still be scanned)", got)
	}
}

// TestUsageScannerTotalTokensAcrossSplitFrames proves the deferred-finalize
// design in mergePassthroughUsage/finalizeTotalTokens: Anthropic reports input
// tokens on message_start and output tokens on a LATER, separate message_delta
// frame. A naive per-fragment TotalTokens default (input+output computed on
// each fragment in isolation, then max-merged across fragments) would yield
// max(8+1, 0+40) = 40, not the true 48.
func TestUsageScannerTotalTokensAcrossSplitFrames(t *testing.T) {
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)
	now := time.Now()
	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"), now)
	s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"), now)

	u := s.usage()
	if u.InputTokens != 8 || u.OutputTokens != 40 || u.TotalTokens != 48 {
		t.Fatalf("usage = %+v, want input=8 output=40 total=48", u)
	}
}

// TestIsContentFrame pins the per-flavor "content frame" definitions the
// generation-window fallback (and its doc comment) depend on.
func TestIsContentFrame(t *testing.T) {
	cases := []struct {
		flavor  string
		payload string
		want    bool
	}{
		{"anthropic_messages", `{"type":"content_block_delta","delta":{}}`, true},
		{"anthropic_messages", `{"type":"message_start"}`, false},
		{"anthropic_messages", `{"type":"content_block_start"}`, false},
		{"anthropic_messages", `{"type":"message_delta"}`, false},
		{"openai_responses", `{"type":"response.output_text.delta"}`, true},
		{"openai_responses", `{"type":"response.reasoning_text.delta"}`, true},
		{"openai_responses", `{"type":"response.function_call_arguments.delta"}`, true},
		{"openai_responses", `{"type":"response.created"}`, false},
		{"openai_responses", `{"type":"response.output_item.added"}`, false},
		{"openai_responses", `{"type":"response.completed"}`, false},
	}
	for _, tc := range cases {
		if got := isContentFrame(tc.flavor, []byte(tc.payload)); got != tc.want {
			t.Fatalf("isContentFrame(%q, %q) = %v, want %v", tc.flavor, tc.payload, got, tc.want)
		}
	}
}

// TestPassthroughAnthropicFallbackNeedsAnAuthoritativeTerminalUsageFrame is the
// placeholder-only stream: message_start's usage.output_tokens is Anthropic's
// PLACEHOLDER (1), a content frame does arrive, the stream closes cleanly with
// message_stop 20s later -- and no message_delta ever reports the real total.
// Deriving a rate here would record 1/20s = 0.05 t/s as a MEASURED sample, and a
// recorded rate also feeds an opted-in mapping's throughput EWMA, so the invented
// figure would become a routing input. No authoritative terminal usage frame, no
// rate.
func TestPassthroughAnthropicFallbackNeedsAnAuthoritativeTerminalUsageFrame(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes)

	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"), base)
	s.feed([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"), base.Add(time.Second))
	s.feed([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), base.Add(21*time.Second))

	u := s.usage()
	if u.TokensPerSecond != 0 {
		t.Fatalf("TokensPerSecond = %v, want 0 (message_start's output_tokens is a placeholder and message_stop carries no count)", u.TokensPerSecond)
	}
	// The placeholder count itself is still recorded -- only the derived RATE is
	// gated, since the rate is the value that would be presented as measured and
	// blended into the routing EWMA.
	if u.OutputTokens != 1 {
		t.Fatalf("OutputTokens = %d, want 1 (the merged count is unchanged by this gate)", u.OutputTokens)
	}
}

// TestIsTerminalUsageFrame pins the per-flavor "authoritative terminal usage
// frame" definitions the gated fallback (and its doc comment) depend on. The two
// Anthropic negatives are the point: message_start carries a placeholder count,
// and message_stop is terminal but carries no count at all.
func TestIsTerminalUsageFrame(t *testing.T) {
	cases := []struct {
		flavor  string
		payload string
		want    bool
	}{
		{"anthropic_messages", `{"type":"message_delta","usage":{"output_tokens":40}}`, true},
		{"anthropic_messages", `{"type":"message","usage":{"output_tokens":40}}`, true},
		{"anthropic_messages", `{"type":"message_start","message":{"usage":{"output_tokens":1}}}`, false},
		{"anthropic_messages", `{"type":"message_stop"}`, false},
		{"anthropic_messages", `{"type":"content_block_delta"}`, false},
		{"openai_responses", `{"type":"response.completed","response":{"usage":{}}}`, true},
		{"openai_responses", `{"type":"response.in_progress"}`, false},
		{"openai_responses", `{"type":"response.output_text.delta"}`, false},
		{"", `{"type":"message_delta"}`, false},
		{"anthropic_messages", `not json`, false},
	}
	for _, tc := range cases {
		if got := isTerminalUsageFrame(tc.flavor, []byte(tc.payload)); got != tc.want {
			t.Fatalf("isTerminalUsageFrame(%q, %q) = %v, want %v", tc.flavor, tc.payload, got, tc.want)
		}
	}
}

// TestPassthroughRecordsResponsesUpstreamRateOnTheUsageEvent closes the branch's
// headline claim for the Responses flavor at the level it was actually made:
// before this branch the completed-activity table reported 0 tokens/sec for
// every /v1/responses request. The existing coverage stopped at
// parsePassthroughUsage's return value; this asserts the number reaches the
// recorded usage_events row.
func TestPassthroughRecordsResponsesUpstreamRateOnTheUsageEvent(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}},"timings":{"prompt_per_second":120.5,"predicted_per_second":38.25}}` +
		"\n\n"
	prov := &recordingProxyProvider{respBody: body}
	srv := newNativeProxyTestServer(prov, true, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].TokensPerSecond != 38.25 {
		t.Fatalf("recorded TokensPerSecond = %v, want 38.25 (llama.cpp's own predicted_per_second off the terminal frame)", events[0].TokensPerSecond)
	}
	if events[0].PromptPerSecond != 120.5 {
		t.Fatalf("recorded PromptPerSecond = %v, want 120.5", events[0].PromptPerSecond)
	}
}

// pacedProxyBody serves a native-passthrough response body across multiple
// Reads, each held back by gap, so a test can force REAL elapsed wall-clock
// time between two SSE frames rather than two back-to-back time.Now() calls a
// few nanoseconds apart (which an ordinary in-process httptest round trip
// would otherwise produce). Modeled on server_stream_timeout_test.go's
// trickleReader.
type pacedProxyBody struct {
	pieces []string
	gap    time.Duration
}

func (r *pacedProxyBody) Read(p []byte) (int, error) {
	if len(r.pieces) == 0 {
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

// pacedNativeProxyProvider is recordingProxyProvider's ProxyNative, minus the
// call-recording fields this package's tests don't need, with its body served
// through pacedProxyBody instead of a single strings.Reader -- so the caller
// controls how much real wall-clock time separates one SSE frame from the
// next.
type pacedNativeProxyProvider struct {
	pieces []string
	gap    time.Duration
}

func (pacedNativeProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (pacedNativeProxyProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{}})
}

func (p pacedNativeProxyProvider) ProxyNative(context.Context, routing.Target, string, []byte) (*provider.ProxyResponse, error) {
	return &provider.ProxyResponse{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&pacedProxyBody{pieces: append([]string(nil), p.pieces...), gap: p.gap}),
	}, nil
}

// TestPassthroughRecordsAnthropicDerivedRateOnTheUsageEvent is the same claim for
// the Anthropic flavor, which carries no timings on any frame and therefore
// depends on usageScanner's derived rate. The exact value is not assertable end
// to end, so the assertion is the one that actually changed: a POSITIVE
// recorded rate where the pre-branch value was always exactly 0.
//
// The body is deliberately PACED (content_block_delta, then a real 80ms
// sleep, then message_delta) rather than served in one shot: an ordinary
// in-process httptest round trip copies the whole canned body in a single
// Read, so the scanner's first-content and last-activity timestamps would
// otherwise be two time.Now() calls a few nanoseconds apart -- exactly the
// implausible-window case Task 7's floor (minGatewayRateWindow, mirrored from
// request_progress.go) now suppresses. Without the real gap this test would
// assert on the very bug that floor exists to prevent.
func TestPassthroughRecordsAnthropicDerivedRateOnTheUsageEvent(t *testing.T) {
	prov := pacedNativeProxyProvider{
		pieces: []string{
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
			"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":40}}` + "\n\n" +
				"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
		gap: 80 * time.Millisecond,
	}
	srv := newNativeProxyTestServer(prov, false, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].OutputTokens != 40 {
		t.Fatalf("recorded OutputTokens = %d, want 40", events[0].OutputTokens)
	}
	if events[0].TokensPerSecond <= 0 {
		t.Fatalf("recorded TokensPerSecond = %v, want > 0 (the derived rate must reach the usage_events row, not just the scanner)", events[0].TokensPerSecond)
	}
}

// TestPassthroughPlaceholderOnlyAnthropicStreamRecordsNoRate closes F3 for the
// other direction: it is not enough for a POSITIVE rate to reach the recorded
// usage_events row (TestPassthroughRecordsAnthropicDerivedRateOnTheUsageEvent
// above); the fix this row exists to prove is that a stream whose only usage
// frame is message_start's PLACEHOLDER (`output_tokens: 1`) must NOT leave a
// derived rate on that row either, because a recorded rate is a routing input
// (recordUsage -> UpdateMappingOpportunisticMetrics) once opportunistic metrics
// are enabled. Unlike the positive test, this stream never sends message_delta,
// so isTerminalUsageFrame's gate is the only thing standing between this stream
// and an invented `1 / window` sample landing in the row — asserting on the
// scanner alone (TestPassthroughAnthropicFallbackNeedsAnAuthoritativeTerminalUsageFrame)
// does not prove the gate actually reaches the recorded event through the full
// native-passthrough server.
func TestPassthroughPlaceholderOnlyAnthropicStreamRecordsNoRate(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	prov := &recordingProxyProvider{respBody: body}
	srv := newNativeProxyTestServer(prov, false, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	// The placeholder count itself is still recorded — only the derived rate is
	// gated, since the rate is what would be presented as measured and blended
	// into the mapping's throughput EWMA.
	if events[0].OutputTokens != 1 {
		t.Fatalf("recorded OutputTokens = %d, want 1 (the merged placeholder count is unchanged by this gate)", events[0].OutputTokens)
	}
	if events[0].TokensPerSecond != 0 {
		t.Fatalf("recorded TokensPerSecond = %v, want 0 (message_start's output_tokens is a placeholder, not an authoritative terminal usage frame, and must not reach the row as a measured rate)", events[0].TokensPerSecond)
	}
}
