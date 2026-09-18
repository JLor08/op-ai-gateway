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

// TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly pins the carrier
// rule at the merge itself: llama.cpp's `timings.predicted_n` becomes
// LiveOutputTokens and touches neither OutputTokens nor TotalTokens.
//
// The rule needs a test of its own because this merge writes to TWO
// destinations. scan (passthrough_usage_scan.go) calls mergePassthroughUsage
// once into a per-frame scratch Usage -- what the live column reads -- and once
// into the scanner's ACCUMULATOR, which usage() hands recordUsage. A field added
// to the timings struct therefore reaches the accumulator BY CONSTRUCTION.
// Choosing a separate field is the only thing that keeps it out of the recorded
// row, and so out of usage_events, the Activity totals, the usage timeseries and
// the principal rate limiter's input. The negative assertion below is what turns
// that from a reviewed fact into a pinned one.
//
// The running max is asserted in both directions for the same reason
// DraftTokens asserts it above: mergeResponsesUsage runs once per SSE frame, and
// feed()'s documented tolerance for scanning a line more than once rests on
// every COUNT this merge writes being monotone. predicted_n is monotone over a
// generation, so a max is its final value.
//
// The last case is a `timings` object with no `predicted_n` key. Scoped
// precisely, because this package contains the counter-example: no measured
// PARTIAL carrying a `timings` object lacked a `predicted_n` (90 of 90 carried
// one), but the merge is not partial-only, and the terminal-frame fixtures two
// files over -- including the subtest named "llama.cpp: the terminal frame
// carries its own timings" -- use exactly this shape, with publishProgress's
// authoritative arm supplying their count. Either way the rule is one rule:
// report no count rather than infer one from the rate sitting beside it.
func TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly(t *testing.T) {
	var u inference.Usage
	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_per_second":38.25,"predicted_n":12}}`))
	if u.LiveOutputTokens != 12 {
		t.Fatalf("LiveOutputTokens = %d, want 12 (timings.predicted_n)", u.LiveOutputTokens)
	}
	if u.OutputTokens != 0 || u.TotalTokens != 0 {
		t.Fatalf("OutputTokens/TotalTokens = %d/%d, want 0/0 — predicted_n must land in NEITHER: those two are what usageScanner.usage hands recordUsage, and thence usage_events, the Activity totals, the timeseries and the rate limiter", u.OutputTokens, u.TotalTokens)
	}

	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_n":9}}`))
	if u.LiveOutputTokens != 12 {
		t.Fatalf("LiveOutputTokens = %d, want 12 (a later, smaller predicted_n must not overwrite the running max)", u.LiveOutputTokens)
	}
	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_n":20}}`))
	if u.LiveOutputTokens != 20 {
		t.Fatalf("LiveOutputTokens = %d, want 20 (a later, larger predicted_n must raise the running max)", u.LiveOutputTokens)
	}

	var absent inference.Usage
	mergeResponsesUsage(&absent, []byte(`{"timings":{"predicted_per_second":38.25}}`))
	if absent.LiveOutputTokens != 0 {
		t.Fatalf("LiveOutputTokens = %d, want 0 — a timings object carrying no predicted_n key reports no count, and none may be inferred from the rate beside it", absent.LiveOutputTokens)
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
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)

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
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)

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
		s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)
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
		s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)
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
	s := newUsageScanner("openai_responses", 16, nil)
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
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
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

