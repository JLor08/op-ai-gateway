// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func captureHWLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A failing GPU collector drops its GPUs from the inventory; that must leave a
// trace in the log, or a report without GPUs is indistinguishable from a host
// without any (the boot race behind a GPU-less AMD report).
func TestCollectHardwareLogsFailingGPUCollector(t *testing.T) {
	buf := captureHWLogs(t)
	CollectHardware(context.Background(), []GPUCollector{erroringGPUCollector{}}, "x", HardwareOptions{Vulkan: noVulkan})
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "hardware inventory: gpu collect failed") ||
		!strings.Contains(out, "collector=nvidia-smi") || !strings.Contains(out, "deadline exceeded") {
		t.Fatalf("missing warn for the failing collector:\n%s", out)
	}
}

func TestCollectHardwareLogsCollectorWithoutGPUs(t *testing.T) {
	buf := captureHWLogs(t)
	CollectHardware(context.Background(), []GPUCollector{fixedGPUCollector{}}, "x", HardwareOptions{Vulkan: noVulkan})
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "hardware inventory: gpu collector found no GPUs") ||
		!strings.Contains(out, "collector=fixed") {
		t.Fatalf("missing debug line for the empty collector:\n%s", out)
	}
}
