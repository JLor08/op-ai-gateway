// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// strataResponsesStream returns the frames of a streamed `strata serve`
// /v1/responses answer, which carries no `timings` on any event: created,
// in_progress, two reasoning deltas, a text delta, and the terminal
// response.completed with input 58 (cached as given), output 35.
func strataResponsesStream(cached int) (created, inProgress, reasoning1, reasoning2, text, completed []byte) {
	created = []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\",\"status\":\"in_progress\"}}\n\n")
	inProgress = []byte("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_s\",\"status\":\"in_progress\"}}\n\n")
	reasoning1 = []byte("event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"The\"}\n\n")
	reasoning2 = []byte("event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"delta\":\" user\"}\n\n")
	text = []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
	completed = []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"status\":\"completed\",\"usage\":{\"input_tokens\":58,\"input_tokens_details\":{\"cached_tokens\":" +
		strconv.Itoa(cached) + "},\"output_tokens\":35,\"output_tokens_details\":{\"reasoning_tokens\":32},\"total_tokens\":93}}}\n\n")
	return created, inProgress, reasoning1, reasoning2, text, completed
}

// feedStrataResponses feeds a Strata Responses stream on a fixed timeline: the
// request goes upstream at base, response.created arrives at +0.5s, the first
// reasoning delta (first content) at +2s, the rest up to response.completed at
// +7s. TTFT is therefore 2s and the generation window 5s.
func feedStrataResponses(s *usageScanner, base time.Time, cached int) {
	created, inProgress, r1, r2, text, completed := strataResponsesStream(cached)
	s.setUpstreamSentAt(base)
	s.feed(created, base.Add(500*time.Millisecond))
	s.feed(inProgress, base.Add(500*time.Millisecond))
	s.feed(r1, base.Add(2*time.Second))
	s.feed(r2, base.Add(3*time.Second))
	s.feed(text, base.Add(5*time.Second))
	s.feed(completed, base.Add(7*time.Second))
	s.finish(base.Add(7 * time.Second))
}

// A self-hosted Responses stream without `timings` (Strata's `strata serve`)
// gets the gateway generation rate -- output 35 over the 5s window from the
// first reasoning delta -- and a gateway-measured prompt rate: the 58 uncached
// input tokens over the 2s from the upstream send to the first content (#426).
func TestPassthroughSelfHostedResponsesDerivesGatewayRates(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
	feedStrataResponses(s, base, 0)

	u := s.usage()
	if want := 35.0 / 5; u.TokensPerSecond != want {
		t.Fatalf("TokensPerSecond = %v, want %v (35 output tokens over the 5s generation window)", u.TokensPerSecond, want)
	}
	if want := 58.0 / 2; u.PromptPerSecond != want {
		t.Fatalf("PromptPerSecond = %v, want %v (58 prompt tokens over the 2s time to first content)", u.PromptPerSecond, want)
	}
}

// Cache-read tokens were not processed by the upstream, so the prompt rate
// counts only the 58 - 50 = 8 uncached input tokens.
func TestPassthroughGatewayPromptRateExcludesCacheReads(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
	feedStrataResponses(s, base, 50)

	if want, got := 8.0/2, s.usage().PromptPerSecond; got != want {
		t.Fatalf("PromptPerSecond = %v, want %v (8 uncached prompt tokens over 2s)", got, want)
	}
}