// TestUsageScannerBufferedBodyUsageSurvivesPrettyPrinting pins issue #78: a
// buffered (non-streaming) native-passthrough body must yield its usage no
// matter how the JSON is whitespace-formatted. The pretty-printed body whose
// last newline falls BEFORE its closing brace is the case that regressed — feed
// split there, handing jsonPayloads a truncated, unparseable fragment, so ALL
// of the response's usage was lost (in=0 out=0 total=0). That zeroed both the
// persisted usage row AND the token budget the request consumed, invisibly,
// because the request itself still succeeded.
//
// The two already-working rows are kept deliberately: a compact body has no
// newline (feed scans nothing, finish scans the intact body) and a pretty body
// WITH a trailing newline puts the split point after the closing brace. They
// are the two shapes the code was designed around, which is what makes the
// pretty-no-trailing-newline body a gap rather than a design choice — so a fix
// must keep all three green. All three call finish before reading usage,
// matching the real caller (nativeCopier.run defers finish before usage()).
func TestUsageScannerBufferedBodyUsageSurvivesPrettyPrinting(t *testing.T) {
	const prettyNoNL = `{
  "id": "resp_1",
  "usage": {"input_tokens": 5, "output_tokens": 9, "total_tokens": 14}
}`
	cases := []struct {
		name string
		body string
	}{
		{"pretty, no trailing newline", prettyNoNL},
		{"pretty, with trailing newline", prettyNoNL + "\n"},
		{"compact, no newline at all", `{"id":"resp_1","usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
			now := time.Now()
			s.feed([]byte(tc.body), now)
			s.finish(now)
			if u := s.usage(); u.InputTokens != 5 || u.OutputTokens != 9 || u.TotalTokens != 14 {
				t.Fatalf("usage = %+v, want input=5 output=9 total=14", u)
			}
		})
	}
}

// TestUsageScannerBufferedBodyOverCapStillYieldsUsage pins issue #91: a buffered
// (non-streaming) body LARGER than capBytes must still yield its usage. #78
// fixed a buffered body whose newline placement lost the usage; this is its size
// sibling — a buffered body that simply runs past the capture cap. The old feed
// dropped the WHOLE carry the moment it crossed capBytes (`s.carry = nil`),
// taking the trailing `usage` object — and with it the persisted usage row and
// the request's entire token budget — down with it, and silently, because the
// request itself still returned 200. The fix keeps a bounded trailing window of
// the body instead of dropping it, so finish still recovers the usage object
// that every API this path serves places at the END of the value.
//
// The body is fed in small chunks on purpose: that is what a large buffered
// response does through nativeCopier.run (32 KB reads), and it exercises the
// sliding-window trim across feeds, not just a single over-cap append. The carry
// is asserted bounded (<= 2*capBytes, the retained window plus the amortization
// headroom) after EVERY feed, and bufferedTruncated is asserted set before
// finish, so the recovery is proven to run through the truncated path and cannot
// be bought with unbounded memory — the guarantee #91 says the fix must keep.
//
// The pretty-printed row also covers whitespace around the trailing usage key's
// colon and inside its object, since the retained fragment is scanned by hand
// (trailingJSONObject/balancedObject) rather than json.Unmarshaled.
func TestUsageScannerBufferedBodyOverCapStillYieldsUsage(t *testing.T) {
	const capBytes = 256
	// Padding that clears 2*capBytes by a wide margin, so the body forces several
	// trim cycles however the chunk boundaries fall; kept free of the substring
	// "usage" so the tail scan can only find the real trailing key.
	pad := strings.Repeat("A", 10*capBytes)
	prettyUsage := "\n  \"usage\" : {\n    \"input_tokens\": 13,\n    \"output_tokens\": 21,\n    \"total_tokens\": 34\n  }\n"
	cases := []struct {
		name                       string
		apiFlavor                  string
		body                       string
		wantIn, wantOut, wantTotal int
	}{
		{
			name:      "openai_responses",
			apiFlavor: "openai_responses",
			body:      `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":"` + pad + `"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`,
			wantIn:    11, wantOut: 22, wantTotal: 33,
		},
		{
			name:      "anthropic_messages",
			apiFlavor: "anthropic_messages",
			body:      `{"id":"msg_1","type":"message","content":[{"type":"text","text":"` + pad + `"}],"usage":{"input_tokens":7,"output_tokens":40}}`,
			wantIn:    7, wantOut: 40, wantTotal: 47,
		},
		{
			name:      "openai_responses, pretty-printed usage tail",
			apiFlavor: "openai_responses",
			body:      `{"id":"resp_2","object":"response","output":[{"type":"message","content":"` + pad + `"}],` + prettyUsage + `}`,
			wantIn:    13, wantOut: 21, wantTotal: 34,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if len(body) <= 2*capBytes {
				t.Fatalf("test body is %d bytes, must exceed 2*cap=%d to exercise the truncated path", len(body), 2*capBytes)
			}
			s := newUsageScanner(tc.apiFlavor, capBytes, nil)
			now := time.Now()
			for off := 0; off < len(body); off += 40 {
				end := off + 40
				if end > len(body) {
					end = len(body)
				}
				s.feed(body[off:end], now)
				if len(s.carry) > 2*capBytes {
					t.Fatalf("carry = %d bytes after feeding %d/%d, want <= 2*cap=%d (memory must stay bounded)", len(s.carry), end, len(body), 2*capBytes)
				}
			}
			if !s.bufferedTruncated {
				t.Fatalf("bufferedTruncated = false, want true (the over-cap body must have been trimmed, so finish exercises the tail recovery)")
			}
			s.finish(now)
			if u := s.usage(); u.InputTokens != tc.wantIn || u.OutputTokens != tc.wantOut || u.TotalTokens != tc.wantTotal {
				t.Fatalf("usage = %+v, want input=%d output=%d total=%d", u, tc.wantIn, tc.wantOut, tc.wantTotal)
			}
		})
	}
}

