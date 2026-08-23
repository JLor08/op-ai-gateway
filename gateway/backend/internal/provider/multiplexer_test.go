// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestMultiplexerDispatchesByTargetProvider(t *testing.T) {
	vllm := &recordingClient{response: Response{Text: "vllm response"}}
	ollama := &recordingClient{response: Response{Text: "ollama response"}}
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:   vllm,
		routing.ProviderOllama: ollama,
	}, nil)

	resp, err := mux.Complete(context.Background(), routing.Target{Provider: routing.ProviderVLLM, ProviderModel: "qwen-vllm"}, inference.Request{Model: "qwen-coder"})
	if err != nil {
		t.Fatalf("Complete returned %v", err)
	}
	if resp.Text != "vllm response" {
		t.Fatalf("Text = %q", resp.Text)
	}
	if vllm.calls != 1 {
		t.Fatalf("vllm calls = %d, want 1", vllm.calls)
	}
	if ollama.calls != 0 {
		t.Fatalf("ollama calls = %d, want 0", ollama.calls)
	}
	if vllm.target.ProviderModel != "qwen-vllm" {
		t.Fatalf("target = %#v", vllm.target)
	}
}

// TestMultiplexerRoutesLiteLLMToSharedOpenAIClient mirrors how providerClients
// registers ONE shared OpenAI-compatible client instance under both vllm and
// litellm: a litellm-provider request must reach that same shared instance.
func TestMultiplexerRoutesLiteLLMToSharedOpenAIClient(t *testing.T) {
	shared := &recordingClient{response: Response{Text: "shared response"}}
	ollama := &recordingClient{response: Response{Text: "ollama response"}}
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:    shared,
		routing.ProviderLiteLLM: shared, // same instance, exactly as providerClients wires it
		routing.ProviderOllama:  ollama,
	}, nil)

	resp, err := mux.Complete(context.Background(), routing.Target{Provider: routing.ProviderLiteLLM, ProviderModel: "gpt-4o"}, inference.Request{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Complete(litellm) returned %v", err)
	}
	if resp.Text != "shared response" {
		t.Fatalf("Text = %q, want shared response", resp.Text)
	}
	if shared.calls != 1 {
		t.Fatalf("shared client calls = %d, want 1", shared.calls)
	}
	if ollama.calls != 0 {
		t.Fatalf("ollama calls = %d, want 0", ollama.calls)
	}
}

func TestMultiplexerReturnsUnavailableForUnknownProvider(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderMock: NewMock(),
	}, nil)

	_, err := mux.Complete(context.Background(), routing.Target{Provider: routing.ProviderLlamaCPP}, inference.Request{Model: "qwen-coder"})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Complete error = %v, want ErrUnavailable", err)
	}
}

func TestMultiplexerDispatchesListModelsByTargetProvider(t *testing.T) {
	vllm := &recordingListerClient{models: []string{"vllm-model"}}
	ollama := &recordingListerClient{models: []string{"ollama-model"}}
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:   vllm,
		routing.ProviderOllama: ollama,
	}, nil)

	models, err := mux.ListModels(context.Background(), routing.Target{Provider: routing.ProviderVLLM})
	if err != nil {
		t.Fatalf("ListModels returned %v", err)
	}
	if len(models) != 1 || models[0] != "vllm-model" {
		t.Fatalf("models = %#v, want [vllm-model]", models)
	}
	if vllm.calls != 1 {
		t.Fatalf("vllm calls = %d, want 1", vllm.calls)
	}
	if ollama.calls != 0 {
		t.Fatalf("ollama calls = %d, want 0", ollama.calls)
	}
}

func TestMultiplexerListModelsReturnsUnavailableForUnknownProvider(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderMock: NewMock(),
	}, nil)

	_, err := mux.ListModels(context.Background(), routing.Target{Provider: routing.ProviderLlamaCPP})

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ListModels error = %v, want ErrUnavailable", err)
	}
}

func TestMultiplexerDispatchesProbeByTargetProvider(t *testing.T) {
	vllm := &recordingProberClient{}
	ollama := &recordingProberClient{}
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:   vllm,
		routing.ProviderOllama: ollama,
	}, nil)

	if err := mux.Probe(context.Background(), routing.Target{Provider: routing.ProviderVLLM, Endpoint: "http://vllm:8000"}, "/v1/health"); err != nil {
		t.Fatalf("Probe returned %v", err)
	}
	if vllm.calls != 1 {
		t.Fatalf("vllm calls = %d, want 1", vllm.calls)
	}
	if ollama.calls != 0 {
		t.Fatalf("ollama calls = %d, want 0", ollama.calls)
	}
	if vllm.path != "/v1/health" {
		t.Fatalf("vllm path = %q, want /v1/health", vllm.path)
	}
}

func TestMultiplexerProbeFallsBackToFallbackProber(t *testing.T) {
	fallback := &recordingProberClient{}
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, fallback)

	if err := mux.Probe(context.Background(), routing.Target{Provider: routing.ProviderLlamaCPP, Endpoint: "http://x:8000"}, "/v1/health"); err != nil {
		t.Fatalf("Probe returned %v", err)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallback.calls)
	}
}

func TestMultiplexerProbeReturnsUnavailableWithoutProber(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)

	err := mux.Probe(context.Background(), routing.Target{Provider: routing.ProviderLlamaCPP}, "/v1/health")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Probe error = %v, want ErrUnavailable", err)
	}
}

func TestMultiplexerCompleteStreamRoutesToStreamingClient(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)
	got := ""
	err := mux.CompleteStream(context.Background(), routing.Target{Provider: routing.ProviderMock},
		inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}},
		func(ev inference.StreamEvent) error { got += ev.Text; return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got == "" {
		t.Fatalf("expected streamed text")
	}
}

func TestMultiplexerCompleteStreamUnsupportedProvider(t *testing.T) {
	mux := NewMultiplexer(map[string]Client{"x": nonStreamingClient{}}, nil)
	err := mux.CompleteStream(context.Background(), routing.Target{Provider: "x"},
		inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}},
		func(inference.StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

type recordingClient struct {
	response Response
	calls    int
	target   routing.Target
}

func (c *recordingClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	c.calls++
	c.target = target
	return c.response, nil
}

type recordingListerClient struct {
	models []string
	calls  int
	target routing.Target
}

func (c *recordingListerClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	return Response{}, ErrUnavailable
}

func (c *recordingListerClient) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	c.calls++
	c.target = target
	return c.models, nil
}

type recordingProberClient struct {
	calls  int
	target routing.Target
	path   string
	err    error
}

func (c *recordingProberClient) Complete(context.Context, routing.Target, inference.Request) (Response, error) {
	return Response{}, ErrUnavailable
}

func (c *recordingProberClient) Probe(_ context.Context, target routing.Target, path string) error {
	c.calls++
	c.target = target
	c.path = path
	return c.err
}

type nonStreamingClient struct{}

func (nonStreamingClient) Complete(context.Context, routing.Target, inference.Request) (Response, error) {
	return Response{}, nil
}
