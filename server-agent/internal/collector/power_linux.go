// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

// Default sysfs roots for the RAPL/hwmon readers; raplCollector itself is
// build-tag-free and testable on any OS via newRAPLCollector's explicit
// powercapRoot param (tests point it at a synthetic tree) -- these two
// defaults are only needed here, where the real native collector is wired
// up for the standard Linux sysfs layout.
const (
	defaultPowercapRoot = "/sys/class/powercap"
	defaultHwmonRoot    = "/sys/class/hwmon"
)

// newNativePowerCollector returns the Linux RAPL power collector reading the
// standard powercap root, or powercapRoot instead when non-empty (e.g. a
// host-provided virtiofs tree). hwmon is a separate collector
// (newNativeHwmonCollector) so the privileged helper can slot in between.
func newNativePowerCollector(powercapRoot string) PowerCollector {
	if powercapRoot == "" {
		powercapRoot = defaultPowercapRoot
	}
	return newRAPLCollector(powercapRoot, raplDomainGlobLinux)
}

// newNativeHwmonCollector returns the world-readable hwmon power-sensor collector.
func newNativeHwmonCollector() PowerCollector { return newHwmonPowerCollector(defaultHwmonRoot) }
