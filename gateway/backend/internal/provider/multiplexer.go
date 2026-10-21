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
func (m *Multiplexer) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*ProxyResponse, error) {
	ctx, span := tracing.Start(ctx, "provider.ProxyNative")
	defer span.End()
	res, err := m.dispatchProxyNative(ctx, target, path, body, contentType)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

var _ LiveProgressRejectionMemo = (*Multiplexer)(nil)

// liveProgressMemoFor resolves target to its client and reports the memo
// capability if that client has it. Resolution is the SAME expression
// dispatchProxyNative and dispatchCompleteStream use -- m.clients[target.Provider],
// falling back to m.fallback -- which is precisely why a rejection recorded on
// one of those two endpoints is seen by the other: in production both resolve to
// the one OpenAICompatibleClient registered under every OpenAI-compatible key.
//
// Unlike those two dispatchers this one never errors. The capability is OPTIONAL
// (LiveProgressRejectionMemo), so a client without it, an unresolvable provider
// key and a nil Multiplexer all read as "nothing recorded" -- the state that
// means "send them", i.e. exactly the behaviour every caller had before this
// seam existed.
func (m *Multiplexer) liveProgressMemoFor(target routing.Target) (LiveProgressRejectionMemo, bool) {
	if m == nil {
		return nil, false
	}
	client := m.clients[strings.TrimSpace(target.Provider)]
	if client == nil {
		client = m.fallback
	}
	memo, ok := client.(LiveProgressRejectionMemo)
	return memo, ok
}

// LiveProgressRejected implements LiveProgressRejectionMemo by delegating to the
// resolved client. A provider that cannot remember never refuses.
func (m *Multiplexer) LiveProgressRejected(target routing.Target) bool {
	memo, ok := m.liveProgressMemoFor(target)
	if !ok {
		return false
	}
	return memo.LiveProgressRejected(target)
}

// RecordLiveProgressRejection implements LiveProgressRejectionMemo by delegating
// to the resolved client. A provider that cannot remember drops the record, which
// only restores the pre-memo cost of one wasted round trip on that mapping.
func (m *Multiplexer) RecordLiveProgressRejection(target routing.Target) {
	if memo, ok := m.liveProgressMemoFor(target); ok {
		memo.RecordLiveProgressRejection(target)
	}
}

func (m *Multiplexer) dispatchProxyNative(ctx context.Context, target routing.Target, path string, body []byte, contentType string) (*ProxyResponse, error) {
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
	return proxy.ProxyNative(ctx, target, path, body, contentType)
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

var _ SdcppCapabilitiesProber = (*Multiplexer)(nil)

// ProbeSdcppCapabilities routes the stable-diffusion.cpp capability probe to
// the provider's client (like ProbeModelInfo), falling back to the fallback
// client. Unlike ProbeModelInfo, a provider whose client does not implement
// SdcppCapabilitiesProber yields (SdcppVerdicts{}, ErrUnavailable): an error
// is what tells the caller there is no verdict to write, and a zero value
// with a nil error would read as a successful probe that found nothing.
func (m *Multiplexer) ProbeSdcppCapabilities(ctx context.Context, target routing.Target) (SdcppVerdicts, error) {
	ctx, span := tracing.Start(ctx, "provider.ProbeSdcppCapabilities")
	defer span.End()
	res, err := m.dispatchProbeSdcppCapabilities(ctx, target)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProbeSdcppCapabilities(ctx context.Context, target routing.Target) (SdcppVerdicts, error) {
	if m == nil {
		return SdcppVerdicts{}, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if p, ok := client.(SdcppCapabilitiesProber); ok {
			return p.ProbeSdcppCapabilities(ctx, target)
		}
	}
	if p, ok := m.fallback.(SdcppCapabilitiesProber); ok {
		return p.ProbeSdcppCapabilities(ctx, target)
	}
	return SdcppVerdicts{}, ErrUnavailable
}

var _ AudioCppModelsProber = (*Multiplexer)(nil)

// ProbeAudioCppModels routes the audio.cpp /v1/models speech probe to the
// provider's client (like ProbeSdcppCapabilities), falling back to the fallback
// client. A provider whose client does not implement AudioCppModelsProber
// yields (AudioCppVerdicts{}, ErrUnavailable), for the same reason as the
// sdcpp probe: an error is what tells the caller there is no verdict to write,
// and a zero value with a nil error would read as a successful probe that found
// nothing.
func (m *Multiplexer) ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	ctx, span := tracing.Start(ctx, "provider.ProbeAudioCppModels")
	defer span.End()
	res, err := m.dispatchProbeAudioCppModels(ctx, target, modelID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	if m == nil {
		return AudioCppVerdicts{}, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if p, ok := client.(AudioCppModelsProber); ok {
			return p.ProbeAudioCppModels(ctx, target, modelID)
		}
	}
	if p, ok := m.fallback.(AudioCppModelsProber); ok {
		return p.ProbeAudioCppModels(ctx, target, modelID)
	}
	return AudioCppVerdicts{}, ErrUnavailable
}

var _ AudioCppVoicesLister = (*Multiplexer)(nil)

// ListVoices routes the audio.cpp /v1/audio/voices listing to the provider's
// client (like ProbeAudioCppModels), falling back to the fallback client. A
// provider whose client does not implement AudioCppVoicesLister yields
// (nil, ErrUnavailable): an error is what tells the caller there are no voices to
// store, and an empty list with a nil error would read as a successful listing
// that found none.
func (m *Multiplexer) ListVoices(ctx context.Context, target routing.Target, model string) ([]Voice, error) {
	ctx, span := tracing.Start(ctx, "provider.ListVoices")
	defer span.End()
	res, err := m.dispatchListVoices(ctx, target, model)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchListVoices(ctx context.Context, target routing.Target, model string) ([]Voice, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if l, ok := client.(AudioCppVoicesLister); ok {
			return l.ListVoices(ctx, target, model)
		}
	}
	if l, ok := m.fallback.(AudioCppVoicesLister); ok {
		return l.ListVoices(ctx, target, model)
	}
	return nil, ErrUnavailable
}

var _ SdcppImageOptionsProber = (*Multiplexer)(nil)

// ProbeSdcppImageOptions routes the stable-diffusion.cpp image-options probe to
// the provider's client, exactly like ProbeSdcppCapabilities: a provider whose
// client does not implement SdcppImageOptionsProber yields
// (SdcppImageOptions{}, ErrUnavailable), because a zero value with a nil error
// would read as a successful probe that found no options.
func (m *Multiplexer) ProbeSdcppImageOptions(ctx context.Context, target routing.Target) (SdcppImageOptions, error) {
	ctx, span := tracing.Start(ctx, "provider.ProbeSdcppImageOptions")
	defer span.End()
	res, err := m.dispatchProbeSdcppImageOptions(ctx, target)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

func (m *Multiplexer) dispatchProbeSdcppImageOptions(ctx context.Context, target routing.Target) (SdcppImageOptions, error) {
	if m == nil {
		return SdcppImageOptions{}, ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if p, ok := client.(SdcppImageOptionsProber); ok {
			return p.ProbeSdcppImageOptions(ctx, target)
		}
	}
	if p, ok := m.fallback.(SdcppImageOptionsProber); ok {
		return p.ProbeSdcppImageOptions(ctx, target)
	}
	return SdcppImageOptions{}, ErrUnavailable
}

var _ RuntimeEnsurer = (*Multiplexer)(nil)

// EnsureRuntimeModel routes the agent router's ensure call to the provider's
// client (like ProbeSdcppCapabilities), falling back to the fallback client. A
// provider whose client does not implement RuntimeEnsurer yields
// ErrUnavailable: a nil error would read as a child that is running.
func (m *Multiplexer) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	ctx, span := tracing.Start(ctx, "provider.EnsureRuntimeModel")
	defer span.End()
	err := m.dispatchEnsureRuntimeModel(ctx, target)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (m *Multiplexer) dispatchEnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	if m == nil {
		return ErrUnavailable
	}
	if client, ok := m.clients[strings.TrimSpace(target.Provider)]; ok {
		if e, ok := client.(RuntimeEnsurer); ok {
			return e.EnsureRuntimeModel(ctx, target)
		}
	}
	if e, ok := m.fallback.(RuntimeEnsurer); ok {
		return e.EnsureRuntimeModel(ctx, target)
	}
	return fmt.Errorf("%w: runtime ensure not supported for provider %q", ErrUnavailable, target.Provider)
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
