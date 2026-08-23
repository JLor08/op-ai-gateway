// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/tracing"
	"strings"

	"go.opentelemetry.io/otel/codes"
)

type Multiplexer struct {
	clients  map[string]Client
	fallback Client
}

var (
	_ Client      = (*Multiplexer)(nil)
	_ ModelLister = (*Multiplexer)(nil)
	_ Prober      = (*Multiplexer)(nil)
)

func NewMultiplexer(clients map[string]Client, fallback Client) *Multiplexer {
	copied := make(map[string]Client, len(clients))
	for provider, client := range clients {
		provider = strings.TrimSpace(provider)
		if provider == "" || client == nil {
			continue
		}
		copied[provider] = client
	}
	return &Multiplexer{clients: copied, fallback: fallback}
}

func (m *Multiplexer) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	ctx, span := tracing.Start(ctx, "provider.Complete")
	defer span.End()
	res, err := m.dispatchComplete(ctx, target, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchComplete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if m == nil {
		return Response{}, ErrUnavailable
	}
	if client := m.clients[strings.TrimSpace(target.Provider)]; client != nil {
		return client.Complete(ctx, target, req)
	}
	if m.fallback != nil {
		return m.fallback.Complete(ctx, target, req)
	}
	return Response{}, fmt.Errorf("%w: no client configured for provider %q", ErrUnavailable, target.Provider)
}

var _ StreamingClient = (*Multiplexer)(nil)

func (m *Multiplexer) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	ctx, span := tracing.Start(ctx, "provider.CompleteStream")
	defer span.End()
	err := m.dispatchCompleteStream(ctx, target, req, emit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (m *Multiplexer) dispatchCompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	if m == nil {
		return ErrUnavailable
	}
	client := m.clients[strings.TrimSpace(target.Provider)]
	if client == nil {
		client = m.fallback
	}
	streamer, ok := client.(StreamingClient)
	if !ok {
		// Unlike ListModels, a capability mismatch does NOT cross-route to the
		// fallback: streaming must come from the provider routing resolved to.
		return fmt.Errorf("%w: streaming not supported for provider %q", ErrUnavailable, target.Provider)
	}
	return streamer.CompleteStream(ctx, target, req, emit)
}

var _ NativeProxyClient = (*Multiplexer)(nil)

// ProxyNative routes native passthrough to the provider's client (like
// CompleteStream, it does NOT cross-route to the fallback on a capability
// mismatch — the passthrough must reach the provider routing resolved to).
func (m *Multiplexer) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	ctx, span := tracing.Start(ctx, "provider.ProxyNative")
	defer span.End()
	res, err := m.dispatchProxyNative(ctx, target, path, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	client := m.clients[strings.TrimSpace(target.Provider)]
	if client == nil {
		client = m.fallback
	}
	proxy, ok := client.(NativeProxyClient)
	if !ok {
		return nil, fmt.Errorf("%w: native passthrough not supported for provider %q", ErrUnavailable, target.Provider)
	}
	return proxy.ProxyNative(ctx, target, path, body)
}

func (m *Multiplexer) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	ctx, span := tracing.Start(ctx, "provider.ListModels")
	defer span.End()
	res, err := m.dispatchListModels(ctx, target)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchListModels(ctx context.Context, target routing.Target) ([]string, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if lister, ok := client.(ModelLister); ok {
			return lister.ListModels(ctx, target)
		}
	}
	if lister, ok := m.fallback.(ModelLister); ok {
		return lister.ListModels(ctx, target)
	}
	return nil, fmt.Errorf("%w: no model lister for provider %q", ErrUnavailable, target.Provider)
}