// TestKeepTailCopiesIntoABoundedBuffer pins the load-bearing property of keepTail
// that the len-based memory assertions above cannot see (issue #91 review):
// keepTail must COPY the last n bytes into a fresh, bounded buffer, not re-slice
// the (arbitrarily large) input. A re-slice would keep len == n while the whole
// grown backing array stayed reachable and unfreed, defeating the bound feed's
// buffered branch relies on. Asserting on cap() and on independence from the
// source is what distinguishes the two: a re-slice inherits the input's capacity
// and shares its bytes; a copy does neither.
func TestKeepTailCopiesIntoABoundedBuffer(t *testing.T) {
	const n = 256
	// A large input with even larger spare capacity — exactly what feed's append
	// produces just before a trim. A re-slice of this keeps cap in the thousands.
	big := make([]byte, 8*n, 32*n)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	got := keepTail(big, n)
	if len(got) != n {
		t.Fatalf("len(keepTail) = %d, want %d (the last n bytes)", len(got), n)
	}
	if cap(got) > 2*n {
		t.Fatalf("cap(keepTail) = %d, want <= %d — keepTail must copy into a bounded buffer, not re-slice the grown input (which keeps its whole backing array alive)", cap(got), 2*n)
	}
	if string(got) != string(big[len(big)-n:]) {
		t.Fatalf("keepTail returned the wrong bytes; want the last %d of the input", n)
	}
	// Mutating the copy must not touch the source: proves a genuine copy, i.e. the
	// source's backing array is no longer aliased and can be freed.
	got[0] ^= 0xff
	if got[0] == big[len(big)-n] {
		t.Fatalf("keepTail aliases the input's backing array; it must return an independent copy")
	}
}

// TestBufferedTailUsageBoundsHostileInput pins the recovery-cost bounds added in
// the #91 review: a hostile buffered tail must be handled in bounded work rather
// than scanned quadratically (the pre-#91 drop did O(1) work, so an unbounded
// scan here would be a new CPU-amplification vector). Each bound is pinned by an
// input where removing it changes the RESULT, not just the work — the only way a
// cost bound can be observed from a black-box test: the per-object byte bound by
// an object that closes just past it, and the probe cap by a real object placed
// just beyond it.
func TestBufferedTailUsageBoundsHostileInput(t *testing.T) {
	// balancedObject gives up once an object fails to close within
	// maxBufferedUsageObjectBytes: an object whose closing brace sits one byte
	// past the bound is treated as unclosed (the assertion that distinguishes a
	// bounded scan from an unbounded one — without the bound this recovers it).
	justPast := []byte("{" + strings.Repeat("a", maxBufferedUsageObjectBytes) + "}")
	if obj := balancedObject(justPast, 0); obj != nil {
		t.Fatalf("balancedObject recovered an object closing past the %d-byte bound; want nil", maxBufferedUsageObjectBytes)
	}
	// A well-sized object is still recovered whole.
	small := []byte(`{"input_tokens":1}`)
	if obj := balancedObject(small, 0); string(obj) != string(small) {
		t.Fatalf("balancedObject(%q) = %q, want the whole object", small, obj)
	}
	// A tail of nothing but unclosed `"usage":{` decoys must terminate and recover
	// nothing (no hang, no panic, no false recovery).
	hostile := []byte(strings.Repeat(`"usage":{`, 50000))
	if obj := trailingJSONObject(hostile, "usage"); obj != nil {
		t.Fatalf("trailingJSONObject recovered a usage object from an all-unclosed tail; want nil")
	}
	if synth := bufferedTailUsage(hostile); synth != nil {
		t.Fatalf("bufferedTailUsage returned %q for a hostile tail; want nil (recover nothing)", synth)
	}

	// The probe cap itself — observable only through an input where it changes the
	// RESULT, not just the work: a genuine, closeable usage object placed FARTHER
	// back than maxBufferedUsageKeyProbes occurrences from the end, behind that
	// many unclosed decoys nearer the end. trailingJSONObject scans occurrences
	// from the end backward, so with the cap it exhausts its budget on the decoys
	// and gives up (nil) before reaching the real object; WITHOUT the cap it walks
	// all the way back and recovers it (non-nil). Asserting nil therefore fails iff
	// the cap is removed — the all-decoy case above cannot pin that, since it is
	// nil either way. A client that stuffs this many decoys only zeroes its own
	// usage row, exactly as the pre-#91 drop did.
	validBody := `{"usage":{"input_tokens":1,"output_tokens":9}}`
	beyondCap := []byte(validBody + strings.Repeat(`"usage":{`, maxBufferedUsageKeyProbes+8))
	if obj := trailingJSONObject(beyondCap, "usage"); obj != nil {
		t.Fatalf("trailingJSONObject recovered a usage object past the %d-probe cap = %q; want nil (the cap must give up before reaching it)", maxBufferedUsageKeyProbes, obj)
	}
	// The same object WITHIN the cap is recovered, so the cap is a reachability
	// limit, not a blanket failure.
	withinCap := []byte(validBody + strings.Repeat(`"usage":{`, maxBufferedUsageKeyProbes-8))
	if obj := trailingJSONObject(withinCap, "usage"); string(obj) != `{"input_tokens":1,"output_tokens":9}` {
		t.Fatalf("trailingJSONObject within the probe cap = %q, want the real usage object", obj)
	}
}

// TestUsageScannerTotalTokensAcrossSplitFrames proves the deferred-finalize
// design in mergePassthroughUsage/finalizeTotalTokens: Anthropic reports input
// tokens on message_start and output tokens on a LATER, separate message_delta
// frame. A naive per-fragment TotalTokens default (input+output computed on
// each fragment in isolation, then max-merged across fragments) would yield
// max(8+1, 0+40) = 40, not the true 48.
func TestUsageScannerTotalTokensAcrossSplitFrames(t *testing.T) {
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)
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
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)

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

