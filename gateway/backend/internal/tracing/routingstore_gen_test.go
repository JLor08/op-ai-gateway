// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing_test

import (
	"context"
	"io"
	"log/slog"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/tracing"
	"testing"
)

func TestRoutingStoreDecoratorDelegatesAndTraces(t *testing.T) {
	logs := logbuffer.NewBuffer(20, logbuffer.LevelTrace)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	defer slog.SetDefault(prev)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())

	base := routing.NewMemoryStore()
	wrapped := tracing.NewRoutingStoreWithTracing(base)
	if _, err := wrapped.AIServers(context.Background()); err != nil {
		t.Fatalf("delegate AIServers: %v", err)
	}
	var found bool
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" && r.Attrs["span"] == "routing.Store.AIServers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AIServers span not emitted: %+v", logs.Snapshot())
	}
}
