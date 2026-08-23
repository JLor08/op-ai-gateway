// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build darwin

package collector

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// powermetricsCollector reads Apple-SoC CPU package power via the macOS
// `powermetrics` CLI (root/sudo required). Total system watts is not available
// CGO-free (SMC PSTR), so it is always nil.
type powermetricsCollector struct {
	interval time.Duration
}

// newNativePowerCollector returns the macOS powermetrics-backed power collector.
func newNativePowerCollector() PowerCollector {
	return &powermetricsCollector{interval: 200 * time.Millisecond}
}

// Name identifies this collector.
func (c *powermetricsCollector) Name() string { return "powermetrics" }

// Available reports whether we are on darwin with powermetrics on PATH.
func (c *powermetricsCollector) Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("powermetrics")
	return err == nil
}

// Collect runs one powermetrics cpu_power sample and parses CPU package watts.
// System watts is always nil on macOS. A permission failure (powermetrics needs
// root) or a parse miss is best-effort: the exec error is returned so the composite
// can skip this sub-collector, and a missing line yields (nil, nil, nil).
func (c *powermetricsCollector) Collect(ctx context.Context) (*float64, *float64, error) {
	cmd := exec.CommandContext(ctx, "powermetrics",
		"--samplers", "cpu_power",
		"-n", "1",
		"-i", strconv.FormatInt(c.interval.Milliseconds(), 10),
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("powermetrics: %w", err)
	}
	return parsePowermetricsCPUWatts(out), nil, nil
}
