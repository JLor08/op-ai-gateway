// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

import (
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// dynamicSampler wraps a ratio sampler with a live on/off switch. When disabled
// it drops every span (a non-recording span is cheap: no attributes, no
// processor, no export), which is the default-fast posture. Enabled, it defers
// to a ParentBased(TraceIDRatioBased(ratio)) sampler.
type dynamicSampler struct {
	enabled  atomic.Bool
	delegate sdktrace.Sampler
}

func newDynamicSampler(ratio float64) *dynamicSampler {
	return &dynamicSampler{
		delegate: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)),
	}
}

func (d *dynamicSampler) SetEnabled(on bool) { d.enabled.Store(on) }
func (d *dynamicSampler) Enabled() bool      { return d.enabled.Load() }

func (d *dynamicSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if !d.enabled.Load() {
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}
	return d.delegate.ShouldSample(p)
}

func (d *dynamicSampler) Description() string { return "OP-dynamic-sampler" }
