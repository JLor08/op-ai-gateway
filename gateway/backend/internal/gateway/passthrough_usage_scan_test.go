// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
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
