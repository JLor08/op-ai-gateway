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
