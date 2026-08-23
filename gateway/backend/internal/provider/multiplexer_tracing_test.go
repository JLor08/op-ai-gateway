// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"io"
	"log/slog"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/tracing"
	"testing"
)

func TestMultiplexerCompleteEmitsSpan(t *testing.T) {
	logs := logbuffer.NewBuffer(20, logbuffer.LevelTrace)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	defer slog.SetDefault(prev)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())

	mux := NewMultiplexer(map[string]Client{routing.ProviderMock: NewMock()}, nil)
	_, _ = mux.Complete(context.Background(), routing.Target{Provider: routing.ProviderMock}, inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}})

	var found bool
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" && r.Attrs["span"] == "provider.Complete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider.Complete span not emitted: %+v", logs.Snapshot())
	}
}
