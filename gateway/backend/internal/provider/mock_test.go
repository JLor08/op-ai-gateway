// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func TestMockProviderIncludesPromptText(t *testing.T) {
	mock := NewMock()
	req := inference.Request{
		Model: "gpt-oss-20b",
		Messages: []inference.Message{
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}},
		},
	}

	// The upstream app model (target.ProviderModel) differs from the selected
	// gateway model (req.Model). The mock reply must echo the SELECTED model.
	resp, err := mock.Complete(context.Background(), routing.Target{ProviderModel: "qwen2.5-upstream"}, req)
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	if resp.Text != "Mock response for gpt-oss-20b: hello" {
		t.Fatalf("Text = %q, want the selected gateway model gpt-oss-20b", resp.Text)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatalf("TotalTokens = 0, want nonzero")
	}
}

func TestMockProviderReturnsCanceledForPreCanceledContext(t *testing.T) {
	mock := NewMock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mock.Complete(ctx, routing.Target{ProviderModel: "qwen-coder"}, inference.Request{Model: "qwen-coder"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete error = %v, want context.Canceled", err)
	}
}

func TestMockProviderUsesLastUserMessage(t *testing.T) {
	mock := NewMock()
	req := inference.Request{
		Model: "qwen-coder",
		Messages: []inference.Message{
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "first prompt"}}},
			{Role: inference.RoleAssistant, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "assistant reply"}}},
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "last prompt"}}},
			{Role: inference.RoleAssistant, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "later assistant"}}},
			{Role: inference.RoleTool, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "later tool"}}},
		},
	}

	resp, err := mock.Complete(context.Background(), routing.Target{ProviderModel: "qwen-coder"}, req)
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	if resp.Text != "Mock response for qwen-coder: last prompt" {
		t.Fatalf("Text = %q", resp.Text)
	}
}

func TestMockProviderCountsUsageExactly(t *testing.T) {
	mock := NewMock()
	req := inference.Request{
		Model: "qwen-coder",
		Messages: []inference.Message{
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "alpha beta"}}},
		},
	}

	resp, err := mock.Complete(context.Background(), routing.Target{ProviderModel: "qwen-coder"}, req)
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}

	if resp.Usage.InputTokens != 2 {
		t.Fatalf("InputTokens = %d, want 2", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 6 {
		t.Fatalf("OutputTokens = %d, want 6", resp.Usage.OutputTokens)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Fatalf("TotalTokens = %d, want 8", resp.Usage.TotalTokens)
	}
	if resp.Usage.TotalTokens != resp.Usage.InputTokens+resp.Usage.OutputTokens {
		t.Fatalf("TotalTokens = %d, want InputTokens + OutputTokens (%d)", resp.Usage.TotalTokens, resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
}

func TestMockCompleteStreamEmitsDeltasAndUsage(t *testing.T) {
	m := NewMock()
	req := inference.Request{Model: "qwen-coder", Messages: []inference.Message{
		{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "ping"}}},
	}}
	var text string
	var completed *inference.Usage
	err := m.CompleteStream(context.Background(), routing.Target{}, req, func(ev inference.StreamEvent) error {
		if ev.Type == inference.StreamEventTextDelta {
			text += ev.Text
		}
		if ev.Type == inference.StreamEventCompleted {
			completed = ev.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(text, "ping") {
		t.Fatalf("streamed text = %q, want it to contain the prompt", text)
	}
	if completed == nil || completed.TotalTokens == 0 {
		t.Fatalf("missing completed usage: %#v", completed)
	}
}

func TestMockCompleteStreamStopsOnEmitError(t *testing.T) {
	m := NewMock()
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "a b c d e"}}}}}
	sentinel := errors.New("client gone")
	calls := 0
	err := m.CompleteStream(context.Background(), routing.Target{}, req, func(inference.StreamEvent) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("emit called %d times, want 1 (abort on first error)", calls)
	}
}

func TestMockCompleteStreamRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	err := NewMock().CompleteStream(ctx, routing.Target{}, req, func(inference.StreamEvent) error {
		t.Fatalf("emit should not be called on a canceled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewMockWithDelayStoresDelay(t *testing.T) {
	if got := NewMockWithDelay(50 * time.Millisecond).Delay; got != 50*time.Millisecond {
		t.Fatalf("NewMockWithDelay Delay = %v, want 50ms", got)
	}
	if got := NewMock().Delay; got != 0 {
		t.Fatalf("NewMock Delay = %v, want 0", got)
	}
}

// With Delay set the mock waits before producing output, but the wait must honor
// ctx: a canceled context returns promptly (ctx.Err()) instead of blocking for
// the full delay.
func TestMockCompleteDelayReturnsPromptlyOnCanceledContext(t *testing.T) {
	m := NewMockWithDelay(time.Hour) // would block ~forever if the wait ignored ctx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := m.Complete(ctx, routing.Target{ProviderModel: "qwen-coder"}, inference.Request{Model: "qwen-coder"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Complete took %v with a canceled ctx, want prompt return (delay not ctx-aware?)", elapsed)
	}
}

func TestMockCompleteStreamDelayReturnsPromptlyOnCanceledContext(t *testing.T) {
	m := NewMockWithDelay(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := inference.Request{Model: "qwen-coder", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	start := time.Now()
	err := m.CompleteStream(ctx, routing.Target{}, req, func(inference.StreamEvent) error {
		t.Fatal("emit must not be called: the delay must abort before producing output")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteStream error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("CompleteStream took %v with a canceled ctx, want prompt return (delay not ctx-aware?)", elapsed)
	}
}

// The seeded mock server (http://mock.local:8000) has no real socket, so the
// mock provider must always report reachable to keep dev/seed/tests green.
// An images path answers with the generations-shaped body sd-server returns, so
// the gateway's images relay (and an image-edit e2e against the mock) counts one
// image instead of reading a Responses SSE stream that has none.
func TestMockProxyNativeImagesPathReturnsGenerationsBody(t *testing.T) {
	for _, path := range []string{"/v1/images/edits", "/v1/images/generations"} {
		t.Run(path, func(t *testing.T) {
			resp, err := NewMock().ProxyNative(context.Background(), routing.Target{ProviderModel: "sd-cpp-local"}, path, []byte("ignored"), "multipart/form-data; boundary=X")
			if err != nil {
				t.Fatalf("ProxyNative: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
			raw, _ := io.ReadAll(resp.Body)
			var out struct {
				Created      int64  `json:"created"`
				OutputFormat string `json:"output_format"`
				Data         []struct {
					B64JSON string `json:"b64_json"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("body %q is not JSON: %v", raw, err)
			}
			if out.Created == 0 || out.OutputFormat != "png" {
				t.Fatalf("created=%d output_format=%q, want a timestamp and png", out.Created, out.OutputFormat)
			}
			if len(out.Data) != 1 {
				t.Fatalf("data has %d items, want exactly 1", len(out.Data))
			}
			// The payload must be a real image so the portal can render the
			// turn it produces, not just a string in the right key.
			pngBytes, err := base64.StdEncoding.DecodeString(out.Data[0].B64JSON)
			if err != nil {
				t.Fatalf("b64_json is not base64: %v", err)
			}
			if _, err := png.Decode(bytes.NewReader(pngBytes)); err != nil {
				t.Fatalf("b64_json does not decode as a PNG: %v", err)
			}
		})
	}
}

// Every other path keeps the canned Responses-style SSE stream the mock has always
// returned, whatever content type the caller sends.
// The transcription path answers with the document audio.cpp returns, not a Responses
// stream: the text, and the timing block the gateway bills audio_second from
// (timing.audio_duration_ms). Without this case a dev/e2e transcription would be
// handed an SSE body for a request that wants JSON, and the relay would find no
// duration to meter.
func TestMockProxyNativeTranscriptionPathReturnsTranscriptionBody(t *testing.T) {
	resp, err := NewMock().ProxyNative(context.Background(), routing.Target{ProviderModel: "whisper_x"}, "/v1/audio/transcriptions", []byte("ignored"), "multipart/form-data; boundary=X")
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (not the Responses event stream)", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Text   string `json:"text"`
		Timing struct {
			AudioDurationMs *float64 `json:"audio_duration_ms"`
		} `json:"timing"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body %q is not JSON: %v", raw, err)
	}
	if out.Text == "" {
		t.Fatalf("body %q carries no transcript text", raw)
	}
	if out.Timing.AudioDurationMs == nil || *out.Timing.AudioDurationMs != 2000 {
		t.Fatalf("body %q: timing.audio_duration_ms = %v, want 2000 (the duration the gateway bills from)", raw, out.Timing.AudioDurationMs)
	}
	if strings.Contains(string(raw), "event:") {
		t.Fatalf("body %q looks like an SSE stream", raw)
	}
}

// The diarized transcription path (/v1/audio/transcriptions/details) answers with the
// document audio.cpp's details endpoint returns, not the plain transcription's and
// not a Responses stream: the transcript, its speaker turns (offsets in samples,
// speaker_id a string, counted in sample_rate), and the same timing block the gateway
// bills audio_second from. The mock's turns carry TWO speakers, so a consumer that
// renders a speaker view only for a multi-speaker result has one to render in dev and
// e2e. The path is matched EXACTLY and apart from the plain one: the plain path's
// document has no speaker_turns, and this one is no prefix match on it.
func TestMockProxyNativeTranscriptionDetailsPathReturnsSpeakerTurns(t *testing.T) {
	resp, err := NewMock().ProxyNative(context.Background(), routing.Target{ProviderModel: "whisper_x"}, "/v1/audio/transcriptions/details", []byte("ignored"), "multipart/form-data; boundary=X")
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (not the Responses event stream)", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Text         string `json:"text"`
		SampleRate   int    `json:"sample_rate"`
		SpeakerTurns []struct {
			StartSample *int   `json:"start_sample"`
			EndSample   *int   `json:"end_sample"`
			SpeakerID   string `json:"speaker_id"`
			Text        string `json:"text"`
		} `json:"speaker_turns"`
		Timing struct {
			AudioDurationMs *float64 `json:"audio_duration_ms"`
		} `json:"timing"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body %q is not JSON: %v", raw, err)
	}
	if out.Text == "" || out.SampleRate <= 0 {
		t.Fatalf("body %q: text = %q, sample_rate = %d, want a transcript and the rate its turns count in", raw, out.Text, out.SampleRate)
	}
	speakers := map[string]bool{}
	for i, turn := range out.SpeakerTurns {
		if turn.StartSample == nil || turn.EndSample == nil || *turn.EndSample <= *turn.StartSample || turn.SpeakerID == "" || turn.Text == "" {
			t.Fatalf("body %q: speaker_turns[%d] = %+v, want a non-empty span, a speaker_id string and text", raw, i, turn)
		}
		speakers[turn.SpeakerID] = true
	}
	if len(speakers) < 2 {
		t.Fatalf("body %q: %d distinct speakers, want at least 2 so a speaker view has something to show", raw, len(speakers))
	}
	if out.Timing.AudioDurationMs == nil || *out.Timing.AudioDurationMs != 2000 {
		t.Fatalf("body %q: timing.audio_duration_ms = %v, want 2000 (the duration the gateway bills from)", raw, out.Timing.AudioDurationMs)
	}
	if strings.Contains(string(raw), "event:") {
		t.Fatalf("body %q looks like an SSE stream", raw)
	}
}

func TestMockProxyNativeOtherPathsKeepResponsesStream(t *testing.T) {
	resp, err := NewMock().ProxyNative(context.Background(), routing.Target{ProviderModel: "qwen-coder"}, "/v1/responses", []byte(`{}`), "application/json")
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "event: response.completed") || !strings.Contains(string(raw), "Mock native passthrough for qwen-coder") {
		t.Fatalf("body = %q, want the Responses SSE stream echoing the model", raw)
	}
}

func TestMockProxyNativeUnreachableFailsOnImagesPathToo(t *testing.T) {
	_, err := NewMock().WithUnreachable(true).ProxyNative(context.Background(), routing.Target{}, "/v1/images/edits", nil, "multipart/form-data; boundary=X")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProxyNative error = %v, want ErrUnavailable", err)
	}
}

func TestMockProxyNativeUnreachableFailsOnTranscriptionPathToo(t *testing.T) {
	_, err := NewMock().WithUnreachable(true).ProxyNative(context.Background(), routing.Target{}, "/v1/audio/transcriptions", nil, "multipart/form-data; boundary=X")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProxyNative error = %v, want ErrUnavailable", err)
	}
}

func TestMockProxyNativeUnreachableFailsOnTranscriptionDetailsPathToo(t *testing.T) {
	_, err := NewMock().WithUnreachable(true).ProxyNative(context.Background(), routing.Target{}, "/v1/audio/transcriptions/details", nil, "multipart/form-data; boundary=X")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProxyNative error = %v, want ErrUnavailable", err)
	}
}

func TestMockProviderProbeAlwaysReachable(t *testing.T) {
	if err := NewMock().Probe(context.Background(), routing.Target{Endpoint: "mock://local"}, "/v1/health"); err != nil {
		t.Fatalf("Probe returned %v, want nil", err)
	}
}

// The Unreachable seam forces Probe to fail so an operator/e2e can drop the
// mock's application from routing + offered models. Complete/CompleteStream are
// unaffected.
func TestMockProviderProbeUnreachable(t *testing.T) {
	m := NewMock().WithUnreachable(true)
	if err := m.Probe(context.Background(), routing.Target{Endpoint: "mock://local"}, "/v1/health"); err == nil {
		t.Fatalf("Probe returned nil, want an error when Unreachable is set")
	}
	// ListModels also fails when unreachable so a model_sync-mode application can
	// be forced unreachable (reachability there = ListModels success).
	if _, err := m.ListModels(context.Background(), routing.Target{Endpoint: "mock://local"}); err == nil {
		t.Fatalf("ListModels returned nil error, want an error when Unreachable is set")
	}
	// The reachable default is unchanged for both paths.
	if err := m.WithUnreachable(false).Probe(context.Background(), routing.Target{Endpoint: "mock://local"}, "/v1/health"); err != nil {
		t.Fatalf("Probe returned %v after WithUnreachable(false), want nil", err)
	}
	if _, err := m.WithUnreachable(false).ListModels(context.Background(), routing.Target{Endpoint: "mock://local"}); err != nil {
		t.Fatalf("ListModels returned %v after WithUnreachable(false), want nil", err)
	}
	// Complete still works while unreachable.
	req := inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	if _, err := m.Complete(context.Background(), routing.Target{}, req); err != nil {
		t.Fatalf("Complete returned %v while Unreachable, want nil", err)
	}
}

func TestMockProviderListModelsReturnsFixedList(t *testing.T) {
	mock := NewMock()

	models, err := mock.ListModels(context.Background(), routing.Target{})
	if err != nil {
		t.Fatalf("ListModels returned %v", err)
	}
	if len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty", models)
	}
	want := []string{"qwen-coder", "gpt-oss-20b"}
	if len(models) != len(want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
	for i, m := range want {
		if models[i] != m {
			t.Fatalf("models = %#v, want %#v", models, want)
		}
	}
}
