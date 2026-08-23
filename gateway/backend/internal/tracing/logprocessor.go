// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/logbuffer"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// logSpanProcessor emits one slog record at LevelTrace per ended span. It uses
// the process slog default (the logbuffer handler), so the record is gated by
// the live log level: at "info" the handler drops it (no I/O); at "trace" it
// lands in the Logs view + stderr. Only SAMPLED spans reach OnEnd, so this is a
// no-op when tracing is disabled.
type logSpanProcessor struct{ logs *logbuffer.Buffer }

func newLogSpanProcessor(logs *logbuffer.Buffer) sdktrace.SpanProcessor {
	return &logSpanProcessor{logs: logs}
}

func (p *logSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {
	// no-op: this processor only logs on OnEnd (see the type doc comment above).
}

func (p *logSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	// Cheap gate: skip building the record entirely unless the live level admits
	// LevelTrace. (slog would also gate, but this avoids the attr allocation.)
	if p.logs.Level() > logbuffer.LevelTrace {
		return
	}
	sc := s.SpanContext()
	attrs := []slog.Attr{
		slog.String("span", s.Name()),
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
		slog.Int64("dur_us", s.EndTime().Sub(s.StartTime()).Microseconds()),
		slog.String("status", s.Status().Code.String()),
	}
	if parent := s.Parent(); parent.HasSpanID() {
		attrs = append(attrs, slog.String("parent_span_id", parent.SpanID().String()))
	}
	ctx := context.Background()
	slog.LogAttrs(ctx, logbuffer.LevelTrace, "span", attrs...)
}

func (p *logSpanProcessor) Shutdown(context.Context) error   { return nil }
func (p *logSpanProcessor) ForceFlush(context.Context) error { return nil }
