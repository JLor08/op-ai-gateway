// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"time"
)

// usageScanner incrementally extracts token usage — and, for the Anthropic
// fallback rate, the generation-window start — from a native-passthrough
// response AS ITS BYTES PASS THROUGH THE COPIER, independently of the capture
// tee's cap.
//
// The capture buffer (nativeCopier.respBuf, bounded by capBytes) exists for a
// different purpose entirely: retaining a bounded sample of the response body
// for the Activity capture view, with its own budget for that job. Before this
// type existed, parsePassthroughUsage ran exactly once, AFTER the copy
// finished, over that SAME capped buffer — so a response bigger than the cap
// (the common case for a long agent turn) silently lost whatever usage frame
// fell past the cutoff, including the terminal one. Even the FINAL token count
// was dropped, independently of anything to do with throughput. Token
// accounting must not ride on a budget that belongs to a different feature, so
// this scanner is fed every chunk directly — native_passthrough.go's
// writeChunk, BEFORE the capture-cap check — and keeps its own small, bounded
// amount of state instead.
type usageScanner struct {
	apiFlavor string
	// capBytes bounds the carry (see feed) — deliberately the SAME budget the
	// capture tee uses, since both exist to cap gateway memory against a
	// pathological or enormous upstream response, not because the two features
	// are otherwise related.
	capBytes int

	// carry holds bytes seen since the last complete '\n'-terminated line. An SSE
	// frame is usually exactly one line; a buffered non-stream JSON body is
	// usually ONE unterminated "line" for its entire length, recovered by finish
	// instead of feed.
	carry []byte

	acc inference.Usage

	haveFirstContent bool
	firstContentAt   time.Time
	// lastAt is the timestamp of the most recent feed/finish call: the best
	// available estimate of "generation completed" for the Anthropic fallback
	// rate's generation-window end (see usage below).
	lastAt time.Time
}

// newUsageScanner returns a scanner for one native-passthrough response.
// capBytes should be the same budget the capture tee uses (see usageScanner's
// doc comment for why the two share a budget without being related features).
func newUsageScanner(apiFlavor string, capBytes int) *usageScanner {
	return &usageScanner{apiFlavor: apiFlavor, capBytes: capBytes}
}

// feed appends chunk (one read from the upstream body) to the carry buffer,
// hands every COMPLETE '\n'-terminated line in it to the usage/content-frame
// scan, and keeps only the trailing, still-incomplete line as the new carry.
//
// The carry is bounded by capBytes: if it grows past that without ever seeing a
// newline — one pathologically long line, or an upstream that never terminates
// a frame — it is dropped rather than grown further. This is a deliberate,
// advisory trade: an ordinary SSE upstream never gets near this (every frame
// this codebase, and a real llama.cpp/OpenAI/Anthropic-compatible server,
// emits ends in "\n\n"), but nothing here may let a misbehaving or hostile
// upstream make the gateway allocate without limit just because it happens to
// be scanning for usage. Losing an over-long line's numbers is an acceptable
// trade; unbounded memory growth on an advisory accounting path is not.
//
// A line is scanned exactly once here (it leaves the carry as soon as it's
// complete), but correctness never actually depends on that: mergePassthroughUsage's
// per-field `take` is a running max, so re-merging the same or overlapping
// bytes could only ever leave recorded values unchanged or move them up, never
// corrupt them. See mergePassthroughUsage's doc comment (native_passthrough.go).
func (s *usageScanner) feed(chunk []byte, at time.Time) {
	if s == nil {
		return
	}
	s.lastAt = at
	s.carry = append(s.carry, chunk...)
	i := bytes.LastIndexByte(s.carry, '\n')
	if i < 0 {
		if len(s.carry) > s.capBytes {
			s.carry = nil
		}
		return
	}
	complete := s.carry[:i+1]
	s.scan(complete, at)
	rest := s.carry[i+1:]
	if len(rest) > s.capBytes {
		rest = nil
	}
	// Copy the remainder into a fresh slice so the retained carry doesn't keep
	// `complete`'s (potentially large) backing array alive.
	next := make([]byte, len(rest))
	copy(next, rest)
	s.carry = next
}