// Probe routes the reachability check to the provider's client (like
// ListModels), falling back to the fallback client when the provider is unknown
// or does not implement Prober. A nil return means reachable.
func (m *Multiplexer) Probe(ctx context.Context, target routing.Target, path string) error {
	ctx, span := tracing.Start(ctx, "provider.Probe")
	defer span.End()
	err := m.dispatchProbe(ctx, target, path)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (m *Multiplexer) dispatchProbe(ctx context.Context, target routing.Target, path string) error {
	if m == nil {
		return ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if prober, ok := client.(Prober); ok {
			return prober.Probe(ctx, target, path)
		}
	}
	if prober, ok := m.fallback.(Prober); ok {
		return prober.Probe(ctx, target, path)
	}
	return fmt.Errorf("%w: no prober for provider %q", ErrUnavailable, target.Provider)
}

// LoadedModels routes the loaded-model probe to the provider's client (like
// ListModels/Probe), falling back to the fallback client. A provider whose client
// does not implement LoadedModelLister yields (nil, nil): the feature is simply
// unavailable there, not an error.
func (m *Multiplexer) LoadedModels(ctx context.Context, target routing.Target, statusPath, format string) ([]string, error) {
	ctx, span := tracing.Start(ctx, "provider.LoadedModels")
	defer span.End()
	res, err := m.dispatchLoadedModels(ctx, target, statusPath, format)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchLoadedModels(ctx context.Context, target routing.Target, statusPath, format string) ([]string, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if lister, ok := client.(LoadedModelLister); ok {
			return lister.LoadedModels(ctx, target, statusPath, format)
		}
	}
	if lister, ok := m.fallback.(LoadedModelLister); ok {
		return lister.LoadedModels(ctx, target, statusPath, format)
	}
	return nil, nil
}

// UnloadModel routes the best-effort model-unload to the provider's client (like
// LoadedModels). A client that does not implement ModelUnloader yields (false, nil):
// unloading is simply unsupported there, not an error.
func (m *Multiplexer) UnloadModel(ctx context.Context, target routing.Target, model string) (bool, error) {
	ctx, span := tracing.Start(ctx, "provider.UnloadModel")
	defer span.End()
	res, err := m.dispatchUnloadModel(ctx, target, model)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchUnloadModel(ctx context.Context, target routing.Target, model string) (bool, error) {
	if m == nil {
		return false, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if unloader, ok := client.(ModelUnloader); ok {
			return unloader.UnloadModel(ctx, target, model)
		}
	}
	if unloader, ok := m.fallback.(ModelUnloader); ok {
		return unloader.UnloadModel(ctx, target, model)
	}
	return false, nil
}

// ProbeModelInfo routes the model-info probe to the provider's client (like
// LoadedModels), falling back to the fallback client. A provider whose client does
// not implement ModelInfoProber yields (nil, nil): the feature is simply unavailable
// there, not an error.
func (m *Multiplexer) ProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error) {
	ctx, span := tracing.Start(ctx, "provider.ProbeModelInfo")
	defer span.End()
	res, err := m.dispatchProbeModelInfo(ctx, target, probePath)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if p, ok := client.(ModelInfoProber); ok {
			return p.ProbeModelInfo(ctx, target, probePath)
		}
	}
	if p, ok := m.fallback.(ModelInfoProber); ok {
		return p.ProbeModelInfo(ctx, target, probePath)
	}
	return nil, nil
}

var _ MemoryProber = (*Multiplexer)(nil)

// ProbeServerMemory routes the upstream saturation probe to the provider's client
// (like ProbeModelInfo), falling back to the fallback client. A provider whose
// client does not implement MemoryProber yields (ServerMemory{}, nil): the feature
// is simply unavailable there, not an error.
func (m *Multiplexer) ProbeServerMemory(ctx context.Context, target routing.Target, probePath, format string) (ServerMemory, error) {
	ctx, span := tracing.Start(ctx, "provider.ProbeServerMemory")
	defer span.End()
	res, err := m.dispatchProbeServerMemory(ctx, target, probePath, format)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProbeServerMemory(ctx context.Context, target routing.Target, probePath, format string) (ServerMemory, error) {
	if m == nil {
		return ServerMemory{}, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if p, ok := client.(MemoryProber); ok {
			return p.ProbeServerMemory(ctx, target, probePath, format)
		}
	}
	if p, ok := m.fallback.(MemoryProber); ok {
		return p.ProbeServerMemory(ctx, target, probePath, format)
	}
	return ServerMemory{}, nil
}
