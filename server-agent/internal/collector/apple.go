// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
)

// ioreg field patterns. The IOAccelerator dump is a free-form text dump, so the
// collector scrapes the few fields it needs by regex and tolerates any of them
// being absent (the value then defaults to zero / empty).
var (
	// Device Utilization % lives inside the PerformanceStatistics dict.
	ioregUtilRe = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)
	// model may appear bare ("model" = "Apple M4 Max") or data-wrapped
	// ("model" = <"Apple M1">); the optional '<' covers both.
	ioregModelRe = regexp.MustCompile(`"model"\s*=\s*<?"([^"]+)"`)
	// IOGPUModel is an alternate model key on some OS versions.
	ioregGPUModelRe = regexp.MustCompile(`"IOGPUModel"\s*=\s*<?"([^"]+)"`)
	// The trailing-quote form deliberately excludes the "(driver)" variant.
	ioregInUseRe = regexp.MustCompile(`"In use system memory"\s*=\s*(\d+)`)
	ioregAllocRe = regexp.MustCompile(`"Alloc system memory"\s*=\s*(\d+)`)
)

// appleCollector reports the Apple integrated GPU via the ioreg CLI.
type appleCollector struct{}

// NewApple returns an Apple GPUCollector backed by ioreg.
func NewApple() GPUCollector { return &appleCollector{} }

// Name identifies this collector.
func (c *appleCollector) Name() string { return "apple" }

// Available reports whether we are on darwin with ioreg on PATH.
func (c *appleCollector) Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("ioreg")
	return err == nil
}

// Collect runs ioreg for the IOAccelerator node and parses its GPU metrics.
func (c *appleCollector) Collect(ctx context.Context) ([]sample.GPU, error) {
	cmd := exec.CommandContext(ctx, "ioreg", "-r", "-c", "IOAccelerator", "-d", "1")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseIoregGPU(out)
}

// parseIoregGPU scrapes the Apple GPU metrics from an ioreg IOAccelerator dump.
// Apple exposes a single integrated GPU, so it always returns exactly one GPU
// (Index 0). Every field is best-effort: an absent key leaves its zero value
// and the parse never fails or panics on arbitrary text. Memory is reported as
// in-use vs. allocated system memory (Apple uses unified memory, so there is no
// dedicated VRAM total).
func parseIoregGPU(data []byte) ([]sample.GPU, error) {
	text := string(data)

	gpu := sample.GPU{Index: 0}
	if m := ioregModelRe.FindStringSubmatch(text); m != nil {
		gpu.Name = m[1]
	} else if m := ioregGPUModelRe.FindStringSubmatch(text); m != nil {
		gpu.Name = m[1]
	}
	gpu.UtilPct = float64(ioregMatchInt64(text, ioregUtilRe))
	gpu.MemUsedBytes = ioregMatchInt64(text, ioregInUseRe)
	gpu.MemTotalBytes = ioregMatchInt64(text, ioregAllocRe)

	return []sample.GPU{gpu}, nil
}

// ioregMatchInt64 returns the first capture group of re parsed as int64, or 0
// when the pattern does not match or the value does not parse.
func ioregMatchInt64(text string, re *regexp.Regexp) int64 {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}