// Strata's streamed /v1/messages: input_tokens 1, cache_creation 25,
// cache_read 20855, output 69. The prompt rate counts the 26 tokens the
// upstream processed (fresh + cache creation) over the 2s TTFT; the generation
// rate is the existing window rate, 69 over 3s.
func TestPassthroughAnthropicGatewayPromptRateCountsFreshAndCacheCreation(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	usageJSON := `{"input_tokens":1,"cache_creation_input_tokens":25,"cache_read_input_tokens":20855,"output_tokens":69}`
	s := newUsageScanner("anthropic_messages", defaultCaptureMaxBytes, nil)
	s.setUpstreamSentAt(base)
	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_creation_input_tokens\":25,\"cache_read_input_tokens\":20855,\"output_tokens\":1}}}\n\n"), base.Add(time.Second))
	s.feed([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"), base.Add(2*time.Second))
	s.feed([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":"+usageJSON+"}\n\n"), base.Add(5*time.Second))
	s.finish(base.Add(5 * time.Second))

	u := s.usage()
	if want := 26.0 / 2; u.PromptPerSecond != want {
		t.Fatalf("PromptPerSecond = %v, want %v (26 processed prompt tokens over the 2s TTFT; cache reads excluded)", u.PromptPerSecond, want)
	}
	if want := 69.0 / 3; u.TokensPerSecond != want {
		t.Fatalf("TokensPerSecond = %v, want %v (69 output tokens over the 3s generation window)", u.TokensPerSecond, want)
	}
}

// Upstream `timings` (llama.cpp-style, on response.completed) win for both
// rates: the gateway derives neither when the server said.
func TestPassthroughResponsesUpstreamTimingsWinOverGatewayRates(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	created, _, r1, _, _, _ := strataResponsesStream(0)
	completed := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"usage\":{\"input_tokens\":58,\"output_tokens\":35,\"total_tokens\":93}},\"timings\":{\"prompt_per_second\":120.5,\"predicted_per_second\":38.25}}\n\n")
	s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
	s.setUpstreamSentAt(base)
	s.feed(created, base.Add(500*time.Millisecond))
	s.feed(r1, base.Add(2*time.Second))
	s.feed(completed, base.Add(7*time.Second))

	u := s.usage()
	if u.PromptPerSecond != 120.5 || u.TokensPerSecond != 38.25 {
		t.Fatalf("prompt/generation = %v/%v, want 120.5/38.25 (the upstream's own timings)", u.PromptPerSecond, u.TokensPerSecond)
	}
}

// The prompt-rate guards: no send timestamp, no content frame, a TTFT under
// minGatewayRateWindow, or nothing processed (all input was a cache read)
// each leave the prompt rate 0.
func TestPassthroughGatewayPromptRateGuards(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	created, _, r1, _, _, completed := strataResponsesStream(0)

	t.Run("no send timestamp", func(t *testing.T) {
		s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
		s.feed(created, base)
		s.feed(r1, base.Add(2*time.Second))
		s.feed(completed, base.Add(7*time.Second))
		if got := s.usage().PromptPerSecond; got != 0 {
			t.Fatalf("PromptPerSecond = %v, want 0 (no upstream send time)", got)
		}
	})
	t.Run("no content frame", func(t *testing.T) {
		s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
		s.setUpstreamSentAt(base)
		s.feed(created, base.Add(time.Second))
		s.feed(completed, base.Add(7*time.Second))
		if got := s.usage().PromptPerSecond; got != 0 {
			t.Fatalf("PromptPerSecond = %v, want 0 (no first content)", got)
		}
	})
	t.Run("TTFT under the floor", func(t *testing.T) {
		s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
		s.setUpstreamSentAt(base)
		s.feed(created, base)
		s.feed(r1, base.Add(49*time.Millisecond))
		s.feed(completed, base.Add(7*time.Second))
		if got := s.usage().PromptPerSecond; got != 0 {
			t.Fatalf("PromptPerSecond = %v, want 0 (49ms TTFT is below the 50ms floor)", got)
		}
	})
	t.Run("all input from the cache", func(t *testing.T) {
		_, _, _, _, _, allCached := strataResponsesStream(58)
		s := newUsageScanner("openai_responses", defaultCaptureMaxBytes, nil)
		s.setUpstreamSentAt(base)
		s.feed(created, base)
		s.feed(r1, base.Add(2*time.Second))
		s.feed(allCached, base.Add(7*time.Second))
		if got := s.usage().PromptPerSecond; got != 0 {
			t.Fatalf("PromptPerSecond = %v, want 0 (no uncached prompt tokens)", got)
		}
	})
}

