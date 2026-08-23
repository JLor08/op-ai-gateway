// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

type Response struct {
	Text  string
	Usage inference.Usage
	// ToolCalls are the function/tool calls the model made (may be empty). A
	// tool-call-only response has empty Text and a non-empty ToolCalls.
	ToolCalls []inference.ToolCall
	// Reasoning is the model's chain-of-thought (harmony "analysis" channel /
	// `reasoning_content`), used to render a Responses `reasoning` output item on
	// the non-stream path. Empty for models/providers that emit none.
	Reasoning string
	// FinishReason is the upstream OpenAI Chat Completions finish_reason ("stop" /
	// "length" / "tool_calls" / …). It drives the Anthropic stop_reason mapping on
	// the Messages edge. Empty when the provider emits none (mock, ollama).
	FinishReason string
}

// Mock is the built-in fake provider. Delay is a test-only artificial latency
// (default 0 = off): when > 0 the provider waits Delay (honoring ctx) before
// producing output, so a live e2e can observe an in-flight request. It is wired
// only from the OP_AI_GATEWAY_MOCK_DELAY_MS config (default 0), never in production.
//
// Unreachable is a test/e2e seam (default false, production-safe): when true,
// both Probe AND ListModels report the mock unreachable (return an error) so an
// operator/e2e can force the seeded mock's application to fall out of routing +
// offered models under EITHER health-check mode (health_path probes via Probe;
// model_sync probes via ListModels). It affects only those two reachability
// paths; Complete/CompleteStream are unchanged. Wired from
// OP_AI_GATEWAY_MOCK_UNREACHABLE (default false).
type Mock struct {
	Delay       time.Duration
	Unreachable bool
}

var _ Client = Mock{}

func NewMock() Mock {
	return Mock{}
}

// NewMockWithDelay builds a Mock that waits d before producing output. d <= 0
// behaves exactly like NewMock (no delay).
func NewMockWithDelay(d time.Duration) Mock {
	return Mock{Delay: d}
}

// WithUnreachable returns a copy of the mock whose Probe reports unreachable
// (returns an error) when unreachable is true. Test/e2e seam only.
func (m Mock) WithUnreachable(unreachable bool) Mock {
	m.Unreachable = unreachable
	return m
}

// waitDelay blocks for m.Delay, honoring ctx cancellation. It returns ctx.Err()
// if the context is done first, or nil once the delay elapses (or immediately
// when Delay <= 0).
func (m Mock) waitDelay(ctx context.Context) error {
	if m.Delay <= 0 {
		return nil
	}
	select {
	case <-time.After(m.Delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mockText(target routing.Target, req inference.Request) (string, inference.Usage) {
	prompt := lastUserText(req.Messages)
	// Echo the SELECTED (gateway) model the caller requested — req.Model — not the
	// resolved upstream app model (target.ProviderModel, used by real providers for
	// the outbound call), so the mock reply reflects what the user picked. Fall back
	// to the upstream model only when the request carries no model.
	model := req.Model
	if model == "" {
		model = providerModel(target, req)
	}
	text := "Mock response for " + model + ": " + prompt
	inputTokens := countWords(prompt)
	outputTokens := countWords(text)
	return text, inference.Usage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
}

func (m Mock) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if err := m.waitDelay(ctx); err != nil {
		return Response{}, err
	}
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	default:
	}
	text, usage := mockText(target, req)
	return Response{Text: text, Usage: usage}, nil
}

var _ NativeProxyClient = Mock{}

// ProxyNative returns a canned OpenAI-Responses-style SSE stream so a
// native-passthrough application works in dev/mock mode. It echoes the resolved
// upstream model and a terminal response.completed carrying usage, mirroring what
// a real Codex-capable upstream would emit.
func (m Mock) ProxyNative(_ context.Context, target routing.Target, _ string, _ []byte) (*ProxyResponse, error) {
	if m.Unreachable {
		return nil, ErrUnavailable
	}
	model := target.ProviderModel
	if model == "" {
		model = target.Model
	}
	text := "Mock native passthrough for " + model
	body := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_mock","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_mock","object":"response","status":"completed","model":"` + model + `","usage":{"input_tokens":1,"output_tokens":5,"total_tokens":6}}}` + "\n\n"
	return &ProxyResponse{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

var _ StreamingClient = Mock{}

func (m Mock) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	if err := m.waitDelay(ctx); err != nil {
		return err
	}
	text, usage := mockText(target, req)
	words := strings.Fields(text)
	for i, word := range words {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		chunk := word
		if i < len(words)-1 {
			chunk += " "
		}
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: chunk}); err != nil {
			return err
		}
	}
	u := usage
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func (m Mock) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	// The Unreachable seam also fails discovery so a model_sync-mode application
	// can be forced unreachable in an e2e (reachability there = ListModels ok).
	if m.Unreachable {
		return nil, errors.New("provider.mock_unreachable")
	}
	return []string{"qwen-coder", "gpt-oss-20b"}, nil
}

var _ Prober = Mock{}

// Probe reports reachable (nil) by default. The seeded mock server has no real
// socket, so a real HTTP probe would fail; returning nil keeps dev/seed/tests
// green. When Unreachable is set (test/e2e seam), it returns an error so the
// mock's application is treated unreachable by the probe loop.
func (m Mock) Probe(ctx context.Context, target routing.Target, path string) error {
	if m.Unreachable {
		return errors.New("provider.mock_unreachable")
	}
	return nil
}

var _ MemoryProber = Mock{}

// ProbeServerMemory returns a canned idle upstream-saturation reading so a capacity
// benchmark works in dev/mock mode: a small slot count with nothing processing or
// deferred (never saturated). A blank probe path is a no-op (feature off); the
// Unreachable seam makes it report unavailable.
func (m Mock) ProbeServerMemory(_ context.Context, _ routing.Target, probePath, _ string) (ServerMemory, error) {
	if m.Unreachable {
		return ServerMemory{}, ErrUnavailable
	}
	if strings.TrimSpace(probePath) == "" {
		return ServerMemory{}, nil
	}
	return ServerMemory{RequestsDeferred: 0, RequestsProcessing: 0, TotalSlots: 4, OK: true}, nil
}

func lastUserText(messages []inference.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == inference.RoleUser {
			return messages[i].Text()
		}
	}
	return ""
}

func countWords(text string) int {
	return len(strings.Fields(text))
}
