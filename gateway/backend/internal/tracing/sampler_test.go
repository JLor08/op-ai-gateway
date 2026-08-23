// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

import (
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestDynamicSamplerOffDrops(t *testing.T) {
	s := newDynamicSampler(1.0)
	res := s.ShouldSample(sdktrace.SamplingParameters{TraceID: oteltrace.TraceID{0x1}})
	if res.Decision != sdktrace.Drop {
		t.Fatalf("disabled sampler decision = %v, want Drop", res.Decision)
	}
}

func TestDynamicSamplerOnSamples(t *testing.T) {
	s := newDynamicSampler(1.0)
	s.SetEnabled(true)
	res := s.ShouldSample(sdktrace.SamplingParameters{TraceID: oteltrace.TraceID{0x1}})
	if res.Decision != sdktrace.RecordAndSample {
		t.Fatalf("enabled ratio=1 decision = %v, want RecordAndSample", res.Decision)
	}
	s.SetEnabled(false)
	if s.ShouldSample(sdktrace.SamplingParameters{TraceID: oteltrace.TraceID{0x1}}).Decision != sdktrace.Drop {
		t.Fatalf("re-disabled sampler still samples")
	}
}