// TestPublishProgressStampsOnlyTheFirstContentFrame is publishProgress's
// ISOLATED test, and the only one in this feature that pins the live row's
// first-token stamp to an EXACT instant.
//
// It exists because the server-level subtest cannot. In
// TestPassthroughAnthropicPlaceholderTokensNeverReachTheLiveRow's real-ordering
// case, message_start and the content frame arrive in ONE chunk and therefore
// share one arrival timestamp, so dropping publishProgress's "nothing before the
// first content frame" guard breaks that case only through a zero-value
// artifact: an unstamped s.firstContentAt is the zero time.Time, and
// liveProgressDTO clamps the resulting negative TTFT to 0. The rule that
// subtest NAMES — the row's TTFT is not stamped off a bookkeeping frame — is
// therefore not pinned by it. Here the two frames arrive a second apart and the
// assertions are on the stamp itself: absent after the bookkeeping frame, and
// exactly the CONTENT frame's own arrival instant after it.
//
// It is also the demonstration behind the pointer-over-callback choice for
// usageScanner.progress: a real &requestProgress{} is allocated and read
// directly, with no HTTP server, no provider, no clock injection and no sleeps.
func TestPublishProgressStampsOnlyTheFirstContentFrame(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	messageStart := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n")
	contentDelta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")

	prog := &requestProgress{}
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, prog)

	s.feed(messageStart, base)
	if got := prog.firstTokenUnixNano.Load(); got != 0 {
		t.Fatalf("first-token stamp = %d, want 0: message_start is a bookkeeping frame, so nothing may be published from it — the row's TTFT must not be stamped before any content exists", got)
	}
	if got := prog.outputTokens.Load(); got != 0 {
		t.Fatalf("live output_tokens = %d, want 0 (message_start's output_tokens is a placeholder, not a count to display)", got)
	}

	contentAt := base.Add(time.Second)
	s.feed(contentDelta, contentAt)
	if got := prog.firstTokenUnixNano.Load(); got != contentAt.UnixNano() {
		t.Fatalf("first-token stamp = %d, want %d — the FIRST CONTENT frame's own arrival instant (s.firstContentAt), neither the zero time nor a bookkeeping frame's", got, contentAt.UnixNano())
	}

	// A repeated bookkeeping frame AFTER content is the adversarial ordering: the
	// guard above no longer applies, so the authoritative-usage-frame gate is the
	// only thing keeping the placeholder 1 off the row, and the stamp's
	// compare-and-swap is the only thing keeping the TTFT where content put it.
	s.feed(messageStart, base.Add(2*time.Second))
	if got := prog.outputTokens.Load(); got != 0 {
		t.Fatalf("live output_tokens = %d, want 0 (a repeated message_start is still not an authoritative usage frame)", got)
	}
	if got := prog.firstTokenUnixNano.Load(); got != contentAt.UnixNano() {
		t.Fatalf("first-token stamp = %d, want %d unchanged (the first content frame fixes it once)", got, contentAt.UnixNano())
	}
}

// TestScanKeepsEachFrameSeparateWithinOnePayload pins the per-frame reset of the
// scratch Usage that scan now owns, which is the only thing keeping BOTH
// surfaces per-frame rather than per-payload.
//
// One payload carrying several SSE frames is the NORMAL case, not a corner:
// nativeCopier.run reads the upstream in 32 KiB chunks, so a fast generation
// puts many frames in one feed. mergePassthroughUsage max-merges into whatever
// destination it is handed, so if `var frame inference.Usage` were declared
// OUTSIDE scan's loop -- the textbook "allocate once, reuse" edit that hoisting
// the merge out of publishProgress invites -- the second frame's merge would
// still see the first frame's 42.5 and yield max(42.5, 38.25) = 42.5. The live
// column AND the recorded rate would both become a per-payload PEAK: exactly
// the defect this change removes, relocated one level down. That mutation is
// why this test exists; without it the whole package passes under it.
//
// The fixture descends because llama.cpp's predicted_per_second is a cumulative
// average that FALLS as the KV cache grows, which is what makes the LATER frame
// the honest answer for both surfaces. Deliberately no terminal frame here: the
// freeze is pinned separately, so this test's claim stays narrowly "frames in
// one payload do not bleed into each other".
func TestScanKeepsEachFrameSeparateWithinOnePayload(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	payload := []byte(
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"hi","timings":{"prompt_per_second":150.0,"predicted_per_second":42.5}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":" there","timings":{"prompt_per_second":140.0,"predicted_per_second":38.25}}` + "\n\n")

	prog := &requestProgress{}
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, prog)
	s.feed(payload, at)

	if got := prog.upstreamTPSMilli.Load(); got != 38250 {
		t.Fatalf("live upstreamTPSMilli = %d, want 38250 — the SECOND frame's own 38.25; 42500 means the scratch Usage leaked across frames inside one payload and the live column became a per-payload max", got)
	}
	if got := s.usage().TokensPerSecond; got != 38.25 {
		t.Fatalf("recorded TokensPerSecond = %v, want 38.25 — the last rate a frame reported; 42.5 means the same leak reached the routing input", got)
	}
	if got := s.usage().PromptPerSecond; got != 140.0 {
		t.Fatalf("recorded PromptPerSecond = %v, want 140.0 — the second frame's own figure, not the 150.0 peak", got)
	}
}

