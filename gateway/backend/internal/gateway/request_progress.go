// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/inference"
	"sync/atomic"
	"time"
)

// requestProgress is one in-flight request's live counters. Written by the single
// goroutine that owns the stream and read lock-free while the DTO is built, so it
// is deliberately NOT a field of ActiveRequest itself: activeRegistry stores
// ActiveRequest by VALUE and its mutex sits on the routing hot path
// (routing.Resolver calls ServerActivity several times per resolution, inside
// per-candidate loops), so a write lock per token would serialize routing behind
// the token rate -- and an atomic embedded in a copied struct would trip
// `copylocks` in every existing Snapshot/range. Reached by pointer, an
// ActiveRequest copy shares the one live counter, which is exactly what the DTO
// builder needs.
//
// Same shape as the repo's two existing hot-path counters: chatRunRegistry holds
// *ChatRun and mutates per delta through the run's OWN lock (chat_runs.go), and
// runtimeLogSub.dropped is an atomic on a per-subscriber object (runtime_logs.go).
type requestProgress struct {
	// outputTokens is the upstream's own cumulative count. 0 means the upstream
	// reported none -- never a gateway guess.
	outputTokens atomic.Int64
	// upstreamTPSMilli is the upstream's own rate x1000 (milli-tokens/second: one
	// decimal is displayed, so this is ample, and it avoids float bit-punning in
	// an atomic). 0 means the upstream reported no rate.
	upstreamTPSMilli atomic.Int64
	// firstTokenUnixNano stamps the first delta that carried content. 0 = none yet.
	firstTokenUnixNano atomic.Int64
}

// observeDelta records the first content delta's timestamp (once) and any exact,
// upstream-reported progress carried with it. Nil-safe: every non-streaming path
// and every test literal has no progress struct at all.
//
// Two callers, both streaming, both feeding it FACTS rather than conclusions:
// stream_session.go on the translate path (from the provider's chunkProgress)
// and usageScanner.publishProgress on the native-passthrough path (from each
// relayed SSE frame's own numbers). Neither derives a rate — liveProgressDTO
// does that, once, from an exact count over the window, so there is exactly one
// derivation in the feature and nothing for a second one to drift against.
func (p *requestProgress) observeDelta(at time.Time, prog *inference.StreamProgress) {
	if p == nil {
		return
	}
	p.firstTokenUnixNano.CompareAndSwap(0, at.UnixNano())
	if prog == nil {
		return
	}
	if prog.OutputTokens > 0 {
		p.outputTokens.Store(int64(prog.OutputTokens))
	}
	if prog.TokensPerSecond > 0 {
		p.upstreamTPSMilli.Store(int64(prog.TokensPerSecond * 1000))
	}
}

// minGatewayRateWindow floors the generation window a GATEWAY-derived rate may be
// computed over. Without it a DTO built microseconds after the first delta divides
// an exact count by a window of ~0 and renders something like "1000000.0" for one
// poll before self-correcting -- a nonsense figure in the one feature whose thesis
// is that a displayed number is a real measurement. Suppressing a sub-50ms sample
// costs nothing: the row showed the shared "never measured" em-dash a moment
// earlier and shows it for one more poll. It does NOT apply to an
// upstream-reported rate, which is a measurement the gateway did not make and has
// no window of its own.
const minGatewayRateWindow = 50 * time.Millisecond

// liveProgressDTO resolves one in-flight request's counters into its wire values.
// Nil-safe throughout: a request with no progress struct resolves to "not
// measured" -- an explicit "" source rather than a fabricated zero.
//
// The provenance rule is structural, not merely documented: a gateway-derived rate
// is only ever computed over an EXACT upstream count, so there is no code path in
// which a guessed number becomes a displayed rate.
func liveProgressDTO(row ActiveRequest, now time.Time) (outputTokens int, tps float64, source string, ttftMS int64) {
	p := row.Progress
	if p == nil {
		return 0, 0, "", 0
	}
	outputTokens = int(p.outputTokens.Load())
	first := p.firstTokenUnixNano.Load()
	if first == 0 {
		return outputTokens, 0, "", 0
	}
	firstAt := time.Unix(0, first)
	if ttftMS = firstAt.Sub(row.StartedAt).Milliseconds(); ttftMS < 0 {
		ttftMS = 0
	}
	if milli := p.upstreamTPSMilli.Load(); milli > 0 {
		return outputTokens, float64(milli) / 1000, "upstream", ttftMS
	}
	if window := now.Sub(firstAt); outputTokens > 0 && window >= minGatewayRateWindow {
		return outputTokens, float64(outputTokens) / window.Seconds(), "gateway", ttftMS
	}
	return outputTokens, 0, "", ttftMS
}