// A buffered body has no first-content frame distinct from its end, so neither
// rate is derived -- even with a send timestamp a second before the body.
func TestPassthroughBufferedBodyDerivesNoPromptRate(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for _, flavor := range []string{"openai_responses", "anthropic_messages"} {
		t.Run(flavor, func(t *testing.T) {
			body := `{"id":"r","object":"response","usage":{"input_tokens":58,"output_tokens":35,"total_tokens":93}}`
			if flavor == "anthropic_messages" {
				body = `{"id":"m","type":"message","usage":{"input_tokens":1,"cache_creation_input_tokens":25,"output_tokens":35}}`
			}
			s := newUsageScanner(flavor, defaultCaptureMaxBytes, nil)
			s.setUpstreamSentAt(base)
			s.feed([]byte(body), base.Add(time.Second))
			s.finish(base.Add(time.Second))
			u := s.usage()
			if u.PromptPerSecond != 0 || u.TokensPerSecond != 0 {
				t.Fatalf("prompt/generation = %v/%v, want 0/0 (a buffered body has no window)", u.PromptPerSecond, u.TokensPerSecond)
			}
		})
	}
}

// Chat Completions derives nothing: its upstreams report `timings` themselves.
func TestPassthroughChatCompletionsDerivesNoGatewayRates(t *testing.T) {
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s := newUsageScanner("openai_chat_completions", defaultCaptureMaxBytes, nil)
	s.setUpstreamSentAt(base)
	s.feed([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n"), base.Add(2*time.Second))
	s.feed([]byte(`data: {"choices":[],"usage":{"prompt_tokens":58,"completion_tokens":35,"total_tokens":93}}`+"\n\n"), base.Add(7*time.Second))
	u := s.usage()
	if u.PromptPerSecond != 0 || u.TokensPerSecond != 0 {
		t.Fatalf("prompt/generation = %v/%v, want 0/0", u.PromptPerSecond, u.TokensPerSecond)
	}
}

// End to end against a real HTTP upstream: a streamed Strata /v1/responses with
// no `timings`, paced so the first content arrives after the upstream send by
// more than minGatewayRateWindow and the generation window is as long, records
// both a prompt and a generation rate on the usage row. The send timestamp is
// taken by proxyNative, so this is what proves it reaches the scanner.
func TestStrataStreamedResponsesRecordsGatewayRates(t *testing.T) {
	created, inProgress, r1, r2, text, completed := strataResponsesStream(0)
	const gap = 80 * time.Millisecond
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(b []byte) {
			_, _ = w.Write(b)
			if fl != nil {
				fl.Flush()
			}
		}
		write(append(created, inProgress...))
		time.Sleep(gap)
		write(r1)
		write(r2)
		time.Sleep(gap)
		write(append(text, completed...))
	}))
	t.Cleanup(up.Close)
	srv := newStrataTestServer(t, up.URL)
	postStrata(t, srv, "/v1/responses", `{"model":"strata-model","stream":true,"input":"hi"}`)
	ev := lastUsageEvent(t, srv)
	if ev.TokensPerSecond <= 0 || ev.PromptPerSecond <= 0 {
		t.Fatalf("recorded prompt/generation tokens per second = %v/%v, want both > 0 (gateway-derived)", ev.PromptPerSecond, ev.TokensPerSecond)
	}
	if maxPrompt := 58 / gap.Seconds(); ev.PromptPerSecond > maxPrompt {
		t.Fatalf("recorded prompt rate %v exceeds %v: the TTFT window must be at least the %v pacing gap", ev.PromptPerSecond, maxPrompt, gap)
	}
	if ev.OutputTokens != 35 {
		t.Fatalf("recorded output = %d, want 35", ev.OutputTokens)
	}
}