// TestPassthroughResponsesRecordsTheTerminalRateNotThePeak is the defect this
// change fixes, asserted where the defect does its damage: the RECORDED
// end-of-request rate of a native-passthrough `openai_responses` stream.
//
// mergeResponsesUsage max-merges every field it sees, which is correct for every
// COUNT (all monotone over a turn, so the max is the final value) and wrong for
// the two RATES: llama.cpp's prompt_per_second/predicted_per_second are
// cumulative averages over the generation, so predicted_per_second FALLS as the
// KV cache grows while swinging frame to frame, and its running max is a
// mid-stream PEAK. The fixture's three frames report 42.5 -> 38.25 -> 30.0 for
// exactly that reason; before this change the row recorded 42.5.
//
// It was reachable with no gateway change at all, and has been MEASURED end to
// end on the operator's deployment (llama.cpp build b10448-ad1de39e0 serving an
// MTP model, through the server-agent's runtime router). A client that sets
// `timings_per_token` itself has the flag relayed untouched, and llama.cpp then
// attaches `timings` to the partials — 39 of a 48-frame Responses stream's
// frames on the run measured DIRECT to the runtime router. The defect figures
// come from a SECOND, SEPARATE request measured THROUGH this gateway: 41
// timings-bearing frames, whose terminal frame reported
// predicted_per_second = 47.389 while the maximum over them was 52.200 — and
// 52.200240121104564 is what the gateway recorded, the peak, +10.2%. (Two runs,
// so neither frame count describes the other's stream.) And it is not a
// cosmetic misreport: a recorded rate is a ROUTING input (recordUsage ->
// UpdateMappingOpportunisticMetrics), blended into the throughput EWMA the
// scorer and a group's MinTokensPerSecond gate read back, so a peak recorded as
// the final figure steers routing.
//
// The same fixture pins the LIVE column in the same request, which is the point
// of using the progress-observing harness here rather than a plain recording
// provider: the panel's figure is deliberately per-frame (publishProgress reads
// THIS frame's rate, never the accumulator), so at the terminal frame it shows
// 30.0. An implementation that fed the live column from the accumulator would
// show the 42.5 peak and fail below. What the two surfaces must NOT become is
// one shared value — see
// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate, whose
// stream has no terminal frame and where they therefore disagree outright.
//
// `draft_n` rides along on the `timings` objects because that is the shape the
// upstream sends (the measured run carried draft_n = 28 on its terminal frame
// with the flag set, so nothing about `speculation_observed` changes here).
// Nothing about it is asserted at THIS level — a recorded usage.Event carries no
// draft count at all, only the `speculation_observed` capability row does — and
// the field's behaviour under this gate is pinned where it can be:
// TestUsageScannerTerminalRateGateLeavesCountsAndDraftTokensOnTheRunningMax
// below, on the scanner, including the terminal-frame-omits-the-key case.
//
// What the FIXTURE pins is this gateway's HANDLING of partial-frame `timings`.
// Those partials are measured, not assumed — see
// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate
// (passthrough_progress_test.go) for the deployment, the build and the 39-of-48
// frame count, and for the caveat that it is one build on one deployment.
func TestPassthroughResponsesRecordsTheTerminalRateNotThePeak(t *testing.T) {
	var got liveRow
	prov := &progressObservingProxyProvider{
		pieces: []string{
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi","timings":{"prompt_per_second":150.0,"predicted_per_second":42.5,"draft_n":9}}` + "\n\n",
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":" there","timings":{"prompt_per_second":140.0,"predicted_per_second":38.25,"draft_n":12}}` + "\n\n",
			"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_x","usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}},"timings":{"prompt_per_second":120.5,"predicted_per_second":30.0}}` + "\n\n",
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
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].TokensPerSecond != 30.0 {
		t.Fatalf("recorded TokensPerSecond = %v, want 30.0 — response.completed's own predicted_per_second, the generation's FINAL cumulative average, not the 42.5 peak an earlier frame reported", events[0].TokensPerSecond)
	}
	if events[0].PromptPerSecond != 120.5 {
		t.Fatalf("recorded PromptPerSecond = %v, want 120.5 — the terminal frame's own prompt_per_second, not the 150.0 max across the stream", events[0].PromptPerSecond)
	}
	// The counts are untouched by the rate gate: they arrive on the terminal
	// frame and are the upstream's own.
	if events[0].OutputTokens != 40 || events[0].InputTokens != 8 || events[0].TotalTokens != 48 {
		t.Fatalf("recorded counts = in %d / out %d / total %d, want 8/40/48 (the rate gate must not disturb them)", events[0].InputTokens, events[0].OutputTokens, events[0].TotalTokens)
	}
	if got.tps != 30.0 {
		t.Fatalf("live tokens_per_second = %v, want 30.0 — the terminal frame's OWN rate, read per-frame; 42.5 would mean the live column started reading the max-merged accumulator", got.tps)
	}
	if got.source != "upstream" {
		t.Fatalf("live tokens_per_second_source = %q, want %q (llama.cpp's own measurement off the terminal frame)", got.source, "upstream")
	}
	if got.outputTokens != 40 {
		t.Fatalf("live output_tokens = %d, want 40 (response.completed's own count, with no delta-derived contribution)", got.outputTokens)
	}
}

// TestUsageScannerResponsesTerminalRateSurvivesLaterFramesAndTheLoopsFrequency
// pins the STRUCTURAL half of the fix, which the end-to-end test above cannot
// see: scan's per-payload loop is CONDITIONAL
// (`!haveFirstContent || !haveTerminalUsage || progress != nil`) while the
// accumulator merge below it is not, so with no live counter attached the loop
// stops the moment both flags are set and the merge goes on for every remaining
// chunk.
//
// Two consequences are asserted, in both loop regimes:
//
//   - A frame arriving AFTER the terminal one cannot raise the recorded rate.
//     The trailing frame here reports a 50.0 that still reaches the accumulator
//     through the unconditional merge, so an implementation that read the
//     accumulator's rate records 50.0 (or the 42.5 peak) instead of 30.0.
//
//   - The recorded rate is the SAME whether or not a live progress counter is
//     attached. The capture is FROZEN by the first authoritative frame, which
//     scan's own condition guarantees is inside the loop (`!haveTerminalUsage`
//     cannot be false while the flag is unset); nothing later has that
//     guarantee. This is what the trailing frame being a SECOND
//     `response.completed` pins: a capture left open past the first
//     authoritative frame reads that trailing frame only in the
//     attached-counter regime — where the loop is still running — so the two
//     subtests below would disagree, and the recorded rate would silently
//     depend on whether the row happened to be displayed. (Before that frame
//     the capture IS open, deliberately, and the same condition keeps the loop
//     running for every frame there — see
//     TestUsageScannerResponsesCutOffStreamRecordsTheLastRateNotThePeak.)
//
// The trailing frame's ordering is deliberately adversarial: no llama.cpp
// stream sends two `response.completed` frames, and the point is that both
// rules hold STRUCTURALLY rather than by accident of arrival order.
//
// The attached-counter case also shows the two surfaces are genuinely separate:
// the live counter ends at the trailing frame's 50.0 (per-frame, by design)
// while the recorded row keeps the first terminal frame's 30.0.
func TestUsageScannerResponsesTerminalRateSurvivesLaterFramesAndTheLoopsFrequency(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	partialPeak := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi","timings":{"prompt_per_second":150.0,"predicted_per_second":42.5}}` + "\n\n")
	terminal := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}},"timings":{"prompt_per_second":120.5,"predicted_per_second":30.0}}` + "\n\n")
	trailing := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}},"timings":{"prompt_per_second":900.0,"predicted_per_second":50.0}}` + "\n\n")

	for _, tc := range []struct {
		name string
		prog *requestProgress
	}{
		{"no live counter: scan's loop stops after the terminal frame", nil},
		{"a live counter attached: scan's loop keeps running for every frame", &requestProgress{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, tc.prog)
			s.feed(partialPeak, base)
			s.feed(terminal, base.Add(time.Second))
			s.feed(trailing, base.Add(2*time.Second))

			u := s.usage()
			if u.TokensPerSecond != 30.0 {
				t.Fatalf("TokensPerSecond = %v, want 30.0 (response.completed's own rate; 42.5 is the earlier peak and 50.0 arrived after the authoritative frame)", u.TokensPerSecond)
			}
			if u.PromptPerSecond != 120.5 {
				t.Fatalf("PromptPerSecond = %v, want 120.5 (the terminal frame's own; 150.0/900.0 are the other frames')", u.PromptPerSecond)
			}
			if u.OutputTokens != 40 || u.InputTokens != 8 || u.TotalTokens != 48 {
				t.Fatalf("counts = in %d / out %d / total %d, want 8/40/48", u.InputTokens, u.OutputTokens, u.TotalTokens)
			}
			if tc.prog != nil {
				if got := tc.prog.upstreamTPSMilli.Load(); got != 50000 {
					t.Fatalf("live upstreamTPSMilli = %d, want 50000 — the LAST frame's own rate: the live column is per-frame by design, and it must not have been pulled onto the recorded row's terminal-frame value", got)
				}
			}
		})
	}
}