// finish scans whatever is left in the carry as if it were itself a complete
// line, and must be called exactly once after the last feed() — nativeCopier.run
// does this via a defer. Without it, a stream whose final frame is never itself
// newline-terminated would go unscanned, since feed only acts on complete
// lines — and for a NON-streaming (buffered) response that is the NORMAL case:
// the entire JSON body is typically emitted as one line with no embedded
// newline at all. finish is what lets one scanner serve both the streaming and
// the buffered native-passthrough shape.
func (s *usageScanner) finish(at time.Time) {
	if s == nil {
		return
	}
	s.lastAt = at
	if len(s.carry) == 0 {
		return
	}
	s.scan(s.carry, at)
	s.carry = nil
}

// scan stamps the first-content-frame timestamp (if not already seen) and
// merges payload's usage/timings fields into the running total.
func (s *usageScanner) scan(payload []byte, at time.Time) {
	if !s.haveFirstContent {
		for _, p := range jsonPayloads(payload) {
			if isContentFrame(s.apiFlavor, p) {
				s.haveFirstContent = true
				s.firstContentAt = at
				break
			}
		}
	}
	mergePassthroughUsage(&s.acc, s.apiFlavor, payload)
}

// usage returns the accumulated usage for the recording path.
//
// For the Anthropic flavor only, when the upstream reported an output-token
// count but no rate of its own (Anthropic carries no `timings` object on any
// frame at all — unlike the Responses shape; see parsePassthroughUsage), a rate
// is derived from that EXACT count over the generation window: first content
// frame -> last observed activity (lastAt, stamped by every feed/finish call,
// so it tracks the true end of the stream). Never over the whole request — that
// would fold in queueing and prompt processing and stop being the same
// quantity every other surface in this feature reports. The arithmetic mirrors
// streamOnce in benchmark_runner.go:113-118 (output tokens / generation
// seconds).
//
// The Responses shape deliberately does NOT get this fallback: llama.cpp
// attaches no timings to the Anthropic shape at all, which is the only reason
// Anthropic needs a derived rate here. An absent Responses `timings` object is
// left at 0 — out of scope for this change.
func (s *usageScanner) usage() inference.Usage {
	if s == nil {
		return inference.Usage{}
	}
	u := s.acc
	finalizeTotalTokens(&u)
	if s.apiFlavor == "anthropic_messages" && u.TokensPerSecond == 0 && u.OutputTokens > 0 && s.haveFirstContent {
		if genSecs := s.lastAt.Sub(s.firstContentAt).Seconds(); genSecs > 0 {
			u.TokensPerSecond = float64(u.OutputTokens) / genSecs
		}
	}
	return u
}

// isContentFrame reports whether payload — one JSON usage/event object as
// returned by jsonPayloads (a single SSE `data:` line's payload, or a whole
// buffered body) — is a frame carrying upstream-GENERATED content, as opposed
// to a structural/bookkeeping one. This is what stamps the first-content
// timestamp in scan above, i.e. the start of the generation window usage()'s
// Anthropic fallback is computed over.
//
// Anthropic: a `content_block_delta` is the ONLY event type that carries
// generated bytes (text/thinking deltas, or tool-call `partial_json`
// fragments); `message_start` — despite carrying the message's initial usage
// snapshot — carries no generated content of its own and is explicitly NOT
// content.
//
// Responses: judged the same way. The three `*.delta` event types that carry an
// actual generated fragment are content: `response.output_text.delta`
// (assistant text), `response.reasoning_text.delta` (reasoning/thinking text),
// and `response.function_call_arguments.delta` (tool-call argument bytes).
// Every structural/bookkeeping event around them — response.created,
// response.in_progress, response.output_item.added/done,
// response.content_part.added/done, and the terminal response.completed /
// response.failed — carries no generated bytes of its own, mirroring
// message_start's exclusion above. (This flavor's definition is written down
// here for completeness, and because it shares content-frame stamping with
// Anthropic in scan above, but the Responses fallback itself stays out of
// scope: see usage()'s doc comment.)
func isContentFrame(apiFlavor string, payload []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return false
	}
	switch apiFlavor {
	case "anthropic_messages":
		return probe.Type == "content_block_delta"
	case "openai_responses":
		switch probe.Type {
		case "response.output_text.delta", "response.reasoning_text.delta", "response.function_call_arguments.delta":
			return true
		}
	}
	return false
}
