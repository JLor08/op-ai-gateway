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

func TestStartNoopWhenDisabled(t *testing.T) {
	logs := logbuffer.NewBuffer(10, logbuffer.LevelTrace)
	p, err := Setup(Options{Enabled: false, SampleRatio: 1.0}, logs)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer p.Shutdown(context.Background())
	_, span := Start(context.Background(), "x")
	if span.IsRecording() {
		t.Fatalf("span recording while tracing disabled")
	}
	if len(logs.Snapshot()) != 0 {
		t.Fatalf("span line emitted while disabled")
	}
}

func TestStartRecordsAndMirrorsWhenEnabled(t *testing.T) {
	logs := logbuffer.NewBuffer(10, logbuffer.LevelTrace)
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	p, err := Setup(Options{Enabled: true, SampleRatio: 1.0}, logs)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer p.Shutdown(context.Background())
	_, span := Start(context.Background(), "unit.op")
	if !span.IsRecording() {
		t.Fatalf("span not recording while enabled")
	}
	span.End()
	snap := logs.Snapshot()
	if len(snap) != 1 || snap[0].Msg != "span" || snap[0].Attrs["span"] != "unit.op" {
		t.Fatalf("span not mirrored to logbuffer: %+v", snap)
	}
}

func TestSetEnabledLive(t *testing.T) {
	logs := logbuffer.NewBuffer(10, logbuffer.LevelTrace)
	p, _ := Setup(Options{Enabled: false, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())
	p.SetEnabled(true)
	_, span := Start(context.Background(), "y")
	if !span.IsRecording() {
		t.Fatalf("SetEnabled(true) did not enable recording")
	}
}
