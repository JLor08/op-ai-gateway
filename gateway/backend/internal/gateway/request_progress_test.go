// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func TestRequestProgressObserveDelta(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	p := &requestProgress{}

	// A first content delta with no upstream count still stamps TTFT.
	p.observeDelta(start.Add(300*time.Millisecond), nil)
	// A later delta carrying an upstream count and rate.
	p.observeDelta(start.Add(2*time.Second), &inference.StreamProgress{OutputTokens: 40, TokensPerSecond: 20})
	// The first-token stamp must NOT move.
	if got := p.firstTokenUnixNano.Load(); got != start.Add(300*time.Millisecond).UnixNano() {
		t.Fatalf("first-token stamp moved: %d", got)
	}
	if got := p.outputTokens.Load(); got != 40 {
		t.Fatalf("outputTokens = %d, want 40", got)
	}
	if got := p.upstreamTPSMilli.Load(); got != 20000 {
		t.Fatalf("upstreamTPSMilli = %d, want 20000", got)
	}
}

func TestRequestProgressNilReceiverIsSafe(t *testing.T) {
	var p *requestProgress
	p.observeDelta(time.Now(), &inference.StreamProgress{OutputTokens: 1})
}

// recordingStreamer emits a fixed event sequence, sleeping gap before every event
// after the first so consecutive events land at clearly distinct wall-clock
// times — the timestamp-ordering assertion in
// TestStreamSessionObservesProgressOnContentDeltas below needs that separation to
// avoid being a clock-resolution coin flip.
type recordingStreamer struct {
	events []inference.StreamEvent
	gap    time.Duration
}

func (recordingStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (r recordingStreamer) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for i, ev := range r.events {
		if i > 0 {
			time.Sleep(r.gap)
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	return nil
}

// TestStreamSessionObservesProgressOnContentDeltas drives streamSession.stream
// directly (bypassing the full HTTP scaffold, which streamSession's own doc
// comment says is not needed for its methods) through a fake streamer emitting:
//  1. an empty text delta carrying Progress{OutputTokens: 1} -> must be ignored
//     entirely (no content, so it is not "the first token").
//  2. Text: "a" with Progress{OutputTokens: 5, TokensPerSecond: 10} -> stamps TTFT.
//  3. Text: "b" with Progress{OutputTokens: 9, TokensPerSecond: 12} -> updates the
//     counts but must NOT move the TTFT stamp.
//
// This is the single hook in streamSession.stream that all three translate
// flavors (chat-completions, Responses, Anthropic) share, so covering it once
// here covers all three.
func TestStreamSessionObservesProgressOnContentDeltas(t *testing.T) {
	const gap = 20 * time.Millisecond
	streamer := recordingStreamer{
		gap: gap,
		events: []inference.StreamEvent{
			{Type: inference.StreamEventTextDelta, Text: "", Progress: &inference.StreamProgress{OutputTokens: 1}},
			{Type: inference.StreamEventTextDelta, Text: "a", Progress: &inference.StreamProgress{OutputTokens: 5, TokensPerSecond: 10}},
			{Type: inference.StreamEventTextDelta, Text: "b", Progress: &inference.StreamProgress{OutputTokens: 9, TokensPerSecond: 12}},
		},
	}
	p := &requestProgress{}
	ss := &streamSession{ctx: context.Background(), streamer: streamer, progress: p}

	// Event 1 fires ~immediately; event 2 fires only after the gap. A stamp taken
	// before that gap elapsed can only have come from the (empty, ignored) event 1.
	beforeStream := time.Now()
	if err := ss.stream(func(inference.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}

	stamp := p.firstTokenUnixNano.Load()
	if stamp == 0 {
		t.Fatal("firstTokenUnixNano not stamped")
	}
	if threshold := beforeStream.Add(gap / 2); stamp < threshold.UnixNano() {
		t.Fatalf("firstTokenUnixNano = %d looks stamped by the empty first delta, not the second (threshold %d)", stamp, threshold.UnixNano())
	}
	if got := p.outputTokens.Load(); got != 9 {
		t.Fatalf("outputTokens = %d, want 9", got)
	}
	if got := p.upstreamTPSMilli.Load(); got != 12000 {
		t.Fatalf("upstreamTPSMilli = %d, want 12000", got)
	}
}

// progressPacedStreamer emits n text deltas spaced by gap, each carrying an
// upstream-reported progress count, then completes.
type progressPacedStreamer struct {
	n   int
	gap time.Duration
}

func (progressPacedStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p progressPacedStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for i := 1; i <= p.n; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.gap):
		}
		ev := inference.StreamEvent{
			Type:     inference.StreamEventTextDelta,
			Text:     "tok",
			Progress: &inference.StreamProgress{OutputTokens: i, TokensPerSecond: float64(i)},
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{OutputTokens: p.n, TotalTokens: p.n}})
}