// TestUsageScannerTerminalRateGateLeavesCountsAndDraftTokensOnTheRunningMax is
// the boundary of the gate, pinned so a later reader cannot "simplify" it into
// covering the fields where the running MAX is the correct merge.
//
// Both subtests are cases where taking a COUNT from the authoritative frame
// would lose data, which is precisely why only the two rates are taken:
//
//   - Responses: `timings.draft_n` sits on the same object as the rates but is
//     monotone, and llama.cpp emits the key only when it drafted — so a terminal
//     frame that omits it must leave the partials' max standing, not zero it.
//
//   - Anthropic: isTerminalUsageFrame accepts EVERY `message_delta`, not just
//     the last, and the capture reads the FIRST authoritative frame. A count
//     taken from that frame would therefore freeze at an intermediate
//     `output_tokens` — 20 here instead of 40 — and the derived rate would
//     halve with it. (The input side is the same argument one frame earlier:
//     message_start carries input_tokens and message_delta does not, pinned by
//     TestUsageScannerTotalTokensAcrossSplitFrames.)
func TestUsageScannerTerminalRateGateLeavesCountsAndDraftTokensOnTheRunningMax(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("responses: draft_n keeps its max when the terminal frame omits it", func(t *testing.T) {
		s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
		s.feed([]byte("event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"hi","timings":{"predicted_per_second":42.5,"draft_n":9}}`+"\n\n"), base)
		s.feed([]byte("event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":" there","timings":{"predicted_per_second":38.25,"draft_n":12}}`+"\n\n"), base.Add(time.Second))
		s.feed([]byte("event: response.completed\n"+
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}},"timings":{"predicted_per_second":30.0}}`+"\n\n"), base.Add(2*time.Second))

		u := s.usage()
		if u.DraftTokens != 12 {
			t.Fatalf("DraftTokens = %d, want 12 (monotone, so the running max IS the final value; the terminal frame carries no draft_n to take it from)", u.DraftTokens)
		}
		if u.TokensPerSecond != 30.0 {
			t.Fatalf("TokensPerSecond = %v, want 30.0 (the rate, unlike draft_n, comes from the terminal frame)", u.TokensPerSecond)
		}
	})

	t.Run("anthropic: a second message_delta still raises the count and the derived rate", func(t *testing.T) {
		s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)
		s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"), base)
		s.feed([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"), base)
		s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\n"), base.Add(time.Second))
		s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"), base.Add(2*time.Second))

		u := s.usage()
		if u.InputTokens != 8 || u.OutputTokens != 40 || u.TotalTokens != 48 {
			t.Fatalf("counts = in %d / out %d / total %d, want 8/40/48 (the FIRST message_delta is authoritative too, so a count taken from it would freeze at 20)", u.InputTokens, u.OutputTokens, u.TotalTokens)
		}
		if u.TokensPerSecond != 20.0 {
			t.Fatalf("TokensPerSecond = %v, want 20.0 (40 tokens over the 2s generation window; a count frozen at 20 would derive 10.0)", u.TokensPerSecond)
		}
	})
}

// TestUsageScannerResponsesCutOffStreamRecordsTheLastRateNotThePeak is the case
// a terminal-frame-only capture cannot reach: a Responses stream with partial
// `timings` and NO `response.completed`. There is no final frame to read, but
// there IS a last real measurement, and recording the accumulator's mid-stream
// PEAK instead of it is the same defect this change removes for the complete
// stream.
//
// Two truncated shapes exist and they land on DIFFERENT surfaces — the
// distinction matters, because getting it backwards makes this test look
// display-only and therefore droppable:
//
//   - Ends CLEANLY at 200 with no `response.completed` — a `response.failed` or
//     `response.incomplete` terminal event (this repo's own translate path
//     emits `response.failed`, inference_complete.go), or an upstream that
//     simply stops. nativeTerminalStatus records that status "success", and
//     recordUsage's EWMA feed is gated on success, so the rate IS a ROUTING
//     input (UpdateMappingOpportunisticMetrics) exactly as a complete stream's
//     is. That is the shape this fixture models: three partials and then
//     nothing.
//   - A client disconnect, a copy error or an idle timeout is recorded status
//     "error" by that same function, so its rate reaches only the Activity row.
//     A peak there is still a misreport of what the upstream measured, just not
//     a routing one.
//
// Either way a truncated generation is a reason to record its last measured
// value, not a licence to record its best moment.
//
// Nothing about the per-payload loop's frequency is traded away to get it, which
// is the structural point: scan's loop condition includes `!haveTerminalUsage`,
// so for a response that never produces an authoritative frame the loop is
// ALREADY running for every payload — precisely in this case. The regime the
// sibling test above contrasts (loop stops early) cannot arise here at all, so
// this test needs no table over it.
//
// The trailing frame reports a `timings` object whose rates are 0.0, which is a
// measured shape rather than an invented one: on the operator's deployment the
// per-frame predicted_per_second series opens at exactly 0.0 (0.0, 16.62, 33.24,
// 49.86, 34.06, …). "This frame has nothing to report yet" must therefore leave
// the last real figure standing — takeLastNonZeroF — instead of erasing it.
func TestUsageScannerResponsesCutOffStreamRecordsTheLastRateNotThePeak(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
	s.feed([]byte("event: response.output_text.delta\n"+
		`data: {"type":"response.output_text.delta","delta":"hi","timings":{"prompt_per_second":150.0,"predicted_per_second":50.0}}`+"\n\n"), base)
	s.feed([]byte("event: response.output_text.delta\n"+
		`data: {"type":"response.output_text.delta","delta":" there","timings":{"prompt_per_second":120.5,"predicted_per_second":30.0}}`+"\n\n"), base.Add(time.Second))
	s.feed([]byte("event: response.output_text.delta\n"+
		`data: {"type":"response.output_text.delta","delta":"!","timings":{"prompt_per_second":0.0,"predicted_per_second":0.0}}`+"\n\n"), base.Add(2*time.Second))

	u := s.usage()
	if u.TokensPerSecond != 30.0 {
		t.Fatalf("TokensPerSecond = %v, want 30.0 — the LAST rate this cut-off stream actually reported; 50.0 is the accumulator's mid-stream peak and 0 is the trailing frame reporting nothing yet", u.TokensPerSecond)
	}
	if u.PromptPerSecond != 120.5 {
		t.Fatalf("PromptPerSecond = %v, want 120.5 (same rule; 150.0 is the peak)", u.PromptPerSecond)
	}
}

// TestUsageScannerRateSubstitutionNeverZeroesTheAccumulatorsFigure pins the
// boundary that keeps the substitution's flavor-agnostic form a no-op for
// `anthropic_messages` by CONSTRUCTION rather than by an assumption a later
// reader has to re-derive: a rate the capture never saw is left alone, never
// overwritten with a 0.
//
// The shape this defends is not the fixture. It is a flavor whose merge one day
// reads a rate from frames the capture is frozen before — and Anthropic is
// already one bad commit away from being it, because isTerminalUsageFrame
// accepts EVERY `message_delta`, so the capture freezes at the first. Today
// mergeAnthropicUsage writes no rate field at all (its anthropicUsage struct has
// none, llama.cpp attaching no `timings` to any Anthropic frame), so both sides
// are 0 there and the branch is unreachable through that flavor; a Responses
// stream whose authoritative frame carries no `timings` while a LATER frame does
// is the only way to reach it with today's merges. The ordering is deliberately
// adversarial, exactly as the trailing frame in
// TestUsageScannerResponsesTerminalRateSurvivesLaterFramesAndTheLoopsFrequency
// is: the rule has to hold structurally, not by luck of arrival order.
func TestUsageScannerRateSubstitutionNeverZeroesTheAccumulatorsFigure(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
	s.feed([]byte("event: response.completed\n"+
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":40,"total_tokens":48}}}`+"\n\n"), base)
	s.feed([]byte("event: response.output_text.delta\n"+
		`data: {"type":"response.output_text.delta","delta":"late","timings":{"prompt_per_second":900.0,"predicted_per_second":50.0}}`+"\n\n"), base.Add(time.Second))

	u := s.usage()
	if u.TokensPerSecond != 50.0 {
		t.Fatalf("TokensPerSecond = %v, want 50.0 — the authoritative frame reported NO rate, so the accumulator's figure must stand; substituting the captured 0 would overwrite a real measurement with nothing", u.TokensPerSecond)
	}
	if u.PromptPerSecond != 900.0 {
		t.Fatalf("PromptPerSecond = %v, want 900.0 (same rule)", u.PromptPerSecond)
	}
	if u.OutputTokens != 40 || u.InputTokens != 8 || u.TotalTokens != 48 {
		t.Fatalf("counts = in %d / out %d / total %d, want 8/40/48 (the rate rules must not disturb them)", u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
}

// An images response gets NO usage scanner. Two independent reasons, and the
// test pins the decision rather than either symptom: mergePassthroughUsage
// (native_passthrough.go) switches on openai_responses/anthropic_messages with
// no default, so a scan of an images body can produce nothing at all; and
// discovering that is not free -- an images body is one buffered JSON value, so
// feed takes its buffered branch and retains up to 2×capBytes (2 MiB by
// default) of base64 per in-flight request on the endpoint with the largest
// bodies in the system. The count that IS wanted from those bytes comes from
// imagesDataCounter, whose carry is small and bounded.
func TestNewUsageScannerSkipsImages(t *testing.T) {
	if s := newUsageScanner(apiFlavorImages, defaultCaptureMaxBytes, nil); s != nil {
		t.Fatalf("newUsageScanner(%q) = %+v, want nil -- the scan is a guaranteed no-op for images and retains the body to prove it", apiFlavorImages, s)
	}
	// The contrast: the two flavors the merge does have cases for still get one.
	for _, flavor := range []string{"openai_responses", "anthropic_messages"} {
		if newUsageScanner(flavor, defaultCaptureMaxBytes, nil) == nil {
			t.Fatalf("newUsageScanner(%q) = nil, want a scanner", flavor)
		}
	}
	// nil is what proxyNative then hands nativeCopier, so the whole sequence the
	// copier drives must be a no-op on a nil receiver rather than a panic.
	var s *usageScanner
	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s.feed([]byte(`{"data":[{"b64_json":"QUJD"}]}`), at)
	s.finish(at)
	if got := s.usage(); got != (inference.Usage{}) {
		t.Fatalf("nil scanner usage() = %+v, want the zero Usage", got)
	}
}
