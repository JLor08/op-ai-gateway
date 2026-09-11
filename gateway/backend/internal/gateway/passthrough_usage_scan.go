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
	// haveTerminalUsage records whether an AUTHORITATIVE TERMINAL usage frame was
	// seen (isTerminalUsageFrame). usage()'s derived rate is gated on it, because
	// the merged output-token count alone cannot say whether it came from a real
	// total or from a placeholder snapshot.
	haveTerminalUsage bool
	// finalPromptPerSecond / finalTokensPerSecond are the LATEST rates any frame
	// reported for ITSELF, frozen at the first authoritative terminal frame (see
	// takeFinalRates) and held OUTSIDE the max-merged accumulator, so usage()
	// can hand the recording path the generation's FINAL rate rather than its
	// PEAK.
	//
	// The accumulator is right to max-merge everything else and wrong to
	// max-merge these two. Every COUNT it holds — input/output/total/cached
	// tokens, and `timings.draft_n` — grows monotonically over a turn, so its
	// max IS its final value, which is the whole reason a fragment-by-fragment
	// scan can work at all. A RATE does not: llama.cpp's
	// prompt_per_second/predicted_per_second are cumulative averages over the
	// generation, so predicted_per_second falls as the KV cache grows
	// (publishProgress's doc comment states the same fact for the live column)
	// while swinging frame to frame, and the max of such a series is a
	// mid-stream peak.
	//
	// MEASURED, not inferred — on the operator's own deployment (llama.cpp build
	// b10448-ad1de39e0 serving an MTP model, reached through the server-agent's
	// runtime router). The figures come from SEPARATE flagged requests, and
	// which run each one belongs to matters:
	//
	//   - Measured DIRECT to the runtime router, one 48-frame Responses stream:
	//     39 of those frames carried a top-level `timings` object — on
	//     response.reasoning_text.delta and response.output_text.delta alike.
	//   - Measured THROUGH this gateway, a second, separate request: 41
	//     timings-bearing frames, whose terminal frame reported
	//     predicted_per_second = 47.389 while the maximum over them was 52.200 —
	//     and 52.200240121104564 is what this gateway recorded, the peak,
	//     +10.2%, straight into the routing EWMA below.
	//
	// The series is a sawtooth rather than a monotone decay (0.0, 16.62, 33.24,
	// 49.86, 34.06, 42.57, 51.09, …), so the max can sit anywhere in the stream:
	// another run peaked at 53.222 mid-stream against a terminal 48.490.
	// prompt_per_second's max EQUALLED its terminal value in both runs that
	// reported a terminal figure, which is why only one of the two fields shows
	// a measurable bias — taking prompt_per_second from the same frame is
	// defensive rather than corrective. Those are figures from THAT build on
	// THAT deployment, not a guarantee about every llama.cpp build.
	//
	// The precondition is a client's own `timings_per_token`, RELAYED UNTOUCHED
	// (a rule this path pins deliberately — see
	// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate). The
	// flag is what puts `timings` on the partials at all: the SAME PROMPT
	// REPLAYED WITHOUT the flag produced exactly ONE timings-bearing frame, the
	// terminal response.completed. A replay, not the same request — a request
	// either carried the flag or it did not. So the peak is reachable whenever a
	// client asks for mid-stream timings, and this capture is a measured no-op
	// for traffic that does not. A recorded rate is a ROUTING input — recordUsage
	// feeds it into the mapping's throughput EWMA
	// (UpdateMappingOpportunisticMetrics, inference_complete.go), which the
	// scorer and a model group's MinTokensPerSecond gate read back — so a peak
	// recorded as the end-of-request figure does not merely misreport one row,
	// it steers routing.
	//
	// A zero never overwrites a rate already held (takeLastNonZeroF): a frame
	// can carry a `timings` object whose predicted_per_second is 0.0 — the
	// measured series above opens with exactly that — and "this frame had
	// nothing to report yet" must not erase what the stream did report.
	finalPromptPerSecond float64
	finalTokensPerSecond float64
	// lastAt is the timestamp of the most recent feed/finish call: the best
	// available estimate of "generation completed" for the Anthropic fallback
	// rate's generation-window end (see usage below).
	lastAt time.Time

	// progress is the live counter behind this request's running-connections row
	// (ActiveRequest.Progress), or nil. The scanner holds the POINTER rather
	// than a callback because requestProgress is not a collaborator to be
	// faked: it is three atomics plus one nil-safe method in this same package
	// (request_progress.go), so a scanner test allocates a real one and reads
	// it -- a callback would add an indirection that buys no isolation.
	//
	// Non-nil only for a STREAMING passthrough request (proxyNative decides;
	// see there for why a buffered one gets none). Everything written through
	// it is a FACT the upstream itself reported -- a first-content timestamp,
	// its own cumulative count, its own rate. No rate is derived here:
	// liveProgressDTO already derives one over the window when no upstream rate
	// is present, and a second derivation would let the two drift.
	progress *requestProgress
}

