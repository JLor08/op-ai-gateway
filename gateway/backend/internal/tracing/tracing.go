// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package tracing owns the gateway's opt-in OpenTelemetry TracerProvider. It is
// off by default (the dynamic sampler drops every span) and mirrors each sampled
// span into the shared logbuffer at the TRACE level; an OTLP-HTTP exporter is
// added only when an endpoint is configured. The rest of the code uses Start.
package tracing

import (
	"context"
	"op-ai-gateway/internal/logbuffer"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const tracerName = "op-ai-gateway"

// tracer holds the process tracer that Start uses. It is seeded at init with the
// OTel global (a no-op until Setup runs) and updated by Setup to the installed
// provider's own tracer. Caching it — instead of calling otel.Tracer(name) per
// span — avoids the two process-global lookup mutexes that call takes every time,
// keeping Start cheap on the disabled (default) hot path. The atomic.Pointer makes
// the Setup write / Start reads race-free (Setup may run while background
// goroutines call Start) AND lets tests that call Setup repeatedly each route
// their spans to their own provider (a plain init-cached global tracer only
// adopts the FIRST SetTracerProvider via otel's one-shot delegate).
var tracer atomic.Pointer[oteltrace.Tracer]

func init() {
	t := otel.Tracer(tracerName)
	tracer.Store(&t)
}

// Options configures Setup.
type Options struct {
	Enabled      bool
	SampleRatio  float64
	OTLPEndpoint string // empty ⇒ no OTLP exporter
}

// Provider is the handle returned by Setup: it flips the live on/off switch and
// shuts the SDK down.
type Provider struct {
	tp      *sdktrace.TracerProvider
	sampler *dynamicSampler
}

// Setup builds the TracerProvider (dynamic sampler + logbuffer mirror + optional
// OTLP) and installs it as the OTel global. Start resolves its tracer from that
// global on each call, so there is no unsynchronized package state. The caller
// folds Shutdown into its cleanup.
func Setup(opts Options, logs *logbuffer.Buffer) (*Provider, error) {
	sampler := newDynamicSampler(opts.SampleRatio)
	sampler.SetEnabled(opts.Enabled)

	// A single-attribute resource with the schema URL avoids the schema-conflict
	// error resource.Merge(resource.Default(), …) can raise while still setting
	// service.name.
	res := resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName(tracerName),
	)

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
		// Synchronous mirror so a span line appears in the Logs view immediately.
		sdktrace.WithSpanProcessor(newLogSpanProcessor(logs)),
	}
	if opts.OTLPEndpoint != "" {
		exp, err := otlptracehttp.New(context.Background(),
			otlptracehttp.WithEndpointURL(opts.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	// Cache this provider's tracer so Start resolves it with a single atomic load
	// (no per-call global lookup). Updating here also routes each test's repeated
	// Setup to its own provider.
	t := tp.Tracer(tracerName)
	tracer.Store(&t)
	return &Provider{tp: tp, sampler: sampler}, nil
}

// SetEnabled flips tracing on/off at runtime (no restart).
func (p *Provider) SetEnabled(on bool) {
	if p != nil && p.sampler != nil {
		p.sampler.SetEnabled(on)
	}
}

// Enabled reports the current live master state.
func (p *Provider) Enabled() bool {
	return p != nil && p.sampler != nil && p.sampler.Enabled()
}

// Shutdown flushes and stops the SDK (idempotent-safe for a nil Provider).
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// Start opens a span from the global provider's tracer (installed by Setup;
// before Setup it is the OTel no-op global, so Start is always safe). Resolving
// the tracer from the synchronized global each call avoids any unsynchronized
// package state. When tracing is disabled the returned span is non-recording (a
// cheap no-op) and ctx is unchanged in effect.
func Start(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span) {
	return (*tracer.Load()).Start(ctx, name, opts...)
}
