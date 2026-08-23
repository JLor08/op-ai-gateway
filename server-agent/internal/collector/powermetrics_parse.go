// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"regexp"
	"strconv"
	"strings"
)

// powermetricsCPUPowerRe matches the "CPU Power: <n> mW" (or W) line emitted by
// `powermetrics --samplers cpu_power` on Apple silicon. Anchored to the line start
// with the exact "CPU Power:" label so it never matches "E-Cluster Power:",
// "P-Cluster Power:", or "Combined Power:".
var powermetricsCPUPowerRe = regexp.MustCompile(`(?mi)^\s*CPU Power:\s*([0-9]+(?:[.,][0-9]+)?)\s*(mW|W)\b`)

// parsePowermetricsCPUWatts extracts CPU package power (in watts) from powermetrics
// text output. Returns nil when the CPU Power line is absent/unparseable. A "mW"
// reading is divided by 1000; a "W" reading is used as-is. A comma decimal
// separator (some locales) is normalized to a dot.
func parsePowermetricsCPUWatts(data []byte) *float64 {
	m := powermetricsCPUPowerRe.FindSubmatch(data)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(strings.Replace(string(m[1]), ",", ".", 1), 64)
	if err != nil {
		return nil
	}
	if strings.EqualFold(string(m[2]), "mW") {
		v /= 1000
	}
	w := v
	return &w
}
