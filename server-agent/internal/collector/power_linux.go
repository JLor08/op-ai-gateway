// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

// Default sysfs roots for the RAPL/hwmon readers; raplCollector itself is
// build-tag-free and testable on any OS via newRAPLCollector's explicit
// powercapRoot/hwmonRoot params (tests point them at a synthetic tree) -- these
// two defaults are only needed here, where the real native collector is wired
// up for the standard Linux sysfs layout.
const (
	defaultPowercapRoot = "/sys/class/powercap"
	defaultHwmonRoot    = "/sys/class/hwmon"
)

// newNativePowerCollector returns the Linux RAPL/hwmon power collector reading the
// standard sysfs roots.
func newNativePowerCollector() PowerCollector {
	return newRAPLCollector(defaultPowercapRoot, defaultHwmonRoot)
}
