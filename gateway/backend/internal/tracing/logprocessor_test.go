// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

import (
	"context"
	"io"
	"log/slog"
	"op-ai-gateway/internal/logbuffer"
	"testing"
)

func TestLogProcessorGatedByLevel(t *testing.T) {
	logs := logbuffer.NewBuffer(10, 0) // info: no trace lines
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	p, _ := Setup(Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())
	_, span := Start(context.Background(), "op.gated")
	span.End()
	if n := len(logs.Snapshot()); n != 0 {
		t.Fatalf("span mirrored at info level: %d", n)
	}
	logs.SetLevel(logbuffer.LevelTrace)
	_, span2 := Start(context.Background(), "op.visible")
	span2.End()
	snap := logs.Snapshot()
	if len(snap) != 1 || snap[0].Attrs["span"] != "op.visible" {
		t.Fatalf("span not mirrored at trace level: %+v", snap)
	}
	if _, ok := snap[0].Attrs["trace_id"]; !ok {
		t.Fatalf("span line missing trace_id")
	}
}
