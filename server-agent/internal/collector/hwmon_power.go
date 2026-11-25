// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"path/filepath"
)

// hwmonPowerInputs lists the hwmon power*_input files under root. An empty root
// returns nil without globbing: filepath.Join("", "*", ...) would be a relative
// pattern resolved against the working directory.
func hwmonPowerInputs(root string) []string {
	if root == "" {
		return nil
	}
	inputs, _ := filepath.Glob(filepath.Join(root, "*", "power*_input"))
	return inputs
}

// hwmonPowerCollector reports system watts from the first readable hwmon
// power*_input (microwatts, instantaneous). CPU watts are always nil. hwmon
// power files are world-readable, so this works for an unprivileged agent.
type hwmonPowerCollector struct{ root string }

// newHwmonPowerCollector builds the collector reading the given hwmon root.
func newHwmonPowerCollector(root string) *hwmonPowerCollector {
	return &hwmonPowerCollector{root: root}
}

// Name identifies this collector.
func (c *hwmonPowerCollector) Name() string { return "hwmon" }

// Available reports whether any hwmon power input exists.
func (c *hwmonPowerCollector) Available() bool { return len(hwmonPowerInputs(c.root)) > 0 }

// Collect returns the first readable power input as system watts; never errors.
func (c *hwmonPowerCollector) Collect(context.Context) (*float64, *float64, error) {
	for _, p := range hwmonPowerInputs(c.root) {
		if uw, ok := readSysfsUint(p); ok {
			w := float64(uw) / 1e6 // microwatts -> watts
			return nil, &w, nil
		}
	}
	return nil, nil, nil
}