// TestActiveRegistryProgressConcurrentReadWrite drives a real streaming request
// through the HTTP layer (mirroring this package's other registry lifecycle
// tests) while a second goroutine concurrently snapshots the registry and reads
// the in-flight row's live Progress counters. Run under `go test -race`: the
// counters are atomics precisely so this concurrent access is safe — a plain
// (non-atomic) Progress field written by the stream goroutine and read here would
// trip the race detector.
func TestActiveRegistryProgressConcurrentReadWrite(t *testing.T) {
	srv := newStreamTestServerWithProvider(progressPacedStreamer{n: 20, gap: time.Millisecond})

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer dev-secret")
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-reqDone:
				return
			default:
			}
			for _, row := range srv.Active.Snapshot() {
				if row.Progress == nil {
					continue
				}
				_ = row.Progress.outputTokens.Load()
				_ = row.Progress.upstreamTPSMilli.Load()
				_ = row.Progress.firstTokenUnixNano.Load()
			}
		}
	}()

	select {
	case <-reqDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream request did not finish")
	}
	<-readerDone
}

func TestLiveProgressDTO(t *testing.T) {
	start := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	first := start.Add(500 * time.Millisecond)
	now := first.Add(2 * time.Second)

	upstream := &requestProgress{}
	upstream.firstTokenUnixNano.Store(first.UnixNano())
	upstream.outputTokens.Store(40)
	upstream.upstreamTPSMilli.Store(21500)

	gateway := &requestProgress{}
	gateway.firstTokenUnixNano.Store(first.UnixNano())
	gateway.outputTokens.Store(50) // 50 tokens over 2s -> 25.0

	// An exact count was never reported: no rate may be invented, however much
	// time has passed.
	noCount := &requestProgress{}
	noCount.firstTokenUnixNano.Store(first.UnixNano())

	// Clock skew: firstTokenUnixNano is recorded before StartedAt. The negative
	// TTFT must be clamped to 0.
	earlyFirst := start.Add(-100 * time.Millisecond)
	beforeStart := &requestProgress{}
	beforeStart.firstTokenUnixNano.Store(earlyFirst.UnixNano())

	cases := []struct {
		name       string
		p          *requestProgress
		wantTokens int
		wantTPS    float64
		wantSource string
		wantTTFT   int64
	}{
		{"upstream reported", upstream, 40, 21.5, "upstream", 500},
		{"gateway derived", gateway, 50, 25, "gateway", 500},
		{"no exact count", noCount, 0, 0, "", 500},
		{"first token before start (clock skew)", beforeStart, 0, 0, "", 0},
		{"no progress at all", nil, 0, 0, "", 0},
	}
	for _, tc := range cases {
		row := ActiveRequest{StartedAt: start, Progress: tc.p}
		tokens, tps, source, ttft := liveProgressDTO(row, now)
		if tokens != tc.wantTokens || tps != tc.wantTPS || source != tc.wantSource || ttft != tc.wantTTFT {
			t.Fatalf("%s: got (%d, %v, %q, %d)", tc.name, tokens, tps, source, ttft)
		}
	}
}
