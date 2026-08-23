// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// visionFakeProvider classifies a streaming request by whether it carries an image
// part. failText makes the text-only baseline fail (server-down). rejectImages makes
// an image request fail (no vision). answer is the text emitted for an accepted image
// request (used to test verify-mode token matching).
type visionFakeProvider struct {
	failText     bool
	rejectImages bool
	answer       string
}

func reqHasImage(req inference.Request) bool {
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == inference.ContentImage {
				return true
			}
		}
	}
	return false
}

// Complete satisfies provider.Client (required by Server.Provider's field type);
// the vision probe only exercises CompleteStream.
func (f visionFakeProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (f visionFakeProvider) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	img := reqHasImage(req)
	if !img && f.failText {
		return errors.New("upstream down")
	}
	if img && f.rejectImages {
		return errors.New("this server does not support multimodal")
	}
	text := "ok"
	if img && f.answer != "" {
		text = f.answer
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: text}); err != nil {
		return err
	}
	u := inference.Usage{OutputTokens: 1}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func TestMeasureVisionTarget(t *testing.T) {
	ctx := context.Background()
	tgt := benchTestTarget()

	cases := []struct {
		name    string
		mode    string
		prov    visionFakeProvider
		wantNil bool
		wantCap bool
	}{
		{"baseline-error-inconclusive", "accept", visionFakeProvider{failText: true}, true, false},
		{"image-rejected-not-capable", "accept", visionFakeProvider{rejectImages: true}, false, false},
		{"accept-ok-capable", "accept", visionFakeProvider{answer: "a photo"}, false, true},
		{"verify-token-hit", "verify", visionFakeProvider{answer: "I see RED and BLUE"}, false, true},
		{"verify-token-miss", "verify", visionFakeProvider{answer: "I see nothing"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{Provider: tc.prov}
			// The fake ignores the image bytes, so a placeholder data URL is fine.
			res := srv.measureVisionTarget(ctx, tgt, tc.mode, "data:image/png;base64,iVBORw0KGgo=", []string{"red", "blue"})
			if tc.wantNil {
				if res.VisionCapable != nil {
					t.Fatalf("VisionCapable = %v, want nil (inconclusive)", *res.VisionCapable)
				}
				return
			}
			if res.VisionCapable == nil {
				t.Fatalf("VisionCapable = nil, want %v", tc.wantCap)
			}
			if *res.VisionCapable != tc.wantCap {
				t.Fatalf("VisionCapable = %v, want %v", *res.VisionCapable, tc.wantCap)
			}
		})
	}
}

// visionHangingImageProvider answers a fast text-only baseline but BLOCKS on an image
// request until its context is cancelled — modelling a stalled upstream during the
// vision probe's image call (streamCollect's call site, mirroring benchHangingProvider
// used for streamOnce). Only the idle watchdog (via ctx cancellation) can end it.
type visionHangingImageProvider struct{}

func (visionHangingImageProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (visionHangingImageProvider) CompleteStream(ctx context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if reqHasImage(req) {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	u := inference.Usage{OutputTokens: 1}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

// TestStreamCollectHungUpstreamSelfTerminates guards the vision probe's streamCollect
// call site against the exact stall class this benchmark runner has been bitten by
// before (see benchmarkDefaultStreamIdle's doc comment): an upstream that accepts the
// image request but never emits and never errors. Without an idle watchdog on
// streamCollect, measureVisionTarget's image call would hang forever on
// context.Background() (a benchmark run has no client to end it) and the server would
// stay routing-excluded permanently. The baseline text call succeeds fast; only the
// image call hangs, so this specifically exercises streamCollect (not streamOnce,
// which TestBenchmarkRunHungUpstreamSelfTerminates already covers).
func TestStreamCollectHungUpstreamSelfTerminates(t *testing.T) {
	srv := &Server{Provider: visionHangingImageProvider{}}
	srv.streamIdleTimeout = 20 * time.Millisecond // watchdog fires fast

	done := make(chan struct{})
	var res BenchmarkResult
	go func() {
		res = srv.measureVisionTarget(context.Background(), benchTestTarget(), "accept", "data:image/png;base64,iVBORw0KGgo=", []string{"red", "blue"})
		close(done)
	}()

	select {
	case <-done:
		// measureVisionTarget returned -> the watchdog self-terminated the hung image stream.
	case <-time.After(2 * time.Second):
		t.Fatalf("measureVisionTarget did not return within 2s against a hung image request (idle watchdog did not fire)")
	}

	if res.VisionCapable == nil {
		t.Fatalf("VisionCapable = nil, want false (a cancelled/errored image request is a definitive not-capable)")
	}
	if *res.VisionCapable {
		t.Fatalf("VisionCapable = true, want false")
	}
}