// newUsageScanner returns a scanner for one native-passthrough response.
// capBytes should be the same budget the capture tee uses (see usageScanner's
// doc comment for why the two share a budget without being related features).
// progress is the live counter to publish per-frame facts into, or nil for a
// response whose progress is not displayed (a buffered one, and every test that
// only cares about the recorded totals).
func newUsageScanner(apiFlavor string, capBytes int, progress *requestProgress) *usageScanner {
	return &usageScanner{apiFlavor: apiFlavor, capBytes: capBytes, progress: progress}
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
// The two RATE fields do not ride on that monotonicity — they are not
// max-merged at all (see the final* fields) — but they are equally immune to a
// repeat: a re-scanned payload replays the same frames in the same order, so a
// last-wins capture lands on the same value, and once an authoritative frame has
// frozen the capture, re-seeing any frame takes nothing at all.
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

// scan stamps the first-content-frame timestamp and the
// authoritative-terminal-usage flag — each once, the first time such a frame is
// seen — keeps the LATEST rate the frames themselves report for the recording
// path, publishes each frame's own reported facts to the live progress counter,
// and merges payload's usage/timings fields into the running total.
//
// The per-payload probe stops running as soon as both flags are set, EXCEPT when
// a progress counter is attached: the live column is fed from every frame, not
// just the first of each kind, so for a displayed stream the loop keeps going.
//
// ONE scratch read of each frame serves both the recorded rate and the live
// column (takeFinalRates and publishProgress below): mergePassthroughUsage over
// a SINGLE payload into a scratch inference.Usage is precisely "what did this
// frame report" (jsonPayloads passes a lone JSON object through untouched), and
// doing it HERE rather than inside each consumer is what keeps the two
// consumers on ONE read of each frame instead of two. Reading those fields any
// other way would mean a FOURTH copy of the per-flavor usage switch, and this
// codebase deliberately keeps exactly three (mergePassthroughUsage,
// isContentFrame, isTerminalUsageFrame).
//
// What that costs, stated exactly rather than rounded to nothing: the read sits
// BEFORE publishProgress's `progress == nil || !haveFirstContent` guard, where
// the live column's own read used to sit AFTER it. So a displayed stream pays
// +ONE parse per PRE-CONTENT frame and none after — roughly four such frames
// for Responses (response.created, response.in_progress,
// response.output_item.added, response.content_part.added) and roughly two for
// Anthropic (message_start, content_block_start). A stream with NO counter
// attached pays one small parse per frame until the authoritative frame and
// none after it.
//
// `frame` is declared INSIDE the loop body and must stay there. The per-frame
// reset is load-bearing for BOTH surfaces: multiple SSE frames per payload is
// the normal case (nativeCopier.run reads in 32 KB chunks, so one scan call
// routinely covers several frames), and mergePassthroughUsage's per-field take
// is a running MAX — so a single `frame` hoisted out of the loop, the textbook
// "allocate once outside the loop" edit, would make the live cell AND the
// recorded rate a per-payload PEAK. That is this defect, relocated into the
// live column. Pinned by
// TestUsageScannerTwoRateBearingFramesInOnePayloadTakeTheLastFramesRate.
//
// The capture is FROZEN by the first authoritative frame: takeFinalRates
// self-gates on haveTerminalUsage, which is set immediately after it, so that
// frame's own rate is the last one taken. The loop's own condition is what makes
// both halves of that well-defined, in both directions:
//
//   - The condition includes !s.haveTerminalUsage, so the flag can never be set
//     while the loop is skipped, and the loop can never be skipped while the
//     flag is unset: the FIRST authoritative frame is always iterated,
//     regardless of the progress counter. Nothing LATER enjoys that guarantee —
//     with no counter attached the loop stops the moment both flags are set
//     while the accumulator merge below goes on for every remaining chunk — so a
//     capture left open past that frame would silently read a DIFFERENT frame
//     depending on whether the row was being displayed.
//   - Symmetrically, while NO authoritative frame has arrived that same
//     condition keeps the loop running for EVERY payload. That is what makes
//     keeping the latest rate free of the very hazard above rather than subject
//     to it: the only case where the latest rate decides the recorded figure — a
//     stream cut off before its terminal frame — is exactly the case where the
//     loop runs throughout, counter or no counter.
//
// For `openai_responses` the first authoritative frame is also the last
// (`response.completed` occurs once per response); for `anthropic_messages`
// EVERY `message_delta` is authoritative, and freezing at the first is what
// keeps that difference from mattering there.
func (s *usageScanner) scan(payload []byte, at time.Time) {
	if !s.haveFirstContent || !s.haveTerminalUsage || s.progress != nil {
		for _, p := range jsonPayloads(payload) {
			if !s.haveFirstContent && isContentFrame(s.apiFlavor, p) {
				s.haveFirstContent = true
				s.firstContentAt = at
			}
			authoritative := isTerminalUsageFrame(s.apiFlavor, p)
			var frame inference.Usage
			mergePassthroughUsage(&frame, s.apiFlavor, p)
			s.takeFinalRates(frame)
			if authoritative {
				s.haveTerminalUsage = true
			}
			s.publishProgress(frame, authoritative)
		}
	}
	mergePassthroughUsage(&s.acc, s.apiFlavor, payload)
}

// takeFinalRates keeps ONE frame's own rate fields as the figures usage() hands
// the recording path in place of the accumulator's running max, and is FROZEN by
// the first authoritative terminal frame: scan sets haveTerminalUsage
// immediately after calling this, so that frame's own rates are the last ones
// taken and every frame after it is ignored. See the final* fields for why a max
// is right for every count and wrong for these two, and scan for the loop
// guarantee both halves rest on.
//
// Until such a frame arrives the LATEST frame's rate stands. That is not a
// compromise for the responses that end without one — it is the only real
// measurement they have:
//
//   - A stream that ends before its `response.completed` carries `timings` on
//     its partials and no authoritative frame at all. Its last reported rate is
//     the generation's last measured value, where the accumulator's would be the
//     peak — the same defect, in the one case a terminal-only capture could not
//     reach. WHICH SURFACE that reaches depends on the recorded status, and the
//     distinction is worth keeping straight: a stream that ends CLEANLY at 200
//     without a `response.completed` — a `response.failed` or
//     `response.incomplete` terminal event (this repo's own translate path
//     emits `response.failed`, inference_complete.go), or an upstream that
//     simply stops — is recorded status "success", so its rate reaches the
//     routing EWMA too (recordUsage's feed is gated on success). A client
//     disconnect, a copy error or an idle timeout is recorded status "error"
//     (nativeTerminalStatus, native_passthrough.go), so it reaches only the
//     Activity row — where a peak is still a misreport, just not a routing
//     input.
//   - A BUFFERED body is ONE payload, so its own reported rate is trivially both
//     the latest and the only one. No `type` discriminator is needed to arrive
//     at it, which is why this needs no special case: a buffered body is not
//     authoritative, and the latest-rate rule already says its own figure.
//
// frame is the scratch Usage scan merged this payload into, shared with
// publishProgress — see scan for why the read lives there.
//
// The choice deliberately does NOT live inside mergeResponsesUsage. That
// function is also publishProgress's per-frame reader, and per-frame is the live
// column's FEATURE for the very reason above — a falling cumulative average must
// be shown falling, not pinned at the stream's peak — so a max-versus-latest
// decision baked into the merge would silently take that away. Keeping the
// choice here also preserves feed's documented "a line may be scanned more than
// once" tolerance, which rests on the merge itself being monotone.
func (s *usageScanner) takeFinalRates(frame inference.Usage) {
	if s.haveTerminalUsage {
		return
	}
	takeLastNonZeroF(&s.finalPromptPerSecond, frame.PromptPerSecond)
	takeLastNonZeroF(&s.finalTokensPerSecond, frame.TokensPerSecond)
}

// takeLastNonZeroF is the RATE counterpart to native_passthrough.go's takeMaxF:
// last-wins rather than running max, because a rate is a cumulative average
// whose latest value — never its largest — is the answer, and a zero counts as
// "this frame reported nothing" so it cannot erase a real measurement. Both uses
// are that one rule: takeFinalRates taking a frame's rate, and usage()
// substituting the result over the accumulator's max.
func takeLastNonZeroF(dst *float64, v float64) {
	if v > 0 {
		*dst = v
	}
}

// publishProgress reports ONE frame's own upstream-reported facts to the live
// progress counter, and nothing else — no gateway count, no gateway rate. It
// reuses requestProgress.observeDelta, whose inference.StreamProgress argument
// is already documented as "upstream-reported, never derived": the translate
// path feeds the same struct from chunkProgress, so both paths populate the
// running-connections row through one method with one contract.
//
// Three deliberate restrictions:
//
//   - Nothing is published before the first CONTENT frame. observeDelta's
//     first-token stamp is a compare-and-swap, so the first call fixes the row's
//     TTFT; publishing an earlier bookkeeping frame (Anthropic's message_start)
//     would stamp it before any content existed.
//
//   - The output-token count is published only from an AUTHORITATIVE usage frame
//     (isTerminalUsageFrame), never from any frame that merely carries a usage
//     object. This is usage()'s placeholder gate applied to the live column, for
//     a sharper reason: message_start's `output_tokens: 1` is indistinguishable
//     from a real total, and liveProgressDTO would divide that 1 by the
//     generation window and DISPLAY the result as a measured rate for the rest
//     of the stream. The recorded row tolerates the placeholder count because it
//     is presented as a count; a live rate derived from it would not be.
//
//   - The rate is read from THIS frame, not from the max-merged accumulator.
//     llama.cpp's `predicted_per_second` is a cumulative average over the
//     generation, so it commonly DROPS as the KV cache grows; a running max
//     would pin the live column at whatever the stream's early peak was and
//     never come down. observeDelta stores the value it is given, exactly as it
//     does for a translated chunk.
//
// frame is the scratch Usage scan merged this payload into — one frame's own
// numbers, under the same per-flavor rules the recorded row reads. See scan for
// why that read lives there and is shared with the recorded rate.
func (s *usageScanner) publishProgress(frame inference.Usage, authoritativeUsage bool) {
	if s.progress == nil || !s.haveFirstContent {
		return
	}
	prog := inference.StreamProgress{TokensPerSecond: frame.TokensPerSecond}
	if authoritativeUsage {
		prog.OutputTokens = frame.OutputTokens
	}
	s.progress.observeDelta(s.firstContentAt, &prog)
}

// usage returns the accumulated usage for the recording path — with the two
// RATE fields taken from the frames themselves rather than from the
// accumulator's running max (see the final* fields and takeFinalRates), because
// a max over a cumulative average that falls and swings is the generation's
// mid-stream peak. The figure is the authoritative terminal frame's own where
// one arrived, and the last rate the stream reported where none did.
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
// The fallback additionally requires an AUTHORITATIVE TERMINAL usage frame
// (isTerminalUsageFrame). An output-token count on its own is not enough:
// mergePassthroughUsage max-merges every usage object it sees into one field, so
// it cannot tell Anthropic's `message_start` PLACEHOLDER (`output_tokens: 1`)
// from a real `message_delta` total. Without this gate a stream that closes
// cleanly but whose only usage frame was `message_start` would derive
// `1 / 20s = 0.05` t/s and record it as a measured sample — and since a recorded
// rate also feeds an opted-in mapping's throughput EWMA (recordUsage ->
// UpdateMappingOpportunisticMetrics, inference_complete.go), that invented
// figure would become a ROUTING input. This is the same "only from an exact
// count" discipline the rest of the feature applies, aimed at *which* count is
// authoritative.
//
// It also floors the generation window itself at minGatewayRateWindow (see
// request_progress.go), for the same reason that constant exists there: a
// window measured microseconds after the first content frame divides an exact
// count by ~0 and yields an implausible rate. On the live column that would be
// a one-poll display glitch that self-corrects; here it is worse, because
// usage() feeds UpdateMappingOpportunisticMetrics's gen_tokens_per_second
// EWMA -- a ROUTING input the scorer and a model group's MinTokensPerSecond
// gate read -- so an implausible sample would not self-correct, it would be
// permanently blended into a number that steers request routing.
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
	// The rates the frames reported for THEMSELVES replace the accumulator's
	// running max (see the final* fields for why a rate must not be max-merged):
	// for `openai_responses` this is what turns a recorded PEAK back into the
	// generation's final figure, whether the stream ended with a
	// `response.completed` or was cut off before one.
	//
	// Written flavor-agnostically on purpose. It costs `anthropic_messages`
	// nothing and adds no fourth per-flavor switch: mergeAnthropicUsage
	// (native_passthrough.go) writes no rate field at all — its anthropicUsage
	// struct has none, because llama.cpp attaches no `timings` object to any
	// Anthropic frame — so for that flavor both sides are 0 and the fallback gate
	// below opens in exactly the same cases as before this substitution existed.
	//
	// Substituting only a NON-ZERO figure (takeLastNonZeroF) is what keeps that a
	// no-op rather than an assumption to be believed. isTerminalUsageFrame
	// accepts EVERY Anthropic `message_delta`, so the capture freezes at the
	// first one; if some flavor's merge ever did read a rate from frames this
	// capture does not see, the worst case is that the accumulator's figure
	// stands — never that a real measurement is overwritten with a 0.
	takeLastNonZeroF(&u.PromptPerSecond, s.finalPromptPerSecond)
	takeLastNonZeroF(&u.TokensPerSecond, s.finalTokensPerSecond)
	if s.apiFlavor == "anthropic_messages" && u.TokensPerSecond == 0 && u.OutputTokens > 0 &&
		s.haveFirstContent && s.haveTerminalUsage {
		if window := s.lastAt.Sub(s.firstContentAt); window >= minGatewayRateWindow {
			u.TokensPerSecond = float64(u.OutputTokens) / window.Seconds()
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

// isTerminalUsageFrame reports whether payload — one JSON usage/event object as
// returned by jsonPayloads — is a frame whose own usage numbers are the
// upstream's AUTHORITATIVE, FINAL output-token count for the response, as opposed
// to a running or placeholder snapshot. It is what gates usage()'s derived rate,
// and it is defined explicitly per API flavor here for the same reason
// isContentFrame above is.
//
// Anthropic: `message_delta` is the frame carrying the final
// `usage.output_tokens` of a streamed message, and a BUFFERED (non-streaming)
// response is itself a `message` object with the same authoritative top-level
// usage. Two frames are deliberately NOT authoritative:
//   - `message_start` carries `message.usage.output_tokens` as a PLACEHOLDER
//     (Anthropic sends 1) that mergePassthroughUsage cannot distinguish from a
//     real total, since it max-merges both into the same field. That
//     indistinguishability is the entire reason this predicate exists.
//   - `message_stop` is terminal but carries no usage object at all. Accepting a
//     frame that reports no count as evidence that an exact count WAS reported
//     would readmit exactly the placeholder-only stream this rules out, so
//     "terminal" alone is not the test — "terminal AND carries the total" is.
//
// Responses: `response.completed`, whose nested `response.usage` is the final
// count and onto which llama.cpp bolts its `timings` object. This branch is
// LOAD-BEARING for the LIVE column — do not delete it as unused: publishProgress
// gates the live output-token count on this predicate, so `response.completed`
// is the one frame that puts a Responses stream's own final count on the
// still-active running-connections row (and liveProgressDTO then derives a
// window rate from it), for the window between that frame and proxyNative's
// deferred Active.Remove. Pinned by
// TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves.
//
// It is load-bearing for the RECORDED row as well, though for a different
// value: the frame this branch identifies FREEZES takeFinalRates's capture, so
// `response.completed`'s own rate — not the accumulator's mid-stream peak, and
// not a rate from anything after it — is what the recording path gets. usage()'s
// DERIVED rate remains Anthropic-only, so the Responses shape still takes none
// of that (see usage()).
//
// What this branch does NOT decide is whether a rate is recorded at all. A
// response it never matches keeps the last rate its own frames reported, which
// covers both shapes that reach that state: a TRUNCATED stream (partial
// `timings`, no `response.completed` — a client that disconnects or an upstream
// that fails mid-generation), and a buffered body, whose single payload makes
// its own reported figure trivially the latest one. The buffered case needs no
// rule of its own for a second reason as well: this repo's own account of the
// shapes llama.cpp bolts `timings` onto (parsePassthroughUsage,
// native_passthrough.go, and §8.4.3 of
// docs/architecture/cross-cutting/telemetry-usage-observability.md) has the
// non-streaming `/v1/responses` body carrying no `timings` object at all, so
// there is no buffered Responses rate for any rule to protect.
func isTerminalUsageFrame(apiFlavor string, payload []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return false
	}
	switch apiFlavor {
	case "anthropic_messages":
		return probe.Type == "message_delta" || probe.Type == "message"
	case "openai_responses":
		return probe.Type == "response.completed"
	}
	return false
}
