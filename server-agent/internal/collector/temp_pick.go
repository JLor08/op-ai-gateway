// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "strings"

// sensorTemp is a minimal, testable projection of gopsutil's
// sensors.TemperatureStat (SensorKey/Temperature only — the fields pickCPUTemp
// needs). Kept as its own tiny type (rather than depending on the gopsutil type
// directly) so pickCPUTemp is pure and OS-independent: it is unit-tested here on
// every host, while the actual gopsutil call lives behind the Linux build tag in
// temp_linux.go (mirrors the dmiReader/parseDmidecodeMemory pattern used by
// hwinfo_linux.go for the same reason).
type sensorTemp struct {
	Key  string
	Temp float64
}

// pickCPUTemp chooses the CPU package temperature from hwmon-style sensor keys:
// prefer an Intel coretemp "package" sensor, then an AMD k10temp "tctl"/"tdie",
// then a generic cpu_thermal/cpu-package sensor; ignore non-positive readings;
// nil when nothing matches.
func pickCPUTemp(stats []sensorTemp) *float64 {
	norm := func(s string) string { return strings.ToLower(strings.NewReplacer(" ", "", "_", "").Replace(s)) }
	var pkg, tctl, generic *float64
	for i := range stats {
		if stats[i].Temp <= 0 {
			continue
		}
		k := norm(stats[i].Key)
		switch {
		case strings.Contains(k, "coretemp") && strings.Contains(k, "package"):
			pkg = &stats[i].Temp
		case strings.Contains(k, "k10temp") && (strings.Contains(k, "tctl") || strings.Contains(k, "tdie")):
			if tctl == nil {
				tctl = &stats[i].Temp
			}
		case strings.Contains(k, "cpu") && (strings.Contains(k, "thermal") || strings.Contains(k, "package")):
			if generic == nil {
				generic = &stats[i].Temp
			}
		}
	}
	if pkg != nil {
		return pkg
	}
	if tctl != nil {
		return tctl
	}
	return generic
}
